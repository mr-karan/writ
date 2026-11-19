package control_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/control/controltest"
)

// requests are the request ids of rows of kind "t", in order, so a test
// sees only the rows it wrote and not those its fixtures wrote.
func requests(rows []control.Entry) string {
	var out []string
	for _, e := range rows {
		if e.Kind == "t" {
			out = append(out, e.Request)
		}
	}
	return strings.Join(out, " ")
}

func TestAuditRowsFilters(t *testing.T) {
	each(t, func(t *testing.T, s *control.Store, _ string) {
		ctx := context.Background()
		l := newLifecycle(t, s)
		id := l.plan("network", t0, control.Pending, 0)
		p, err := s.Plan(ctx, id, t0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := l.begin(id, "RUN1", "", t0); err != nil {
			t.Fatal(err)
		}
		rows := []control.Entry{
			{Request: "a", Env: "prod", Stacks: []string{"network"}, Plan: p.Ref, Person: "gitlab/user/1", Op: "plan"},
			{Request: "b", Env: "prod", Stacks: []string{"network"}, Plan: id, Person: "gitlab/user/2", Op: "approve"},
			{Request: "c", Env: "prod", Stacks: []string{"network"}, Plan: id, Run: "RUN1", Person: "gitlab/user/1", Op: "apply"},
			{Request: "d", Env: "prod", Stacks: []string{"network"}, Run: "RUN1", Op: "state-write"},
			{Request: "e", Env: "prod", Stacks: []string{"app", "network"}, Person: "gitlab/user/2", Op: "read"},
			{Request: "f", Env: "staging", Stacks: []string{"network"}, Person: "gitlab/user/1", Op: "read"},
			{Request: "g", Op: "admin"},
		}
		for i, e := range rows {
			e.At, e.Kind, e.Decision = t0.Add(time.Duration(i)*time.Minute), "t", control.Allowed
			if err := s.Audit(ctx, e); err != nil {
				t.Fatal(err)
			}
		}
		for _, tc := range []struct {
			name string
			f    control.AuditFilter
			want string
		}{
			{"every row, newest first", control.AuditFilter{}, "g f e d c b a"},
			{"a plan by its id: its ref, its id, and its run", control.AuditFilter{Plan: id}, "d c b a"},
			{"a plan by its ref", control.AuditFilter{Plan: p.Ref}, "d c b a"},
			{"a plan writ serve no longer keeps", control.AuditFilter{Plan: "0123456789abcdef0123456789abcdef"}, ""},
			{"a run", control.AuditFilter{Run: "RUN1"}, "d c"},
			{"an environment", control.AuditFilter{Env: "staging"}, "f"},
			{"a stack in an environment", control.AuditFilter{Env: "prod", Stack: "app"}, "e"},
			{"a stack named with another", control.AuditFilter{Env: "prod", Stack: "network"}, "e d c b a"},
			{"a person", control.AuditFilter{Person: "gitlab/user/2"}, "e b"},
			{"an operation", control.AuditFilter{Op: "read"}, "f e"},
			{"since is inclusive", control.AuditFilter{Since: t0.Add(5 * time.Minute)}, "g f"},
			{"until is exclusive", control.AuditFilter{Until: t0.Add(2 * time.Minute)}, "b a"},
			{"since and until", control.AuditFilter{Since: t0.Add(time.Minute), Until: t0.Add(3 * time.Minute)}, "c b"},
			{"filters combine", control.AuditFilter{Env: "prod", Person: "gitlab/user/1"}, "c a"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				tc.f.Limit = 100
				got, next, err := s.AuditRows(ctx, tc.f)
				if err != nil {
					t.Fatal(err)
				}
				if requests(got) != tc.want || next != "" {
					t.Fatalf("rows %q next %q, want %q", requests(got), next, tc.want)
				}
			})
		}
		got, _, err := s.AuditRows(ctx, control.AuditFilter{Run: "RUN1", Limit: 1})
		if err != nil || len(got) != 1 {
			t.Fatalf("one row: %+v %v", got, err)
		}
		if e := got[0]; e.Request != "d" || !e.At.Equal(t0.Add(3*time.Minute)) || e.Op != "state-write" || e.Env != "prod" || strings.Join(e.Stacks, ",") != "network" || e.Decision != control.Allowed {
			t.Fatalf("the row read back: %+v", e)
		}
	})
}

// Pages follow each other with no row twice or missing, rows of the same
// millisecond included, and rows Keep drops take no place.
func TestAuditRowsPages(t *testing.T) {
	each(t, func(t *testing.T, s *control.Store, _ string) {
		ctx := context.Background()
		names := "abcdefghij"
		for i, r := range names {
			// Two rows share each millisecond, so the order within one
			// comes from the row's id.
			e := control.Entry{At: t0.Add(time.Duration(i/2) * time.Millisecond), Request: string(r), Kind: "t", Decision: control.Allowed, Env: "prod", Stacks: []string{"a"}}
			if i%3 == 0 {
				e.Stacks = []string{"hidden"}
			}
			if err := s.Audit(ctx, e); err != nil {
				t.Fatal(err)
			}
		}
		pages := func(keep func(control.Entry) bool) []string {
			t.Helper()
			var out []string
			cursor := ""
			for range 20 {
				got, next, err := s.AuditRows(ctx, control.AuditFilter{Limit: 3, Cursor: cursor, Keep: keep})
				if err != nil {
					t.Fatal(err)
				}
				out = append(out, requests(got))
				if next == "" {
					return out
				}
				cursor = next
			}
			t.Fatal("the pages never end")
			return nil
		}
		if got := strings.Join(pages(nil), "|"); got != "j i h|g f e|d c b|a" {
			t.Fatalf("pages %q", got)
		}
		visible := func(e control.Entry) bool { return e.Stacks[0] != "hidden" }
		if got := strings.Join(pages(visible), "|"); got != "i h f|e c b" {
			t.Fatalf("pages of the rows Keep keeps %q", got)
		}
		if _, _, err := s.AuditRows(ctx, control.AuditFilter{Limit: 2, Cursor: "garbage"}); !errors.Is(err, control.ErrBadCursor) {
			t.Fatalf("bad cursor: %v", err)
		}
	})
}

// A caller that may see none of the rows reads at most AuditScan of them
// for one page, and gets a cursor past them.
func TestAuditRowsScanIsBounded(t *testing.T) {
	each(t, func(t *testing.T, s *control.Store, loc string) {
		ctx := context.Background()
		db := controltest.Raw(t, loc)
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		for i := range control.AuditScan + 1 {
			if _, err := tx.ExecContext(ctx, "INSERT INTO audit (at, request, kind, decision) VALUES (?, ?, 'n', 'none')", t0.UnixMilli()+int64(i), "r"); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		seen := 0
		keep := func(control.Entry) bool { seen++; return false }
		got, next, err := s.AuditRows(ctx, control.AuditFilter{Limit: 50, Keep: keep})
		if err != nil || len(got) != 0 || next == "" || seen != control.AuditScan {
			t.Fatalf("first page: %d rows, next %q, %d rows read, %v", len(got), next, seen, err)
		}
		seen = 0
		got, next, err = s.AuditRows(ctx, control.AuditFilter{Limit: 50, Keep: keep, Cursor: next})
		if err != nil || len(got) != 0 || next != "" || seen != 1 {
			t.Fatalf("second page: %d rows, next %q, %d rows read, %v", len(got), next, seen, err)
		}
	})
}
