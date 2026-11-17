package server_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/server"
)

// withheldAfter is an Options.Reap that ends the processes of the first n
// uids it is given, the pool's fill at start, and fails for every uid
// after, so each uid a plan or apply frees is withheld.
func withheldAfter(n int32) func(*server.Options) {
	var calls atomic.Int32
	return func(o *server.Options) {
		o.Reap = func(uint32) error {
			if calls.Add(1) <= n {
				return nil
			}
			return errors.New("processes of the uid are still running")
		}
	}
}

// twoUIDs gives the server two provider uids and providers outside data,
// as provider_uids needs.
func twoUIDs(t *testing.T) string {
	return `provider_uids = "61000-61001"`
}

// startPlan starts a plan with a token of its own, since a CI token makes
// one plan.
func (f *fixture) startPlan() string {
	f.t.Helper()
	r := f.do("POST", "/v1/plans", f.mrToken(nil, nil), planRequest(nil))
	if r.status != http.StatusAccepted {
		f.t.Fatalf("plan: %d %s", r.status, r.body)
	}
	return r.doc["plan"].(map[string]any)["ref"].(string)
}

func (f *fixture) running() (plans, applies int) {
	f.t.Helper()
	for range 100 {
		plans, applies = f.srv.Running()
		if plans == 0 && applies == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	return
}

// A plan whose OpenTofu does not return ends failed at
// limits.plan_timeout, and gives back its place.
func TestPlanTimeout(t *testing.T) {
	f := planFixture(t, func(s string) string {
		return strings.Replace(withForge(t, "")(s), "limits {\n", "limits {\n  plans_per_principal = 1\n  plan_timeout = \"300ms\"\n", 1)
	})
	f.addInfra()
	gate := make(chan struct{})
	defer close(gate)
	f.tofu.gate = gate
	tok := f.token(nil)
	start := time.Now()
	p := f.waitPlan(tok, f.startPlan())
	if p.Plan.Status != "failed" || !strings.Contains(string(p.Document), "the plan ran past limits.plan_timeout, 300ms") {
		t.Fatalf("plan %s: %s", p.Plan.Status, p.Document)
	}
	if took := time.Since(start); took < 300*time.Millisecond || took > 5*time.Second {
		t.Errorf("the plan ended after %s, want about 300ms", took)
	}
	if plans, _ := f.running(); plans != 0 {
		t.Fatalf("%d plans still count as running", plans)
	}
	f.startPlan()
}

// With every provider uid withheld, a new plan fails at
// limits.plan_timeout, says why, and gives back its place. The two plans
// that withhold the uids get a minute, so only the third plan races the
// 500ms limit on a busy machine.
func TestPlanTimeoutWithEveryUIDWithheld(t *testing.T) {
	var limits atomic.Int32
	setupGetsAMinute := func(o *server.Options) {
		o.WithTimeout = func(ctx context.Context, d time.Duration, cause error) (context.Context, context.CancelFunc) {
			if limits.Add(1) <= 2 {
				d = time.Minute
			}
			return context.WithTimeoutCause(ctx, d, cause)
		}
	}
	f := planFixtureUIDs(t, twoUIDs(t)+"\n", func(s string) string {
		return strings.Replace(withForge(t, "")(s), "limits {\n", "limits {\n  plans = 2\n  plans_per_principal = 1\n  plan_timeout = \"500ms\"\n", 1)
	}, withheldAfter(2), setupGetsAMinute)
	f.addInfra()
	tok := f.token(nil)
	for i := range 2 {
		if p := f.waitPlan(tok, f.startPlan()); p.Plan.Status != "approved" {
			t.Fatalf("plan %d: %s %s", i+1, p.Plan.Status, p.Document)
		}
	}
	start := time.Now()
	p := f.waitPlan(tok, f.startPlan())
	want := "the plan ran past limits.plan_timeout, 500ms, while it waited for a provider uid: 2 of the 2 provider_uids are withheld"
	if p.Plan.Status != "failed" || !strings.Contains(string(p.Document), want) {
		t.Fatalf("plan %s: %s, want %q", p.Plan.Status, p.Document, want)
	}
	if took := time.Since(start); took < 500*time.Millisecond || took > 5*time.Second {
		t.Errorf("the plan ended after %s, want about 500ms", took)
	}
	if plans, _ := f.running(); plans != 0 {
		t.Fatalf("%d plans still count as running", plans)
	}
	if n := len(f.tofu.stacks); n != 2 {
		t.Errorf("OpenTofu planned %d times, want 2: the third plan got no uid", n)
	}
}

// With every provider uid withheld, an apply fails at
// limits.apply_start_timeout before it starts, and gives back its place.
func TestApplyStartTimeout(t *testing.T) {
	limits := func(s string) string {
		return strings.Replace(s, "requests_per_address = 1000\n", "requests_per_address = 1000\n  plans = 2\n  apply_start_timeout = \"500ms\"\n", 1)
	}
	f := planFixtureUIDs(t, twoUIDs(t)+"\n", withApprovals(t, limits), withheldAfter(2))
	f.addInfra()
	files := changed("resource \"random_pet\" \"pet2\" {}\n")
	sha := f.gitlab.Commit(4242, "mr", files)
	p := f.mrPlan(sha, "mr", "network", "400")
	if res := f.do("POST", "/v1/plans/"+p.Plan.ID+"/approvals", bob(f, map[string]any{"pipeline_id": "400"}), nil); res.status != http.StatusOK {
		t.Fatalf("approve: %d %s", res.status, res.body)
	}
	// A plan of another stack takes the second uid, which is withheld
	// after it.
	tok := f.mrToken(nil, nil)
	r := f.do("POST", "/v1/plans", tok, planRequest(func(m map[string]any) { m["stacks"] = []string{"database"} }))
	if p := f.waitPlan(tok, r.doc["plan"].(map[string]any)["ref"].(string)); p.Plan.Status != "pending" {
		t.Fatalf("second plan: %s %s", p.Plan.Status, p.Document)
	}
	merged := f.gitlab.Commit(4242, "main", files)
	main := mainJob(f, merged)
	start := time.Now()
	res := f.do("POST", "/v1/runs", main, mergedRequest("network"))
	if res.status != http.StatusAccepted {
		t.Fatalf("apply: %d %s", res.status, res.body)
	}
	run := f.waitRun(main, res.doc["run"].(map[string]any)["id"].(string))
	want := "the apply's start ran past limits.apply_start_timeout, 500ms, while it waited for a provider uid: 2 of the 2 provider_uids are withheld"
	if run.Run.Status != "failed" || !strings.Contains(run.Text, want) {
		t.Fatalf("run %s: %s, want %q", run.Run.Status, run.Text, want)
	}
	if took := time.Since(start); took < 500*time.Millisecond || took > 5*time.Second {
		t.Errorf("the apply ended after %s, want about 500ms", took)
	}
	if n := len(f.tofu.appliedSoFar()); n != 0 {
		t.Errorf("OpenTofu applied %d times, want none", n)
	}
	if plans, applies := f.running(); plans != 0 || applies != 0 {
		t.Fatalf("%d plans and %d applies still count as running", plans, applies)
	}
}
