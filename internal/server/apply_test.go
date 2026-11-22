package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/authz"
	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/control/controltest"
	"github.com/mr-karan/writ/internal/forge/forgetest"
	"github.com/mr-karan/writ/internal/oidc/oidctest"
	"github.com/mr-karan/writ/internal/server"
)

// withApprovals is the merge request flow's config: any change to prod
// needs one approval, maintainers of the project approve, and the jobs of
// protected main apply after a merge. edit, when set, changes the result.
func withApprovals(t *testing.T, more ...func(string) string) func(string) string {
	forge := withForge(t, `approval {
  stacks = ["*"]
  envs = ["prod"]
  risks = ["create", "update", "replace", "delete", "data-loss"]
  need = 1
}`)
	return func(text string) string {
		text = forge(text)
		text += `team "infra_main" {
  issuer = "gitlab"
  match = { project_id = ["4242"], ref_path = ["refs/heads/main"], ref_protected = ["true"] }
}
`
		text = strings.Replace(text, "grant {", `grant {
  teams = ["reviewers"]
  stacks = ["*"]
  envs = ["prod"]
  can = ["approve"]
}

grant {
  teams = ["infra_main"]
  stacks = ["*"]
  envs = ["prod"]
  can = ["read", "apply"]
  risks = ["create"]
}

grant {`, 1)
		for _, m := range more {
			text = m(text)
		}
		return text
	}
}

// bob is a maintainer who runs the approve job of alice's pipeline.
func bob(f *fixture, edits ...map[string]any) string {
	claims := map[string]any{"user_id": "2002", "user_login": "bob", "user_access_level": "maintainer"}
	for _, e := range edits {
		for k, v := range e {
			claims[k] = v
		}
	}
	return f.token(claims)
}

// mainJob is the apply job of the pipeline on main after a merge.
func mainJob(f *fixture, sha string) string {
	return f.token(map[string]any{"sha": sha, "ref": "main", "ref_path": "refs/heads/main", "ref_protected": "true",
		"pipeline_source": "push", "pipeline_id": "1849290", "job_id": "9922150"})
}

func deniedMainJob(f *fixture, sha string) string {
	return f.token(map[string]any{"sha": sha, "ref": "main", "ref_path": "refs/heads/main", "ref_protected": "false",
		"pipeline_source": "push", "pipeline_id": "1849291", "job_id": "9922151"})
}

func checkMergedDenial(t *testing.T, r response, hidden ...string) {
	t.Helper()
	if r.status != http.StatusForbidden || r.class() != "denied" {
		t.Errorf("merged apply without a grant: %d %s", r.status, r.body)
	}
	for _, value := range hidden {
		if strings.Contains(string(r.body), value) {
			t.Errorf("merged apply disclosed %q: %s", value, r.body)
		}
	}
}

func runRequest(m map[string]any) *bytes.Reader {
	m["format"] = "writ.run-request/v1"
	b, _ := json.Marshal(m)
	return bytes.NewReader(b)
}

func mergedRequest(stack string) *bytes.Reader {
	return runRequest(map[string]any{"env": "prod", "stacks": []string{stack}, "merged": map[string]any{}})
}

// waitRun waits for a run to end.
func (f *fixture) waitRun(tok, id string) server.RemoteRun {
	f.t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		r := f.do("GET", "/v1/runs/"+id+"?wait=10s", tok, nil)
		var run server.RemoteRun
		if err := json.Unmarshal(r.body, &run); err != nil || run.Run == nil {
			f.t.Fatalf("runs %s: %d %s", id, r.status, r.body)
		}
		if run.Run.Status != "applying" {
			return run
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("runs %s still applying: %s", id, r.body)
		}
	}
}

// mrPlan plans stack at a merge request's commit on branch, from the job
// of pipeline, and returns the plan.
func (f *fixture) mrPlan(sha, branch, stack, pipeline string) server.RemotePlan {
	f.t.Helper()
	tok := f.token(map[string]any{"sha": sha, "pipeline_id": pipeline, "ref": branch, "ref_path": "refs/heads/" + branch})
	r := f.do("POST", "/v1/plans", tok, planRequest(func(m map[string]any) { m["stacks"] = []string{stack} }))
	if r.status != http.StatusAccepted {
		f.t.Fatalf("plan: %d %s", r.status, r.body)
	}
	return f.waitPlan(tok, r.doc["plan"].(map[string]any)["ref"].(string))
}

func (f *fixture) addInfra() {
	f.t.Helper()
	f.gitlab.AddProject(4242, "acme/infra", "main")
	f.gitlab.Commit(4242, "main", infraFiles())
	if r := f.do("POST", "/v1/repos", admin(f), repoRequest(nil)); r.status != http.StatusCreated {
		f.t.Fatalf("add: %d %s", r.status, r.body)
	}
}

// changed is infraFiles with one more resource in network.
func changed(extra string) map[string]forgetest.File {
	files := infraFiles()
	files["infra/network/main.tf"] = forgetest.File{Content: files["infra/network/main.tf"].Content + extra}
	return files
}

// The merge request flow end to end: a pipeline plans a commit, the
// author's own approval and apply are refused, a maintainer approves from
// the pipeline, and after the merge the job on main applies exactly that
// saved plan, found by the merged commit's sources.
func TestMergeRequestFlow(t *testing.T) {
	f := planFixture(t, withApprovals(t))
	f.addInfra()
	files := changed("resource \"random_pet\" \"pet2\" {}\n")
	mr := f.gitlab.Commit(4242, "add-subnet-d", files)
	p := f.mrPlan(mr, "add-subnet-d", "network", "1849213")
	id := p.Plan.ID
	if p.Plan.Status != "pending" || p.Plan.Approvals.Need != 1 || len(p.Plan.Approvals.Rules) != 1 || p.Plan.Approvals.Rules[0] != 1 {
		t.Fatalf("plan %+v", p.Plan)
	}
	if strings.Join(p.Plan.Approvals.TeamsThatMayApprove, ",") != "reviewers" {
		t.Fatalf("teams that may approve %v", p.Plan.Approvals.TeamsThatMayApprove)
	}

	// Before any approval, the merge request job may not apply, and the
	// job on main finds the plan and says it needs an approval.
	if r := f.do("POST", "/v1/runs", f.token(map[string]any{"sha": mr}), runRequest(map[string]any{"plan": id})); r.status != http.StatusForbidden || r.class() != "denied" {
		t.Fatalf("the merge request job applied: %d %s", r.status, r.body)
	}
	early := f.gitlab.Commit(4242, "main", files)
	if r := f.do("POST", "/v1/runs", mainJob(f, early), mergedRequest("network")); r.status != http.StatusForbidden || !strings.Contains(string(r.body), "needs 1 approval;") {
		t.Fatalf("applied with no approval: %d %s", r.status, r.body)
	}

	t.Run("approval details require an apply grant", func(t *testing.T) {
		checkMergedDenial(t, f.do("POST", "/v1/runs", deniedMainJob(f, early), mergedRequest("network")), id, "needs 1 approval")
	})

	// alice cannot approve her own plan, a job of another pipeline cannot
	// approve it, and an approve job on a team that matches only the job is
	// refused by the config check (TestApproveGrantNeedsAPerson).
	pipeline := runRequestBody(`{"format":"writ.approve-request/v0","pipeline":true}`)
	if r := f.do("POST", "/v1/approvals", f.token(map[string]any{"user_access_level": "maintainer"}), pipeline()); r.status != http.StatusForbidden || !strings.Contains(string(r.body), "cannot approve it") {
		t.Fatalf("self-approval: %d %s", r.status, r.body)
	}
	if r := f.do("POST", "/v1/plans/"+id+"/approvals", bob(f, map[string]any{"pipeline_id": "777"}), nil); r.status != http.StatusForbidden || !strings.Contains(string(r.body), "its own pipeline") {
		t.Fatalf("another pipeline's approval: %d %s", r.status, r.body)
	}
	if r := f.do("POST", "/v1/plans/"+id+"/approvals", f.token(nil), nil); r.status != http.StatusForbidden || !strings.Contains(string(r.body), "may not approve") {
		t.Fatalf("an approval without approve: %d %s", r.status, r.body)
	}
	// A GitLab token whose claims name no user fails the token check, so
	// no approval comes from no person.
	if r := f.do("POST", "/v1/plans/"+id+"/approvals", bob(f, map[string]any{"user_id": nil}), nil); r.status != http.StatusUnauthorized || !strings.Contains(string(r.body), "user_id is missing") {
		t.Fatalf("an approval by no person: %d %s", r.status, r.body)
	}
	tok := bob(f)
	r := f.do("POST", "/v1/approvals", tok, pipeline())
	if r.status != http.StatusOK {
		t.Fatalf("bob's approval: %d %s", r.status, r.body)
	}
	var ad server.ApproveDoc
	_ = json.Unmarshal(r.body, &ad)
	if len(ad.Plans) != 1 || ad.Plans[0].Status != "approved" || len(ad.Plans[0].ApprovedBy) != 1 || ad.Plans[0].ApprovedBy[0].Via != "gitlab job 9922101" {
		t.Fatalf("approve doc %s", r.body)
	}
	if r := f.do("POST", "/v1/approvals", tok, pipeline()); r.status != http.StatusUnauthorized || r.class() != "auth" {
		t.Fatalf("a token approved twice: %d %s", r.status, r.body)
	}
	// Now that the plan has its approval, alice's job passes too.
	if r := f.do("POST", "/v1/approvals", f.token(map[string]any{"user_access_level": "maintainer"}), pipeline()); r.status != http.StatusOK {
		t.Fatalf("the approve job after an approval: %d %s", r.status, r.body)
	}

	// The merge. The job on main names no plan: the merged commit's
	// sources of network equal the plan's.
	merged := f.gitlab.Commit(4242, "main", files)
	main := mainJob(f, merged)
	r = f.do("POST", "/v1/runs", main, mergedRequest("network"))
	if r.status != http.StatusAccepted {
		t.Fatalf("apply after the merge: %d %s", r.status, r.body)
	}
	runID := r.doc["run"].(map[string]any)["id"].(string)
	run := f.waitRun(main, runID)
	if run.Run.Status != "applied" || run.Run.Plan != id || run.Run.NamedBy != "merged" || run.Run.MergedCommit != merged || len(run.Run.ApprovedBy) != 1 {
		t.Fatalf("run %+v", run.Run)
	}
	applied := f.tofu.appliedSoFar()
	if len(applied) != 1 || string(applied[0].saved) != "saved" || applied[0].stack.Dir != "network" {
		t.Fatalf("OpenTofu applied %+v", applied)
	}
	got := f.waitPlan(main, id)
	if got.Plan.Status != "applied" || got.Plan.Run != runID {
		t.Fatalf("after the run: %+v", got.Plan)
	}
	var moves []string
	for _, ev := range got.Plan.History {
		moves = append(moves, ev.From+">"+ev.To)
	}
	if strings.Join(moves, " ") != "planning>pending pending>approved approved>applying applying>applied" {
		t.Fatalf("history %v", moves)
	}

	// The same commit again, as a retried apply job sends it, has nothing
	// to apply: a 200 that names the plan and the run that applied it, and
	// no run starts. It was a 409 saved-plan refusal, which failed the job.
	r = f.do("POST", "/v1/runs", mainJob(f, merged), mergedRequest("network"))
	var again server.RemoteRun
	_ = json.Unmarshal(r.body, &again)
	if r.status != http.StatusOK || !again.OK || again.Run != nil || again.Plan != id ||
		!strings.Contains(again.Text, "Run "+runID+" applied plan "+id+" of network@prod") || !strings.Contains(again.Text, "there is nothing to apply") {
		t.Fatalf("a second apply: %d %s", r.status, r.body)
	}
	if n := len(f.tofu.appliedSoFar()); n != 1 {
		t.Fatalf("the second apply ran OpenTofu: %d applies", n)
	}
	if rows := f.audit(controltest.AuditFilter{Request: r.header.Get("X-Request-Id")}); len(rows) != 1 || rows[0].Decision != control.Allowed || rows[0].Run != runID ||
		rows[0].Plan != id || !strings.Contains(rows[0].Reason, "nothing to apply: run "+runID) {
		t.Fatalf("the second apply's audit rows %+v", rows)
	}
	// Once the state moves past what the run wrote, the same request is
	// refused, so the job asks for a new plan.
	raw := controltest.Raw(t, f.loc)
	if _, err := raw.ExecContext(context.Background(), `INSERT INTO tfstate_versions (env, stack, version, serial, lineage, sha256, run_id, created_at, sealed)
		VALUES ('prod', 'network', 99, 99, 'lin', 'hash', 'OUTSIDEXXXXXXXXXXXXXXXXXXX', 0, ?)`, []byte("sealed")); err != nil {
		t.Fatal(err)
	}
	if r := f.do("POST", "/v1/runs", mainJob(f, merged), mergedRequest("network")); r.status != http.StatusConflict || r.class() != "state" ||
		!strings.Contains(string(r.body), "is at version 99, which run "+runID+" did not write: plan again") {
		t.Fatalf("an apply after the state moved: %d %s", r.status, r.body)
	}
	t.Run("applied run details require an apply grant", func(t *testing.T) {
		checkMergedDenial(t, f.do("POST", "/v1/runs", deniedMainJob(f, merged), mergedRequest("network")), id, runID, "version 99")
	})
	// The audit trail names the approval and the run.
	rows := f.audit(controltest.AuditFilter{Plan: id})
	var kinds []string
	for _, e := range rows {
		kinds = append(kinds, e.Kind+":"+e.Op+":"+string(e.Decision))
	}
	all := strings.Join(kinds, " ")
	for _, want := range []string{"apply-finished:apply:none", "request:apply:allowed", "request:approve:allowed", "request:apply:denied"} {
		if !strings.Contains(all, want) {
			t.Errorf("no audit row %s in %s", want, all)
		}
	}
}

func runRequestBody(s string) func() *bytes.Reader {
	return func() *bytes.Reader { return bytes.NewReader([]byte(s)) }
}

// An apply after a merge whose sources differ from every approved plan's
// is refused; of two approved plans with the same sources, the newest
// applies, and the run supersedes the other.
func TestSourceMatch(t *testing.T) {
	f := planFixture(t, withApprovals(t))
	f.addInfra()
	approve := func(pipeline string) {
		t.Helper()
		r := f.do("POST", "/v1/approvals", bob(f, map[string]any{"pipeline_id": pipeline}), runRequestBody(`{"format":"writ.approve-request/v0","pipeline":true}`)())
		if r.status != http.StatusOK {
			t.Fatalf("approve %s: %d %s", pipeline, r.status, r.body)
		}
	}
	files := changed("resource \"random_pet\" \"pet2\" {}\n")
	a := f.mrPlan(f.gitlab.Commit(4242, "mr-a", files), "mr-a", "network", "100")
	approve("100")
	// A merge whose included file differs from the plan's.
	other := changed("resource \"random_pet\" \"pet2\" {}\n")
	other["infra/platform/main.tf"] = forgetest.File{Content: "output \"region\" {\n  value = \"eu-west-1\"\n}\n"}
	m1 := f.gitlab.Commit(4242, "main", other)
	r := f.do("POST", "/v1/runs", mainJob(f, m1), mergedRequest("network"))
	if r.status != http.StatusConflict || r.class() != "saved-plan" || !strings.Contains(string(r.body), "no approved plan") {
		t.Fatalf("a changed included file: %d %s", r.status, r.body)
	}
	b := f.mrPlan(f.gitlab.Commit(4242, "mr-b", files), "mr-b", "network", "200")
	approve("200")
	m2 := f.gitlab.Commit(4242, "main", files)
	main := mainJob(f, m2)
	r = f.do("POST", "/v1/runs", main, mergedRequest("network"))
	if r.status != http.StatusAccepted || r.doc["run"].(map[string]any)["plan"] != b.Plan.ID {
		t.Fatalf("the newest equal plan: %d %s", r.status, r.body)
	}
	f.waitRun(main, r.doc["run"].(map[string]any)["id"].(string))
	if got := f.waitPlan(main, a.Plan.ID); got.Plan.Status != "superseded" || !strings.Contains(got.Plan.SupersededWhy, b.Plan.ID) {
		t.Fatalf("the older plan: %+v", got.Plan)
	}
	// A job on a branch other than main may not apply after a merge, and a
	// commit not on main is not merged.
	notMain := f.token(map[string]any{"sha": m2, "ref_protected": "true"})
	if r := f.do("POST", "/v1/runs", notMain, mergedRequest("network")); r.status != http.StatusForbidden {
		t.Fatalf("a branch job applied after a merge: %d %s", r.status, r.body)
	}
	branchOnly := f.gitlab.Commit(4242, "mr-c", changed("resource \"random_pet\" \"pet3\" {}\n"))
	if r := f.do("POST", "/v1/runs", mainJob(f, branchOnly), mergedRequest("network")); r.status != http.StatusForbidden || !strings.Contains(string(r.body), "not merged") {
		t.Fatalf("a commit not on main: %d %s", r.status, r.body)
	}
}

// tickClock is a server clock a test moves by hand.
type tickClock struct{ at atomic.Int64 }

// now moves the clock a millisecond each time it is read, as a real
// clock would between two requests.
func (c *tickClock) now() time.Time             { return time.Unix(0, c.at.Add(int64(time.Millisecond))) }
func (c *tickClock) add(d time.Duration)        { c.at.Add(int64(d)) }
func newClock(t time.Time) *tickClock           { c := &tickClock{}; c.at.Store(t.UnixNano()); return c }
func (c *tickClock) set() func(*server.Options) { return func(o *server.Options) { o.Now = c.now } }

// An apply by plan id is refused when the plan's branch moved to sources
// that differ, or is gone, or the plan expired; a branch that moved with
// no change to the stack's sources has not moved for the plan.
func TestStaleByPlanID(t *testing.T) {
	c := newClock(time.Now())
	// The merge request pipeline may apply prod: apply before the merge.
	mrApply := func(text string) string {
		return strings.Replace(text, "grant {", `grant {
  teams = ["infra_mr"]
  stacks = ["*"]
  envs = ["prod"]
  can = ["apply"]
  risks = ["create"]
}

grant {`, 1)
	}
	f := planFixture(t, withApprovals(t, mrApply), c.set())
	f.addInfra()
	plan := func(branch, pipeline string, files map[string]forgetest.File) string {
		t.Helper()
		p := f.mrPlan(f.gitlab.Commit(4242, branch, files), branch, "network", pipeline)
		if r := f.do("POST", "/v1/plans/"+p.Plan.ID+"/approvals", bob(f, map[string]any{"pipeline_id": pipeline}), nil); r.status != http.StatusOK {
			t.Fatalf("approve: %d %s", r.status, r.body)
		}
		return p.Plan.ID
	}
	apply := func(id, pipeline string) response {
		return f.do("POST", "/v1/runs", f.token(map[string]any{"pipeline_id": pipeline}), runRequest(map[string]any{"plan": id}))
	}

	moved := plan("moves", "10", changed("resource \"random_pet\" \"a\" {}\n"))
	f.gitlab.Commit(4242, "moves", changed("resource \"random_pet\" \"b\" {}\n"))
	if r := apply(moved, "10"); r.status != http.StatusConflict || r.class() != "saved-plan" || !strings.Contains(string(r.body), "moved to") {
		t.Fatalf("a moved branch: %d %s", r.status, r.body)
	}
	if got := f.waitPlan(f.token(nil), moved); got.Plan.Status != "superseded" {
		t.Fatalf("the plan of a moved branch is %s", got.Plan.Status)
	}

	gone := plan("gone", "11", changed("resource \"random_pet\" \"c\" {}\n"))
	f.gitlab.DeleteBranch(4242, "gone")
	if r := apply(gone, "11"); r.status != http.StatusConflict || !strings.Contains(string(r.body), "is gone") {
		t.Fatalf("a deleted branch: %d %s", r.status, r.body)
	}

	files := changed("resource \"random_pet\" \"d\" {}\n")
	same := plan("readme", "12", files)
	files["README.md"] = forgetest.File{Content: "a new readme\n"}
	f.gitlab.Commit(4242, "readme", files)
	if r := apply(same, "12"); r.status != http.StatusAccepted {
		t.Fatalf("a branch that moved with the same sources: %d %s", r.status, r.body)
	} else {
		f.waitRun(f.token(nil), r.doc["run"].(map[string]any)["id"].(string))
	}

	old := plan("old", "13", changed("resource \"random_pet\" \"e\" {}\n"))
	c.add(25 * time.Hour)
	if r := apply(old, "13"); r.status != http.StatusConflict || !strings.Contains(string(r.body), "expired") {
		t.Fatalf("an expired plan: %d %s", r.status, r.body)
	}
	if got := f.waitPlan(f.token(nil), old); got.Plan.Status != "expired" {
		t.Fatalf("the expired plan is %s", got.Plan.Status)
	}

	// A CI job gives no override, and applies by id only its own
	// pipeline's plan.
	other := plan("other", "14", changed("resource \"random_pet\" \"f\" {}\n"))
	if r := f.do("POST", "/v1/runs", f.token(map[string]any{"pipeline_id": "14"}), runRequest(map[string]any{"plan": other, "override": "bob is on leave, OPS-112"})); r.status != http.StatusForbidden || !strings.Contains(string(r.body), "an override is a person's") {
		t.Fatalf("a CI override: %d %s", r.status, r.body)
	}
	if r := apply(other, "15"); r.status != http.StatusForbidden || !strings.Contains(string(r.body), "its own pipeline") {
		t.Fatalf("another pipeline's plan: %d %s", r.status, r.body)
	}
}

// An approval of a pipeline is all or none: a plan the approver may not
// approve refuses the request, and nothing is recorded.
func TestPipelineApprovalAllOrNone(t *testing.T) {
	narrow := func(text string) string {
		return strings.Replace(text, "teams = [\"reviewers\"]\n  stacks = [\"*\"]\n  envs = [\"prod\"]\n  can = [\"approve\"]", "teams = [\"reviewers\"]\n  stacks = [\"network\"]\n  envs = [\"prod\"]\n  can = [\"approve\"]", 1)
	}
	f := planFixture(t, withApprovals(t, narrow))
	f.addInfra()
	files := changed("resource \"random_pet\" \"pet2\" {}\n")
	files["infra/database/main.tf"] = forgetest.File{Content: files["infra/database/main.tf"].Content + "resource \"random_id\" \"id2\" {\n  byte_length = 4\n}\n"}
	sha := f.gitlab.Commit(4242, "both", files)
	n := f.mrPlan(sha, "both", "network", "300")
	f.mrPlan(sha, "both", "database", "300")
	r := f.do("POST", "/v1/approvals", bob(f, map[string]any{"pipeline_id": "300"}), runRequestBody(`{"format":"writ.approve-request/v0","pipeline":true}`)())
	if r.status != http.StatusForbidden || !strings.Contains(string(r.body), "database") {
		t.Fatalf("approved part of a pipeline: %d %s", r.status, r.body)
	}
	if got := f.waitPlan(f.token(nil), n.Plan.ID); got.Plan.Status != "pending" || len(got.Plan.Approvals.ApprovedBy) != 0 {
		t.Fatalf("network after a refused approval: %+v", got.Plan)
	}
}

// devServer is a dev control plane with an approval rule, which no config
// of writ serve --dev sets, for the paths only a person takes. r stands in
// for OpenTofu, and repository infra is acme/infra on the forge.
func devServer(t *testing.T, r *fakeTofu, edits ...func(*server.Config)) (*fixture, string) {
	return devServerWithOptions(t, r, nil, edits...)
}

func devServerWithOptions(t *testing.T, r *fakeTofu, setup func(*server.Options), edits ...func(*server.Config)) (*fixture, string) {
	t.Helper()
	ctx := context.Background()
	is := oidctest.New(t)
	gl := forgetest.Handler(gitlabToken)
	gl.URL = is.URL
	is.Mount("/api/v4/", gl)
	data := t.TempDir()
	ca, tokenFile := filepath.Join(data, "issuer.pem"), filepath.Join(data, "gitlab_read")
	if err := os.WriteFile(ca, is.CAPEM(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, []byte(gitlabToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := control.Open(ctx, control.Options{Location: filepath.Join(data, "control.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	cfg := server.DevConfig(server.DevOptions{URL: "http://127.0.0.1:1", Listen: "127.0.0.1:1", Database: filepath.Join(data, "control.db"), Data: data})
	cfg.Forges = []server.Forge{{Name: "gitlab", Kind: control.GitLab, URL: is.URL, ReadTokenFile: tokenFile, CAFile: ca}}
	cfg.Policy.Approvals = []authz.ApprovalRule{{Stacks: []string{"*"}, Envs: []string{"*"}, Risks: []string{"create"}, Need: 1}}
	for _, edit := range edits {
		edit(cfg)
	}
	if r.plan == nil {
		r.plan = createOnly
	}
	token := strings.Repeat("d", 40)
	opts := server.Options{Config: cfg, Store: store, Tofu: r, DevToken: token, DevUser: "karan", Version: "test"}
	if setup != nil {
		setup(&opts)
	}
	srv, err := server.New(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { h.Close(); srv.Close() })
	r.base = h.URL
	f := &fixture{t: t, is: is, gitlab: gl, http: h, srv: srv, store: store, tofu: r, cfg: cfg}
	gl.AddProject(4242, "acme/infra", "main")
	gl.Commit(4242, "main", infraFiles())
	if res := f.do("POST", "/v1/repos", token, repoRequest(nil)); res.status != http.StatusCreated {
		t.Fatalf("add: %d %s", res.status, res.body)
	}
	return f, token
}

// devPlan plans network at the head of main on a dev control plane, and
// returns the plan once it ends.
func (f *fixture) devPlan(tok string) server.RemotePlan {
	f.t.Helper()
	sha := f.gitlab.Commit(4242, "main", infraFiles())
	p := f.do("POST", "/v1/plans", tok, planRequest(func(m map[string]any) {
		m["env"] = ""
		m["source"] = map[string]any{"repo": "infra", "commit": sha}
	}))
	if p.status != http.StatusAccepted {
		f.t.Fatalf("plan: %d %s", p.status, p.body)
	}
	return f.waitPlan(tok, p.doc["plan"].(map[string]any)["ref"].(string))
}

// keptSpeculative stores a pending speculative plan of network@prod by
// alice, of files a client sent, as writ serve kept one before it planned
// only commits, and returns its id.
func keptSpeculative(t *testing.T, f *fixture) string {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	cfgID, err := f.store.RecordConfig(ctx, f.cfg.Text, f.cfg.URL, "test", &f.cfg.Policy, now)
	if err != nil {
		t.Fatal(err)
	}
	e := control.Entry{At: now, Kind: "test", Decision: control.Allowed, ConfigID: cfgID}
	pl := control.Plan{Ref: "SPECULATIVE", Env: "prod", Stack: "network", ConfigID: cfgID, RequestedAt: now, SourceDigest: "digest",
		Author: control.Principal{Issuer: "gitlab", Person: "gitlab/user/1001", Display: "alice", Detail: json.RawMessage(`{"kind":"person"}`)},
		Origin: json.RawMessage(`{}`)}
	if err := f.store.CreatePlan(ctx, pl, control.TokenUse{}, e); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("5", 32)
	res := control.PlanResult{ID: id, Status: control.Pending, MadeAt: now, ExpiresAt: now.Add(time.Hour), Risks: []string{"create"}, Changes: 1, Need: 1, Saved: []byte("saved")}
	if err := f.store.FinishPlan(ctx, pl.Ref, res, e); err != nil {
		t.Fatal(err)
	}
	return id
}

// No control plane takes an approval of a speculative plan, a dev one
// included, and the UI offers none: it is refused as a drift plan is, and
// stores no approval.
func TestSpeculativePlanTakesNoApproval(t *testing.T) {
	controltest.Each(t, func(t *testing.T, e controltest.Engine) {
		store, _ := controltest.Open(t, e)
		dev, devTok := devServerWithOptions(t, &fakeTofu{}, func(o *server.Options) { o.Store = store })
		dev.store = store
		team := tofuFixtureOn(t, e, &fakeTofu{plan: createOnly}, uidLines(t), withApprovals(t))
		for _, tt := range []struct {
			name string
			f    *fixture
			tok  string
		}{
			{"dev", dev, devTok},
			{"team", team, bob(team)},
		} {
			t.Run(tt.name, func(t *testing.T) {
				id := keptSpeculative(t, tt.f)
				r := tt.f.do("POST", "/v1/plans/"+id+"/approvals", tt.tok, nil)
				if r.status != http.StatusConflict || r.class() != "state" || !strings.Contains(string(r.body), "plan "+id+" is a speculative plan, of files a client sent, which writ serve never approves") {
					t.Errorf("approve a speculative plan: %d %s", r.status, r.body)
				}
				got, err := tt.f.store.Approvals(context.Background(), []string{id})
				if err != nil || len(got[id]) != 0 {
					t.Errorf("approvals of the speculative plan: %+v, %v", got[id], err)
				}
			})
		}
	})
}

// An override applies a plan without its approvals, for a person with
// override, with a reason; it is refused for a plan that has them.
func TestOverride(t *testing.T) {
	r := &fakeTofu{}
	f, tok := devServer(t, r)
	plan := f.devPlan(tok)
	id := plan.Plan.ID
	if plan.Plan.Status != "pending" {
		t.Fatalf("plan %+v", plan.Plan)
	}
	if r := f.do("POST", "/v1/plans/"+id+"/approvals", tok, nil); r.status != http.StatusForbidden || !strings.Contains(string(r.body), "cannot approve it") {
		t.Fatalf("self-approval: %d %s", r.status, r.body)
	}
	if r := f.do("POST", "/v1/runs", tok, runRequest(map[string]any{"plan": id})); r.status != http.StatusForbidden || !strings.Contains(string(r.body), "0 of the 1 approval its rules need") {
		t.Fatalf("applied with no approval: %d %s", r.status, r.body)
	}
	if r := f.do("POST", "/v1/runs", tok, runRequest(map[string]any{"plan": id, "override": "too short"})); r.status != http.StatusBadRequest {
		t.Fatalf("a short override: %d %s", r.status, r.body)
	}
	reason := "bob on leave, rotate leaked key, OPS-112"
	res := f.do("POST", "/v1/runs", tok, runRequest(map[string]any{"plan": id, "override": reason}))
	if res.status != http.StatusAccepted {
		t.Fatalf("override: %d %s", res.status, res.body)
	}
	run := f.waitRun(tok, res.doc["run"].(map[string]any)["id"].(string))
	if run.Run.Status != "applied" || run.Run.Override != reason || len(run.Run.ApprovedBy) != 0 {
		t.Fatalf("run %+v", run.Run)
	}
	if applied := r.appliedSoFar(); len(applied) != 1 {
		t.Fatalf("%d applies ran", len(applied))
	}
}

// Requests that race to apply one plan, or two plans of one stack@env,
// begin one run at most.
func TestApplyRace(t *testing.T) {
	gate := make(chan struct{})
	r := &fakeTofu{apply: func(context.Context, stateCall) ([]byte, error) { <-gate; return nil, nil }}
	f, tok := devServer(t, r)
	var ids []string
	for range 2 {
		ids = append(ids, f.devPlan(tok).Plan.ID)
	}
	var wg sync.WaitGroup
	var began atomic.Int32
	var statuses sync.Map
	for i := range 8 {
		wg.Go(func() {
			res := f.do("POST", "/v1/runs", tok, runRequest(map[string]any{"plan": ids[i%2], "override": "a race test, ticket OPS-1"}))
			statuses.Store(i, fmt.Sprintf("%d %s", res.status, res.body))
			if res.status == http.StatusAccepted {
				began.Add(1)
			}
		})
	}
	wg.Wait()
	close(gate)
	if began.Load() != 1 {
		var all []string
		statuses.Range(func(k, v any) bool { all = append(all, fmt.Sprint(v)); return true })
		t.Fatalf("%d applies began: %v", began.Load(), all)
	}
}

// A control plane that stops during an apply fails the run at its next
// start, and supersedes the plans that waited on the stack@env.
func TestRestartFailsApplying(t *testing.T) {
	gate := make(chan struct{})
	defer close(gate)
	f := planFixture(t, withApprovals(t))
	f.tofu.apply = func(context.Context, stateCall) ([]byte, error) { <-gate; return nil, nil }
	f.addInfra()
	files := changed("resource \"random_pet\" \"pet2\" {}\n")
	sha := f.gitlab.Commit(4242, "mr", files)
	p := f.mrPlan(sha, "mr", "network", "400")
	if res := f.do("POST", "/v1/plans/"+p.Plan.ID+"/approvals", bob(f, map[string]any{"pipeline_id": "400"}), nil); res.status != http.StatusOK {
		t.Fatalf("approve: %d %s", res.status, res.body)
	}
	merged := f.gitlab.Commit(4242, "main", files)
	res := f.do("POST", "/v1/runs", mainJob(f, merged), mergedRequest("network"))
	if res.status != http.StatusAccepted {
		t.Fatalf("apply: %d %s", res.status, res.body)
	}
	runID := res.doc["run"].(map[string]any)["id"].(string)
	// A second process starts on the same database while the first one's
	// run is held: the first one counts as stopped.
	again, err := server.New(context.Background(), server.Options{Config: f.cfg, Store: f.store, Tofu: &fakeTofu{}, Reap: func(uint32) error { return nil }, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	run, err := f.store.GetRun(context.Background(), runID)
	if err != nil || run.Status != control.RunFailed || !strings.Contains(run.Text, "stopped during this run") {
		t.Fatalf("after a restart: %+v, %v", run, err)
	}
	if got, _ := f.store.Plan(context.Background(), p.Plan.ID, time.Now()); got.Status != control.Failed {
		t.Fatalf("the plan is %s", got.Status)
	}
}

// Malformed and hostile requests to the new routes are refused with the
// route's own document, and change nothing.
func TestApplyRoutesRefuseBadInput(t *testing.T) {
	f, tok := devServer(t, &fakeTofu{})
	id := f.devPlan(tok).Plan.ID
	big := append(append([]byte(`{"format":"`), bytes.Repeat([]byte("a"), 17<<20)...), `"}`...)
	for _, tc := range []struct {
		name, method, path string
		body               []byte
		status             int
		format, want       string
	}{
		{"an unknown field", "POST", "/v1/runs", []byte(`{"format":"writ.run-request/v1","plan":"` + id + `","force":true}`), 400, "writ.remote-run/v0", "unknown field"},
		{"another format", "POST", "/v1/runs", []byte(`{"format":"writ.run-request/v0","plan":"` + id + `"}`), 400, "writ.remote-run/v0", "not writ.run-request/v1"},
		{"plan and merged", "POST", "/v1/runs", []byte(`{"format":"writ.run-request/v1","plan":"` + id + `","merged":{}}`), 400, "writ.remote-run/v0", "not both"},
		{"neither", "POST", "/v1/runs", []byte(`{"format":"writ.run-request/v1"}`), 400, "writ.remote-run/v0", "a run needs plan"},
		{"a path in the plan id", "POST", "/v1/runs", []byte(`{"format":"writ.run-request/v1","plan":"../../etc/passwd"}`), 404, "writ.remote-run/v0", "not a plan id"},
		{"an unknown plan", "POST", "/v1/runs", []byte(`{"format":"writ.run-request/v1","plan":"` + strings.Repeat("0", 32) + `"}`), 404, "writ.remote-run/v0", "no plan"},
		{"two stacks after a merge", "POST", "/v1/runs", []byte(`{"format":"writ.run-request/v1","env":"dev","stacks":["a","b"],"merged":{"repo":"x","commit":"` + strings.Repeat("a", 40) + `"}}`), 400, "writ.remote-run/v0", "exactly one stack"},
		{"a merged commit that is not an id", "POST", "/v1/runs", []byte(`{"format":"writ.run-request/v1","env":"dev","stacks":["a"],"merged":{"repo":"x","commit":"HEAD"}}`), 400, "writ.remote-run/v0", "full id"},
		{"an override after a merge", "POST", "/v1/runs", []byte(`{"format":"writ.run-request/v1","env":"dev","stacks":["a"],"override":"0123456789ab","merged":{}}`), 400, "writ.remote-run/v0", "override names a plan"},
		{"an override too long", "POST", "/v1/runs", []byte(`{"format":"writ.run-request/v1","plan":"` + id + `","override":"` + strings.Repeat("x", 501) + `"}`), 400, "writ.remote-run/v0", "10 to 500"},
		{"a body over the limit", "POST", "/v1/runs", big, 413, "writ.remote-run/v0", "over"},
		{"a run id that is not one", "GET", "/v1/runs/..%2Fplans", nil, 404, "writ.remote-run/v0", "not a run id"},
		{"an unknown run", "GET", "/v1/runs/" + strings.Repeat("A", 26), nil, 404, "writ.remote-run/v0", "no run"},
		{"an approval of a ref", "POST", "/v1/plans/" + strings.Repeat("A", 26) + "/approvals", nil, 404, "writ.approve/v0", "not a plan id"},
		{"an approval body of another format", "POST", "/v1/plans/" + id + "/approvals", []byte(`{"format":"writ.approve/v0"}`), 400, "writ.approve/v0", "not writ.approve-request/v0"},
		{"a pipeline approval without a CI token", "POST", "/v1/approvals", []byte(`{"format":"writ.approve-request/v0","pipeline":true}`), 400, "writ.approve/v0", "only a CI job"},
		{"an approvals body without pipeline", "POST", "/v1/approvals", []byte(`{"format":"writ.approve-request/v0"}`), 400, "writ.approve/v0", "pipeline: true"},
		{"a withdrawal with no approval", "DELETE", "/v1/plans/" + id + "/approvals/mine", nil, 404, "writ.approve/v0", "no approval of plan"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body *bytes.Reader
			if tc.body != nil {
				body = bytes.NewReader(tc.body)
			} else {
				body = bytes.NewReader(nil)
			}
			r := f.do(tc.method, tc.path, tok, body)
			if r.status != tc.status || r.doc["format"] != tc.format || r.doc["ok"] != false || !strings.Contains(string(r.body), tc.want) {
				t.Fatalf("got %d %s", r.status, r.body)
			}
		})
	}
	if got := f.waitPlan(tok, id); got.Plan.Status != "pending" || got.Plan.Run != "" {
		t.Fatalf("a refused request changed the plan: %+v", got.Plan)
	}
}

// A project, pipeline, or repository id is unique only within one GitLab
// instance, or one GitHub, so a CI job owns a plan only when the plan came
// from its own issuer. A job of a second GitLab whose project and pipeline
// ids equal the plan's approves, applies, and finds nothing, and a GitHub
// job, whose token names no project_id, owns no plan of a person.
func TestCIOwnershipNamesTheIssuer(t *testing.T) {
	other, actions := oidctest.New(t), oidctest.New(t)
	dir := t.TempDir()
	caOf := func(is *oidctest.Issuer, name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, is.CAPEM(), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	edit := func(s string) string {
		s += fmt.Sprintf(`
issuer "other" {
  kind = "gitlab-ci"
  identity = "other"
  url = %q
  ca_file = %q
}

issuer "actions" {
  kind = "github-actions"
  identity = "github-enterprise"
  url = %q
  ca_file = %q
}

team "other_reviewers" {
  issuer = "other"
  match = { project_id = ["4242"], user_access_level = ["maintainer"] }
}

team "other_main" {
  issuer = "other"
  match = { project_id = ["4242"], ref_path = ["refs/heads/main"] }
}

team "actions_reviewers" {
  issuer = "actions"
  match = { repository_owner = ["acme"], actor_id = ["2002"] }
}
`, other.URL, caOf(other, "other.pem"), actions.URL, caOf(actions, "actions.pem"))
		return strings.Replace(s, "grant {", `grant {
  teams = ["other_reviewers", "actions_reviewers"]
  stacks = ["*"]
  envs = ["prod"]
  can = ["read", "approve", "apply"]
  risks = ["create"]
}

grant {
  teams = ["other_main"]
  stacks = ["*"]
  envs = ["prod"]
  can = ["read", "apply"]
  risks = ["create"]
}

grant {`, 1)
	}
	f := planFixture(t, withApprovals(t, edit))
	f.addInfra()
	files := changed("resource \"random_pet\" \"pet2\" {}\n")
	sha := f.gitlab.Commit(4242, "mr", files)
	p := f.mrPlan(sha, "mr", "network", "1849213")

	otherJob := func(edits map[string]any) string {
		c := oidctest.GitLabJob(other.URL, audience, time.Now())
		c["user_id"], c["user_login"], c["user_access_level"] = "2002", "bob", "maintainer"
		for k, v := range edits {
			c[k] = v
		}
		return oidctest.Sign(other.Keys()[0], c)
	}
	if r := f.do("POST", "/v1/plans/"+p.Plan.ID+"/approvals", otherJob(nil), nil); r.status != http.StatusForbidden || !strings.Contains(string(r.body), "own pipeline") {
		t.Errorf("another GitLab's job approved by id: %d %s", r.status, r.body)
	}
	if r := f.do("POST", "/v1/approvals", otherJob(nil), runRequestBody(`{"format":"writ.approve-request/v0","pipeline":true}`)()); r.status != http.StatusNotFound {
		t.Errorf("another GitLab's job approved its pipeline: %d %s", r.status, r.body)
	}
	if r := f.do("POST", "/v1/runs", otherJob(nil), runRequest(map[string]any{"plan": p.Plan.ID})); r.status != http.StatusForbidden || !strings.Contains(string(r.body), "own pipeline") {
		t.Errorf("another GitLab's job applied by id: %d %s", r.status, r.body)
	}

	// The plan's own pipeline approves it, and after the merge only this
	// GitLab's job on main applies it.
	if r := f.do("POST", "/v1/plans/"+p.Plan.ID+"/approvals", bob(f), nil); r.status != http.StatusOK {
		t.Fatalf("bob: %d %s", r.status, r.body)
	}
	merged := f.gitlab.Commit(4242, "main", files)
	otherMain := otherJob(map[string]any{"sha": merged, "ref": "main", "ref_path": "refs/heads/main", "pipeline_id": "1849290", "job_id": "9922150"})
	if r := f.do("POST", "/v1/runs", otherMain, mergedRequest("network")); r.status != http.StatusForbidden || !strings.Contains(string(r.body), "has no repository") {
		t.Errorf("another GitLab's job on main applied after the merge: %d %s", r.status, r.body)
	}

	// A person's commit plan names no CI run, so no CI job owns it.
	sess := &server.Session{ID: "S1", Display: "carol", Person: "gitlab/user/3003", Issuer: "sso", Subject: "3003", Teams: []string{"infra_mr"}, EndsAt: time.Now().Add(time.Hour)}
	body, _ := io.ReadAll(commitPlan(map[string]any{"repo": "infra", "commit": sha, "ref": "refs/heads/mr"}))
	status, out := f.srv.Post(httptest.NewRequest("POST", "/ui/", nil), sess, "/v1/plans", body)
	var started server.RemotePlan
	if err := json.Unmarshal(out, &started); status != http.StatusAccepted || err != nil || started.Plan == nil {
		t.Fatalf("carol's plan: %d %s", status, out)
	}
	mine := f.waitPlan(f.token(nil), started.Plan.Ref)
	gh := oidctest.Sign(actions.Keys()[0], oidctest.GitHubJob(actions.URL, audience, time.Now()))
	// The person is named by the issuer's identity, not by its host.
	if r := f.do("GET", "/v1/whoami", gh, nil); r.status != http.StatusOK || !strings.Contains(string(r.body), `"person":"github-enterprise/user/2002"`) {
		t.Errorf("the GitHub job's person: %d %s", r.status, r.body)
	}
	if r := f.do("POST", "/v1/plans/"+mine.Plan.ID+"/approvals", gh, nil); r.status != http.StatusForbidden || !strings.Contains(string(r.body), "own pipeline") {
		t.Errorf("a GitHub job approved a person's plan: %d %s", r.status, r.body)
	}
	if r := f.do("POST", "/v1/runs", gh, runRequest(map[string]any{"plan": mine.Plan.ID})); r.status != http.StatusForbidden || !strings.Contains(string(r.body), "own pipeline") {
		t.Errorf("a GitHub job applied a person's plan: %d %s", r.status, r.body)
	}
}

// A person id holds the identity of its issuer, and the self-approval
// check compares ids, so neither side may hold an id of an identity the
// config does not define: a session from an older build, whose id held a
// host, or a plan made under a config that named its author another way.
// The author under the config's identity is refused as the author.
func TestApprovalNeedsKnownIdentity(t *testing.T) {
	f := planFixture(t, withApprovals(t))
	f.addInfra()
	sha := f.gitlab.Commit(4242, "mr", changed("resource \"random_pet\" \"pet2\" {}\n"))
	p := f.mrPlan(sha, "mr", "network", "1849213")
	if p.Plan.Author.Person != "gitlab/user/1001" {
		t.Fatalf("alice's job names person %q, want gitlab/user/1001", p.Plan.Author.Person)
	}
	approve := func(person string) (int, string) {
		sess := &server.Session{ID: "S1", Display: "someone", Person: person, Issuer: "sso", Subject: "x", Teams: []string{"reviewers"}, EndsAt: time.Now().Add(time.Hour)}
		status, out := f.srv.Post(httptest.NewRequest("POST", "/ui/", nil), sess, "/v1/plans/"+p.Plan.ID+"/approvals", nil)
		return status, string(out)
	}
	if status, out := approve("gitlab.com/user/2002"); status != http.StatusForbidden || !strings.Contains(out, "of no identity the config defines: sign in again") {
		t.Errorf("a session of a host-named person approved: %d %s", status, out)
	}
	if status, out := approve("gitlab/user/1001"); status != http.StatusForbidden || !strings.Contains(out, "cannot approve it") {
		t.Errorf("alice approved her own plan with a session: %d %s", status, out)
	}

	raw := controltest.Raw(t, f.loc)
	if _, err := raw.ExecContext(context.Background(), "UPDATE plans SET author_person = ? WHERE id = ?", "GITLAB.example.com:443/user/1001", p.Plan.ID); err != nil {
		t.Fatal(err)
	}
	if status, out := approve("gitlab/user/2002"); status != http.StatusConflict || !strings.Contains(out, "of no identity the config defines, so its author cannot be told from an approver: plan again") {
		t.Errorf("a plan of a host-named author was approved: %d %s", status, out)
	}
	if r := f.do("POST", "/v1/plans/"+p.Plan.ID+"/approvals", bob(f), nil); r.status != http.StatusConflict {
		t.Errorf("bob's job approved a plan of a host-named author: %d %s", r.status, r.body)
	}
}

// An approval kept under an identity the config no longer defines counts
// nowhere. Two approvals are needed and bob gives one as review-old; the
// config renames that issuer's identity to review-new, and the server
// restarts. Bob's second approval, as review-new/user/2002, is the only
// one that counts, so the plan stays pending and the merged apply is
// refused until a second person approves.
func TestRenamedIdentityCountsOnce(t *testing.T) {
	reviewer := oidctest.New(t)
	f := planFixture(t, withApprovals(t, func(s string) string {
		s = strings.ReplaceAll(s, "need = 1", "need = 2")
		s += fmt.Sprintf(`
issuer "review_sso" {
  kind = "people"
  identity = "review-old"
  url = %q
  client_id = "review-client"
  client_secret_file = "synthetic-secret"
}

team "review_people" {
  issuer = "review_sso"
  match = { groups = ["reviewers"] }
}
`, reviewer.URL)
		return strings.Replace(s, "grant {", `grant {
  teams = ["review_people"]
  stacks = ["*"]
  envs = ["prod"]
  can = ["approve"]
}

grant {`, 1)
	}))
	f.addInfra()
	files := changed("resource \"random_pet\" \"pet2\" {}\n")
	sha := f.gitlab.Commit(4242, "mr", files)
	p := f.mrPlan(sha, "mr", "network", "1849213")
	approve := func(identity, id string) server.ApproveDoc {
		t.Helper()
		sess := &server.Session{ID: identity + id, Display: "user " + id, Person: identity + "/user/" + id, Issuer: "review_sso", Subject: id, Teams: []string{"review_people"}, EndsAt: time.Now().Add(time.Hour)}
		status, body := f.srv.Post(httptest.NewRequest("POST", "/ui/", nil), sess, "/v1/plans/"+p.Plan.ID+"/approvals", nil)
		var doc server.ApproveDoc
		if err := json.Unmarshal(body, &doc); status != http.StatusOK || err != nil || len(doc.Plans) != 1 {
			t.Fatalf("approval of %s/user/%s: %d %s", identity, id, status, body)
		}
		return doc
	}
	if got := approve("review-old", "2002").Plans[0]; got.Status != "pending" || len(got.ApprovedBy) != 1 {
		t.Fatalf("bob's first approval: %+v", got)
	}

	f.srv.Close()
	next, diags := server.ParseConfig(filepath.Join(f.dir, "serve"), []byte(strings.ReplaceAll(f.cfg.Text, "review-old", "review-new")))
	if diags.HasErrors() {
		t.Fatal(diags)
	}
	f.cfg = next
	verifier, err := server.NewVerifier(context.Background(), f.cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.srv, err = server.New(context.Background(), server.Options{Config: f.cfg, Store: f.store, Verifier: verifier, Tofu: f.tofu, Reap: func(uint32) error { return nil }, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.srv.Close)

	got := approve("review-new", "2002").Plans[0]
	if got.Status != "pending" || len(got.ApprovedBy) != 1 || got.ApprovedBy[0].Person != "review-new/user/2002" || got.Need != 2 {
		t.Fatalf("bob counted twice under two identities: %+v", got)
	}
	mergedApply := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/v1/runs", mergedRequest("network"))
		req.Header.Set("Authorization", "Bearer "+mainJob(f, f.gitlab.Commit(4242, "main", files)))
		rec := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(rec, req)
		return rec
	}
	if rec := mergedApply(); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "needs 2 approvals;") {
		t.Fatalf("the merged apply with one reviewer: %d %s", rec.Code, rec.Body)
	}
	stored, err := f.store.Approvals(context.Background(), []string{p.Plan.ID})
	if err != nil || len(stored[p.Plan.ID]) != 2 {
		t.Fatalf("the approval under review-old is no longer kept: %+v, %v", stored, err)
	}

	if got := approve("review-new", "3003").Plans[0]; got.Status != "approved" || len(got.ApprovedBy) != 2 {
		t.Fatalf("a second reviewer's approval: %+v", got)
	}
	if rec := mergedApply(); rec.Code != http.StatusAccepted {
		t.Fatalf("the merged apply with two reviewers: %d %s", rec.Code, rec.Body)
	}
}

// TestApplyGrantBeforeForge checks that an apply by a principal with no
// apply grant on the stack@env is refused before writ serve calls the
// forge with its token: neither an apply after a merge nor one by a plan's
// id reads anything of the repository.
func TestApplyGrantBeforeForge(t *testing.T) {
	f := planFixture(t, withApprovals(t))
	f.addInfra()
	tok := f.mrToken(changed("resource \"random_pet\" \"pet2\" {}\n"), nil)
	r := f.do("POST", "/v1/plans", tok, commitPlan(map[string]any{}))
	if r.status != http.StatusAccepted {
		t.Fatalf("plan: %d %s", r.status, r.body)
	}
	id := f.waitPlan(tok, r.doc["plan"].(map[string]any)["ref"].(string)).Plan.ID
	merged := f.gitlab.Commit(4242, "main", changed("resource \"random_pet\" \"pet2\" {}\n"))
	for _, tc := range []struct {
		name, tok string
		body      *bytes.Reader
		want      string
	}{
		{"after a merge", deniedMainJob(f, merged), mergedRequest("network"), "may not apply to network@prod"},
		{"by id", f.token(nil), runRequest(map[string]any{"plan": id}), "may not apply plan " + id},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.gitlab.Calls()
			r := f.do("POST", "/v1/runs", tc.tok, tc.body)
			if r.status != http.StatusForbidden || r.class() != "denied" || !strings.Contains(string(r.body), tc.want) {
				t.Fatalf("got %d %s", r.status, r.body)
			}
			if calls := f.gitlab.Calls(); len(calls) != 0 {
				t.Fatalf("the refused apply called the forge: %v", calls)
			}
		})
	}
	// Each refusal's audit row names its target, so a team that reads the
	// stack sees the denied apply.
	rows, _ := f.auditAll(f.token(nil), nil)
	denied := 0
	for _, row := range rows {
		if row.Method == "POST" && row.Route == "/v1/runs" && row.Status == http.StatusForbidden {
			denied++
			if row.Env != "prod" || len(row.Stacks) != 1 {
				t.Fatalf("a denied apply's audit row names %q %v", row.Env, row.Stacks)
			}
		}
	}
	if denied != 2 {
		t.Fatalf("%d denied apply rows, want 2", denied)
	}
}
