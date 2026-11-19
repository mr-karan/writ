package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/awsrole"
	"github.com/mr-karan/writ/internal/awsrole/ststest"
	"github.com/mr-karan/writ/internal/server"
	"github.com/mr-karan/writ/internal/tofu"
)

// TestPlanRole plans in an environment with a plan role, against the STS
// stand-in through the AWS SDK. The plan's providers get the run's own
// environment, a uid of their own, and an endpoint whose credentials are
// the plan role's, assumed with the session name, source identity, and
// duration the config and the token give. The token dies with the run.
func TestPlanRole(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIACONTROLPLANE0001")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "control-plane-secret")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	st := ststest.New()
	defer st.Close()
	assumer, err := awsrole.NewSTS(context.Background(), awsrole.STSOptions{Region: "eu-west-1", Endpoint: st.URL})
	if err != nil {
		t.Fatal(err)
	}
	nomad := filepath.Join(dir, "nomad_token")
	if err := os.WriteFile(nomad, []byte("nomad-token-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const role = "arn:aws:iam::222222222222:role/writ-plan"
	var mu sync.Mutex
	var reaped []uint32
	runner := &fakeTofu{}
	f := newFixture(t, func(s string) string {
		s = withForge(t, "")(s)
		s = strings.Replace(s, "key = \"prod.key\"\n", fmt.Sprintf("key = \"prod.key\"\n  plan_role = %q\n  provider_env = { NOMAD_TOKEN = %q }\n", role, nomad), 1)
		return s + fmt.Sprintf("aws {\n  region = \"eu-west-1\"\n  sts_endpoint = %q\n  session = \"30m\"\n}\nprovider_uids = \"61000-61063\"\n", st.URL)
	}, func(o *server.Options) {
		o.Tofu = runner
		o.Assumer = assumer
		o.Reap = func(uid uint32) error {
			mu.Lock()
			defer mu.Unlock()
			reaped = append(reaped, uid)
			return nil
		}
	})
	if len(reaped) != 64 || reaped[0] != 61000 || reaped[63] != 61063 {
		t.Fatalf("at start, writ serve reaped %v, want every provider uid", reaped)
	}

	f.tofu = runner
	f.addInfra()
	var fetched map[string]string
	runner.plan = func() (tofu.Plan, error) {
		runner.mu.Lock()
		run := runner.stacks[len(runner.stacks)-1]
		runner.mu.Unlock()
		env := envMap(run.Creds)
		req, _ := http.NewRequest("GET", env["AWS_CONTAINER_CREDENTIALS_FULL_URI"], nil)
		req.Header.Set("Authorization", env["AWS_CONTAINER_AUTHORIZATION_TOKEN"])
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			return createOnly()
		}
		defer resp.Body.Close()
		_ = json.NewDecoder(resp.Body).Decode(&fetched)
		return createOnly()
	}
	tok := f.mrToken(nil, nil)
	r := f.do("POST", "/v1/plans", tok, planRequest(nil))
	ref := r.doc["plan"].(map[string]any)["ref"].(string)
	if got := f.do("GET", "/v1/plans/"+ref+"?wait=10s", tok, nil); got.doc["plan"].(map[string]any)["status"] != "approved" {
		t.Fatalf("the plan: %s", got.body)
	}

	reqs := st.Requests()
	if len(reqs) != 1 {
		t.Fatalf("STS got %d requests, want one AssumeRole", len(reqs))
	}
	form := reqs[0].Form
	want := map[string]string{"Action": "AssumeRole", "RoleArn": role, "RoleSessionName": "writ-ci-" + ref, "SourceIdentity": "gitlab-4242-1849213", "DurationSeconds": "1800"}
	for k, v := range want {
		if form.Get(k) != v {
			t.Errorf("AssumeRole %s = %q, want %q", k, form.Get(k), v)
		}
	}
	if reqs[0].AccessKeyID != "AKIACONTROLPLANE0001" {
		t.Errorf("AssumeRole was signed by %s, not the control plane's key", reqs[0].AccessKeyID)
	}
	issued, ok := st.Issued(fetched["AccessKeyId"])
	if !ok || issued.SessionName != "writ-ci-"+ref || fetched["Token"] != issued.SessionToken || fetched["AccountId"] != "222222222222" {
		t.Fatalf("during the plan the endpoint served %v, which STS did not issue for this run", fetched)
	}
	// The control store records the run's session as Writ's own.
	sessions, err := f.store.RoleSessions(context.Background(), role, "writ-ci-"+ref)
	if err != nil || len(sessions) != 1 || sessions[0].SourceIdentity != "gitlab-4242-1849213" || !sessions[0].Until.Equal(issued.Expires) ||
		sessions[0].From.After(time.Now()) || sessions[0].Until.Sub(sessions[0].From) > 31*time.Minute {
		t.Fatalf("recorded sessions %+v, %v; STS issued %+v", sessions, err, issued)
	}

	l := runner.stacks[0]
	if l.User == nil || l.User.UID < 61000 || l.User.UID > 61063 || l.User.GID != l.User.UID {
		t.Fatalf("the run's user %+v", l.User)
	}
	env := envMap(l.Creds)
	var names []string
	for k := range env {
		names = append(names, k)
	}
	slices.Sort(names)
	if fmt.Sprint(names) != "[AWS_CONTAINER_AUTHORIZATION_TOKEN AWS_CONTAINER_CREDENTIALS_FULL_URI NOMAD_TOKEN]" {
		t.Fatalf("the run's credentials are %v", names)
	}
	if env["NOMAD_TOKEN"] != "nomad-token-value" || !strings.HasPrefix(env["AWS_CONTAINER_CREDENTIALS_FULL_URI"], "http://127.0.0.1:") {
		t.Fatalf("the providers' environment: %v", env)
	}
	mu.Lock()
	freed := reaped[len(reaped)-1]
	mu.Unlock()
	if freed != l.User.UID {
		t.Fatalf("after the plan, writ serve reaped %d, not the plan's uid %d", freed, l.User.UID)
	}
	req, _ := http.NewRequest("GET", env["AWS_CONTAINER_CREDENTIALS_FULL_URI"], nil)
	req.Header.Set("Authorization", env["AWS_CONTAINER_AUTHORIZATION_TOKEN"])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("after the plan, its token got %d, want 401", resp.StatusCode)
	}

	t.Run("a role STS refuses fails the plan before providers start", func(t *testing.T) {
		st.Deny(role)
		tok := f.mrToken(nil, map[string]any{"jti": "denied-role"})
		r := f.do("POST", "/v1/plans", tok, planRequest(nil))
		ref := r.doc["plan"].(map[string]any)["ref"].(string)
		got := f.do("GET", "/v1/plans/"+ref+"?wait=10s", tok, nil)
		doc, _ := got.doc["document"].(map[string]any)
		ds, _ := doc["diagnostics"].([]any)
		if got.doc["plan"].(map[string]any)["status"] != "failed" || len(ds) != 1 {
			t.Fatalf("the plan: %s", got.body)
		}
		d := ds[0].(map[string]any)
		if d["class"] != "denied" || !strings.Contains(d["message"].(string), role) || !strings.Contains(d["message"].(string), "AccessDenied") {
			t.Fatalf("the diagnostic: %v", d)
		}
		if len(runner.stacks) != 1 {
			t.Fatal("OpenTofu ran a plan whose role STS refused")
		}
	})
}

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

// TestApplyRoleAlone applies in an environment that names an apply role
// and no plan role: the plan assumes nothing, and the apply's providers
// get the apply role's credentials.
func TestApplyRoleAlone(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIACONTROLPLANE0001")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "control-plane-secret")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	st := ststest.New()
	defer st.Close()
	assumer, err := awsrole.NewSTS(context.Background(), awsrole.STSOptions{Region: "eu-west-1", Endpoint: st.URL})
	if err != nil {
		t.Fatal(err)
	}
	const role = "arn:aws:iam::000000000000:role/writ-apply"
	runner := &fakeTofu{}
	f := tofuApplyFixtureEdit(t, runner, func(s string) string {
		s = strings.Replace(s, "key = \"prod.key\"\n", fmt.Sprintf("key = \"prod.key\"\n  apply_role = %q\n", role), 1)
		return s + fmt.Sprintf("aws {\n  region = \"eu-west-1\"\n  sts_endpoint = %q\n}\n", st.URL)
	}, func(o *server.Options) { o.Assumer = assumer })
	p, _ := f.planCommit(tofuFiles(), "add-cert")
	if len(st.Requests()) != 0 {
		t.Fatalf("the plan assumed a role: %+v", st.Requests())
	}
	if r := f.do("POST", "/v1/plans/"+p.Plan.ID+"/approvals", bob(f), nil); r.status != http.StatusOK {
		t.Fatalf("bob's approval: %d %s", r.status, r.body)
	}
	run := f.tofuRun(bob(f), map[string]any{"plan": p.Plan.ID})
	if run.Run.Status != "applied" {
		t.Fatalf("run %+v\n%s", run.Run, run.Text)
	}
	reqs := st.Requests()
	if len(reqs) != 1 || reqs[0].Form.Get("RoleArn") != role {
		t.Fatalf("STS got %+v, want one AssumeRole of the apply role", reqs)
	}
	if env := envMap(runner.applied[0].stack.Creds); !strings.HasPrefix(env["AWS_CONTAINER_CREDENTIALS_FULL_URI"], "http://127.0.0.1:") {
		t.Fatalf("the apply's credentials: %v", env)
	}
}
