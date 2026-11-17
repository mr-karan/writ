package control_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/control/controltest"
	"github.com/mr-karan/writ/internal/sqldb"
)

// The first serial writ serve reads of a stack on S3 is its baseline, a
// serial an apply wrote or passed through is the apply's, and any other
// unseen serial is a write outside Writ.
func TestJudgeSerial(t *testing.T) {
	each(t, func(t *testing.T, s *control.Store, loc string) {
		ctx := context.Background()
		judge := func(lineage string, serial int64) control.Judged {
			t.Helper()
			j, err := s.JudgeSerial(ctx, "prod", "app", lineage, serial)
			if err != nil {
				t.Fatal(err)
			}
			return j
		}
		put := func(v control.StateSerial, secrets []byte) {
			t.Helper()
			v.Env, v.Stack = "prod", "app"
			if v.SeenAt.IsZero() {
				v.SeenAt = t0
			}
			if _, err := s.PutSerial(ctx, v, secrets); err != nil {
				t.Fatal(err)
			}
		}
		if j := judge("L1", 4); j.Writer != control.SerialBaseline {
			t.Fatalf("the first serial of app: %+v, want the baseline", j)
		}
		put(control.StateSerial{Lineage: "L1", Serial: 4, From: 4}, nil)
		if j := judge("L1", 4); j.Writer != control.SerialSeen || j.RunID != "" {
			t.Fatalf("the baseline read again: %+v", j)
		}
		// RUN1's plan read serial 4, and OpenTofu saved serials 5 and 6
		// on its way to 7.
		put(control.StateSerial{Lineage: "L1", Serial: 7, From: 4, RunID: "RUN1"}, []byte("sealed 7"))
		for _, tc := range []struct {
			lineage string
			serial  int64
			want    control.Judged
		}{
			{"L1", 5, control.Judged{Writer: control.SerialWrit, RunID: "RUN1"}},
			{"L1", 7, control.Judged{Writer: control.SerialSeen, RunID: "RUN1"}},
			{"L1", 8, control.Judged{Writer: control.SerialOutside}},
			{"L1", 3, control.Judged{Writer: control.SerialOutside}},
			{"L2", 5, control.Judged{Writer: control.SerialOutside}},
		} {
			if j := judge(tc.lineage, tc.serial); j != tc.want {
				t.Errorf("serial %d of lineage %s: %+v, want %+v", tc.serial, tc.lineage, j, tc.want)
			}
		}
		if j, err := s.JudgeSerial(ctx, "prod", "api", "L1", 8); err != nil || j.Writer != control.SerialBaseline {
			t.Fatalf("the first serial of another stack: %+v %v", j, err)
		}

		if got, err := s.SerialSecrets(ctx, "prod", "app", "L1", 7); err != nil || string(got) != "sealed 7" {
			t.Fatalf("the secrets of serial 7: %q %v", got, err)
		}
		for _, serial := range []int64{4, 5} {
			if got, err := s.SerialSecrets(ctx, "prod", "app", "L1", serial); !errors.Is(err, control.ErrNotFound) {
				t.Errorf("the secrets of serial %d, which no apply ended at: %q %v, want ErrNotFound", serial, got, err)
			}
		}
		// A serial seen again by a refresh keeps the apply that wrote it.
		put(control.StateSerial{Lineage: "L1", Serial: 7, From: 7}, nil)
		if j := judge("L1", 6); j.Writer != control.SerialWrit {
			t.Fatalf("serial 6 after a refresh read serial 7 again: %+v", j)
		}

		// Many writes outside Writ prune the oldest of their own rows and
		// keep the apply's.
		for i := range int64(25) {
			put(control.StateSerial{Lineage: "L1", Serial: 8 + i, From: 8 + i, SeenAt: t0.Add(time.Duration(i+1) * time.Minute)}, nil)
		}
		if j := judge("L1", 8); j.Writer != control.SerialOutside {
			t.Fatalf("a pruned serial: %+v, want outside", j)
		}
		if j := judge("L1", 32); j.Writer != control.SerialSeen {
			t.Fatalf("the newest serial: %+v, want seen", j)
		}
		if got, err := s.SerialSecrets(ctx, "prod", "app", "L1", 7); err != nil || string(got) != "sealed 7" {
			t.Fatalf("the secrets of the apply's serial after the prune: %q %v", got, err)
		}
	})
}

// A refresh reads the stacks on S3 that an apply of writ serve wrote,
// from the newest such apply, and none whose apply runs now.
func TestS3Stacks(t *testing.T) {
	each(t, func(t *testing.T, s *control.Store, loc string) {
		ctx := context.Background()
		l := newLifecycle(t, s)
		for i, v := range []control.StateSerial{
			{Stack: "app", Lineage: "L1", Serial: 2, From: 1, RunID: "RUN1"},
			{Stack: "app", Lineage: "L1", Serial: 3, From: 2, RunID: "RUN2"},
			{Stack: "app", Lineage: "L1", Serial: 4, From: 4},
			{Stack: "web", Lineage: "L9", Serial: 1, From: 1},
			{Stack: "stack", Lineage: "L5", Serial: 6, From: 5, RunID: "RUN3"},
		} {
			v.Env, v.SeenAt = "prod", t0.Add(time.Duration(i)*time.Minute)
			if _, err := s.PutSerial(ctx, v, []byte("sealed")); err != nil {
				t.Fatal(err)
			}
		}
		want := []control.S3Stack{{"prod", "app", "RUN2"}, {"prod", "stack", "RUN3"}}
		if got, err := s.S3Stacks(ctx, 10); err != nil || !slices.Equal(got, want) {
			t.Fatalf("stacks on S3: %v %v, want %v", got, err, want)
		}
		if got, err := s.S3Stacks(ctx, 1); err != nil || len(got) != 1 {
			t.Fatalf("stacks on S3 with a limit of one: %v %v", got, err)
		}
		since := t0.Add(time.Hour)
		id := l.plan("stack", since, control.Approved, 0)
		if _, err := l.begin(id, "RUN9", "", since); err != nil {
			t.Fatal(err)
		}
		if got, err := s.S3Stacks(ctx, 10); err != nil || !slices.Equal(got, want[:1]) {
			t.Fatalf("stacks on S3 while an apply of stack runs: %v %v", got, err)
		}
	})
}

// Each look of the timer gets a turn for every stack on S3, however many
// there are: those never checked come first, then those checked longest
// ago, so stacks past the first limit are not left out.
func TestS3StacksTakeTurns(t *testing.T) {
	each(t, func(t *testing.T, s *control.Store, loc string) {
		ctx := context.Background()
		const stacks, limit = 7, 3
		for i := range stacks {
			v := control.StateSerial{Env: "prod", Stack: fmt.Sprintf("s%02d", i), Lineage: "L", Serial: 2, From: 1, RunID: fmt.Sprintf("RUN%d", i), SeenAt: t0}
			if _, err := s.PutSerial(ctx, v, []byte("sealed")); err != nil {
				t.Fatal(err)
			}
		}
		seen := map[string]int{}
		at := t0
		for look := range 3 {
			got, err := s.S3Stacks(ctx, limit)
			if err != nil || len(got) != limit {
				t.Fatalf("look %d: %v %v, want %d stacks", look, got, err, limit)
			}
			for _, st := range got {
				seen[st.Stack]++
				at = at.Add(time.Minute)
				if err := s.CheckedS3(ctx, st.Env, st.Stack, at); err != nil {
					t.Fatal(err)
				}
			}
		}
		// Three looks of three take each of the seven once, and then the
		// two checked first again.
		for i := range stacks {
			want := 1
			if i < 2 {
				want = 2
			}
			if n := seen[fmt.Sprintf("s%02d", i)]; n != want {
				t.Errorf("stack s%02d had %d turns in three looks, want %d: %v", i, n, want, seen)
			}
		}
	})
}

// Each read of a stack's state on S3 takes the next place among the
// reads of its stack@env, and a read again of a serial keeps its writer
// and its sealed secrets.
func TestSerialReads(t *testing.T) {
	each(t, func(t *testing.T, s *control.Store, loc string) {
		ctx := context.Background()
		put := func(stack string, v control.StateSerial, secrets []byte) int64 {
			t.Helper()
			v.Env, v.Stack, v.SeenAt = "prod", stack, t0
			seq, err := s.PutSerial(ctx, v, secrets)
			if err != nil {
				t.Fatal(err)
			}
			return seq
		}
		var got []int64
		got = append(got, put("app", control.StateSerial{Lineage: "L1", Serial: 4, From: 4}, nil))
		got = append(got, put("app", control.StateSerial{Lineage: "L1", Serial: 5, From: 4, RunID: "RUN1"}, []byte("sealed 5")))
		got = append(got, put("web", control.StateSerial{Lineage: "L9", Serial: 1, From: 1}, nil))
		got = append(got, put("app", control.StateSerial{Lineage: "L1", Serial: 5, From: 5}, nil))
		if want := []int64{1, 2, 1, 3}; !slices.Equal(got, want) {
			t.Fatalf("the places of the reads: %v, want %v", got, want)
		}
		if j, err := s.JudgeSerial(ctx, "prod", "app", "L1", 5); err != nil || j != (control.Judged{Writer: control.SerialSeen, RunID: "RUN1"}) {
			t.Fatalf("serial 5 after a read again: %+v %v", j, err)
		}
		if sealed, err := s.SerialSecrets(ctx, "prod", "app", "L1", 5); err != nil || string(sealed) != "sealed 5" {
			t.Fatalf("the secrets of serial 5 after a read again: %q %v", sealed, err)
		}
	})
}

// An index of a state on S3 is ordered by the read it came from, not by
// its serial or lineage: a refresh that read lineage A before an apply
// wrote and indexed lineage B, and ends after it, does not replace B's
// index, and a later read of an older serial does.
func TestS3Index(t *testing.T) {
	each(t, func(t *testing.T, s *control.Store, loc string) {
		ctx := context.Background()
		read := func(lineage string, serial int64, run string) int64 {
			t.Helper()
			seq, err := s.PutSerial(ctx, control.StateSerial{Env: "prod", Stack: "app", Lineage: lineage, Serial: serial, RunID: run, From: serial - 1, SeenAt: t0}, []byte("sealed"))
			if err != nil {
				t.Fatal(err)
			}
			return seq
		}
		w := func(lineage string, serial, seq int64, id string) control.IndexWrite {
			return control.IndexWrite{Env: "prod", Stack: "app", Backend: control.BackendS3, Lineage: lineage, Version: serial, Seq: seq, At: t0,
				Rows: []control.IndexRow{{Address: "random_pet.name", Type: "random_pet", Provider: "registry.opentofu.org/hashicorp/random", ID: id}}}
		}
		// The refresh reads serial 7 of lineage A; then an apply writes
		// serial 1 of lineage B, reads it, and indexes it; then the
		// refresh stores its index.
		refresh := read("A", 7, "RUN1")
		apply := read("B", 1, "RUN2")
		if ok, err := s.ReplaceIndex(ctx, w("B", 1, apply, "b")); !ok || err != nil {
			t.Fatalf("the apply's index: %v %v", ok, err)
		}
		if ok, err := s.ReplaceIndex(ctx, w("A", 7, refresh, "a")); ok || err != nil {
			t.Fatalf("the slow refresh's index of an older read: %v %v, want it refused", ok, err)
		}
		if ix, err := s.Index(ctx, "prod", "app"); err != nil || ix.Lineage != "B" || ix.Version != 1 || len(ix.Rows) != 1 || ix.Rows[0].ID != "b" {
			t.Fatalf("app's index after the slow refresh: %+v %v", ix, err)
		}
		// A later read of lineage A's serial 7, as after a write that
		// put it back, replaces the index, and the same read again does
		// too.
		later := read("A", 7, "")
		for range 2 {
			if ok, err := s.ReplaceIndex(ctx, w("A", 7, later, "a")); !ok || err != nil {
				t.Fatalf("an index of a later read: %v %v", ok, err)
			}
		}
		if ix, err := s.Index(ctx, "prod", "app"); err != nil || ix.Lineage != "A" || ix.Version != 7 || ix.Rows[0].ID != "a" {
			t.Fatalf("app's index after a later read: %+v %v", ix, err)
		}
		// Any later read makes the index of an older read stale, whatever
		// the serials: a refresh reads serial 7, a plan reads serial 8,
		// and then the refresh stores its index of serial 7, or finds the
		// index already holds it.
		slow := read("A", 7, "")
		_, plan, err := s.ObserveSerial(ctx, control.SerialRead{Env: "prod", Stack: "app", Lineage: "A", Serial: 8, At: t0, Since: t0})
		if err != nil || plan != slow+1 {
			t.Fatalf("the plan's read: place %d %v, want %d", plan, err, slow+1)
		}
		if ok, err := s.ReplaceIndex(ctx, w("A", 7, slow, "slow")); ok || err != nil {
			t.Fatalf("the index of a read a plan's read follows: %v %v, want it refused", ok, err)
		}
		if held, err := s.S3IndexHolds(ctx, "prod", "app", "A", 7, slow); !errors.Is(err, control.ErrStaleRead) {
			t.Fatalf("the index holds the serial of a read a plan's read follows: %v %v, want ErrStaleRead", held, err)
		}
		for serial, want := range map[int64]bool{7: true, 8: false} {
			if held, err := s.S3IndexHolds(ctx, "prod", "app", "A", serial, plan); err != nil || held != want {
				t.Errorf("the index holds serial %d, for the newest read: %v %v, want %v", serial, held, err, want)
			}
		}
		if ix, err := s.Index(ctx, "prod", "app"); err != nil || ix.Version != 7 || ix.Rows[0].ID != "a" {
			t.Fatalf("app's index after the stale write: %+v %v", ix, err)
		}
		for _, bad := range []control.IndexWrite{w("", 1, later+1, "e"), w("A", 8, 0, "e"),
			{Env: "prod", Stack: "app", Backend: control.BackendHTTP, Lineage: "L1", Version: 1},
			{Env: "prod", Stack: "app", Backend: control.BackendHTTP, Version: 1, Seq: 9}} {
			if _, err := s.ReplaceIndex(ctx, bad); err == nil {
				t.Errorf("an index of backend %s with lineage %q and read %d was stored", bad.Backend, bad.Lineage, bad.Seq)
			}
		}
		if _, err := s.ReplaceIndex(ctx, control.IndexWrite{Env: "prod", Stack: "app", Backend: "gcs", Version: 1}); err == nil {
			t.Fatal("an index of backend gcs was stored")
		}
	})
}

// A read that no apply claims is judged and recorded in one step: the
// first is the baseline, one no apply wrote marks drift on the index
// record, and one an apply passed through is the apply's. A read that an
// apply overlapped, running or ended since the read started, records
// nothing and takes no place.
func TestObserveSerial(t *testing.T) {
	each(t, func(t *testing.T, s *control.Store, loc string) {
		ctx := context.Background()
		l := newLifecycle(t, s)
		observe := func(serial int64, at, since time.Time) (control.Judged, int64) {
			t.Helper()
			j, seq, err := s.ObserveSerial(ctx, control.SerialRead{Env: "prod", Stack: "app", Lineage: "L1", Serial: serial, At: at, Since: since})
			if err != nil {
				t.Fatal(err)
			}
			return j, seq
		}
		if j, seq := observe(3, t0, t0); j.Writer != control.SerialBaseline || seq != 1 {
			t.Fatalf("the first read: %+v at %d", j, seq)
		}
		if _, err := s.Index(ctx, "prod", "app"); !errors.Is(err, control.ErrNotFound) {
			t.Fatalf("the index after the baseline: %v, want no record", err)
		}
		if j, seq := observe(4, t0.Add(time.Minute), t0); j.Writer != control.SerialOutside || seq != 2 {
			t.Fatalf("a serial no apply wrote: %+v at %d", j, seq)
		}
		if ix, err := s.Index(ctx, "prod", "app"); err != nil || ix.Version != 0 || ix.Drift != 4 || !ix.DriftAt.Equal(t0.Add(time.Minute)) {
			t.Fatalf("the index record after drift: %+v %v", ix, err)
		}
		if j, seq := observe(4, t0.Add(2*time.Minute), t0); j.Writer != control.SerialSeen || seq != 3 {
			t.Fatalf("the drift read again: %+v at %d", j, seq)
		}

		// RUN1 starts at t0+1h, and its plan read serial 4.
		since := t0.Add(time.Hour)
		id := l.plan("app", since, control.Approved, 0)
		if _, err := l.begin(id, "RUN1", "", since); err != nil {
			t.Fatal(err)
		}
		for _, from := range []time.Time{t0, since.Add(time.Hour)} {
			if j, seq := observe(6, since.Add(2*time.Hour), from); seq != 0 || j != (control.Judged{}) {
				t.Fatalf("a read that started at %v while RUN1 runs: %+v at %d, want nothing recorded", from, j, seq)
			}
		}
		end := since.Add(time.Minute)
		if _, err := s.FinishRun(ctx, "RUN1", control.RunResult{Status: control.RunApplied, EndedAt: end}, l.entry("apply")); err != nil {
			t.Fatal(err)
		}
		if _, seq := observe(6, end, end); seq != 0 {
			t.Fatalf("a read that started as RUN1 ended took place %d, want none", seq)
		}
		if j, err := s.JudgeSerial(ctx, "prod", "app", "L1", 6); err != nil || j.Writer != control.SerialOutside {
			t.Fatalf("serial 6 after the reads RUN1 overlapped: %+v %v, want it unseen", j, err)
		}
		// RUN1 wrote serials 5 and 6, and a read after its end judges 5
		// as its.
		if seq, err := s.PutSerial(ctx, control.StateSerial{Env: "prod", Stack: "app", Lineage: "L1", Serial: 6, From: 4, RunID: "RUN1", SeenAt: end}, []byte("sealed")); err != nil || seq != 4 {
			t.Fatalf("RUN1's serial: place %d %v, want 4", seq, err)
		}
		after := end.Add(time.Millisecond)
		if j, seq := observe(5, after, after); j != (control.Judged{Writer: control.SerialWrit, RunID: "RUN1"}) || seq != 5 {
			t.Fatalf("a serial RUN1 passed through: %+v at %d", j, seq)
		}
		if ix, err := s.Index(ctx, "prod", "app"); err != nil || ix.Drift != 4 {
			t.Fatalf("the drift after RUN1's serials: %+v %v, want 4 kept", ix, err)
		}
	})
}

// An apply that ends while a read is recorded is seen by the read's
// check: the check runs only once the read holds the stack@env's row,
// which an apply's own record of its serial takes too.
func TestObserveSerialWaitsForTheStack(t *testing.T) {
	each(t, func(t *testing.T, s *control.Store, loc string) {
		ctx := context.Background()
		l := newLifecycle(t, s)
		id := l.plan("app", t0, control.Approved, 0)
		if _, err := l.begin(id, "RUN1", "", t0); err != nil {
			t.Fatal(err)
		}
		if _, err := s.FinishRun(ctx, "RUN1", control.RunResult{Status: control.RunApplied, EndedAt: t0.Add(time.Minute)}, l.entry("apply")); err != nil {
			t.Fatal(err)
		}
		if _, err := s.PutSerial(ctx, control.StateSerial{Env: "prod", Stack: "app", Lineage: "L1", Serial: 5, From: 4, RunID: "RUN1", SeenAt: t0}, []byte("sealed")); err != nil {
			t.Fatal(err)
		}
		since := t0.Add(time.Hour)
		type result struct {
			seq int64
			err error
		}
		done := make(chan result, 1)
		raw := controltest.Raw(t, loc)
		err := raw.Write(ctx, func(tx *sqldb.Tx) error {
			// SQLite's write transaction holds the whole database already.
			if tx.Postgres() {
				var seq int64
				if err := tx.QueryRowContext(ctx, "SELECT read_seq FROM state_s3_stacks WHERE env = 'prod' AND stack = 'app' FOR UPDATE").Scan(&seq); err != nil {
					return err
				}
			}
			go func() {
				_, seq, err := s.ObserveSerial(ctx, control.SerialRead{Env: "prod", Stack: "app", Lineage: "L1", Serial: 4, At: since, Since: since})
				done <- result{seq, err}
			}()
			time.Sleep(300 * time.Millisecond)
			// RUN1 ends while the read waits, after the read started.
			_, err := tx.ExecContext(ctx, "UPDATE runs SET ended_at = ? WHERE id = 'RUN1'", since.Add(time.Minute).UnixMilli())
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if r := <-done; r.err != nil || r.seq != 0 {
			t.Fatalf("a read an apply ended during: place %d %v, want nothing recorded", r.seq, r.err)
		}
	})
}

// Drift on a stack on S3 is kept on its index record: an index that
// replaces the rows keeps the mark, and newer drift replaces it.
func TestMarkDrift(t *testing.T) {
	each(t, func(t *testing.T, s *control.Store, loc string) {
		ctx := context.Background()
		observe := func(serial int64, at time.Time) int64 {
			t.Helper()
			_, seq, err := s.ObserveSerial(ctx, control.SerialRead{Env: "prod", Stack: "app", Lineage: "L1", Serial: serial, At: at, Since: at})
			if err != nil {
				t.Fatal(err)
			}
			return seq
		}
		observe(3, t0)
		observe(4, t0)
		// A read of serial 3 again, as after a write that put it back,
		// is the newest, and indexes it.
		w := control.IndexWrite{Env: "prod", Stack: "app", Backend: control.BackendS3, Lineage: "L1", Version: 3, Seq: observe(3, t0), At: t0.Add(time.Minute),
			Rows: []control.IndexRow{{Address: "random_pet.name", Type: "random_pet", Provider: "registry.opentofu.org/hashicorp/random", ID: "a"}}}
		if ok, err := s.ReplaceIndex(ctx, w); !ok || err != nil {
			t.Fatalf("an index after drift: %v %v", ok, err)
		}
		if ix, err := s.Index(ctx, "prod", "app"); err != nil || ix.Version != 3 || len(ix.Rows) != 1 || ix.Drift != 4 || !ix.DriftAt.Equal(t0) {
			t.Fatalf("the index after a replace: %+v %v, want drift 4 kept", ix, err)
		}
		later := t0.Add(time.Hour)
		observe(6, later)
		if ix, err := s.Index(ctx, "prod", "app"); err != nil || ix.Version != 3 || len(ix.Rows) != 1 || ix.Drift != 6 || !ix.DriftAt.Equal(later) {
			t.Fatalf("the index after new drift: %+v %v", ix, err)
		}
		if ix, err := s.Index(ctx, "prod", "web"); err != control.ErrNotFound {
			t.Fatalf("a stack with no drift and no index: %+v %v", ix, err)
		}
	})
}
