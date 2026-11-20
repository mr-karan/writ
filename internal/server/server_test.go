package server_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/control/controltest"
	"github.com/mr-karan/writ/internal/control/pgtest"
	"github.com/mr-karan/writ/internal/forge/forgetest"
	"github.com/mr-karan/writ/internal/oidc/oidctest"
	"github.com/mr-karan/writ/internal/server"
	"github.com/mr-karan/writ/internal/tofu"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

const audience = "http://127.0.0.1:8080"

type fixture struct {
	t  *testing.T
	is *oidctest.Issuer
	// gitlab answers GitLab's REST API on the issuer's host, as GitLab
	// serves both, with the token in gitlabToken.
	gitlab *forgetest.GitLab
	store  *control.Store
	srv    *server.Server
	http   *httptest.Server
	// tofu is the stand-in for OpenTofu of planFixture and the tofu
	// fixtures, nil on a plain fixture.
	tofu   *fakeTofu
	secret string
	// dir holds the config, whose relative paths are relative to it.
	dir string
	cfg *server.Config
	// loc is where the control database is, for a test's own SQL.
	loc string
}

func newFixture(t *testing.T, edit func(string) string, setup ...func(*server.Options)) *fixture {
	t.Helper()
	return newFixtureOn(t, controltest.Default(), edit, setup...)
}

// newFixtureOn is newFixture with its control database on e.
func newFixtureOn(t *testing.T, e controltest.Engine, edit func(string) string, setup ...func(*server.Options)) *fixture {
	t.Helper()
	is := oidctest.New(t)
	gl := forgetest.Handler(gitlabToken)
	gl.URL = is.URL
	is.Mount("/api/v4/", gl)
	dir := t.TempDir()
	ca := filepath.Join(dir, "issuer.pem")
	if err := os.WriteFile(ca, is.CAPEM(), 0o600); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(dir, "db_password")
	if err := os.WriteFile(secret, []byte("s3cret-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, loc := controltest.Open(t, e)
	db := loc
	if !strings.HasPrefix(loc, "postgres://") {
		db = "postgres://writ@localhost/unused"
	}
	text := fmt.Sprintf(`url = %q
listen = "127.0.0.1:8080"
database = %q
data = %q

issuer "gitlab" {
  kind = "gitlab-ci"
  identity = "gitlab"
  url = %q
  ca_file = %q
}

team "infra_mr" {
  issuer = "gitlab"
  match = { project_id = ["4242"], pipeline_source = ["merge_request_event"] }
}

team "reviewers" {
  issuer = "gitlab"
  match = { project_id = ["4242"], user_access_level = ["maintainer"] }
}

grant {
  teams = ["infra_mr"]
  stacks = ["*"]
  envs = ["prod"]
  can = ["read", "plan"]
}

grant {
  teams = ["reviewers"]
  stacks = ["network"]
  envs = ["prod"]
  can = ["apply"]
  risks = ["create"]
}

env "prod" {
  key = "prod.key"
}

`, audience, db, dir, is.URL, ca)
	if edit != nil {
		text = edit(text)
	}
	cfg, diags := server.ParseConfig(filepath.Join(dir, "serve"), []byte(text))
	if diags.HasErrors() {
		t.Fatal(diags)
	}
	v, err := server.NewVerifier(context.Background(), cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	opts := server.Options{Config: cfg, Store: store, Verifier: v, Version: "test"}
	for _, fn := range setup {
		fn(&opts)
	}
	srv, err := server.New(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { h.Close(); srv.Close() })
	return &fixture{t: t, is: is, gitlab: gl, store: store, srv: srv, http: h, secret: "s3cret-value", dir: dir, cfg: cfg, loc: loc}
}

func (f *fixture) token(edits map[string]any) string {
	c := oidctest.GitLabJob(f.is.URL, audience, time.Now())
	for k, v := range edits {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	return oidctest.Sign(f.is.Keys()[0], c)
}

type response struct {
	status int
	header http.Header
	body   []byte
	doc    map[string]any
}

func (r response) class() string {
	ds, _ := r.doc["diagnostics"].([]any)
	if len(ds) == 0 {
		return ""
	}
	c, _ := ds[0].(map[string]any)["class"].(string)
	return c
}

func (f *fixture) do(method, path, token string, body io.Reader, hdr ...string) response {
	f.t.Helper()
	req, err := http.NewRequest(method, f.http.URL+path, body)
	if err != nil {
		f.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	out := response{status: resp.StatusCode, header: resp.Header, body: b}
	if err := json.Unmarshal(b, &out.doc); err != nil {
		f.t.Fatalf("%s %s: the body is not JSON: %s", method, path, b)
	}
	return out
}

// planRequest is a commit plan of network at the commit the CI job's
// token names.
func planRequest(edit func(map[string]any)) *bytes.Reader {
	req := map[string]any{
		"format": "writ.plan-request/v1", "env": "prod", "stacks": []string{"network"},
		"hints": map[string]any{"merge_request": 42}, "source": map[string]any{},
	}
	if edit != nil {
		edit(req)
	}
	b, _ := json.Marshal(req)
	return bytes.NewReader(b)
}

// createOnly is a plan of OpenTofu that creates one resource.
func createOnly() (tofu.Plan, error) {
	return tofuPlanOf(`["create"]`, `"before":null,"after":{"length":2},"after_unknown":{"id":true},"before_sensitive":false,"after_sensitive":false`)()
}

// uidLines are the config lines that give OpenTofu runs provider uids.
func uidLines(t *testing.T) string {
	return `provider_uids = "61000-61063"` + "\n"
}

// planFixture is a control plane that plans OpenTofu stacks with a
// stand-in for OpenTofu whose plans create one resource, and provider
// uids. edit must add the forge, as withForge does; addInfra adds the
// repository.
func planFixture(t *testing.T, edit func(string) string, setup ...func(*server.Options)) *fixture {
	t.Helper()
	return planFixtureUIDs(t, uidLines(t), edit, setup...)
}

// planFixtureUIDs is planFixture with uids, the config's provider_uids
// and providers_dir.
func planFixtureUIDs(t *testing.T, uids string, edit func(string) string, setup ...func(*server.Options)) *fixture {
	t.Helper()
	return tofuFixtureEdit(t, &fakeTofu{plan: createOnly}, uids, edit, setup...)
}

// mrToken commits infraFiles, with the files in more, to the merge
// request's branch and returns the token of a job of its pipeline, with
// edits, at that commit.
func (f *fixture) mrToken(more map[string]forgetest.File, edits map[string]any) string {
	f.t.Helper()
	files := infraFiles()
	maps.Copy(files, more)
	sha := f.gitlab.Commit(4242, "add-subnet-d", files)
	claims := map[string]any{"sha": sha}
	maps.Copy(claims, edits)
	return f.token(claims)
}

func (f *fixture) audit(filter controltest.AuditFilter) []control.Entry {
	f.t.Helper()
	return controltest.Audit(f.t, f.loc, filter)
}

func TestPlanFlow(t *testing.T) {
	f := planFixture(t, withForge(t, ""))
	f.addInfra()
	tok := f.mrToken(nil, nil)
	r := f.do("POST", "/v1/plans", tok, planRequest(nil))
	if r.status != http.StatusAccepted {
		t.Fatalf("status %d: %s", r.status, r.body)
	}
	plan := r.doc["plan"].(map[string]any)
	ref := plan["ref"].(string)
	if r.header.Get("Location") != "/v1/plans/"+ref || plan["status"] != "planning" || plan["id"] != "" {
		t.Fatalf("202 %s", r.body)
	}
	got := f.do("GET", "/v1/plans/"+ref+"?wait=10s", tok, nil)
	plan = got.doc["plan"].(map[string]any)
	id := plan["id"].(string)
	if got.status != 200 || plan["status"] != "approved" || len(id) != 32 || got.doc["text"] == "" {
		t.Fatalf("after the wait: %s", got.body)
	}
	if plan["expires_at"] == "" || fmt.Sprint(plan["risks"]) != "[create]" {
		t.Fatalf("plan %v", plan)
	}
	if fmt.Sprint(plan["teams_that_may_apply"]) != "[reviewers]" {
		t.Fatalf("teams that may apply: %v", plan["teams_that_may_apply"])
	}
	origin := plan["origin"].(map[string]any)
	if origin["kind"] != "gitlab-merge-request" || origin["merge_request"] != 42.0 || origin["project_id"] != "4242" || origin["pipeline"] != "1849213" {
		t.Fatalf("origin %v", origin)
	}
	author := plan["author"].(map[string]any)
	if author["person"] != "gitlab/user/1001" || !strings.Contains(author["display"].(string), "(alice)") {
		t.Fatalf("author %v", author)
	}
	if byID := f.do("GET", "/v1/plans/"+id, tok, nil); byID.doc["plan"].(map[string]any)["ref"] != ref {
		t.Fatalf("by id: %s", byID.body)
	}
	list := f.do("GET", "/v1/plans?env=prod&stack=network", tok, nil)
	if plans := list.doc["plans"].([]any); len(plans) != 1 {
		t.Fatalf("list %s", list.body)
	}
	// OpenTofu planned the stack's directory once.
	if len(f.tofu.stacks) != 1 || f.tofu.stacks[0].Dir != "network" {
		t.Fatalf("stacks OpenTofu planned: %+v", f.tofu.stacks)
	}
	// The same token cannot make a second plan.
	again := f.do("POST", "/v1/plans", tok, planRequest(nil))
	if again.status != http.StatusUnauthorized || again.class() != "auth" || !strings.Contains(string(again.body), "already made a plan") {
		t.Fatalf("replay: %d %s", again.status, again.body)
	}
	// Every call and the plan's end left an audit row.
	rows := f.audit(controltest.AuditFilter{Plan: id})
	if len(rows) < 2 || rows[len(rows)-1].Kind != "plan-finished" && rows[0].Kind != "plan-finished" {
		t.Fatalf("audit rows of the plan: %+v", rows)
	}
	// Reads of the plan name it too; the request that made it is the POST.
	var created []control.Entry
	for _, e := range f.audit(controltest.AuditFilter{Plan: ref}) {
		if e.Method == "POST" {
			created = append(created, e)
		}
	}
	if len(created) != 1 || created[0].Decision != control.Allowed || created[0].Status != 202 || created[0].Op != "plan" || fmt.Sprint(created[0].Grants) != "[2 3]" {
		t.Fatalf("audit of the request: %+v", created)
	}
	c := created[0]
	if fmt.Sprint(c.Stacks) != "[network]" || c.PrincipalKind != "ci-job" || c.Via != "gitlab job 9922101" {
		t.Fatalf("audit columns: %+v", c)
	}
	finished := 0
	for _, e := range f.audit(controltest.AuditFilter{Plan: id, Stack: "network"}) {
		if e.Kind == "plan-finished" {
			finished++
			if fmt.Sprint(e.Risks) != "[create]" || e.Reason != "approved" {
				t.Fatalf("audit of the plan's end: %+v", e)
			}
		}
	}
	if finished != 1 {
		t.Fatalf("%d plan-finished rows", finished)
	}
	replays := f.audit(controltest.AuditFilter{Decision: control.AuthFailed})
	if len(replays) != 1 || replays[0].Check != "replay" {
		t.Fatalf("audit of the replay: %+v", replays)
	}
}

func TestRefusals(t *testing.T) {
	f := newFixture(t, func(s string) string {
		return s + "limits {\n  requests_per_second = 1000\n  requests_per_address = 1000\n}\n"
	})
	other := oidctest.NewRSAKey(t, "rsa-1", 2048)
	tests := []struct {
		name   string
		token  string
		body   io.Reader
		hdr    []string
		status int
		class  string
		want   string
	}{
		{"no token", "", planRequest(nil), nil, 401, "auth", "no token"},
		{"session token", "writs_abc", planRequest(nil), nil, 401, "auth", "not one writ login gave"},
		{"wrong audience", f.token(map[string]any{"aud": "https://vault.example.com"}), planRequest(nil), nil, 401, "auth", "audience"},
		{"expired", f.token(map[string]any{"exp": time.Now().Add(-time.Hour).Unix()}), planRequest(nil), nil, 401, "auth", "expired"},
		{"forged", oidctest.Sign(other, oidctest.GitLabJob(f.is.URL, audience, time.Now())), planRequest(nil), nil, 401, "auth", "signature"},
		{"no grant", f.token(map[string]any{"project_id": "9999"}), planRequest(nil), nil, 403, "denied", "may not plan network@prod"},
		{"unknown env", f.token(nil), planRequest(func(m map[string]any) { m["env"] = "dev" }), nil, 400, "usage", "not one writ serve plans in"},
		{"two stacks", f.token(nil), planRequest(func(m map[string]any) { m["stacks"] = []string{"a", "b"} }), nil, 400, "usage", "exactly one stack"},
		{"bad stack name", f.token(nil), planRequest(func(m map[string]any) { m["stacks"] = []string{"../etc"} }), nil, 400, "usage", "stack name"},
		{"a state location", f.token(nil), planRequest(func(m map[string]any) { m["state"] = "s3://other-bucket/estate" }), nil, 400, "usage", "unknown field"},
		{"files of the stack", f.token(nil), planRequest(func(m map[string]any) { m["bundle"] = map[string]any{"format": "writ.bundle/v0"} }), nil, 400, "usage", `unknown field \"bundle\"`},
		{"params", f.token(nil), planRequest(func(m map[string]any) { m["params"] = map[string]string{"a": "b"} }), nil, 400, "usage", `unknown field \"params\"`},
		{"no source", f.token(nil), planRequest(func(m map[string]any) { delete(m, "source") }), nil, 400, "usage", "a plan needs source"},
		{"wrong format", f.token(nil), planRequest(func(m map[string]any) { m["format"] = "writ.plan-request/v9" }), nil, 400, "usage", "format"},
		{"not json", f.token(nil), strings.NewReader("{"), nil, 400, "usage", "not the JSON document"},
		{"two documents", f.token(nil), io.MultiReader(planRequest(nil), strings.NewReader(`{}`)), nil, 400, "usage", "more than one"},
		{"oversized", f.token(nil), bytes.NewReader(bytes.Repeat([]byte(" "), 17<<20)), nil, 413, "usage", "over"},
		{"gzip bomb", f.token(nil), gzipped(bytes.Repeat([]byte(" "), 17<<20)), []string{"Content-Encoding", "gzip"}, 413, "usage", "after decompression"},
		{"not gzip", f.token(nil), planRequest(nil), []string{"Content-Encoding", "gzip"}, 400, "usage", "not gzip"},
		{"other encoding", f.token(nil), planRequest(nil), []string{"Content-Encoding", "br"}, 400, "usage", "not gzip"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := f.do("POST", "/v1/plans", tt.token, tt.body, tt.hdr...)
			if r.status != tt.status || r.class() != tt.class || !strings.Contains(string(r.body), tt.want) || r.doc["ok"] != false || r.doc["format"] != "writ.remote-plan/v0" {
				t.Fatalf("got %d %s, want %d %s %q", r.status, r.body, tt.status, tt.class, tt.want)
			}
			if strings.Contains(string(r.body), tt.token) && len(tt.token) > 20 {
				t.Fatal("the response echoes the token")
			}
			id := r.header.Get("X-Request-Id")
			rows := f.audit(controltest.AuditFilter{Request: id})
			if len(rows) != 1 || rows[0].Status != tt.status || rows[0].Decision == control.Allowed {
				t.Fatalf("audit of %s: %+v", id, rows)
			}
		})
	}
	if rows := f.audit(controltest.AuditFilter{Decision: control.Denied}); len(rows) != 1 || rows[0].Person == "" || rows[0].Op != "plan" {
		t.Fatalf("denied rows %+v", rows)
	}
}

func gzipped(b []byte) io.Reader {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(b)
	zw.Close()
	return &buf
}

// The token is checked before the body is read, so a request without one
// costs the server no read of its body.
func TestTokenBeforeBody(t *testing.T) {
	f := newFixture(t, nil)
	body := &countingReader{r: planRequest(nil)}
	req := httptest.NewRequest("POST", "/v1/plans", body)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 401 || body.n != 0 {
		t.Fatalf("status %d after reading %d bytes", rec.Code, body.n)
	}
}

type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func TestReadsAndRoutes(t *testing.T) {
	f := newFixture(t, nil)
	tok := f.token(nil)
	if r := f.do("GET", "/v1/server", "", nil); r.status != 200 || r.doc["format"] != "writ.server/v0" || r.doc["version"] != "test" {
		t.Fatalf("server %d %s", r.status, r.body)
	}
	if r := f.do("GET", "/healthz", "", nil); r.status != 200 || r.doc["database"] != true {
		t.Fatalf("healthz %d %s", r.status, r.body)
	}
	who := f.do("GET", "/v1/whoami", tok, nil)
	if who.status != 200 || fmt.Sprint(who.doc["teams"]) != "[infra_mr]" || len(who.doc["grants"].([]any)) != 1 {
		t.Fatalf("whoami %s", who.body)
	}
	if r := f.do("GET", "/v1/nope", "", nil); r.status != 404 || r.doc["format"] != "writ.error/v0" {
		t.Fatalf("unknown route: %d %s", r.status, r.body)
	}
	if r := f.do("DELETE", "/v1/plans", tok, nil); r.status != 405 || r.header.Get("Allow") != "GET, POST" {
		t.Fatalf("wrong method: %d %s", r.status, r.body)
	}
	if r := f.do("GET", "/v1/plans/not-an-id", tok, nil); r.status != 404 {
		t.Fatalf("bad id: %d %s", r.status, r.body)
	}
	if r := f.do("GET", "/v1/plans?env=prod&cursor=zzz", tok, nil); r.status != 400 {
		t.Fatalf("bad cursor: %d %s", r.status, r.body)
	}
}

// The server's clock stands still, so no bucket refills between requests
// however slowly they run.
func TestLimits(t *testing.T) {
	var log syncBuffer
	f := planFixture(t, func(s string) string {
		return strings.Replace(withForge(t, "")(s), "limits {\n  requests_per_second = 1000\n", "limits {\n  plans_per_principal = 1\n  requests_per_second = 5\n", 1)
	}, frozen(&log))
	f.addInfra()
	gate := make(chan struct{})
	f.tofu.gate = gate
	defer close(gate)
	if r := f.do("POST", "/v1/plans", f.mrToken(nil, nil), planRequest(nil)); r.status != 202 {
		t.Fatalf("first plan %d %s", r.status, r.body)
	}
	r := f.do("POST", "/v1/plans", f.mrToken(nil, nil), planRequest(nil))
	if r.status != 429 || !strings.Contains(string(r.body), "running 1 plans") {
		t.Fatalf("second plan while the first runs: %d %s", r.status, r.body)
	}
	// The repository's add and the two plans took 3 of the burst of 5, so
	// 2 more pass and the rest wait.
	tok := f.token(nil)
	var got []int
	for range 6 {
		got = append(got, f.do("GET", "/v1/whoami", tok, nil).status)
	}
	if want := []int{200, 200, 429, 429, 429, 429}; !slices.Equal(got, want) {
		t.Fatalf("six requests after an add and two plans, at a limit of five a second: %v, want %v", got, want)
	}
}

// A plan left planning when the process died is failed at the next start.
func TestRestartFailsLeftPlans(t *testing.T) {
	f := planFixture(t, withForge(t, ""))
	f.addInfra()
	gate := make(chan struct{})
	f.tofu.gate = gate
	r := f.do("POST", "/v1/plans", f.mrToken(nil, nil), planRequest(nil))
	ref := r.doc["plan"].(map[string]any)["ref"].(string)
	f.srv.Close()
	close(gate)
	p, err := f.store.Plan(context.Background(), ref, time.Now())
	if err != nil || p.Status == "planning" {
		t.Fatalf("after Close: %+v %v", p, err)
	}
}

// A newer pending plan of the same stack@env from the same merge request
// marks the older one superseded, and one from another merge request
// does not.
func TestSupersededBy(t *testing.T) {
	f := planFixture(t, withForge(t, ""))
	f.addInfra()
	plan := func(mr int) map[string]any {
		t.Helper()
		tok := f.mrToken(nil, nil)
		r := f.do("POST", "/v1/plans", tok, planRequest(func(m map[string]any) { m["hints"] = map[string]any{"merge_request": mr} }))
		ref := r.doc["plan"].(map[string]any)["ref"].(string)
		return f.do("GET", "/v1/plans/"+ref+"?wait=10s", tok, nil).doc["plan"].(map[string]any)
	}
	first, second, other := plan(42), plan(42), plan(43)
	got := f.do("GET", "/v1/plans/"+first["id"].(string), f.token(nil), nil).doc["plan"].(map[string]any)
	if got["superseded_by"] != second["id"] {
		t.Fatalf("the first plan's superseded_by is %v, want %v", got["superseded_by"], second["id"])
	}
	for _, p := range []map[string]any{second, other} {
		got := f.do("GET", "/v1/plans/"+p["id"].(string), f.token(nil), nil).doc["plan"].(map[string]any)
		if got["superseded_by"] != "" {
			t.Fatalf("plan %v is superseded by %v", p["id"], got["superseded_by"])
		}
	}
}

// Every answer names writ serve's version in X-Writ-Version, one that
// needs no token and a refusal too, so a CLI can say it is older than the
// server before an answer it cannot read.
func TestAnswersNameTheVersion(t *testing.T) {
	f := newFixture(t, nil)
	for _, r := range []response{
		f.do("GET", "/v1/server", "", nil),
		f.do("GET", "/v1/whoami", f.token(nil), nil),
		f.do("GET", "/v1/whoami", "", nil),
		f.do("GET", "/v1/no-such-route", f.token(nil), nil),
	} {
		if got := r.header.Get(server.VersionHeader); got != "test" {
			t.Errorf("%d %s: %s is %q, want test", r.status, r.body, server.VersionHeader, got)
		}
	}
}
