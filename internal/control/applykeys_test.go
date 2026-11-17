package control_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/control/controltest"
)

// keyed is an apply request with a key and a token's use.
func (l *lifecycle) keyed(id, run, key, jti string, now time.Time, commit func(func() error) error) (control.Run, error) {
	return l.s.BeginApply(context.Background(), control.ApplyRequest{RunID: run, PlanID: id, Actor: applier, Via: "gitlab job 2",
		NamedBy: control.ByPlan, Rules: need(0), Now: now, Audit: l.entry("apply"), Owner: "P1", Commit: commit,
		Key: control.RequestKey{Key: key, IssuedAt: now, Request: "req"},
		Use: control.TokenUse{Issuer: "gitlab", JTI: jti, Op: "apply", Expires: now.Add(time.Hour)}})
}

// tokenUses counts the token ids that made a write.
func tokenUses(t *testing.T, loc string) int {
	t.Helper()
	var n int
	if err := controltest.Raw(t, loc).QueryRowContext(context.Background(), "SELECT count(*) FROM token_uses").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A key's row is final. A second request with a started key commits
// nothing and spends no token. A closed key starts no run. A refusal that
// commits a plan's move leaves no row, so the key can still start a run.
func TestApplyKeys(t *testing.T) {
	each(t, func(t *testing.T, s *control.Store, loc string) {
		ctx := context.Background()
		l := newLifecycle(t, s)
		id := l.plan("network", t0, control.Approved, 0)
		if _, err := l.keyed(id, "RUN1", "K1", "jti-1", t0, nil); err != nil {
			t.Fatal(err)
		}
		row, err := s.Key(ctx, "K1")
		if err != nil || row.Outcome != control.KeyStarted || row.RunID != "RUN1" || row.Issuer != "gitlab" || row.Subject != "main" || row.Request != "req" {
			t.Fatalf("the started key %+v, %v", row, err)
		}
		if _, err := l.keyed(id, "RUN2", "K1", "jti-2", t0, nil); !errors.Is(err, control.ErrKeyUsed) {
			t.Fatalf("a repeat of the key: %v", err)
		}
		if n := tokenUses(t, loc); n != 1 {
			t.Fatalf("%d token uses after a repeat, want 1", n)
		}
		if _, err := s.GetRun(ctx, "RUN2"); !errors.Is(err, control.ErrNotFound) {
			t.Fatalf("the repeat began a run: %v", err)
		}

		other := l.plan("cache", t0, control.Approved, 0)
		closed, err := s.CloseKey(ctx, "K2", t0, t0)
		if err != nil || closed.Outcome != control.KeyNone || closed.RunID != "" {
			t.Fatalf("close a key with no row: %+v, %v", closed, err)
		}
		if again, err := s.CloseKey(ctx, "K2", t0, t0.Add(time.Minute)); err != nil || again != closed {
			t.Fatalf("close it again: %+v, %v", again, err)
		}
		if started, err := s.CloseKey(ctx, "K1", t0, t0); err != nil || started.Outcome != control.KeyStarted || started.RunID != "RUN1" {
			t.Fatalf("close a started key: %+v, %v", started, err)
		}
		if _, err := l.keyed(other, "RUN3", "K2", "jti-3", t0, nil); !errors.Is(err, control.ErrKeyUsed) {
			t.Fatalf("a closed key: %v", err)
		}

		// The plan expired: BeginApply commits the move to expired, and
		// no row of the key.
		late := t0.Add(48 * time.Hour)
		var stale *control.StaleError
		if _, err := l.keyed(other, "RUN4", "K3", "jti-4", late, nil); !errors.As(err, &stale) {
			t.Fatalf("an expired plan: %v", err)
		}
		if _, err := s.Key(ctx, "K3"); !errors.Is(err, control.ErrNotFound) {
			t.Fatalf("a stale plan left the key's row: %v", err)
		}
		fresh := l.plan("cache", late, control.Approved, 0)
		if _, err := l.keyed(fresh, "RUN5", "K3", "jti-5", late, nil); err != nil {
			t.Fatalf("the key after a refusal: %v", err)
		}
	})
}

// A commit that returns an error is ErrCommitUnknown, whether the
// transaction is durable or not. CloseKey tells which.
func TestApplyCommitUnknown(t *testing.T) {
	each(t, func(t *testing.T, s *control.Store, _ string) {
		ctx := context.Background()
		l := newLifecycle(t, s)
		lost := errors.New("the answer was lost")
		durable := l.plan("network", t0, control.Approved, 0)
		run, err := l.keyed(durable, "RUN1", "K1", "jti-1", t0, func(commit func() error) error {
			if err := commit(); err != nil {
				return err
			}
			return lost
		})
		if !errors.Is(err, control.ErrCommitUnknown) || run.ID != "RUN1" {
			t.Fatalf("a durable commit that failed: %+v, %v", run, err)
		}
		if k, err := s.CloseKey(ctx, "K1", t0, t0); err != nil || k.Outcome != control.KeyStarted || k.RunID != "RUN1" {
			t.Fatalf("the durable commit's key: %+v, %v", k, err)
		}
		rolled := l.plan("cache", t0, control.Approved, 0)
		if _, err := l.keyed(rolled, "RUN2", "K2", "jti-2", t0, func(func() error) error { return lost }); !errors.Is(err, control.ErrCommitUnknown) {
			t.Fatalf("a commit rolled back: %v", err)
		}
		if k, err := s.CloseKey(ctx, "K2", t0, t0); err != nil || k.Outcome != control.KeyNone {
			t.Fatalf("the rolled back commit's key: %+v, %v", k, err)
		}
		if l.status(rolled, t0) != control.Approved {
			t.Fatalf("the rolled back plan is %s", l.status(rolled, t0))
		}
	})
}

// CloseKey waits for a BeginApply that inserted the key and has not
// committed, and then answers started, never a none that a commit could
// contradict.
func TestCloseKeyWaitsForBeginApply(t *testing.T) {
	each(t, func(t *testing.T, s *control.Store, _ string) {
		ctx := context.Background()
		l := newLifecycle(t, s)
		id := l.plan("network", t0, control.Approved, 0)
		held, release := make(chan struct{}), make(chan struct{})
		began := make(chan error, 1)
		go func() {
			_, err := l.keyed(id, "RUN1", "K1", "jti-1", t0, func(commit func() error) error {
				close(held)
				<-release
				return commit()
			})
			began <- err
		}()
		<-held
		closed := make(chan control.KeyRow, 1)
		go func() {
			k, err := s.CloseKey(ctx, "K1", t0, t0)
			if err != nil {
				t.Error(err)
			}
			closed <- k
		}()
		time.Sleep(300 * time.Millisecond)
		close(release)
		if err := <-began; err != nil {
			t.Fatal(err)
		}
		if k := <-closed; k.Outcome != control.KeyStarted || k.RunID != "RUN1" {
			t.Fatalf("closed a key while its run's start waited to commit: %+v", k)
		}
	})
}

// Prune deletes a none row only an hour past its key's life. A started
// row stays with its run.
func TestPruneKeys(t *testing.T) {
	each(t, func(t *testing.T, s *control.Store, _ string) {
		ctx := context.Background()
		l := newLifecycle(t, s)
		id := l.plan("network", t0, control.Approved, 0)
		if _, err := l.keyed(id, "RUN1", "STARTED", "jti-1", t0, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CloseKey(ctx, "NONE", t0, t0); err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct {
			at   time.Time
			gone int64
		}{
			{t0.Add(control.KeyLife + 59*time.Minute), 0},
			{t0.Add(control.KeyLife + 61*time.Minute), 1},
		} {
			got, err := s.Prune(ctx, c.at, control.Retention{})
			if err != nil || got.Keys != c.gone {
				t.Fatalf("prune at %s: %+v, %v", c.at, got, err)
			}
		}
		if _, err := s.Key(ctx, "NONE"); !errors.Is(err, control.ErrNotFound) {
			t.Fatalf("the none row after its life: %v", err)
		}
		if k, err := s.Key(ctx, "STARTED"); err != nil || k.RunID != "RUN1" {
			t.Fatalf("the started row: %+v, %v", k, err)
		}
	})
}

// FailApplying ends a run of another process, and a run of its own
// process only when that process has no work for it.
func TestFailApplyingOwners(t *testing.T) {
	each(t, func(t *testing.T, s *control.Store, _ string) {
		ctx := context.Background()
		l := newLifecycle(t, s)
		mine := l.plan("network", t0, control.Approved, 0)
		lost := l.plan("cache", t0, control.Approved, 0)
		if _, err := l.keyed(mine, "MINE", "K1", "jti-1", t0, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := l.keyed(lost, "LOST", "K2", "jti-2", t0, nil); err != nil {
			t.Fatal(err)
		}
		ended, err := s.FailApplying(ctx, t0, "P1", func(run string) bool { return run == "MINE" })
		if err != nil || !slices.Equal(ended, []string{"LOST"}) {
			t.Fatalf("the owner's sweep ended %v, %v", ended, err)
		}
		if r, _ := s.GetRun(ctx, "LOST"); r.Status != control.RunFailed || !strings.Contains(r.Text, "could not record") {
			t.Fatalf("the lost run %+v", r)
		}
		ended, err = s.FailApplying(ctx, t0, "P2", func(string) bool { return true })
		if err != nil || !slices.Equal(ended, []string{"MINE"}) {
			t.Fatalf("another process's sweep ended %v, %v", ended, err)
		}
		if r, _ := s.GetRun(ctx, "MINE"); r.Status != control.RunFailed || !strings.Contains(r.Text, "stopped during this run") {
			t.Fatalf("the run of a stopped process %+v", r)
		}
	})
}

// Prune keeps the greatest time of a none key it deleted, and the floor
// never moves down. A key at or below it starts no run, whatever time the
// request carries, and spends no token. A key above it starts one.
func TestPruneFloor(t *testing.T) {
	each(t, func(t *testing.T, s *control.Store, loc string) {
		ctx := context.Background()
		l := newLifecycle(t, s)
		if _, err := s.CloseKey(ctx, "K1", t0, t0); err != nil {
			t.Fatal(err)
		}
		late := t0.Add(26 * time.Hour)
		if got, err := s.Prune(ctx, late, control.Retention{}); err != nil || got.Keys != 1 {
			t.Fatalf("prune: %+v, %v", got, err)
		}
		// An older none row pruned later leaves the floor at t0.
		if _, err := s.CloseKey(ctx, "K0", t0.Add(-time.Hour), t0); err != nil {
			t.Fatal(err)
		}
		if got, err := s.Prune(ctx, late, control.Retention{}); err != nil || got.Keys != 1 {
			t.Fatalf("prune the older key: %+v, %v", got, err)
		}
		id := l.plan("network", t0, control.Approved, 0)
		for _, key := range []string{"K1", "K0"} {
			if _, err := l.keyed(id, "RUN-"+key, key, "jti-"+key, t0, nil); !errors.Is(err, control.ErrKeyClosed) {
				t.Fatalf("the pruned key %s with the clock back at its time: %v", key, err)
			}
		}
		if _, err := s.Key(ctx, "K1"); !errors.Is(err, control.ErrNotFound) {
			t.Fatalf("the refusal wrote a row: %v", err)
		}
		if n := tokenUses(t, loc); n != 0 {
			t.Fatalf("%d token uses after refusals, want 0", n)
		}
		if _, err := l.keyed(id, "RUN2", "K2", "jti-2", t0.Add(time.Millisecond), nil); err != nil {
			t.Fatalf("a key above the floor: %v", err)
		}
	})
}

// A refusal that BeginApply commits, for a plan that expired, spends no
// CI token and leaves no row of the key. A token that started a run is
// still refused as used.
func TestRefusalSpendsNoToken(t *testing.T) {
	each(t, func(t *testing.T, s *control.Store, loc string) {
		ctx := context.Background()
		l := newLifecycle(t, s)
		expired := l.plan("network", t0, control.Approved, 0)
		late := t0.Add(48 * time.Hour)
		var stale *control.StaleError
		for _, jti := range []string{"jti-1", "jti-2"} {
			if _, err := l.keyed(expired, "RUN-"+jti, "K1", jti, late, nil); !errors.As(err, &stale) {
				t.Fatalf("an expired plan with %s: %v", jti, err)
			}
		}
		if _, err := s.Key(ctx, "K1"); !errors.Is(err, control.ErrNotFound) {
			t.Fatalf("the refusals left a row of the key: %v", err)
		}
		if n := tokenUses(t, loc); n != 0 {
			t.Fatalf("%d token uses after two refusals, want 0", n)
		}
		fresh := l.plan("network", late, control.Approved, 0)
		if _, err := l.keyed(fresh, "RUN1", "K1", "jti-1", late, nil); err != nil {
			t.Fatalf("a token a refusal did not spend: %v", err)
		}
		other := l.plan("cache", late, control.Approved, 0)
		if _, err := l.keyed(other, "RUN2", "K2", "jti-1", late, nil); !errors.Is(err, control.ErrReplay) {
			t.Fatalf("the token that started a run, again: %v", err)
		}
		if n := tokenUses(t, loc); n != 1 {
			t.Fatalf("%d token uses, want 1", n)
		}
	})
}
