package ui_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/html"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/control/controltest"
	"github.com/mr-karan/writ/internal/control/pgtest"
	"github.com/mr-karan/writ/internal/forge/forgetest"
	"github.com/mr-karan/writ/internal/oidc/oidctest"
	"github.com/mr-karan/writ/internal/server"
	"github.com/mr-karan/writ/internal/tofu"
	"github.com/mr-karan/writ/internal/ui"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

// Secrets the fixture plans with. None may reach a page.
const (
	paramSecret = "hunter2-ui-param-secret"
	envSecret   = "s3cret-env-value-77"
	clientSec   = "oidc-client-secret-value"
)

// hostile is put into every string a page shows that the fixture
// controls: addresses, messages, values, and names.
const hostile = `<script>alert(1)</script>"'&javascript:alert(2)`

// clock is the server's clock, which tests move to end sessions.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// uiTofu stands in for OpenTofu: each plan creates a file whose name and
// content are hostile and replaces a key, with the sensitive variable set
// to paramSecret. doc, when set, is the plan's `tofu show -json` instead.
type uiTofu struct {
	mu    sync.Mutex
	plans int
	doc   func() string
}

const uiPlanJSON = `{"format_version":"1.2","terraform_version":"1.13.9",
 "variables":{"db_password":{"value":"` + paramSecret + `"}},
 "configuration":{"root_module":{"variables":{"db_password":{"sensitive":true}}}},
 "resource_changes":[
  {"address":"local_file.files[HOSTILEKEY]","mode":"managed","type":"local_file","name":"files","index":"HOSTILE","provider_name":"registry.opentofu.org/hashicorp/local",
   "change":{"actions":["create"],"before":null,"after":{"content":"HOSTILE","filename":"HOSTILE"},"after_unknown":{"id":true},"before_sensitive":false,"after_sensitive":{}}},
  {"address":"tls_private_key.ca_key","mode":"managed","type":"tls_private_key","name":"ca_key","provider_name":"registry.opentofu.org/hashicorp/tls",
   "change":{"actions":["delete","create"],"before":{"ecdsa_curve":"P256","private_key_pem":"` + paramSecret + `"},"after":{"ecdsa_curve":"P384"},"after_unknown":{"private_key_pem":true},"before_sensitive":{"private_key_pem":true},"after_sensitive":{"private_key_pem":true},"replace_paths":[["ecdsa_curve"]]}}
 ]}`

// uiDriftJSON is uiPlanJSON with the key's secret changed outside
// OpenTofu as well, as a drift check of network finds it.
var uiDriftJSON = strings.Replace(uiPlanJSON, `"resource_changes":[`, `"resource_drift":[
  {"address":"tls_private_key.ca_key","mode":"managed","type":"tls_private_key","name":"ca_key","provider_name":"registry.opentofu.org/hashicorp/tls",
   "change":{"actions":["update"],"before":{"ecdsa_curve":"P256","private_key_pem":"`+paramSecret+`-old"},"after":{"ecdsa_curve":"P256","private_key_pem":"`+paramSecret+`"},"after_unknown":{},"before_sensitive":{"private_key_pem":true},"after_sensitive":{"private_key_pem":true}}}
 ],
 "resource_changes":[`, 1)

func (r *uiTofu) Fill(_ context.Context, lock []byte) ([]byte, error) { return lock, nil }

func (r *uiTofu) Plan(context.Context, tofu.Stack) (tofu.Plan, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.plans++
	doc := uiPlanJSON
	if r.doc != nil {
		doc = r.doc()
	}
	quoted, _ := json.Marshal(hostile)
	key, _ := json.Marshal(strconv.Quote(hostile))
	doc = strings.ReplaceAll(doc, `"HOSTILE"`, string(quoted))
	doc = strings.ReplaceAll(doc, `[HOSTILEKEY]`, "["+string(key[1:len(key)-1])+"]")
	return tofu.Plan{File: fmt.Appendf(nil, "saved %d", r.plans), JSON: []byte(doc)}, nil
}

func (r *uiTofu) Apply(context.Context, tofu.Stack, []byte) (tofu.Applied, error) {
	return tofu.Applied{}, errors.New("the UI tests apply nothing")
}

func (r *uiTofu) Show(context.Context, tofu.Stack, []byte) (tofu.State, error) {
	return tofu.State{}, errors.New("the UI tests keep no state")
}

func (r *uiTofu) Pull(context.Context, tofu.Stack) (tofu.State, error) {
	return tofu.State{}, errors.New("the UI tests keep no state")
}

type fixture struct {
	t      *testing.T
	is     *oidctest.Issuer
	https  *httptest.Server
	url    string
	srv    *server.Server
	store  *control.Store
	loc    string
	clock  *clock
	opts   server.Options
	tofu   *uiTofu
	gitlab *forgetest.GitLab
	roots  *x509.CertPool
	// traffic is what every browser of the fixture exchanged with the UI.
	traffic *traffic
}

// newFixture runs writ serve with the control UI over HTTPS, a test issuer
// that is both GitLab CI and the identity provider, and real Postgres.
// Each of issuer runs on the test issuer before writ serve reads its
// discovery document.
func newFixture(t *testing.T, issuer ...func(*oidctest.Issuer)) *fixture {
	t.Helper()
	return newFixtureOn(t, controltest.Default(), "", issuer...)
}

// newFixtureOn is newFixture on the control database of e, with extra
// added to the end of writ serve's config.
func newFixtureOn(t *testing.T, e controltest.Engine, extra string, issuer ...func(*oidctest.Issuer)) *fixture {
	t.Helper()
	is := oidctest.New(t)
	is.SetClient("writ-client", clientSec)
	is.SetPerson(oidctest.GitLabPerson())
	for _, fn := range issuer {
		fn(is)
	}
	gl := forgetest.Handler(forgeToken)
	gl.URL = is.URL
	is.Mount("/api/v4/", gl)
	gl.AddProject(4242, "acme/infra", "main")
	gl.Commit(4242, "main", stackFiles(""))
	var handler http.Handler = http.NotFoundHandler()
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	ts.StartTLS()
	t.Cleanup(ts.Close)
	dir := t.TempDir()
	write := func(name, text string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ca := write("issuer.pem", string(is.CAPEM()))
	secret := write("client_secret", clientSec+"\n")
	envFile := write("api_token", envSecret+"\n")
	readToken := write("gitlab_read", forgeToken+"\n")
	store, loc, db := controltest.ForConfigOn(t, e)
	text := fmt.Sprintf(`url      = %q
listen   = "127.0.0.1:1"
database = %q
data     = %q

limits {
  requests_per_second  = 1000
  requests_per_address = 1000
}

issuer "gitlab" {
  kind     = "gitlab-ci"
  identity = "gitlab"
  url      = %q
  ca_file  = %q
}

issuer "sso" {
  kind               = "people"
  identity           = "gitlab"
  url                = %q
  ca_file            = %q
  client_id          = "writ-client"
  client_secret_file = %q
  groups             = "userinfo"
}

team "infra_mr" {
  issuer = "gitlab"
  match  = { project_id = ["4242"], pipeline_source = ["merge_request_event"] }
}

team "platform" {
  issuer = "sso"
  match  = { groups = ["acme/platform"] }
}

grant {
  teams  = ["infra_mr"]
  stacks = ["*"]
  envs   = ["prod"]
  can    = ["read", "plan"]
}

grant {
  teams  = ["platform"]
  stacks = ["*"]
  envs   = ["*"]
  can    = ["read", "approve"]
}

grant {
  teams  = ["platform"]
  stacks = ["network"]
  envs   = ["staging"]
  can    = ["apply"]
  risks  = ["create", "update"]
}

grant {
  teams  = ["infra_mr"]
  stacks = ["network"]
  envs   = ["prod"]
  can    = ["apply"]
  risks  = ["create", "update", "replace", "delete", "data-loss"]
}

env "prod" {
  key          = "prod.key"
  provider_env = { API_TOKEN = %q }
}

env "staging" {
  key = "staging.key"
}

approval {
  stacks = ["*"]
  envs   = ["prod"]
  risks  = ["create", "update", "replace", "delete", "data-loss"]
  need   = 1
}

provider_uids = "61000-61063"
poll_every    = "1h"

forge "gitlab" {
  kind            = "gitlab"
  url             = %q
  read_token_file = %q
  ca_file         = %q
}

repo {
  name    = "infra"
  forge   = "gitlab"
  project = "acme/infra"
}
`, ts.URL, db, dir, is.URL, ca, is.URL, ca, secret, envFile, is.URL, readToken, ca) + extra
	cfg, diags := server.ParseConfig(filepath.Join(dir, "serve"), []byte(text))
	if diags.HasErrors() {
		t.Fatal(diags)
	}
	ctx := context.Background()
	v, err := server.NewVerifier(ctx, cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	people, err := server.People(ctx, cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{now: time.Now()}
	r := &uiTofu{}
	opts := server.Options{Config: cfg, Store: store, Verifier: v, People: people, Tofu: r, Reap: func(uint32) error { return nil }, Version: "test", Now: c.Now}
	srv, err := server.New(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	u, err := ui.New(ui.Options{Server: srv, Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	tr := watch(t, u)
	u.Mount()
	handler = srv.Handler()
	roots := x509.NewCertPool()
	roots.AddCert(ts.Certificate())
	roots.AppendCertsFromPEM(is.CAPEM())
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(20 * time.Millisecond) {
		if rp, err := store.Repo(ctx, "infra"); err == nil && rp.Head != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the pinned repository was not read")
		}
	}
	return &fixture{t: t, is: is, https: ts, url: ts.URL, srv: srv, store: store, loc: loc, clock: c, opts: opts, tofu: r, gitlab: gl, roots: roots, traffic: tr}
}

// forgeToken is the forge's read token.
const forgeToken = "glpat-ui-read-token"

// stackFiles is acme/infra with the OpenTofu stack network, whose code
// holds note.
func stackFiles(note string) map[string]forgetest.File {
	return map[string]forgetest.File{
		"network/main.tf":             {Content: "terraform {\n  backend \"http\" {}\n}\n\nvariable \"db_password\" {\n  sensitive = true\n}\n# " + note + "\n"},
		"network/.terraform.lock.hcl": {Content: "provider \"registry.opentofu.org/hashicorp/local\" {\n  version = \"2.9.1\"\n  hashes  = [\"h1:abc=\"]\n}\n"},
	}
}

// browser is a client that keeps cookies and runs no script, as a browser
// with JavaScript off does. It sends the headers a browser sends on a
// same-origin navigation.
type browser struct {
	f      *fixture
	c      *http.Client
	follow bool
}

func (f *fixture) browser() *browser {
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, Transport: f.traffic.transport(f.url, &http.Transport{TLSClientConfig: &tls.Config{RootCAs: f.roots}})}
	return &browser{f: f, c: c, follow: true}
}

type page struct {
	status int
	header http.Header
	body   string
	url    *url.URL
	doc    *html.Node
	// served are the documents the page's handler read, for the render
	// check.
	served [][]byte
}

func (b *browser) do(req *http.Request) page {
	b.f.t.Helper()
	if b.follow {
		b.c.CheckRedirect = nil
	} else {
		b.c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	served := b.f.traffic.page(req)
	resp, err := b.c.Do(req)
	if err != nil {
		b.f.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	p := page{status: resp.StatusCode, header: resp.Header, body: string(body), url: resp.Request.URL, served: served()}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		if p.doc, err = html.Parse(bytes.NewReader(body)); err != nil {
			b.f.t.Fatal(err)
		}
	}
	return p
}

// read answers path without following a redirect, nil when the request
// fails, which leaves the render check nothing to allow. A path of the
// API is answered as the browser's session reads it.
func (b *browser) read(path string) []byte {
	if strings.HasPrefix(path, "/v1/") {
		return apiAs(b.f.t, b.f.srv, b.c, b.f.url)(path)
	}
	req, _ := http.NewRequest("GET", b.f.url+path, nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err := b.c.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return body
}

// apiAs answers a path of the API as the session c holds for the control
// UI at base reads it, nil when c holds none.
func apiAs(t *testing.T, srv *server.Server, c *http.Client, base string) func(path string) []byte {
	return func(path string) []byte {
		t.Helper()
		u, _ := url.Parse(base + "/ui/")
		for _, k := range c.Jar.Cookies(u) {
			if !strings.HasSuffix(k.Name, "writ_session") {
				continue
			}
			sess, err := srv.Session(context.Background(), k.Value)
			if err != nil {
				return nil
			}
			_, body := srv.Call(httptest.NewRequest("GET", "/", nil), sess, path)
			return body
		}
		return nil
	}
}

func (b *browser) get(path string) page {
	b.f.t.Helper()
	req, _ := http.NewRequest("GET", b.f.url+path, nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	return b.do(req)
}

func (b *browser) post(path string, form url.Values, site string) page {
	b.f.t.Helper()
	req, _ := http.NewRequest("POST", b.f.url+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if site != "" {
		req.Header.Set("Sec-Fetch-Site", site)
	}
	return b.do(req)
}

// signIn runs the whole sign-in through the test issuer and returns the
// page it lands on.
func (b *browser) signIn(next string) page {
	b.f.t.Helper()
	b.follow = true
	p := b.get("/ui/login/start?issuer=sso&next=" + url.QueryEscape(next))
	if p.status != 200 || p.url.Path == "/ui/login" || p.url.Path == "/ui/callback" {
		b.f.t.Fatalf("sign-in landed on %s with %d: %s", p.url, p.status, p.body)
	}
	return p
}

// token is a CI job's token for writ serve.
func (f *fixture) token(edits map[string]any) string {
	c := oidctest.GitLabJob(f.is.URL, f.url, time.Now())
	for k, v := range edits {
		c[k] = v
	}
	return oidctest.Sign(f.is.Keys()[0], c)
}

// plan commits network to a merge request's branch, plans it from a CI
// job of the merge request, and returns the plan's id.
func (f *fixture) plan(edits map[string]any) string {
	f.t.Helper()
	p := f.planEnded(edits)
	if p.ID == "" {
		f.t.Fatalf("plan %+v did not end pending", p)
	}
	return p.ID
}

// planEnded plans as plan does and returns the plan once it ended,
// failed or not.
func (f *fixture) planEnded(edits map[string]any) *server.PlanInfo {
	f.t.Helper()
	sha := f.gitlab.Commit(4242, "add-subnet-d", stackFiles(fmt.Sprint(time.Now().UnixNano())))
	body, _ := json.Marshal(map[string]any{
		"format": "writ.plan-request/v1", "env": "prod", "stacks": []string{"network"},
		"hints": map[string]any{"merge_request": 42}, "source": map[string]any{},
	})
	if edits == nil {
		edits = map[string]any{}
	}
	if _, ok := edits["sha"]; !ok {
		edits["sha"] = sha
	}
	tok := f.token(edits)
	req, _ := http.NewRequest("POST", f.url+"/v1/plans", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: f.roots}}}
	resp, err := c.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	var doc server.RemotePlan
	_ = json.NewDecoder(resp.Body).Decode(&doc)
	resp.Body.Close()
	if resp.StatusCode != 202 {
		f.t.Fatalf("plan: %d", resp.StatusCode)
	}
	ref := doc.Plan.Ref
	deadline := time.Now().Add(time.Minute)
	for {
		req, _ = http.NewRequest("GET", f.url+"/v1/plans/"+ref+"?wait=10s", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err = c.Do(req)
		if err != nil {
			f.t.Fatal(err)
		}
		err = json.NewDecoder(resp.Body).Decode(&doc)
		resp.Body.Close()
		if err != nil || doc.Plan == nil {
			f.t.Fatalf("plan %s: %v", ref, err)
		}
		if doc.Plan.Status != "planning" {
			break
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("plan %s still planning", ref)
		}
	}
	return doc.Plan
}

// driftCheck indexes network in prod and in staging, and gives network in
// staging a state version with no secret set. It then runs one drift
// check of each as a second control plane on the same database does at
// its start, with the key's secret changed outside OpenTofu, and returns
// once both ended.
func (f *fixture) driftCheck() {
	f.t.Helper()
	ctx := context.Background()
	for _, env := range []string{"prod", "staging"} {
		if _, err := f.store.ReplaceIndex(ctx, control.IndexWrite{Env: env, Stack: "network", Backend: control.BackendHTTP, Version: 1, At: time.Now(),
			Rows: []control.IndexRow{{Address: "tls_private_key.ca_key", Type: "tls_private_key", Provider: "registry.opentofu.org/hashicorp/tls", ID: "ca-key"}}}); err != nil {
			f.t.Fatal(err)
		}
	}
	f.noSecretSet("staging")
	f.tofu.mu.Lock()
	f.tofu.doc = func() string { return uiDriftJSON }
	f.tofu.mu.Unlock()
	defer func() {
		f.tofu.mu.Lock()
		f.tofu.doc = nil
		f.tofu.mu.Unlock()
	}()
	// A drift check that starts no later than a run of its stack ended
	// keeps nothing, and the clock stands still.
	f.clock.Add(time.Second)
	cfg := *f.opts.Config
	cfg.DriftEvery = time.Hour
	o := f.opts
	o.Config = &cfg
	srv, err := server.New(ctx, o)
	if err != nil {
		f.t.Fatal(err)
	}
	defer srv.Close()
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(10 * time.Millisecond) {
		st, _, err := f.store.DriftStacks(ctx, "", "network", 10, func(string, string) bool { return true })
		if err != nil {
			f.t.Fatal(err)
		}
		if len(st) == 2 && !slices.ContainsFunc(st, func(s control.DriftStack) bool { return s.Last == nil || s.Checking }) {
			return
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("the drift checks of network did not end: %+v", st)
		}
	}
}

// noSecretSet writes version 1 of network's state in env with no secret
// set, as writ serve wrote states before it kept them.
func (f *fixture) noSecretSet(env string) {
	f.t.Helper()
	if _, err := controltest.Raw(f.t, f.loc).ExecContext(context.Background(), `INSERT INTO tfstate_versions (env, stack, version, serial, lineage, sha256, run_id, created_at, sealed)
		VALUES (?, 'network', 1, 4, 'lineage-1', 'sha', 'old-run', ?, ?)`, env, time.Now().UnixMilli(), []byte("sealed")); err != nil {
		f.t.Fatal(err)
	}
}

// apply applies plan id from a CI job of its merge request and returns
// the run's id once the run ended. The fixture's OpenTofu applies nothing,
// so the run fails.
func (f *fixture) apply(id string) string {
	f.t.Helper()
	body, _ := json.Marshal(map[string]any{"format": "writ.run-request/v1", "plan": id})
	tok := f.token(nil)
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: f.roots}}}
	call := func(method, path string, body []byte) (int, server.RemoteRun) {
		req, _ := http.NewRequest(method, f.url+path, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := c.Do(req)
		if err != nil {
			f.t.Fatal(err)
		}
		defer resp.Body.Close()
		var doc server.RemoteRun
		if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
			f.t.Fatalf("%s %s: %d %v", method, path, resp.StatusCode, err)
		}
		return resp.StatusCode, doc
	}
	status, doc := call("POST", "/v1/runs", body)
	if status != http.StatusAccepted || doc.Run == nil {
		f.t.Fatalf("apply %s: %d %+v", id, status, doc)
	}
	for deadline := time.Now().Add(time.Minute); doc.Run.Status == "applying"; {
		if time.Now().After(deadline) {
			f.t.Fatalf("run %s still applying", doc.Run.ID)
		}
		if status, doc = call("GET", "/v1/runs/"+doc.Run.ID+"?wait=10s", nil); status != http.StatusOK || doc.Run == nil {
			f.t.Fatalf("run: %d %+v", status, doc)
		}
	}
	return doc.Run.ID
}

// text is the visible text of a node and its children.
func text(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

// each calls fn for every element of the page.
func each(n *html.Node, fn func(*html.Node)) {
	if n.Type == html.ElementNode {
		fn(n)
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		each(c, fn)
	}
}

func attr(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}
