package server_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/control/controltest"
	"github.com/mr-karan/writ/internal/server"
	"github.com/mr-karan/writ/internal/sqldb"
	"github.com/mr-karan/writ/internal/tofu"
)

// tofuApplyFixture is tofuFixture with withApprovals' rules: a change to
// prod needs one approval, by a maintainer, and maintainers may apply
// every risk class of app. limits are lines of the limits block.
func tofuApplyFixture(t *testing.T, runner *fakeTofu, limits ...string) *fixture {
	t.Helper()
	return tofuApplyFixtureEdit(t, runner, func(text string) string {
		return strings.Replace(text, "limits {\n", "limits {\n"+strings.Join(limits, "\n")+"\n", 1)
	})
}

// tofuApplyFixtureEdit is tofuApplyFixture with edit, which changes the
// config, and setup, which changes the server's options.
func tofuApplyFixtureEdit(t *testing.T, runner *fakeTofu, edit func(string) string, setup ...func(*server.Options)) *fixture {
	t.Helper()
	approvals := withApprovals(t, func(text string) string {
		return text + `grant {
  teams = ["reviewers"]
  stacks = ["app"]
  envs = ["prod"]
  can = ["read", "apply"]
  risks = ["create", "update", "replace", "delete", "data-loss"]
}
`
	})
	f := tofuFixtureEdit(t, runner, `provider_uids = "61000-61063"`+"\n", func(text string) string {
		return edit(approvals(text))
	}, setup...)
	files := tofuFiles()
	f.gitlab.Commit(4242, "main", files)
	if r := f.do("POST", "/v1/repos", admin(f), repoRequest(nil)); r.status != http.StatusCreated {
		t.Fatalf("add: %d %s", r.status, r.body)
	}
	return f
}

func (f *fixture) tofuRun(tok string, body map[string]any) server.RemoteRun {
	f.t.Helper()
	r := f.do("POST", "/v1/runs", tok, runRequest(body))
	if r.status != http.StatusAccepted {
		f.t.Fatalf("apply: %d %s", r.status, r.body)
	}
	return f.waitRun(tok, r.doc["run"].(map[string]any)["id"].(string))
}

// An OpenTofu plan of a commit is approved by someone other than its
// author and applied by writ serve: OpenTofu gets the saved plan the plan
// sealed, the commit's files, the apply role's environment, and a token
// that writes the stack's state, which ends with the run. The run keeps
// the masked events, and the saved plan is deleted.
func TestTofuApproveApply(t *testing.T) {
	runner := &fakeTofu{}
	f := tofuApplyFixture(t, runner)
	p, author := f.planCommit(tofuFiles(), "add-cert")
	id := p.Plan.ID
	if p.Plan.Status != "pending" || p.Plan.Approvals.Need != 1 {
		t.Fatalf("plan %+v", p.Plan)
	}
	sealed, _, err := f.store.SavedPlan(context.Background(), id)
	if err != nil || len(sealed) == 0 || bytes.Contains(sealed, []byte("saved")) {
		t.Fatalf("the stored saved plan %q, %v: want it sealed", sealed, err)
	}
	if got := f.do("GET", "/v1/plans/"+id, author, nil); bytes.Contains(got.body, sealed) || strings.Contains(string(got.body), `"saved"`) {
		t.Fatalf("the API serves the saved plan: %s", got.body)
	}

	if r := f.do("POST", "/v1/plans/"+id+"/approvals", f.token(map[string]any{"user_access_level": "maintainer"}), nil); r.status != http.StatusForbidden || !strings.Contains(string(r.body), "cannot approve it") {
		t.Fatalf("the author's approval: %d %s", r.status, r.body)
	}
	if r := f.do("POST", "/v1/runs", bob(f), runRequest(map[string]any{"plan": id})); r.status != http.StatusForbidden || r.class() != "denied" {
		t.Fatalf("an apply with no approval: %d %s", r.status, r.body)
	}
	if len(runner.applied) != 0 {
		t.Fatal("OpenTofu applied a plan with no approval")
	}
	if r := f.do("POST", "/v1/plans/"+id+"/approvals", bob(f), nil); r.status != http.StatusOK || !strings.Contains(string(r.body), `"status":"approved"`) {
		t.Fatalf("bob's approval: %d %s", r.status, r.body)
	}

	run := f.tofuRun(bob(f), map[string]any{"plan": id})
	if run.Run.Status != "applied" || run.Run.Plan != id || len(run.Run.ApprovedBy) != 1 || run.Run.ApprovedBy[0].Person != "gitlab/user/2002" {
		t.Fatalf("run %+v\n%s", run.Run, run.Text)
	}
	var doc server.TofuApply
	if err := json.Unmarshal(run.Document, &doc); err != nil {
		t.Fatal(err)
	}
	wantEvents := []server.TofuApplyEvent{
		{Type: "apply_start", Address: "random_pet.p", Action: "create"},
		{Type: "apply_complete", Address: "random_pet.p", Action: "create"},
		{Type: "apply_complete", Address: `random_id.i["(sensitive)"]`, Action: "create", ElapsedSeconds: 1},
		{Type: "apply_complete", Address: `random_id.k["(sensitive)"]`, Action: "create"},
	}
	if doc.Format != server.FormatTofuApply || !doc.OK || !doc.Applied || doc.StateVersion != 1 || !slices.Equal(doc.Events, wantEvents) ||
		doc.Summary == nil || *doc.Summary != (server.TofuApplySummary{Add: 3}) {
		t.Fatalf("the run's document: %s", run.Document)
	}
	for _, want := range []string{"OpenTofu apply of app@prod\n", "  random_pet.p: create complete\n", "Apply: 3 added, 0 changed, 0 destroyed.\n", "State version 1 of app@prod.\n"} {
		if !strings.Contains(run.Text, want) {
			t.Errorf("the run's text lacks %q:\n%s", want, run.Text)
		}
	}
	served := f.do("GET", "/v1/runs/"+run.Run.ID, bob(f), nil)
	for _, secret := range []string{tofuCanary, tofuEnvSecret, "id_value"} {
		if strings.Contains(string(served.body), secret) {
			t.Errorf("the run the API serves holds %s:\n%s", secret, served.body)
		}
	}

	if len(runner.applied) != 1 {
		t.Fatalf("OpenTofu applied %d times", len(runner.applied))
	}
	a := runner.applied[0]
	if string(a.saved) != "saved" {
		t.Fatalf("OpenTofu applied the saved plan %q, want the one the plan saved", a.saved)
	}
	if a.stack.Dir != "app" || a.stack.User == nil || a.stack.User.UID < 61000 || envValue(a.stack.Creds, "TF_VAR_extra") != tofuEnvSecret || envValue(a.stack.Env, "TF_VAR_extra") != "" {
		t.Fatalf("the apply ran in %q as %v with environment %v and credentials %v", a.stack.Dir, a.stack.User, a.stack.Env, a.stack.Creds)
	}
	if got := runner.call(a.token, "LOCK", "/lock", `{"ID":"late"}`); got != http.StatusUnauthorized {
		t.Fatalf("the apply's state token answered %d after the run, want 401", got)
	}
	if saved, _, err := f.store.SavedPlan(context.Background(), id); err != nil || saved != nil {
		t.Fatalf("the applied plan keeps its saved plan: %q, %v", saved, err)
	}
	writes := auditKind(f, "state-write")
	if len(writes) != 1 || writes[0].Run != run.Run.ID || !slices.Equal(writes[0].Stacks, []string{"app"}) || writes[0].Env != "prod" ||
		!strings.HasPrefix(writes[0].Reason, "state version 1, serial 1, lineage lineage-1") {
		t.Fatalf("the state write's audit rows: %+v", writes)
	}
	if ends := auditKind(f, "apply-finished"); len(ends) != 1 || ends[0].Plan != id || ends[0].Reason != "applied" {
		t.Fatalf("the apply's audit rows: %+v", ends)
	}
	if r := f.do("POST", "/v1/runs", bob(f), runRequest(map[string]any{"plan": id})); r.status != http.StatusConflict || !strings.Contains(string(r.body), "is applied, and only a pending or approved plan is approved or applied") {
		t.Fatalf("a second apply of the plan: %d %s", r.status, r.body)
	}
}

// OpenTofu makes the same saved plan and document from one commit and
// state when two plans run within its timestamp's second, so they have
// one id. The second request answers with the first plan and the status
// its approvals give it, and neither request is left planning.
func TestTofuSamePlanTwice(t *testing.T) {
	f := tofuApplyFixture(t, &fakeTofu{})
	sha := f.gitlab.Commit(4242, "add-cert", tofuFiles())
	plan := func() server.RemotePlan {
		t.Helper()
		tok := f.token(map[string]any{"sha": sha, "ref_path": "refs/heads/add-cert"})
		r := f.do("POST", "/v1/plans", tok, planRequest(func(m map[string]any) { m["stacks"] = []string{"app"} }))
		if r.status != http.StatusAccepted {
			t.Fatalf("plan: %d %s", r.status, r.body)
		}
		return f.waitPlan(tok, r.doc["plan"].(map[string]any)["ref"].(string))
	}
	first := plan()
	if first.Plan.Status != "pending" {
		t.Fatalf("the first plan: %+v", first.Plan)
	}
	if r := f.do("POST", "/v1/plans/"+first.Plan.ID+"/approvals", bob(f), nil); r.status != http.StatusOK {
		t.Fatalf("bob's approval: %d %s", r.status, r.body)
	}
	second := plan()
	if second.Plan.ID != first.Plan.ID || second.Plan.Ref != first.Plan.Ref || second.Plan.Status != "approved" || len(second.Plan.Approvals.ApprovedBy) != 1 {
		t.Fatalf("the second plan: %+v, want the first, approved", second.Plan)
	}
	if n := plansIn(t, f, "planning"); n != 0 {
		t.Fatalf("%d plans are left planning", n)
	}
	if run := f.tofuRun(bob(f), map[string]any{"plan": first.Plan.ID}); run.Run.Status != "applied" {
		t.Fatalf("run %+v\n%s", run.Run, run.Text)
	}
}

// A plan whose end the control database fails to record ends failed with
// a diagnostic, not planning until the server restarts.
func TestTofuPlanRecordFails(t *testing.T) {
	f := tofuApplyFixture(t, &fakeTofu{})
	failWrites(t, f, "UPDATE", "plans", "NEW.saved IS NOT NULL")
	p, _ := f.planCommit(tofuFiles(), "add-cert")
	var doc struct{ Diagnostics []server.Diagnostic }
	if err := json.Unmarshal(p.Document, &doc); err != nil || p.Plan.Status != "failed" || p.Plan.ID != "" || len(doc.Diagnostics) != 1 ||
		!strings.Contains(doc.Diagnostics[0].Message, "could not record the plan") {
		t.Fatalf("plan %+v, document %s", p.Plan, p.Document)
	}
	if n := plansIn(t, f, "planning"); n != 0 {
		t.Fatalf("%d plans are left planning", n)
	}
}

// A plan that a second pipeline made byte for byte the same is that
// pipeline's plan too: its approval job approves it and its apply job
// applies it by id. A pipeline that requested neither still cannot.
func TestTofuSamePlanOtherPipeline(t *testing.T) {
	f := tofuApplyFixture(t, &fakeTofu{})
	sha := f.gitlab.Commit(4242, "add-cert", tofuFiles())
	plan := func(pipeline string) server.RemotePlan {
		t.Helper()
		tok := f.token(map[string]any{"sha": sha, "pipeline_id": pipeline, "ref_path": "refs/heads/add-cert"})
		r := f.do("POST", "/v1/plans", tok, planRequest(func(m map[string]any) { m["stacks"] = []string{"app"} }))
		if r.status != http.StatusAccepted {
			t.Fatalf("plan of pipeline %s: %d %s", pipeline, r.status, r.body)
		}
		return f.waitPlan(tok, r.doc["plan"].(map[string]any)["ref"].(string))
	}
	first, second := plan("801"), plan("802")
	if first.Plan.Status != "pending" || second.Plan.ID != first.Plan.ID {
		t.Fatalf("the plans: %+v and %+v, want one pending plan", first.Plan, second.Plan)
	}
	id := first.Plan.ID
	approveJob := func(pipeline string) response {
		return f.do("POST", "/v1/approvals", bob(f, map[string]any{"pipeline_id": pipeline}), runRequestBody(`{"format":"writ.approve-request/v0","pipeline":true}`)())
	}
	if r := approveJob("803"); r.status != http.StatusNotFound {
		t.Fatalf("pipeline 803's approval job: %d %s", r.status, r.body)
	}
	if r := f.do("POST", "/v1/plans/"+id+"/approvals", bob(f, map[string]any{"pipeline_id": "803"}), nil); r.status != http.StatusForbidden {
		t.Fatalf("pipeline 803 approved plan %s by id: %d %s", id, r.status, r.body)
	}
	if r := f.do("POST", "/v1/runs", bob(f, map[string]any{"pipeline_id": "803"}), runRequest(map[string]any{"plan": id})); r.status != http.StatusForbidden {
		t.Fatalf("pipeline 803 applied plan %s by id: %d %s", id, r.status, r.body)
	}
	if r := approveJob("802"); r.status != http.StatusOK {
		t.Fatalf("pipeline 802's approval job: %d %s", r.status, r.body)
	}
	if run := f.tofuRun(bob(f, map[string]any{"pipeline_id": "802"}), map[string]any{"plan": id}); run.Run.Status != "applied" {
		t.Fatalf("pipeline 802's apply: %+v\n%s", run.Run, run.Text)
	}
}

// A request of the same commit through a repository registered again
// under another name gets the approved plan of the first registration,
// and an apply after the merge of that repository finds the plan by the
// request's own sources.
func TestTofuSamePlanReplacedRepo(t *testing.T) {
	// The jobs of main apply only a plan that creates.
	runner := &fakeTofu{plan: createOnly}
	f := tofuApplyFixture(t, runner)
	sha := f.gitlab.Commit(4242, "add-cert", tofuFiles())
	plan := func(repo, pipeline string) server.RemotePlan {
		t.Helper()
		tok := f.token(map[string]any{"sha": sha, "pipeline_id": pipeline, "ref_path": "refs/heads/add-cert"})
		r := f.do("POST", "/v1/plans", tok, planRequest(func(m map[string]any) {
			m["source"] = map[string]any{"repo": repo}
			m["stacks"] = []string{"app"}
		}))
		if r.status != http.StatusAccepted {
			t.Fatalf("plan of repository %s: %d %s", repo, r.status, r.body)
		}
		return f.waitPlan(tok, r.doc["plan"].(map[string]any)["ref"].(string))
	}
	first := plan("infra", "801")
	if r := f.do("POST", "/v1/plans/"+first.Plan.ID+"/approvals", bob(f, map[string]any{"pipeline_id": "801"}), nil); r.status != http.StatusOK {
		t.Fatalf("bob's approval: %d %s", r.status, r.body)
	}
	// syncRepos replaces a repository whose config changed the same way.
	removed := control.Entry{At: time.Now(), Request: "sync", Kind: "repo-removed", Decision: control.NoDecision}
	if err := f.store.DeleteRepo(context.Background(), "infra", removed); err != nil {
		t.Fatal(err)
	}
	if r := f.do("POST", "/v1/repos", admin(f), repoRequest(func(m map[string]any) { m["name"] = "mirror" })); r.status != http.StatusCreated {
		t.Fatalf("add mirror: %d %s", r.status, r.body)
	}
	if second := plan("mirror", "802"); second.Plan.ID != first.Plan.ID || second.Plan.Status != "approved" {
		t.Fatalf("the mirror's plan: %+v, want approved plan %s", second.Plan, first.Plan.ID)
	}
	merged := f.gitlab.Commit(4242, "main", tofuFiles())
	run := f.tofuRun(mainJob(f, merged), map[string]any{"env": "prod", "stacks": []string{"app"}, "merged": map[string]any{"repo": "mirror"}})
	if run.Run.Status != "applied" || run.Run.Plan != first.Plan.ID || len(runner.applied) != 1 {
		t.Fatalf("the apply after the mirror's merge: %+v\n%s", run.Run, run.Text)
	}
}

// A plan that a newer, different plan of its merge request superseded
// takes no approval and does not apply, for the same reason. The newer
// plan does both.
func TestTofuNewerPlanOfMergeRequest(t *testing.T) {
	runner := &fakeTofu{}
	f := tofuApplyFixture(t, runner)
	sha := f.gitlab.Commit(4242, "add-cert", tofuFiles())
	plan := func(pipeline string) string {
		t.Helper()
		tok := f.token(map[string]any{"sha": sha, "pipeline_id": pipeline, "ref_path": "refs/heads/add-cert"})
		r := f.do("POST", "/v1/plans", tok, planRequest(func(m map[string]any) { m["stacks"] = []string{"app"} }))
		if r.status != http.StatusAccepted {
			t.Fatalf("plan of pipeline %s: %d %s", pipeline, r.status, r.body)
		}
		return f.waitPlan(tok, r.doc["plan"].(map[string]any)["ref"].(string)).Plan.ID
	}
	older := plan("801")
	runner.mu.Lock()
	runner.plan = func() (tofu.Plan, error) {
		return tofu.Plan{File: []byte("another saved plan"), JSON: []byte(tofuPlanJSON)}, nil
	}
	runner.mu.Unlock()
	newer := plan("802")
	if p := f.waitPlan(bob(f), older).Plan; newer == older || p.SupersededBy != newer {
		t.Fatalf("plan %s after plan %s: %+v", older, newer, p)
	}
	want := "plan " + older + " is stale: plan " + newer + " of app@prod is newer; plan again"
	job := bob(f, map[string]any{"pipeline_id": "801"})
	if r := f.do("POST", "/v1/plans/"+older+"/approvals", job, nil); r.status != http.StatusConflict || r.class() != "state" || !strings.Contains(string(r.body), want) {
		t.Fatalf("an approval of the older plan: %d %s", r.status, r.body)
	}
	if r := f.do("POST", "/v1/runs", job, runRequest(map[string]any{"plan": older})); r.status != http.StatusConflict || r.class() != "state" || !strings.Contains(string(r.body), want) {
		t.Fatalf("an apply of the older plan: %d %s", r.status, r.body)
	}
	if len(runner.applied) != 0 {
		t.Fatal("OpenTofu ran the older plan")
	}
	job = bob(f, map[string]any{"pipeline_id": "802"})
	if r := f.do("POST", "/v1/plans/"+newer+"/approvals", job, nil); r.status != http.StatusOK {
		t.Fatalf("an approval of the newer plan: %d %s", r.status, r.body)
	}
	if run := f.tofuRun(job, map[string]any{"plan": newer}); run.Run.Status != "applied" {
		t.Fatalf("the newer plan: %+v\n%s", run.Run, run.Text)
	}
}

// A first record of a plan's end that runs out of time does not take the
// record of its failure with it: the plan ends failed, not planning.
func TestTofuPlanRecordTimesOut(t *testing.T) {
	f := tofuApplyFixture(t, &fakeTofu{})
	if !sqldb.IsPostgres(f.loc) {
		t.Skip("needs Postgres, whose trigger can outlast the record's deadline")
	}
	raw := controltest.Raw(t, f.loc)
	for _, q := range []string{
		"CREATE FUNCTION slow_finish() RETURNS trigger LANGUAGE plpgsql SET search_path FROM CURRENT AS $$ BEGIN IF NEW.saved IS NOT NULL THEN PERFORM pg_sleep(11); END IF; RETURN NEW; END $$",
		"CREATE TRIGGER slow_finish BEFORE UPDATE ON plans FOR EACH ROW EXECUTE FUNCTION slow_finish()",
	} {
		if _, err := raw.ExecContext(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}
	p, _ := f.planCommit(tofuFiles(), "add-cert")
	var doc struct{ Diagnostics []server.Diagnostic }
	if err := json.Unmarshal(p.Document, &doc); err != nil || p.Plan.Status != "failed" || len(doc.Diagnostics) != 1 ||
		!strings.Contains(doc.Diagnostics[0].Message, "could not record the plan") {
		t.Fatalf("plan %+v, document %s", p.Plan, p.Document)
	}
}

// plansIn counts the control database's plans in status.
func plansIn(t *testing.T, f *fixture, status string) int {
	t.Helper()
	var n int
	if err := controltest.Raw(t, f.loc).QueryRowContext(context.Background(), "SELECT count(*) FROM plans WHERE status = '"+status+"'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// auditKind is the audit rows of stack app of one kind.
func auditKind(f *fixture, kind string) []control.Entry {
	var out []control.Entry
	for _, e := range f.audit(controltest.AuditFilter{Stack: "app"}) {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// approvedTofuPlan plans app on branch and has bob approve it.
func approvedTofuPlan(t *testing.T, f *fixture, branch string) string {
	t.Helper()
	p, _ := f.planCommit(tofuFiles(), branch)
	if r := f.do("POST", "/v1/plans/"+p.Plan.ID+"/approvals", bob(f), nil); r.status != http.StatusOK {
		t.Fatalf("bob's approval: %d %s", r.status, r.body)
	}
	return p.Plan.ID
}

// A plan that is not the newest of its stack@env is refused before
// OpenTofu runs. OpenTofu's own refusal of a stale saved plan fails the
// run with that reason.
func TestTofuStaleApply(t *testing.T) {
	t.Run("a newer plan", func(t *testing.T) {
		runner := &fakeTofu{}
		f := tofuApplyFixture(t, runner)
		older := approvedTofuPlan(t, f, "one")
		newer := approvedTofuPlan(t, f, "two")
		r := f.do("POST", "/v1/runs", bob(f), runRequest(map[string]any{"plan": older}))
		if r.status != http.StatusConflict || r.class() != "state" || !strings.Contains(string(r.body), "plan "+newer+" of app@prod is newer") {
			t.Fatalf("an apply of the older plan: %d %s", r.status, r.body)
		}
		if len(runner.applied) != 0 {
			t.Fatal("OpenTofu ran a stale plan")
		}
		if run := f.tofuRun(bob(f), map[string]any{"plan": newer}); run.Run.Status != "applied" {
			t.Fatalf("the newer plan: %+v", run.Run)
		}
	})
	t.Run("OpenTofu refuses it", func(t *testing.T) {
		runner := &fakeTofu{apply: func(context.Context, stateCall) ([]byte, error) {
			out := []byte(`{"type":"diagnostic","diagnostic":{"severity":"error","summary":"Saved plan is stale","detail":"the canary ` + tofuCanary + `"}}` + "\n")
			return out, fmt.Errorf("%w: %w", tofu.ErrStale, &tofu.StepError{Step: "apply", Code: 1, Stdout: out})
		}}
		f := tofuApplyFixture(t, runner)
		id := approvedTofuPlan(t, f, "one")
		run := f.tofuRun(bob(f), map[string]any{"plan": id})
		if run.Run.Status != "failed" || !strings.Contains(run.Text, "OpenTofu refused plan "+id+": the state of app@prod changed after the plan was made; plan again") {
			t.Fatalf("run %+v\n%s", run.Run, run.Text)
		}
		if p, err := f.store.Plan(context.Background(), id, time.Now()); err != nil || p.Status != control.Failed {
			t.Fatalf("the plan after a refused apply: %s, %v", p.Status, err)
		}
	})
}

// A failed apply keeps each diagnostic's severity, summary, and place,
// swept, and never its detail or snippet, which nonsensitive() can fill
// with a secret.
func TestTofuFailedApply(t *testing.T) {
	runner := &fakeTofu{apply: func(context.Context, stateCall) ([]byte, error) {
		out := []byte(`{"type":"apply_start","hook":{"resource":{"addr":"random_pet.p"},"action":"create"}}
{"type":"apply_errored","hook":{"resource":{"addr":"random_pet.p"},"action":"create","elapsed_seconds":0}}
{"type":"diagnostic","diagnostic":{"severity":"error","summary":"Provider failed with ` + tofuEnvSecret + `","detail":"secret ` + tofuCanary + `","range":{"filename":"main.tf","start":{"line":9,"column":1}},"snippet":{"code":"` + tofuCanary + `"}}}
`)
		return out, &tofu.StepError{Step: "apply", Code: 1, Stdout: out, Stderr: []byte("Error: " + tofuCanary)}
	}}
	f := tofuApplyFixture(t, runner)
	id := approvedTofuPlan(t, f, "one")
	run := f.tofuRun(bob(f), map[string]any{"plan": id})
	if run.Run.Status != "failed" {
		t.Fatalf("run %+v", run.Run)
	}
	for _, want := range []string{"  random_pet.p: create errored\n", "error: tofu apply exited with code 1\n", "error: main.tf:9:1: Provider failed with (sensitive)\n"} {
		if !strings.Contains(run.Text, want) {
			t.Errorf("the failed run's text lacks %q:\n%s", want, run.Text)
		}
	}
	served := f.do("GET", "/v1/runs/"+run.Run.ID, bob(f), nil)
	for _, bad := range []string{tofuCanary, tofuEnvSecret, "secret "} {
		if strings.Contains(string(served.body), bad) {
			t.Errorf("the failed run holds %q:\n%s", bad, served.body)
		}
	}
	if saved, _, err := f.store.SavedPlan(context.Background(), id); err != nil || saved != nil {
		t.Fatalf("the failed plan keeps its saved plan: %q, %v", saved, err)
	}
}

// At limits.apply_timeout an OpenTofu apply is interrupted: the run fails
// and says so, and gets no warning of Writ's own drain.
func TestTofuApplyTimeout(t *testing.T) {
	runner := &fakeTofu{apply: func(ctx context.Context, _ stateCall) ([]byte, error) {
		<-ctx.Done()
		return []byte(`{"type":"apply_start","hook":{"resource":{"addr":"random_pet.p"},"action":"create"}}` + "\n"), context.Cause(ctx)
	}}
	f := tofuApplyFixture(t, runner, `apply_timeout = "1s"`)
	id := approvedTofuPlan(t, f, "one")
	run := f.tofuRun(bob(f), map[string]any{"plan": id})
	want := "error: the apply ran past limits.apply_timeout, 1s: OpenTofu was interrupted, started no more changes, and wrote the state it reached; plan again\n"
	if run.Run.Status != "failed" || !strings.Contains(run.Text, want) || strings.Contains(run.Text, "drain") {
		t.Fatalf("run %+v\n%s", run.Run, run.Text)
	}
}

// tofuPlanOf is `tofu show -json` of a plan whose one resource change has
// actions and the rest of its change from change. The changes are cut
// from real tofu 1.13.9 plans of hashicorp/random.
func tofuPlanOf(actions, change string) func() (tofu.Plan, error) {
	return func() (tofu.Plan, error) {
		return tofu.Plan{File: []byte("saved"), JSON: []byte(`{"format_version":"1.2","terraform_version":"1.13.9","resource_changes":[
  {"address":"random_pet.q","mode":"managed","type":"random_pet","name":"q","provider_name":"registry.opentofu.org/hashicorp/random",
   "change":{"actions":` + actions + `,` + change + `}}]}`)}, nil
	}
}

// Every action OpenTofu writes carries a risk class, so a plan that only
// forgets or imports a resource needs the approval its rules ask for
// before it applies, and a plan with an action Writ does not know is
// refused.
func TestTofuPlanRisks(t *testing.T) {
	const (
		forget = `"before":{"id":"artistic-katydid","length":2,"separator":"-"},"after":null,"after_unknown":{},"before_sensitive":{},"after_sensitive":false`
		imp    = `"before":{"id":"abcdefgh","length":8},"after":{"id":"abcdefgh","length":8},"after_unknown":{},"before_sensitive":{},"after_sensitive":{},"importing":{"id":"abcdefgh"}`
	)
	for _, tc := range []struct {
		name, actions, change string
		risks                 []string
		text                  string
	}{
		{"forget", `["forget"]`, forget, []string{"delete"}, "Plan: 0 to add, 0 to change, 0 to destroy, 1 to forget.\n"},
		{"import", `["no-op"]`, imp, []string{"create"}, "Plan: 1 to import, 0 to add, 0 to change, 0 to destroy.\n"},
		{"forget and create", `["forget","create"]`, forget, []string{"replace"}, "Plan: 1 to add, 0 to change, 0 to destroy, 1 to forget.\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeTofu{plan: tofuPlanOf(tc.actions, tc.change)}
			f := tofuApplyFixture(t, runner)
			p, _ := f.planCommit(tofuFiles(), "change")
			if p.Plan.Status != "pending" || p.Plan.Approvals.Need != 1 || !slices.Equal(p.Plan.Risks, tc.risks) || p.Plan.Stacks[0].Changes != 1 {
				t.Fatalf("plan %+v, want pending with risks %v and one approval needed\n%s", p.Plan, tc.risks, p.Text)
			}
			if !strings.Contains(p.Text, tc.text) || strings.Contains(p.Text, "No changes.") {
				t.Fatalf("the plan's text lacks %q:\n%s", tc.text, p.Text)
			}
			if r := f.do("POST", "/v1/runs", bob(f), runRequest(map[string]any{"plan": p.Plan.ID})); r.status != http.StatusForbidden || r.class() != "denied" {
				t.Fatalf("an apply with no approval: %d %s", r.status, r.body)
			}
			if len(runner.applied) != 0 {
				t.Fatal("OpenTofu applied a plan with no approval")
			}
		})
	}
	t.Run("an action writ does not know", func(t *testing.T) {
		runner := &fakeTofu{plan: tofuPlanOf(`["explode"]`, forget)}
		f := tofuApplyFixture(t, runner)
		p, _ := f.planCommit(tofuFiles(), "change")
		if p.Plan.Status != "failed" || p.Plan.ID != "" || !strings.Contains(p.Text, `resource random_pet.q has the actions "explode", which writ serve does not know`) {
			t.Fatalf("plan %s %q:\n%s\nwant it refused, naming the action and the resource", p.Plan.Status, p.Plan.ID, p.Text)
		}
	})
}

// A control plane that stops while OpenTofu holds a stack's lock leaves
// the run applying, its state token live, and the lock held, which no
// expiry releases. The next process to start fails the run, ends the
// token, releases the lock with an audit row, and a new apply of the
// stack takes the lock.
func TestTofuRestartEndsState(t *testing.T) {
	held := make(chan stateCall, 1)
	var mu sync.Mutex
	var relock []int
	tofuRunner := &fakeTofu{}
	var first sync.Once
	tofuRunner.apply = func(ctx context.Context, c stateCall) ([]byte, error) {
		crash := false
		first.Do(func() { crash = true })
		if crash {
			if status := tofuRunner.call(c, "LOCK", "/lock", `{"ID":"lock-1"}`); status != http.StatusOK {
				return nil, fmt.Errorf("lock: %d", status)
			}
			held <- c
			<-ctx.Done()
			return nil, context.Cause(ctx)
		}
		status := tofuRunner.call(c, "LOCK", "/lock", `{"ID":"lock-2"}`)
		mu.Lock()
		relock = append(relock, status)
		mu.Unlock()
		tofuRunner.call(c, "UNLOCK", "/lock", `{"ID":"lock-2"}`)
		return nil, nil
	}
	f := tofuApplyFixture(t, tofuRunner)
	id := approvedTofuPlan(t, f, "one")
	r := f.do("POST", "/v1/runs", bob(f), runRequest(map[string]any{"plan": id}))
	if r.status != http.StatusAccepted {
		t.Fatalf("apply: %d %s", r.status, r.body)
	}
	runID := r.doc["run"].(map[string]any)["id"].(string)
	var c stateCall
	select {
	case c = <-held:
	case <-time.After(30 * time.Second):
		t.Fatal("the apply never took the state's lock")
	}

	// A run that ended without ending its state access left a token and
	// a lock on another stack.
	ctx := context.Background()
	gone := control.StateAuth{Hash: []byte("hash of GONE"), RunID: "GONE", Now: time.Now()}
	if err := f.store.AddStateToken(ctx, gone.Hash, control.StateToken{RunID: "GONE", Env: "prod", Stack: "other", Scope: control.StateApply, ExpiresAt: time.Now().Add(time.Hour)}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.LockState(ctx, gone, "prod", "other", control.StateLock{ID: "lock-gone", RunID: "GONE", Info: "{}", At: time.Now()}, control.Entry{Kind: "request", Decision: control.Allowed}); err != nil {
		t.Fatal(err)
	}

	// A second process starts on the same database while the first one's
	// apply holds the lock: the first one counts as stopped.
	again, err := server.New(context.Background(), server.Options{Config: f.cfg, Store: f.store, Tofu: &fakeTofu{}, Reap: func(uint32) error { return nil }, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if run, err := f.store.GetRun(context.Background(), runID); err != nil || run.Status != control.RunFailed {
		t.Fatalf("after a restart: %+v, %v", run, err)
	}
	if got := tofuRunner.get(c); got != http.StatusUnauthorized {
		t.Fatalf("the crashed run's state token answered %d after the restart, want 401", got)
	}
	if unlocks := auditKind(f, "state-unlock"); len(unlocks) != 1 || unlocks[0].Run != runID || unlocks[0].Env != "prod" || !strings.Contains(unlocks[0].Reason, "released lock lock-1 of run "+runID) {
		t.Fatalf("the forced unlock's audit rows: %+v", unlocks)
	}
	if _, err := f.store.StateTokenByHash(ctx, gone.Hash, time.Now()); !errors.Is(err, control.ErrNotFound) {
		t.Fatalf("the token of a run that no process runs resolves after the restart: %v", err)
	}
	if unlocks := f.audit(controltest.AuditFilter{Stack: "other"}); len(unlocks) != 1 || unlocks[0].Kind != "state-unlock" || unlocks[0].Run != "GONE" {
		t.Fatalf("the audit rows of the orphan's stack: %+v", unlocks)
	}

	next := approvedTofuPlan(t, f, "two")
	if run := f.tofuRun(bob(f), map[string]any{"plan": next}); run.Run.Status != "applied" {
		t.Fatalf("the apply after the restart: %+v\n%s", run.Run, run.Text)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(relock, []int{http.StatusOK}) {
		t.Fatalf("the apply after the restart locked the state with %v, want 200", relock)
	}
}

// failTokenDeletes makes each delete of a state token fail while cond, an
// SQL condition on the deleted row OLD, holds, as a database that times
// out would, until the returned func lifts it.
func failTokenDeletes(t *testing.T, f *fixture, cond string) func() {
	t.Helper()
	return failWrites(t, f, "DELETE", "tfstate_tokens", cond)
}

// failWrites makes each op (DELETE or UPDATE) of a row of table fail while
// cond, an SQL condition on the row OLD or NEW, holds, until the returned
// func lifts it.
func failWrites(t *testing.T, f *fixture, op, table, cond string) func() {
	t.Helper()
	raw := controltest.Raw(t, f.loc)
	create := []string{"CREATE TRIGGER fail_write BEFORE " + op + " ON " + table + " WHEN " + cond + " BEGIN SELECT RAISE(ABORT, 'synthetic failed write'); END"}
	drop := "DROP TRIGGER fail_write"
	if sqldb.IsPostgres(f.loc) {
		row := "NEW"
		if op == "DELETE" {
			row = "OLD"
		}
		create = []string{
			"CREATE FUNCTION fail_write() RETURNS trigger LANGUAGE plpgsql SET search_path FROM CURRENT AS $$ BEGIN IF " + cond +
				" THEN RAISE EXCEPTION 'synthetic failed write'; END IF; RETURN " + row + "; END $$",
			"CREATE TRIGGER fail_write BEFORE " + op + " ON " + table + " FOR EACH ROW EXECUTE FUNCTION fail_write()",
		}
		drop += " ON " + table
	}
	for _, q := range create {
		if _, err := raw.ExecContext(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}
	return func() {
		t.Helper()
		if _, err := raw.ExecContext(context.Background(), drop); err != nil {
			t.Fatal(err)
		}
	}
}

// lockThenFail is an apply that takes the stack's lock, sends its state
// call on held, and fails without the unlock.
func lockThenFail(r *fakeTofu, held chan<- stateCall) func(context.Context, stateCall) ([]byte, error) {
	return func(_ context.Context, c stateCall) ([]byte, error) {
		if status := r.call(c, "LOCK", "/lock", `{"ID":"held"}`); status != http.StatusOK {
			return nil, fmt.Errorf("lock: %d", status)
		}
		held <- c
		return nil, errors.New("synthetic failed apply")
	}
}

// heldLocks counts the stack locks in the control database.
func heldLocks(t *testing.T, f *fixture) int {
	t.Helper()
	var n int
	if err := controltest.Raw(t, f.loc).QueryRowContext(context.Background(), "SELECT count(*) FROM tfstate_locks").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// When a run's own end of its state access fails, the transaction that
// records the run's end ends its token and releases its lock, with an
// audit row: a run never reads as ended while its token works or while
// it holds a lock, which no expiry releases.
func TestTofuRunEndEndsState(t *testing.T) {
	r := &fakeTofu{}
	held := make(chan stateCall, 1)
	r.apply = lockThenFail(r, held)
	f := tofuApplyFixture(t, r)
	id := approvedTofuPlan(t, f, "one")
	failTokenDeletes(t, f, "(SELECT status FROM runs WHERE id = OLD.run_id) = 'applying'")
	run := f.tofuRun(bob(f), map[string]any{"plan": id})
	if run.Run.Status != "failed" {
		t.Fatalf("run %+v\n%s", run.Run, run.Text)
	}
	if got := r.get(<-held); got != http.StatusUnauthorized {
		t.Fatalf("the ended run's state token answered %d, want 401", got)
	}
	if n := heldLocks(t, f); n != 0 {
		t.Fatalf("%d locks held after the run ended", n)
	}
	want := "writ serve released lock held of run " + run.Run.ID + " on app@prod: its run ended while it held the lock"
	if unlocks := auditKind(f, "state-unlock"); len(unlocks) != 1 || unlocks[0].Run != run.Run.ID || unlocks[0].Reason != want {
		t.Fatalf("the state-unlock audit rows: %+v", unlocks)
	}
}

// The sweep each minute ends the state access that no end of a run did:
// a run whose end of its state access and record of its end both failed,
// and a run that ended and left its token and lock. Each token then
// answers 401 and each lock is released, with an audit row.
func TestTofuSweepEndsState(t *testing.T) {
	r := &fakeTofu{}
	held := make(chan stateCall, 1)
	r.apply = lockThenFail(r, held)
	f := tofuApplyFixture(t, r)
	id := approvedTofuPlan(t, f, "one")
	lift := failTokenDeletes(t, f, "1 = 1")
	resp := f.do("POST", "/v1/runs", bob(f), runRequest(map[string]any{"plan": id}))
	if resp.status != http.StatusAccepted {
		t.Fatalf("apply: %d %s", resp.status, resp.body)
	}
	runID := resp.doc["run"].(map[string]any)["id"].(string)
	c := <-held
	for deadline := time.Now().Add(30 * time.Second); f.srv.Applies() != 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the apply never ended")
		}
	}
	lift()

	ctx := context.Background()
	gone := control.StateAuth{Hash: []byte("hash of GONE"), RunID: "GONE", Now: time.Now()}
	if err := f.store.AddStateToken(ctx, gone.Hash, control.StateToken{RunID: "GONE", Env: "prod", Stack: "other", Scope: control.StateApply, ExpiresAt: time.Now().Add(time.Hour)}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.LockState(ctx, gone, "prod", "other", control.StateLock{ID: "lock-gone", RunID: "GONE", Info: "{}", At: time.Now()}, control.Entry{Kind: "request", Decision: control.Allowed}); err != nil {
		t.Fatal(err)
	}

	f.srv.Sweep()
	if run := f.waitRun(bob(f), runID); run.Run.Status != "failed" {
		t.Fatalf("run %+v\n%s", run.Run, run.Text)
	}
	if got := r.get(c); got != http.StatusUnauthorized {
		t.Fatalf("the run's state token answered %d after the sweep, want 401", got)
	}
	if _, err := f.store.StateTokenByHash(ctx, gone.Hash, time.Now()); !errors.Is(err, control.ErrNotFound) {
		t.Fatalf("the left token resolves after the sweep: %v", err)
	}
	if n := heldLocks(t, f); n != 0 {
		t.Fatalf("%d locks held after the sweep", n)
	}
	if unlocks := auditKind(f, "state-unlock"); len(unlocks) != 1 || unlocks[0].Run != runID || !strings.Contains(unlocks[0].Reason, "released lock held of run "+runID) {
		t.Fatalf("the state-unlock audit rows of app: %+v", unlocks)
	}
	if unlocks := f.audit(controltest.AuditFilter{Stack: "other"}); len(unlocks) != 1 || unlocks[0].Kind != "state-unlock" || unlocks[0].Run != "GONE" {
		t.Fatalf("the audit rows of the left lock's stack: %+v", unlocks)
	}
}

// appliedApp plans and approves tofuFiles at a new branch, has bob apply
// it, and returns the run.
func appliedApp(t *testing.T, f *fixture, branch string) server.RemoteRun {
	t.Helper()
	p, _ := f.planCommit(tofuFiles(), branch)
	if r := f.do("POST", "/v1/plans/"+p.Plan.ID+"/approvals", bob(f), nil); r.status != http.StatusOK {
		t.Fatalf("bob's approval: %d %s", r.status, r.body)
	}
	run := f.tofuRun(bob(f), map[string]any{"plan": p.Plan.ID})
	if run.Run.Status != "applied" {
		t.Fatalf("run %+v\n%s", run.Run, run.Text)
	}
	return run
}

// indexDump is every row of the state index tables, as text.
func indexDump(t *testing.T, f *fixture) string {
	t.Helper()
	raw := controltest.Raw(t, f.loc)
	var b strings.Builder
	for _, table := range []string{"state_index", "state_index_stacks"} {
		rows, err := raw.QueryContext(context.Background(), "SELECT * FROM "+table)
		if err != nil {
			t.Fatal(err)
		}
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&b, "%s %s\n", table, vals)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return b.String()
}

// An apply that succeeds fills the state index of its stack@env from the
// state it left, through the mask: each managed resource, in the root
// module and in a child module, with only the attributes the index keeps.
// An attribute the state marks sensitive, one the index does not keep,
// and a nomad_job's attribute on another type never reach the
// table.
func TestTofuApplyFillsIndex(t *testing.T) {
	runner := &fakeTofu{}
	f := tofuApplyFixture(t, runner)
	appliedApp(t, f, "add-cert")
	ix, err := f.store.Index(context.Background(), "prod", "app")
	if err != nil || ix.Backend != control.BackendHTTP || ix.Version != 1 || !slices.Equal(ix.Rows, tofuIndex) {
		t.Fatalf("the index of app@prod: %+v %v\nwant rows %+v", ix, err, tofuIndex)
	}
	dump := indexDump(t, f)
	if !strings.Contains(dump, "keen-mole") {
		t.Fatalf("the dump of the index lacks a row it holds:\n%s", dump)
	}
	for _, canary := range []string{tofuMarked, tofuNotListed, tofuNotForType, tofuCanary, tofuEnvSecret} {
		if strings.Contains(dump, canary) {
			t.Errorf("the state index holds %s:\n%s", canary, dump)
		}
	}
}

// A state whose index was not stored, because the show after its apply
// failed, is indexed by the next refresh: a run of show on the stored
// state, with no credentials and no state backend, as a provider uid,
// whose mask still knows provider_env's secrets and the secrets the
// apply's mask learned from the plan. A refresh of a current index runs
// nothing. Neither canary is then in a row of the control database or a
// file of the data directory.
func TestTofuIndexRefresh(t *testing.T) {
	runner := &fakeTofu{showErr: errors.New("tofu show exited with code 1")}
	f := tofuApplyFixture(t, runner)
	appliedApp(t, f, "add-cert")
	if _, err := f.store.Index(context.Background(), "prod", "app"); !errors.Is(err, control.ErrNotFound) {
		t.Fatalf("the index after a failed show: %v, want none", err)
	}
	runner.mu.Lock()
	runner.showErr = nil
	// Addresses keyed by nonsensitive() of provider_env's variable, and of
	// a sensitive variable that only the plan holds, which no mark covers.
	runner.state = `{"format_version":"1.0","values":{"root_module":{"resources":[
		{"address":"terraform_data.keyed[\"` + tofuEnvSecret + `\"]","mode":"managed","type":"terraform_data","name":"keyed",
		 "provider_name":"terraform.io/builtin/terraform","values":{"id":"u-1"},"sensitive_values":{}},
		{"address":"terraform_data.planned[\"` + tofuCanary + `\"]","mode":"managed","type":"terraform_data","name":"planned",
		 "provider_name":"terraform.io/builtin/terraform","values":{"id":"u-2"},"sensitive_values":{}}]}}}`
	runner.mu.Unlock()
	f.srv.RefreshIndexes()
	ix, err := f.store.Index(context.Background(), "prod", "app")
	want := []control.IndexRow{
		{Address: `terraform_data.keyed["(sensitive)"]`, Type: "terraform_data", Provider: "terraform.io/builtin/terraform", ID: "u-1"},
		{Address: `terraform_data.planned["(sensitive)"]`, Type: "terraform_data", Provider: "terraform.io/builtin/terraform", ID: "u-2"},
	}
	if err != nil || ix.Version != 1 || !slices.Equal(ix.Rows, want) {
		t.Fatalf("the index after a refresh: %+v %v", ix, err)
	}
	runner.mu.Lock()
	shown := slices.Clone(runner.shown)
	runner.mu.Unlock()
	if len(shown) != 1 {
		t.Fatalf("%d shows, want one", len(shown))
	}
	if want := `{"version":4,"serial":1,"lineage":"lineage-1","resources":[]}`; string(shown[0].state) != want {
		t.Errorf("show was given the state %q, want the stored version %q", shown[0].state, want)
	}
	if s := shown[0].stack; !slices.Equal(s.Env, []string{"PATH=/usr/local/bin:/usr/bin:/bin", "AWS_EC2_METADATA_DISABLED=true"}) || len(s.Creds) != 0 ||
		s.User == nil || s.Dir != "app" {
		t.Errorf("show ran with env %q, creds %q, user %v, dir %q", s.Env, s.Creds, s.User, s.Dir)
	}
	f.srv.RefreshIndexes()
	runner.mu.Lock()
	again := len(runner.shown)
	runner.mu.Unlock()
	if again != 1 {
		t.Fatalf("a refresh of a current index ran show again")
	}
	scanForCanaries(t, f, tofuCanary, tofuEnvSecret)
}

// A state version with no sealed secrets of its apply's mask, as one
// written before writ serve kept them, is not indexed: a refresh cannot
// know the plan's secrets, so it runs nothing and the index stays as it
// was until an apply writes a newer version.
func TestTofuIndexRefreshWithoutSecrets(t *testing.T) {
	runner := &fakeTofu{showErr: errors.New("tofu show exited with code 1")}
	f := tofuApplyFixture(t, runner)
	appliedApp(t, f, "add-cert")
	if _, err := controltest.Raw(t, f.loc).ExecContext(context.Background(), "UPDATE tfstate_versions SET index_secrets = NULL"); err != nil {
		t.Fatal(err)
	}
	runner.mu.Lock()
	runner.showErr = nil
	runner.mu.Unlock()
	f.srv.RefreshIndexes()
	if ix, err := f.store.Index(context.Background(), "prod", "app"); !errors.Is(err, control.ErrNotFound) {
		t.Fatalf("the index of a version with no sealed secrets: %+v %v, want none", ix, err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.shown) != 0 {
		t.Fatalf("a refresh of a version with no sealed secrets ran show")
	}
}

// scanForCanaries fails if a canary, plain or encoded, is in a row of a
// table of f's control database or a file of its data directory.
func scanForCanaries(t *testing.T, f *fixture, canaries ...string) {
	t.Helper()
	ctx := context.Background()
	raw := controltest.Raw(t, f.loc)
	list := "SELECT name FROM sqlite_master WHERE type = 'table'"
	if sqldb.IsPostgres(f.loc) {
		list = "SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema()"
	}
	rows, err := raw.QueryContext(ctx, list)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	if err := rows.Close(); err != nil || len(tables) == 0 {
		t.Fatalf("the tables of the control database %v: %v", tables, err)
	}
	texts := map[string]string{}
	for _, table := range tables {
		rows, err := raw.QueryContext(ctx, `SELECT * FROM "`+table+`"`)
		if err != nil {
			t.Fatal(err)
		}
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			for _, v := range vals {
				if bs, ok := v.([]byte); ok {
					v = string(bs)
				}
				fmt.Fprintf(&b, "%v\x00", v)
			}
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		texts["table "+table] = b.String()
	}
	err = filepath.WalkDir(f.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		b, err := os.ReadFile(p)
		texts["file "+p] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for where, text := range texts {
		for _, canary := range canaries {
			for form, v := range map[string]string{
				"plain":           canary,
				"base64":          base64.StdEncoding.EncodeToString([]byte(canary)),
				"unpadded base64": base64.RawStdEncoding.EncodeToString([]byte(canary)),
				"URL-safe base64": base64.RawURLEncoding.EncodeToString([]byte(canary)),
				"hex":             hex.EncodeToString([]byte(canary)),
				"upper case":      strings.ToUpper(canary),
			} {
				if strings.Contains(text, v) {
					t.Errorf("%s holds %s, %s", where, canary, form)
				}
			}
		}
	}
}

// GET /v1/runs lists the runs of an OpenTofu stack, which Writ's engine
// keeps no state for, as writ.runs/v0, from the runs the control database
// recorded: who applied which plan of which sources, with the approvals
// that counted. A stack@env with no run still has none to list.
func TestTofuRunsListed(t *testing.T) {
	f := tofuApplyFixture(t, &fakeTofu{})
	list := func() server.RemoteRuns {
		t.Helper()
		r := f.do("GET", "/v1/applies?env=prod&stack=app", bob(f), nil)
		var doc server.RemoteRuns
		if err := json.Unmarshal(r.body, &doc); err != nil || r.status != http.StatusOK || doc.Format != "writ.remote-runs/v0" || !doc.OK || doc.More {
			t.Fatalf("GET /v1/applies: %d %s", r.status, r.body)
		}
		return doc
	}
	if doc := list(); len(doc.Runs) != 0 {
		t.Fatalf("the runs of a stack with none: %+v", doc.Runs)
	}
	run := appliedApp(t, f, "add-cert")
	plan, err := f.store.Plan(context.Background(), run.Run.Plan, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	doc := list()
	if len(doc.Runs) != 1 {
		t.Fatalf("runs %+v", doc.Runs)
	}
	got := doc.Runs[0]
	if got.ID != run.Run.ID || got.Plan != plan.ID || got.Sources != plan.SourceDigest || got.Sources == "" || got.Actor.Person != "gitlab/user/2002" || got.Actor.Kind != "ci-job" ||
		got.StartedAt != run.Run.StartedAt || len(got.ApprovedBy) != 1 || got.ApprovedBy[0].Person != "gitlab/user/2002" {
		t.Fatalf("the run listed: %+v", got)
	}
}

// GET /v1/applies lists the runs writ serve started in an environment,
// newest first, each with its stack and plan, and only to a principal
// that may read the stack.
func TestTofuRunList(t *testing.T) {
	f := tofuFixtureEdit(t, &fakeTofu{}, `provider_uids = "61000-61063"`+"\n", withApprovals(t, func(text string) string {
		return text + `grant {
  teams = ["reviewers"]
  stacks = ["app"]
  envs = ["prod"]
  can = ["read", "apply"]
  risks = ["create", "update", "replace", "delete", "data-loss"]
}

team "outsiders" {
  issuer = "gitlab"
  match = { project_id = ["777"] }
}

grant {
  teams = ["outsiders"]
  stacks = ["network"]
  envs = ["prod"]
  can = ["read"]
}
`
	}))
	f.gitlab.Commit(4242, "main", tofuFiles())
	if r := f.do("POST", "/v1/repos", admin(f), repoRequest(nil)); r.status != http.StatusCreated {
		t.Fatalf("add: %d %s", r.status, r.body)
	}
	id := approvedTofuPlan(t, f, "one")
	run := f.tofuRun(bob(f), map[string]any{"plan": id})
	var doc server.RemoteRuns
	list := func(tok, query string) response {
		t.Helper()
		r := f.do("GET", "/v1/applies"+query, tok, nil)
		doc = server.RemoteRuns{}
		if r.status == http.StatusOK {
			if err := json.Unmarshal(r.body, &doc); err != nil {
				t.Fatal(err)
			}
		}
		return r
	}
	for _, q := range []string{"?env=prod", "?env=prod&stack=app"} {
		if r := list(bob(f), q); r.status != http.StatusOK || doc.Format != server.FormatRemoteRuns || len(doc.Runs) != 1 || doc.More {
			t.Fatalf("GET /v1/applies%s: %d %s", q, r.status, r.body)
		}
		got := doc.Runs[0]
		if got.ID != run.Run.ID || got.Plan != id || got.Stack != "app" || got.Env != "prod" || got.Status != "applied" || got.StartedAt == "" || got.EndedAt == "" {
			t.Fatalf("GET /v1/applies%s: %+v", q, got)
		}
	}
	if r := list(bob(f), "?env=prod&stack=other"); r.status != http.StatusOK || len(doc.Runs) != 0 {
		t.Fatalf("the runs of another stack: %d %s", r.status, r.body)
	}
	for q, want := range map[string]string{"": "applies needs ?env=", "?env=prod&limit=500": "limit must be 1 to 200", "?env=nowhere": "is not one writ serve plans in"} {
		if r := list(bob(f), q); r.status != http.StatusBadRequest || !strings.Contains(string(r.body), want) {
			t.Errorf("GET /v1/applies%s: %d %s", q, r.status, r.body)
		}
	}
	// A CI job of another project may read network, not app.
	if r := list(f.token(map[string]any{"project_id": "777"}), "?env=prod"); r.status != http.StatusOK || len(doc.Runs) != 0 {
		t.Fatalf("a principal that may not read app listed its run: %d %s", r.status, r.body)
	}
}
