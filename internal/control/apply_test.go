package control_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/control/controltest"
	"github.com/mr-karan/writ/internal/sqldb"
)

// lifecycle is one store with a config, for the tests of approvals and
// runs.
type lifecycle struct {
	t   *testing.T
	s   *control.Store
	cfg int64
	n   int
	raw *sqldb.DB // the store's database, for commitPlan
}

func newLifecycle(t *testing.T, s *control.Store) *lifecycle {
	cfg, err := s.RecordConfig(context.Background(), "{}", "https://writ.example.com", "test", policy(), t0)
	if err != nil {
		t.Fatal(err)
	}
	return &lifecycle{t: t, s: s, cfg: cfg}
}

func (l *lifecycle) entry(op string) control.Entry {
	l.n++
	return control.Entry{At: t0, Request: fmt.Sprintf("R%d", l.n), Kind: "request", Decision: control.Allowed, Op: op, ConfigID: l.cfg}
}

// plan makes a plan of stack@prod by alice, requested at, that ends with
// status and needs need approvals, and returns its id.
func (l *lifecycle) plan(stack string, at time.Time, status control.PlanStatus, need int) string {
	l.t.Helper()
	ctx := context.Background()
	l.n++
	ref := fmt.Sprintf("REF%022d", l.n)
	id := fmt.Sprintf("%032x", l.n)
	if err := l.s.CreatePlan(ctx, newPlan(ref, l.cfg, "prod", stack, at), control.TokenUse{}, l.entry("plan")); err != nil {
		l.t.Fatal(err)
	}
	res := control.PlanResult{ID: id, Status: status, MadeAt: at, ExpiresAt: at.Add(24 * time.Hour), Risks: []string{"create"}, Changes: 1,
		Saved: []byte("saved"), Need: need, NeedRules: []int{1}}
	if err := l.s.FinishPlan(ctx, ref, res, l.entry("plan")); err != nil {
		l.t.Fatal(err)
	}
	return id
}

func (l *lifecycle) status(id string, now time.Time) control.PlanStatus {
	l.t.Helper()
	p, err := l.s.Plan(context.Background(), id, now)
	if err != nil {
		l.t.Fatal(err)
	}
	return p.Status
}

func person(name string) control.Approval {
	return control.Approval{Person: "gitlab.com/user/" + name, Display: name, Issuer: "gitlab", Subject: name, Via: "gitlab job 1", ApprovedAt: t0.Add(time.Minute)}
}

var applier = control.Principal{Issuer: "gitlab", Subject: "main", Person: "gitlab.com/user/carol", Display: "carol"}

func (l *lifecycle) begin(id, run, override string, now time.Time) (control.Run, error) {
	return l.s.BeginApply(context.Background(), control.ApplyRequest{RunID: run, PlanID: id, Actor: applier, Via: "gitlab job 2",
		Override: override, NamedBy: control.ByPlan, Rules: need(0), Now: now, Audit: l.entry("apply")})
}

// A request whose plan was the same as an earlier request's finds that
// plan by its ref, by its own pipeline or GitHub run, and by its commit,
// its pipeline owns it, and it carries that plan's policy decision. A plan that a newer, different plan of its
// merge request supersedes still reads as itself, and takes neither an
// approval nor an apply.
func TestSamePlanRequests(t *testing.T) {
	each(t, func(t *testing.T, s *control.Store, _ string) {
		ctx := context.Background()
		l := newLifecycle(t, s)
		gitlab := func(pipeline string) control.Pipeline {
			return control.Pipeline{Issuer: "https://gitlab.example.com", ProjectID: "4242", Pipeline: pipeline}
		}
		github := control.Pipeline{Issuer: "https://token.actions.githubusercontent.com", ProjectID: "77", Pipeline: "9"}
		request := func(ref, repo, origin, id string, at time.Time) {
			t.Helper()
			p := newPlan(ref, l.cfg, "prod", "network", at)
			p.Origin, p.Source = json.RawMessage(origin), control.PlanSource{Repo: repo, Ref: "refs/heads/main", Commit: "c1"}
			// As writ serve does, the request learns its digest only when
			// the commit is read, and its plan ends with it.
			p.SourceDigest = ""
			if err := s.CreatePlan(ctx, p, control.TokenUse{}, l.entry("plan")); err != nil {
				t.Fatal(err)
			}
			res := control.PlanResult{ID: id, Status: control.Pending, MadeAt: at, ExpiresAt: at.Add(24 * time.Hour), Changes: 1, Saved: []byte("saved"), Need: 1, SourceDigest: "abc",
				Policy: json.RawMessage(`{"point":"plan","request":"` + ref + `"}`)}
			if err := s.FinishPlan(ctx, ref, res, l.entry("plan")); err != nil {
				t.Fatal(err)
			}
		}
		mr := func(pipeline string) string {
			return `{"kind":"gitlab-merge-request","issuer":"https://gitlab.example.com","project_id":"4242","pipeline":"` + pipeline + `","merge_request":5}`
		}
		x, y := fmt.Sprintf("%032x", 1), fmt.Sprintf("%032x", 2)
		request("REFA", "infra", mr("801"), x, t0)
		request("REFB", "mirror", mr("802"), x, t0.Add(time.Minute))
		request("REFC", "infra", `{"kind":"github-run","issuer":"https://token.actions.githubusercontent.com","repository_id":"77","run":"9"}`, x, t0.Add(2*time.Minute))
		now := t0.Add(3 * time.Minute)

		const policyA = `{"point":"plan","request":"REFA"}`
		for _, ref := range []string{"REFB", "REFC"} {
			if p, err := s.Plan(ctx, ref, now); err != nil || p.ID != x || p.Ref != "REFA" || p.Status != control.Pending || string(p.Policy) != policyA {
				t.Fatalf("request %s reads as %+v, %v; want plan %s", ref, p, err, x)
			}
		}
		rows, _, err := s.Plans(ctx, control.PlanFilter{Limit: 10}, now)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range rows {
			if string(p.Policy) != policyA {
				t.Errorf("request %s keeps the policy decision %s; want plan %s's, %s", p.Ref, p.Policy, x, policyA)
			}
		}
		for _, p := range []control.Pipeline{gitlab("801"), gitlab("802"), github} {
			plans, err := s.PipelinePlans(ctx, p, now, 10)
			if err != nil || len(plans) != 1 || plans[0].ID != x {
				t.Fatalf("plans of %+v: %+v, %v", p, plans, err)
			}
			if has, err := s.PipelineHas(ctx, p, x); err != nil || !has {
				t.Fatalf("%+v does not own plan %s: %v", p, x, err)
			}
		}
		other := github
		other.Pipeline = "10"
		for _, p := range []control.Pipeline{gitlab("803"), other} {
			plans, err := s.PipelinePlans(ctx, p, now, 10)
			if has, herr := s.PipelineHas(ctx, p, x); err != nil || herr != nil || len(plans) != 0 || has {
				t.Fatalf("%+v, which requested no plan, has %+v, owns %v: %v, %v", p, plans, has, err, herr)
			}
		}
		if p, _, err := s.MergedPlan(ctx, "mirror", "prod", "network", "abc", need(1), now); err != nil || p.ID != x {
			t.Fatalf("the merged plan of the mirror's commit: %+v, %v", p, err)
		}

		request("REFD", "infra", mr("803"), y, now)
		later := now.Add(time.Minute)
		for _, ref := range []string{"REFA", "REFB"} {
			if p, err := s.Plan(ctx, ref, later); err != nil || p.ID != x || p.SupersededBy != y {
				t.Fatalf("request %s after a newer plan reads as %+v, %v; want plan %s superseded by %s", ref, p, err, x, y)
			}
		}
		if plans, err := s.PipelinePlans(ctx, gitlab("803"), later, 10); err != nil || len(plans) != 1 || plans[0].ID != y {
			t.Fatalf("plans of pipeline 803: %+v, %v", plans, err)
		}

		// Plan x, superseded by y, takes no approval and does not apply,
		// for one reason; the apply moves it to superseded. Plan y does
		// both.
		want := "plan " + x + " is stale: plan " + y + " of network@prod is newer; plan again"
		var ae *control.ApprovalError
		if _, err := s.Approve(ctx, person("bob"), []string{x}, need(1), control.TokenUse{}, l.entry("approve")); !errors.As(err, &ae) || ae.Msg != want {
			t.Fatalf("an approval of plan %s: %v; want %q", x, err, want)
		}
		var stale *control.StaleError
		if _, err := l.begin(x, "RUN1", "", later); !errors.As(err, &stale) || stale.Reason != want || l.status(x, later) != control.Superseded {
			t.Fatalf("an apply of plan %s: %v, %s; want %q", x, err, l.status(x, later), want)
		}
		if _, err := s.Approve(ctx, person("bob"), []string{y}, need(1), control.TokenUse{}, l.entry("approve")); err != nil {
			t.Fatalf("an approval of plan %s: %v", y, err)
		}
		if _, err := l.begin(y, "RUN2", "", later); err != nil {
			t.Fatalf("an apply of plan %s: %v", y, err)
		}
	})
}

func TestApprovals(t *testing.T) {
	each(t, func(t *testing.T, s *control.Store, _ string) {
		ctx := context.Background()
		l := newLifecycle(t, s)
		id := l.plan("network", t0, control.Pending, 2)
		approve := func(who control.Approval) error {
			_, err := s.Approve(ctx, who, []string{id}, need(2), control.TokenUse{}, l.entry("approve"))
			return err
		}
		var ae *control.ApprovalError
		if err := approve(person("1001")); !errors.As(err, &ae) {
			t.Fatalf("the author approved her own plan: %v", err)
		}
		if err := approve(person("bob")); err != nil || l.status(id, t0) != control.Pending {
			t.Fatalf("one of two approvals: %v, %s", err, l.status(id, t0))
		}
		if err := approve(person("bob")); err != nil {
			t.Fatalf("a second approval by the same person: %v", err)
		}
		if got, _ := s.Approvals(ctx, []string{id}); len(got[id]) != 1 {
			t.Fatalf("bob counted %d times", len(got[id]))
		}
		if err := approve(person("dave")); err != nil || l.status(id, t0) != control.Approved {
			t.Fatalf("two of two approvals: %v, %s", err, l.status(id, t0))
		}
		// A third approver of an approved plan passes and adds nothing.
		if err := approve(person("erin")); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.Approvals(ctx, []string{id}); len(got[id]) != 2 {
			t.Fatalf("approvals %+v", got[id])
		}
		if _, err := s.Withdraw(ctx, id, "gitlab.com/user/dave", "dave", need(2), t0.Add(2*time.Minute), l.entry("approve")); err != nil || l.status(id, t0) != control.Pending {
			t.Fatalf("withdraw: %v, %s", err, l.status(id, t0))
		}
		if _, err := s.Withdraw(ctx, id, "gitlab.com/user/dave", "dave", need(2), t0.Add(2*time.Minute), l.entry("approve")); !errors.As(err, &ae) {
			t.Fatalf("withdrew twice: %v", err)
		}
		// Past its expiry a plan takes no approval.
		_, err := s.Approve(ctx, control.Approval{Person: "x", Display: "x", ApprovedAt: t0.Add(25 * time.Hour)}, []string{id}, need(0), control.TokenUse{}, l.entry("approve"))
		if !errors.As(err, &ae) || ae.Status != control.Expired {
			t.Fatalf("an expired plan took an approval: %v", err)
		}
		// The events record each move with who made it.
		p, _ := s.Plan(ctx, id, t0)
		evs, err := s.PlanEvents(ctx, p.Ref)
		if err != nil {
			t.Fatal(err)
		}
		var moves []string
		for _, ev := range evs {
			moves = append(moves, string(ev.From)+">"+string(ev.To)+" "+ev.Display)
		}
		want := []string{"planning>pending ", "pending>approved dave", "approved>pending dave"}
		if !slices.Equal(moves, want) {
			t.Fatalf("events %q, want %q", moves, want)
		}
	})
}

// An approval is all or nothing over the plans it names: one that the
// approver made refuses the whole request, and records no approval.
func TestApproveAllOrNothing(t *testing.T) {
	each(t, func(t *testing.T, s *control.Store, _ string) {
		ctx := context.Background()
		l := newLifecycle(t, s)
		a, b := l.plan("a", t0, control.Pending, 1), l.plan("b", t0, control.Pending, 1)
		var ae *control.ApprovalError
		// alice made both; bob approves a, then alice asks for both.
		if _, err := s.Approve(ctx, person("bob"), []string{a}, need(1), control.TokenUse{}, l.entry("approve")); err != nil {
			t.Fatal(err)
		}
		_, err := s.Approve(ctx, person("1001"), []string{a, b}, need(1), control.TokenUse{}, l.entry("approve"))
		if !errors.As(err, &ae) || ae.Plan != b {
			t.Fatalf("the author's approval of b: %v", err)
		}
		use := control.TokenUse{Issuer: "gitlab", JTI: "approve-1", Op: "approve", Expires: t0.Add(time.Hour)}
		if _, err := s.Approve(ctx, person("bob"), []string{a, b}, need(1), use, l.entry("approve")); err != nil {
			t.Fatal(err)
		}
		if l.status(a, t0) != control.Approved || l.status(b, t0) != control.Approved {
			t.Fatal("bob's approval did not approve both")
		}
		if _, err := s.Approve(ctx, person("dave"), []string{b}, need(0), use, l.entry("approve")); !errors.Is(err, control.ErrReplay) {
			t.Fatalf("a token approved twice: %v", err)
		}
	})
}

func TestBeginApply(t *testing.T) {
	each(t, func(t *testing.T, s *control.Store, _ string) {
		ctx := context.Background()
		l := newLifecycle(t, s)
		id := l.plan("network", t0, control.Pending, 1)
		var na *control.NotApplicableError
		if _, err := l.begin(id, "RUN1", "", t0); !errors.As(err, &na) || na.Have != 0 || na.Need != 1 {
			t.Fatalf("applied with no approval: %v", err)
		}
		if _, err := l.begin(id, "RUN1", "short", t0); err == nil {
			t.Fatal("an override of 5 characters")
		}
		if _, err := s.Approve(ctx, person("bob"), []string{id}, need(0), control.TokenUse{}, l.entry("approve")); err != nil {
			t.Fatal(err)
		}
		if _, err := l.begin(id, "RUN1", "bob is on leave, OPS-112", t0); !errors.As(err, &na) {
			t.Fatalf("an override of a plan with its approvals: %v", err)
		}
		// Rules made stricter since the plan count at apply.
		_, err := s.BeginApply(ctx, control.ApplyRequest{RunID: "RUN1", PlanID: id, Actor: applier, NamedBy: control.ByPlan, Rules: need(2), Now: t0, Audit: l.entry("apply")})
		if !errors.As(err, &na) || na.Need != 2 {
			t.Fatalf("applied with 1 of 2 approvals: %v", err)
		}
		run, err := l.begin(id, "RUN1", "", t0)
		if err != nil || run.Env != "prod" || run.Stack != "network" || len(run.Approvals) != 1 || run.Approvals[0].Display != "bob" {
			t.Fatalf("begin: %+v, %v", run, err)
		}
		if l.status(id, t0) != control.Applying {
			t.Fatalf("status %s", l.status(id, t0))
		}
		if _, err := l.begin(id, "RUN2", "", t0); !errors.As(err, &na) || na.Status != control.Applying {
			t.Fatalf("a plan applied twice: %v", err)
		}
		// Withdrawal ends when the plan starts applying.
		var ae *control.ApprovalError
		if _, err := s.Withdraw(ctx, id, "gitlab.com/user/bob", "bob", need(1), t0, l.entry("approve")); !errors.As(err, &ae) {
			t.Fatalf("withdrew from an applying plan: %v", err)
		}
		got, err := s.GetRun(ctx, "RUN1")
		if err != nil || got.Status != control.RunApplying || got.PlanID != id {
			t.Fatalf("run %+v, %v", got, err)
		}
		voided, err := s.FinishRun(ctx, "RUN1", control.RunResult{Status: control.RunApplied, EndedAt: t0.Add(time.Minute), Text: "Applied 1 change."}, l.entry("apply"))
		if err != nil || len(voided) != 0 || l.status(id, t0) != control.Applied {
			t.Fatalf("finish: %v %v %s", voided, err, l.status(id, t0))
		}
		if _, err := s.FinishRun(ctx, "RUN1", control.RunResult{Status: control.RunFailed, EndedAt: t0}, l.entry("apply")); err == nil {
			t.Fatal("a run ended twice")
		}
		if _, err := l.begin(id, "RUN3", "", t0); !errors.As(err, &na) || na.Status != control.Applied {
			t.Fatalf("an applied plan applied again: %v", err)
		}
	})
}

// An apply of a plan that expired, or whose state changed after it was
// requested, is refused, and the plan becomes expired or superseded.
func TestStalePlans(t *testing.T) {
	each(t, func(t *testing.T, s *control.Store, _ string) {
		ctx := context.Background()
		l := newLifecycle(t, s)
		var se *control.StaleError
		old := l.plan("network", t0, control.Approved, 0)
		if _, err := l.begin(old, "RUN0", "", t0.Add(25*time.Hour)); !errors.As(err, &se) || se.Status != control.Expired {
			t.Fatalf("an expired plan: %v", err)
		}
		if p, _ := s.Plan(ctx, old, t0); p.Status != control.Expired {
			t.Fatalf("the expired plan is stored %s", p.Status)
		}

		first := l.plan("network", t0, control.Approved, 0)
		second := l.plan("network", t0.Add(time.Second), control.Approved, 0)
		other := l.plan("database", t0, control.Approved, 0)
		if _, err := l.begin(first, "RUN1", "", t0.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
		// While RUN1 applies, a plan of network made then is superseded, and
		// one of database is not.
		during := l.plan("network", t0.Add(3*time.Second), control.Approved, 0)
		if got := l.status(during, t0); got != control.Superseded {
			t.Fatalf("a plan made during a run is %s", got)
		}
		if _, err := l.begin(second, "RUN2", "", t0.Add(4*time.Second)); !errors.As(err, &se) || se.Status != control.Superseded {
			t.Fatalf("a second apply on the stack while one runs: %v", err)
		}
		voided, err := s.FinishRun(ctx, "RUN1", control.RunResult{Status: control.RunFailed, EndedAt: t0.Add(5 * time.Second)}, l.entry("apply"))
		if err != nil || len(voided) != 0 {
			t.Fatalf("voided %v, %v", voided, err)
		}
		if l.status(first, t0) != control.Failed || l.status(other, t0) != control.Approved {
			t.Fatalf("after the run: %s, %s", l.status(first, t0), l.status(other, t0))
		}
		// A plan made after the run ended applies.
		after := l.plan("network", t0.Add(6*time.Second), control.Approved, 0)
		if _, err := l.begin(after, "RUN3", "", t0.Add(7*time.Second)); err != nil {
			t.Fatalf("a plan made after the run: %v", err)
		}
		live := l.plan("network", t0.Add(8*time.Second), control.Pending, 1)
		_ = live
		voided, err = s.FinishRun(ctx, "RUN3", control.RunResult{Status: control.RunApplied, EndedAt: t0.Add(7 * time.Second)}, l.entry("apply"))
		if err != nil {
			t.Fatal(err)
		}
		// live was made while RUN3 ran, so it was superseded when it ended
		// planning, not by the voiding rule.
		if len(voided) != 0 || l.status(live, t0) != control.Superseded {
			t.Fatalf("voided %v, live is %s", voided, l.status(live, t0))
		}
		// The voiding rule: a live plan made before a run ends is
		// superseded when it ends.
		a := l.plan("cache", t0.Add(10*time.Second), control.Approved, 0)
		b := l.plan("cache", t0.Add(11*time.Second), control.Pending, 1)
		if _, err := l.begin(a, "RUN4", "", t0.Add(12*time.Second)); err != nil {
			t.Fatal(err)
		}
		voided, err = s.FinishRun(ctx, "RUN4", control.RunResult{Status: control.RunApplied, EndedAt: t0.Add(13 * time.Second)}, l.entry("apply"))
		if err != nil || !slices.Equal(voided, []string{b}) || l.status(b, t0) != control.Superseded {
			t.Fatalf("voided %v (%v), b is %s", voided, err, l.status(b, t0))
		}
		p, _ := s.Plan(ctx, b, t0)
		if p.SupersededWhy == "" {
			t.Fatal("no reason for the supersede")
		}
		var na *control.NotApplicableError
		if _, err := l.begin(b, "RUN5", "", t0.Add(14*time.Second)); !errors.As(err, &na) || na.Status != control.Superseded {
			t.Fatalf("a superseded plan: %v", err)
		}
		if err := s.Supersede(ctx, other, "the ref moved", t0); err != nil || l.status(other, t0) != control.Superseded {
			t.Fatalf("supersede: %v", err)
		}
		if last, err := s.LastApplied(ctx, "prod", "cache", t0); err != nil || last.ID != a {
			t.Fatalf("last applied %+v, %v", last, err)
		}
	})
}

func TestFailApplying(t *testing.T) {
	each(t, func(t *testing.T, s *control.Store, _ string) {
		ctx := context.Background()
		l := newLifecycle(t, s)
		id := l.plan("network", t0, control.Approved, 0)
		waiting := l.plan("network", t0, control.Approved, 0)
		if _, err := l.begin(id, "RUN1", "", t0); err != nil {
			t.Fatal(err)
		}
		ended, err := s.FailApplying(ctx, t0.Add(time.Minute), "NEXT", func(string) bool { return false })
		if err != nil || !slices.Equal(ended, []string{"RUN1"}) {
			t.Fatalf("ended %v, %v", ended, err)
		}
		run, _ := s.GetRun(ctx, "RUN1")
		if run.Status != control.RunFailed || run.EndedAt.IsZero() || len(run.Document) == 0 {
			t.Fatalf("run %+v", run)
		}
		if l.status(id, t0) != control.Failed || l.status(waiting, t0) != control.Superseded {
			t.Fatalf("after a crash: %s, %s", l.status(id, t0), l.status(waiting, t0))
		}
	})
}

// Requests that race on one plan, or on one stack@env, leave the plan in
// one status the lifecycle allows, with at most one run of it, and a run
// only when the plan had its approvals as it began.
func TestApplyRaces(t *testing.T) {
	each(t, func(t *testing.T, s *control.Store, _ string) {
		ctx := context.Background()
		l := newLifecycle(t, s)
		for i := range 20 {
			at := t0.Add(time.Duration(i) * time.Hour)
			id := l.plan(fmt.Sprintf("s%d", i), at, control.Pending, 1)
			rival := l.plan(fmt.Sprintf("s%d", i), at, control.Pending, 1)
			for _, p := range []string{id, rival} {
				if _, err := s.Approve(ctx, control.Approval{Person: "bob", Display: "bob", ApprovedAt: at}, []string{p}, need(0), control.TokenUse{}, l.entry("approve")); err != nil {
					t.Fatal(err)
				}
			}
			var wg sync.WaitGroup
			began := make([]bool, 4)
			for k := range 4 {
				wg.Go(func() {
					target := id
					if k == 3 {
						target = rival
					}
					_, err := s.BeginApply(ctx, control.ApplyRequest{RunID: fmt.Sprintf("R%d-%d", i, k), PlanID: target, Actor: applier,
						NamedBy: control.ByPlan, Rules: need(0), Now: at.Add(time.Minute), Audit: control.Entry{At: at, Request: fmt.Sprintf("A%d-%d", i, k), Kind: "request", Decision: control.Allowed}})
					began[k] = err == nil
				})
			}
			wg.Go(func() {
				_, _ = s.Withdraw(ctx, id, "bob", "bob", need(1), at.Add(time.Minute), control.Entry{At: at, Request: fmt.Sprintf("W%d", i), Kind: "request", Decision: control.Allowed})
			})
			wg.Wait()
			n := 0
			for _, b := range began {
				if b {
					n++
				}
			}
			if n > 1 {
				t.Fatalf("round %d: %d applies began on one stack@env", i, n)
			}
			for _, p := range []string{id, rival} {
				st := l.status(p, at)
				run, err := s.PlanRun(ctx, p)
				switch {
				case errors.Is(err, control.ErrNotFound):
					if st == control.Applying {
						t.Fatalf("round %d: plan %s applying with no run", i, p)
					}
				case err != nil:
					t.Fatal(err)
				default:
					if st != control.Applying || len(run.Approvals) < 1 {
						t.Fatalf("round %d: plan %s is %s with run %+v", i, p, st, run)
					}
				}
			}
		}
	})
}

// Prune deletes a superseded plan with its events, and keeps one that a
// run or an approval names.
func TestPruneLifecycle(t *testing.T) {
	each(t, func(t *testing.T, s *control.Store, _ string) {
		ctx := context.Background()
		l := newLifecycle(t, s)
		gone := l.plan("network", t0, control.Approved, 0)
		kept := l.plan("network", t0, control.Pending, 1)
		if _, err := s.Approve(ctx, person("bob"), []string{kept}, need(0), control.TokenUse{}, l.entry("approve")); err != nil {
			t.Fatal(err)
		}
		if err := s.Supersede(ctx, gone, "test", t0); err != nil {
			t.Fatal(err)
		}
		live := l.plan("database", t0, control.Pending, 1)
		got, err := s.Prune(ctx, t0.Add(48*time.Hour), control.Retention{Plans: 24 * time.Hour})
		if err != nil || got.Expired != 2 || got.Plans != 2 {
			t.Fatalf("pruned %+v, %v", got, err)
		}
		if _, err := s.Plan(ctx, gone, t0); !errors.Is(err, control.ErrNotFound) {
			t.Fatalf("the superseded plan stayed: %v", err)
		}
		if _, err := s.Plan(ctx, kept, t0); err != nil {
			t.Fatalf("the approved plan went: %v", err)
		}
		_ = live
	})
}

// need is rules that count every approval and ask n of each plan.
// A run begins only with the approvals the apply point's policy judged.
// Bob's approval is withdrawn and Dave's added after the judgment, and
// Dave alone meets the rules: the run's start refuses, records nothing,
// and leaves the plan approved. With the digest of Dave's approval, it
// begins.
func TestBeginApplyChecksPolicyApprovals(t *testing.T) {
	each(t, func(t *testing.T, s *control.Store, _ string) {
		ctx := context.Background()
		l := newLifecycle(t, s)
		id := l.plan("network", t0, control.Pending, 1)
		approve := func(who control.Approval) {
			t.Helper()
			if _, err := s.Approve(ctx, who, []string{id}, need(1), control.TokenUse{}, l.entry("approve")); err != nil {
				t.Fatal(err)
			}
		}
		judged := func() string {
			t.Helper()
			got, err := s.Approvals(ctx, []string{id})
			if err != nil {
				t.Fatal(err)
			}
			return control.ApprovalsDigest(got[id])
		}
		begin := func(run, digest string) (control.Run, error) {
			return s.BeginApply(ctx, control.ApplyRequest{RunID: run, PlanID: id, Actor: applier, NamedBy: control.ByPlan, Rules: need(1), Now: t0,
				Audit: l.entry("apply"), Policy: []byte(`{"outcome":"pass"}`), PolicyApprovals: digest})
		}
		approve(person("bob"))
		withBob := judged()
		if _, err := s.Withdraw(ctx, id, "gitlab.com/user/bob", "bob", need(1), t0.Add(2*time.Minute), l.entry("approve")); err != nil {
			t.Fatal(err)
		}
		approve(person("dave"))
		if _, err := begin("RUN1", withBob); !errors.Is(err, control.ErrApprovalsChanged) {
			t.Fatalf("began with approvals the policy did not judge: %v", err)
		}
		if st := l.status(id, t0); st != control.Approved {
			t.Fatalf("the plan after the refusal is %s", st)
		}
		if _, err := s.GetRun(ctx, "RUN1"); !errors.Is(err, control.ErrNotFound) {
			t.Fatalf("the refused run was recorded: %v", err)
		}
		// Bob approves again: the same person, a new approval.
		if _, err := s.Withdraw(ctx, id, "gitlab.com/user/dave", "dave", need(1), t0.Add(3*time.Minute), l.entry("approve")); err != nil {
			t.Fatal(err)
		}
		approve(person("bob"))
		if _, err := begin("RUN1", withBob); !errors.Is(err, control.ErrApprovalsChanged) {
			t.Fatalf("began with a withdrawn approval's digest: %v", err)
		}
		run, err := begin("RUN1", judged())
		if err != nil || len(run.Approvals) != 1 || run.Approvals[0].Display != "bob" {
			t.Fatalf("the run with the approvals the policy judged: %+v, %v", run, err)
		}
	})
}

func need(n int) control.Rules {
	return control.Rules{Counts: func(string) bool { return true }, Need: func(control.Plan) int { return n }, Speculative: true}
}

// An approval of a person the rules no longer count, such as one kept
// under an identity a config renamed, counts nowhere: not toward an
// approval, not in a plan's status, and not when a run begins. The row
// stays, as history.
func TestApprovalsNotCounted(t *testing.T) {
	each(t, func(t *testing.T, s *control.Store, _ string) {
		ctx := context.Background()
		l := newLifecycle(t, s)
		renamed := func(n int) control.Rules {
			r := need(n)
			r.Counts = func(person string) bool { return strings.HasPrefix(person, "new/") }
			return r
		}
		bob := func(identity string) control.Approval {
			a := person("bob")
			a.Person = identity + "/user/2002"
			return a
		}
		two := l.plan("network", t0, control.Pending, 2)
		if _, err := s.Approve(ctx, bob("old"), []string{two}, need(2), control.TokenUse{}, l.entry("approve")); err != nil {
			t.Fatal(err)
		}
		got, err := s.Approve(ctx, bob("new"), []string{two}, renamed(2), control.TokenUse{}, l.entry("approve"))
		if err != nil || got[0].Status != control.Pending || l.status(two, t0) != control.Pending {
			t.Fatalf("bob approved twice under two identities: %+v, %v", got, err)
		}
		var na *control.NotApplicableError
		_, err = s.BeginApply(ctx, control.ApplyRequest{RunID: "RUN1", PlanID: two, Actor: applier, NamedBy: control.ByPlan, Rules: renamed(2), Now: t0, Audit: l.entry("apply")})
		if !errors.As(err, &na) || na.Have != 1 || na.Need != 2 {
			t.Fatalf("applied with bob counted twice: %v", err)
		}
		stored, err := s.Approvals(ctx, []string{two})
		if err != nil || len(stored[two]) != 2 {
			t.Fatalf("the approval of the old identity is gone: %+v, %v", stored, err)
		}

		// A plan approved under the old identity reads as pending, and an
		// approval that counts settles its stored status.
		one := l.plan("database", t0, control.Pending, 1)
		if _, err := s.Approve(ctx, bob("old"), []string{one}, need(1), control.TokenUse{}, l.entry("approve")); err != nil || l.status(one, t0) != control.Approved {
			t.Fatalf("approve: %v, %s", err, l.status(one, t0))
		}
		p, err := s.Plan(ctx, one, t0)
		if err != nil {
			t.Fatal(err)
		}
		stored, _ = s.Approvals(ctx, []string{one})
		if tally := renamed(1).Tally(p, stored[one]); tally.Status != control.Pending || len(tally.Approvals) != 0 {
			t.Fatalf("tally %+v", tally)
		}
		_, err = s.BeginApply(ctx, control.ApplyRequest{RunID: "RUN2", PlanID: one, Actor: applier, NamedBy: control.ByPlan, Rules: renamed(1), Now: t0, Audit: l.entry("apply")})
		if !errors.As(err, &na) || na.Have != 0 {
			t.Fatalf("applied on an approval that does not count: %v", err)
		}
		if _, err := s.Withdraw(ctx, one, "old/user/2002", "bob", renamed(1), t0, l.entry("approve")); err != nil || l.status(one, t0) != control.Pending {
			t.Fatalf("withdraw: %v, %s", err, l.status(one, t0))
		}
	})
}

// commitPlan makes a plan of a commit of repository infra, as l.plan
// does, with the source digest digest.
func (l *lifecycle) commitPlan(stack string, at time.Time, status control.PlanStatus, need int, digest string) string {
	l.t.Helper()
	id := l.plan(stack, at, status, need)
	if _, err := l.raw.ExecContext(context.Background(), "UPDATE plans SET repo = 'infra', source_commit = 'commit', source_digest = ? WHERE id = ?", digest, id); err != nil {
		l.t.Fatal(err)
	}
	return id
}

// An apply after a merge weighs every live plan of the merged sources,
// however many newer ones there are. The oldest plan's approval counts.
// The approvals of the newer plans, more than one read of candidates
// holds, are under an identity the config dropped.
func TestMergedPlanWeighsEveryPlan(t *testing.T) {
	each(t, func(t *testing.T, s *control.Store, loc string) {
		ctx := context.Background()
		l := newLifecycle(t, s)
		l.raw = controltest.Raw(t, loc)
		rules := need(1)
		rules.Counts = func(person string) bool { return strings.HasPrefix(person, "new/") }
		var ids []string
		for i := range 101 {
			id := l.commitPlan("network", t0.Add(time.Duration(i)*time.Millisecond), control.Pending, 1, "same")
			a := person("bob")
			a.Person = "old/user/2002"
			if i == 0 {
				a.Person = "new/user/2002"
			}
			if _, err := s.Approve(ctx, a, []string{id}, need(1), control.TokenUse{}, l.entry("approve")); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		p, tally, err := s.MergedPlan(ctx, "infra", "prod", "network", "same", rules, t0.Add(time.Minute))
		if err != nil || p.ID != ids[0] || tally.Status != control.Approved || len(tally.Approvals) != 1 {
			t.Fatalf("the oldest plan, whose approval counts, is hidden: got %s, tally status=%s count=%d need=%d, err=%v", p.ID, tally.Status, len(tally.Approvals), tally.Need, err)
		}

		// With no approval that counts, the newest plan is chosen, pending.
		none := need(1)
		none.Counts = func(string) bool { return false }
		p, tally, err = s.MergedPlan(ctx, "infra", "prod", "network", "same", none, t0.Add(time.Minute))
		if err != nil || p.ID != ids[100] || tally.Status != control.Pending {
			t.Fatalf("with no approval that counts: got %s, tally status=%s, err=%v", p.ID, tally.Status, err)
		}
		if _, _, err := s.MergedPlan(ctx, "infra", "prod", "network", "other", rules, t0.Add(time.Minute)); !errors.Is(err, control.ErrNotFound) {
			t.Fatalf("a digest of no plan: %v", err)
		}
	})
}

// A plan's tally decides every apply. Bob approves under the old
// identity, the config renames it, and carol approves and withdraws
// under the new one. When the config rolls the rename back, bob's
// approval counts again: the tally is approved although the stored status
// is pending, the apply after a merge chooses the plan, and a run begins
// without an override and refuses one. The run settles the stored status
// first, so the plan's history shows approved before applying.
func TestTallyDecidesApply(t *testing.T) {
	each(t, func(t *testing.T, s *control.Store, loc string) {
		ctx := context.Background()
		l := newLifecycle(t, s)
		l.raw = controltest.Raw(t, loc)
		id := l.commitPlan("network", t0, control.Pending, 1, "same")
		bob := person("bob")
		bob.Person = "old/user/2002"
		carol := person("carol")
		carol.Person = "new/user/3003"
		identity := func(prefix string) control.Rules {
			r := need(1)
			r.Counts = func(p string) bool { return strings.HasPrefix(p, prefix) }
			return r
		}
		if _, err := s.Approve(ctx, bob, []string{id}, identity("old/"), control.TokenUse{}, l.entry("approve")); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Approve(ctx, carol, []string{id}, identity("new/"), control.TokenUse{}, l.entry("approve")); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Withdraw(ctx, id, carol.Person, carol.Display, identity("new/"), t0, l.entry("approve")); err != nil {
			t.Fatal(err)
		}
		if got := l.status(id, t0); got != control.Pending {
			t.Fatalf("stored status after the withdrawal: %s", got)
		}

		rollback := identity("old/")
		p, err := s.Plan(ctx, id, t0)
		if err != nil {
			t.Fatal(err)
		}
		stored, err := s.Approvals(ctx, []string{id})
		if err != nil {
			t.Fatal(err)
		}
		if tally := rollback.Tally(p, stored[id]); tally.Status != control.Approved || len(tally.Approvals) != 1 {
			t.Fatalf("bob's approval counts again, and the tally is %s with %d of %d", tally.Status, len(tally.Approvals), tally.Need)
		}
		merged, tally, err := s.MergedPlan(ctx, "infra", "prod", "network", "same", rollback, t0)
		if err != nil || merged.ID != id || tally.Status != control.Approved {
			t.Fatalf("the apply after a merge: %s, %s, %v", merged.ID, tally.Status, err)
		}
		var na *control.NotApplicableError
		_, err = s.BeginApply(ctx, control.ApplyRequest{RunID: "RUN0", PlanID: id, Actor: applier, NamedBy: control.ByPlan, Override: "bob is on leave, OPS-112", Rules: rollback, Now: t0, Audit: l.entry("apply")})
		if !errors.As(err, &na) || !strings.Contains(na.Msg, "an override skips nothing") {
			t.Fatalf("an override of an approved tally: %v", err)
		}
		if _, err := s.BeginApply(ctx, control.ApplyRequest{RunID: "RUN1", PlanID: id, Actor: applier, NamedBy: control.ByPlan, Rules: rollback, Now: t0, Audit: l.entry("apply")}); err != nil {
			t.Fatalf("the run of an approved tally: %v", err)
		}
		events, err := s.PlanEvents(ctx, p.Ref)
		if err != nil || len(events) < 2 {
			t.Fatalf("events %+v, %v", events, err)
		}
		if last, before := events[len(events)-1], events[len(events)-2]; before.From != control.Pending || before.To != control.Approved || last.From != control.Approved || last.To != control.Applying {
			t.Fatalf("history %+v", events)
		}
	})
}

// A team's control plane never applies a speculative plan, so its tally
// stays pending whatever its approvals, and no run of it begins, with an
// override or without.
func TestSpeculativeTallyPending(t *testing.T) {
	each(t, func(t *testing.T, s *control.Store, _ string) {
		ctx := context.Background()
		l := newLifecycle(t, s)
		team := need(1)
		team.Speculative = false
		id := l.plan("network", t0, control.Pending, 1)
		got, err := s.Approve(ctx, person("bob"), []string{id}, team, control.TokenUse{}, l.entry("approve"))
		if err != nil || got[0].Status != control.Pending || l.status(id, t0) != control.Pending {
			t.Fatalf("a speculative plan with its approvals: %+v, %v", got, err)
		}
		var na *control.NotApplicableError
		for _, override := range []string{"", "bob is on leave, OPS-112"} {
			_, err := s.BeginApply(ctx, control.ApplyRequest{RunID: "RUN1", PlanID: id, Actor: applier, NamedBy: control.ByPlan, Override: override, Rules: team, Now: t0, Audit: l.entry("apply")})
			if !errors.As(err, &na) || !strings.Contains(na.Msg, "speculative") {
				t.Fatalf("a run of a speculative plan, override %q: %v", override, err)
			}
		}
		none := l.plan("database", t0, control.Pending, 0)
		dev, teamNone := need(0), need(0)
		teamNone.Speculative = false
		if p, err := s.Plan(ctx, none, t0); err != nil || dev.Tally(p, nil).Status != control.Approved || teamNone.Tally(p, nil).Status != control.Pending {
			t.Fatalf("a speculative plan that needs no approval: %v", err)
		}
	})
}

// Runs lists an environment's runs, or one stack's, newest first, up to
// a limit, and says when it left older ones out. It reads no document.
func TestRuns(t *testing.T) {
	each(t, func(t *testing.T, s *control.Store, _ string) {
		ctx := context.Background()
		l := newLifecycle(t, s)
		for i, stack := range []string{"network", "app", "network"} {
			at := t0.Add(time.Duration(i) * time.Minute)
			id := l.plan(stack, at, control.Pending, 0)
			run := fmt.Sprintf("RUN%d", i+1)
			if _, err := l.begin(id, run, "", at); err != nil {
				t.Fatal(err)
			}
			if _, err := s.FinishRun(ctx, run, control.RunResult{Status: control.RunApplied, EndedAt: at.Add(time.Second), Document: []byte(`{"ok":true}`), Text: "Applied."}, l.entry("apply")); err != nil {
				t.Fatal(err)
			}
		}
		ids := func(runs []control.ListedRun) string {
			var out []string
			for _, r := range runs {
				out = append(out, r.ID+"@"+r.Stack)
				if r.Document != nil || r.Text != "" || r.Status != control.RunApplied {
					t.Errorf("run %s in a list: %+v", r.ID, r)
				}
			}
			return strings.Join(out, " ")
		}
		for _, tc := range []struct {
			env, stack string
			limit      int
			want       string
			more       bool
			only       string
		}{
			{"prod", "", 10, "RUN3@network RUN2@app RUN1@network", false, ""},
			{"prod", "network", 10, "RUN3@network RUN1@network", false, ""},
			{"prod", "", 2, "RUN3@network RUN2@app", true, ""},
			{"staging", "", 10, "", false, ""},
			{"prod", "", 1, "RUN3@network", true, "network"},
			{"prod", "", 2, "RUN3@network RUN1@network", false, "network"},
			{"prod", "app", 1, "", false, "network"},
		} {
			keep := func(control.Run) bool { return true }
			if tc.only != "" {
				keep = func(r control.Run) bool { return r.Stack == tc.only }
			}
			runs, more, err := s.Runs(ctx, tc.env, tc.stack, tc.limit, keep)
			if err != nil || ids(runs) != tc.want || more != tc.more {
				t.Errorf("Runs(%s, %q, %d, only %q) = %s, %v, %v; want %s, %v", tc.env, tc.stack, tc.limit, tc.only, ids(runs), more, err, tc.want, tc.more)
			}
		}
	})
}
