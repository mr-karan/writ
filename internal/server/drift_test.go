package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/control/controltest"
	"github.com/mr-karan/writ/internal/forge/forgetest"
	"github.com/mr-karan/writ/internal/server"
	"github.com/mr-karan/writ/internal/tofu"
)

// tofuOutside is a value of random_pet.p's keepers that someone set
// outside OpenTofu: only a mark covers it, since no input of writ serve
// holds it.
const tofuOutside = "canary-set-outside-9c3e"

// driftPlanJSON is `tofu show -json` of a no-change plan of app's
// random_pet.p, with drift and changes when drifted: OpenTofu read the
// pet's keepers and length changed outside it, and an apply would set
// them back and delete random_id.gone.
func driftPlanJSON(drifted bool) string {
	pet := func(c string, length int) string {
		return `{"id":"keen-mole","keepers":{"c":"` + c + `"},"length":` + map[int]string{2: "2", 3: "3"}[length] + `}`
	}
	sensitive := `{"keepers":{"c":true}}`
	change := func(actions, before, after string) string {
		return `{"address":"random_pet.p","mode":"managed","type":"random_pet","name":"p","provider_name":"registry.opentofu.org/hashicorp/random",
   "change":{"actions":` + actions + `,"before":` + before + `,"after":` + after + `,"before_sensitive":` + sensitive + `,"after_sensitive":` + sensitive + `}}`
	}
	changes := change(`["no-op"]`, pet(tofuCanary, 2), pet(tofuCanary, 2))
	drift := ""
	if drifted {
		changes = change(`["update"]`, pet(tofuOutside, 3), pet(tofuCanary, 2)) + `,
  {"address":"random_id.gone","mode":"managed","type":"random_id","name":"gone","provider_name":"registry.opentofu.org/hashicorp/random",
   "change":{"actions":["delete"],"before":{"byte_length":4},"after":null,"before_sensitive":false,"after_sensitive":false}}`
		drift = `,"resource_drift":[` + change(`["update"]`, pet(tofuCanary, 2), pet(tofuOutside, 3)) + `]`
	}
	return `{"format_version":"1.2","terraform_version":"1.13.9",
 "variables":{"canary":{"value":"` + tofuCanary + `"}},
 "configuration":{"provider_config":{"random":{"name":"random","full_name":"registry.opentofu.org/hashicorp/random"}},
  "root_module":{"variables":{"canary":{"sensitive":true}}}},
 "resource_changes":[` + changes + `]` + drift + `}`
}

// driftPlan answers each plan with driftPlanJSON.
func driftPlan(drifted bool) func() (tofu.Plan, error) {
	return func() (tofu.Plan, error) {
		return tofu.Plan{File: []byte("saved drift plan"), JSON: []byte(driftPlanJSON(drifted))}, nil
	}
}

// driftFiles is tofuFiles with a second OpenTofu stack, web.
func driftFiles() map[string]forgetest.File {
	files := tofuFiles()
	files["infra/web/main.tf"] = forgetest.File{Content: "terraform {\n  backend \"http\" {}\n}\n\nresource \"random_pet\" \"w\" {}\n"}
	files["infra/web/.terraform.lock.hcl"] = forgetest.File{Content: tofuLockFile}
	return files
}

// driftFixture is a control plane with tofuApplyFixture's rules, a team
// that may read only web, at most one plan for each principal, limits
// added to its limits block, and the repository infra whose main branch
// holds app and web. app is applied, and the state index holds web too.
func driftFixture(t *testing.T, runner *fakeTofu, limits ...string) *fixture {
	t.Helper()
	return driftFixtureOn(t, controltest.Default(), runner, limits...)
}

// driftFixtureOn is driftFixture with its control database on e.
func driftFixtureOn(t *testing.T, e controltest.Engine, runner *fakeTofu, limits ...string) *fixture {
	t.Helper()
	approvals := withApprovals(t, func(text string) string {
		return strings.Replace(text, "limits {\n", "limits {\nplans_per_principal = 1\n"+strings.Join(limits, "\n")+"\n", 1) + `grant {
  teams = ["reviewers"]
  stacks = ["app"]
  envs = ["prod"]
  can = ["read", "apply"]
  risks = ["create", "update", "replace", "delete", "data-loss"]
}

team "web_readers" {
  issuer = "gitlab"
  match = { project_id = ["4343"] }
}

grant {
  teams = ["web_readers"]
  stacks = ["web"]
  envs = ["prod"]
  can = ["read"]
}
`
	})
	f := tofuFixtureOn(t, e, runner, `provider_uids = "61000-61063"`+"\n", approvals)
	if f.cfg.Limits.PlansPerPrincipal != 1 {
		t.Fatalf("the config allows %d plans for each principal, want 1", f.cfg.Limits.PlansPerPrincipal)
	}
	f.gitlab.Commit(4242, "main", driftFiles())
	if r := f.do("POST", "/v1/repos", admin(f), repoRequest(nil)); r.status != http.StatusCreated {
		t.Fatalf("add: %d %s", r.status, r.body)
	}
	appliedApp(t, f, "add-cert")
	if err := f.srv.FillIndex("prod", "web", 1, []byte(tofuStateJSON)); err != nil {
		t.Fatal(err)
	}
	return f
}

// drift gets GET /v1/drift with path's query as tok, and returns its
// document.
func (f *fixture) drift(tok, query string) server.DriftDoc {
	f.t.Helper()
	r := f.do("GET", "/v1/drift"+query, tok, nil)
	var doc server.DriftDoc
	if r.status != http.StatusOK || json.Unmarshal(r.body, &doc) != nil {
		f.t.Fatalf("GET /v1/drift%s: %d %s", query, r.status, r.body)
	}
	return doc
}

// checkOf returns the newest drift check of stack in doc, or fails.
func checkOf(t *testing.T, doc server.DriftDoc, stack string) server.DriftCheckInfo {
	t.Helper()
	for _, st := range doc.Stacks {
		if st.Stack == stack && st.Check != nil {
			return *st.Check
		}
	}
	t.Fatalf("no drift check of %s in %+v", stack, doc)
	return server.DriftCheckInfo{}
}

// checked is the stacks the runner planned since plan n, in order.
func (r *fakeTofu) planned(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, s := range r.stacks[n:] {
		out = append(out, s.Dir)
	}
	return out
}

func (r *fakeTofu) plans() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.stacks)
}

// Drift checks plan each OpenTofu stack@env that writ serve keeps state
// for, at the head of its repository's default branch, with a state
// token that lives while the check plans, through the plan slots: one at
// a time here, those checked longest ago first. A check whose plan
// changes something marks the stack drifted, with what changed and never
// a value, until a clean check clears it.
func TestDriftChecks(t *testing.T) {
	runner := &fakeTofu{}
	f := driftFixture(t, runner)
	main, err := f.store.Repo(context.Background(), "infra")
	if err != nil {
		t.Fatal(err)
	}
	// The state backend answers a check's token after the sweep of the
	// state access of runs that no process runs.
	var during []int
	runner.mu.Lock()
	runner.plan = func() (tofu.Plan, error) {
		f.srv.Sweep()
		runner.mu.Lock()
		c := runner.tokens[len(runner.tokens)-1]
		runner.mu.Unlock()
		during = append(during, runner.get(c))
		return driftPlan(false)()
	}
	runner.mu.Unlock()

	n := runner.plans()
	f.srv.LookForDrift(time.Hour)
	if got := runner.planned(n); !slices.Equal(got, []string{"app"}) {
		t.Fatalf("the first look planned %v, want app alone: one plan slot, and app sorts first\n%+v", got, checkOf(t, f.drift(f.token(nil), ""), "app"))
	}
	if !slices.Equal(during, []int{http.StatusOK}) {
		t.Fatalf("the state backend answered the check's token with %v after a sweep, want 200", during)
	}
	tok := f.token(nil)
	listed := f.drift(tok, "?env=prod")
	app := checkOf(t, listed, "app")
	if listed.Every != "1h" || app.StartedBy != server.StartedByDrift {
		t.Fatalf("GET /v1/drift says checks run every %q, and its check of app was started by %q; want 1h and drift", listed.Every, app.StartedBy)
	}
	if app.Status != "clean" || app.Plan == "" || app.Commit != main.Head || app.Repo != "infra" || app.Outside+app.Code != 0 || app.DriftedSince != "" || app.Error != "" {
		t.Fatalf("the check of app: %+v, want clean at %s", app, main.Head)
	}

	n = runner.plans()
	f.srv.LookForDrift(time.Hour)
	if got := runner.planned(n); !slices.Equal(got, []string{"web"}) {
		t.Fatalf("the second look planned %v, want web, the stack never checked", got)
	}
	n = runner.plans()
	f.srv.LookForDrift(time.Hour)
	if got := runner.planned(n); len(got) != 0 {
		t.Fatalf("a look within drift.every of each check planned %v", got)
	}
	for _, want := range []string{"app", "web", "app"} {
		time.Sleep(5 * time.Millisecond)
		n = runner.plans()
		f.srv.LookForDrift(time.Millisecond)
		if got := runner.planned(n); !slices.Equal(got, []string{want}) {
			t.Fatalf("a look with every check due planned %v, want %s, the one checked longest ago", got, want)
		}
	}

	// Each look checks one stack, so lookBoth checks web and then app.
	lookBoth := func() {
		for range 2 {
			time.Sleep(5 * time.Millisecond)
			f.srv.LookForDrift(time.Millisecond)
		}
	}
	// A plan that finds drift marks app, and a second keeps when it began.
	runner.mu.Lock()
	runner.plan = driftPlan(true)
	runner.mu.Unlock()
	lookBoth()
	r := f.do("GET", "/v1/drift?env=prod&stack=app", tok, nil)
	var doc server.DriftDoc
	if err := json.Unmarshal(r.body, &doc); err != nil || r.status != http.StatusOK || len(doc.Stacks) != 1 {
		t.Fatalf("GET /v1/drift of app: %d %v %s", r.status, err, r.body)
	}
	got := checkOf(t, doc, "app")
	want := []control.DriftChange{
		{Address: "random_pet.p", Kind: "outside", Action: "update", Attributes: []string{"keepers", "length"}},
		{Address: "random_id.gone", Kind: "code", Action: "delete"},
		{Address: "random_pet.p", Kind: "code", Action: "update", Attributes: []string{"keepers", "length"}},
	}
	slices.SortFunc(got.Changes, func(a, b control.DriftChange) int { return strings.Compare(a.Kind+a.Address, b.Kind+b.Address) })
	slices.SortFunc(want, func(a, b control.DriftChange) int { return strings.Compare(a.Kind+a.Address, b.Kind+b.Address) })
	if got.Status != "drifted" || got.Outside != 1 || got.Code != 2 || got.DriftedSince == "" || got.DriftedSince != got.CheckedAt ||
		!slices.EqualFunc(got.Changes, want, func(a, b control.DriftChange) bool {
			return a.Address == b.Address && a.Kind == b.Kind && a.Action == b.Action && slices.Equal(a.Attributes, b.Attributes)
		}) {
		t.Fatalf("the drifted check of app: %+v\nwant changes %+v", got, want)
	}
	for _, secret := range []string{tofuCanary, tofuOutside, tofuEnvSecret} {
		if strings.Contains(string(r.body), secret) {
			t.Errorf("GET /v1/drift holds %s:\n%s", secret, r.body)
		}
	}
	first := got
	stored := func() control.DriftCheck {
		t.Helper()
		st, _, err := f.store.DriftStacks(context.Background(), "prod", "app", 1, func(string, string) bool { return true })
		if err != nil || len(st) != 1 || st[0].Last == nil {
			t.Fatalf("the stored check of app: %+v %v", st, err)
		}
		return *st[0].Last
	}
	firstStored := stored()
	lookBoth()
	if again := stored(); again.Status != control.DriftDrifted || !again.MadeAt.After(firstStored.MadeAt) || !again.DriftedSince.Equal(firstStored.MadeAt) {
		t.Fatalf("a second drifted check of app: %+v, want drifted since %s", again, firstStored.MadeAt)
	}

	// Neither approvals nor applies take a drift plan. A principal that
	// may not read the stack learns nothing of it.
	for _, req := range []struct{ method, path, body string }{
		{"POST", "/v1/plans/" + first.Plan + "/approvals", ""},
		{"POST", "/v1/runs", `{"format":"writ.run-request/v1","plan":"` + first.Plan + `"}`},
	} {
		r := f.do(req.method, req.path, bob(f), strings.NewReader(req.body))
		if r.status != http.StatusConflict || r.class() != "state" || !strings.Contains(string(r.body), "is a drift check of app@prod, which writ serve never approves or applies") {
			t.Errorf("%s %s with a drift plan: %d %s", req.method, req.path, r.status, r.body)
		}
		stranger := f.token(map[string]any{"project_id": "9999", "user_access_level": "maintainer"})
		if r := f.do(req.method, req.path, stranger, strings.NewReader(req.body)); r.status != http.StatusNotFound || strings.Contains(string(r.body), "app@prod") || strings.Contains(string(r.body), "drift") {
			t.Errorf("%s %s with a drift plan by a principal in no team: %d %s", req.method, req.path, r.status, r.body)
		}
	}

	// A clean plan clears the mark.
	runner.mu.Lock()
	runner.plan = driftPlan(false)
	runner.mu.Unlock()
	lookBoth()
	if clean := checkOf(t, f.drift(tok, "?env=prod&stack=app"), "app"); clean.Status != "clean" || clean.DriftedSince != "" || len(clean.Changes) != 0 {
		t.Fatalf("the check of app after a clean plan: %+v", clean)
	}
	scanForCanaries(t, f, tofuCanary, tofuEnvSecret, tofuOutside)
}

// GET /v1/drift lists only the stack@envs the principal may read, before
// it counts the limit, and GET /v1/index serves a stack's index rows with
// the allow-listed attributes and nothing else, to a principal that may
// read the stack.
func TestDriftRoutesByGrant(t *testing.T) {
	runner := &fakeTofu{}
	f := driftFixture(t, runner)
	webReader := f.token(map[string]any{"project_id": "4343"})
	stranger := f.token(map[string]any{"project_id": "9999"})

	if doc := f.drift(f.token(nil), "?limit=1"); len(doc.Stacks) != 1 || doc.Stacks[0].Stack != "app" || !doc.More {
		t.Fatalf("GET /v1/drift?limit=1 as a reader of every stack: %+v", doc)
	}
	doc := f.drift(webReader, "?limit=1")
	if len(doc.Stacks) != 1 || doc.Stacks[0].Stack != "web" || doc.More {
		t.Fatalf("GET /v1/drift?limit=1 as a reader of web alone: %+v, want web and no more", doc)
	}
	if doc.Every != "" {
		t.Fatalf("GET /v1/drift of a config with no drift block says checks run every %q", doc.Every)
	}
	if w := doc.Stacks[0]; w.Env != "prod" || w.Backend != "http" || w.Resources != len(tofuIndex) || w.IndexedVersion != 1 || w.OutsideWrite != nil || w.Check != nil {
		t.Fatalf("web in GET /v1/drift: %+v", w)
	}
	if doc := f.drift(stranger, ""); len(doc.Stacks) != 0 {
		t.Fatalf("GET /v1/drift as a principal in no team: %+v", doc)
	}
	if r := f.do("GET", "/v1/drift?env=staging", f.token(nil), nil); r.status != http.StatusBadRequest {
		t.Fatalf("GET /v1/drift of an env writ serve does not serve: %d %s", r.status, r.body)
	}

	for _, c := range []struct {
		tok, stack string
		status     int
	}{{webReader, "app", http.StatusForbidden}, {stranger, "web", http.StatusForbidden}, {f.token(nil), "network", http.StatusNotFound}} {
		if r := f.do("GET", "/v1/index?env=prod&stack="+c.stack, c.tok, nil); r.status != c.status || strings.Contains(string(r.body), "keen-mole") {
			t.Errorf("GET /v1/index of %s: %d %s, want %d", c.stack, r.status, r.body, c.status)
		}
	}
	r := f.do("GET", "/v1/index?env=prod&stack=web", webReader, nil)
	var ix struct {
		Format    string           `json:"format"`
		Backend   string           `json:"backend"`
		Version   int64            `json:"indexed_version"`
		Resources []map[string]any `json:"resources"`
	}
	if r.status != http.StatusOK || json.Unmarshal(r.body, &ix) != nil || ix.Format != "writ.state-index/v0" || ix.Backend != "http" || ix.Version != 1 || len(ix.Resources) != len(tofuIndex) {
		t.Fatalf("GET /v1/index of web: %d %s", r.status, r.body)
	}
	keys := []string{"address", "arn", "id", "identifier", "modify_index", "namespace", "provider", "region", "type"}
	for i, res := range ix.Resources {
		want := tofuIndex[i]
		if got := slices.Sorted(maps.Keys(res)); !slices.Equal(got, keys) || res["address"] != want.Address || res["id"] != want.ID || res["arn"] != want.ARN {
			t.Errorf("resource %d of web's index: %v, want the keys %v of %+v", i, res, keys, want)
		}
	}
	for _, canary := range []string{tofuMarked, tofuNotListed, tofuNotForType, tofuCanary} {
		if strings.Contains(string(r.body), canary) {
			t.Errorf("GET /v1/index holds %s:\n%s", canary, r.body)
		}
	}
}

// A drift check during which an apply of its stack@env ran keeps nothing,
// since it may have read a state the apply was changing, and the stack is
// due again at the next look.
func TestDriftCheckDuringApply(t *testing.T) {
	runner := &fakeTofu{}
	f := driftFixture(t, runner)
	release := make(chan struct{})
	entered := make(chan struct{})
	runner.mu.Lock()
	runner.plan = func() (tofu.Plan, error) {
		close(entered)
		<-release
		return driftPlan(true)()
	}
	runner.mu.Unlock()
	f.srv.StartDriftLook(time.Hour)
	select {
	case <-entered:
	case <-time.After(time.Minute):
		t.Fatalf("no drift check planned: %+v", f.drift(f.token(nil), ""))
	}
	runner.mu.Lock()
	runner.plan = nil
	runner.mu.Unlock()
	appliedApp(t, f, "edit")
	close(release)
	f.srv.WaitDrift()
	runner.mu.Lock()
	runner.plan = driftPlan(false)
	runner.mu.Unlock()
	if doc := f.drift(f.token(nil), "?env=prod&stack=app"); len(doc.Stacks) != 1 || doc.Stacks[0].Check != nil {
		t.Fatalf("app after a check overlapped by an apply: %+v, want no check kept", doc)
	}
	n := runner.plans()
	f.srv.LookForDrift(time.Hour)
	if got := runner.planned(n); !slices.Equal(got, []string{"app"}) {
		t.Fatalf("the next look planned %v, want app again", got)
	}
	if c := checkOf(t, f.drift(f.token(nil), "?env=prod&stack=app"), "app"); c.Status != "clean" {
		t.Fatalf("the check after the apply: %+v", c)
	}
}

// A drift check never takes the last free plan slot. With limits.plans =
// 2 and one person's plan running, a look starts no check, and a second
// person's plan still runs.
func TestDriftLeavesAPlanSlot(t *testing.T) {
	runner := &fakeTofu{}
	f := driftFixture(t, runner, "plans = 2")
	release := make(chan struct{})
	entered := make(chan struct{}, 3)
	runner.mu.Lock()
	runner.plan = func() (tofu.Plan, error) {
		entered <- struct{}{}
		<-release
		return tofu.Plan{File: []byte("saved"), JSON: []byte(tofuPlanJSON)}, nil
	}
	runner.mu.Unlock()
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	post := func(person, branch string) (int, string, string) {
		t.Helper()
		sha := f.gitlab.Commit(4242, branch, tofuFiles())
		tok := f.token(map[string]any{"sha": sha, "ref_path": "refs/heads/" + branch, "user_id": person, "user_login": person})
		r := f.do("POST", "/v1/plans", tok, planRequest(func(m map[string]any) {
			delete(m, "bundle")
			m["source"] = map[string]any{}
			m["stacks"] = []string{"app"}
		}))
		ref := ""
		if p, ok := r.doc["plan"].(map[string]any); ok {
			ref, _ = p["ref"].(string)
		}
		return r.status, tok, ref
	}
	status, aliceTok, aliceRef := post("alice", "alice")
	if status != http.StatusAccepted {
		t.Fatalf("alice's plan: %d", status)
	}
	<-entered
	f.srv.StartDriftLook(time.Hour)
	for _, st := range f.drift(f.token(nil), "").Stacks {
		if st.Checking {
			t.Errorf("a look with one plan slot free started a drift check of %s@%s, which takes the last slot", st.Stack, st.Env)
		}
	}
	status, carolTok, carolRef := post("carol", "carol")
	if status != http.StatusAccepted {
		t.Fatalf("carol's plan while alice's runs, after a drift look: %d, want 202", status)
	}
	<-entered
	close(release)
	released = true
	f.waitPlan(aliceTok, aliceRef)
	f.waitPlan(carolTok, carolRef)
	f.srv.WaitDrift()
}

// tofuRotated is the value app's sensitive variable has after it changed
// from tofuCanary.
const tofuRotated = "canary-rotated-var-3a7c"

// rotatedPlanJSON is `tofu show -json` of a plan of app after its
// sensitive variable changed from tofuCanary to tofuRotated, as OpenTofu
// 1.13.9 writes it for a resource keyed by nonsensitive(var.canary): the
// delete of the old key holds tofuCanary in its address with no mark, and
// nothing in the plan names tofuCanary as a secret.
const rotatedPlanJSON = `{"format_version":"1.2","terraform_version":"1.13.9",
 "variables":{"canary":{"value":"` + tofuRotated + `"}},
 "configuration":{"provider_config":{"terraform":{"name":"terraform","full_name":"terraform.io/builtin/terraform"}},
  "root_module":{"variables":{"canary":{"sensitive":true}}}},
 "resource_changes":[
  {"address":"terraform_data.keyed[\"` + tofuRotated + `\"]","mode":"managed","type":"terraform_data","name":"keyed","index":"` + tofuRotated + `","provider_name":"terraform.io/builtin/terraform",
   "change":{"actions":["create"],"before":null,"after":{"input":true},"after_unknown":{"id":true},"before_sensitive":false,"after_sensitive":{}}},
  {"address":"terraform_data.keyed[\"` + tofuCanary + `\"]","mode":"managed","type":"terraform_data","name":"keyed","index":"` + tofuCanary + `","provider_name":"terraform.io/builtin/terraform",
   "change":{"actions":["delete"],"before":{"id":"u-1","input":true},"after":null,"before_sensitive":{},"after_sensitive":false},"action_reason":"delete_because_each_key"}
 ]}`

// keyedChanges is the addresses and actions of the resource changes of a
// masked plan document.
func keyedChanges(t *testing.T, doc json.RawMessage) []string {
	t.Helper()
	var d struct {
		ResourceChanges []struct {
			Address string
			Change  struct{ Actions []string }
		} `json:"resource_changes"`
	}
	if err := json.Unmarshal(doc, &d); err != nil {
		t.Fatalf("the plan document: %v\n%s", err, doc)
	}
	var out []string
	for _, c := range d.ResourceChanges {
		out = append(out, c.Address+" "+strings.Join(c.Change.Actions, ","))
	}
	return out
}

// A state keeps a past secret where nothing marks it: app's apply sealed
// tofuCanary with state version 1, then the sensitive variable changed,
// and the delete of the resource keyed by nonsensitive(var.canary) holds
// tofuCanary in its address. A person's plan and a drift check each mask
// it with the secret set of the version they read, so neither the API,
// the control database, nor the data directory holds it.
func TestTofuPlanMasksPastSecrets(t *testing.T) {
	runner := &fakeTofu{}
	f := driftFixture(t, runner)
	runner.mu.Lock()
	runner.plan = func() (tofu.Plan, error) {
		return tofu.Plan{File: []byte("saved rotated plan"), JSON: []byte(rotatedPlanJSON)}, nil
	}
	runner.mu.Unlock()
	want := []string{`terraform_data.keyed["(sensitive)"] create`, `terraform_data.keyed["(sensitive)"] delete`}
	p, _ := f.planCommit(tofuFiles(), "rotate")
	body, _ := json.Marshal(p)
	if got := keyedChanges(t, p.Document); p.Plan.Status != "pending" || !slices.Equal(got, want) || strings.Contains(string(body), tofuCanary) {
		t.Fatalf("the plan after the variable changed: %s, changes %q, want %q\n%s", p.Plan.Status, got, want, body)
	}

	f.srv.LookForDrift(time.Hour)
	r := f.do("GET", "/v1/drift?env=prod&stack=app", f.token(nil), nil)
	var doc server.DriftDoc
	if err := json.Unmarshal(r.body, &doc); err != nil || r.status != http.StatusOK {
		t.Fatalf("GET /v1/drift: %d %v %s", r.status, err, r.body)
	}
	c := checkOf(t, doc, "app")
	var got []string
	for _, ch := range c.Changes {
		got = append(got, ch.Address+" "+ch.Action)
	}
	slices.Sort(got)
	if c.Status != "drifted" || c.Code != 2 || !slices.Equal(got, want) || strings.Contains(string(r.body), tofuCanary) {
		t.Fatalf("the drift check after the variable changed: %+v, changes %q, want %q\n%s", c, got, want, r.body)
	}
	scanForCanaries(t, f, tofuCanary, tofuRotated, tofuEnvSecret)
}

// A state version with no secret set, as one written before writ serve
// kept them, may keep a past secret that no set names. A plan against it
// still runs, but keeps no detail: it fails and says why, so no one can
// approve it. A drift check finds drift unknown and keeps nothing else.
func TestTofuPlanWithoutSecretSet(t *testing.T) {
	runner := &fakeTofu{}
	f := driftFixture(t, runner)
	if _, err := controltest.Raw(t, f.loc).ExecContext(context.Background(), "UPDATE tfstate_versions SET index_secrets = NULL"); err != nil {
		t.Fatal(err)
	}
	runner.mu.Lock()
	runner.plan = func() (tofu.Plan, error) {
		return tofu.Plan{File: []byte("saved rotated plan"), JSON: []byte(rotatedPlanJSON)}, nil
	}
	runner.mu.Unlock()
	n := runner.plans()
	p, _ := f.planCommit(tofuFiles(), "rotate")
	why := "no secret set for state version 1 of app@prod, so writ serve keeps no detail of a plan against it and never approves one"
	if runner.plans() != n+1 || p.Plan.Status != "failed" || p.Plan.ID != "" || p.OK || !strings.Contains(p.Text, why) || len(keyedChanges(t, p.Document)) != 0 {
		t.Fatalf("a plan against a version with no secret set: ran %d plans, %+v\n%s\n%s", runner.plans()-n, p.Plan, p.Text, p.Document)
	}
	if r := f.do("POST", "/v1/plans/"+p.Plan.Ref+"/approvals", bob(f), nil); r.status == http.StatusOK {
		t.Fatalf("bob approved a plan against a version with no secret set: %s", r.body)
	}

	f.srv.LookForDrift(time.Hour)
	c := checkOf(t, f.drift(f.token(nil), "?env=prod&stack=app"), "app")
	if c.Status != "failed" || c.Error != "drift unknown: no secret set for state version 1 of app@prod" || c.Plan != "" || len(c.Changes) != 0 || c.Outside+c.Code != 0 {
		t.Fatalf("a drift check against a version with no secret set: %+v", c)
	}
	scanForCanaries(t, f, tofuCanary, tofuRotated, tofuEnvSecret)
}

// A plan reads the state version that is newest when it ends. An apply
// that writes a version while a plan runs may seal with it a secret the
// plan's mask never learned, so the plan keeps nothing of tofu's output,
// a refused plan's diagnostics included, and fails: plan again.
func TestTofuPlanWhileStateMoves(t *testing.T) {
	refused := func() (tofu.Plan, error) {
		out := []byte(`{"type":"diagnostic","diagnostic":{"severity":"error","summary":"Cannot delete terraform_data.keyed[\"` + tofuCanary + `\"]"}}` + "\n")
		return tofu.Plan{}, &tofu.StepError{Step: "plan", Code: 1, Stdout: out}
	}
	planned := func() (tofu.Plan, error) {
		return tofu.Plan{File: []byte("saved plan"), JSON: []byte(tofuPlanJSON)}, nil
	}
	for _, tc := range []struct {
		name string
		plan func() (tofu.Plan, error)
	}{
		{"a plan tofu refused", refused},
		{"a plan tofu made", planned},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeTofu{}
			f := tofuApplyFixture(t, runner)
			id := approvedTofuPlan(t, f, "first")
			runner.mu.Lock()
			runner.plan = func() (tofu.Plan, error) {
				if run := f.tofuRun(bob(f), map[string]any{"plan": id}); run.Run.Status != "applied" {
					t.Errorf("the run inside the plan: %+v\n%s", run.Run, run.Text)
				}
				return tc.plan()
			}
			runner.mu.Unlock()
			p, _ := f.planCommit(tofuFiles(), "overlap")
			why := "the state of app@prod moved from version 0 to 1 while the plan ran"
			if p.Plan.Status != "failed" || p.Plan.ID != "" || !strings.Contains(p.Text, why) || strings.Contains(p.Text, "Cannot delete") || len(keyedChanges(t, p.Document)) != 0 {
				t.Fatalf("a plan while the state moved: %+v\n%s\n%s", p.Plan, p.Text, p.Document)
			}
			scanForCanaries(t, f, tofuCanary, tofuEnvSecret)
		})
	}
}

// GET /v1/drift/{id} returns one drift check by its ref or plan id, with
// its changes or its error, and enforces read on the check's stack@env.
// GET /v1/drift?env=&stack= lists the stack's kept checks, and GET
// /v1/overview names the check on drift alerts.
func TestGetDriftCheck(t *testing.T) {
	runner := &fakeTofu{}
	runner.plan = driftPlan(true)
	f := driftFixture(t, runner)
	f.srv.LookForDrift(time.Hour)

	tok := f.token(nil)
	listed := f.drift(tok, "?env=prod&stack=app")
	if len(listed.Stacks) != 1 || listed.Stacks[0].Check == nil {
		t.Fatalf("GET /v1/drift: %+v", listed)
	}
	c := listed.Stacks[0].Check
	if c.Ref == "" || c.Plan == "" {
		t.Fatalf("check lacks ref or plan: %+v", c)
	}
	if len(listed.Checks) != 1 || listed.Checks[0].Ref != c.Ref || listed.Checks[0].Plan != c.Plan || listed.Checks[0].Code != c.Code {
		t.Fatalf("kept checks: %+v", listed.Checks)
	}
	if all := f.drift(tok, "?env=prod"); all.Checks != nil {
		t.Fatalf("GET /v1/drift of an env lists kept checks: %+v", all.Checks)
	}

	for _, id := range []string{c.Ref, c.Plan} {
		r := f.do("GET", "/v1/drift/"+id, tok, nil)
		var doc server.DriftCheckDoc
		if r.status != http.StatusOK || json.Unmarshal(r.body, &doc) != nil {
			t.Fatalf("GET /v1/drift/%s: %d %s", id, r.status, r.body)
		}
		if doc.Format != server.FormatDriftCheck || !doc.OK || doc.Env != "prod" || doc.Stack != "app" || doc.Backend != "http" || doc.Every != "1h" ||
			doc.Check == nil || doc.Check.Ref != c.Ref || doc.Check.Plan != c.Plan || doc.Check.Status != server.DriftDrifted || doc.Check.StartedBy != server.StartedByDrift || len(doc.Check.Changes) != 3 {
			t.Fatalf("GET /v1/drift/%s: %+v (check %+v)", id, doc, doc.Check)
		}
		for _, secret := range []string{tofuCanary, tofuOutside, tofuEnvSecret} {
			if strings.Contains(string(r.body), secret) {
				t.Errorf("GET /v1/drift/%s holds %s", id, secret)
			}
		}
	}

	// web_readers may not read app@prod.
	webTok := f.token(map[string]any{"project_id": "4343"})
	if r := f.do("GET", "/v1/drift/"+c.Ref, webTok, nil); r.status != http.StatusForbidden || r.class() != "denied" {
		t.Fatalf("web_readers reading app's drift check: %d %s", r.status, r.body)
	}
	if d := f.drift(webTok, "?env=prod&stack=app"); len(d.Stacks) != 0 || d.Checks != nil {
		t.Fatalf("web_readers listing app's kept checks: %+v", d)
	}
	if r := f.do("GET", "/v1/drift/"+strings.Repeat("0", 32), tok, nil); r.status != http.StatusNotFound {
		t.Fatalf("unknown drift check: %d %s", r.status, r.body)
	}

	// Overview alerts name the drift check's ref.
	ov := f.do("GET", "/v1/overview", tok, nil)
	var ovDoc server.OverviewDoc
	if ov.status != http.StatusOK || json.Unmarshal(ov.body, &ovDoc) != nil {
		t.Fatalf("GET /v1/overview: %d %s", ov.status, ov.body)
	}
	if !slices.ContainsFunc(ovDoc.Alerts, func(a server.Alert) bool { return a.Kind == server.AlertDrifted && a.Drift == c.Ref }) {
		t.Fatalf("overview alerts lack drift ref %s: %+v", c.Ref, ovDoc.Alerts)
	}

	// A failed check is readable by ref with its diagnostic.
	runner.mu.Lock()
	runner.plan = func() (tofu.Plan, error) {
		return tofu.Plan{}, errors.New("provider timed out")
	}
	runner.mu.Unlock()
	time.Sleep(5 * time.Millisecond)
	f.srv.LookForDrift(time.Millisecond) // checks web first
	time.Sleep(5 * time.Millisecond)
	f.srv.LookForDrift(time.Millisecond) // checks app
	listed = f.drift(tok, "?env=prod&stack=app")
	failed := listed.Stacks[0].Check
	if failed == nil || failed.Status != server.DriftFailed || failed.Ref == "" || failed.Plan != "" {
		t.Fatalf("failed check: %+v", failed)
	}
	if len(listed.Checks) != 2 || listed.Checks[0].Ref != failed.Ref || listed.Checks[1].Ref != c.Ref {
		t.Fatalf("kept checks after failure: %+v", listed.Checks)
	}
	r := f.do("GET", "/v1/drift/"+failed.Ref, tok, nil)
	var fdoc server.DriftCheckDoc
	if r.status != http.StatusOK || json.Unmarshal(r.body, &fdoc) != nil || fdoc.OK || fdoc.Check == nil || fdoc.Check.Status != server.DriftFailed || !strings.Contains(fdoc.Check.Error, "provider timed out") {
		t.Fatalf("failed drift check doc: %d %s", r.status, r.body)
	}
}
