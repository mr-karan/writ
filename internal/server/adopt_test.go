package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/control/controltest"
	"github.com/mr-karan/writ/internal/oidc/oidctest"
	"github.com/mr-karan/writ/internal/server"
	"github.com/mr-karan/writ/internal/tofu"
)

// Canaries of a state that no apply of writ serve sealed a secret set
// with: one the state marks sensitive, and one it holds where nothing
// marks it, in an address keyed by nonsensitive() of a variable.
const (
	adoptMarked   = "canary-adopt-marked-6e1d"
	adoptUnmarked = "canary-adopt-unmarked-90b3"
)

// adoptStateJSON is `tofu show -json` of such a state: adoptMarked in a
// marked attribute and in an address, and adoptUnmarked in an address.
const adoptStateJSON = `{"format_version":"1.0","terraform_version":"1.13.9","values":{"root_module":{"resources":[
 {"address":"terraform_data.secret","mode":"managed","type":"terraform_data","name":"secret","provider_name":"terraform.io/builtin/terraform",
  "values":{"id":"d-1","input":"` + adoptMarked + `"},"sensitive_values":{"input":true}},
 {"address":"terraform_data.keyed[\"` + adoptMarked + `\"]","mode":"managed","type":"terraform_data","name":"keyed","index":"` + adoptMarked + `","provider_name":"terraform.io/builtin/terraform",
  "values":{"id":"d-2"},"sensitive_values":{}},
 {"address":"terraform_data.loose[\"` + adoptUnmarked + `\"]","mode":"managed","type":"terraform_data","name":"loose","index":"` + adoptUnmarked + `","provider_name":"terraform.io/builtin/terraform",
  "values":{"id":"d-3"},"sensitive_values":{}}
]}}}`

// adoptPlanJSON is `tofu show -json` of a plan against that state after
// both keys changed: each delete names its old key in its address under
// no mark, and nothing in the plan names either as a secret.
const adoptPlanJSON = `{"format_version":"1.2","terraform_version":"1.13.9",
 "configuration":{"provider_config":{"terraform":{"name":"terraform","full_name":"terraform.io/builtin/terraform"}},"root_module":{}},
 "resource_changes":[
  {"address":"terraform_data.keyed[\"` + adoptMarked + `\"]","mode":"managed","type":"terraform_data","name":"keyed","index":"` + adoptMarked + `","provider_name":"terraform.io/builtin/terraform",
   "change":{"actions":["delete"],"before":{"id":"d-2"},"after":null,"before_sensitive":{},"after_sensitive":false},"action_reason":"delete_because_each_key"},
  {"address":"terraform_data.loose[\"` + adoptUnmarked + `\"]","mode":"managed","type":"terraform_data","name":"loose","index":"` + adoptUnmarked + `","provider_name":"terraform.io/builtin/terraform",
   "change":{"actions":["delete"],"before":{"id":"d-3"},"after":null,"before_sensitive":{},"after_sensitive":false},"action_reason":"delete_because_each_key"}
 ]}`

// adoptPeople is a people issuer and its teams: stewards may adopt app,
// web_stewards may adopt only web, and app_readers may only read app.
func adoptPeople(t *testing.T) func(string) string {
	people := oidctest.New(t)
	return func(text string) string {
		return text + fmt.Sprintf(`
issuer "sso" {
  kind = "people"
  identity = "gitlab"
  url = %q
  client_id = "writ-client"
  client_secret_file = "synthetic-secret"
}

team "stewards" {
  issuer = "sso"
  match = { groups = ["acme/stewards"] }
}

team "web_stewards" {
  issuer = "sso"
  match = { groups = ["acme/web"] }
}

team "app_readers" {
  issuer = "sso"
  match = { groups = ["acme"] }
}

grant {
  teams = ["stewards"]
  stacks = ["app"]
  envs = ["prod"]
  can = ["read", "adopt"]
}

grant {
  teams = ["web_stewards"]
  stacks = ["web"]
  envs = ["prod"]
  can = ["read", "adopt"]
}

grant {
  teams = ["app_readers"]
  stacks = ["app"]
  envs = ["prod"]
  can = ["read"]
}
`, people.URL)
	}
}

const adoptReason = "state from before writ, OPS-311"

// adopt sends POST /v1/state/adopt with body, edited from an adoption of
// version 1 of app@prod, as carol of teams through the control UI's
// session, or as tok when it is not "".
func adopt(f *fixture, tok string, teams []string, edit map[string]any) response {
	f.t.Helper()
	body := map[string]any{"format": server.FormatStateAdoptRequest, "env": "prod", "stack": "app", "version": 1, "reason": adoptReason}
	for k, v := range edit {
		if v == nil {
			delete(body, k)
		} else {
			body[k] = v
		}
	}
	b, _ := json.Marshal(body)
	if tok != "" {
		return f.do("POST", "/v1/state/adopt", tok, strings.NewReader(string(b)))
	}
	sess := &server.Session{ID: "carol-session", Display: "carol", Person: "gitlab/user/3003", Issuer: "sso", Subject: "3003", Teams: teams, EndsAt: time.Now().Add(time.Hour)}
	status, out := f.srv.Post(httptest.NewRequest("POST", "/ui/", nil), sess, "/v1/state/adopt", b)
	r := response{status: status, body: out}
	_ = json.Unmarshal(out, &r.doc)
	return r
}

// adoptions are the audit rows of carol's adoptions.
func adoptions(f *fixture) []control.Entry {
	var out []control.Entry
	for _, e := range f.audit(controltest.AuditFilter{Person: "gitlab/user/3003"}) {
		if e.Kind == "state-adopt" {
			out = append(out, e)
		}
	}
	return out
}

// A state version with no secret set, as one written before writ serve
// kept them, can be approved again only after a person adopts it. A CI
// job, a person with no adopt on the stack, and a request that names a
// state that is not the newest are refused before tofu runs. carol, of
// stewards, adopts version 1: a run shows the stored state as a file
// with no credential, no state token, and no provider_env, the mask
// learns the value the state marks sensitive, and writ serve seals it
// with the version, writes one state-adopt audit row, and indexes the
// state. The drift check that the adoption makes due runs, and a plan is
// approvable and applies. The value the state marks is swept from its
// address in the plan, and the value it holds where nothing marks it is
// not learned and shows, as the docs say.
func TestAdoptState(t *testing.T) {
	runner := &fakeTofu{}
	f := tofuApplyFixtureEdit(t, runner, adoptPeople(t))
	appliedApp(t, f, "first")
	if _, err := controltest.Raw(t, f.loc).ExecContext(context.Background(), "UPDATE tfstate_versions SET index_secrets = NULL"); err != nil {
		t.Fatal(err)
	}
	runner.mu.Lock()
	runner.state = adoptStateJSON
	runner.plan = func() (tofu.Plan, error) {
		return tofu.Plan{File: []byte("saved adopt plan"), JSON: []byte(adoptPlanJSON)}, nil
	}
	runner.mu.Unlock()
	p, _ := f.planCommit(tofuFiles(), "before")
	if p.Plan.Status != "failed" || !strings.Contains(p.Text, "no secret set for state version 1 of app@prod") || !strings.Contains(p.Text, "writ state adopt") {
		t.Fatalf("a plan before the adoption: %+v\n%s", p.Plan, p.Text)
	}
	f.srv.LookForDrift(time.Hour)
	if c := checkOf(t, f.drift(f.token(nil), "?env=prod&stack=app"), "app"); c.Status != "failed" || c.Error != "drift unknown: no secret set for state version 1 of app@prod" {
		t.Fatalf("the drift check before the adoption: %+v", c)
	}

	stewards := []string{"stewards"}
	for _, tc := range []struct {
		name   string
		tok    string
		teams  []string
		edit   map[string]any
		status int
		want   string
	}{
		{"a CI job", bob(f), nil, nil, http.StatusForbidden, "a CI job cannot adopt a state"},
		{"no adopt grant", "", []string{"app_readers"}, nil, http.StatusForbidden, "carol may not adopt app@prod: no grant gives the teams app_readers adopt on app@prod"},
		{"adopt on another stack", "", []string{"web_stewards"}, nil, http.StatusForbidden, "no grant gives the teams web_stewards adopt on app@prod"},
		{"a version that is not the newest", "", stewards, map[string]any{"version": 2}, http.StatusConflict, "state version 2 of app@prod is not its newest, version 1"},
		{"a serial of a stack on the http backend", "", stewards, map[string]any{"version": nil, "serial": 1}, http.StatusNotFound, "has read no serial of the state of app@prod on S3"},
		{"both a version and a serial", "", stewards, map[string]any{"serial": 1}, http.StatusBadRequest, "an adoption names one state"},
		{"a short reason", "", stewards, map[string]any{"reason": "old"}, http.StatusBadRequest, "reason needs 10 to 500 characters"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := len(runner.shown)
			r := adopt(f, tc.tok, tc.teams, tc.edit)
			if r.status != tc.status || !strings.Contains(string(r.body), tc.want) {
				t.Fatalf("got %d %s, want %d %q", r.status, r.body, tc.status, tc.want)
			}
			if len(runner.shown) != n || len(adoptions(f)) != 0 {
				t.Fatalf("a refused adoption ran tofu %d times and wrote %d adoption rows", len(runner.shown)-n, len(adoptions(f)))
			}
		})
	}

	r := adopt(f, "", stewards, nil)
	var doc server.StateAdoptDoc
	if err := json.Unmarshal(r.body, &doc); r.status != http.StatusOK || err != nil {
		t.Fatalf("carol's adoption: %d %v %s", r.status, err, r.body)
	}
	if doc.Format != server.FormatStateAdopt || !doc.OK || doc.Backend != "http" || doc.Version != 1 || doc.Serial != 1 || doc.Lineage != "lineage-1" ||
		doc.Repo != "infra" || doc.Resources != 3 || !doc.Indexed || len(doc.Diagnostics) != 1 || doc.Diagnostics[0].Message != server.AdoptLimit {
		t.Fatalf("the adoption's answer: %s", r.body)
	}
	runner.mu.Lock()
	shown := runner.shown[len(runner.shown)-1]
	runner.mu.Unlock()
	if !slices.Equal(shown.stack.Env, []string{"PATH=/usr/local/bin:/usr/bin:/bin", "AWS_EC2_METADATA_DISABLED=true"}) || len(shown.stack.Creds) != 0 || shown.stack.StateCreds ||
		shown.stack.User == nil || string(shown.state) != `{"version":4,"serial":1,"lineage":"lineage-1","resources":[]}` {
		t.Fatalf("the adoption's show: env %q, creds %q, state creds %t, user %v, state %s", shown.stack.Env, shown.stack.Creds, shown.stack.StateCreds, shown.stack.User, shown.state)
	}
	rows := adoptions(f)
	if len(rows) != 1 {
		t.Fatalf("%d adoption rows, want 1", len(rows))
	}
	if e := rows[0]; e.Op != "adopt" || e.Env != "prod" || !slices.Equal(e.Stacks, []string{"app"}) || e.Decision != control.Allowed || e.Status != http.StatusOK ||
		e.Principal != "carol" || e.Reason != "adopted state version 1 of app@prod, serial 1 of lineage lineage-1: "+adoptReason {
		t.Fatalf("the adoption's audit row: %+v", e)
	}
	if r := adopt(f, "", stewards, nil); r.status != http.StatusConflict || !strings.Contains(string(r.body), "state version 1 of app@prod has a secret set already") {
		t.Fatalf("a second adoption: %d %s", r.status, r.body)
	}

	// The adoption makes a drift check due, though one ran within
	// drift.every, and it now keeps what the plan changes.
	time.Sleep(5 * time.Millisecond)
	f.srv.LookForDrift(time.Hour)
	if c := checkOf(t, f.drift(f.token(nil), "?env=prod&stack=app"), "app"); c.Status != "drifted" || c.Code != 2 {
		t.Fatalf("the drift check after the adoption: %+v", c)
	}

	want := []string{`terraform_data.keyed["(sensitive)"] delete`, `terraform_data.loose["` + adoptUnmarked + `"] delete`}
	p, _ = f.planCommit(tofuFiles(), "after")
	if got := keyedChanges(t, p.Document); p.Plan.Status != "pending" || !slices.Equal(got, want) {
		t.Fatalf("a plan after the adoption: %s, changes %q, want %q\n%s", p.Plan.Status, got, want, p.Text)
	}
	if r := f.do("POST", "/v1/plans/"+p.Plan.ID+"/approvals", bob(f), nil); r.status != http.StatusOK {
		t.Fatalf("bob's approval: %d %s", r.status, r.body)
	}
	if run := f.tofuRun(bob(f), map[string]any{"plan": p.Plan.ID}); run.Run.Status != "applied" {
		t.Fatalf("the apply after the adoption: %+v\n%s", run.Run, run.Text)
	}
	scanForCanaries(t, f, adoptMarked, tofuCanary, tofuEnvSecret)
}

// A stack on S3 whose bucket held a state before writ serve first planned
// it has no secret set at the serial writ serve read first. carol adopts
// that serial: a run pulls the state with the plan role, and the pulled
// serial must be the one she named. A serial that is not the newest writ
// serve read is refused. Then a plan is approvable and applies.
func TestAdoptStateS3(t *testing.T) {
	runner := &fakeTofu{
		plan: func() (tofu.Plan, error) {
			return tofu.Plan{File: savedPlan(t, "L", 5), JSON: []byte(adoptPlanJSON)}, nil
		},
		pull: func() (tofu.State, error) {
			return tofu.State{Serial: tofu.Serial{Lineage: "L", Serial: 5}, Unmasked: []byte(adoptStateJSON)}, nil
		},
		applyS3: func() (tofu.Applied, error) {
			return tofu.Applied{Plan: []byte(adoptPlanJSON), State: tofu.State{Serial: tofu.Serial{Lineage: "L", Serial: 6}, Unmasked: []byte(adoptStateJSON)}}, nil
		},
	}
	f := tofuS3Fixture(t, runner, adoptPeople(t))
	f.gitlab.Commit(4242, "main", tofuS3Files())
	f.srv.Poll()
	p, _ := f.planCommit(tofuS3Files(), "before")
	if p.Plan.Status != "failed" || !strings.Contains(p.Text, "no secret set for state version 5 of app@prod") {
		t.Fatalf("a plan of the state the bucket held: %+v\n%s", p.Plan, p.Text)
	}
	stewards := []string{"stewards"}
	if r := adopt(f, "", stewards, map[string]any{"version": nil, "serial": 4}); r.status != http.StatusConflict ||
		!strings.Contains(string(r.body), "serial 4 of the state of app@prod is not the newest that writ serve read, serial 5") {
		t.Fatalf("an adoption of an older serial: %d %s", r.status, r.body)
	}
	if r := adopt(f, "", stewards, map[string]any{"version": 5}); r.status != http.StatusNotFound || !strings.Contains(string(r.body), "keeps no state version of app@prod on the http backend") {
		t.Fatalf("an adoption by version of a stack on S3: %d %s", r.status, r.body)
	}
	if len(runner.pulled) != 0 {
		t.Fatalf("refused adoptions pulled the state %d times", len(runner.pulled))
	}

	r := adopt(f, "", stewards, map[string]any{"version": nil, "serial": 5})
	var doc server.StateAdoptDoc
	if err := json.Unmarshal(r.body, &doc); r.status != http.StatusOK || err != nil || doc.Backend != "s3" || doc.Version != 0 || doc.Serial != 5 || doc.Lineage != "L" || !doc.Indexed || doc.Resources != 3 {
		t.Fatalf("carol's adoption of serial 5: %d %v %s", r.status, err, r.body)
	}
	runner.mu.Lock()
	pulled := runner.pulled
	runner.mu.Unlock()
	if len(pulled) != 1 || !pulled[0].StateCreds || envValue(pulled[0].Creds, "AWS_CONTAINER_CREDENTIALS_FULL_URI") == "" || pulled[0].User == nil {
		t.Fatalf("the adoption's pull: %+v", pulled)
	}
	var n int
	var first, last string
	if err := controltest.Raw(t, f.loc).QueryRowContext(context.Background(), "SELECT count(*), min(role), max(role) FROM role_sessions WHERE session LIKE '%adopt-%'").Scan(&n, &first, &last); err != nil ||
		n != 1 || first != "arn:aws:iam::222222222222:role/writ-plan" || last != first {
		t.Fatalf("the adoption assumed %d roles, %q to %q, want the plan role alone: %v", n, first, last, err)
	}
	if rows := adoptions(f); len(rows) != 1 || rows[0].Reason != "adopted serial 5 of lineage L of the state of app@prod on S3: "+adoptReason {
		t.Fatalf("the adoption's audit rows: %+v", rows)
	}

	want := []string{`terraform_data.keyed["(sensitive)"] delete`, `terraform_data.loose["` + adoptUnmarked + `"] delete`}
	p, _ = f.planCommit(tofuS3Files(), "after")
	if got := keyedChanges(t, p.Document); p.Plan.Status != "pending" || !slices.Equal(got, want) {
		t.Fatalf("a plan after the adoption: %s, changes %q, want %q\n%s", p.Plan.Status, got, want, p.Text)
	}
	if r := f.do("POST", "/v1/plans/"+p.Plan.ID+"/approvals", bob(f), nil); r.status != http.StatusOK {
		t.Fatalf("bob's approval: %d %s", r.status, r.body)
	}
	if run := f.tofuRun(bob(f), map[string]any{"plan": p.Plan.ID}); run.Run.Status != "applied" {
		t.Fatalf("the apply after the adoption: %+v\n%s", run.Run, run.Text)
	}
	scanForCanaries(t, f, adoptMarked, tofuEnvSecret)
}

// startHeldApply starts an apply of plan id whose run's start, once
// committed, holds until the test ends, so the run stays applying. It
// returns a channel closed at that commit.
func startHeldApply(t *testing.T, f *fixture, id string) <-chan struct{} {
	t.Helper()
	committed, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	f.srv.HoldCommit(func(commit func() error) error {
		if err := commit(); err != nil {
			return err
		}
		close(committed)
		<-release
		return nil
	})
	go func() {
		defer close(done)
		f.do("POST", "/v1/runs", bob(f), runRequest(map[string]any{"plan": id}))
	}()
	t.Cleanup(func() {
		close(release)
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("the held apply did not end")
		}
	})
	return committed
}

// adoptedSet reports whether version 1 of app@prod has a secret set.
func adoptedSet(t *testing.T, f *fixture) bool {
	t.Helper()
	var has bool
	if err := controltest.Raw(t, f.loc).QueryRowContext(context.Background(),
		"SELECT index_secrets IS NOT NULL FROM tfstate_versions WHERE env = 'prod' AND stack = 'app' AND version = 1").Scan(&has); err != nil {
		t.Fatal(err)
	}
	return has
}

// An adoption while an apply of the stack@env runs is refused before
// tofu runs: the apply may write a newer state.
func TestAdoptDuringApply(t *testing.T) {
	runner := &fakeTofu{}
	f := tofuApplyFixtureEdit(t, runner, adoptPeople(t))
	appliedApp(t, f, "first")
	id := approvedTofuPlan(t, f, "next")
	if _, err := controltest.Raw(t, f.loc).ExecContext(context.Background(), "UPDATE tfstate_versions SET index_secrets = NULL"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-startHeldApply(t, f, id):
	case <-time.After(30 * time.Second):
		t.Fatal("the apply did not start")
	}
	runner.mu.Lock()
	shown := len(runner.shown)
	runner.mu.Unlock()
	r := adopt(f, "", []string{"stewards"}, nil)
	if r.status != http.StatusConflict || !strings.Contains(string(r.body), "an apply of app@prod runs") {
		t.Fatalf("an adoption during an apply: %d %s", r.status, r.body)
	}
	runner.mu.Lock()
	shown = len(runner.shown) - shown
	runner.mu.Unlock()
	if shown != 0 || len(adoptions(f)) != 0 || adoptedSet(t, f) {
		t.Fatalf("the refused adoption ran show %d times, wrote %d adoption rows, sealed a set: %t", shown, len(adoptions(f)), adoptedSet(t, f))
	}
}

// An apply that starts while an adoption's transaction holds the
// stack@env's turn waits for it to commit, so an adoption never commits
// after an apply of its stack@env began. The adoption is held after it
// counted the runs, at its write of the set.
func TestAdoptRacesApply(t *testing.T) {
	if controltest.Default().Name != "postgres" {
		t.Skip("SQLite's write lock runs the two transactions one after the other")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	runner := &fakeTofu{}
	f := tofuApplyFixtureEdit(t, runner, adoptPeople(t))
	appliedApp(t, f, "first")
	id := approvedTofuPlan(t, f, "next")
	db := controltest.Raw(t, f.loc)
	for _, q := range []string{
		"UPDATE tfstate_versions SET index_secrets = NULL",
		`CREATE FUNCTION hold_adopt() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(1919); RETURN NEW; END $$`,
		`CREATE TRIGGER hold_adopt BEFORE UPDATE OF index_secrets ON tfstate_versions FOR EACH ROW EXECUTE FUNCTION hold_adopt()`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	hold, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback()
	if _, err := hold.ExecContext(ctx, "SELECT pg_advisory_xact_lock(1919)"); err != nil {
		t.Fatal(err)
	}
	waiting := func(event string) bool {
		var n int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock' AND (wait_event = 'advisory') = ?",
			event == "advisory").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n > 0
	}
	adopted := make(chan response, 1)
	go func() { adopted <- adopt(f, "", []string{"stewards"}, nil) }()
	for !waiting("advisory") {
		select {
		case <-time.After(10 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal("the adoption did not reach its write of the set")
		}
	}
	committed := startHeldApply(t, f, id)
	for !waiting("row") {
		select {
		case <-committed:
			t.Fatal("an apply began while an adoption of its stack@env held its turn")
		case <-time.After(10 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal("the apply neither began nor waited")
		}
	}
	if err := hold.Commit(); err != nil {
		t.Fatal(err)
	}
	if r := <-adopted; r.status != http.StatusOK {
		t.Fatalf("the adoption the apply waited for: %d %s", r.status, r.body)
	}
	select {
	case <-committed:
	case <-ctx.Done():
		t.Fatal("the apply did not begin after the adoption")
	}
	if !adoptedSet(t, f) || len(adoptions(f)) != 1 {
		t.Fatalf("after the adoption: set %t, %d adoption rows", adoptedSet(t, f), len(adoptions(f)))
	}
}

// The secret set, the audit row, and the rows of the state index of an
// adoption commit together: when the index's write fails, none is kept,
// the answer is an error that only the refusal's audit row records, and
// the adoption can run again.
func TestAdoptIndexFails(t *testing.T) {
	ctx := context.Background()
	runner := &fakeTofu{}
	f := tofuApplyFixtureEdit(t, runner, adoptPeople(t))
	appliedApp(t, f, "first")
	db := controltest.Raw(t, f.loc)
	runner.mu.Lock()
	runner.state = adoptStateJSON
	runner.mu.Unlock()
	fail := `CREATE TRIGGER fail_index BEFORE INSERT ON state_index BEGIN SELECT RAISE(FAIL, 'index write failed'); END`
	if controltest.Default().Name == "postgres" {
		if _, err := db.ExecContext(ctx, `CREATE FUNCTION fail_index() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'index write failed'; END $$`); err != nil {
			t.Fatal(err)
		}
		fail = `CREATE TRIGGER fail_index BEFORE INSERT ON state_index FOR EACH ROW EXECUTE FUNCTION fail_index()`
	}
	for _, q := range []string{"UPDATE tfstate_versions SET index_secrets = NULL", fail} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if r := adopt(f, "", []string{"stewards"}, nil); r.status != http.StatusServiceUnavailable || !strings.Contains(string(r.body), "the control database failed") {
		t.Fatalf("an adoption whose index write fails: %d %s", r.status, r.body)
	}
	if rows := adoptions(f); adoptedSet(t, f) || len(rows) != 1 || rows[0].Status != http.StatusServiceUnavailable {
		t.Fatalf("a failed adoption kept its set: %t, or wrote audit rows other than its refusal: %+v", adoptedSet(t, f), rows)
	}
	drop := "DROP TRIGGER fail_index"
	if controltest.Default().Name == "postgres" {
		drop += " ON state_index"
	}
	if _, err := db.ExecContext(ctx, drop); err != nil {
		t.Fatal(err)
	}
	r := adopt(f, "", []string{"stewards"}, nil)
	if r.status != http.StatusOK || r.doc["indexed"] != true || r.doc["resources"] != float64(3) || !adoptedSet(t, f) || len(adoptions(f)) != 2 {
		t.Fatalf("the adoption again: %d %s, set %t, %d adoption rows", r.status, r.body, adoptedSet(t, f), len(adoptions(f)))
	}
	if got := indexDump(t, f); !strings.Contains(got, `terraform_data.loose["`+adoptUnmarked+`"]`) {
		t.Fatalf("the index after the adoption:\n%s", got)
	}
}
