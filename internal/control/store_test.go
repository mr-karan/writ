package control_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/authz"
	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/control/controltest"
	"github.com/mr-karan/writ/internal/control/pgtest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

var t0 = time.Date(2026, 11, 10, 10, 42, 0, 0, time.UTC)

// each runs a store test on every engine, with a new database each time.
func each(t *testing.T, f func(t *testing.T, s *control.Store, loc string)) {
	controltest.Each(t, func(t *testing.T, e controltest.Engine) {
		s, loc := controltest.Open(t, e)
		f(t, s, loc)
	})
}

func TestMigrateIsIdempotent(t *testing.T) {
	each(t, func(t *testing.T, s *control.Store, u string) {
		ctx := context.Background()
		v, err := controltest.Raw(t, u).Version(ctx)
		if err != nil || v != control.Migrations() {
			t.Fatalf("version %d, %v", v, err)
		}
		var applied []string
		rows, err := controltest.Raw(t, u).QueryContext(ctx, "SELECT name FROM schema_migrations ORDER BY version")
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			applied = append(applied, name)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(applied, control.MigrationFiles()) {
			t.Fatalf("a new database applied %v, want %v", applied, control.MigrationFiles())
		}
		again, err := control.Open(ctx, control.Options{Location: u})
		if err != nil {
			t.Fatal(err)
		}
		again.Close()
		raw := controltest.Raw(t, u)
		if _, err := raw.ExecContext(ctx, "INSERT INTO schema_migrations (version, name, applied_at) VALUES (99, 'future', 0)"); err != nil {
			t.Fatal(err)
		}
		if _, err := control.Open(ctx, control.Options{Location: u}); err == nil || !strings.Contains(err.Error(), "newer writ") {
			t.Fatalf("opened a database from a newer writ: %v", err)
		}
	})
}

// A control database whose migration 1 is another file than this writ's,
// such as the Postgres-only schema of earlier builds, is refused by name.
func TestMigrationOfAnotherBuild(t *testing.T) {
	each(t, func(t *testing.T, s *control.Store, u string) {
		ctx := context.Background()
		raw := controltest.Raw(t, u)
		if _, err := raw.ExecContext(ctx, "UPDATE schema_migrations SET name = '0001_init.sql' WHERE version = 1"); err != nil {
			t.Fatal(err)
		}
		_, err := control.Open(ctx, control.Options{Location: u})
		if err == nil || !strings.Contains(err.Error(), "recorded migration 1 as 0001_init.sql") {
			t.Fatalf("opened a database of another build: %v", err)
		}
	})
}

func TestCheckURL(t *testing.T) {
	for _, u := range []string{"postgres://writ:hunter2@db/writ", "postgres://writ@db/writ?password=x", "mysql://db/writ", "/var/lib/writ/control.db"} {
		if err := control.CheckURL(u); err == nil {
			t.Errorf("accepted %s", u)
		}
	}
	if err := control.CheckURL("postgres://writ@db/writ?sslmode=verify-full&passfile=/run/secrets/pgpass"); err != nil {
		t.Error(err)
	}
}

func policy() *authz.Policy {
	return &authz.Policy{
		Teams:  []authz.Team{{Name: "infra_mr", Issuer: "gitlab", Match: []authz.Rule{{Claim: "project_id", Values: []string{"4242"}}}}},
		Grants: []authz.Grant{{Teams: []string{"infra_mr"}, Stacks: []string{"*"}, Envs: []string{"prod"}, Can: []authz.Op{authz.Read, authz.Plan}}},
	}
}

func TestRecordConfig(t *testing.T) { each(t, testRecordConfig) }

func testRecordConfig(t *testing.T, s *control.Store, _ string) {
	ctx := context.Background()
	id, err := s.RecordConfig(ctx, "{ url: \"https://writ.example.com\" }", "https://writ.example.com", "host 1", policy(), t0)
	if err != nil {
		t.Fatal(err)
	}
	same, err := s.RecordConfig(ctx, "{ url: \"https://writ.example.com\" }", "https://writ.example.com", "host 2", policy(), t0.Add(time.Hour))
	if err != nil || same != id {
		t.Fatalf("the same text got id %d, want %d: %v", same, id, err)
	}
	other, err := s.RecordConfig(ctx, "{ url: \"https://writ.example.com\" } ", "https://writ.example.com", "host 2", policy(), t0)
	if err != nil || other == id {
		t.Fatalf("a new text got id %d: %v", other, err)
	}
}

func newPlan(ref string, cfg int64, env, stack string, at time.Time) control.Plan {
	return control.Plan{
		Ref: ref, Env: env, Stack: stack, RequestedAt: at, SourceDigest: "abc", ConfigID: cfg,
		Author: control.Principal{Issuer: "gitlab", Subject: "project_path:acme/infra", Person: "gitlab.com/user/1001", Display: "alice", Detail: json.RawMessage(`{"kind":"ci-job"}`)},
		Teams:  []string{"infra_mr"}, Origin: json.RawMessage(`{"project_id":"4242","pipeline":"1849213"}`),
	}
}

func TestPlanLifecycle(t *testing.T) { each(t, testPlanLifecycle) }

func testPlanLifecycle(t *testing.T, s *control.Store, loc string) {
	ctx := context.Background()
	cfg, err := s.RecordConfig(ctx, "{}", "https://writ.example.com", "test", policy(), t0)
	if err != nil {
		t.Fatal(err)
	}
	use := control.TokenUse{Issuer: "gitlab", JTI: "j1", Op: "plan", Expires: t0.Add(5 * time.Minute)}
	entry := control.Entry{At: t0, Request: "r1", Kind: "request", Decision: control.Allowed, Op: "plan", Env: "prod", Stacks: []string{"network"}, ConfigID: cfg}
	if err := s.CreatePlan(ctx, newPlan("REF1", cfg, "prod", "network", t0), use, entry); err != nil {
		t.Fatal(err)
	}
	// The same token id cannot make a second plan, and nothing is stored.
	entry.Request = "r2"
	if err := s.CreatePlan(ctx, newPlan("REF2", cfg, "prod", "network", t0), use, entry); !errors.Is(err, control.ErrReplay) {
		t.Fatalf("replay: %v", err)
	}
	if _, err := s.Plan(ctx, "REF2", t0); !errors.Is(err, control.ErrNotFound) {
		t.Fatalf("a replayed plan was stored: %v", err)
	}
	if rows := controltest.Audit(t, loc, controltest.AuditFilter{Request: "r2"}); len(rows) != 0 {
		t.Fatal("a rolled-back request left an audit row")
	}
	p, err := s.Plan(ctx, "REF1", t0)
	if err != nil || p.Status != "planning" || p.ID != "" {
		t.Fatalf("got %+v, %v", p, err)
	}
	res := control.PlanResult{
		ID: "3f9c2a7e5b1d48c09e6a2f71b4d8c5e0", Status: "pending", MadeAt: t0.Add(time.Second), ExpiresAt: t0.Add(24 * time.Hour),
		Risks: []string{"create"}, Changes: 1, Document: json.RawMessage(`{"format":"writ.plan.json/v2"}`), Text: "Plan: 1 create.", Saved: []byte("saved"),
	}
	if err := s.FinishPlan(ctx, "REF1", res, control.Entry{At: t0, Request: "r1", Kind: "plan-finished", Decision: control.NoDecision, Plan: res.ID}); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishPlan(ctx, "REF1", res, control.Entry{At: t0, Request: "r1", Kind: "plan-finished", Decision: control.NoDecision}); err == nil {
		t.Fatal("finished a plan twice")
	}
	byID, err := s.Plan(ctx, res.ID, t0.Add(time.Hour))
	if err != nil || byID.Ref != "REF1" || byID.Status != "pending" || byID.Changes != 1 || byID.Saved != nil {
		t.Fatalf("got %+v, %v", byID, err)
	}
	if saved, _, err := s.SavedPlan(ctx, res.ID); err != nil || string(saved) != "saved" {
		t.Fatalf("saved %q, %v", saved, err)
	}
	if late, _ := s.Plan(ctx, res.ID, t0.Add(25*time.Hour)); late.Status != "expired" {
		t.Fatalf("a plan past its expiry reads %s", late.Status)
	}
	// The audit rows of one stack come from audit_stacks.
	if rows := controltest.Audit(t, loc, controltest.AuditFilter{Stack: "network"}); len(rows) != 1 || rows[0].Request != "r1" || rows[0].Stacks[0] != "network" {
		t.Fatalf("audit of network %+v", rows)
	}
	if rows := controltest.Audit(t, loc, controltest.AuditFilter{Stack: "database"}); len(rows) != 0 {
		t.Fatalf("audit of database %+v", rows)
	}
}

func TestPlansPage(t *testing.T) { each(t, testPlansPage) }

func testPlansPage(t *testing.T, s *control.Store, _ string) {
	ctx := context.Background()
	cfg, _ := s.RecordConfig(ctx, "{}", "u", "test", policy(), t0)
	for i, st := range []string{"a", "b", "a", "a", "c"} {
		ref := string(rune('A' + i))
		use := control.TokenUse{Issuer: "gitlab", JTI: ref, Op: "plan", Expires: t0.Add(time.Hour)}
		e := control.Entry{At: t0, Request: ref, Kind: "request", Decision: control.Allowed}
		if err := s.CreatePlan(ctx, newPlan(ref, cfg, "prod", st, t0.Add(time.Duration(i)*time.Minute)), use, e); err != nil {
			t.Fatal(err)
		}
	}
	var refs []string
	cursor := ""
	for {
		page, next, err := s.Plans(ctx, control.PlanFilter{Env: "prod", Stack: "a", Limit: 2, Cursor: cursor}, t0)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range page {
			refs = append(refs, p.Ref)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if strings.Join(refs, "") != "DCA" {
		t.Fatalf("pages %v, want D C A", refs)
	}
	if _, _, err := s.Plans(ctx, control.PlanFilter{Limit: 2, Cursor: "garbage"}, t0); !errors.Is(err, control.ErrBadCursor) {
		t.Fatalf("bad cursor: %v", err)
	}
}

func TestFailPlanningAndPrune(t *testing.T) { each(t, testFailPlanningAndPrune) }

func testFailPlanningAndPrune(t *testing.T, s *control.Store, _ string) {
	ctx := context.Background()
	cfg, _ := s.RecordConfig(ctx, "{}", "u", "test", policy(), t0)
	e := control.Entry{At: t0, Request: "r", Kind: "request", Decision: control.Allowed}
	if err := s.CreatePlan(ctx, newPlan("LEFT", cfg, "prod", "a", t0), control.TokenUse{Issuer: "g", JTI: "1", Op: "plan", Expires: t0}, e); err != nil {
		t.Fatal(err)
	}
	if n, err := s.FailPlanning(ctx, t0); err != nil || n != 1 {
		t.Fatalf("marked %d: %v", n, err)
	}
	p, _ := s.Plan(ctx, "LEFT", t0)
	if p.Status != "failed" {
		t.Fatalf("status %s", p.Status)
	}
	got, err := s.Prune(ctx, t0.Add(48*time.Hour), control.Retention{Audit: 24 * time.Hour, Plans: 24 * time.Hour})
	if err != nil || got.Plans != 1 || got.Audit != 1 || got.Tokens != 1 {
		t.Fatalf("pruned %+v, %v", got, err)
	}
}

// A newer pending plan from the same merge request supersedes an older
// one of the same stack@env, and nothing else. A merge request is named
// by its issuer too: another GitLab's project and merge request of the
// same ids is another merge request.
func TestSupersededBy(t *testing.T) { each(t, testSupersededBy) }

func testSupersededBy(t *testing.T, s *control.Store, _ string) {
	ctx := context.Background()
	cfg, _ := s.RecordConfig(ctx, "{}", "u", "test", policy(), t0)
	issuer := "https://gitlab.com"
	finish := func(ref, stack, mr string, at time.Time, id string) {
		t.Helper()
		p := newPlan(ref, cfg, "prod", stack, at)
		p.Origin = json.RawMessage(`{"issuer":"` + issuer + `","project_id":"4242","merge_request":` + mr + `}`)
		e := control.Entry{At: at, Request: ref, Kind: "request", Decision: control.Allowed}
		if err := s.CreatePlan(ctx, p, control.TokenUse{Issuer: "g", JTI: ref, Op: "plan", Expires: at.Add(time.Hour)}, e); err != nil {
			t.Fatal(err)
		}
		res := control.PlanResult{ID: id, Status: "pending", MadeAt: at, ExpiresAt: at.Add(time.Hour)}
		if id == "" {
			res.Status = "refused"
		}
		if err := s.FinishPlan(ctx, ref, res, control.Entry{At: at, Request: ref, Kind: "plan-finished", Decision: control.NoDecision}); err != nil {
			t.Fatal(err)
		}
	}
	finish("A", "network", "42", t0, "a1")
	finish("B", "database", "42", t0.Add(time.Minute), "b1")
	finish("C", "network", "43", t0.Add(2*time.Minute), "c1")
	finish("D", "network", "42", t0.Add(3*time.Minute), "")
	finish("E", "network", "42", t0.Add(4*time.Minute), "e1")
	issuer = "https://gitlab.example.com"
	finish("F", "network", "42", t0.Add(5*time.Minute), "f1")
	want := map[string]string{"A": "e1", "B": "", "C": "", "D": "", "E": "", "F": ""}
	for ref, by := range want {
		p, err := s.Plan(ctx, ref, t0)
		if err != nil || p.SupersededBy != by {
			t.Errorf("plan %s superseded by %q, want %q: %v", ref, p.SupersededBy, by, err)
		}
	}
}
