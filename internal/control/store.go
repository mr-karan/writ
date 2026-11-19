// Package control is writ serve's own store: the configs it ran with, their
// teams and grants, the kept plans, approvals, runs, overrides, sessions,
// used token ids, an audit row for every API call and decision, and the
// state OpenTofu stacks keep on writ serve's http backend.
//
// One schema and one code path run on both engines, through internal/sqldb:
// Postgres for a team, where the tables live in the schema writ_control,
// and SQLite under writ serve --dev, where they live in control.db.
// Callers see the types below, never rows.
package control

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/mr-karan/writ/internal/sqldb"
)

// Schema is the Postgres schema of the control plane's tables.
const Schema = "writ_control"

// migrationLock serializes migrations of one Postgres database across
// processes.
const migrationLock = 735300

// what names the control database in errors.
const what = "control database"

//go:embed migrations/*.sql
var migrations embed.FS

// Store is an open control database.
type Store struct {
	db *sqldb.DB
}

// ErrNotFound means no row has the key asked for.
var ErrNotFound = errors.New("not found")

// ErrReplay means the token id already made a write of this kind.
var ErrReplay = errors.New("this token was already used for this operation")

// Options configures Open.
type Options struct {
	Location string
	// ConnectTimeout bounds a Postgres connection. Zero uses five seconds.
	ConnectTimeout time.Duration
}

// Open opens the control database at location and applies every migration
// it has not applied yet: a postgres:// URL, or the path of a SQLite file.
// A database migrated by a newer writ is refused.
func Open(ctx context.Context, opts Options) (*Store, error) {
	location := opts.Location
	s := &Store{}
	var err error
	if sqldb.IsPostgres(location) {
		if err := CheckURL(location); err != nil {
			return nil, err
		}
		s.db, err = sqldb.OpenPostgres(location, sqldb.PostgresOptions{SearchPath: Schema, AppName: "writ serve", MaxConns: 16, What: what, ConnectTimeout: opts.ConnectTimeout})
		if errors.Is(err, sqldb.ErrBadURL) {
			return nil, errors.New("invalid control database URL")
		}
	} else {
		dir := filepath.Dir(location)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("open control database: %w", err)
		}
		s.db, err = sqldb.OpenSQLite(location, sqldb.Durable, what)
	}
	if err != nil {
		return nil, fmt.Errorf("open control database: %w", err)
	}
	if err := s.db.Migrate(ctx, sqldb.Migrations{Files: migrations, Dir: "migrations", Schema: Schema, Lock: migrationLock, What: what, Now: time.Now}); err != nil {
		s.db.Close()
		return nil, err
	}
	return s, nil
}

// CheckURL refuses a database URL that is not postgres:// or that holds a
// password. A password belongs in a passfile, named by the URL's passfile
// parameter or PGPASSFILE, because the URL is in the config and the config
// holds no secret.
func CheckURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return errors.New("the database URL must be postgres://")
	}
	if _, has := u.User.Password(); has || u.Query().Has("password") {
		return errors.New("the database URL holds a password; put it in a passfile and name it with ?passfile=")
	}
	return nil
}

func (s *Store) Close() { s.db.Close() }

// Ping checks the database answers.
func (s *Store) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return s.db.Ping(ctx)
}

// fromMS reads a time the store keeps as Unix milliseconds.
func fromMS(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

// Environments lists environments with a state, indexed or not, or plans,
// including plans that have not reached state preparation yet.
func (s *Store) Environments(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT env FROM ("+driftTargets+") t UNION SELECT env FROM plans ORDER BY env")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var env string
		if err := rows.Scan(&env); err != nil {
			return nil, s.db.Error(err)
		}
		out = append(out, env)
	}
	return out, s.db.Error(rows.Err())
}
