package server_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/cli"
	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/control/controltest"
	"github.com/mr-karan/writ/internal/server"
)

// mainSub is the subject of the jobs on main, a principal other than the
// merge request jobs'.
const mainSub = "project_path:acme/infra:ref_type:branch:ref:main"

// mainApplier is the apply job on main after a merge, with the subject
// GitLab gives a job of main.
func mainApplier(f *fixture, sha string) string {
	return f.token(map[string]any{"sha": sha, "ref": "main", "ref_path": "refs/heads/main", "ref_protected": "true",
		"pipeline_source": "push", "pipeline_id": "1849290", "job_id": "9922150", "sub": mainSub})
}

// approvedMerge plans a change on a merge request's branch, has bob
// approve it, merges it, and returns the merged commit.
func approvedMerge(f *fixture, branch, pipeline, extra string) string {
	f.t.Helper()
	files := changed(extra)
	p := f.mrPlan(f.gitlab.Commit(4242, branch, files), branch, "network", pipeline)
	if res := f.do("POST", "/v1/plans/"+p.Plan.ID+"/approvals", bob(f, map[string]any{"pipeline_id": pipeline}), nil); res.status != http.StatusOK {
		f.t.Fatalf("approve: %d %s", res.status, res.body)
	}
	return f.gitlab.Commit(4242, "main", files)
}

// apply sends POST /v1/runs with a key.
func (f *fixture) apply(tok, key string, body map[string]any) response {
	f.t.Helper()
	return f.do("POST", "/v1/runs", tok, runRequest(body), server.ApplyKeyHeader, key)
}

func (f *fixture) outcome(tok, key string) response {
	f.t.Helper()
	return f.do("POST", "/v1/apply-requests/"+key+"/outcome", tok, nil)
}

func runOf(r response) string {
	run, _ := r.doc["run"].(map[string]any)
	id, _ := run["id"].(string)
	return id
}

func merged() map[string]any {
	return map[string]any{"env": "prod", "stacks": []string{"network"}, "merged": map[string]any{}}
}

// A repeat of a key that started a run answers 202 with the same run, from
// the same CI token, which spends nothing more: without the key the token
// is refused as used. Another request with the key is refused. Requests
// with one key at the same time start one run.
func TestApplyKeyRepeat(t *testing.T) {
	f := planFixture(t, withApprovals(t))
	gate := holdApplies(f.tofu)
	defer close(gate)
	f.addInfra()
	sha := approvedMerge(f, "mr", "400", "resource \"random_pet\" \"pet2\" {}\n")
	tok := mainApplier(f, sha)
	key := server.NewApplyKey(time.Now())
	first := f.apply(tok, key, merged())
	if first.status != http.StatusAccepted {
		t.Fatalf("apply: %d %s", first.status, first.body)
	}
	again := f.apply(tok, key, merged())
	if again.status != http.StatusAccepted || runOf(again) != runOf(first) {
		t.Fatalf("a repeat of the key: %d %s", again.status, again.body)
	}
	var uses int
	if err := controltest.Raw(t, f.loc).QueryRowContext(context.Background(), "SELECT count(*) FROM token_uses WHERE op = 'apply'").Scan(&uses); err != nil || uses != 1 {
		t.Fatalf("%d token uses after a repeat, %v; want 1", uses, err)
	}
	other := map[string]any{"env": "prod", "stacks": []string{"network"}, "merged": map[string]any{"repo": "infra"}}
	if res := f.apply(tok, key, other); res.status != http.StatusBadRequest || res.class() != "usage" || !strings.Contains(string(res.body), "another request") {
		t.Fatalf("the key with another request: %d %s", res.status, res.body)
	}
	if res := f.apply(bob(f), key, merged()); res.status != http.StatusForbidden {
		t.Fatalf("the key from another principal: %d %s", res.status, res.body)
	}
	// The run reaches OpenTofu, where the gate holds it, in its own time.
	for deadline := time.Now().Add(time.Minute); len(f.tofu.appliedSoFar()) == 0 && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	if n := len(f.tofu.appliedSoFar()); n != 1 {
		t.Fatalf("%d applies", n)
	}

	// Two requests with one key at once.
	dr := &fakeTofu{}
	dgate := holdApplies(dr)
	defer close(dgate)
	d, devTok := devServer(t, dr)
	id := d.devPlan(devTok).Plan.ID
	both := server.NewApplyKey(time.Now())
	var wg sync.WaitGroup
	runs := make([]string, 2)
	for i := range runs {
		wg.Go(func() {
			res := d.apply(devTok, both, map[string]any{"plan": id, "override": "a race of one key, OPS-1"})
			if res.status != http.StatusAccepted {
				t.Errorf("request %d: %d %s", i, res.status, res.body)
			}
			runs[i] = runOf(res)
		})
	}
	wg.Wait()
	if runs[0] == "" || runs[0] != runs[1] || len(dr.appliedSoFar()) > 1 {
		t.Fatalf("two requests with one key started %v", runs)
	}
}

// A request refused before its commit leaves no row, so the command can
// send the key again. A key the outcome route closed starts no run, and
// the CI token stays unspent.
func TestApplyKeyRolledBack(t *testing.T) {
	f := planFixture(t, withApprovals(t))
	f.addInfra()
	files := changed("resource \"random_pet\" \"pet2\" {}\n")
	p := f.mrPlan(f.gitlab.Commit(4242, "mr", files), "mr", "network", "400")
	sha := f.gitlab.Commit(4242, "main", files)
	tok := mainApplier(f, sha)
	first := server.NewApplyKey(time.Now())
	if res := f.apply(tok, first, merged()); res.status != http.StatusForbidden {
		t.Fatalf("before the approval: %d %s", res.status, res.body)
	}
	if _, err := f.store.Key(context.Background(), first); !errors.Is(err, control.ErrNotFound) {
		t.Fatalf("a refused request left its key's row: %v", err)
	}
	if res := f.do("POST", "/v1/plans/"+p.Plan.ID+"/approvals", bob(f, map[string]any{"pipeline_id": "400"}), nil); res.status != http.StatusOK {
		t.Fatalf("approve: %d %s", res.status, res.body)
	}
	closed := server.NewApplyKey(time.Now())
	if res := f.outcome(tok, closed); res.status != http.StatusOK || res.doc["outcome"] != "none" || res.doc["run"] != nil {
		t.Fatalf("the outcome of a new key: %d %s", res.status, res.body)
	}
	res := f.apply(tok, closed, merged())
	if res.status != http.StatusConflict || res.class() != "state" || !strings.Contains(string(res.body), "closed with no run") {
		t.Fatalf("a closed key: %d %s", res.status, res.body)
	}
	if got, _ := f.store.Plan(context.Background(), p.Plan.ID, time.Now()); got.Status != control.Approved {
		t.Fatalf("the plan is %s after a closed key", got.Status)
	}
	if res := f.apply(tok, first, merged()); res.status != http.StatusAccepted {
		t.Fatalf("the refused request's key, with the same token: %d %s", res.status, res.body)
	}
}

// Only the principal of a started run and an admin learn the run. Any
// principal that knows a key with no run can close it.
func TestApplyOutcomeAuthorization(t *testing.T) {
	f := planFixture(t, withApprovals(t))
	gate := holdApplies(f.tofu)
	defer close(gate)
	f.addInfra()
	sha := approvedMerge(f, "mr", "400", "resource \"random_pet\" \"pet2\" {}\n")
	tok := mainApplier(f, sha)
	stranger := f.token(nil)

	// The key of a request not sent yet, which the job printed, is closed
	// by another principal: the apply with it starts nothing.
	disclosed := server.NewApplyKey(time.Now())
	if res := f.outcome(stranger, disclosed); res.status != http.StatusOK || res.doc["outcome"] != "none" {
		t.Fatalf("another principal closes a disclosed key: %d %s", res.status, res.body)
	}
	if res := f.apply(tok, disclosed, merged()); res.status != http.StatusConflict || !strings.Contains(string(res.body), "closed with no run") {
		t.Fatalf("an apply with the closed key: %d %s", res.status, res.body)
	}
	if n := len(f.tofu.appliedSoFar()); n != 0 {
		t.Fatalf("%d applies with a closed key", n)
	}

	key := server.NewApplyKey(time.Now())
	started := f.apply(tok, key, merged())
	if started.status != http.StatusAccepted {
		t.Fatalf("apply: %d %s", started.status, started.body)
	}
	for who, tok := range map[string]string{"the run's principal": tok, "an admin": bob(f)} {
		res := f.outcome(tok, key)
		if res.status != http.StatusOK || res.doc["format"] != server.FormatOutcome || res.doc["outcome"] != "started" || runOf(res) != runOf(started) {
			t.Errorf("%s: %d %s", who, res.status, res.body)
		}
	}
	res := f.outcome(stranger, key)
	if res.status != http.StatusForbidden || res.class() != "denied" || strings.Contains(string(res.body), runOf(started)) {
		t.Fatalf("another principal: %d %s", res.status, res.body)
	}
	denied := f.audit(controltest.AuditFilter{Decision: control.Denied})
	if len(denied) == 0 || denied[0].Route != "/v1/apply-requests/{key}/outcome" || denied[0].Run != runOf(started) {
		t.Fatalf("the audit rows of refusals: %+v", denied)
	}
	if res := f.outcome("", key); res.status != http.StatusUnauthorized {
		t.Fatalf("no credential: %d %s", res.status, res.body)
	}
	if res := f.outcome(tok, "1794000000-SHORT"); res.status != http.StatusBadRequest || res.class() != "usage" {
		t.Fatalf("a malformed key: %d %s", res.status, res.body)
	}
}

// A commit of a run's start that returns an error is settled by the key:
// a durable run applies, a rolled back one answers 503 with outcome none.
// When the settle fails too, the answer has outcome unknown, the apply
// slot stays held, and a settle in the background ends it within its
// deadline. The background settles wait until the test has seen the slot
// held, so a loaded machine cannot settle the key before the test looks.
func TestApplyCommitError(t *testing.T) {
	lost := errors.New("the commit's answer was lost")
	durable := func(commit func() error) error {
		if err := commit(); err != nil {
			return err
		}
		return lost
	}
	rolledBack := func(func() error) error { return lost }
	for _, c := range []struct {
		name string
		step func(func() error) error
		// fails is how many settles fail first, -1 for every one.
		fails int
		// status and outcome are the answer's, ran whether a run applies.
		status  int
		outcome string
		ran     bool
	}{
		{"durable", durable, 0, http.StatusAccepted, "", true},
		{"rolled back", rolledBack, 0, http.StatusServiceUnavailable, "none", false},
		{"durable, settled later", durable, 2, http.StatusServiceUnavailable, "unknown", true},
		{"rolled back, settled later", rolledBack, 2, http.StatusServiceUnavailable, "unknown", false},
		{"durable, never settled", durable, -1, http.StatusServiceUnavailable, "unknown", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, tok := devServer(t, &fakeTofu{})
			var fails, calls atomic.Int32
			fails.Store(int32(c.fails))
			looked := make(chan struct{})
			sawSlot := sync.OnceFunc(func() { close(looked) })
			t.Cleanup(sawSlot)
			f.srv.HoldCommit(c.step)
			f.srv.CloseKeyWith(func(ctx context.Context, key string, issued, now time.Time) (control.KeyRow, error) {
				// The first settle answers the request; the rest are in
				// the background.
				if calls.Add(1) > 1 {
					<-looked
				}
				if fails.Load() != 0 {
					fails.Add(-1)
					return control.KeyRow{}, errors.New("the control database is down")
				}
				return f.store.CloseKey(ctx, key, issued, now)
			}, 50*time.Millisecond, 2*time.Second)
			id := f.devPlan(tok).Plan.ID
			key := server.NewApplyKey(time.Now())
			res := f.apply(tok, key, map[string]any{"plan": id, "override": "a lost commit, OPS-2"})
			if res.status != c.status || (res.doc["outcome"] != nil || c.outcome != "") && res.doc["outcome"] != c.outcome {
				t.Fatalf("answer %d %s; want %d with outcome %q", res.status, res.body, c.status, c.outcome)
			}
			if c.outcome == "unknown" && f.srv.Applies() != 1 {
				t.Fatalf("%d apply slots held while the key is not settled, want 1", f.srv.Applies())
			}
			sawSlot()
			deadline := time.Now().Add(time.Minute)
			for f.srv.Applies() != 0 && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			if n := f.srv.Applies(); n != 0 {
				t.Fatalf("%d apply slots held after the settle", n)
			}
			if got := len(f.tofu.appliedSoFar()); got != map[bool]int{true: 1, false: 0}[c.ran] {
				t.Fatalf("%d applies, want ran=%v", got, c.ran)
			}
			fails.Store(0)
			out := f.outcome(tok, key)
			if out.status != http.StatusOK {
				t.Fatalf("outcome: %d %s", out.status, out.body)
			}
			if out.doc["outcome"] == "none" {
				if c.ran || out.doc["outcome"] != "none" {
					t.Fatalf("outcome %s", out.body)
				}
				return
			}
			// A durable run whose settle never answered stays applying
			// with no work behind it until the sweep fails it.
			run := runOf(out)
			if !c.ran {
				f.srv.Sweep()
			}
			got := f.waitRun(tok, run)
			want := "applied"
			if !c.ran {
				want = "failed"
			}
			if got.Run.Status != want {
				t.Fatalf("run %s is %s, want %s: %s", run, got.Run.Status, want, got.Text)
			}
		})
	}
}

// A run's start that an earlier process's transaction commits after a new
// process started on the same database is failed by the new process's
// sweep. The start-up scan alone cannot see it, since it commits later.
func TestRestartWithAnUnresolvedCommit(t *testing.T) {
	if controltest.Default().Name != "postgres" {
		t.Skip("SQLite's write lock makes a new process wait for the commit, and its start fails the run")
	}
	f := planFixture(t, withApprovals(t))
	f.addInfra()
	sha := approvedMerge(f, "mr", "400", "resource \"random_pet\" \"pet2\" {}\n")
	held, release, dead := make(chan struct{}), make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(dead) })
	f.srv.HoldCommit(func(commit func() error) error {
		close(held)
		<-release
		err := commit()
		// The first process stops after its commit: it does no work.
		<-dead
		return err
	})
	key := server.NewApplyKey(time.Now())
	go func() {
		c := http.Client{Timeout: 2 * time.Second}
		req, _ := http.NewRequest("POST", f.http.URL+"/v1/runs", runRequest(merged()))
		req.Header.Set("Authorization", "Bearer "+mainApplier(f, sha))
		req.Header.Set(server.ApplyKeyHeader, key)
		if resp, err := c.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	<-held
	again, err := server.New(context.Background(), server.Options{Config: f.cfg, Store: f.store, Tofu: &fakeTofu{}, Reap: func(uint32) error { return nil }, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	close(release)
	var row control.KeyRow
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if row, err = f.store.Key(context.Background(), key); err == nil {
			break
		}
	}
	if row.Outcome != control.KeyStarted {
		t.Fatalf("the held commit: %+v, %v", row, err)
	}
	if run, _ := f.store.GetRun(context.Background(), row.RunID); run.Status != control.RunApplying {
		t.Fatalf("the late run is %s before the sweep", run.Status)
	}
	again.Sweep()
	run, err := f.store.GetRun(context.Background(), row.RunID)
	if err != nil || run.Status != control.RunFailed || !strings.Contains(run.Text, "stopped during this run") {
		t.Fatalf("after the new process's sweep: %+v, %v", run, err)
	}
}

// A closed key's none row is deleted once the key is past its life, and
// the key is still refused: the server refuses a key past its life
// before it reads the key's row.
func TestPruneThenReplay(t *testing.T) {
	c := newClock(time.Now())
	f := planFixture(t, withApprovals(t), c.set())
	f.addInfra()
	sha := approvedMerge(f, "mr", "400", "resource \"random_pet\" \"pet2\" {}\n")
	key := server.NewApplyKey(time.Now())
	if res := f.outcome(bob(f), key); res.status != http.StatusOK || res.doc["outcome"] != "none" {
		t.Fatalf("close the key: %d %s", res.status, res.body)
	}
	c.add(control.KeyLife + 2*time.Hour)
	got, err := f.store.Prune(context.Background(), c.now(), control.Retention{})
	if err != nil || got.Keys != 1 {
		t.Fatalf("prune: %+v, %v", got, err)
	}
	if _, err := f.store.Key(context.Background(), key); !errors.Is(err, control.ErrNotFound) {
		t.Fatalf("the none row after the prune: %v", err)
	}
	res := f.apply(mainApplier(f, sha), key, merged())
	if res.status != http.StatusBadRequest || !strings.Contains(string(res.body), "starts a run for 24h0m0s") {
		t.Fatalf("a replay of the pruned key: %d %s", res.status, res.body)
	}
	if _, err := f.store.Key(context.Background(), key); !errors.Is(err, control.ErrNotFound) {
		t.Fatalf("the replay wrote a row: %v", err)
	}
}

// Every 503 of POST /v1/runs says whether the request started a run.
func TestApplyRequest503HasOutcome(t *testing.T) {
	f, tok := devServer(t, &fakeTofu{})
	f.srv.HoldCommit(func(func() error) error { return errors.New("lost") })
	f.srv.CloseKeyWith(func(context.Context, string, time.Time, time.Time) (control.KeyRow, error) {
		return control.KeyRow{}, errors.New("down")
	}, time.Hour, time.Hour)
	id := f.devPlan(tok).Plan.ID
	res := f.apply(tok, server.NewApplyKey(time.Now()), map[string]any{"plan": id, "override": "a lost commit, OPS-3"})
	if res.status != http.StatusServiceUnavailable || res.doc["outcome"] != "unknown" || !strings.Contains(string(res.body), "writ show --remote --request") {
		t.Fatalf("an unsettled start: %d %s", res.status, res.body)
	}
	out := f.outcome(tok, server.NewApplyKey(time.Now()))
	if out.status != http.StatusServiceUnavailable || out.doc["outcome"] != "unknown" {
		t.Fatalf("the outcome route with the database down: %d %s", out.status, out.body)
	}
}

// A key whose none row was pruned stays closed after the clock of writ
// serve moves back, so it starts no run: the prune keeps the greatest
// time of a key it deleted, and BeginApply refuses a key at or below it.
func TestPruneThenClockBack(t *testing.T) {
	start := time.Now()
	c := newClock(start)
	f := planFixture(t, withApprovals(t), c.set())
	f.addInfra()
	key := server.NewApplyKey(start)
	if res := f.outcome(bob(f), key); res.status != http.StatusOK || res.doc["outcome"] != "none" {
		t.Fatalf("close the key: %d %s", res.status, res.body)
	}
	c.add(26 * time.Hour)
	if got, err := f.store.Prune(context.Background(), c.now(), control.Retention{}); err != nil || got.Keys != 1 {
		t.Fatalf("prune: %+v, %v", got, err)
	}
	c.add(-26 * time.Hour)
	sha := approvedMerge(f, "mr", "400", "resource \"random_pet\" \"pet2\" {}\n")
	tok := mainApplier(f, sha)
	res := f.apply(tok, key, merged())
	if res.status != http.StatusConflict || res.class() != "state" || res.doc["key_closed"] != true {
		t.Fatalf("the pruned key after the clock moved back: %d %s", res.status, res.body)
	}
	if _, err := f.store.Key(context.Background(), key); !errors.Is(err, control.ErrNotFound) {
		t.Fatalf("the refusal wrote a row: %v", err)
	}
	// A key carries whole seconds: a key of a later second is above the
	// floor.
	c.add(time.Second)
	if res := f.apply(tok, server.NewApplyKey(c.now()), merged()); res.status != http.StatusAccepted {
		t.Fatalf("a new key with the same token: %d %s", res.status, res.body)
	}
}

// failCommits makes the next n commits of a run's start fail with no commit.
func failCommits(f *fixture, n int32) *atomic.Int32 {
	var left atomic.Int32
	left.Store(n)
	f.srv.HoldCommit(func(commit func() error) error {
		if left.Add(-1) >= 0 {
			return errors.New("the commit failed")
		}
		return commit()
	})
	return &left
}

// A start whose commit failed and whose settle found no run closes the
// key: the 503 and every later 409 for the key say key_closed, and
// writ apply --remote sends the request again with a new key.
func TestApplyKeyClosedBySettle(t *testing.T) {
	f := planFixture(t, withApprovals(t))
	f.addInfra()
	sha := approvedMerge(f, "mr", "400", "resource \"random_pet\" \"pet2\" {}\n")
	tok := mainApplier(f, sha)
	failCommits(f, 1)
	key := server.NewApplyKey(time.Now())
	res := f.apply(tok, key, merged())
	if res.status != http.StatusServiceUnavailable || res.doc["outcome"] != "none" || res.doc["key_closed"] != true {
		t.Fatalf("a start that settled to none: %d %s", res.status, res.body)
	}
	again := f.apply(tok, key, merged())
	if again.status != http.StatusConflict || again.class() != "state" || again.doc["key_closed"] != true {
		t.Fatalf("the closed key again: %d %s", again.status, again.body)
	}
	res = f.apply(tok, server.NewApplyKey(time.Now()), merged())
	if res.status != http.StatusAccepted {
		t.Fatalf("a new key: %d %s", res.status, res.body)
	}

	if run := f.waitRun(tok, runOf(res)); run.Run.Status != "applied" {
		t.Fatalf("the first apply: %s %s", run.Run.Status, run.Document)
	}

	sha = approvedMerge(f, "mr2", "401", "resource \"random_pet\" \"pet3\" {}\n")
	failCommits(f, 1)
	var out bytes.Buffer
	data := t.TempDir()
	tok = mainApplier(f, sha)
	a := cli.New(cli.Options{Dir: t.TempDir(), Server: f.http.URL, Env: "prod", Stdout: &out, LookupEnv: func(name string) (string, bool) {
		switch name {
		case "WRIT_ID_TOKEN":
			return tok, true
		case "XDG_CONFIG_HOME", "XDG_DATA_HOME":
			return filepath.Join(data, name), true
		}
		return "", false
	}})
	if err := a.RemoteApply(context.Background(), cli.RemoteApplyOptions{Merged: true, Stack: "network"}); err != nil {
		t.Fatalf("writ apply --remote: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "closed the key") || !strings.Contains(out.String(), "with a new key") {
		t.Fatalf("the output:\n%s", out.String())
	}
	var none, started int
	if err := controltest.Raw(t, f.loc).QueryRowContext(context.Background(),
		"SELECT count(*) FILTER (WHERE outcome = 'none'), count(*) FILTER (WHERE outcome = 'started') FROM apply_requests").Scan(&none, &started); err != nil {
		t.Fatal(err)
	}
	if none != 2 || started != 2 {
		t.Fatalf("%d none keys and %d started keys, want 2 and 2", none, started)
	}
}
