package ui_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/html"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/control/controltest"
	"github.com/mr-karan/writ/internal/server"
	"github.com/mr-karan/writ/internal/tofu"
	"github.com/mr-karan/writ/internal/ui"
)

// The canaries are the values testdata/tofu/stack/*.tfvars give the
// stack's sensitive variable.
const (
	canaryFirst  = "ui-canary-first-3f9a1c"
	canarySecond = "ui-canary-second-7b2e4d"
)

// tofuRecording is what tofu wrote for one plan of testdata/tofu/stack,
// and for its apply.
type tofuRecording struct {
	plan, apply string
	// failed is set when the recorded apply failed.
	failed bool
}

var (
	// second forgets, imports, replaces with replace_paths, creates in a
	// module, changes outputs, and finds drift.
	second = tofuRecording{plan: "second-plan.json", apply: "second-apply.jsonl"}
	// third creates a file that cannot be written, so its apply fails.
	third = tofuRecording{plan: "third-plan.json", apply: "third-apply.jsonl", failed: true}
)

// recordedTofu stands in for OpenTofu at writ serve's boundary: it answers
// each plan and apply with the output tofu 1.13.9 wrote for the recording
// it is set to. writ serve masks that output as it masks tofu's.
type recordedTofu struct {
	t   *testing.T
	mu  sync.Mutex
	rec tofuRecording
	// gate, when set, holds each plan until it is closed.
	gate chan struct{}
}

func (r *recordedTofu) set(rec tofuRecording) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rec = rec
}

func (r *recordedTofu) read(name string) []byte {
	b, err := os.ReadFile(filepath.Join("testdata", "tofu", name))
	if err != nil {
		r.t.Error(err)
	}
	return b
}

func (r *recordedTofu) Fill(context.Context, []byte) error { return nil }

func (r *recordedTofu) Plan(ctx context.Context, _ tofu.Stack) (tofu.Plan, error) {
	r.mu.Lock()
	gate := r.gate
	r.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return tofu.Plan{}, ctx.Err()
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return tofu.Plan{File: []byte("saved " + r.rec.plan), JSON: r.read(r.rec.plan)}, nil
}

func (r *recordedTofu) Apply(context.Context, tofu.Stack, []byte) (tofu.Applied, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	log := r.read(r.rec.apply)
	out := tofu.Applied{Plan: r.read(r.rec.plan), Log: log}
	if r.rec.failed {
		return out, &tofu.StepError{Step: "apply", Code: 1, Stdout: log}
	}
	out.ShowErr = errNoState
	return out, nil
}

// errNoState is why a recording shows no state: the pages read none, and
// the recordings hold only plans and applies.
var errNoState = errors.New("the recording holds no state")

func (r *recordedTofu) Show(context.Context, tofu.Stack, []byte) (tofu.State, error) {
	return tofu.State{}, errNoState
}

func (r *recordedTofu) Pull(context.Context, tofu.Stack) (tofu.State, error) {
	return tofu.State{}, errNoState
}

// tofuStack is a Git repository with the OpenTofu stack app on the http
// backend.
func tofuStack(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"app/main.tf": "terraform {\n  backend \"http\" {}\n}\n\nresource \"random_pet\" \"kept\" {}\n",
		"app/.terraform.lock.hcl": "provider \"registry.opentofu.org/hashicorp/random\" {\n  version = \"3.9.1\"\n  hashes  = [\"h1:abc=\"]\n}\n\n" +
			"provider \"registry.opentofu.org/hashicorp/local\" {\n  version = \"2.9.1\"\n  hashes  = [\"h1:def=\"]\n}\n",
	}
	for name, text := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "."}, {"commit", "-q", "-m", "first"}} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Customer A", "GIT_AUTHOR_EMAIL=user@example.com", "GIT_COMMITTER_NAME=Customer A",
			"GIT_COMMITTER_EMAIL=user@example.com", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	top, _ := filepath.EvalSymlinks(dir)
	return top
}

// tofuUI is a dev control plane with its control UI, the repository
// demo that holds the OpenTofu stack app, and a browser signed in as the
// dev user.
type tofuUI struct {
	t      *testing.T
	srv    *server.Server
	store  *control.Store
	cfg    *server.Config
	base   string
	tofu   *recordedTofu
	commit string
	c      *http.Client
	// traffic is what c exchanged with the UI.
	traffic *traffic
}

func newTofuUI(t *testing.T, edits ...func(*server.Config)) *tofuUI {
	t.Helper()
	ctx := context.Background()
	data := t.TempDir()
	store, err := control.Open(ctx, control.Options{Location: filepath.Join(data, "control.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	ts := httptest.NewUnstartedServer(nil)
	url := "http://" + ts.Listener.Addr().String()
	cfg := server.DevConfig(server.DevOptions{URL: url, Listen: ts.Listener.Addr().String(), Database: filepath.Join(data, "control.db"), Data: data})
	for _, e := range edits {
		e(cfg)
	}
	rec := &recordedTofu{t: t}
	srv, err := server.New(ctx, server.Options{Config: cfg, Store: store, Tofu: rec, DevToken: strings.Repeat("d", 40), DevUser: "tester", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	u, err := ui.New(ui.Options{Server: srv})
	if err != nil {
		t.Fatal(err)
	}
	tr := watch(t, u)
	u.Mount()
	ts.Config.Handler = srv.Handler()
	ts.Start()
	t.Cleanup(ts.Close)
	dir := tofuStack(t)
	x := &tofuUI{t: t, srv: srv, store: store, cfg: cfg, base: url, tofu: rec, commit: gitHead(t, dir), traffic: tr}
	if status, body := x.post("/v1/repos", map[string]any{"format": "writ.repo-request/v0", "name": "demo", "forge": "dir", "project": dir}); status != http.StatusCreated {
		t.Fatalf("add the repository: %d %s", status, body)
	}
	jar, _ := cookiejar.New(nil)
	x.c = &http.Client{Jar: jar, Transport: tr.transport(url, http.DefaultTransport)}
	if status, at, body := visit(t, x.c, "GET", srv.DevLink().URL, nil); status != http.StatusOK {
		t.Fatalf("sign in: %d %s %s", status, at, body)
	}
	return x
}

func (x *tofuUI) post(path string, body map[string]any) (int, []byte) {
	x.t.Helper()
	b, _ := json.Marshal(body)
	return x.srv.Post(httptest.NewRequest("POST", "/", nil), devSession(x.t, x.srv), path, b)
}

// wait gets path with ?wait= until its document's status at key is no
// longer pending.
func (x *tofuUI) wait(path, key, pending string, into any) {
	x.t.Helper()
	for deadline := time.Now().Add(time.Minute); ; {
		status, body := x.srv.Call(httptest.NewRequest("GET", "/", nil), devSession(x.t, x.srv), path+"?wait=30s")
		var head map[string]json.RawMessage
		var obj struct{ Status string }
		if status != http.StatusOK || json.Unmarshal(body, &head) != nil || json.Unmarshal(head[key], &obj) != nil || json.Unmarshal(body, into) != nil {
			x.t.Fatalf("GET %s: %d %.500s", path, status, body)
		}
		if obj.Status != pending {
			return
		}
		if time.Now().After(deadline) {
			x.t.Fatalf("GET %s: still %s", path, pending)
		}
	}
}

// plan plans app at the commit with rec as tofu's output, and returns
// the plan once it ends approved.
func (x *tofuUI) plan(rec tofuRecording) server.RemotePlan {
	x.t.Helper()
	p := x.planEnded(rec, server.DevEnv)
	if p.Plan.Status != "approved" || p.Plan.ID == "" {
		x.t.Fatalf("plan %+v: %s", p.Plan, p.Document)
	}
	return p
}

// planEnded plans app in env at the commit with rec as tofu's output,
// and returns the plan once it ends, failed or not.
func (x *tofuUI) planEnded(rec tofuRecording, env string) server.RemotePlan {
	x.t.Helper()
	var p server.RemotePlan
	x.wait("/v1/plans/"+x.planStarted(rec, env), "plan", "planning", &p)
	return p
}

// planStarted asks for a plan of app in env as planEnded does, and
// returns its ref without waiting for it.
func (x *tofuUI) planStarted(rec tofuRecording, env string) string {
	x.t.Helper()
	x.tofu.set(rec)
	status, body := x.post("/v1/plans", map[string]any{"format": "writ.plan-request/v1", "env": env, "stacks": []string{"app"}, "source": map[string]any{"repo": "demo", "commit": x.commit}})
	if status != http.StatusAccepted {
		x.t.Fatalf("plan: %d %s", status, body)
	}
	var created server.RemotePlan
	if err := json.Unmarshal(body, &created); err != nil {
		x.t.Fatal(err)
	}
	return created.Plan.Ref
}

// noSecretSet writes version 1 of app's state in env with no secret set,
// as writ serve wrote states before it kept them.
func (x *tofuUI) noSecretSet(env string) {
	x.t.Helper()
	if _, err := controltest.Raw(x.t, x.cfg.Database).ExecContext(context.Background(), `INSERT INTO tfstate_versions (env, stack, version, serial, lineage, sha256, run_id, created_at, sealed)
		VALUES (?, 'app', 1, 4, 'lineage-1', 'sha', 'old-run', ?, ?)`, env, time.Now().UnixMilli(), []byte("sealed")); err != nil {
		x.t.Fatal(err)
	}
}

// apply applies plan id and returns the run once it ends.
func (x *tofuUI) apply(id string) server.RemoteRun {
	x.t.Helper()
	status, body := x.post("/v1/runs", map[string]any{"format": "writ.run-request/v1", "plan": id})
	if status != http.StatusAccepted {
		x.t.Fatalf("apply: %d %s", status, body)
	}
	var started server.RemoteRun
	if err := json.Unmarshal(body, &started); err != nil {
		x.t.Fatal(err)
	}
	var run server.RemoteRun
	x.wait("/v1/runs/"+started.Run.ID, "run", "applying", &run)
	return run
}

// driftCheck indexes app as one managed pet, and runs one drift check of
// it with rec as tofu's output: a second control plane on the same
// database, whose drift checks look at its start, plans app in each env
// at the head of demo's default branch. It returns once those checks
// ended.
func (x *tofuUI) driftCheck(rec tofuRecording) {
	x.t.Helper()
	ctx := context.Background()
	if _, err := x.store.ReplaceIndex(ctx, control.IndexWrite{Env: server.DevEnv, Stack: "app", Backend: control.BackendHTTP, Version: 1, At: time.Now(),
		Rows: []control.IndexRow{{Address: "random_pet.kept", Type: "random_pet", Provider: "registry.opentofu.org/hashicorp/random", ID: "kept-pet"}}}); err != nil {
		x.t.Fatal(err)
	}
	x.tofu.set(rec)
	cfg := *x.cfg
	cfg.DriftEvery = time.Hour
	srv, err := server.New(ctx, server.Options{Config: &cfg, Store: x.store, Tofu: x.tofu, DevToken: strings.Repeat("e", 40), DevUser: "checker", Version: "test"})
	if err != nil {
		x.t.Fatal(err)
	}
	defer srv.Close()
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(10 * time.Millisecond) {
		st, _, err := x.store.DriftStacks(ctx, "", "app", 10, func(string, string) bool { return true })
		if err != nil {
			x.t.Fatal(err)
		}
		if len(st) > 0 && !slices.ContainsFunc(st, func(s control.DriftStack) bool { return s.Last == nil || s.Checking }) {
			return
		}
		if time.Now().After(deadline) {
			x.t.Fatalf("no drift check of app ended: %+v", st)
		}
	}
}

// page gets a page of the control UI, runs checkWords on it, and returns
// its body and its parsed document.
func (x *tofuUI) page(path string) (string, *html.Node) {
	x.t.Helper()
	req, _ := http.NewRequest("GET", x.base+path, nil)
	served := x.traffic.page(req)
	resp, err := x.c.Do(req)
	if err != nil {
		x.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	body := string(b)
	if resp.StatusCode != http.StatusOK || resp.Request.URL.Path != strings.SplitN(path, "?", 2)[0] {
		x.t.Fatalf("GET %s: %d at %s\n%s", path, resp.StatusCode, resp.Request.URL.Path, body)
	}
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		x.t.Fatal(err)
	}
	checkWords(x.t, path, doc, served(), nil, nil)
	return body, doc
}

// byID returns the element with id, or fails.
func byID(t *testing.T, doc *html.Node, id string) *html.Node {
	t.Helper()
	var found *html.Node
	each(doc, func(n *html.Node) {
		if v, ok := attr(n, "id"); ok && v == id {
			found = n
		}
	})
	if found == nil {
		t.Fatalf("the page has no element %q", id)
	}
	return found
}

// words is the visible text of n with its white space folded, and a
// space after each cell, item, and paragraph, as a reader hears them.
func words(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if n.Type == html.ElementNode {
			switch n.Data {
			case "td", "th", "dt", "dd", "li", "p", "caption", "h1", "h2", "h3", "h4", "summary":
				b.WriteByte(' ')
			}
		}
	}
	walk(n)
	return strings.Join(strings.Fields(b.String()), " ")
}

// An OpenTofu plan's page shows, from the masked document, each resource
// change by module and address with its actions, reason, replace_paths,
// import marker, and before and after values, the drift apart, and the
// outputs that change.
func TestTofuPlanReview(t *testing.T) {
	x := newTofuUI(t)
	p := x.plan(second)
	_, doc := x.page("/ui/plans/" + p.Plan.ID)
	body := words(doc)
	for _, want := range []string{
		"+ 2 to add ~ 0 to change - 0 to destroy -/+ 1 to replace 1 to forget 1 to import",
		"Changes (5)",
		"Changed outside OpenTofu (1)",
		"Outputs that change (2)",
		"May apply dev",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q:\n%s", want, body)
		}
	}

	// The changes run by module, the root module first, then by address.
	root, inner := words(byID(t, doc, "h-module-0")), words(byID(t, doc, "h-module-1"))
	if root != "Root module (4)" || inner != "Module module.inner (1)" {
		t.Errorf("the module headings: %q, %q", root, inner)
	}
	var order []string
	each(doc, func(n *html.Node) {
		if v, ok := attr(n, "class"); ok && v == "change" {
			id, _ := attr(n, "id")
			order = append(order, id+" "+strings.Fields(words(n))[1])
		}
	})
	if got := strings.Join(order, ", "); got != "change-1 local_file.note, change-2 random_id.imported, change-3 random_pet.gone, change-4 random_pet.rotated, change-5 module.inner.random_pet.p, drift-1 local_file.note" {
		t.Errorf("the changes in order: %s", got)
	}

	for _, tc := range []struct {
		name, id string
		want     []string
	}{
		{"a replace with replace_paths", "change-4", []string{
			"-/+ random_pet.rotated replace replace",
			"Actions: delete, create. Reason: replace_because_cannot_update, the provider cannot update it in place.",
			"Replaced because keepers cannot change in place.",
			`keepers {"secret":"(sensitive)"} {"secret":"(sensitive)"} forces replace sensitive value changes`,
			`id "`, "(known after apply)",
		}},
		{"a forget", "change-3", []string{
			"~ random_pet.gone forget delete",
			"Actions: forget. Reason: delete_because_no_resource_config, the code no longer declares it.",
			"OpenTofu removes this object from the state and leaves the object itself in place.",
			"What OpenTofu stops managing",
		}},
		{"an import", "change-2", []string{
			"<- random_id.imported import import create",
			"Imports an object that exists, id p-9hUg, into the state.",
			"6 attributes stay the same",
			`b64_url "p-9hUg"`, "byte_length 4",
		}},
		{"a create in a module", "change-5", []string{"+ module.inner.random_pet.p create create", `length 3`, "id (known after apply)"}},
		{"drift", "drift-1", []string{
			"- local_file.note delete",
			"Actions: delete.",
			"What changed",
			"content (sensitive)", `filename "./note.txt"`,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := words(byID(t, doc, tc.id))
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("%s lacks %q:\n%s", tc.id, want, got)
				}
			}
		})
	}
	// A value the mask hid is marked as masked, apart from the values
	// around it.
	masked := 0
	each(byID(t, doc, "change-1"), func(n *html.Node) {
		if v, _ := attr(n, "class"); v == "val masked" {
			masked++
			if text(n) != "(sensitive)" {
				t.Errorf("a value marked masked reads %q", text(n))
			}
		}
	})
	if masked != 1 {
		t.Errorf("local_file.note marks %d values as masked, want its content", masked)
	}
	outputs := words(byID(t, doc, "h-outputs").Parent)
	for _, want := range []string{`rotated ~ update "`, "(known after apply)", "secret ~ update (sensitive) (sensitive) sensitive value changes"} {
		if !strings.Contains(outputs, want) {
			t.Errorf("the outputs lack %q:\n%s", want, outputs)
		}
	}
}

// The dev control plane's access page lists its one team, the team its
// grant names.
func TestDevAccess(t *testing.T) {
	x := newTofuUI(t)
	_, doc := x.page("/ui/access")
	if got := words(byID(t, doc, "h-teams").Parent); !strings.Contains(got, "Teams (1)") || !strings.Contains(got, "dev dev no claim: only the one user of writ serve --dev is in it") {
		t.Errorf("the teams of the dev control plane:\n%s", got)
	}
}

// The filters narrow an OpenTofu plan's changes by action and by risk.
func TestTofuPlanFilters(t *testing.T) {
	x := newTofuUI(t)
	p := x.plan(second)
	for query, want := range map[string]string{
		"?action=forget": "random_pet.gone",
		"?risk=create":   "local_file.note random_id.imported module.inner.random_pet.p",
		"?risk=replace":  "random_pet.rotated",
	} {
		_, doc := x.page("/ui/plans/" + p.Plan.ID + query)
		var addrs []string
		each(doc, func(n *html.Node) {
			if v, ok := attr(n, "class"); ok && v == "change" {
				if id, _ := attr(n, "id"); strings.HasPrefix(id, "change-") {
					addrs = append(addrs, strings.Fields(words(n))[1])
				}
			}
		})
		if got := strings.Join(addrs, " "); got != want {
			t.Errorf("%s shows %s, want %s", query, got, want)
		}
	}
}

// A run that applied an OpenTofu plan shows its status, its plan, each
// resource's events, and its timing. A failed one shows the masked
// diagnostics. The run lists show both runs with their stack@env.
func TestTofuRunPages(t *testing.T) {
	x := newTofuUI(t)
	p := x.plan(second)
	ok := x.apply(p.Plan.ID)
	if ok.Run.Status != "applied" {
		t.Fatalf("run %+v: %s", ok.Run, ok.Text)
	}
	_, doc := x.page("/ui/runs/" + ok.Run.ID)
	body := words(doc)
	for _, want := range []string{
		"Run " + ok.Run.ID[:8] + " applied",
		"Stack app@dev, an OpenTofu stack",
		"Plan " + p.Plan.ID,
		"Applied by tester",
		"+ 3 added ~ 0 changed - 1 destroyed 1 imported",
		"Resources (4)",
		"random_pet.rotated delete complete 0",
		"local_file.note create complete 0",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the applied run's page lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "The apply failed") || strings.Contains(body, "Diagnostics") {
		t.Errorf("the applied run's page shows a failure:\n%s", body)
	}

	p3 := x.plan(third)
	failed := x.apply(p3.Plan.ID)
	if failed.Run.Status != "failed" {
		t.Fatalf("run %+v: %s", failed.Run, failed.Text)
	}
	_, doc = x.page("/ui/runs/" + failed.Run.ID)
	body = words(doc)
	for _, want := range []string{
		"Run " + failed.Run.ID[:8] + " failed",
		"The apply failed. The diagnostics below say why.",
		"Diagnostics (2)",
		"error usage: tofu apply exited with code 1",
		"error usage: main.tf:23:32: Create local file error",
		"Resources (1)",
		"local_file.broken create errored 0",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the failed run's page lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "is a directory") {
		t.Errorf("the failed run's page shows a diagnostic's detail:\n%s", body)
	}

	for _, path := range []string{"/ui/runs", "/ui/runs?env=dev&stack=app", "/ui/stack?stack=app&env=dev&view=runs"} {
		_, doc := x.page(path)
		body := words(doc)
		for _, want := range []string{"failed " + failed.Run.ID[:8] + " app@dev " + p3.Plan.ID[:8] + " tester", "applied " + ok.Run.ID[:8] + " app@dev " + p.Plan.ID[:8] + " tester"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s lacks %q:\n%s", path, want, body)
			}
		}
	}
	_, doc = x.page("/ui/plans?env=dev")
	if body := words(doc); !strings.Contains(body, p3.Plan.ID[:8]+" dev app") {
		t.Errorf("the plans list lacks the OpenTofu plan with its stack and env:\n%s", body)
	}
}

// The overview lists each OpenTofu stack with its drift, and an OpenTofu
// stack's page shows its drift check, each changed resource with what
// found it, its action, and the attributes that change, and its
// resources from the state index.
func TestTofuDriftPages(t *testing.T) {
	x := newTofuUI(t)
	x.driftCheck(second)
	_, doc := x.page("/ui/")
	if got := words(byID(t, doc, "tofu-stacks")); !strings.Contains(got, "app dev http 1 drifted 1 outside, 5 in code") {
		t.Errorf("the overview's OpenTofu stacks: %s", got)
	}
	body, doc := x.page("/ui/stack?stack=app&env=dev")
	facts := words(byID(t, doc, "drift-facts"))
	for _, want := range []string{"Status drifted", "Commit " + x.commit[:8] + " of demo", ", never applied", "Drifted since"} {
		if !strings.Contains(facts, want) {
			t.Errorf("the stack's drift lacks %q: %s", want, facts)
		}
	}
	changes := words(byID(t, doc, "drift-changes"))
	for _, want := range []string{
		"1 changed outside OpenTofu, 5 that the code would change",
		"local_file.note changed outside OpenTofu delete none",
		"local_file.note in the code create none",
		"random_id.imported in the code import none",
		"random_pet.gone in the code forget none",
		"random_pet.rotated in the code replace id, keepers",
		"module.inner.random_pet.p in the code create none",
	} {
		if !strings.Contains(changes, want) {
			t.Errorf("the stack's drift changes lack %q: %s", want, changes)
		}
	}
	if got := words(byID(t, doc, "index-rows")); !strings.Contains(got, "random_pet.kept random_pet registry.opentofu.org/hashicorp/random kept-pet") {
		t.Errorf("the stack's resources: %s", got)
	}
	if strings.Contains(body, "writ serve has no state for") {
		t.Errorf("the OpenTofu stack's page reads Writ state:\n%s", body)
	}
	_, _, js := visit(t, x.c, "GET", x.base+"/ui/stack?stack=app&env=dev&as=json", nil)
	if !strings.Contains(js, `"format":"writ.state-index/v0"`) {
		t.Errorf("the stack page's main document is not its state index: %s", js)
	}
}

// A stack with a state that writ serve has not indexed, such as one with
// no secret set, is on the overview, and the page it links answers with
// the stack's drift record and says the index is not built yet. No link
// of the overview answers 404.
func TestTofuStackWithoutIndex(t *testing.T) {
	x := newTofuUI(t)
	x.noSecretSet(server.DevEnv)
	path := "/ui/stack?env=" + server.DevEnv + "&stack=app"
	_, home := x.page("/ui/")
	if got := words(byID(t, home, "tofu-stacks")); !strings.Contains(got, "app "+server.DevEnv+" http 0") {
		t.Fatalf("the overview's stacks: %s", got)
	}
	var links []string
	each(home, func(n *html.Node) {
		if h, _ := attr(n, "href"); n.Data == "a" && strings.HasPrefix(h, "/ui/") && !slices.Contains(links, h) {
			links = append(links, h)
		}
	})
	if !slices.Contains(links, path) {
		t.Fatalf("the overview links no %s: %q", path, links)
	}
	for _, link := range links {
		if status, at, _ := visit(t, x.c, "GET", x.base+link, nil); status != http.StatusOK {
			t.Errorf("the overview links %s, which answers %d at %s", link, status, at)
		}
	}
	_, doc := x.page(path)
	if got := words(byID(t, doc, "no-index")); got != ui.Nones["unindexed"] {
		t.Errorf("the stack's resources: %s", got)
	}
	if got := words(byID(t, doc, "h-drift")); got != "Drift" {
		t.Errorf("the stack's drift: %s", got)
	}
}

// A state version with no secret set, as one written before writ serve
// kept them, makes a plan of its stack fail and its drift check find
// drift unknown. The plan's page and the stack's page each say that a
// person with adopt can seal a set for it, and with which command.
func TestTofuAdoptHint(t *testing.T) {
	x := newTofuUI(t)
	x.noSecretSet(server.DevEnv)
	want := "writ state adopt app --env " + server.DevEnv + " --version 1 --reason REASON"
	p := x.planEnded(second, server.DevEnv)
	if p.Plan.Status != "failed" {
		t.Fatalf("the plan of a state with no secret set: %+v\n%s", p.Plan, p.Text)
	}
	_, doc := x.page("/ui/plans/" + p.Plan.Ref)
	if got := words(byID(t, doc, "adopt")); !strings.Contains(got, "A person with adopt on the stack can seal a set for it") || !strings.HasSuffix(got, want) {
		t.Errorf("the plan's page: %s", got)
	}

	x.driftCheck(second)
	_, doc = x.page("/ui/stack?stack=app&env=" + server.DevEnv)
	if got := words(byID(t, doc, "drift-facts")); !strings.Contains(got, "Status failed") {
		t.Errorf("the stack's drift: %s", got)
	}
	if got := words(byID(t, doc, "adopt")); !strings.HasSuffix(got, want) {
		t.Errorf("the stack's page: %s", got)
	}
}
