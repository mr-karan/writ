package server_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/mr-karan/writ/internal/control/controltest"
	"github.com/mr-karan/writ/internal/server"
	"github.com/mr-karan/writ/internal/tofu"
)

// tofuPlanJSONOf is a plan of OpenTofu with the given resource_changes,
// output_changes, and resource_drift, each a JSON value.
func tofuPlanJSONOf(resources, outputs, drift string) func() (tofu.Plan, error) {
	return func() (tofu.Plan, error) {
		return tofu.Plan{File: []byte("saved"), JSON: []byte(`{"format_version":"1.2","terraform_version":"1.13.9",
 "resource_changes":` + resources + `,"output_changes":` + outputs + `,"resource_drift":` + drift + `}`)}, nil
	}
}

const (
	petNoOp = `{"address":"random_pet.q","mode":"managed","type":"random_pet","name":"q","provider_name":"registry.opentofu.org/hashicorp/random",
   "change":{"actions":["no-op"],"before":{"length":2},"after":{"length":2},"after_unknown":{},"before_sensitive":{},"after_sensitive":{}}}`
	petMoved = `{"address":"random_pet.r","previous_address":"random_pet.q","mode":"managed","type":"random_pet","name":"r","provider_name":"registry.opentofu.org/hashicorp/random",
   "change":{"actions":["no-op"],"before":{"length":2},"after":{"length":2},"after_unknown":{},"before_sensitive":{},"after_sensitive":{}}}`
	petRead = `{"address":"data.random_pet.q","mode":"data","type":"random_pet","name":"q","provider_name":"registry.opentofu.org/hashicorp/random",
   "change":{"actions":["read"],"before":null,"after":{},"after_unknown":{"id":true},"before_sensitive":false,"after_sensitive":{}}}`
	petDrifted = `{"address":"random_pet.q","mode":"managed","type":"random_pet","name":"q","provider_name":"registry.opentofu.org/hashicorp/random",
   "change":{"actions":["update"],"before":{"length":2},"after":{"length":3},"after_unknown":{},"before_sensitive":{},"after_sensitive":{}}}`
	outputSame   = `{"region":{"actions":["no-op"],"before":"eu-west-1","after":"eu-west-1","after_unknown":false,"before_sensitive":false,"after_sensitive":false}}`
	outputChange = `{"region":{"actions":["create"],"before":null,"after":"eu-west-1","after_unknown":false,"before_sensitive":false,"after_sensitive":false}}`
)

// noChangeFixture is the merge request flow's control plane whose
// stand-in for OpenTofu plans what set gives it last, where the jobs of
// a merge request pipeline may apply prod by plan id.
func noChangeFixture(t *testing.T, e controltest.Engine) (*fixture, func(func() (tofu.Plan, error))) {
	var mu sync.Mutex
	plan := tofuPlanJSONOf("["+petNoOp+"]", outputSame, "[]")
	runner := &fakeTofu{plan: func() (tofu.Plan, error) {
		mu.Lock()
		defer mu.Unlock()
		return plan()
	}}
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
	f := tofuFixtureOn(t, e, runner, uidLines(t), withApprovals(t, mrApply))
	f.addInfra()
	return f, func(p func() (tofu.Plan, error)) {
		mu.Lock()
		defer mu.Unlock()
		plan = p
	}
}

// A plan that would change nothing ends no-changes: it takes no approval,
// no apply by id runs it, no list of live plans counts it, and an apply
// after its merge has nothing to apply.
func TestNoChangePlan(t *testing.T) {
	controltest.Each(t, func(t *testing.T, e controltest.Engine) {
		f, _ := noChangeFixture(t, e)
		files := changed("# a comment\n")
		p := f.mrPlan(f.gitlab.Commit(4242, "comment", files), "comment", "network", "300")
		id := p.Plan.ID
		if p.Plan.Status != "no-changes" || p.Plan.Approvals.Need != 0 || p.Plan.ExpiresAt != "" || p.Plan.Stacks[0].Changes != 0 {
			t.Fatalf("plan %+v", p.Plan)
		}
		var moves []string
		for _, ev := range p.Plan.History {
			moves = append(moves, ev.From+">"+ev.To)
		}
		if strings.Join(moves, " ") != "planning>no-changes" {
			t.Fatalf("history %v", moves)
		}

		// Neither an approval nor an apply by id takes it.
		if r := f.do("POST", "/v1/plans/"+id+"/approvals", bob(f, map[string]any{"pipeline_id": "300"}), nil); r.status != http.StatusConflict || r.class() != "state" || !strings.Contains(string(r.body), "changes no resource") {
			t.Fatalf("an approval: %d %s", r.status, r.body)
		}
		r := f.do("POST", "/v1/runs", f.token(map[string]any{"pipeline_id": "300"}), runRequest(map[string]any{"plan": id}))
		if want := "plan " + id + " changes no resource; there is nothing to apply"; r.status != http.StatusConflict || r.class() != "state" || !strings.Contains(string(r.body), want) {
			t.Fatalf("an apply by id: %d %s", r.status, r.body)
		}

		// The pipeline's approve job passes, and approves nothing.
		r = f.do("POST", "/v1/approvals", bob(f, map[string]any{"pipeline_id": "300"}), runRequestBody(`{"format":"writ.approve-request/v0","pipeline":true}`)())
		var ad server.ApproveDoc
		if err := json.Unmarshal(r.body, &ad); err != nil || r.status != http.StatusOK || len(ad.Plans) != 1 || ad.Plans[0].ID != id ||
			ad.Plans[0].Status != "no-changes" || len(ad.Plans[0].ApprovedBy) != 0 {
			t.Fatalf("the approve job: %d %s", r.status, r.body)
		}

		// Lists of plans find it by its status, and none of the live ones
		// counts it.
		reader := f.token(nil)
		for status, want := range map[string]bool{"no-changes": true, "approved": false, "pending": false} {
			r := f.do("GET", "/v1/plans?env=prod&status="+status, reader, nil)
			if r.status != http.StatusOK {
				t.Fatalf("plans of status %s: %d %s", status, r.status, r.body)
			}
			if got := strings.Contains(string(r.body), id); got != want {
				t.Errorf("plans of status %s list it: %v, want %v: %s", status, got, want, r.body)
			}
		}
		r = f.do("GET", "/v1/overview", reader, nil)
		var ov server.OverviewDoc
		if err := json.Unmarshal(r.body, &ov); err != nil || r.status != http.StatusOK {
			t.Fatalf("overview: %d %s", r.status, r.body)
		}
		for _, env := range ov.Envs {
			if env.Plans != (server.LiveCounts{}) {
				t.Errorf("env %s counts %+v", env.Name, env.Plans)
			}
		}
		for _, st := range ov.Stacks {
			if st.Plans != (server.LiveCounts{}) {
				t.Errorf("stack %s@%s counts %+v", st.Stack, st.Env, st.Plans)
			}
		}

		// After the merge, the job on main has nothing to apply: it gets a
		// 200 that names the plan and starts no run.
		main := mainJob(f, f.gitlab.Commit(4242, "main", files))
		r = f.do("POST", "/v1/runs", main, mergedRequest("network"))
		var run server.RemoteRun
		if err := json.Unmarshal(r.body, &run); err != nil || r.status != http.StatusOK || !run.OK || run.Run != nil || run.Plan != id ||
			!strings.Contains(run.Text, "nothing to apply") {
			t.Fatalf("the apply after the merge: %d %s", r.status, r.body)
		}
		if applied := f.tofu.appliedSoFar(); len(applied) != 0 {
			t.Fatalf("OpenTofu applied %d plans", len(applied))
		}
	})
}

// Only a plan whose apply would change nothing is no-changes, as OpenTofu
// decides that a saved plan has something to apply: a move, a read at
// apply time, an output that changes, or an object that changed outside
// OpenTofu each keeps the plan live.
func TestNoChangeIsOpenTofusNothingToApply(t *testing.T) {
	controltest.Each(t, func(t *testing.T, e controltest.Engine) {
		f, set := noChangeFixture(t, e)
		for i, tc := range []struct {
			name                      string
			resources, outputs, drift string
			status                    string
		}{
			{"no resource", "[]", "{}", "[]", "no-changes"},
			{"a no-op and the same output", "[" + petNoOp + "]", outputSame, "[]", "no-changes"},
			{"an output that changes", "[" + petNoOp + "]", outputChange, "[]", "approved"},
			{"a move", "[" + petMoved + "]", "{}", "[]", "approved"},
			{"a read at apply time", "[" + petRead + "]", "{}", "[]", "approved"},
			{"a change outside OpenTofu", "[" + petNoOp + "]", "{}", "[" + petDrifted + "]", "approved"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				set(tofuPlanJSONOf(tc.resources, tc.outputs, tc.drift))
				branch := fmt.Sprintf("case-%d", i)
				p := f.mrPlan(f.gitlab.Commit(4242, branch, changed(fmt.Sprintf("# case %d\n", i))), branch, "network", fmt.Sprint(400+i))
				if p.Plan.Status != tc.status {
					t.Fatalf("status %s, want %s: %+v", p.Plan.Status, tc.status, p.Plan)
				}
			})
		}
	})
}

// A no-change plan says there is nothing to apply only while the state it
// read is the state of its stack@env: after a run applies to the stack,
// an apply after the merge of its sources is refused, and asks for a new
// plan.
func TestNoChangePlanAfterARun(t *testing.T) {
	controltest.Each(t, func(t *testing.T, e controltest.Engine) {
		f, set := noChangeFixture(t, e)
		quiet := changed("# a comment\n")
		n := f.mrPlan(f.gitlab.Commit(4242, "comment", quiet), "comment", "network", "300")
		if n.Plan.Status != "no-changes" {
			t.Fatalf("plan %+v", n.Plan)
		}

		set(tofuPlanJSONOf("[]", outputChange, "[]"))
		loud := changed("output \"region\" {\n  value = \"eu-west-1\"\n}\n")
		o := f.mrPlan(f.gitlab.Commit(4242, "output", loud), "output", "network", "301")
		if o.Plan.Status != "approved" {
			t.Fatalf("plan %+v", o.Plan)
		}
		main := mainJob(f, f.gitlab.Commit(4242, "main", loud))
		r := f.do("POST", "/v1/runs", main, mergedRequest("network"))
		if r.status != http.StatusAccepted {
			t.Fatalf("apply the output: %d %s", r.status, r.body)
		}
		if run := f.waitRun(main, r.doc["run"].(map[string]any)["id"].(string)); run.Run.Status != "applied" {
			t.Fatalf("run %+v", run.Run)
		}

		r = f.do("POST", "/v1/runs", mainJob(f, f.gitlab.Commit(4242, "main", quiet)), mergedRequest("network"))
		if r.status != http.StatusConflict || r.class() != "state" || !strings.Contains(string(r.body), n.Plan.ID) || !strings.Contains(string(r.body), "plan again") {
			t.Fatalf("the merge of the no-change plan after a run: %d %s", r.status, r.body)
		}
		if got := f.waitPlan(f.token(nil), n.Plan.ID); got.Plan.Status != "no-changes" {
			t.Fatalf("the no-change plan after a run is %s", got.Plan.Status)
		}
	})
}
