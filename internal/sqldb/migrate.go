package sqldb

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Migrations is a numbered list of schema files, NNNN_name.sql, written
// once in the subset of SQL both engines run. The numbers rise and may
// skip, so branches that each add one can claim distinct numbers. A
// database is never given a migration below the highest it has, so every
// database ran its migrations in one order. Two words differ between the
// engines, and the runner substitutes them:
//
//   - {{blob}} is bytea on Postgres and blob on SQLite.
//   - {{id}} is a bigint identity primary key on Postgres and an INTEGER
//     PRIMARY KEY, SQLite's row id, on SQLite. Both take RETURNING id.
//
// {{now}} is the time the migrations run, in Unix milliseconds, for a row
// a migration writes, such as an event: the engines spell the current
// time differently.
//
// A migration that the subset cannot say, such as a change to a table's
// CHECK, which SQLite cannot alter, is a pair of files of one number:
// NNNN_name.postgres.sql and NNNN_name.sqlite.sql. Each engine runs its
// own and records it as NNNN_name.sql. A file of the pair without its twin
// is refused, so no engine skips a migration the other runs.
type Migrations struct {
	// Files holds the migrations; Dir is the directory in it.
	Files fs.FS
	Dir   string
	// Schema is the Postgres schema the tables live in, made before the
	// first migration. SQLite has none.
	Schema string
	// Lock is the Postgres advisory lock that serializes migrations of one
	// database across processes. SQLite's BEGIN IMMEDIATE does the same.
	Lock int64
	// What names the database in errors, such as "control database".
	What string
	Now  func() time.Time
}

// Dialect substitutes the words of the migration text that differ between
// the engines.
func Dialect(text string, pg bool) string {
	blob, id := "blob", "INTEGER PRIMARY KEY"
	if pg {
		blob, id = "bytea", "bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY"
	}
	return strings.NewReplacer("{{blob}}", blob, "{{id}}", id).Replace(text)
}

// Migrate applies, in one transaction, every migration the database has not
// applied, and records each in schema_migrations. A database migrated by a
// newer writ, or one whose recorded migration has another name than the
// file of the same number, is refused.
func (d *DB) Migrate(ctx context.Context, m Migrations) error {
	names, err := fs.Glob(m.Files, path.Join(m.Dir, "*.sql"))
	if err != nil {
		return err
	}
	slices.Sort(names)
	files, err := engineFiles(names, d.pg)
	if err != nil {
		return err
	}
	now := strconv.FormatInt(m.Now().UnixMilli(), 10)
	return d.Write(ctx, func(tx *Tx) error {
		var stmts []string
		if d.pg {
			stmts = append(stmts, "SELECT pg_advisory_xact_lock("+strconv.FormatInt(m.Lock, 10)+")")
			if m.Schema != "" {
				stmts = append(stmts, "CREATE SCHEMA IF NOT EXISTS "+QuoteIdent(m.Schema))
			}
		}
		stmts = append(stmts, "CREATE TABLE IF NOT EXISTS schema_migrations (version integer PRIMARY KEY, name text NOT NULL, applied_at bigint NOT NULL)")
		for _, q := range stmts {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return fmt.Errorf("migrate the %s: %w", m.What, err)
			}
		}
		applied := map[int]string{}
		rows, err := tx.QueryContext(ctx, "SELECT version, name FROM schema_migrations")
		if err != nil {
			return fmt.Errorf("migrate the %s: %w", m.What, err)
		}
		for rows.Next() {
			var v int
			var name string
			if err := rows.Scan(&v, &name); err != nil {
				rows.Close()
				return err
			}
			applied[v] = name
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		versions := make([]int, len(files))
		for i, f := range files {
			base := f.name
			v, err := strconv.Atoi(strings.SplitN(base, "_", 2)[0])
			if err != nil || len(base) < 6 || base[4] != '_' || v < 1 || i > 0 && v <= versions[i-1] {
				return fmt.Errorf("migration %s is out of sequence", base)
			}
			versions[i] = v
		}
		newest := 0
		if len(versions) > 0 {
			newest = versions[len(versions)-1]
		}
		if len(applied) > 0 && slices.Max(mapKeys(applied)) > newest {
			return fmt.Errorf("the %s is at schema version %d, and this writ knows %d: run a newer writ", m.What, slices.Max(mapKeys(applied)), newest)
		}
		for v, name := range applied {
			if !slices.Contains(versions, v) {
				return fmt.Errorf("the %s recorded migration %d as %s, which this writ does not have: it was made by another build of writ; make a new database", m.What, v, name)
			}
		}
		highest := 0
		if len(applied) > 0 {
			highest = slices.Max(mapKeys(applied))
		}
		for i, f := range files {
			version, base, file := versions[i], f.name, f.path
			if have, ok := applied[version]; ok {
				if have != base {
					return fmt.Errorf("the %s recorded migration %d as %s, and this writ's is %s: it was made by another build of writ; make a new database", m.What, version, have, base)
				}
				continue
			}
			// Applied after a higher one, a migration would run in another
			// order than on a fresh database, and the two could differ.
			if version < highest {
				return fmt.Errorf("the %s is at schema version %d and lacks migration %s, which comes before it: it was made by another build of writ; make a new database", m.What, highest, base)
			}
			text, err := fs.ReadFile(m.Files, file)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, strings.ReplaceAll(Dialect(string(text), d.pg), "{{now}}", now)); err != nil {
				return fmt.Errorf("migrate the %s to version %d: %w", m.What, version, err)
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)", version, base, m.Now().UnixMilli()); err != nil {
				return err
			}
		}
		return nil
	})
}

// migrationFile is a migration this engine runs: the file at path, which
// it records as name.
type migrationFile struct{ path, name string }

// engineFiles are the migrations of names that the engine runs, in order:
// every file of the shared subset, and of each pair of engine files the
// one of this engine, named without its engine.
func engineFiles(names []string, pg bool) ([]migrationFile, error) {
	own, other := ".sqlite.sql", ".postgres.sql"
	if pg {
		own, other = other, own
	}
	var out []migrationFile
	for _, file := range names {
		base := path.Base(file)
		name := base
		for _, suffix := range []string{own, other} {
			stem, ok := strings.CutSuffix(file, suffix)
			if !ok {
				continue
			}
			twin := own
			if suffix == own {
				twin = other
			}
			if !slices.Contains(names, stem+twin) {
				return nil, fmt.Errorf("migration %s has no twin %s", base, path.Base(stem+twin))
			}
			name = path.Base(stem) + ".sql"
		}
		if strings.HasSuffix(file, other) {
			continue
		}
		out = append(out, migrationFile{path: file, name: name})
	}
	return out, nil
}

// Version returns the newest migration the database records, 0 for none.
func (d *DB) Version(ctx context.Context) (int, error) {
	var v int
	err := d.QueryRowContext(ctx, "SELECT coalesce(max(version), 0) FROM schema_migrations").Scan(&v)
	return v, err
}

func mapKeys(m map[int]string) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
