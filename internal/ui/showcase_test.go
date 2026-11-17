//go:build uishowcase

package ui_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/authz"
	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/policy"
	"github.com/mr-karan/writ/internal/server"
	"github.com/mr-karan/writ/internal/tofu"
	"github.com/mr-karan/writ/internal/ui"
)

// TestShowcase serves the control UI of two dev control planes for
// screenshots and design work: one holds a plan and a run in every state
// the pages show, and one holds nothing. It writes their addresses, a
// one-time sign-in link for each, and the pages worth seeing to
// $WRIT_SHOWCASE_OUT/pages.json, then serves until
// $WRIT_SHOWCASE_OUT/stop exists or $WRIT_SHOWCASE_HOLD (default 4m)
// passes. It reaches no host but loopback:
//
//	go test -c -tags uishowcase -o showcase.test ./internal/ui
//	cd internal/ui && WRIT_SHOWCASE_OUT=DIR ../../showcase.test -test.run '^TestShowcase$'
func TestShowcase(t *testing.T) {
	out := os.Getenv("WRIT_SHOWCASE_OUT")
	if out == "" {
		t.Fatal("set WRIT_SHOWCASE_OUT to the directory pages.json goes to")
	}
	hold := 4 * time.Minute
	if v := os.Getenv("WRIT_SHOWCASE_HOLD"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			t.Fatal(err)
		}
		hold = d
	}
	sites := showcaseSites(t)
	for i := range sites {
		sites[i].Login = sites[i].srv.DevLink().URL
	}
	b, _ := json.MarshalIndent(sites, "", "  ")
	if err := os.WriteFile(filepath.Join(out, "pages.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("serving %s and %s", sites[0].Base, sites[1].Base)
	for deadline := time.Now().Add(hold); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(out, "stop")); err == nil {
			return
		}
	}
}

// TestShowcaseServes runs the showcase's two control planes as
// TestShowcase does, and returns once each served every page it lists.
func TestShowcaseServes(t *testing.T) {
	showcaseSites(t)
}

// showSite is a control plane of the showcase, as pages.json lists it.
type showSite struct {
	Name  string            `json:"name"`
	Base  string            `json:"base"`
	Login string            `json:"login"`
	Pages map[string]string `json:"pages"`
	srv   *server.Server
}

// showcaseSites runs the showcase's two control planes, and fails unless
// each, signed in with a sign-in link of its own, serves every page it
// lists.
func showcaseSites(t *testing.T) []showSite {
	t.Helper()
	full := newShowcase(t)
	pages := full.seed()
	empty := newShowcase(t)
	sites := []showSite{
		{Name: "full", Base: full.base, Pages: pages, srv: full.srv},
		{Name: "empty", Base: empty.base, srv: empty.srv, Pages: map[string]string{
			"overview": "/ui/", "start": "/ui/start", "plans": "/ui/plans", "approvals": "/ui/approvals", "runs": "/ui/runs", "repos": "/ui/repos",
		}},
	}
	for _, s := range sites {
		jar, _ := cookiejar.New(nil)
		c := &http.Client{Jar: jar}
		if status, at, body := visit(t, c, "GET", s.srv.DevLink().URL, nil); status != http.StatusOK || at != "/ui/start" {
			t.Fatalf("%s: sign in: %d at %s: %.300s", s.Name, status, at, tagless(body))
		}
		for name, path := range s.Pages {
			want := http.StatusOK
			if name == "not-found" {
				want = http.StatusNotFound
			}
			if status, _, body := visit(t, c, "GET", s.Base+path, nil); status != want {
				t.Errorf("%s: %s %s: %d, want %d: %.300s", s.Name, name, path, status, want, tagless(body))
			}
		}
	}
	return sites
}

// showTofu stands in for OpenTofu: each plan and apply answers with a
// recording of testdata/tofu. A plan of the stack queue and an apply of
// the stack slow wait until the showcase ends, so a plan stays planning
// and a run stays applying.
type showTofu struct {
	t       *testing.T
	mu      sync.Mutex
	rec     map[string]tofuRecording
	release chan struct{}
}

func (s *showTofu) set(stack string, rec tofuRecording) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec[stack] = rec
}

func (s *showTofu) read(name string) []byte {
	b, err := os.ReadFile(filepath.Join("testdata", "tofu", name))
	if err != nil {
		s.t.Error(err)
	}
	return b
}

func (s *showTofu) wait(ctx context.Context) error {
	select {
	case <-s.release:
		return errors.New("the showcase ended")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *showTofu) Fill(context.Context, []byte) error { return nil }

func (s *showTofu) Plan(ctx context.Context, st tofu.Stack) (tofu.Plan, error) {
	if st.Dir == "queue" {
		return tofu.Plan{}, s.wait(ctx)
	}
	s.mu.Lock()
	rec, ok := s.rec[st.Dir]
	s.mu.Unlock()
	if !ok {
		rec = second
	}
	return tofu.Plan{File: []byte(rec.plan), JSON: s.read(rec.plan)}, nil
}

func (s *showTofu) Apply(ctx context.Context, st tofu.Stack, saved []byte) (tofu.Applied, error) {
	if st.Dir == "slow" {
		return tofu.Applied{}, s.wait(ctx)
	}
	rec := second
	if string(saved) == third.plan {
		rec = third
	}
	log := s.read(rec.apply)
	out := tofu.Applied{Plan: s.read(rec.plan), Log: log}
	if rec.failed {
		return out, &tofu.StepError{Step: "apply", Code: 1, Stdout: log}
	}
	out.ShowErr = errNoState
	return out, nil
}

func (s *showTofu) Show(context.Context, tofu.Stack, []byte) (tofu.State, error) {
	return tofu.State{}, errNoState
}

func (s *showTofu) Pull(context.Context, tofu.Stack) (tofu.State, error) {
	return tofu.State{}, errNoState
}

type showcase struct {
	t      *testing.T
	srv    *server.Server
	store  *control.Store
	cfg    *server.Config
	base   string
	tofu   *showTofu
	repo   string
	commit string
	n      int
}

// newShowcase runs a dev control plane whose plans need one approval
// from someone other than their author, and where a policy denies a
// plan that forgets a resource in prod.
func newShowcase(t *testing.T) *showcase {
	t.Helper()
	ctx := context.Background()
	data := t.TempDir()
	store, err := control.Open(ctx, control.Options{Location: filepath.Join(data, "control.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	ts := httptest.NewUnstartedServer(nil)
	base := "http://" + ts.Listener.Addr().String()
	cfg := server.DevConfig(server.DevOptions{URL: base, Listen: ts.Listener.Addr().String(), Database: filepath.Join(data, "control.db"), Data: data})
	cfg.Policy.Approvals = []authz.ApprovalRule{{Stacks: []string{"**"}, Envs: []string{"*"}, Risks: authz.Risks, Need: 1}}
	cfg.Policies = &server.Policies{Timeout: time.Minute, Memory: policy.DefaultMemory, Sets: []server.PolicySet{
		{Name: "prod-forgets", Level: policy.Mandatory, Files: map[string]string{"forget.rego": `package main

import rego.v1

deny contains sprintf("%s is forgotten in prod; remove it with a removed block reviewed by the owners", [rc.address]) if {
	input.writ.environment == "prod"
	some rc in input.resource_changes
	rc.change.actions == ["forget"]
}
`}},
	}}
	st := &showTofu{t: t, rec: map[string]tofuRecording{}, release: make(chan struct{})}
	srv, err := server.New(ctx, server.Options{Config: cfg, Store: store, Tofu: st, DevToken: strings.Repeat("d", 40), DevUser: "tester", Version: "showcase"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(st.release) })
	u, err := ui.New(ui.Options{Server: srv})
	if err != nil {
		t.Fatal(err)
	}
	u.Mount()
	ts.Config.Handler = srv.Handler()
	ts.Start()
	t.Cleanup(ts.Close)
	return &showcase{t: t, srv: srv, store: store, cfg: cfg, base: base, tofu: st}
}

// showcaseRepo is a Git repository of four OpenTofu stacks on the http backend.
func showcaseRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	lock := "provider \"registry.opentofu.org/hashicorp/random\" {\n  version = \"3.9.1\"\n  hashes  = [\"h1:abc=\"]\n}\n\n" +
		"provider \"registry.opentofu.org/hashicorp/local\" {\n  version = \"2.9.1\"\n  hashes  = [\"h1:def=\"]\n}\n"
	for _, stack := range []string{"app", "network", "slow", "queue"} {
		if err := os.MkdirAll(filepath.Join(dir, stack), 0o755); err != nil {
			t.Fatal(err)
		}
		for name, text := range map[string]string{
			"main.tf":             "terraform {\n  backend \"http\" {}\n}\n\nresource \"random_pet\" \"kept\" {}\n",
			".terraform.lock.hcl": lock,
		} {
			if err := os.WriteFile(filepath.Join(dir, stack, name), []byte(text), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "first")
	top, _ := filepath.EvalSymlinks(dir)
	return top
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Customer A", "GIT_AUTHOR_EMAIL=user@example.com", "GIT_COMMITTER_NAME=Customer A",
		"GIT_COMMITTER_EMAIL=user@example.com", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func (x *showcase) call(srv *server.Server, method, path string, body map[string]any, into any) int {
	x.t.Helper()
	sess := devSession(x.t, srv)
	var status int
	var resp []byte
	if method == http.MethodPost {
		b, _ := json.Marshal(body)
		status, resp = srv.Post(httptest.NewRequest("POST", "/", nil), sess, path, b)
	} else {
		status, resp = srv.Call(httptest.NewRequest("GET", "/", nil), sess, path)
	}
	if into != nil {
		if err := json.Unmarshal(resp, into); err != nil {
			x.t.Fatalf("%s %s: %d %v %.500s", method, path, status, err, resp)
		}
	}
	return status
}

// recommit commits a change to stack, so each plan has its own commit
// and so its own id.
func (x *showcase) recommit(stack string) {
	x.t.Helper()
	x.n++
	f := filepath.Join(x.repo, stack, "main.tf")
	b, err := os.ReadFile(f)
	if err != nil {
		x.t.Fatal(err)
	}
	if err := os.WriteFile(f, append(b, fmt.Sprintf("# change %d\n", x.n)...), 0o644); err != nil {
		x.t.Fatal(err)
	}
	git(x.t, x.repo, "commit", "-q", "-am", fmt.Sprintf("change %d", x.n))
	x.commit = gitHead(x.t, x.repo)
}

// plan asks srv for a plan of stack in env, and returns it once it is no
// longer planning, unless wait is false.
func (x *showcase) plan(srv *server.Server, stack, env string, wait bool) *server.PlanInfo {
	x.t.Helper()
	x.recommit(stack)
	var created server.RemotePlan
	if status := x.call(srv, http.MethodPost, "/v1/plans", map[string]any{"format": "writ.plan-request/v1", "env": env, "stacks": []string{stack},
		"source": map[string]any{"repo": "demo", "commit": x.commit}}, &created); status != http.StatusAccepted {
		x.t.Fatalf("plan %s@%s: %d", stack, env, status)
	}
	if !wait {
		return created.Plan
	}
	return x.until("/v1/plans/"+created.Plan.Ref, "plan", "planning").Plan
}

// until gets path until the status of its document's key is no longer
// pending.
func (x *showcase) until(path, key, pending string) server.RemotePlan {
	x.t.Helper()
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(20 * time.Millisecond) {
		var head map[string]json.RawMessage
		var obj struct{ Status string }
		status := x.call(x.srv, http.MethodGet, path, nil, &head)
		if status != http.StatusOK || json.Unmarshal(head[key], &obj) != nil {
			x.t.Fatalf("GET %s: %d", path, status)
		}
		if obj.Status != pending {
			var p server.RemotePlan
			x.call(x.srv, http.MethodGet, path, nil, &p)
			return p
		}
		if time.Now().After(deadline) {
			x.t.Fatalf("GET %s: still %s", path, pending)
		}
	}
}

func (x *showcase) approve(reviewer *server.Server, id string) {
	x.t.Helper()
	if status := x.call(reviewer, http.MethodPost, "/v1/plans/"+id+"/approvals", nil, nil); status != http.StatusOK {
		x.t.Fatalf("approve %s: %d", id, status)
	}
}

// apply starts a run of plan id and returns its id, once it ends unless
// wait is false.
func (x *showcase) apply(id string, wait bool) string {
	x.t.Helper()
	var started server.RemoteRun
	if status := x.call(x.srv, http.MethodPost, "/v1/runs", map[string]any{"format": "writ.run-request/v1", "plan": id}, &started); status != http.StatusAccepted {
		x.t.Fatalf("apply %s: %d", id, status)
	}
	if wait {
		x.until("/v1/runs/"+started.Run.ID, "run", "applying")
	}
	return started.Run.ID
}

// seed fills the control plane and returns the pages that show it, by
// name.
func (x *showcase) seed() map[string]string {
	t := x.t
	ctx := context.Background()
	x.repo = showcaseRepo(t)
	x.commit = gitHead(t, x.repo)
	if status := x.call(x.srv, http.MethodPost, "/v1/repos", map[string]any{"format": "writ.repo-request/v0", "name": "demo", "forge": "dir", "project": x.repo}, nil); status != http.StatusCreated {
		t.Fatalf("add the repository: %d", status)
	}
	reviewer, err := server.New(ctx, server.Options{Config: x.cfg, Store: x.store, Tofu: x.tofu, DevToken: strings.Repeat("r", 40), DevUser: "reviewer", Version: "showcase"})
	if err != nil {
		t.Fatal(err)
	}
	// The reviewer signs in as the dev user does, through the control UI.
	ru, err := ui.New(ui.Options{Server: reviewer})
	if err != nil {
		t.Fatal(err)
	}
	ru.Mount()

	superseded := x.plan(x.srv, "app", server.DevEnv, true)
	applied := x.plan(x.srv, "app", server.DevEnv, true)
	x.approve(reviewer, applied.ID)
	appliedRun := x.apply(applied.ID, true)

	x.tofu.set("app", third)
	failed := x.plan(x.srv, "app", server.DevEnv, true)
	x.approve(reviewer, failed.ID)
	failedRun := x.apply(failed.ID, true)
	x.tofu.set("app", second)

	pending := x.plan(x.srv, "app", server.DevEnv, true)
	approved := x.plan(x.srv, "network", server.DevEnv, true)
	x.approve(reviewer, approved.ID)
	others := x.plan(reviewer, "network", server.DevEnv, true)
	refused := x.plan(x.srv, "app", "prod", true)

	slow := x.plan(x.srv, "slow", server.DevEnv, true)
	x.approve(reviewer, slow.ID)
	reviewer.Close()

	if _, err := x.store.ReplaceIndex(ctx, control.IndexWrite{Env: server.DevEnv, Stack: "app", Backend: control.BackendHTTP, Version: 1, At: time.Now(),
		Rows: []control.IndexRow{
			{Address: "random_pet.kept", Type: "random_pet", Provider: "registry.opentofu.org/hashicorp/random", ID: "kept-pet"},
			{Address: "random_id.imported", Type: "random_id", Provider: "registry.opentofu.org/hashicorp/random", ID: "aGVsbG8"},
			{Address: "local_file.note", Type: "local_file", Provider: "registry.opentofu.org/hashicorp/local", ID: "8b7df143d91c716ecfa5fc1730022f6b421b05ce"},
		}}); err != nil {
		t.Fatal(err)
	}
	cfg := *x.cfg
	cfg.DriftEvery = time.Hour
	checker, err := server.New(ctx, server.Options{Config: &cfg, Store: x.store, Tofu: x.tofu, DevToken: strings.Repeat("e", 40), DevUser: "checker", Version: "showcase"})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(20 * time.Millisecond) {
		st, _, err := x.store.DriftStacks(ctx, server.DevEnv, "app", 1, func(string, string) bool { return true })
		if err != nil {
			t.Fatal(err)
		}
		if len(st) == 1 && st[0].Last != nil && !st[0].Checking {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no drift check of app ended")
		}
	}
	checker.Close()

	// Another control plane that starts on the same database ends what it
	// finds in flight, so these start after the others closed.
	applyingRun := x.apply(slow.ID, false)
	planning := x.plan(x.srv, "queue", server.DevEnv, false)

	return map[string]string{
		"overview":        "/ui/",
		"start":           "/ui/start",
		"plans":           "/ui/plans",
		"approvals":       "/ui/approvals",
		"plan-pending":    "/ui/plans/" + pending.ID,
		"plan-others":     "/ui/plans/" + others.ID,
		"plan-approved":   "/ui/plans/" + approved.ID,
		"plan-applied":    "/ui/plans/" + applied.ID,
		"plan-applying":   "/ui/plans/" + slow.ID,
		"plan-failed":     "/ui/plans/" + failed.ID,
		"plan-refused":    "/ui/plans/" + refused.ID,
		"plan-superseded": "/ui/plans/" + superseded.ID,
		"plan-planning":   "/ui/plans/" + planning.Ref,
		"plan-filtered":   "/ui/plans/" + pending.ID + "?risk=delete",
		"runs":            "/ui/runs",
		"run-applied":     "/ui/runs/" + appliedRun,
		"run-failed":      "/ui/runs/" + failedRun,
		"run-applying":    "/ui/runs/" + applyingRun,
		"stack":           "/ui/stack?stack=app&env=" + server.DevEnv,
		"stack-runs":      "/ui/stack?stack=app&env=" + server.DevEnv + "&view=runs",
		"stack-plans":     "/ui/stack?stack=app&env=" + server.DevEnv + "&view=plans",
		"repos":           "/ui/repos",
		"repo":            "/ui/repos/demo",
		"repo-new":        "/ui/repos/new",
		"access":          "/ui/access",
		"whoami":          "/ui/whoami",
		"settings":        "/ui/settings",
		"not-found":       "/ui/plans/" + strings.Repeat("0", 32),
	}
}
