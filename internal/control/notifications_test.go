package control_test

import (
	"context"
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

		if err := s.RecordNotification(ctx, control.NotificationResult{Name: "ops-slack", Event: "drift", OK: true, At: t1}); err != nil {
			t.Fatalf("record ok: %v", err)
		}
		if err := s.RecordNotification(ctx, control.NotificationResult{Name: "ops-slack", Event: "apply-failed", Failed: 1, Err: "http status 502", At: t2}); err != nil {
			t.Fatalf("record fail: %v", err)
		}
		if err := s.RecordNotification(ctx, control.NotificationResult{Name: "pager", Event: "outside-write", Failed: 1, Err: "request timed out", At: t2}); err != nil {
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
		if err := s.RecordNotification(ctx, control.NotificationResult{Name: "pager", Event: "drift", Failed: 3, Err: "queue full: dropped 3 events", At: t3}); err != nil {
			t.Fatalf("record drops: %v", err)
		}
		if err := s.RecordNotification(ctx, control.NotificationResult{Name: "ops-slack", Event: "applied", OK: true, At: t3}); err != nil {
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
		if pg := got["pager"]; pg.Failed != 4 || pg.LastError != "queue full: dropped 3 events" {
			t.Fatalf("pager after drops = %+v", pg)
		}

		if e.Name != "postgres" {
			return
		}
		peeked, err := control.PeekNotificationStatuses(ctx, loc)
		if err != nil {
			t.Fatalf("peek: %v", err)
		}
		if p := peeked["ops-slack"]; p.Delivered != 2 || p.Failed != 1 || !p.LastOKAt.Equal(t3) {
			t.Fatalf("peeked ops-slack = %+v", p)
		}
		if p := peeked["pager"]; p.Failed != 4 || p.LastError != "queue full: dropped 3 events" {
			t.Fatalf("peeked pager = %+v", p)
		}
	})
}

func TestFinishDriftReturnsPreviousDriftStatus(t *testing.T) {
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
			{"DRIFT1", control.DriftFailed, ""},
			{"DRIFT2", control.DriftClean, ""},
			{"DRIFT3", control.DriftDrifted, control.DriftClean},
			{"DRIFT4", control.DriftFailed, control.DriftDrifted},
			{"DRIFT5", control.DriftDrifted, control.DriftDrifted},
			{"DRIFT6", control.DriftClean, control.DriftDrifted},
			{"DRIFT7", control.DriftFailed, control.DriftClean},
			{"DRIFT8", control.DriftClean, control.DriftClean},
		}
		for i, st := range steps {
			at := t0.Add(time.Duration(i) * time.Minute)
			if err := s.BeginDrift(ctx, st.ref, "prod", "network", at); err != nil {
				t.Fatalf("BeginDrift %s: %v", st.ref, err)
			}
			stored, prev, err := s.FinishDrift(ctx, control.DriftCheck{
				Ref:    st.ref,
				Env:    "prod",
				Stack:  "network",
				Status: st.status,
				MadeAt: at.Add(10 * time.Second),
			})
			if err != nil {
				t.Fatalf("FinishDrift %s: %v", st.ref, err)
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
