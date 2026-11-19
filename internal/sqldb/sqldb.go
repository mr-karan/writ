// Package sqldb owns Writ's SQL drivers: it opens SQLite files and Postgres
// databases behind one type, rewrites the ? placeholders that every
// statement is written with into Postgres's $n, and turns driver errors into
// an *IOError that holds no connection detail. Both state and the control
// store run their statements through it, so each is written once for both
// engines.
//
// It is the only package that imports database/sql, pgx, or the SQLite
// driver. Callers see the aliases below for the few database/sql types a
// query needs.
package sqldb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite" // the "sqlite" driver of Open
)

// The database/sql types callers need, so that no other package imports it.
type (
	Rows       = sql.Rows
	Result     = sql.Result
	TxOptions  = sql.TxOptions
	NullInt64  = sql.NullInt64
	NullString = sql.NullString
)

// ErrNoRows is the answer of a QueryRow that matched nothing.
var ErrNoRows = sql.ErrNoRows

// IsPostgres reports whether a location names a Postgres database, not a
// SQLite file.
func IsPostgres(location string) bool {
	return strings.HasPrefix(location, "postgres://") || strings.HasPrefix(location, "postgresql://")
}

// SQLiteMode is how a SQLite file is opened.
type SQLiteMode int

const (
	// Durable writes through WAL with a full fsync on every commit.
	Durable SQLiteMode = iota
	// ReadOnly cannot change the file, and sets no journal mode, so a read
	// never changes it either.
	ReadOnly
	// Scratch is a working copy whose durable copy lives elsewhere, so it
	// skips fsync.
	Scratch
)

// DB is an open pool on one engine. Its statements are written with ?
// placeholders.
type DB struct {
	db *sql.DB
	pg bool
	// what names the database in errors, such as "postgres state".
	what string
}

// OpenSQLite opens the SQLite file at path. Every connection gets the same
// pragmas, foreign keys are on, and every write transaction is BEGIN
// IMMEDIATE, so a read-then-write never fails on a lock upgrade.
// busy_timeout is how long a write waits for another connection.
//
// A writable open makes the files private to their owner: it creates the
// missing directories with mode 0700 and the file with mode 0600, and
// tightens a file, a -wal, or a -shm that an older writ left with group or
// other bits. SQLite gives a -wal and a -shm the mode of the database file,
// so creating that file first covers them without a umask change. A
// read-only open uses SQLite's normal mode=ro path without changing permissions.
func OpenSQLite(path string, mode SQLiteMode, what string) (*DB, error) {
	dsn, err := sqliteSource(path, mode)
	if err != nil {
		return nil, err
	}
	if mode != ReadOnly {
		if err := makePrivate(path); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	return pooled(db, false, what, 8), nil
}

// makePrivate creates the database file at path if it is missing, with mode
// 0600 in a directory of mode 0700, and removes the group and other bits of
// the file. Owned -wal and -shm files also regain owner read/write access,
// including sidecars left read-only by an older or killed reader.
func makePrivate(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		info, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("sqlite state path is not a regular file: %s", p)
		}
		if p != path {
			if err := makeOwnedSidecarPrivate(p, info); err != nil {
				return err
			}
			continue
		}
		if m := info.Mode().Perm(); m&0o077 != 0 {
			if err := os.Chmod(p, m&^0o077); err != nil {
				return err
			}
		}
	}
	return nil
}

// sqliteSource builds a file: URI, so a path holding ? or # stays a path.
func sqliteSource(path string, mode SQLiteMode) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	q := url.Values{}
	switch mode {
	case ReadOnly:
		q.Set("mode", "ro")
		q.Add("_pragma", "busy_timeout(5000)")
	case Durable, Scratch:
		sync := "FULL"
		if mode == Scratch {
			sync = "OFF"
		}
		q.Add("_pragma", "busy_timeout(5000)")
		q.Add("_pragma", "journal_mode(WAL)")
		q.Add("_pragma", "synchronous("+sync+")")
		q.Add("_pragma", "foreign_keys(1)")
		q.Set("_txlock", "immediate")
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs), RawQuery: q.Encode()}
	return u.String(), nil
}

// PostgresOptions configures OpenPostgres.
type PostgresOptions struct {
	// ConnectTimeout bounds connection setup. Zero uses five seconds.
	ConnectTimeout time.Duration
	// SearchPath is the schema every unqualified table name resolves in,
	// "" for the role's default.
	SearchPath string
	// ReadOnly starts every transaction read-only, whatever the role may do.
	ReadOnly bool
	// AppName is the connection's application_name, "" for none.
	AppName string
	// MaxConns bounds the pool; 0 means 8.
	MaxConns int
	What     string
}

// ErrBadURL is the error of a Postgres URL that does not parse.
var ErrBadURL = errors.New("invalid postgres URL")

// OpenPostgres opens a pool on the Postgres database at rawURL, with the
// timeouts every Writ connection runs under.
func OpenPostgres(rawURL string, o PostgresOptions) (*DB, error) {
	config, err := pgx.ParseConfig(rawURL)
	if err != nil {
		return nil, ErrBadURL
	}
	if o.ConnectTimeout < 0 {
		return nil, errors.New("postgres connect timeout must be positive")
	}
	config.ConnectTimeout = o.ConnectTimeout
	if config.ConnectTimeout == 0 {
		config.ConnectTimeout = DefaultTimeout(5 * time.Second)
	}
	config.RuntimeParams["statement_timeout"] = strconv.FormatInt(DefaultTimeout(10*time.Second).Milliseconds(), 10)
	config.RuntimeParams["lock_timeout"] = strconv.FormatInt(DefaultTimeout(5*time.Second).Milliseconds(), 10)
	config.RuntimeParams["idle_in_transaction_session_timeout"] = strconv.FormatInt(DefaultTimeout(15*time.Second).Milliseconds(), 10)
	if o.SearchPath != "" {
		config.RuntimeParams["search_path"] = QuoteIdent(o.SearchPath)
	}
	if o.ReadOnly {
		config.RuntimeParams["default_transaction_read_only"] = "on"
	}
	if o.AppName != "" {
		config.RuntimeParams["application_name"] = o.AppName
	}
	n := o.MaxConns
	if n == 0 {
		n = 8
	}
	return pooled(stdlib.OpenDB(*config), true, o.What, n), nil
}

func pooled(db *sql.DB, pg bool, what string, conns int) *DB {
	db.SetMaxOpenConns(conns)
	db.SetMaxIdleConns(max(conns/2, 1))
	db.SetConnMaxLifetime(30 * time.Minute)
	return &DB{db: db, pg: pg, what: what}
}

// QuoteIdent quotes a Postgres identifier.
func QuoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// Close closes the pool.
func (d *DB) Close() error { return d.db.Close() }

// Ping checks the database answers.
func (d *DB) Ping(ctx context.Context) error { return d.Error(d.db.PingContext(ctx)) }

// Error turns a driver error of this pool into an *IOError.
func (d *DB) Error(err error) error { return Classify(d.pg, d.what, err) }

// Rebind rewrites a statement written for SQLite into the pool's dialect:
// on Postgres, each ? becomes $n. Text inside a '...' literal, a "..." identifier,
// a -- comment, or a /* */ comment stays as it is. A doubled quote inside a
// literal or an identifier needs no case of its own: it closes the text and
// opens it again. The statements are SQLite's, so Postgres's E'...' and
// $tag$ quoting do not occur.
func Rebind(query string, pg bool) string {
	if !pg {
		return query
	}
	var b strings.Builder
	n := 0
	for i := 0; i < len(query); i++ {
		c := query[i]
		open, end := 1, ""
		switch {
		case c == '\'' || c == '"':
			end = string(c)
		case strings.HasPrefix(query[i:], "--"):
			open, end = 2, "\n"
		case strings.HasPrefix(query[i:], "/*"):
			open, end = 2, "*/"
		case c == '?':
			n++
			fmt.Fprintf(&b, "$%d", n)
			continue
		default:
			b.WriteByte(c)
			continue
		}
		j := strings.Index(query[i+open:], end)
		if j < 0 {
			b.WriteString(query[i:])
			break
		}
		stop := i + open + j + len(end)
		b.WriteString(query[i:stop])
		i = stop - 1
	}
	return b.String()
}

// ExecContext runs a statement.
func (d *DB) ExecContext(ctx context.Context, q string, args ...any) (Result, error) {
	r, err := d.db.ExecContext(ctx, Rebind(q, d.pg), args...)
	return r, d.Error(err)
}

// QueryContext runs a query.
func (d *DB) QueryContext(ctx context.Context, q string, args ...any) (*Rows, error) {
	r, err := d.db.QueryContext(ctx, Rebind(q, d.pg), args...)
	return r, d.Error(err)
}

// QueryRowContext runs a query that returns at most one row.
func (d *DB) QueryRowContext(ctx context.Context, q string, args ...any) Row {
	return Row{row: d.db.QueryRowContext(ctx, Rebind(q, d.pg), args...), db: d}
}

// Row is the result of QueryRowContext.
type Row struct {
	row *sql.Row
	db  *DB
}

// Scan copies the row's columns into dest. A query that matched nothing
// returns ErrNoRows.
func (r Row) Scan(dest ...any) error { return r.db.Error(r.row.Scan(dest...)) }

// Tx is a transaction. Its statements run under the context it began
// with, so a caller that hands the transaction to others keeps one deadline
// for all of them.
type Tx struct {
	tx  *sql.Tx
	ctx context.Context
	db  *DB
}

// BeginTx starts a transaction. On SQLite a pool opened Durable or Scratch
// begins it IMMEDIATE, which takes the write lock at once.
func (d *DB) BeginTx(ctx context.Context, opts *TxOptions) (*Tx, error) {
	tx, err := d.db.BeginTx(ctx, opts)
	if err != nil {
		return nil, d.Error(err)
	}
	return &Tx{tx: tx, ctx: ctx, db: d}, nil
}

// Postgres reports whether the transaction is on Postgres.
func (t *Tx) Postgres() bool { return t.db.pg }

// ExecContext runs a statement in the transaction.
func (t *Tx) ExecContext(_ context.Context, q string, args ...any) (Result, error) {
	r, err := t.tx.ExecContext(t.ctx, Rebind(q, t.db.pg), args...)
	return r, t.db.Error(err)
}

// QueryContext runs a query in the transaction.
func (t *Tx) QueryContext(_ context.Context, q string, args ...any) (*Rows, error) {
	r, err := t.tx.QueryContext(t.ctx, Rebind(q, t.db.pg), args...)
	return r, t.db.Error(err)
}

// QueryRowContext runs a query that returns at most one row.
func (t *Tx) QueryRowContext(_ context.Context, q string, args ...any) Row {
	return Row{row: t.tx.QueryRowContext(t.ctx, Rebind(q, t.db.pg), args...), db: t.db}
}

// Commit commits the transaction.
func (t *Tx) Commit() error { return t.db.Error(t.tx.Commit()) }

// Rollback ends the transaction without its changes. It is safe after
// Commit.
func (t *Tx) Rollback() error { return t.tx.Rollback() }

// Write runs fn in a transaction and commits it when fn returns nil. An
// error of the commit itself is a *CommitError.
func (d *DB) Write(ctx context.Context, fn func(tx *Tx) error) error {
	return d.WriteCommit(ctx, fn, nil)
}

// CommitStep wraps the commit of WriteCommit. It calls commit at most
// once, and returns its error or one of its own. A step that returns
// without a call to commit rolls the transaction back.
type CommitStep func(commit func() error) error

// WriteCommit is Write with step around the commit, nil for none. Tests
// use a step to hold a commit, or to fail one after it is durable.
func (d *DB) WriteCommit(ctx context.Context, fn func(tx *Tx) error, step CommitStep) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	if step == nil {
		err = tx.Commit()
	} else {
		err = step(tx.Commit)
	}
	if err != nil {
		return &CommitError{Err: err}
	}
	return nil
}

// CommitError is a commit that returned an error. The caller cannot know
// whether the transaction is durable: an engine can complete a commit
// after its caller stops the wait, or lose the answer of one it made.
type CommitError struct{ Err error }

func (e *CommitError) Error() string { return "commit: " + e.Err.Error() }

// Unwrap returns the commit's own error.
func (e *CommitError) Unwrap() error { return e.Err }

// IOError reports that the database itself failed: it was busy, timed out,
// lost its connection, or could not be read or written. It says nothing
// about the caller's own rules.
type IOError struct {
	Err error
}

func (e *IOError) Error() string { return e.Err.Error() }

// Unwrap returns the driver's error, or for Postgres its safe summary.
func (e *IOError) Unwrap() error { return e.Err }

// Classify turns a driver error into an *IOError. ErrNoRows is an answer,
// not a failure, and passes through. Postgres driver errors can contain
// connection credentials or server-supplied text, so only a fixed summary
// and the SQLSTATE reach the error. what names the database, such as "control database".
func Classify(pg bool, what string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, sql.ErrNoRows):
		return sql.ErrNoRows
	}
	if _, ok := errors.AsType[*IOError](err); ok {
		return err
	}
	if !pg {
		return &IOError{Err: err}
	}
	for _, safe := range []error{context.Canceled, context.DeadlineExceeded, sql.ErrTxDone} {
		if errors.Is(err, safe) {
			return &IOError{Err: safe}
		}
	}
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
		cause := "operation failed"
		switch pgErr.Code {
		case "42501":
			cause = "permission denied"
		case "25006":
			cause = "read-only transaction"
		}
		return &IOError{Err: fmt.Errorf("%s %s (SQLSTATE %s)", what, cause, pgErr.Code)}
	}
	return &IOError{Err: fmt.Errorf("%s operation failed", what)}
}
