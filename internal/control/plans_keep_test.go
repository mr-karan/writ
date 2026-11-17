package control_test

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/control/controltest"
	"github.com/mr-karan/writ/internal/sqldb"
)

// keepRow is a plan as the keep tests seed it.
type keepRow struct {
	ref, env, stack, status string
	at                      int64 // ms after t0
	expired                 bool
	// approvals are the people who approved it; withdrawn approved and
	// withdrew.
	approvals, withdrawn []string
}

// keepRules need two approvals of web, one of private, and none of db, and
// count no approval by ghost. A speculative plan takes approvals, so each
// seeded live plan shows what its approvals reach.
var keepRules = control.Rules{
	Counts:      func(person string) bool { return person != "ghost" },
	Need:        func(p control.Plan) int { return map[string]int{"web": 2, "private": 1}[p.Stack] },
	Speculative: true,
}

// seedKeepPlans stores one plan through the store, then copies it raw into
// each row, so the rows differ only in what the list reads.
func seedKeepPlans(t *testing.T, s *control.Store, loc string, rows []keepRow) *sqldb.DB {
	t.Helper()
	ctx := context.Background()
	cfg, err := s.RecordConfig(ctx, "{}", "u", "test", policy(), t0)
	if err != nil {
		t.Fatal(err)
	}
	use := control.TokenUse{Issuer: "gitlab", JTI: "seed", Op: "plan", Expires: t0.Add(time.Hour)}
	if err := s.CreatePlan(ctx, newPlan("SEED", cfg, "seed", "seed", t0), use, control.Entry{At: t0, Request: "seed", Kind: "request", Decision: control.Allowed}); err != nil {
		t.Fatal(err)
	}
	res := control.PlanResult{ID: "seed-id", Status: "pending", MadeAt: t0, ExpiresAt: t0.Add(24 * time.Hour), Changes: 1, Saved: []byte("saved")}
	if err := s.FinishPlan(ctx, "SEED", res, control.Entry{At: t0, Request: "seed", Kind: "plan-finished", Decision: control.NoDecision}); err != nil {
		t.Fatal(err)
	}
	raw := controltest.Raw(t, loc)
	for _, r := range rows {
		expires := t0.Add(24 * time.Hour)
		if r.expired {
			expires = t0.Add(-time.Minute)
		}
		if _, err := raw.ExecContext(ctx, `INSERT INTO plans (ref, id, status, env, stack, author_issuer, author_subject, author_person, author_display,
			author, teams, origin, requested_at, made_at, expires_at, risks, changes, source_digest, config_id, document, text)
			SELECT ?, ?, ?, ?, ?, author_issuer, author_subject, author_person, author_display, author, teams, origin,
			?, made_at, ?, risks, changes, source_digest, config_id, document, text FROM plans WHERE ref = 'SEED'`,
			r.ref, "id-"+r.ref, r.status, r.env, r.stack, t0.UnixMilli()+r.at, expires.UnixMilli()); err != nil {
			t.Fatal(err)
		}
		for _, a := range slices.Concat(r.approvals, r.withdrawn) {
			var withdrawn any
			if slices.Contains(r.withdrawn, a) {
				withdrawn = t0.UnixMilli()
			}
			if _, err := raw.ExecContext(ctx, `INSERT INTO approvals (plan_id, person, display, issuer, subject, via, approved_at, withdrawn_at)
				VALUES (?, ?, ?, 'gitlab', ?, 'dev token', ?, ?)`, "id-"+r.ref, a, a, a, t0.UnixMilli(), withdrawn); err != nil {
				t.Fatal(err)
			}
		}
	}
	return raw
}

// TestPlansKeepHoldsThePageThroughAStatusChange pages three pending plans
// one at a time while the newest starts to apply between the pass that
// keeps plans and the pass that reads them. No plan that stays pending may
// be skipped.
func TestPlansKeepHoldsThePageThroughAStatusChange(t *testing.T) {
	each(t, func(t *testing.T, s *control.Store, loc string) {
		ctx := context.Background()
		raw := seedKeepPlans(t, s, loc, []keepRow{
			{ref: "P1", env: "prod", stack: "web", status: "pending", at: 1},
			{ref: "P2", env: "prod", stack: "web", status: "pending", at: 2},
			{ref: "P3", env: "prod", stack: "web", status: "pending", at: 3},
		})
		calls := 0
		keep := func(string, string) bool {
			calls++
			if calls == 2 {
				if _, err := raw.ExecContext(ctx, "UPDATE plans SET status = 'applying' WHERE ref = 'P3'"); err != nil {
					t.Error(err)
				}
			}
			return true
		}
		var refs []string
		f := control.PlanFilter{Env: "prod", Status: "pending", Rules: keepRules, Limit: 1, Keep: keep}
		for range 5 {
			page, next, err := s.Plans(ctx, f, t0)
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range page {
				refs = append(refs, p.Ref)
			}
			if next == "" {
				break
			}
			f.Cursor = next
		}
		if !slices.Contains(refs, "P2") || !slices.Contains(refs, "P1") {
			t.Fatalf("pages %v skipped a plan that stayed pending", refs)
		}
		if refs[0] != "P3" {
			t.Fatalf("pages %v, want the first page to hold the plan the first pass kept, P3", refs)
		}
	})
}

// TestPlansKeepPagesAsTheModel pages a fixed estate under every filter,
// limit, and keep, and holds each page and cursor to a model that sorts the
// rows newest first, drops what the filter and keep drop, and cuts pages of
// the limit. A live plan matches the status its approvals reach under
// keepRules, not the stored one. With keep true for all, every page and cursor must equal those
// of the list with no keep, which reads in one pass.
func TestPlansKeepPagesAsTheModel(t *testing.T) {
	var rows []keepRow
	statuses := []string{"pending", "approved", "planning", "applying", "failed", "applied", "refused", "superseded"}
	for i := range 60 {
		rows = append(rows, keepRow{
			ref:     fmt.Sprintf("R%02d", i),
			env:     []string{"prod", "staging"}[i%2],
			stack:   []string{"web", "private", "db"}[i%3],
			status:  statuses[i%len(statuses)],
			at:      int64(1 + i/3), // three plans a millisecond, so ref breaks ties
			expired: i%5 == 0,
		})
		switch i % 7 {
		case 1:
			rows[i].approvals = []string{"ann", "ben"}
		case 2:
			rows[i].approvals, rows[i].withdrawn = []string{"ann"}, []string{"ben"}
		case 3:
			rows[i].approvals = []string{"ghost"}
		case 4:
			rows[i].approvals = []string{"ann", "ghost", "ben"}
		}
	}
	each(t, func(t *testing.T, s *control.Store, loc string) {
		ctx := context.Background()
		seedKeepPlans(t, s, loc, rows)
		rows := append(rows, keepRow{ref: "SEED", env: "seed", stack: "seed", status: "pending"})
		all, _, err := s.Plans(ctx, control.PlanFilter{Limit: 1000}, t0)
		if err != nil {
			t.Fatal(err)
		}
		full := map[string]control.Plan{}
		for _, p := range all {
			full[p.Ref] = p
		}
		keeps := map[string]func(env, stack string) bool{
			"all":  func(string, string) bool { return true },
			"web":  func(_, stack string) bool { return stack == "web" },
			"prod": func(env, _ string) bool { return env == "prod" },
			"none": func(string, string) bool { return false },
		}
		pages := 0
		for _, env := range []string{"", "prod", "missing"} {
			for _, stack := range []string{"", "web", "private"} {
				for _, status := range append([]string{"", "expired"}, statuses...) {
					for _, limit := range []int{1, 2, 7, 200} {
						for name, keep := range keeps {
							want := modelPlans(rows, env, stack, status, keep)
							f := control.PlanFilter{Env: env, Stack: stack, Status: status, Rules: keepRules, Limit: limit, Keep: keep}
							plain := control.PlanFilter{Env: env, Stack: stack, Status: status, Rules: keepRules, Limit: limit}
							for at := 0; ; at += limit {
								got, next, err := s.Plans(ctx, f, t0)
								if err != nil {
									t.Fatal(err)
								}
								pages++
								end := min(at+limit, len(want))
								var wantPage []control.Plan
								for _, ref := range want[at:end] {
									wantPage = append(wantPage, full[ref])
								}
								if !reflect.DeepEqual(got, wantPage) {
									t.Fatalf("env %q stack %q status %q limit %d keep %s, page at %d: %v, want %v", env, stack, status, limit, name, at, refsOf(got), want[at:end])
								}
								if (next == "") != (end == len(want)) {
									t.Fatalf("env %q stack %q status %q limit %d keep %s, page at %d: next %q with %d of %d listed", env, stack, status, limit, name, at, next, end, len(want))
								}
								if name == "all" {
									gotPlain, nextPlain, err := s.Plans(ctx, plain, t0)
									if err != nil || nextPlain != next || !reflect.DeepEqual(gotPlain, got) {
										t.Fatalf("env %q stack %q status %q limit %d, page at %d: keep all %v next %q, no keep %v next %q, %v", env, stack, status, limit, at, refsOf(got), next, refsOf(gotPlain), nextPlain, err)
									}
									plain.Cursor = nextPlain
								}
								if next == "" {
									break
								}
								f.Cursor = next
							}
						}
					}
				}
			}
		}
		t.Logf("%d pages and cursors held to the model", pages)
	})
}

// modelPlans is the refs a list must page through: the rows of env, stack,
// and status of now that keep holds, newest first and by ref on a tie. A
// pending or approved plan past its expiry reads as expired, and one
// before it reads as approved when the approvals that count reach what
// keepRules need of its stack, and pending if not.
func modelPlans(rows []keepRow, env, stack, status string, keep func(env, stack string) bool) []string {
	var match []keepRow
	for _, r := range rows {
		live := r.status
		if live == "pending" || live == "approved" {
			counted := 0
			for _, a := range r.approvals {
				if a != "ghost" {
					counted++
				}
			}
			live = "pending"
			if counted >= map[string]int{"web": 2, "private": 1}[r.stack] {
				live = "approved"
			}
			if r.expired {
				live = "expired"
			}
		}
		if (env == "" || r.env == env) && (stack == "" || r.stack == stack) && (status == "" || live == status) && keep(r.env, r.stack) {
			match = append(match, r)
		}
	}
	slices.SortFunc(match, func(a, b keepRow) int {
		if a.at != b.at {
			return int(b.at - a.at)
		}
		return strings.Compare(b.ref, a.ref)
	})
	refs := []string{}
	for _, r := range match {
		refs = append(refs, r.ref)
	}
	return refs
}

func refsOf(ps []control.Plan) []string {
	var refs []string
	for _, p := range ps {
		refs = append(refs, p.Ref)
	}
	return refs
}
