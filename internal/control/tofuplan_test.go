package control_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/control/controltest"
	"github.com/mr-karan/writ/internal/sqldb"
)

// tofuPlan makes a commit plan of an OpenTofu stack of stack@prod that
// read state version version, approved, and returns its id.
func (l *lifecycle) tofuPlan(stack string, at time.Time, version int64) string {
	l.t.Helper()
	ctx := context.Background()
	l.n++
	ref := fmt.Sprintf("REF%022d", l.n)
	id := fmt.Sprintf("%032x", l.n)
	p := newPlan(ref, l.cfg, "prod", stack, at)
	p.Source = control.PlanSource{Repo: "infra", Ref: "refs/heads/main", Commit: strings.Repeat("c", 40)}
	if err := l.s.CreatePlan(ctx, p, control.TokenUse{}, l.entry("plan")); err != nil {
		l.t.Fatal(err)
	}
	res := control.PlanResult{ID: id, Status: control.Approved, MadeAt: at, ExpiresAt: at.Add(24 * time.Hour), Risks: []string{"create"}, Changes: 1,
		Saved: []byte("sealed saved plan"), Tofu: &control.TofuPlan{StateVersion: version}}
	if err := l.s.FinishPlan(ctx, ref, res, l.entry("plan")); err != nil {
		l.t.Fatal(err)
	}
	return id
}

// writeState stores a new version of stack@prod's state as a run on the
// http backend writes it: with the run's token and its lock.
func writeState(t *testing.T, l *lifecycle, stack, run string, serial int64) {
	t.Helper()
	ctx := context.Background()
	hash := []byte("hash of " + run)
	if err := l.s.AddStateToken(ctx, hash, control.StateToken{RunID: run, Env: "prod", Stack: stack, Scope: control.StateApply, ExpiresAt: t0.Add(time.Hour)}, t0); err != nil {
		t.Fatal(err)
	}
	auth := control.StateAuth{Hash: hash, RunID: run, Now: t0}
	if _, err := l.s.LockState(ctx, auth, "prod", stack, control.StateLock{ID: "lock-" + run, RunID: run, Info: "{}", At: t0}, lockRow); err != nil {
		t.Fatal(err)
	}
	err := l.s.PutState(ctx, control.StatePut{Env: "prod", Stack: stack, Auth: auth, LockID: "lock-" + run, Keep: 5,
		Version: control.StateVersion{Serial: serial, Lineage: "lineage", SHA256: fmt.Sprintf("%064d", serial), RunID: run, At: t0},
		Seal:    func(int64) []byte { return []byte("sealed") },
		Audit:   control.Entry{At: t0, Request: "W" + run, Kind: "state-write", Op: "state-write", Env: "prod", Stacks: []string{stack}, Run: run, Decision: control.Allowed}})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.s.EndStateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
}

func savedOf(t *testing.T, s *control.Store, id string) []byte {
	t.Helper()
	saved, _, err := s.SavedPlan(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return saved
}

// An apply of an OpenTofu plan begins only while the plan is the newest
// of its stack@env and the stack's state is still the version it read.
// Each refusal supersedes the plan and deletes its saved plan.
func TestTofuStalePlans(t *testing.T) {
	each(t, func(t *testing.T, s *control.Store, loc string) {
		l := newLifecycle(t, s)
		var stale *control.StaleError

		older := l.tofuPlan("app", t0, 0)
		newer := l.tofuPlan("app", t0.Add(time.Minute), 0)
		_, err := l.begin(older, "RUN1", "", t0.Add(2*time.Minute))
		if !errors.As(err, &stale) || !strings.Contains(stale.Reason, "plan "+newer+" of app@prod is newer") {
			t.Fatalf("apply of a plan with a newer one: %v", err)
		}
		if st := l.status(older, t0.Add(2*time.Minute)); st != control.Superseded {
			t.Fatalf("the older plan is %s, want superseded", st)
		}
		if saved := savedOf(t, s, older); saved != nil {
			t.Fatalf("the superseded plan keeps its saved plan %q", saved)
		}
		if saved := savedOf(t, s, newer); string(saved) != "sealed saved plan" {
			t.Fatalf("the newest plan's saved plan is %q", saved)
		}

		// A state version that no run of this store began, as a write
		// outside a run would leave.
		writeState(t, l, "app", "OUTSIDE", 1)
		_, err = l.begin(newer, "RUN2", "", t0.Add(3*time.Minute))
		if !errors.As(err, &stale) || !strings.Contains(stale.Reason, "the state of app@prod is at version 1, and the plan read version 0") {
			t.Fatalf("apply after the state moved: %v", err)
		}
		if saved := savedOf(t, s, newer); saved != nil {
			t.Fatalf("the stale plan keeps its saved plan %q", saved)
		}

		// A plan that read the newest version begins, and its saved plan
		// is deleted when its run ends.
		fresh := l.tofuPlan("app", t0.Add(4*time.Minute), 1)
		if _, err := l.begin(fresh, "RUN3", "", t0.Add(5*time.Minute)); err != nil {
			t.Fatalf("apply of a fresh plan: %v", err)
		}
		if saved := savedOf(t, s, fresh); string(saved) != "sealed saved plan" {
			t.Fatalf("the applying plan's saved plan is %q", saved)
		}
		if _, err := s.FinishRun(context.Background(), "RUN3", control.RunResult{Status: control.RunApplied, EndedAt: t0.Add(6 * time.Minute)}, l.entry("apply")); err != nil {
			t.Fatal(err)
		}
		if saved := savedOf(t, s, fresh); saved != nil {
			t.Fatalf("the applied plan keeps its saved plan %q", saved)
		}

		// A plan of Writ's engine keeps its saved plan after it applies.
		writ := l.plan("other", t0.Add(7*time.Minute), control.Approved, 0)
		if _, err := l.begin(writ, "RUN4", "", t0.Add(8*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.FinishRun(context.Background(), "RUN4", control.RunResult{Status: control.RunApplied, EndedAt: t0.Add(9 * time.Minute)}, l.entry("apply")); err != nil {
			t.Fatal(err)
		}
		if saved := savedOf(t, s, writ); string(saved) != "saved" {
			t.Fatalf("the Writ plan's saved plan is %q", saved)
		}

		var reason string
		if err := controltest.Raw(t, loc).QueryRowContext(context.Background(), "SELECT reason FROM audit WHERE kind = 'state-write' AND run = 'OUTSIDE'").Scan(&reason); err != nil {
			t.Fatal(err)
		}
		if want := "state version 1, serial 1, lineage lineage, sha256 " + fmt.Sprintf("%064d", 1); reason != want {
			t.Fatalf("the state write's audit row says %q, want %q", reason, want)
		}
	})
}

// Two plans of one stack@env requested in the same millisecond still have
// an order: the one made first cannot begin, and the other can.
func TestTofuStalePlanSameMillisecond(t *testing.T) {
	each(t, func(t *testing.T, s *control.Store, _ string) {
		l := newLifecycle(t, s)
		older := l.tofuPlan("app", t0, 0)
		newer := l.tofuPlan("app", t0, 0)
		var stale *control.StaleError
		if _, err := l.begin(older, "RUN1", "", t0.Add(time.Minute)); !errors.As(err, &stale) || !strings.Contains(stale.Reason, "plan "+newer+" of app@prod is newer") {
			t.Fatalf("apply of the older of two plans of one millisecond: %v", err)
		}
		if _, err := l.begin(newer, "RUN2", "", t0.Add(time.Minute)); err != nil {
			t.Fatalf("apply of the newer of two plans of one millisecond: %v", err)
		}
	})
}

// holdState gives run a token of stack@prod and takes the stack's lock
// with it, as tofu does at the start of an apply, and returns the token's
// hash.
func holdState(t *testing.T, s *control.Store, stack, run string) []byte {
	t.Helper()
	ctx := context.Background()
	hash := []byte("hash of " + run)
	if err := s.AddStateToken(ctx, hash, control.StateToken{RunID: run, Env: "prod", Stack: stack, Scope: control.StateApply, ExpiresAt: t0.Add(time.Hour)}, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LockState(ctx, control.StateAuth{Hash: hash, RunID: run, Now: t0}, "prod", stack, control.StateLock{ID: "lock-" + run, RunID: run, Info: "{}", At: t0}, lockRow); err != nil {
		t.Fatal(err)
	}
	return hash
}

// A crash during an apply leaves the run applying, its state token live,
// and its stack's lock held, which no expiry releases. The next process
// fails the run and ends its token and lock, ends those of every run that
// no process runs, keeps those of a run that applies, and audits each
// lock it releases. A new apply of the stack then takes its lock.
func TestRecoveryEndsState(t *testing.T) {
	each(t, func(t *testing.T, s *control.Store, loc string) {
		ctx := context.Background()
		l := newLifecycle(t, s)
		at := t0.Add(time.Minute)
		crashed := l.tofuPlan("app", t0, 0)
		if _, err := l.begin(crashed, "RUN1", "", at); err != nil {
			t.Fatal(err)
		}
		runToken := holdState(t, s, "app", "RUN1")
		// A run that ended without ending its state access.
		goneToken := holdState(t, s, "other", "GONE")

		for i := range 2 {
			unlocked, err := s.EndOrphanState(ctx, at)
			if want := []string{"other@prod"}; err != nil || i == 0 && !slices.Equal(unlocked, want) || i == 1 && len(unlocked) != 0 {
				t.Fatalf("EndOrphanState call %d unlocked %v, %v", i+1, unlocked, err)
			}
		}
		if _, err := s.StateTokenByHash(ctx, goneToken, at); !errors.Is(err, control.ErrNotFound) {
			t.Fatalf("the token of a run that no process runs still resolves: %v", err)
		}
		if _, err := s.StateTokenByHash(ctx, runToken, at); err != nil {
			t.Fatalf("the token of a run that applies was ended: %v", err)
		}

		if ended, err := s.FailApplying(ctx, at, "NEXT", func(string) bool { return false }); err != nil || !slices.Equal(ended, []string{"RUN1"}) {
			t.Fatalf("ended %v, %v", ended, err)
		}
		if _, err := s.StateTokenByHash(ctx, runToken, at); !errors.Is(err, control.ErrNotFound) {
			t.Fatalf("the failed run's token still resolves: %v", err)
		}
		fresh := l.tofuPlan("app", t0.Add(2*time.Minute), 0)
		if _, err := l.begin(fresh, "RUN2", "", t0.Add(3*time.Minute)); err != nil {
			t.Fatalf("apply after the recovery: %v", err)
		}
		holdState(t, s, "app", "RUN2")

		rows, err := controltest.Raw(t, loc).QueryContext(ctx, "SELECT run, env, stacks, reason FROM audit WHERE kind = 'state-unlock' ORDER BY run")
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for rows.Next() {
			var run, env, stacks, reason string
			if err := rows.Scan(&run, &env, &stacks, &reason); err != nil {
				t.Fatal(err)
			}
			got = append(got, strings.Join([]string{run, env, stacks, reason}, " | "))
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		want := []string{
			`GONE | prod | ["other"] | writ serve released lock lock-GONE of run GONE on other@prod: no process runs it, as after a crash`,
			`RUN1 | prod | ["app"] | writ serve released lock lock-RUN1 of run RUN1 on app@prod: the run failed with no process to end it`,
		}
		if !slices.Equal(got, want) {
			t.Fatalf("the state-unlock audit rows:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	})
}

// Every end of a run's state access deletes the run's tokens, then its
// locks, in one transaction, so a LockState in flight either commits its
// lock before the end reads the locks, and the end releases it, or finds
// no token. On Postgres the test holds LockState's insert until the end
// waits on the token's row: an end that released the locks first left
// the new lock with no token, and no expiry releases a lock. SQLite runs
// one writer at a time, so there the end follows the lock.
func TestEndStateAfterLockInFlight(t *testing.T) {
	at := t0.Add(time.Minute)
	cases := []struct {
		name string
		// applying begins the run, as an apply does before its token.
		applying bool
		// audited is whether the end writes a state-unlock row.
		audited bool
		end     func(ctx context.Context, s *control.Store, l *lifecycle, run string) error
	}{
		{"EndStateRun", true, false, func(ctx context.Context, s *control.Store, _ *lifecycle, run string) error {
			return s.EndStateRun(ctx, run)
		}},
		{"FinishRun", true, true, func(ctx context.Context, s *control.Store, l *lifecycle, run string) error {
			_, err := s.FinishRun(ctx, run, control.RunResult{Status: control.RunFailed, EndedAt: at}, l.entry("apply"))
			return err
		}},
		{"FailApplying", true, true, func(ctx context.Context, s *control.Store, _ *lifecycle, _ string) error {
			_, err := s.FailApplying(ctx, at, "NEXT", func(string) bool { return false })
			return err
		}},
		{"EndOrphanState", false, true, func(ctx context.Context, s *control.Store, _ *lifecycle, _ string) error {
			_, err := s.EndOrphanState(ctx, at)
			return err
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			each(t, func(t *testing.T, s *control.Store, loc string) {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				l := newLifecycle(t, s)
				const run = "RUN1"
				if c.applying {
					if _, err := l.begin(l.tofuPlan("app", t0, 0), run, "", at); err != nil {
						t.Fatal(err)
					}
				}
				hash := []byte("hash of " + run)
				if err := s.AddStateToken(ctx, hash, control.StateToken{RunID: run, Env: "prod", Stack: "app", Scope: control.StateApply, ExpiresAt: t0.Add(time.Hour)}, t0); err != nil {
					t.Fatal(err)
				}
				raw := controltest.Raw(t, loc)
				pg := sqldb.IsPostgres(loc)
				var gate *sqldb.Tx
				if pg {
					for _, q := range []string{
						"CREATE FUNCTION hold_lock_insert() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(731298); RETURN NEW; END $$",
						"CREATE TRIGGER hold_lock_insert BEFORE INSERT ON tfstate_locks FOR EACH ROW EXECUTE FUNCTION hold_lock_insert()",
					} {
						if _, err := raw.ExecContext(ctx, q); err != nil {
							t.Fatal(err)
						}
					}
					var err error
					if gate, err = raw.BeginTx(ctx, nil); err != nil {
						t.Fatal(err)
					}
					defer gate.Rollback()
					if _, err := gate.ExecContext(ctx, "SELECT pg_advisory_xact_lock(731298)"); err != nil {
						t.Fatal(err)
					}
				}
				waitFor := func(prefix string) {
					for {
						var waiting int
						if err := raw.QueryRowContext(ctx, "SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock' AND query LIKE ?",
							prefix+"%").Scan(&waiting); err != nil {
							t.Fatal(err)
						}
						if waiting > 0 {
							return
						}
						select {
						case <-ctx.Done():
							t.Fatalf("no statement %q waits on a lock: %v", prefix, ctx.Err())
						case <-time.After(10 * time.Millisecond):
						}
					}
				}

				locked := make(chan error, 1)
				go func() {
					_, err := s.LockState(ctx, control.StateAuth{Hash: hash, RunID: run, Now: at}, "prod", "app", control.StateLock{ID: "lock-" + run, RunID: run, Info: "{}", At: at}, lockRow)
					locked <- err
				}()
				if pg {
					waitFor("INSERT INTO tfstate_locks")
				} else if err := <-locked; err != nil {
					t.Fatalf("lock: %v", err)
				}
				ended := make(chan error, 1)
				go func() { ended <- c.end(ctx, s, l, run) }()
				if pg {
					waitFor("DELETE FROM tfstate_tokens")
					if err := gate.Commit(); err != nil {
						t.Fatal(err)
					}
					if err := <-locked; err != nil {
						t.Fatalf("lock: %v", err)
					}
				}
				if err := <-ended; err != nil {
					t.Fatalf("end: %v", err)
				}

				var locks, tokens int
				if err := raw.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM tfstate_locks), (SELECT count(*) FROM tfstate_tokens)").Scan(&locks, &tokens); err != nil {
					t.Fatal(err)
				}
				if locks != 0 || tokens != 0 {
					t.Fatalf("after the end: locks=%d tokens=%d, want both zero", locks, tokens)
				}
				var audits int
				if err := raw.QueryRowContext(ctx, "SELECT count(*) FROM audit WHERE kind = 'state-unlock' AND run = ? AND reason LIKE ?",
					run, "writ serve released lock lock-"+run+" of run "+run+" on app@prod: %").Scan(&audits); err != nil {
					t.Fatal(err)
				}
				if want := map[bool]int{true: 1, false: 0}[c.audited]; audits != want {
					t.Fatalf("state-unlock audit rows of the released lock: %d, want %d", audits, want)
				}
			})
		})
	}
}

// A plan's end ends its state tokens in the transaction that records it,
// so a token that the plan's own end of its state access left is gone
// once the plan reads as ended.
func TestFinishPlanEndsState(t *testing.T) {
	each(t, func(t *testing.T, s *control.Store, _ string) {
		ctx := context.Background()
		l := newLifecycle(t, s)
		const ref = "REF0000000000000000000099"
		if err := s.CreatePlan(ctx, newPlan(ref, l.cfg, "prod", "app", t0), control.TokenUse{}, l.entry("plan")); err != nil {
			t.Fatal(err)
		}
		hash := []byte("hash of " + ref)
		if err := s.AddStateToken(ctx, hash, control.StateToken{RunID: ref, Env: "prod", Stack: "app", Scope: control.StatePlan, ExpiresAt: t0.Add(time.Hour)}, t0); err != nil {
			t.Fatal(err)
		}
		res := control.PlanResult{Status: control.Failed, MadeAt: t0.Add(time.Minute)}
		if err := s.FinishPlan(ctx, ref, res, l.entry("plan")); err != nil {
			t.Fatal(err)
		}
		if _, err := s.StateTokenByHash(ctx, hash, t0.Add(time.Minute)); !errors.Is(err, control.ErrNotFound) {
			t.Fatalf("the ended plan's state token resolves: %v", err)
		}
	})
}

// lockRow is the audit row each test lock commits with.
var lockRow = control.Entry{Kind: "request", Op: "state-lock", Decision: control.Allowed}
