//go:build e2e && e2e_tofu && linux

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/jackc/pgx/v5"

	"github.com/mr-karan/writ/internal/awsrole/ststest"
	"github.com/mr-karan/writ/internal/control/pgtest"
	"github.com/mr-karan/writ/internal/forge/forgetest"
	"github.com/mr-karan/writ/internal/oidc/oidctest"
	"github.com/mr-karan/writ/internal/server"
)

// tofuVersityEnv names the versitygw binary of the child of TestTofuS3.
const tofuVersityEnv = "WRIT_E2E_VERSITYGW"

const (
	s3Bucket     = "writ-states"
	s3Key        = "app/terraform.tfstate"
	s3RootKey    = "root-key"
	s3RootSecret = "root-secret"
)

var (
	s3PlanRole  = "arn:aws:iam::" + strings.Repeat("2", 12) + ":role/writ-plan"
	s3ApplyRole = "arn:aws:iam::" + strings.Repeat("2", 12) + ":role/writ-apply"
)

// TestTofuS3 is the test of stacks on S3: a team's writ serve runs an
// OpenTofu stack whose state is in its own bucket on a local S3 stand-in,
// versitygw, with use_lockfile. The plan role's user may read the state's
// key and write nothing; the apply role's may write the key and its lock
// file. STS answers each role with that user's keys. A merge request job
// plans app, which writes nothing to the bucket; a second person
// approves; the job on main applies it, and the index holds the state's
// resources at its serial. A drift check, which the apply makes due,
// plans the head of main with no change and writes nothing, and its plan
// is never approved. The same files plan no changes, and a plan and apply
// of an edit go on from Writ's serial, after which a drift check finds no
// drift. Then a tofu apply with the bucket owner's keys, outside Writ,
// writes a new serial: writ serve's refresh at start marks it as drift on
// the stack's index record and leaves the index at Writ's serial. No
// secret set was sealed with that serial, so a drift check finds drift
// unknown, and a plan keeps no detail, cannot be approved, and marks
// nothing more. Neither the control database nor the data directory holds
// the canary.
func TestTofuS3(t *testing.T) {
	if os.Getenv(tofuChildEnv) != "" {
		tofuS3Child(t)
		return
	}
	gw, err := exec.LookPath("versitygw")
	if err != nil {
		t.Fatalf("this test needs versitygw on PATH, as just e2e-tofu-s3 gives: %v", err)
	}
	base, stop, err := pgtest.Start()
	if err != nil {
		t.Fatalf("this test needs Postgres's initdb, pg_ctl, and createdb on PATH, as just e2e-tofu-s3 gives: %v", err)
	}
	t.Cleanup(stop)
	inTofuNamespace(t, "TestTofuS3", tofuTeamNamespace, tofuPGEnv+"="+pgtest.Database(t, base), tofuVersityEnv+"="+realPath(t, gw))
}

// s3AppFiles is repository infra with stack app, whose state is in the
// bucket on the stand-in at endpoint, and whose terraform_data.note holds
// note.
func s3AppFiles(lock []byte, endpoint, note string) map[string]forgetest.File {
	return map[string]forgetest.File{
		"infra/app/.terraform.lock.hcl": {Content: string(lock)},
		"infra/app/main.tf": {Content: s3Backend(endpoint) + `
variable "canary" {
  type      = string
  sensitive = true
}

resource "random_pet" "name" {
  keepers = { canary = var.canary }
}

resource "terraform_data" "secret" {
  input = base64encode(var.canary)
}

resource "terraform_data" "note" {
  input = "` + note + `"
}
`},
	}
}

// s3Backend is the terraform block of a stack on the stand-in. The skips
// keep tofu from asking STS who it is, which the stand-in's users cannot.
func s3Backend(endpoint string) string {
	return `terraform {
  backend "s3" {
    bucket                      = "` + s3Bucket + `"
    key                         = "` + s3Key + `"
    region                      = "us-east-1"
    use_lockfile                = true
    use_path_style              = true
    skip_credentials_validation = true
    skip_requesting_account_id  = true
    skip_region_validation      = true
    endpoints = {
      s3 = "` + endpoint + `"
    }
  }
` + tofuProviders + "}\n"
}

// versity is a versitygw on loopback with one bucket.
type versity struct {
	t        *testing.T
	endpoint string
}

// startVersity starts versitygw over a directory under top, with its own
// users: writ-plan may list the bucket and read the state's key, and
// writ-apply may also write the key and its lock file and delete the lock
// file. The bucket's owner is the root user.
func startVersity(t *testing.T, top string) *versity {
	t.Helper()
	data, iam := filepath.Join(top, "s3"), filepath.Join(top, "s3-iam")
	for _, d := range []string{data, iam} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	log := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	gw := os.Getenv(tofuVersityEnv)
	cmd := exec.CommandContext(ctx, gw, "--port", addr, "--access", s3RootKey, "--secret", s3RootSecret, "--iam-dir", iam, "--quiet", "posix", data)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("versitygw log:\n%s", log.String())
		}
	})
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("versitygw did not listen on %s:\n%s", addr, log.String())
		}
	}
	v := &versity{t: t, endpoint: "http://" + addr}
	for _, u := range [][2]string{{"writ-plan", "plan-secret"}, {"writ-apply", "apply-secret"}} {
		out, err := exec.Command(gw, "admin", "--access", s3RootKey, "--secret", s3RootSecret, "--endpoint-url", v.endpoint,
			"create-user", "--access", u[0], "--secret", u[1], "--role", "user").CombinedOutput()
		if err != nil {
			t.Fatalf("create user %s: %v\n%s", u[0], err, out)
		}
	}
	root := v.client(s3RootKey, s3RootSecret)
	if _, err := root.CreateBucket(context.Background(), &s3.CreateBucketInput{Bucket: aws.String(s3Bucket)}); err != nil {
		t.Fatal(err)
	}
	arn := "arn:aws:s3:::" + s3Bucket
	policy := `{"Version":"2012-10-17","Statement":[
{"Effect":"Allow","Principal":{"AWS":["writ-plan","writ-apply"]},"Action":["s3:ListBucket"],"Resource":"` + arn + `"},
{"Effect":"Allow","Principal":{"AWS":["writ-plan","writ-apply"]},"Action":["s3:GetObject"],"Resource":["` + arn + `/` + s3Key + `","` + arn + `/` + s3Key + `.tflock"]},
{"Effect":"Allow","Principal":{"AWS":["writ-apply"]},"Action":["s3:PutObject"],"Resource":["` + arn + `/` + s3Key + `","` + arn + `/` + s3Key + `.tflock"]},
{"Effect":"Allow","Principal":{"AWS":["writ-apply"]},"Action":["s3:DeleteObject"],"Resource":"` + arn + `/` + s3Key + `.tflock"}
]}`
	if _, err := root.PutBucketPolicy(context.Background(), &s3.PutBucketPolicyInput{Bucket: aws.String(s3Bucket), Policy: aws.String(policy)}); err != nil {
		t.Fatal(err)
	}
	return v
}

func (v *versity) client(key, secret string) *s3.Client {
	return s3.New(s3.Options{Region: "us-east-1", BaseEndpoint: aws.String(v.endpoint), UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider(key, secret, "")})
}

// objects lists the bucket as its owner sees it: each key with its ETag.
func (v *versity) objects() map[string]string {
	v.t.Helper()
	out, err := v.client(s3RootKey, s3RootSecret).ListObjectsV2(context.Background(), &s3.ListObjectsV2Input{Bucket: aws.String(s3Bucket)})
	if err != nil {
		v.t.Fatal(err)
	}
	objs := map[string]string{}
	for _, o := range out.Contents {
		objs[aws.ToString(o.Key)] = aws.ToString(o.ETag)
	}
	return objs
}

// serial is the serial of the state in the bucket.
func (v *versity) serial() int64 {
	v.t.Helper()
	out, err := v.client(s3RootKey, s3RootSecret).GetObject(context.Background(), &s3.GetObjectInput{Bucket: aws.String(s3Bucket), Key: aws.String(s3Key)})
	if err != nil {
		v.t.Fatal(err)
	}
	defer out.Body.Close()
	var st struct {
		Serial int64 `json:"serial"`
	}
	if err := json.NewDecoder(out.Body).Decode(&st); err != nil {
		v.t.Fatal(err)
	}
	return st.Serial
}

// serveAgain starts writ serve with cfg, as runServe does, and returns
// what stops it.
func serveAgain(t *testing.T, s *served, cfg string) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, writBin, "serve", "--config", cfg)
	cmd.Env = os.Environ()
	cmd.Stdout, cmd.Stderr = s.log, s.log
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 30 * time.Second
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stop = func() {
		cancel()
		_ = cmd.Wait()
	}
	t.Cleanup(stop)
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		resp, err := http.Get(s.url + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return stop
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("writ serve did not answer /healthz: %v\n%s", err, s.log.String())
		}
	}
}

func tofuS3Child(t *testing.T) {
	db := os.Getenv(tofuPGEnv)
	u, err := url.Parse(db)
	if err != nil || u.Query().Get("host") == "" {
		t.Fatalf("the database %q names no socket directory", db)
	}
	lock := tofuLock(t)
	// versitygw is in /nix, which the root holds.
	bin, shared := tofuChildRoot(t, u.Query().Get("host"))

	top, err := os.MkdirTemp("/tmp", "writ-tofu-s3-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(top) })
	cache, scratch := filepath.Join(top, "plugins"), filepath.Join(top, "runs")
	if err := os.CopyFS(cache, os.DirFS(shared)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(scratch, 0o755); err != nil {
		t.Fatal(err)
	}
	for d, mode := range map[string]os.FileMode{top: 0o755, scratch: os.ModeSticky | 0o777} {
		if err := os.Chmod(d, mode); err != nil {
			t.Fatal(err)
		}
	}
	gw := startVersity(t, top)
	st := ststest.New()
	t.Cleanup(st.Close)
	st.Static(s3PlanRole, "writ-plan", "plan-secret")
	st.Static(s3ApplyRole, "writ-apply", "apply-secret")
	// The control plane's own identity, which signs only its calls to
	// STS, and nothing of a developer's AWS setup.
	for k, v := range map[string]string{
		"AWS_ACCESS_KEY_ID": "AKIACONTROLPLANE0001", "AWS_SECRET_ACCESS_KEY": "control-plane-secret", "AWS_SESSION_TOKEN": "",
		"AWS_PROFILE": "", "AWS_CONFIG_FILE": os.DevNull, "AWS_SHARED_CREDENTIALS_FILE": os.DevNull, "AWS_EC2_METADATA_DISABLED": "true",
		"AWS_ENDPOINT_URL": "", "AWS_ENDPOINT_URL_S3": "",
	} {
		t.Setenv(k, v)
	}

	is := oidctest.New(t)
	is.SetClient("writ-e2e", servedClientSecret)
	is.SetPerson(oidctest.GitLabPerson())
	g := forgetest.Handler(forgeToken)
	g.URL = is.URL
	is.Mount("/api/v4/", g)
	data := t.TempDir()
	write := func(name, text string) string {
		p := filepath.Join(data, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ca := write("issuer.pem", string(is.CAPEM()))
	tokenFile := write("secrets/gitlab_read", forgeToken+"\n")
	clientSecret := write("secrets/oidc_client_secret", servedClientSecret+"\n")
	canaryFile := filepath.Join(t.TempDir(), "canary")
	if err := os.WriteFile(canaryFile, []byte(tofuCanary+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	srvURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	cfg := write("serve", fmt.Sprintf(`url           = %[1]q
listen        = "127.0.0.1:%[2]d"
database      = %[3]q
data          = %[4]q
provider_uids = "61000-61063"
poll_every    = "1s"

limits {
  requests_per_second  = 100
  requests_per_address = 1000
}

aws {
  region       = "us-east-1"
  sts_endpoint = %[12]q
  session      = "1h"
}

tofu {
  binary       = %[5]q
  plugin_cache = %[6]q
  scratch_dir  = %[7]q
}

drift {
  every = "1h"
}

issuer "gitlab" {
  kind     = "gitlab-ci"
  identity = "gitlab"
  url      = %[8]q
  ca_file  = %[9]q
}

issuer "sso" {
  kind               = "people"
  identity           = "gitlab"
  url                = %[8]q
  ca_file            = %[9]q
  client_id          = "writ-e2e"
  client_secret_file = %[16]q
  groups             = "userinfo"
}

forge "gitlab" {
  kind            = "gitlab"
  url             = %[8]q
  read_token_file = %[10]q
  ca_file         = %[9]q
}

repo {
  name    = "infra"
  forge   = "gitlab"
  project = "acme/infra"
  path    = "infra"
}

team "infra_mr" {
  issuer = "gitlab"
  match  = { project_id = ["4242"], pipeline_source = ["merge_request_event"] }
}

team "infra_reviewers" {
  issuer = "gitlab"
  match  = { project_id = ["4242"], user_access_level = ["maintainer", "owner"] }
}

team "infra_main" {
  issuer = "gitlab"
  match  = { project_id = ["4242"], ref_path = ["refs/heads/main"], ref_protected = ["true"] }
}

grant {
  teams  = ["infra_mr"]
  stacks = ["app"]
  envs   = ["prod"]
  can    = ["read", "plan"]
}

grant {
  teams  = ["infra_reviewers"]
  stacks = ["app"]
  envs   = ["prod"]
  can    = ["approve"]
}

grant {
  teams  = ["infra_main"]
  stacks = ["app"]
  envs   = ["prod"]
  can    = ["read", "apply"]
  risks  = ["create", "update", "replace", "delete"]
}

team "stewards" {
  issuer = "sso"
  match  = { groups = ["acme/platform"] }
}

grant {
  teams  = ["stewards"]
  stacks = ["app"]
  envs   = ["prod"]
  can    = ["read", "adopt"]
}

approval {
  stacks = ["*"]
  envs   = ["prod"]
  risks  = ["create", "update", "replace", "delete", "data-loss"]
  need   = 1
}

env "prod" {
  key          = "keys/prod.key"
  plan_role    = %[13]q
  apply_role   = %[14]q
  s3_endpoints = [%[15]q]
  provider_env = { TF_VAR_canary = %[11]q }
}

`, srvURL, port, db, data, bin, cache, scratch, is.URL, ca, tokenFile, canaryFile, st.URL, s3PlanRole, s3ApplyRole, gw.endpoint, clientSecret))
	g.AddProject(4242, "acme/infra", "main")
	v1 := s3AppFiles(lock, gw.endpoint, "v1")
	g.Commit(4242, "main", v1)
	jobDir := filepath.Join(t.TempDir(), "app")
	if err := os.Mkdir(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	s := &served{t: t, is: is, url: srvURL, db: db, data: data, log: &syncBuffer{}, stack: &stackDir{t: t, dir: jobDir}}
	if r := s.stack.exec(nil, "serve", "--config", cfg, "--check"); r.code != 0 {
		t.Fatalf("writ serve --check: %s", r)
	}
	stop := serveAgain(t, s, cfg)
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if code, doc := s.api("GET", "/v1/repos/infra", s.token(nil), nil); code == 200 && doc["commit"] != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the pinned repository was not read:\n%s", s.log.String())
		}
	}

	pipeline := 1849400
	pipelines := map[string]string{}
	plan := func(branch string, files map[string]forgetest.File) tofuPlanDoc {
		t.Helper()
		head := g.Commit(4242, branch, files)
		pipeline++
		tok := s.token(map[string]any{"sha": head, "ref": branch, "ref_path": "refs/heads/" + branch, "pipeline_id": fmt.Sprint(pipeline)})
		var p tofuPlanDoc
		r := s.writ(0, tok, "plan", "--remote", "--json", "--env", "prod", "--stack", "app", "--commit", head)
		if err := json.Unmarshal([]byte(r.stdout), &p); err != nil || !p.OK || p.Plan.ID == "" {
			t.Fatalf("the plan of app at %s: %v\n%s\n%s", branch, err, r, s.log.String())
		}
		t.Logf("writ plan --remote of app at %s:\n%s", branch, p.Text)
		pipelines[p.Plan.ID] = fmt.Sprint(pipeline)
		return p
	}
	approve := func(id string) {
		t.Helper()
		bob := s.token(map[string]any{"user_id": "2002", "user_login": "bob", "user_access_level": "maintainer", "pipeline_id": pipelines[id]})
		if r := s.writ(0, bob, "approve", "--remote", id); !strings.Contains(r.stdout, ", approved.") {
			t.Fatalf("bob's approval of %s:\n%s", id, r)
		}
	}
	// The drift checks plan the head of main that writ serve read, so an
	// apply waits for writ serve to read the merged commit.
	applied := func(files map[string]forgetest.File) tofuRun {
		t.Helper()
		merged := g.Commit(4242, "main", files)
		for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
			if code, doc := s.api("GET", "/v1/repos/infra", s.token(nil), nil); code == 200 {
				if c, _ := doc["commit"].(map[string]any); c != nil && c["sha"] == merged {
					break
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("writ serve did not read the merged commit %s:\n%s", merged, s.log.String())
			}
		}
		pipeline++
		tok := s.token(map[string]any{"sha": merged, "ref": "main", "ref_path": "refs/heads/main", "ref_protected": "true",
			"pipeline_source": "push", "pipeline_id": fmt.Sprint(pipeline)})
		var run tofuRun
		r := s.writ(0, tok, "apply", "--remote", "--json", "--merged", "--env", "prod", "--stack", "app")
		if err := json.Unmarshal([]byte(r.stdout), &run); err != nil || run.Document == nil {
			t.Fatalf("the apply after the merge: %v\n%s", err, r)
		}
		t.Logf("writ apply --remote --merged:\n%s", run.Text)
		return run
	}
	// mainHead is the head of main that writ serve read.
	mainHead := func() string {
		t.Helper()
		code, doc := s.api("GET", "/v1/repos/infra", s.token(nil), nil)
		c, _ := doc["commit"].(map[string]any)
		sha, _ := c["sha"].(string)
		if code != 200 || sha == "" {
			t.Fatalf("GET /v1/repos/infra: %d %v", code, doc)
		}
		return sha
	}
	actions := func(p tofuPlanDoc) map[string]string {
		out := map[string]string{}
		for _, c := range p.Document.ResourceChanges {
			out[c.Address] = strings.Join(c.Change.Actions, ",")
		}
		return out
	}
	conn, err := pgx.Connect(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	// index requires the state index of app@prod to be of the state at
	// serial, with a row for each resource.
	index := func(serial int64) {
		t.Helper()
		var backend string
		var got int64
		if err := conn.QueryRow(context.Background(), "SELECT backend, version FROM writ_control.state_index_stacks WHERE env = 'prod' AND stack = 'app'").Scan(&backend, &got); err != nil || backend != "s3" || got != serial {
			t.Fatalf("the state index of app@prod is of %s serial %d, want s3 serial %d: %v", backend, got, serial, err)
		}
		rows, err := conn.Query(context.Background(), "SELECT address || ' ' || provider || ' ' || (id <> '')::text FROM writ_control.state_index WHERE env = 'prod' AND stack = 'app' ORDER BY address")
		if err != nil {
			t.Fatal(err)
		}
		lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
		want := []string{
			"random_pet.name registry.opentofu.org/hashicorp/random true",
			"terraform_data.note terraform.io/builtin/terraform true",
			"terraform_data.secret terraform.io/builtin/terraform true",
		}
		if err != nil || !slices.Equal(lines, want) {
			t.Fatalf("the state index of app@prod: %v\n%s\nwant:\n%s", err, strings.Join(lines, "\n"), strings.Join(want, "\n"))
		}
	}
	// drift is the drift marked on app@prod's index record, as "serial N
	// seen" or "none".
	drift := func() string {
		t.Helper()
		var serial, at int64
		err := conn.QueryRow(context.Background(), `SELECT COALESCE((SELECT outside_serial FROM writ_control.state_index_stacks WHERE env = 'prod' AND stack = 'app'), 0),
			COALESCE((SELECT outside_at FROM writ_control.state_index_stacks WHERE env = 'prod' AND stack = 'app'), 0)`).Scan(&serial, &at)
		if err != nil {
			t.Fatal(err)
		}
		if serial == 0 {
			return "none"
		}
		return fmt.Sprintf("serial %d seen %t", serial, at > 0)
	}

	// checked waits for the newest drift check of app@prod to end with
	// status, of commit when it is not "", with no check running, and
	// returns app's row of GET /v1/drift.
	checked := func(status, commit string) server.DriftStack {
		t.Helper()
		for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(500 * time.Millisecond) {
			code, doc := s.api("GET", "/v1/drift?env=prod&stack=app", s.token(nil), nil)
			b, _ := json.Marshal(doc)
			var d server.DriftDoc
			if code == 200 && json.Unmarshal(b, &d) == nil && len(d.Stacks) == 1 && !d.Stacks[0].Checking && d.Stacks[0].Check != nil && d.Stacks[0].Check.Status == status &&
				(commit == "" || d.Stacks[0].Check.Commit == commit) {
				t.Logf("the drift check of app@prod: %s", b)
				return d.Stacks[0]
			}
			if time.Now().After(deadline) {
				t.Fatalf("no drift check of app@prod ended %s: %d %s\n%s", status, code, b, s.log.String())
			}
		}
	}

	// The plan role cannot write the state or its lock file; the stand-in
	// enforces the bucket's policy.
	for _, k := range []string{s3Key, s3Key + ".tflock"} {
		_, err := gw.client("writ-plan", "plan-secret").PutObject(context.Background(), &s3.PutObjectInput{Bucket: aws.String(s3Bucket), Key: aws.String(k), Body: bytes.NewReader([]byte("{}"))})
		if err == nil || !strings.Contains(err.Error(), "api error AccessDenied") {
			t.Fatalf("a write of %s with the plan role's keys: %v, want AccessDenied", k, err)
		}
	}

	// A plan reads no state and writes nothing to the bucket.
	p := plan("add-app", v1)
	want := map[string]string{"random_pet.name": "create", "terraform_data.secret": "create", "terraform_data.note": "create"}
	if got := actions(p); p.Plan.Status != "pending" || p.Plan.Stacks[0].Changes != 3 || !maps.Equal(got, want) {
		t.Fatalf("the first plan: %+v, changes %v", p.Plan, got)
	}
	if objs := gw.objects(); len(objs) != 0 {
		t.Fatalf("the bucket after the first plan holds %v, want nothing", objs)
	}

	// A second person approves, and the job on main applies with the
	// apply role, which takes the lock file and lets it go.
	approve(p.Plan.ID)
	run := applied(v1)
	doc := run.Document
	first := gw.serial()
	if !run.OK || run.Run.Status != "applied" || !doc.Applied || first == 0 || doc.StateVersion != first || doc.Summary == nil || *doc.Summary != (server.TofuApplySummary{Add: 3}) {
		t.Fatalf("the first apply, with the bucket's state at serial %d: %+v %+v\n%s", first, run.Run, doc, run.Text)
	}
	objs := gw.objects()
	if _, ok := objs[s3Key]; !ok || len(objs) != 1 {
		t.Fatalf("the bucket after the apply holds %v, want only %s", objs, s3Key)
	}
	index(first)

	// The apply makes a drift check due: the head of main plans no change
	// against the bucket's state, and writes nothing. Its plan is never
	// approved.
	clean := checked("clean", "")
	if c := clean.Check; clean.Backend != "s3" || clean.Resources != 3 || clean.IndexedVersion != first || clean.OutsideWrite != nil || c.Plan == "" || c.Repo != "infra" || c.Outside+c.Code != 0 {
		t.Fatalf("app@prod after the first apply: %+v %+v", clean, clean.Check)
	}
	if after := gw.objects(); !maps.Equal(after, objs) {
		t.Fatalf("a drift check changed the bucket: %v, then %v", objs, after)
	}
	bob := s.token(map[string]any{"user_id": "2002", "user_login": "bob", "user_access_level": "maintainer"})
	if r := s.writ(1, bob, "approve", "--remote", clean.Check.Plan); !strings.Contains(r.stdout+r.stderr, "is a drift check of app@prod, which writ serve never approves or applies") {
		t.Fatalf("bob's approval of the drift plan %s:\n%s", clean.Check.Plan, r)
	}

	// The same files plan no changes, and the plan writes nothing.
	if again := plan("again", v1); again.Plan.Stacks[0].Changes != 0 || !strings.Contains(again.Text, "No changes.") {
		t.Fatalf("the plan of the applied files: %+v, changes %v\n%s", again.Plan, actions(again), again.Text)
	}
	if after := gw.objects(); !maps.Equal(after, objs) {
		t.Fatalf("a plan changed the bucket: %v, then %v", objs, after)
	}
	if got := drift(); got != "none" {
		t.Fatalf("drift %s before any write outside Writ", got)
	}

	// A plan and apply of an edit go on from Writ's serial, and the drift
	// check the apply makes due finds no drift.
	v2 := s3AppFiles(lock, gw.endpoint, "v2")
	edit := plan("edit", v2)
	if got := actions(edit); edit.Plan.Stacks[0].Changes != 1 || got["terraform_data.note"] != "update" {
		t.Fatalf("the plan of the edit: %+v, changes %v", edit.Plan, got)
	}
	approve(edit.Plan.ID)
	run = applied(v2)
	last := gw.serial()
	if run.Run.Status != "applied" || last <= first || run.Document.StateVersion != last || run.Document.Summary == nil || *run.Document.Summary != (server.TofuApplySummary{Change: 1}) {
		t.Fatalf("the apply of the edit, with the bucket's state at serial %d: %+v %+v\n%s", last, run.Run, run.Document, run.Text)
	}
	index(last)
	if cleared := checked("clean", mainHead()); cleared.Check.DriftedSince != "" || cleared.Check.Code != 0 || cleared.OutsideWrite != nil {
		t.Fatalf("app@prod after the apply of the edit: %+v %+v", cleared, cleared.Check)
	}

	// Someone applies an edit with the bucket owner's keys, outside Writ.
	direct := filepath.Join(top, "direct")
	if err := os.Mkdir(direct, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, f := range s3AppFiles(lock, gw.endpoint, "outside") {
		if err := os.WriteFile(filepath.Join(direct, filepath.Base(name)), []byte(f.Content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-input=false", "-lockfile=readonly", "-no-color"}, {"apply", "-input=false", "-auto-approve", "-no-color"}} {
		cmd := exec.Command(bin, args...)
		cmd.Dir = direct
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + direct, "TF_PLUGIN_CACHE_DIR=" + shared, "TF_CLI_CONFIG_FILE=" + os.DevNull,
			"CHECKPOINT_DISABLE=1", "TF_VAR_canary=" + tofuCanary, "AWS_ACCESS_KEY_ID=" + s3RootKey, "AWS_SECRET_ACCESS_KEY=" + s3RootSecret,
			"AWS_CONFIG_FILE=" + os.DevNull, "AWS_SHARED_CREDENTIALS_FILE=" + os.DevNull, "AWS_EC2_METADATA_DISABLED=true"}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("tofu %s outside Writ: %v\n%s", args[0], err, out)
		}
	}
	written := gw.serial()
	if written <= last {
		t.Fatalf("the apply outside Writ left serial %d, after Writ's %d", written, last)
	}

	// writ serve's refresh at start reads the new serial with the plan
	// role, marks it as drift on the stack's index record, and leaves the
	// index at Writ's serial: no mask knew that apply's secrets.
	stop()
	s.log = &syncBuffer{}
	serveAgain(t, s, cfg)
	wantDrift := fmt.Sprintf("serial %d seen true", written)
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(200 * time.Millisecond) {
		got := drift()
		if got == wantDrift {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the refresh: drift %s, want %s\n%s", got, wantDrift, s.log.String())
		}
	}
	for deadline := time.Now().Add(time.Minute); !strings.Contains(s.log.String(), fmt.Sprintf("serial %d of the state is not indexed: no apply of writ serve wrote it", written)); time.Sleep(200 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the refresh did not say why it left the index:\n%s", s.log.String())
		}
	}
	index(last)
	// No secret set was sealed with the serial the apply outside Writ
	// wrote, so a drift check of it finds drift unknown and keeps nothing
	// of its plan.
	unknown := checked("failed", "")
	if c := unknown.Check; unknown.OutsideWrite == nil || unknown.OutsideWrite.Serial != written || c.Error != fmt.Sprintf("drift unknown: no secret set for state version %d of app@prod", written) ||
		c.Plan != "" || c.Outside+c.Code != 0 || len(c.Changes) != 0 || c.DriftedSince != "" {
		t.Fatalf("app@prod after the apply outside Writ: %+v %+v", unknown, unknown.Check)
	}
	if !strings.Contains(s.log.String(), `"msg":"state written outside writ"`) {
		t.Fatalf("writ serve logged no drift:\n%s", s.log.String())
	}
	for line := range strings.Lines(s.log.String()) {
		if strings.Contains(line, "state written outside writ") || strings.Contains(line, "is not indexed") {
			t.Logf("writ serve logged: %s", strings.TrimSpace(line))
		}
	}

	// A plan of the serial written outside Writ keeps no detail: it fails
	// and says why, no one can approve it, and it marks nothing new.
	head := g.Commit(4242, "after-outside", v2)
	pipeline++
	tok := s.token(map[string]any{"sha": head, "ref": "after-outside", "ref_path": "refs/heads/after-outside", "pipeline_id": fmt.Sprint(pipeline)})
	var failed tofuPlanDoc
	r := s.writ(1, tok, "plan", "--remote", "--json", "--env", "prod", "--stack", "app", "--commit", head)
	why := fmt.Sprintf("no secret set for state version %d of app@prod, so writ serve keeps no detail of a plan against it and never approves one", written)
	if err := json.Unmarshal([]byte(r.stdout), &failed); err != nil || failed.OK || failed.Plan.Status != "failed" || failed.Plan.ID != "" || !strings.Contains(failed.Text, why) || len(failed.Document.ResourceChanges) != 0 {
		t.Fatalf("the plan after the apply outside Writ: %v\n%s", err, r)
	}
	if code, doc := s.api("POST", "/v1/plans/"+failed.Plan.Ref+"/approvals", bob, nil); code == 200 {
		t.Fatalf("bob approved the plan after the apply outside Writ: %v", doc)
	}
	if got := drift(); got != wantDrift {
		t.Fatalf("at the end: drift %s, want %s", got, wantDrift)
	}
	if objs := gw.objects(); len(objs) != 1 {
		t.Fatalf("the bucket at the end holds %v, want only %s: a lock file was left", objs, s3Key)
	}

	// bob signs in and adopts the serial the apply outside Writ wrote: a
	// run pulls it with the plan role, and writ serve seals with it the
	// values the state marks sensitive. The drift check the adoption
	// makes due knows the drift again, and a plan of it is approvable and
	// applies from that serial.
	env := []string{"XDG_CONFIG_HOME=" + t.TempDir(), "XDG_DATA_HOME=" + t.TempDir(), "WRIT_SERVER=" + s.url, "WRIT_ID_TOKEN="}
	if out := signIn(t, s, env); !strings.Contains(out, "Signed in to "+s.url+" as bob (") {
		t.Fatalf("writ login printed:\n%s", out)
	}
	asBob := func(want int, args ...string) result {
		t.Helper()
		r := s.stack.exec(env, args...)
		s.outsMu.Lock()
		s.outs = append(s.outs, r.stdout, r.stderr)
		s.outsMu.Unlock()
		if r.code != want {
			t.Fatalf("want exit %d\n%s\nserve log:\n%s", want, r, s.log.String())
		}
		return r
	}
	before := gw.objects()
	r = asBob(1, "state", "adopt", "app", "--env", "prod", "--serial", fmt.Sprint(written-1), "--reason", "a tofu apply from a laptop, OPS-311")
	if !strings.Contains(r.stdout+r.stderr, fmt.Sprintf("is not the newest that writ serve read, serial %d", written)) {
		t.Fatalf("an adoption of an older serial:\n%s", r)
	}
	r = asBob(0, "state", "adopt", "app", "--env", "prod", "--serial", fmt.Sprint(written), "--reason", "a tofu apply from a laptop, OPS-311")
	t.Logf("writ state adopt:\n%s", r.stdout)
	for _, want := range []string{fmt.Sprintf("Adopted serial %d of app@prod, lineage ", written), "The state index holds its 3 resources.", "warning: " + server.AdoptLimit} {
		if !strings.Contains(r.stdout, want) {
			t.Fatalf("writ state adopt lacks %q:\n%s", want, r)
		}
	}
	index(written)
	if after := gw.objects(); !maps.Equal(after, before) {
		t.Fatalf("the adoption changed the bucket: %v, then %v", before, after)
	}
	var adopted int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM writ_control.audit WHERE kind = 'state-adopt' AND op = 'adopt' AND env = 'prod' AND person = 'gitlab/user/2002' AND reason LIKE $1",
		fmt.Sprintf("adopted serial %d of lineage %%: a tofu apply from a laptop, OPS-311", written)).Scan(&adopted); err != nil || adopted != 1 {
		t.Fatalf("%d audit rows of the adoption: %v", adopted, err)
	}
	if known := checked("drifted", mainHead()); known.Check.Code != 1 || known.Check.Error != "" {
		t.Fatalf("the drift check after the adoption: %+v %+v", known, known.Check)
	}
	again := plan("after-adopt", v2)
	if got := actions(again); again.Plan.Status != "pending" || again.Plan.Stacks[0].Changes != 1 || got["terraform_data.note"] != "update" {
		t.Fatalf("the plan after the adoption: %+v, changes %v", again.Plan, got)
	}
	approve(again.Plan.ID)
	run = applied(v2)
	final := gw.serial()
	if run.Run.Status != "applied" || final <= written || run.Document.StateVersion != final {
		t.Fatalf("the apply after the adoption, with the bucket's state at serial %d: %+v %+v\n%s", final, run.Run, run.Document, run.Text)
	}
	index(final)

	// Plans and refreshes assumed the plan role, applies the apply role.
	roles := map[string]int{}
	for _, req := range st.Requests() {
		if req.Action == "AssumeRole" {
			roles[req.Form.Get("RoleArn")]++
		}
	}
	if roles[s3ApplyRole] != 3 || roles[s3PlanRole] < 6 || len(roles) != 2 {
		t.Fatalf("STS assumed %v, want the apply role for the three applies and the plan role for the rest", roles)
	}

	// No state token was issued, and the canary is in no row, file,
	// output, or log line.
	var tokens, versions int
	if err := conn.QueryRow(context.Background(), "SELECT (SELECT count(*) FROM writ_control.tfstate_tokens), (SELECT count(*) FROM writ_control.tfstate_versions)").Scan(&tokens, &versions); err != nil || tokens != 0 || versions != 0 {
		t.Fatalf("the http backend holds %d tokens and %d state versions of a stack on S3: %v", tokens, versions, err)
	}
	dump := dumpControl(t, conn)
	s.outsMu.Lock()
	outs := append(slices.Clone(s.outs), dump...)
	s.outsMu.Unlock()
	scanForCanary(t, data, outs, s.log.String())
}
