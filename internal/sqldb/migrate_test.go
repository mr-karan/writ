package sqldb_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/mr-karan/writ/internal/sqldb"
)

func migrations(files map[string]string) sqldb.Migrations {
	fs := fstest.MapFS{}
	for name, text := range files {
		fs["m/"+name] = &fstest.MapFile{Data: []byte(text)}
	}
	return sqldb.Migrations{Files: fs, Dir: "m", What: "test database", Now: time.Now}
}

func TestMigrateNumbers(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		first map[string]string
		then  map[string]string
		err   string
	}{
		{name: "a skipped number applies", first: map[string]string{"0001_a.sql": "CREATE TABLE a (x integer)", "0003_c.sql": "CREATE TABLE c (x integer)"}},
		{name: "a number filled in later is refused", first: map[string]string{"0001_a.sql": "CREATE TABLE a (x integer)", "0003_c.sql": "CREATE TABLE c (x integer)"},
			then: map[string]string{"0001_a.sql": "CREATE TABLE a (x integer)", "0002_b.sql": "CREATE TABLE b (x integer)", "0003_c.sql": "CREATE TABLE c (x integer)"},
			err:  "is at schema version 3 and lacks migration 0002_b.sql, which comes before it"},
		{name: "a number after the highest applies across a gap", first: map[string]string{"0001_a.sql": "CREATE TABLE a (x integer)", "0003_c.sql": "CREATE TABLE c (x integer)"},
			then: map[string]string{"0001_a.sql": "CREATE TABLE a (x integer)", "0003_c.sql": "CREATE TABLE c (x integer)", "0005_e.sql": "CREATE TABLE e (x integer)"}},
		{name: "a recorded migration this build lacks", first: map[string]string{"0001_a.sql": "CREATE TABLE a (x integer)", "0002_b.sql": "CREATE TABLE b (x integer)", "0003_c.sql": "CREATE TABLE c (x integer)"},
			then: map[string]string{"0001_a.sql": "CREATE TABLE a (x integer)", "0003_c.sql": "CREATE TABLE c (x integer)"}, err: "recorded migration 2 as 0002_b.sql, which this writ does not have"},
		{name: "a newer database", first: map[string]string{"0001_a.sql": "CREATE TABLE a (x integer)", "0004_d.sql": "CREATE TABLE d (x integer)"},
			then: map[string]string{"0001_a.sql": "CREATE TABLE a (x integer)"}, err: "schema version 4, and this writ knows 1"},
		{name: "a name with no number", first: map[string]string{"0001_a.sql": "CREATE TABLE a (x integer)", "init.sql": "CREATE TABLE b (x integer)"}, err: "init.sql is out of sequence"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := sqldb.OpenSQLite(filepath.Join(t.TempDir(), "t.db"), sqldb.Durable, "test database")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			err = db.Migrate(ctx, migrations(tc.first))
			if tc.then != nil {
				if err != nil {
					t.Fatal(err)
				}
				err = db.Migrate(ctx, migrations(tc.then))
			}
			if tc.err == "" && err != nil || tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)) {
				t.Fatalf("got %v, want %q", err, tc.err)
			}
			if tc.err != "" {
				return
			}
			want := tc.first
			if tc.then != nil {
				want = tc.then
			}
			for name := range want {
				table := strings.TrimSuffix(strings.SplitN(name, "_", 2)[1], ".sql")
				if _, err := db.ExecContext(ctx, "SELECT x FROM "+table); err != nil {
					t.Errorf("table %s: %v", table, err)
				}
			}
		})
	}
}

// {{now}} in a migration is the time the migrations run, in Unix
// milliseconds.
func TestMigrateNow(t *testing.T) {
	ctx := context.Background()
	db, err := sqldb.OpenSQLite(filepath.Join(t.TempDir(), "t.db"), sqldb.Durable, "test database")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m := migrations(map[string]string{"0001_a.sql": "CREATE TABLE a (at bigint); INSERT INTO a (at) VALUES ({{now}})"})
	at := time.Date(2026, 11, 11, 3, 4, 5, 6e6, time.UTC)
	m.Now = func() time.Time { return at }
	if err := db.Migrate(ctx, m); err != nil {
		t.Fatal(err)
	}
	var got int64
	if err := db.QueryRowContext(ctx, "SELECT at FROM a").Scan(&got); err != nil || got != at.UnixMilli() {
		t.Fatalf("{{now}} wrote %d, %v; want %d", got, err, at.UnixMilli())
	}
}
