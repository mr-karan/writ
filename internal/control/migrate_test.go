package control_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/control/controltest"
	"github.com/mr-karan/writ/internal/sqldb"
)

// v4 makes a control database at loc as a build of schema version 4 left
// it: migrations 0001 to 0004 and nothing after.
func v4(t *testing.T, loc string) *sqldb.DB {
	t.Helper()
	files := fstest.MapFS{}
	for _, name := range []string{"0001_schema.sql", "0002_changes.sql", "0003_repos.sql", "0004_plan_ref_check.sql"} {
		b, err := os.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		files["migrations/"+name] = &fstest.MapFile{Data: b}
	}
	raw := controltest.Raw(t, loc)
	if err := raw.Migrate(context.Background(), sqldb.Migrations{Files: files, Dir: "migrations", Schema: control.Schema, What: "v4 database", Now: time.Now}); err != nil {
		t.Fatal(err)
	}
	return raw
}

// No build of schema version 4 had approvals, and its person ids came from
// the issuer's host, which another spelling of the host could split. So
// migration 0005 expires every live plan such a build made, with the
// reason in the plan's events, and keeps its author as history. Before
// it did, a v4 plan by GITLAB.example.com:443/user/1001 stayed pending,
// and gitlab.example.com/user/1001, the same person, approved it.
func TestMigrationExpiresV4Plans(t *testing.T) {
	controltest.Each(t, func(t *testing.T, e controltest.Engine) {
		ctx := context.Background()
		loc := e.New(t)
		raw := v4(t, loc)
		var cfg int64
		if err := raw.QueryRowContext(ctx, `INSERT INTO configs (digest, url, text, first_loaded, last_loaded, loaded_by)
			VALUES ('digest', 'https://writ.example.com', '{}', ?, ?, 'test') RETURNING id`, t0.UnixMilli(), t0.UnixMilli()).Scan(&cfg); err != nil {
			t.Fatal(err)
		}
		const author = "GITLAB.example.com:443/user/1001"
		live := time.Now().Add(time.Hour).UnixMilli()
		for _, p := range []struct {
			ref, status string
			id          any
		}{
			{"PENDING", "pending", "11111111111111111111111111111111"},
			{"PLANNING", "planning", nil},
			{"REFUSED", "refused", "22222222222222222222222222222222"},
			{"APPLIED", "applied", "33333333333333333333333333333333"},
		} {
			if _, err := raw.ExecContext(ctx, `INSERT INTO plans (ref, id, status, env, stack, author_issuer, author_subject, author_person,
				author_display, author, teams, origin, requested_at, made_at, expires_at, source_digest, config_id, saved, repo, source_ref,
				source_commit, source_ref_check)
				VALUES (?, ?, ?, 'prod', 'network', 'gitlab', '1001', ?, 'alice', '{}', '[]', '{}', ?, ?, ?, 'digest', ?, ?, 'infra',
				'refs/heads/mr', 'abcdef', 'pinned')`,
				p.ref, p.id, p.status, author, t0.UnixMilli(), t0.UnixMilli(), live, cfg, []byte("saved")); err != nil {
				t.Fatal(err)
			}
		}
		before := time.Now().Truncate(time.Millisecond)
		s, err := control.Open(ctx, control.Options{Location: loc})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		after := time.Now()

		for ref, want := range map[string]control.PlanStatus{"PENDING": control.Expired, "PLANNING": control.Expired, "REFUSED": control.RefusedPlan, "APPLIED": control.Applied} {
			p, err := s.Plan(ctx, ref, after)
			if err != nil {
				t.Fatal(err)
			}
			if p.Status != want || p.Author.Person != author || p.Author.Display != "alice" || p.Author.Subject != "1001" {
				t.Errorf("plan %s after the migration: status %s, author %+v; want %s by %s", ref, p.Status, p.Author, want, author)
			}
			evs, err := s.PlanEvents(ctx, ref)
			if err != nil {
				t.Fatal(err)
			}
			if want != control.Expired {
				if len(evs) != 0 {
					t.Errorf("plan %s, which was not live, got events %+v", ref, evs)
				}
				continue
			}
			from := control.PlanStatus(map[string]string{"PENDING": "pending", "PLANNING": "planning"}[ref])
			if len(evs) != 1 || evs[0].From != from || evs[0].To != control.Expired || evs[0].Reason != "made before approvals" ||
				evs[0].At.Before(before) || evs[0].At.After(after) || evs[0].Person != "" {
				t.Errorf("plan %s's events: %+v; want one move from %s to expired, made before approvals, between %s and %s", ref, evs, from, before, after)
			}
		}

		// The same person under a canonical spelling of the host cannot
		// approve the plan: it is no longer live.
		_, err = s.Approve(ctx, control.Approval{Person: "gitlab.example.com/user/1001", Display: "alice", ApprovedAt: after},
			[]string{"11111111111111111111111111111111"}, need(1), control.TokenUse{},
			control.Entry{At: after, Kind: "approve", Decision: control.Allowed, ConfigID: cfg})
		var ae *control.ApprovalError
		if !errors.As(err, &ae) || ae.Status != control.Expired {
			t.Errorf("approved a plan of schema version 4: %v", err)
		}
	})
}

// Migration 0006 adds the keys of apply requests and the owner of each
// run to a database with runs. A run from before has no key and no
// owner: it reads as before, and a process's first sweep fails it if it
// was left applying.
func TestMigrateApplyRequests(t *testing.T) {
	controltest.Each(t, func(t *testing.T, e controltest.Engine) {
		ctx := context.Background()
		loc := e.New(t)
		files := fstest.MapFS{}
		for _, name := range []string{"0001_schema.sql", "0002_changes.sql", "0003_repos.sql", "0004_plan_ref_check.sql", "0005_apply.sql"} {
			b, err := os.ReadFile("migrations/" + name)
			if err != nil {
				t.Fatal(err)
			}
			files["migrations/"+name] = &fstest.MapFile{Data: b}
		}
		raw := controltest.Raw(t, loc)
		if err := raw.Migrate(ctx, sqldb.Migrations{Files: files, Dir: "migrations", Schema: control.Schema, What: "v5 database", Now: time.Now}); err != nil {
			t.Fatal(err)
		}
		var cfg int64
		if err := raw.QueryRowContext(ctx, `INSERT INTO configs (digest, url, text, first_loaded, last_loaded, loaded_by)
			VALUES ('digest', 'https://writ.example.com', '{}', ?, ?, 'test') RETURNING id`, t0.UnixMilli(), t0.UnixMilli()).Scan(&cfg); err != nil {
			t.Fatal(err)
		}
		const plan = "11111111111111111111111111111111"
		for _, q := range []string{
			`INSERT INTO plans (ref, id, status, env, stack, author_issuer, author_subject, author_person, author_display, author, teams, origin,
				requested_at, made_at, expires_at, source_digest, config_id, saved)
				VALUES ('REF1', '` + plan + `', 'applying', 'prod', 'network', 'gitlab', '1001', 'gitlab/user/1001', 'alice', '{}', '[]', '{}', ?, ?, ?, 'digest', ?, ?)`,
			`INSERT INTO runs (id, plan_id, env, stack, status, actor_issuer, actor_subject, actor_person, actor_display, actor, via, approvals, named_by, started_at)
				VALUES ('RUN1', '` + plan + `', 'prod', 'network', 'applying', 'gitlab', 'main', 'gitlab/user/1001', 'alice', '{}', 'gitlab job 1', '[]', 'plan', ?)`,
		} {
			args := []any{t0.UnixMilli(), t0.UnixMilli(), t0.Add(time.Hour).UnixMilli(), cfg, []byte("saved")}
			if strings.HasPrefix(strings.TrimSpace(q), "INSERT INTO runs") {
				args = []any{t0.UnixMilli()}
			}
			if _, err := raw.ExecContext(ctx, q, args...); err != nil {
				t.Fatal(err)
			}
		}
		s, err := control.Open(ctx, control.Options{Location: loc})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if run, err := s.GetRun(ctx, "RUN1"); err != nil || run.Status != control.RunApplying || run.PlanID != plan {
			t.Fatalf("the run from before: %+v, %v", run, err)
		}
		if _, err := s.Key(ctx, "1794000000-AAAAAAAAAAAAAAAAAAAAAAAAAA"); !errors.Is(err, control.ErrNotFound) {
			t.Fatalf("a key after the migration: %v", err)
		}
		ended, err := s.FailApplying(ctx, t0, "P1", func(string) bool { return true })
		if err != nil || len(ended) != 1 || ended[0] != "RUN1" {
			t.Fatalf("the first sweep ended %v, %v", ended, err)
		}
		if k, err := s.CloseKey(ctx, "1794000000-AAAAAAAAAAAAAAAAAAAAAAAAAA", t0, t0); err != nil || k.Outcome != control.KeyNone {
			t.Fatalf("close a key: %+v, %v", k, err)
		}
	})
}

// Migration 9 numbers the plans of each stack@env by requested_at, then
// ref, and a plan made after it takes the next number of its stack@env.
func TestMigratePlanSeq(t *testing.T) {
	controltest.Each(t, func(t *testing.T, e controltest.Engine) {
		ctx := context.Background()
		loc := e.New(t)
		files := fstest.MapFS{}
		for i := 1; i <= 8; i++ {
			names, err := filepath.Glob(fmt.Sprintf("migrations/%04d_*.sql", i))
			if err != nil || len(names) != 1 {
				t.Fatalf("migration %d: %v, %v", i, names, err)
			}
			b, err := os.ReadFile(names[0])
			if err != nil {
				t.Fatal(err)
			}
			files[names[0]] = &fstest.MapFile{Data: b}
		}
		raw := controltest.Raw(t, loc)
		if err := raw.Migrate(ctx, sqldb.Migrations{Files: files, Dir: "migrations", Schema: control.Schema, What: "v8 database", Now: time.Now}); err != nil {
			t.Fatal(err)
		}
		var cfg int64
		if err := raw.QueryRowContext(ctx, `INSERT INTO configs (digest, url, text, first_loaded, last_loaded, loaded_by)
			VALUES ('digest', 'https://writ.example.com', '{}', ?, ?, 'test') RETURNING id`, t0.UnixMilli(), t0.UnixMilli()).Scan(&cfg); err != nil {
			t.Fatal(err)
		}
		for _, p := range []struct {
			ref, stack string
			at         time.Time
		}{{"REF3", "app", t0}, {"REF1", "app", t0.Add(time.Minute)}, {"REF2", "app", t0}, {"REF4", "db", t0}} {
			if _, err := raw.ExecContext(ctx, `INSERT INTO plans (ref, status, env, stack, author_issuer, author_subject, author_person, author_display, author, teams, origin,
				requested_at, source_digest, config_id) VALUES (?, 'failed', 'prod', ?, 'gitlab', '1001', 'gitlab/user/1001', 'alice', '{}', '[]', '{}', ?, 'digest', ?)`,
				p.ref, p.stack, p.at.UnixMilli(), cfg); err != nil {
				t.Fatal(err)
			}
		}
		s, err := control.Open(ctx, control.Options{Location: loc})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if err := s.CreatePlan(ctx, newPlan("REF5", cfg, "prod", "app", t0), control.TokenUse{}, control.Entry{At: t0, Request: "R", Kind: "request", Decision: control.Allowed}); err != nil {
			t.Fatal(err)
		}
		rows, err := raw.QueryContext(ctx, "SELECT ref, seq FROM plans ORDER BY ref")
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for rows.Next() {
			var ref string
			var seq int64
			if err := rows.Scan(&ref, &seq); err != nil {
				t.Fatal(err)
			}
			got = append(got, fmt.Sprintf("%s=%d", ref, seq))
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		if want := []string{"REF1=3", "REF2=1", "REF3=2", "REF4=1", "REF5=4"}; !slices.Equal(got, want) {
			t.Fatalf("the plans' seq: %v, want %v", got, want)
		}
	})
}

// Migration 0016 drops the tables only the Writ language and its change
// watcher used, rows and all, and keeps every other row.
func TestMigrateDropsLanguageTables(t *testing.T) {
	controltest.Each(t, func(t *testing.T, e controltest.Engine) {
		ctx := context.Background()
		loc := e.New(t)
		files := fstest.MapFS{}
		names, err := filepath.Glob("migrations/*.sql")
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			if strings.HasPrefix(filepath.Base(name), "0016_") {
				break
			}
			b, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			files[name] = &fstest.MapFile{Data: b}
		}
		raw := controltest.Raw(t, loc)
		if err := raw.Migrate(ctx, sqldb.Migrations{Files: files, Dir: "migrations", Schema: control.Schema, What: "v15 database", Now: time.Now}); err != nil {
			t.Fatal(err)
		}
		at := t0.UnixMilli()
		for _, q := range []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO configs (digest, url, text, first_loaded, last_loaded, loaded_by) VALUES ('digest', 'https://writ.example.com', '{}', ?, ?, 'test')`, []any{at, at}},
			{`INSERT INTO stacks (env, stack, location, created_at) VALUES ('prod', 'network', 'state_prod_network_0a1b2c3d4e', ?)`, []any{at}},
			{`INSERT INTO change_groups (id, env, account, actor_key, actor, service, first_at, last_at, opened_at, received_at, changes, managed, alert)
				VALUES ('G1', 'prod', '123456789012', 'arn:aws:iam::123456789012:user/alice', 'alice', 'ec2.amazonaws.com', ?, ?, ?, ?, 1, 1, 'none')`, []any{at, at, at, at}},
			{`INSERT INTO changes (env, event_id, group_id, at, received_at, account, region, service, name, actor, actor_arn, actor_type, objects, managed)
				VALUES ('prod', 'E1', 'G1', ?, ?, '123456789012', 'ap-south-1', 'ec2.amazonaws.com', 'AuthorizeSecurityGroupIngress', 'alice',
				'arn:aws:iam::123456789012:user/alice', 'IAMUser', '["sg-0a1b2c3d4e5f60718"]', 1)`, []any{at, at}},
			{`INSERT INTO change_targets (env, event_id, stack, addr, type, wire_type, object, outcome)
				VALUES ('prod', 'E1', 'network', 'aws_security_group.web', 'aws_security_group', 'aws_security_group', 'sg-0a1b2c3d4e5f60718', 'indexed')`, nil},
		} {
			if _, err := raw.ExecContext(ctx, q.sql, q.args...); err != nil {
				t.Fatal(err)
			}
		}
		s, err := control.Open(ctx, control.Options{Location: loc})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		for _, table := range []string{"stacks", "change_groups", "changes", "change_targets"} {
			var n int
			if err := raw.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&n); err == nil {
				t.Errorf("table %s survived the migration with %d rows", table, n)
			}
		}
		var n int
		if err := raw.QueryRowContext(ctx, "SELECT count(*) FROM configs").Scan(&n); err != nil || n != 1 {
			t.Fatalf("configs after the migration: %d rows, %v; want the one row kept", n, err)
		}
	})
}

// Migration 19 lets a plan be no-changes. The CHECK of plans named every
// status before it, so a write of no-changes failed; after it, the rows
// from before keep their statuses, their approvals and runs, and the
// CHECK still refuses a status the lifecycle does not know.
func TestMigratePlanNoChanges(t *testing.T) {
	controltest.Each(t, func(t *testing.T, e controltest.Engine) {
		ctx := context.Background()
		loc := e.New(t)
		files := fstest.MapFS{}
		names, err := filepath.Glob("migrations/*.sql")
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			if strings.HasPrefix(filepath.Base(name), "0019_") {
				break
			}
			b, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			files[name] = &fstest.MapFile{Data: b}
		}
		raw := controltest.Raw(t, loc)
		if err := raw.Migrate(ctx, sqldb.Migrations{Files: files, Dir: "migrations", Schema: control.Schema, What: "database before migration 19", Now: time.Now}); err != nil {
			t.Fatal(err)
		}
		var cfg int64
		if err := raw.QueryRowContext(ctx, `INSERT INTO configs (digest, url, text, first_loaded, last_loaded, loaded_by)
			VALUES ('digest', 'https://writ.example.com', '{}', ?, ?, 'test') RETURNING id`, t0.UnixMilli(), t0.UnixMilli()).Scan(&cfg); err != nil {
			t.Fatal(err)
		}
		insert := func(ref, id, status string) error {
			_, err := raw.ExecContext(ctx, `INSERT INTO plans (ref, id, status, env, stack, author_issuer, author_subject, author_person, author_display, author, teams, origin,
				requested_at, made_at, expires_at, source_digest, config_id, seq)
				VALUES (?, ?, ?, 'prod', 'network', 'gitlab', '1001', 'gitlab/user/1001', 'alice', '{}', '[]', '{}', ?, ?, ?, 'digest', ?, ?)`,
				ref, id, status, t0.UnixMilli(), t0.UnixMilli(), t0.Add(time.Hour).UnixMilli(), cfg, len(ref)+int(id[0]))
			return err
		}
		const applied, approved = "11111111111111111111111111111111", "22222222222222222222222222222222"
		if err := insert("REF1", applied, "applied"); err != nil {
			t.Fatal(err)
		}
		if err := insert("REF22", approved, "approved"); err != nil {
			t.Fatal(err)
		}
		for _, q := range []string{
			`INSERT INTO runs (id, plan_id, env, stack, status, actor_issuer, actor_subject, actor_person, actor_display, actor, via, approvals, named_by, started_at, ended_at)
				VALUES ('RUN1', '` + applied + `', 'prod', 'network', 'applied', 'gitlab', 'main', 'gitlab/user/1001', 'alice', '{}', 'gitlab job 1', '[]', 'plan', 1, 2)`,
			`INSERT INTO approvals (plan_id, person, display, issuer, subject, via, approved_at)
				VALUES ('` + approved + `', 'gitlab/user/2002', 'bob', 'gitlab', '2002', 'gitlab job 2', 1)`,
		} {
			if _, err := raw.ExecContext(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
		if err := insert("REF333", "33333333333333333333333333333333", "no-changes"); err == nil {
			t.Fatal("the CHECK of version 17 took no-changes")
		}

		s, err := control.Open(ctx, control.Options{Location: loc})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if err := insert("REF333", "33333333333333333333333333333333", "no-changes"); err != nil {
			t.Fatalf("a no-changes plan after the migration: %v", err)
		}
		if err := insert("REF4444", "44444444444444444444444444444444", "unchanged"); err == nil {
			t.Fatal("the CHECK took a status the lifecycle does not know")
		}
		for id, want := range map[string]control.PlanStatus{applied: control.Applied, approved: control.Approved, "33333333333333333333333333333333": control.NoChanges} {
			if p, err := s.Plan(ctx, id, t0); err != nil || p.Status != want {
				t.Errorf("plan %s: %s, %v; want %s", id, p.Status, err, want)
			}
		}
		if run, err := s.GetRun(ctx, "RUN1"); err != nil || run.PlanID != applied {
			t.Fatalf("the run from before: %+v, %v", run, err)
		}
		if as, err := s.Approvals(ctx, []string{approved}); err != nil || len(as[approved]) != 1 {
			t.Fatalf("the approval from before: %v, %v", as, err)
		}
		if e.Name == "sqlite" {
			for _, check := range []string{"PRAGMA integrity_check", "PRAGMA foreign_key_check"} {
				rows, err := raw.QueryContext(ctx, check)
				if err != nil {
					t.Fatal(err)
				}
				var got []string
				for rows.Next() {
					cols, _ := rows.Columns()
					vals := make([]any, len(cols))
					ptrs := make([]any, len(cols))
					for i := range vals {
						ptrs[i] = &vals[i]
					}
					if err := rows.Scan(ptrs...); err != nil {
						t.Fatal(err)
					}
					got = append(got, fmt.Sprint(vals...))
				}
				rows.Close()
				if check == "PRAGMA integrity_check" && (len(got) != 1 || got[0] != "ok") || check == "PRAGMA foreign_key_check" && len(got) != 0 {
					t.Errorf("%s: %v", check, got)
				}
			}
		}
	})
}

// writ serve --check reads the delivery counts before writ serve migrates
// the database. On the first start after an upgrade to the release that
// adds notification_status, migration 0020, the table is not there yet:
// the peek says so with ErrNotMigrated, and not as a failed operation,
// which --check printed as SQLSTATE 42P01. An empty database, before any
// migration, says the same.
func TestPeekBeforeTheMigration(t *testing.T) {
	if controltest.Default().Name != "postgres" {
		t.Skip("the peek reads Postgres only")
	}
	ctx := context.Background()
	files := fstest.MapFS{}
	ents, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if e.Name() >= "0020_" {
			continue
		}
		b, err := os.ReadFile(filepath.Join("migrations", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		files["migrations/"+e.Name()] = &fstest.MapFile{Data: b}
	}
	empty := controltest.Postgres.New(t)
	if _, err := control.PeekNotificationStatuses(ctx, empty); !errors.Is(err, control.ErrNotMigrated) {
		t.Fatalf("peek of an empty database: %v, want ErrNotMigrated", err)
	}
	loc := controltest.Postgres.New(t)
	raw := controltest.Raw(t, loc)
	if err := raw.Migrate(ctx, sqldb.Migrations{Files: files, Dir: "migrations", Schema: control.Schema, What: "v19 database", Now: time.Now}); err != nil {
		t.Fatal(err)
	}
	if v, err := raw.Version(ctx); err != nil || v != 19 {
		t.Fatalf("the database is at version %d, %v; want 19", v, err)
	}
	if _, err := control.PeekNotificationStatuses(ctx, loc); !errors.Is(err, control.ErrNotMigrated) {
		t.Fatalf("peek of a version 19 database: %v, want ErrNotMigrated", err)
	}
	s, err := control.Open(ctx, control.Options{Location: loc})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if got, err := control.PeekNotificationStatuses(ctx, loc); err != nil || len(got) != 0 {
		t.Fatalf("peek after the migration: %v, %v", got, err)
	}
}
