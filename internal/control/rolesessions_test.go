package control_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/awsrole"
	"github.com/mr-karan/writ/internal/control"
)

// The control plane records each role session it assumes. A refresh moves
// the expiry and keeps the first time, and each source identity is its own
// session. Sessions go two weeks after they expire.
func TestRoleSessions(t *testing.T) {
	each(t, func(t *testing.T, s *control.Store, _ string) {
		ctx := context.Background()
		const role = "arn:aws:iam::123456789012:role/writ-plan"
		first := awsrole.AssumedSession{Role: role, Name: "writ-ci-p1", SourceIdentity: "gitlab-4242-1", From: t0, Until: t0.Add(time.Hour)}
		refresh := first
		refresh.From, refresh.Until = t0.Add(46*time.Minute), t0.Add(106*time.Minute)
		other := awsrole.AssumedSession{Role: role, Name: "writ-ci-p1", SourceIdentity: "gitlab-4242-2", From: t0, Until: t0.Add(time.Hour)}
		for _, r := range []awsrole.AssumedSession{first, refresh, other, first} {
			if err := s.RecordRoleSession(ctx, r); err != nil {
				t.Fatal(err)
			}
		}
		got, err := s.RoleSessions(ctx, role, "writ-ci-p1")
		if err != nil {
			t.Fatal(err)
		}
		slices.SortFunc(got, func(a, b awsrole.AssumedSession) int { return strings.Compare(a.SourceIdentity, b.SourceIdentity) })
		want := []awsrole.AssumedSession{{Role: role, Name: "writ-ci-p1", SourceIdentity: "gitlab-4242-1", From: t0, Until: t0.Add(106 * time.Minute)}, other}
		if len(got) != 2 || !got[0].From.Equal(want[0].From) || !got[0].Until.Equal(want[0].Until) || got[1].SourceIdentity != "gitlab-4242-2" || !got[1].Until.Equal(other.Until) {
			t.Fatalf("sessions %+v, want %+v", got, want)
		}
		if none, err := s.RoleSessions(ctx, role, "writ-outsider"); err != nil || len(none) != 0 {
			t.Fatalf("a session never recorded: %+v %v", none, err)
		}
		const keep = 14 * 24 * time.Hour
		for _, step := range []struct {
			name   string
			at     time.Time
			pruned int64
			left   []string
		}{
			{"two weeks after the first expiry", t0.Add(keep + time.Hour), 0, []string{"gitlab-4242-1", "gitlab-4242-2"}},
			{"past the unrefreshed expiry", t0.Add(keep + 90*time.Minute), 1, []string{"gitlab-4242-1"}},
			{"past the refreshed expiry", t0.Add(keep + 107*time.Minute), 1, nil},
		} {
			pruned, err := s.Prune(ctx, step.at, control.Retention{})
			if err != nil || pruned.RoleSessions != step.pruned {
				t.Fatalf("%s: pruned %+v %v", step.name, pruned, err)
			}
			left, err := s.RoleSessions(ctx, role, "writ-ci-p1")
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, r := range left {
				ids = append(ids, r.SourceIdentity)
			}
			slices.Sort(ids)
			if !slices.Equal(ids, step.left) {
				t.Fatalf("%s: left %v, want %v", step.name, ids, step.left)
			}
		}
	})
}
