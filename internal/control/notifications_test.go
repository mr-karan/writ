package control_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/control/controltest"
)

func TestNotificationStatus(t *testing.T) {
	controltest.Each(t, func(t *testing.T, e controltest.Engine) {
		ctx := context.Background()
		loc := e.New(t)
		s, err := control.Open(ctx, control.Options{Location: loc})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()

		t1 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
		t2 := t1.Add(5 * time.Minute)
		t3 := t2.Add(5 * time.Minute)

		if err := s.RecordNotification(ctx, "ops-slack", "drift", true, "", t1); err != nil {
			t.Fatalf("record ok: %v", err)
		}
		if err := s.RecordNotification(ctx, "ops-slack", "apply-failed", false, "http status 502", t2); err != nil {
			t.Fatalf("record fail: %v", err)
		}
		if err := s.RecordNotification(ctx, "pager", "outside-write", false, "request timed out", t2); err != nil {
			t.Fatalf("record pager fail: %v", err)
		}

		got, err := s.NotificationStatuses(ctx)
		if err != nil {
			t.Fatalf("NotificationStatuses: %v", err)
		}
		st := got["ops-slack"]
		if st.Name != "ops-slack" || st.Delivered != 1 || st.Failed != 1 || st.LastEvent != "apply-failed" ||
			!st.LastOKAt.Equal(t1) || !st.LastErrorAt.Equal(t2) || st.LastError != "http status 502" {
			t.Fatalf("ops-slack status = %+v", st)
		}
		pg := got["pager"]
		if pg.Delivered != 0 || pg.Failed != 1 || !pg.LastOKAt.IsZero() || !pg.LastErrorAt.Equal(t2) || pg.LastError != "request timed out" {
			t.Fatalf("pager status = %+v", pg)
		}

		// Subsequent success clears last_error while preserving counts.
		if err := s.RecordNotification(ctx, "ops-slack", "applied", true, "", t3); err != nil {
			t.Fatalf("record recovery: %v", err)
		}
		got, err = s.NotificationStatuses(ctx)
		if err != nil {
			t.Fatalf("NotificationStatuses after recovery: %v", err)
		}
		st = got["ops-slack"]
		if st.Delivered != 2 || st.Failed != 1 || st.LastEvent != "applied" || !st.LastOKAt.Equal(t3) || !st.LastErrorAt.IsZero() || st.LastError != "" {
			t.Fatalf("ops-slack after recovery = %+v", st)
		}

		// PeekNotificationStatuses reads persisted status without migrating.
		peeked := control.PeekNotificationStatuses(ctx, loc)
		if p := peeked["ops-slack"]; p.Delivered != 2 || p.Failed != 1 || !p.LastOKAt.Equal(t3) {
			t.Fatalf("peeked ops-slack = %+v", p)
		}
		if p := peeked["pager"]; p.Failed != 1 || p.LastError != "request timed out" {
			t.Fatalf("peeked pager = %+v", p)
		}
	})

	// Missing SQLite file returns nil cleanly and does not create it.
	missing := filepath.Join(t.TempDir(), "does-not-exist.db")
	if got := control.PeekNotificationStatuses(context.Background(), missing); len(got) != 0 {
		t.Fatalf("peek missing db = %+v, want empty", got)
	}
}

func TestFinishDriftStatusTracksTransitions(t *testing.T) {
	controltest.Each(t, func(t *testing.T, e controltest.Engine) {
		ctx := context.Background()
		s, err := control.Open(ctx, control.Options{Location: e.New(t)})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()

		t0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
		steps := []struct {
			ref      string
			status   control.DriftStatus
			wantPrev control.DriftStatus
		}{
			{"DRIFT1", control.DriftClean, ""},
			{"DRIFT2", control.DriftDrifted, control.DriftClean},
			{"DRIFT3", control.DriftDrifted, control.DriftDrifted},
			{"DRIFT4", control.DriftClean, control.DriftDrifted},
			{"DRIFT5", control.DriftClean, control.DriftClean},
		}
		for i, st := range steps {
			at := t0.Add(time.Duration(i) * time.Minute)
			if err := s.BeginDrift(ctx, st.ref, "prod", "network", at); err != nil {
				t.Fatalf("BeginDrift %s: %v", st.ref, err)
			}
			stored, prev, err := s.FinishDriftStatus(ctx, control.DriftCheck{
				Ref:    st.ref,
				Env:    "prod",
				Stack:  "network",
				Status: st.status,
				MadeAt: at.Add(10 * time.Second),
			})
			if err != nil {
				t.Fatalf("FinishDriftStatus %s: %v", st.ref, err)
			}
			if !stored {
				t.Fatalf("step %s not stored", st.ref)
			}
			if prev != st.wantPrev {
				t.Errorf("step %s (%s): prev = %q, want %q", st.ref, st.status, prev, st.wantPrev)
			}
		}
	})
}
