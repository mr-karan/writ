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
			"overview": "/ui/", "start": "/ui/start", "plans": "/ui/plans", "approvals": "/ui/approvals", "runs": "/ui/runs", "repos": "/ui/repos", "audit": "/ui/audit",
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
			status, _, body := visit(t, c, "GET", s.Base+path, nil)
			if status != want {
				t.Errorf("%s: %s %s: %d, want %d: %.300s", s.Name, name, path, status, want, tagless(body))
			}
			// The full site shows each thing that starts a plan or a check.
			for _, mark := range map[string][]string{
				"plans":          {`href="#i-person"`, `href="#i-ci"`},
				"stack-drift":    {"Drift check, every <time"},
				"drift-check":    {"Drift check, every <time"},
				"audit-filtered": {"reason withheld", `href="#i-ci"`},
			}[name] {
				if s.Name == "full" && !strings.Contains(body, mark) {
					t.Errorf("%s: %s %s lacks %s", s.Name, name, path, mark)
				}
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

func (s *showTofu) Fill(_ context.Context, lock []byte, _ *tofu.Net, _ []string) ([]byte, error) {
	return lock, nil
}

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
	served := *cfg
	srv, err := server.New(ctx, server.Options{Config: &served, Store: store, Tofu: st, DevToken: strings.Repeat("d", 40), DevUser: "tester", Version: "showcase"})
	if err != nil {
		t.Fatal(err)
	}
	// The pages say drift checks run every 6h, as a config's drift block
	// says, while the seed runs each check when it needs one: set after
	// New, drift.every starts no checks of this control plane.
	served.DriftEvery = 6 * time.Hour
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
	ci := x.ciPlan(pending)
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
	var appDriftRef string
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(20 * time.Millisecond) {
		st, _, err := x.store.DriftStacks(ctx, server.DevEnv, "app", 1, func(string, string) bool { return true })
		if err != nil {
			t.Fatal(err)
		}
		if len(st) == 1 && st[0].Last != nil && !st[0].Checking {
			appDriftRef = st[0].Last.Ref
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no drift check of app ended")
		}
	}
	checker.Close()
	x.seedMany(ctx)

	// An apply of the CI job's plan before its approval is refused, so its
	// audit trail holds a row whose reason the page withholds.
	if status := x.call(x.srv, http.MethodPost, "/v1/runs", map[string]any{"format": "writ.run-request/v1", "plan": ci}, nil); status != http.StatusForbidden {
		t.Fatalf("apply the CI job's plan before its approval: %d", status)
	}

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
		"plan-ci":         "/ui/plans/" + ci,
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
		"stack-drift":     "/ui/stack?stack=app&env=" + server.DevEnv + "&view=drift",
		"drift-check":     "/ui/drift/" + appDriftRef,
		"drift-platform":  "/ui/drift/dc-platform",
		"stack-many":      "/ui/stack?stack=platform&env=" + server.DevEnv,
		"stack-many-find": "/ui/stack?stack=platform&env=" + server.DevEnv + "&q=module.cluster",
		"stack-modules":   "/ui/stack?stack=platform&env=" + server.DevEnv + "&group=module",
		"stack-root":      "/ui/stack?stack=alerts&env=" + server.DevEnv,
		"stack-root-flat": "/ui/stack?stack=alerts&env=" + server.DevEnv + "&group=flat",
		"repos":           "/ui/repos",
		"repo":            "/ui/repos/demo",
		"repo-new":        "/ui/repos/new",
		"access":          "/ui/access",
		"audit":           "/ui/audit",
		"audit-filtered":  "/ui/audit?plan=" + ci,
		"whoami":          "/ui/whoami",
		"settings":        "/ui/settings",
		"not-found":       "/ui/plans/" + strings.Repeat("0", 32),
	}
}

// ciPlan stores a pending plan that a CI job of merge request 42 of
// acme/infra started, with the document and the source of plan from, as
// writ serve stores a CI job's plan, and returns its id. A dev control
// plane verifies no CI token, so the plan is written to the store.
func (x *showcase) ciPlan(from *server.PlanInfo) string {
	x.t.Helper()
	ctx := context.Background()
	src, err := x.store.Plan(ctx, from.ID, time.Now())
	if err != nil {
		x.t.Fatal(err)
	}
	const ref, id = "GL1849213GL1849213GL184921", "5be0c1d27f3a4e968b2d0a71c4f9e352"
	now := time.Now()
	p := control.Plan{Ref: ref, Env: src.Env, Stack: src.Stack, RequestedAt: now, SourceDigest: src.SourceDigest, ConfigID: src.ConfigID, Source: src.Source,
		Author: control.Principal{Issuer: "gitlab", Subject: "project_path:acme/infra:ref_type:branch:ref:add-subnet", Person: "gitlab/user/1001",
			Display: "gitlab acme/infra #1849213 (alice)", Detail: json.RawMessage(`{"kind":"ci-job","issuer":"gitlab","subject":"project_path:acme/infra:ref_type:branch:ref:add-subnet",` +
				`"display":"gitlab acme/infra #1849213 (alice)","person":"gitlab/user/1001","teams":["infra_mr"],"matched":{}}`)},
		Teams: []string{"infra_mr"},
		Origin: json.RawMessage(`{"kind":"gitlab-merge-request","issuer":"https://gitlab.example.com","project_id":"4242","project":"acme/infra",` +
			`"pipeline":"1849213","job":"9922140","merge_request":42,"sha":"` + src.Source.Commit + `","ref":"refs/merge-requests/42/head"}`)}
	e := control.Entry{At: now, Request: ref, Kind: "request", Method: http.MethodPost, Route: "/v1/plans", Status: http.StatusAccepted,
		Principal: p.Author.Display, PrincipalKind: "ci-job", Person: p.Author.Person, Issuer: p.Author.Issuer, Subject: p.Author.Subject,
		Op: "plan", Env: p.Env, Stacks: []string{p.Stack}, Plan: ref, Decision: control.Allowed}
	if err := x.store.CreatePlan(ctx, p, control.TokenUse{}, e); err != nil {
		x.t.Fatal(err)
	}
	res := control.PlanResult{ID: id, Status: control.Pending, MadeAt: now, ExpiresAt: src.ExpiresAt, Risks: src.Risks, Changes: src.Changes,
		Document: src.Document, Text: src.Text, Saved: []byte("saved"), Need: src.Need, NeedRules: src.NeedRules, Tofu: src.Tofu}
	if err := x.store.FinishPlan(ctx, ref, res, control.Entry{At: now, Request: ref, Kind: "plan-finished", Decision: control.NoDecision}); err != nil {
		x.t.Fatal(err)
	}
	return id
}

// seedMany gives stack platform an index of 64 resources across eight
// categories, with made-up ids, and a drift check that names three of
// them, so the resources view is seen on a full list.
func (x *showcase) seedMany(ctx context.Context) {
	const aws, nomad, region = "registry.opentofu.org/hashicorp/aws", "registry.opentofu.org/hashicorp/nomad", "eu-west-1"
	var rows []control.IndexRow
	add := func(addr, typ, id string) *control.IndexRow {
		provider := aws
		if strings.HasPrefix(typ, "nomad_") {
			provider = nomad
		}
		rows = append(rows, control.IndexRow{Address: addr, Type: typ, Provider: provider, ID: id})
		return &rows[len(rows)-1]
	}
	arn := func(r *control.IndexRow, service, path string) {
		r.ARN, r.Region = "arn:aws:"+service+":"+region+":000000000000:"+path, region
	}
	// A load balancer's ID is its ARN, as the AWS provider sets it.
	lb := func(addr, typ, path string) {
		r := add(addr, typ, "")
		arn(r, "elasticloadbalancing", path)
		r.ID = r.ARN
	}
	lb("aws_lb.app", "aws_lb", "loadbalancer/app/app-alb/5c1e0a7d93b2f4e1")
	lb("aws_lb_listener.https", "aws_lb_listener", "listener/app/app-alb/5c1e0a7d93b2f4e1/1c0d2e3f4a5b6c7d")
	lb("aws_lb_listener.http", "aws_lb_listener", "listener/app/app-alb/5c1e0a7d93b2f4e1/2e7a3b4c5d6e7f80")
	for _, c := range []string{"blue", "green"} {
		lb(`aws_lb_target_group.web["`+c+`"]`, "aws_lb_target_group", "targetgroup/tg-web-"+c+"/0f3b4c5d6e7f8091")
	}
	add("module.vpc.aws_vpc.main", "aws_vpc", "vpc-000000001")
	for i, z := range []string{"a", "b", "c"} {
		add(`module.vpc.aws_subnet.private["`+z+`"]`, "aws_subnet", fmt.Sprintf("subnet-0a%d1c7d2e7", i))
		add(`module.vpc.aws_subnet.public["`+z+`"]`, "aws_subnet", fmt.Sprintf("subnet-0b%d4e9f1a2", i))
	}
	add("module.vpc.aws_route_table.private", "aws_route_table", "rtb-000000002")
	add("module.vpc.aws_nat_gateway.main", "aws_nat_gateway", "nat-000000003")
	add("module.vpc.aws_internet_gateway.main", "aws_internet_gateway", "igw-000000004")
	add("module.vpc.aws_eip.nat", "aws_eip", "eipalloc-0a1b2c3d4e")
	add("module.cluster.aws_security_group.nomad_client", "aws_security_group", "sg-000000005")
	add("module.cluster.aws_security_group.nomad_server", "aws_security_group", "sg-000000006")
	add("module.cluster.aws_security_group_rule.ingress_https", "aws_security_group_rule", "sgrule-3107714bd2")
	add("module.cluster.aws_vpc_security_group_ingress_rule.ssh", "aws_vpc_security_group_ingress_rule", "sgr-0a1b2c3d4e")
	for _, r := range []string{"nomad_client", "nomad_server"} {
		arn(add("aws_iam_role."+r, "aws_iam_role", r+"-prod"), "iam", "role/"+r+"-prod")
	}
	add("aws_iam_instance_profile.nomad_client", "aws_iam_instance_profile", "nomad_client-prod")
	add("aws_iam_role_policy_attachment.ssm", "aws_iam_role_policy_attachment", "nomad_client-prod-20261101000000000000000001")
	add("aws_iam_role_policy_attachment.ecr", "aws_iam_role_policy_attachment", "nomad_client-prod-20261101000000000000000002")
	arn(add("aws_iam_policy.logs", "aws_iam_policy", "arn:aws:iam::000000000000:policy/logs-write"), "iam", "policy/logs-write")
	add("aws_iam_role_policy.inline", "aws_iam_role_policy", "nomad_server-prod:inline")
	add("aws_iam_user.ci", "aws_iam_user", "ci-deploy")
	add("aws_iam_access_key.ci", "aws_iam_access_key", "ci-deploy-key")
	add("aws_kms_key.state", "aws_kms_key", "1f2e3d4c-0000-4b5a-9c8d-7e6f5a4b3c2d")
	add("aws_kms_alias.state", "aws_kms_alias", "alias/state")
	for r, lt := range map[string]string{"nomad_client": "lt-0c7e55a1b9", "nomad_server": "lt-0d8f66b2c0"} {
		add("module.cluster.aws_autoscaling_group."+r, "aws_autoscaling_group", strings.ReplaceAll(r, "_", "-")+"-prod")
		add("module.cluster.aws_launch_template."+r, "aws_launch_template", lt)
	}
	add("aws_instance.bastion", "aws_instance", "i-0a1b2c3d4e5f60718")
	add("aws_key_pair.ops", "aws_key_pair", "ops")
	arn(add("aws_lambda_function.rotate", "aws_lambda_function", "rotate-keys"), "lambda", "function:rotate-keys")
	add("aws_lambda_permission.rotate", "aws_lambda_permission", "AllowExecutionFromEvents")
	add("aws_ecr_repository.app", "aws_ecr_repository", "app")
	add("aws_ecr_lifecycle_policy.app", "aws_ecr_lifecycle_policy", "app")
	for i, j := range []string{"web", "worker", `cron["reports"]`, `cron["cleanup"]`} {
		r := add("module.jobs.nomad_job."+j, "nomad_job", strings.NewReplacer(`cron["`, "cron-", `"]`, "").Replace(j))
		r.Namespace, r.ModifyIndex = "app", fmt.Sprint(18200+i*37)
	}
	add("nomad_namespace.app", "nomad_namespace", "app")
	add("nomad_acl_policy.deploy", "nomad_acl_policy", "deploy")
	add("nomad_acl_token.deploy", "nomad_acl_token", "6a0f1e2d-0000-4c3b-8a79-5e4d3c2b1a09")
	add("nomad_variable.app", "nomad_variable", "nomad/jobs/web")
	add("nomad_csi_volume.data", "nomad_csi_volume", "data")
	for _, b := range []string{"logs", "assets"} {
		arn(add("aws_s3_bucket."+b, "aws_s3_bucket", "acme-"+b+"-prod"), "s3", "acme-"+b+"-prod")
	}
	add("aws_s3_bucket_versioning.assets", "aws_s3_bucket_versioning", "acme-assets-prod")
	add("aws_s3_bucket_policy.logs", "aws_s3_bucket_policy", "acme-logs-prod")
	add("aws_ebs_volume.data", "aws_ebs_volume", "vol-0a1b2c3d4e5f6a7b8")
	add("aws_efs_file_system.shared", "aws_efs_file_system", "fs-0a1b2c3d")
	add("aws_route53_zone.main", "aws_route53_zone", "Z0000000EXAMPLE")
	for _, n := range []string{"app", "api", "www"} {
		add("aws_route53_record."+n, "aws_route53_record", "Z0000000EXAMPLE_"+n+".example.com_A")
	}
	arn(add("aws_acm_certificate.app", "aws_acm_certificate", "arn:aws:acm:eu-west-1:000000000000:certificate/0a1b2c3d"), "acm", "certificate/0a1b2c3d")
	add("aws_acm_certificate_validation.app", "aws_acm_certificate_validation", "2026-11-01 00:00:00 +0000 UTC")
	add("kubernetes_namespace.ops", "kubernetes_namespace", "ops")
	if len(rows) < 60 {
		x.t.Fatalf("the full stack has %d resources", len(rows))
	}
	if _, err := x.store.ReplaceIndex(ctx, control.IndexWrite{Env: server.DevEnv, Stack: "platform", Backend: control.BackendHTTP, Version: 63, At: time.Now().Add(-2 * time.Hour), Rows: rows}); err != nil {
		x.t.Fatal(err)
	}
	x.seedRootOnly(ctx)
	const ref = "dc-platform"
	if err := x.store.BeginDrift(ctx, ref, server.DevEnv, "platform", time.Now().Add(-3*time.Hour)); err != nil {
		x.t.Fatal(err)
	}
	if _, err := x.store.FinishDrift(ctx, control.DriftCheck{Ref: ref, Status: control.DriftDrifted, MadeAt: time.Now().Add(-3 * time.Hour), Repo: "demo", Commit: "e81aa0950c4b2d1f", Outside: 1, Code: 2,
		Changes: []control.DriftChange{
			{Address: "aws_lb_listener.https", Kind: control.DriftOutside, Action: "update", Attributes: []string{"ssl_policy"}},
			{Address: "module.cluster.aws_autoscaling_group.nomad_client", Kind: control.DriftCode, Action: "update", Attributes: []string{"max_size", "desired_capacity"}},
			{Address: "module.cluster.aws_security_group_rule.ingress_https", Kind: control.DriftCode, Action: "replace", Attributes: []string{"cidr_blocks"}},
		}}); err != nil {
		x.t.Fatal(err)
	}
}

// seedRootOnly gives stack alerts an index of resources all in the root
// module, shaped like a real service's: a role whose policy attachments
// are keyed by policy ARNs, and a load balancer, its listeners, and its
// target groups, whose IDs are their ARNs.
func (x *showcase) seedRootOnly(ctx context.Context) {
	const aws, nomad, region, acct = "registry.opentofu.org/hashicorp/aws", "registry.opentofu.org/hashicorp/nomad", "eu-west-1", "000000000000"
	var rows []control.IndexRow
	add := func(addr, typ, id string) *control.IndexRow {
		provider := aws
		if strings.HasPrefix(typ, "nomad_") {
			provider = nomad
		}
		rows = append(rows, control.IndexRow{Address: addr, Type: typ, Provider: provider, ID: id})
		return &rows[len(rows)-1]
	}
	lb := func(addr, typ, path string) {
		a := "arn:aws:elasticloadbalancing:" + region + ":" + acct + ":" + path
		add(addr, typ, a).ARN = a
	}
	add("aws_iam_role.acme_nomad_alerts", "aws_iam_role", "acme-nomad-alerts-uat").ARN = "arn:aws:iam::" + acct + ":role/acme-nomad-alerts-uat"
	add("aws_iam_instance_profile.acme_nomad_alerts", "aws_iam_instance_profile", "acme-nomad-alerts-uat")
	for i, policy := range []string{
		"arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore",
		"arn:aws:iam::aws:policy/CloudWatchAgentServerPolicy",
		"arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryReadOnly",
		"arn:aws:iam::aws:policy/service-role/AmazonEC2RoleforSSM",
		"arn:aws:iam::" + acct + ":policy/acme-alerts-secrets-read",
		"arn:aws:iam::" + acct + ":policy/acme-alerts-queue-write",
		"arn:aws:iam::" + acct + ":policy/acme-alerts-logs-write",
		"arn:aws:iam::" + acct + ":policy/acme-alerts-kms-decrypt",
	} {
		add(`aws_iam_role_policy_attachment.acme_nomad_alerts["`+policy+`"]`, "aws_iam_role_policy_attachment", fmt.Sprintf("acme-nomad-alerts-uat-2026110100000000000000%04d", i))
	}
	lb("aws_lb.alerts", "aws_lb", "loadbalancer/app/acme-uat-nomad/edf7a1b2c3d4e5f6")
	lb("aws_lb_listener.https", "aws_lb_listener", "listener/app/acme-uat-nomad/edf7a1b2c3d4e5f6/9a8b7c6d5e4f3a2b")
	lb("aws_lb_listener.http", "aws_lb_listener", "listener/app/acme-uat-nomad/edf7a1b2c3d4e5f6/1b2c3d4e5f6a7b8c")
	lb(`aws_lb_listener_rule.alerts["api"]`, "aws_lb_listener_rule", "listener-rule/app/acme-uat-nomad/edf7a1b2c3d4e5f6/9a8b7c6d5e4f3a2b/0c1d2e3f4a5b6c7d")
	for _, tg := range []string{"web", "api", "triggers"} {
		lb(`aws_lb_target_group.alerts["`+tg+`"]`, "aws_lb_target_group", "targetgroup/acme-uat-alerts-"+tg+"/3c4d5e6f7a8b9c0d")
	}
	add("aws_security_group.alerts_lb", "aws_security_group", "sg-0a1b2c3d4e5f60001")
	add("aws_route53_record.alerts", "aws_route53_record", "Z0000000EXAMPLE_alerts.example.com_CNAME")
	add("aws_sqs_queue.alerts", "aws_sqs_queue", "https://sqs."+region+".amazonaws.com/"+acct+"/acme-alerts-uat").ARN = "arn:aws:sqs:" + region + ":" + acct + ":acme-alerts-uat"
	add("aws_kms_key.alerts", "aws_kms_key", "2a3b4c5d-0000-4e6f-8a9b-0c1d2e3f4a5b")
	for _, j := range []string{"alerts-front", "alerts-core", "alerts-triggerd"} {
		r := add(`nomad_job.alerts["`+j+`"]`, "nomad_job", j)
		r.Namespace, r.ModifyIndex = "alerts", "48211"
	}
	add("nomad_acl_policy.alerts", "nomad_acl_policy", "alerts-deploy")
	add("nomad_acl_token.alerts", "nomad_acl_token", "7b1f2e3d-0000-4c5b-9a87-6e5d4c3b2a10")
	add("nomad_csi_volume.alerts_data", "nomad_csi_volume", "alerts-data")
	if _, err := x.store.ReplaceIndex(ctx, control.IndexWrite{Env: server.DevEnv, Stack: "alerts", Backend: control.BackendHTTP, Version: 12, At: time.Now().Add(-40 * time.Minute), Rows: rows}); err != nil {
		x.t.Fatal(err)
	}
}
