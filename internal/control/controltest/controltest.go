// Package controltest opens control databases for tests: a SQLite file in
// the test's temporary directory always, and a scratch Postgres database
// when pgtest has a server.
package controltest

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/control/pgtest"
	"github.com/mr-karan/writ/internal/sqldb"
)

// Engine is one engine a test runs on.
type Engine struct {
	Name string // sqlite or postgres
	// New returns the location of a new, empty control database.
	New func(t testing.TB) string
}

// SQLite is a control.db in the test's temporary directory.
var SQLite = Engine{Name: "sqlite", New: func(t testing.TB) string { return filepath.Join(t.TempDir(), "control.db") }}

// Postgres is a new database on pgtest's server.
var Postgres = Engine{Name: "postgres", New: pgtest.New}

// Engines are the engines a store test runs on: SQLite, and Postgres when
// pgtest has a server.
func Engines() []Engine {
	if os.Getenv(pgtest.Env) == "" {
		return []Engine{SQLite}
	}
	return []Engine{SQLite, Postgres}
}

// Each runs f once on every engine of Engines, as a subtest.
func Each(t *testing.T, f func(t *testing.T, e Engine)) {
	t.Helper()
	for _, e := range Engines() {
		t.Run(e.Name, func(t *testing.T) { f(t, e) })
	}
}

// Default is the one engine a test of the code above the store runs on:
// Postgres when pgtest has a server, as just test-serve gives it, and
// SQLite otherwise, as just check runs.
func Default() Engine {
	if os.Getenv(pgtest.Env) == "" {
		return SQLite
	}
	return Postgres
}

// Open opens a new store on e and closes it when the test ends. It returns
// the store and its location.
func Open(t testing.TB, e Engine) (*control.Store, string) {
	t.Helper()
	loc := e.New(t)
	s, err := control.Open(context.Background(), control.Options{Location: loc, ConnectTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, loc
}

// Raw opens the control database at loc for a test's own SQL, with the
// control tables on the search path. It closes when the test ends.
func Raw(t testing.TB, loc string) *sqldb.DB {
	t.Helper()
	db := openRaw(t, loc)
	t.Cleanup(func() { db.Close() })
	return db
}

// openRaw is Raw for a caller that closes the database itself.
func openRaw(t testing.TB, loc string) *sqldb.DB {
	t.Helper()
	var db *sqldb.DB
	var err error
	if sqldb.IsPostgres(loc) {
		db, err = sqldb.OpenPostgres(loc, sqldb.PostgresOptions{SearchPath: control.Schema, What: "test database", ConnectTimeout: time.Minute})
	} else {
		db, err = sqldb.OpenSQLite(loc, sqldb.Durable, "test database")
	}
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// ForConfig opens a store on the Default engine for a test of writ serve,
// and returns it with its location and the database URL the test's config
// should name. A team's config must name Postgres, so on SQLite the URL is
// a placeholder that nothing opens: the test hands the server this store.
func ForConfig(t testing.TB) (s *control.Store, loc, url string) {
	t.Helper()
	return ForConfigOn(t, Default())
}

// ForConfigOn is ForConfig on e.
func ForConfigOn(t testing.TB, e Engine) (s *control.Store, loc, url string) {
	t.Helper()
	s, loc = Open(t, e)
	url = loc
	if !sqldb.IsPostgres(loc) {
		url = "postgres://writ@localhost/unused"
	}
	return s, loc, url
}

// AuditFilter narrows Audit. Empty fields match every row.
type AuditFilter struct {
	Request, Person, Env, Stack, Plan string
	Decision                          control.Decision
	Limit                             int
}

// Audit reads audit rows from the database at loc, newest first. At most
// f.Limit rows are read, or 100 when it is zero.
func Audit(t testing.TB, loc string, f AuditFilter) []control.Entry {
	t.Helper()
	where := []string{"1 = 1"}
	var args []any
	add := func(col, v string) {
		if v != "" {
			args = append(args, v)
			where = append(where, col+" = ?")
		}
	}
	add("request", f.Request)
	add("person", f.Person)
	add("env", f.Env)
	if f.Stack != "" {
		args = append(args, f.Stack)
		where = append(where, "id IN (SELECT audit_id FROM audit_stacks WHERE stack = ?)")
	}
	add("plan", f.Plan)
	add("decision", string(f.Decision))
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	args = append(args, limit)
	q := `SELECT at, request, kind, method, route, status, remote, issuer, subject, person, principal, principal_kind, via, teams, op, env,
		stacks, plan, run, addrs, risks, decision, check_failed, reason, grants, coalesce(config_id, 0), elapsed_ms FROM audit WHERE ` +
		strings.Join(where, " AND ") + " ORDER BY id DESC LIMIT ?"
	// A test may read the audit log once for each of its requests, so the
	// connection closes now: one held per read until the test ends would
	// pass Postgres's max_connections.
	db := openRaw(t, loc)
	defer db.Close()
	rows, err := db.QueryContext(context.Background(), q, args...)
	if err != nil {
		t.Fatalf("read the audit log: %v", err)
	}
	defer rows.Close()
	var out []control.Entry
	for rows.Next() {
		var e control.Entry
		var d, teams, stacks, addrs, risks, grants string
		var at, ms int64
		err := rows.Scan(&at, &e.Request, &e.Kind, &e.Method, &e.Route, &e.Status, &e.Remote, &e.Issuer, &e.Subject, &e.Person, &e.Principal,
			&e.PrincipalKind, &e.Via, &teams, &e.Op, &e.Env, &stacks, &e.Plan, &e.Run, &addrs, &risks, &d, &e.Check, &e.Reason, &grants, &e.ConfigID, &ms)
		if err != nil {
			t.Fatalf("read the audit log: %v", err)
		}
		e.At, e.Decision, e.Elapsed = time.UnixMilli(at).UTC(), control.Decision(d), time.Duration(ms)*time.Millisecond
		for _, l := range []struct {
			raw string
			to  any
		}{{teams, &e.Teams}, {stacks, &e.Stacks}, {addrs, &e.Addrs}, {risks, &e.Risks}, {grants, &e.Grants}} {
			if err := json.Unmarshal([]byte(l.raw), l.to); err != nil {
				t.Fatalf("read the audit log: %v", err)
			}
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the audit log: %v", err)
	}
	return out
}
