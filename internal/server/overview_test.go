package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/control/controltest"
	"github.com/mr-karan/writ/internal/server"
	"github.com/mr-karan/writ/internal/tofu"
)

// GET /v1/overview counts what waits, works, failed, and drifted in each
// environment, and holds only the stacks, plans, and runs that GET
// /v1/drift, /v1/plans, and /v1/applies answer the same principal: a
// reader of every stack sees app's pending plan, failed run, and drift; a
// reader of web alone sees web and none of app; a principal in no team
// sees no environment and no repository.
func TestOverviewByGrant(t *testing.T) {
	controltest.Each(t, func(t *testing.T, e controltest.Engine) {
		runner := &fakeTofu{}
		f := driftFixtureOn(t, e, runner)
		failing, _ := f.planCommit(tofuFiles(), "fails")
		if r := f.do("POST", "/v1/plans/"+failing.Plan.ID+"/approvals", bob(f), nil); r.status != http.StatusOK {
			t.Fatalf("bob's approval: %d %s", r.status, r.body)
		}
		runner.mu.Lock()
		runner.apply = func(context.Context, stateCall) ([]byte, error) {
			return nil, &tofu.StepError{Step: "apply", Code: 1}
		}
		runner.mu.Unlock()
		failed := f.tofuRun(bob(f), map[string]any{"plan": failing.Plan.ID})
		if failed.Run.Status != "failed" {
			t.Fatalf("run %+v", failed.Run)
		}
		waiting, _ := f.planCommit(tofuFiles(), "waits")
		if waiting.Plan.Status != "pending" {
			t.Fatalf("plan %+v", waiting.Plan)
		}
		runner.mu.Lock()
		runner.plan = driftPlan(true)
		runner.mu.Unlock()
		f.srv.LookForDrift(time.Hour)
		runner.mu.Lock()
		runner.plan = driftPlan(false)
		runner.mu.Unlock()
		f.srv.LookForDrift(time.Hour)

		type want struct {
			envs   []server.OverviewEnv
			stacks []string
			alerts []string
			// pending are the ids of the pending plans.
			pending []string
			repos   bool
		}
		prod := server.OverviewEnv{Name: "prod", Plans: server.LiveCounts{Pending: 1}, Failed: 1, Stacks: 2, Checked: 2, Drifted: 1}
		app := []string{`"app"`, failing.Plan.ID, failed.Run.ID, waiting.Plan.ID}
		for _, c := range []struct {
			name, tok string
			want      want
			// unseen are strings the answer must not hold.
			unseen []string
		}{
			{"reader of every stack", f.token(nil), want{[]server.OverviewEnv{prod}, []string{"app", "web"}, []string{"drifted app", "run-failed app " + failed.Run.ID}, []string{waiting.Plan.ID}, true}, nil},
			{"reader of web", f.token(map[string]any{"project_id": "4343"}), want{[]server.OverviewEnv{{Name: "prod", Stacks: 1, Checked: 1}}, []string{"web"}, nil, nil, true}, app},
			{"principal in no team", f.token(map[string]any{"project_id": "9999"}), want{nil, nil, nil, nil, false}, append([]string{`"web"`, `"prod"`}, app...)},
		} {
			t.Run(c.name, func(t *testing.T) {
				r := f.do("GET", "/v1/overview", c.tok, nil)
				var doc server.OverviewDoc
				if r.status != http.StatusOK || json.Unmarshal(r.body, &doc) != nil || doc.Format != "writ.overview/v0" {
					t.Fatalf("GET /v1/overview: %d %s", r.status, r.body)
				}
				for _, s := range c.unseen {
					if strings.Contains(string(r.body), s) {
						t.Errorf("the answer holds %s: %s", s, r.body)
					}
				}
				if len(doc.Envs)+len(c.want.envs) > 0 && !slices.Equal(doc.Envs, c.want.envs) {
					t.Errorf("envs %+v, want %+v", doc.Envs, c.want.envs)
				}
				var stacks, drifted []string
				for _, st := range doc.Stacks {
					stacks = append(stacks, st.Stack)
					if st.Drift != nil {
						drifted = append(drifted, st.Stack)
					}
				}
				if !slices.Equal(stacks, c.want.stacks) {
					t.Errorf("stacks %v, want %v", stacks, c.want.stacks)
				}
				var alerts []string
				for _, a := range doc.Alerts {
					s := a.Kind + " " + a.Stack
					if a.Run != "" {
						s += " " + a.Run
					}
					alerts = append(alerts, s)
				}
				if !slices.Equal(alerts, c.want.alerts) {
					t.Errorf("alerts %v, want %v", alerts, c.want.alerts)
				}
				if (doc.Repos != nil) != c.want.repos || doc.Repos != nil && (doc.Repos.Count != 1 || len(doc.Repos.Failed) != 0) {
					t.Errorf("repos %+v, want them: %v", doc.Repos, c.want.repos)
				}

				var fromDrift []string
				for _, st := range f.drift(c.tok, "?limit=200").Stacks {
					fromDrift = append(fromDrift, st.Stack)
				}
				if !slices.Equal(drifted, fromDrift) {
					t.Errorf("the stacks with state %v, GET /v1/drift %v", drifted, fromDrift)
				}
				var pending, fromPlans []string
				for _, p := range doc.Pending {
					pending = append(pending, p.Plan.ID)
				}
				var plans server.RemotePlans
				if r := f.do("GET", "/v1/plans?env=prod&status=pending", c.tok, nil); r.status != http.StatusOK || json.Unmarshal(r.body, &plans) != nil {
					t.Fatalf("GET /v1/plans: %d %s", r.status, r.body)
				}
				for _, p := range plans.Plans {
					fromPlans = append(fromPlans, p.ID)
				}
				if !slices.Equal(pending, c.want.pending) || !slices.Equal(pending, fromPlans) {
					t.Errorf("the pending plans %v, want %v, GET /v1/plans %v", pending, c.want.pending, fromPlans)
				}
				var runs server.RemoteRuns
				if r := f.do("GET", "/v1/applies?env=prod", c.tok, nil); r.status != http.StatusOK || json.Unmarshal(r.body, &runs) != nil {
					t.Fatalf("GET /v1/applies: %d %s", r.status, r.body)
				}
				failedRuns := 0
				for _, run := range runs.Runs {
					if run.Status == "failed" {
						failedRuns++
					}
				}
				counted := 0
				for _, e := range doc.Envs {
					counted += e.Failed
				}
				if counted != failedRuns {
					t.Errorf("%d failed runs, GET /v1/applies %d", counted, failedRuns)
				}
			})
		}
	})
}

// GET /v1/overview counts the plans of each environment as GET
// /v1/plans?env= answers them: 200 newer pending plans of an environment
// writ serve no longer serves leave prod's pending plan in it.
func TestOverviewPlansOfEachEnv(t *testing.T) {
	controltest.Each(t, func(t *testing.T, e controltest.Engine) {
		f := driftFixtureOn(t, e, &fakeTofu{})
		waiting, _ := f.planCommit(tofuFiles(), "waits")
		if waiting.Plan.Status != "pending" {
			t.Fatalf("plan %+v", waiting.Plan)
		}
		copyPlan(t, f.loc, waiting.Plan.ID, "retired", 200)
		r := f.do("GET", "/v1/overview", f.token(nil), nil)
		var doc server.OverviewDoc
		if r.status != http.StatusOK || json.Unmarshal(r.body, &doc) != nil {
			t.Fatalf("GET /v1/overview: %d %s", r.status, r.body)
		}
		var pending []string
		for _, p := range doc.Pending {
			pending = append(pending, p.Plan.ID)
		}
		if !slices.Equal(pending, []string{waiting.Plan.ID}) || len(doc.Envs) != 1 || doc.Envs[0].Plans.Pending != 1 {
			t.Errorf("the pending plans %v and envs %+v, want prod's %s", pending, doc.Envs, waiting.Plan.ID)
		}
	})
}

// GET /v1/overview sorts a plan by the status GET /v1/plans/{id} shows, which
// its approvals and the rules give it now: bob approves app's plan under a
// rule that needs one approval, writ serve starts again with a rule that
// needs two, and the plan waits again in the overview's list, its
// environment's counts, and its stack's counts.
func TestOverviewStatusOfNow(t *testing.T) {
	controltest.Each(t, func(t *testing.T, e controltest.Engine) {
		f := driftFixtureOn(t, e, &fakeTofu{})
		waiting, _ := f.planCommit(tofuFiles(), "waits")
		if r := f.do("POST", "/v1/plans/"+waiting.Plan.ID+"/approvals", bob(f), nil); r.status != http.StatusOK {
			t.Fatalf("bob's approval: %d %s", r.status, r.body)
		}
		check := func(status string, counts server.LiveCounts, pending []string) {
			t.Helper()
			var plan server.RemotePlan
			if r := f.do("GET", "/v1/plans/"+waiting.Plan.ID, f.token(nil), nil); r.status != http.StatusOK || json.Unmarshal(r.body, &plan) != nil || plan.Plan.Status != status {
				t.Fatalf("GET /v1/plans/%s: %d %s, want %s", waiting.Plan.ID, r.status, r.body, status)
			}
			r := f.do("GET", "/v1/overview", f.token(nil), nil)
			var doc server.OverviewDoc
			if r.status != http.StatusOK || json.Unmarshal(r.body, &doc) != nil {
				t.Fatalf("GET /v1/overview: %d %s", r.status, r.body)
			}
			var ids []string
			for _, p := range doc.Pending {
				ids = append(ids, p.Plan.ID)
				if p.Plan.Status != "pending" {
					t.Errorf("the waiting list holds plan %s of status %s", p.Plan.ID, p.Plan.Status)
				}
			}
			if !slices.Equal(ids, pending) {
				t.Errorf("the waiting list %v, want %v", ids, pending)
			}
			if len(doc.Envs) != 1 || doc.Envs[0].Plans != counts {
				t.Errorf("the counts of prod %+v, want %+v", doc.Envs, counts)
			}
			i := slices.IndexFunc(doc.Stacks, func(st server.OverviewStack) bool { return st.Stack == "app" })
			if i < 0 || doc.Stacks[i].Plans != counts {
				t.Errorf("the stacks %+v, want app with %+v", doc.Stacks, counts)
			}
		}
		check("approved", server.LiveCounts{Approved: 1}, nil)
		f.restart(func(text string) string { return strings.ReplaceAll(text, "need = 1", "need = 2") })
		check("pending", server.LiveCounts{Pending: 1}, []string{waiting.Plan.ID})
	})
}

// GET /v1/plans?status= lists a plan by the status each item shows, which
// its approvals and the rules give it now: bob approves app's plan under a
// rule that needs one approval, writ serve starts again with a rule that
// needs two, and ?status=approved stops listing the plan while
// ?status=pending lists it.
func TestPlansStatusOfNow(t *testing.T) {
	controltest.Each(t, func(t *testing.T, e controltest.Engine) {
		f := driftFixtureOn(t, e, &fakeTofu{})
		waiting, _ := f.planCommit(tofuFiles(), "waits")
		if r := f.do("POST", "/v1/plans/"+waiting.Plan.ID+"/approvals", bob(f), nil); r.status != http.StatusOK {
			t.Fatalf("bob's approval: %d %s", r.status, r.body)
		}
		check := func(status string) {
			t.Helper()
			for _, filter := range []string{"pending", "approved"} {
				var plans server.RemotePlans
				if r := f.do("GET", "/v1/plans?env=prod&status="+filter, f.token(nil), nil); r.status != http.StatusOK || json.Unmarshal(r.body, &plans) != nil {
					t.Fatalf("GET /v1/plans?status=%s: %d %s", filter, r.status, r.body)
				}
				var ids []string
				for _, p := range plans.Plans {
					ids = append(ids, p.ID)
					if p.Status != filter {
						t.Errorf("?status=%s lists plan %s, which shows %s", filter, p.ID, p.Status)
					}
				}
				if want := filter == status; slices.Contains(ids, waiting.Plan.ID) != want {
					t.Errorf("?status=%s lists %v; want plan %s listed: %v", filter, ids, waiting.Plan.ID, want)
				}
			}
		}
		check("approved")
		f.restart(func(text string) string { return strings.ReplaceAll(text, "need = 1", "need = 2") })
		check("pending")
	})
}

// GET /v1/overview counts a plan once while its status changes between
// its reads of each status: app's plan flips between pending and applying
// while 50 approved plans keep the reads between the two long, and no
// answer lists the plan twice or counts more plans than there are.
func TestOverviewPlanChangesStatus(t *testing.T) {
	controltest.Each(t, func(t *testing.T, e controltest.Engine) {
		f := driftFixtureOn(t, e, &fakeTofu{})
		waiting, _ := f.planCommit(tofuFiles(), "waits")
		if waiting.Plan.Status != "pending" {
			t.Fatalf("plan %+v", waiting.Plan)
		}
		const approved = 50
		copyPlan(t, f.loc, waiting.Plan.ID, "prod", approved)
		raw := controltest.Raw(t, f.loc)
		if _, err := raw.ExecContext(context.Background(), "UPDATE plans SET status = 'approved' WHERE ref LIKE 'prod-ref-%'"); err != nil {
			t.Fatal(err)
		}
		ctx, stop := context.WithCancel(context.Background())
		flipped := make(chan error, 1)
		go func() {
			for ctx.Err() == nil {
				if _, err := raw.ExecContext(ctx, "UPDATE plans SET status = CASE status WHEN 'pending' THEN 'applying' ELSE 'pending' END WHERE id = ?", waiting.Plan.ID); err != nil && ctx.Err() == nil {
					flipped <- err
					return
				}
				if e.Name == controltest.SQLite.Name {
					// A pause lets the overview's audit row past SQLite's one writer.
					time.Sleep(10 * time.Millisecond)
				}
			}
			flipped <- nil
		}()
		total := func(c server.LiveCounts) int { return c.Planning + c.Pending + c.Approved + c.Applying }
		// The overview answers for two seconds and at least ten times, which
		// is many fast answers and a few slow ones under the race detector.
		twice, answers := 0, 0
		for deadline := time.Now().Add(2 * time.Second); answers < 10 || time.Now().Before(deadline); answers++ {
			r := f.do("GET", "/v1/overview", f.token(nil), nil)
			var doc server.OverviewDoc
			if r.status != http.StatusOK || json.Unmarshal(r.body, &doc) != nil || len(doc.Envs) != 1 {
				stop()
				t.Fatalf("GET /v1/overview: %d %s", r.status, r.body)
			}
			listed := 0
			for _, lp := range slices.Concat(doc.Pending, doc.Applying, doc.Planning) {
				if lp.Plan.ID == waiting.Plan.ID {
					listed++
				}
			}
			i := slices.IndexFunc(doc.Stacks, func(st server.OverviewStack) bool { return st.Stack == "app" })
			if listed > 1 || total(doc.Envs[0].Plans) > approved+1 || i >= 0 && total(doc.Stacks[i].Plans) > approved+1 {
				twice++
			}
		}
		stop()
		if err := <-flipped; err != nil {
			t.Fatal(err)
		}
		if twice > 0 {
			t.Errorf("%d of %d answers counted one plan twice", twice, answers)
		}
	})
}

// GET /v1/overview counts drift over every stack the principal may read,
// not only the 200 it lists: with 200 stacks named before app and web, app's
// drift still counts and still alerts, and the audit row names app.
func TestOverviewDriftPastTheList(t *testing.T) {
	controltest.Each(t, func(t *testing.T, e controltest.Engine) {
		runner := &fakeTofu{}
		f := driftFixtureOn(t, e, runner)
		runner.mu.Lock()
		runner.plan = driftPlan(true)
		runner.mu.Unlock()
		f.srv.LookForDrift(time.Hour)
		runner.mu.Lock()
		runner.plan = driftPlan(false)
		runner.mu.Unlock()
		f.srv.LookForDrift(time.Hour)
		for i := range 200 {
			if _, err := f.store.ReplaceIndex(context.Background(), control.IndexWrite{Env: "prod", Stack: fmt.Sprintf("a%03d", i), Backend: control.BackendHTTP, Version: 1, At: time.Now()}); err != nil {
				t.Fatal(err)
			}
		}
		r := f.do("GET", "/v1/overview", f.token(nil), nil)
		var doc server.OverviewDoc
		if r.status != http.StatusOK || json.Unmarshal(r.body, &doc) != nil {
			t.Fatalf("GET /v1/overview: %d %s", r.status, r.body)
		}
		if len(doc.Stacks) != 200 || !doc.StacksMore || slices.ContainsFunc(doc.Stacks, func(st server.OverviewStack) bool { return st.Stack == "app" }) {
			t.Fatalf("the overview lists %d stacks, more %v, want the 200 named before app and more", len(doc.Stacks), doc.StacksMore)
		}
		want := server.OverviewEnv{Name: "prod", Stacks: 202, Checked: 2, Drifted: 1}
		if len(doc.Envs) != 1 || doc.Envs[0] != want {
			t.Errorf("envs %+v, want %+v", doc.Envs, want)
		}
		var alerts []string
		for _, a := range doc.Alerts {
			alerts = append(alerts, a.Kind+" "+a.Stack)
		}
		if !slices.Equal(alerts, []string{"drifted app"}) || doc.Envs[0].AlertsMore {
			t.Errorf("alerts %v, more %v, want app's drift", alerts, doc.Envs[0].AlertsMore)
		}
		if rows := f.audit(controltest.AuditFilter{Stack: "app", Limit: 1}); len(rows) != 1 || !strings.Contains(rows[0].Route, "/v1/overview") {
			t.Errorf("the newest audit row that names app: %+v, want the overview's", rows)
		}
	})
}

// GET /v1/overview counts every failed run of the last day, not only those
// among each environment's newest 200 runs, and marks the plan counts of
// an environment that holds more than 200 plans of a status as floors.
func TestOverviewCountsPastTheList(t *testing.T) {
	controltest.Each(t, func(t *testing.T, e controltest.Engine) {
		runner := &fakeTofu{}
		f := driftFixtureOn(t, e, runner)
		failing, _ := f.planCommit(tofuFiles(), "fails")
		if r := f.do("POST", "/v1/plans/"+failing.Plan.ID+"/approvals", bob(f), nil); r.status != http.StatusOK {
			t.Fatalf("bob's approval: %d %s", r.status, r.body)
		}
		runner.mu.Lock()
		runner.apply = func(context.Context, stateCall) ([]byte, error) {
			return nil, &tofu.StepError{Step: "apply", Code: 1}
		}
		runner.mu.Unlock()
		failed := f.tofuRun(bob(f), map[string]any{"plan": failing.Plan.ID})
		if failed.Run.Status != "failed" {
			t.Fatalf("run %+v", failed.Run)
		}
		waiting, _ := f.planCommit(tofuFiles(), "waits")
		if waiting.Plan.Status != "pending" {
			t.Fatalf("plan %+v", waiting.Plan)
		}
		copyPlan(t, f.loc, waiting.Plan.ID, "prod", 200)
		copyRun(t, f.loc, failed.Run.ID, "applied", 200)

		r := f.do("GET", "/v1/overview", f.token(nil), nil)
		var doc server.OverviewDoc
		if r.status != http.StatusOK || json.Unmarshal(r.body, &doc) != nil || len(doc.Envs) != 1 {
			t.Fatalf("GET /v1/overview: %d %s", r.status, r.body)
		}
		if env := doc.Envs[0]; env.Failed != 1 || !env.PlansMore || env.Plans.Pending != 200 {
			t.Errorf("prod %+v, want 1 failed run and 200 pending plans or more", env)
		}
		if !slices.ContainsFunc(doc.Alerts, func(a server.Alert) bool { return a.Kind == "run-failed" && a.Run == failed.Run.ID }) {
			t.Errorf("no alert of run %s: %+v", failed.Run.ID, doc.Alerts)
		}
	})
}

// copyPlan writes n copies of the plan with id into env, each newer than
// it, as rows a config that dropped env leaves behind.
func copyPlan(t *testing.T, loc, id, env string, n int) {
	t.Helper()
	copyRows(t, loc, "plans", id, n, func(i int, col string, v any) any {
		switch col {
		case "ref":
			return fmt.Sprintf("%s-ref-%d", env, i)
		case "id":
			return fmt.Sprintf("%s-id-%d", env, i)
		case "env":
			return env
		case "requested_at", "seq":
			return v.(int64) + int64(i) + 1
		}
		return v
	})
}

// copyRun writes n runs with status, each a copy of the run with id that
// started and ended after it, of its own copy of the run's plan.
func copyRun(t *testing.T, loc, id, status string, n int) {
	t.Helper()
	var plan string
	if err := controltest.Raw(t, loc).QueryRowContext(context.Background(), "SELECT plan_id FROM runs WHERE id = ?", id).Scan(&plan); err != nil {
		t.Fatal(err)
	}
	copyRows(t, loc, "plans", plan, n, func(i int, col string, v any) any {
		switch col {
		case "ref", "id":
			return fmt.Sprintf("%v-run-%d", v, i)
		case "seq":
			return v.(int64) + int64(i) + 1000
		}
		return v
	})
	copyRows(t, loc, "runs", id, n, func(i int, col string, v any) any {
		switch col {
		case "id":
			return fmt.Sprintf("%s-%d", id, i)
		case "plan_id":
			return fmt.Sprintf("%s-run-%d", plan, i)
		case "status":
			return status
		case "started_at", "ended_at":
			return v.(int64) + int64(i) + 1
		}
		return v
	})
}

// copyRows writes n copies of the row of table with id, each with the
// columns edit gives the ith copy.
func copyRows(t *testing.T, loc, table, id string, n int, edit func(i int, col string, v any) any) {
	t.Helper()
	ctx := context.Background()
	raw := controltest.Raw(t, loc)
	rows, err := raw.QueryContext(ctx, "SELECT * FROM "+table+" WHERE id = ?", id)
	if err != nil {
		t.Fatal(err)
	}
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if !rows.Next() {
		t.Fatalf("no row %s in %s", id, table)
	}
	if err := rows.Scan(ptrs...); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	marks := "(" + strings.TrimSuffix(strings.Repeat("?, ", len(cols)), ", ") + ")"
	var tuples []string
	var args []any
	for i := range n {
		for j, c := range cols {
			args = append(args, edit(i, c, vals[j]))
		}
		tuples = append(tuples, marks)
	}
	if _, err := raw.ExecContext(ctx, "INSERT INTO "+table+" ("+strings.Join(cols, ", ")+") VALUES "+strings.Join(tuples, ", "), args...); err != nil {
		t.Fatal(err)
	}
}

// restart starts writ serve again on the fixture's control database, with
// the config edit makes of the fixture's.
func (f *fixture) restart(edit func(string) string) {
	f.t.Helper()
	f.http.Close()
	f.srv.Close()
	cfg, diags := server.ParseConfig(filepath.Join(f.dir, "serve"), []byte(edit(f.cfg.Text)))
	if diags.HasErrors() {
		f.t.Fatal(diags)
	}
	v, err := server.NewVerifier(context.Background(), cfg, nil, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	srv, err := server.New(context.Background(), server.Options{Config: cfg, Store: f.store, Verifier: v, Tofu: f.tofu, Reap: func(uint32) error { return nil }, Version: "test"})
	if err != nil {
		f.t.Fatal(err)
	}
	h := httptest.NewServer(srv.Handler())
	f.t.Cleanup(func() { h.Close(); srv.Close() })
	f.cfg, f.srv, f.http = cfg, srv, h
	f.tofu.base = h.URL
}

// GET /v1/plans pages over the plans the caller may read: a reader of web
// gets no page and no cursor from app's plans, however many there are.
func TestPlansPageOnlyReadable(t *testing.T) {
	controltest.Each(t, func(t *testing.T, e controltest.Engine) {
		f := driftFixtureOn(t, e, &fakeTofu{})
		waiting, _ := f.planCommit(tofuFiles(), "waits")
		copyRows(t, f.loc, "plans", waiting.Plan.ID, 60, func(i int, col string, v any) any {
			switch col {
			case "ref":
				return fmt.Sprintf("app-ref-%d", i)
			case "id":
				return fmt.Sprintf("app-id-%d", i)
			case "requested_at", "seq":
				return v.(int64) + int64(i) + 1
			}
			return v
		})
		r := f.do("GET", "/v1/plans?env=prod&limit=50", f.token(map[string]any{"project_id": "4343"}), nil)
		var doc server.RemotePlans
		if r.status != http.StatusOK || json.Unmarshal(r.body, &doc) != nil {
			t.Fatalf("GET /v1/plans: %d %s", r.status, r.body)
		}
		if len(doc.Plans) != 0 || doc.Next != "" {
			t.Fatalf("a reader of web got %d plans and the cursor %q, want none of app's", len(doc.Plans), doc.Next)
		}
	})
}
