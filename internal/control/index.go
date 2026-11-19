package control

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mr-karan/writ/internal/sqldb"
)

// The kinds of state backend a stack's index is read from.
const (
	// BackendHTTP is a state that writ serve keeps on the http backend it
	// provides. The state index of such a stack@env is numbered by the
	// tfstate_versions row its rows were read from.
	BackendHTTP = "http"
	// BackendS3 is a state in the stack's own S3 bucket. Its index is
	// numbered by the state's serial, within its lineage.
	BackendS3 = "s3"
)

// IndexRow is one resource of the state index: its address, type, and
// provider, and the attributes a drift check or a lookup matches on. An attribute
// the state does not hold as a string, or marks sensitive, is "".
type IndexRow struct {
	Address, Type, Provider string
	ID, ARN, Identifier     string
	Region                  string
	Namespace, ModifyIndex  string
}

// IndexWrite is the whole state index of one stack@env, read from one
// version of its state.
type IndexWrite struct {
	Env, Stack string
	// Backend is the kind of state backend, BackendHTTP or BackendS3.
	Backend string
	// Version is the version of the state the rows were read from: its
	// serial, for BackendS3.
	Version int64
	// Lineage is the lineage of a state on BackendS3, "" on BackendHTTP.
	Lineage string
	// Seq is, on BackendS3, the place PutSerial or ObserveSerial gave the
	// read of the serial the rows were read from: only the newest read
	// writes the index. 0 on BackendHTTP.
	Seq  int64
	At   time.Time
	Rows []IndexRow
}

// Indexed is what the state index holds for one stack@env.
type Indexed struct {
	Backend string
	Version int64
	Lineage string
	At      time.Time
	Rows    []IndexRow // sorted by address
	// Drift is the newest serial of a state on S3 that writ serve read and
	// no apply of writ serve wrote, and DriftAt when it read it. 0 when it
	// read none. A stack with drift and no index has Version 0.
	Drift   int64
	DriftAt time.Time
}

// StaleIndex is a stack@env whose newest state is newer than its index.
type StaleIndex struct {
	Env, Stack string
	Version    int64
}

const indexCols = "address, type, provider, id, arn, identifier, region, namespace, modify_index"

// ReplaceIndex replaces the rows of w's stack@env with w's rows, in one
// transaction. It stores nothing and reports false when w is older than
// what the index may hold: on BackendHTTP when the index holds a newer
// version, and on BackendS3 when w.Seq is not the newest read of the
// stack@env, of whatever lineage and serial. On S3 any later read, by a
// plan, an apply or a refresh, makes w stale, as when a refresh ends after
// a plan read a newer serial; the check holds the stack@env's row of
// state_s3_stacks until the write commits. A state of another backend
// replaces the index.
func (s *Store) ReplaceIndex(ctx context.Context, w IndexWrite) (bool, error) {
	if err := w.check(); err != nil {
		return false, err
	}
	var replaced bool
	err := s.db.Write(ctx, func(tx *sqldb.Tx) error {
		var err error
		replaced, err = replaceIndex(ctx, tx, w)
		return err
	})
	return replaced, err
}

// check refuses an IndexWrite whose backend, lineage, and read do not fit.
func (w IndexWrite) check() error {
	switch {
	case w.Backend != BackendHTTP && w.Backend != BackendS3:
		return fmt.Errorf("state backend %q has no state index", w.Backend)
	case (w.Backend == BackendS3) != (w.Lineage != ""):
		return fmt.Errorf("an index of state backend %s with lineage %q", w.Backend, w.Lineage)
	case (w.Backend == BackendS3) != (w.Seq > 0):
		return fmt.Errorf("an index of state backend %s with read %d", w.Backend, w.Seq)
	}
	return nil
}

// replaceIndex is ReplaceIndex within tx.
func replaceIndex(ctx context.Context, tx *sqldb.Tx, w IndexWrite) (bool, error) {
	if w.Backend == BackendS3 {
		var newest int64
		err := tx.QueryRowContext(ctx, "SELECT read_seq FROM state_s3_stacks WHERE env = ? AND stack = ?"+forUpdate(tx), w.Env, w.Stack).Scan(&newest)
		if errors.Is(err, sqldb.ErrNoRows) || err == nil && newest != w.Seq {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
	// The upsert holds the stack's row until the transaction ends, so
	// a second writer waits, then finds the version this one stored.
	res, err := tx.ExecContext(ctx, `INSERT INTO state_index_stacks (env, stack, backend, version, lineage, read_seq, indexed_at) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (env, stack) DO UPDATE SET backend = excluded.backend, version = excluded.version, lineage = excluded.lineage,
			read_seq = excluded.read_seq, indexed_at = excluded.indexed_at
		WHERE state_index_stacks.backend <> excluded.backend OR excluded.backend = ? OR state_index_stacks.version <= excluded.version`,
		w.Env, w.Stack, w.Backend, w.Version, w.Lineage, w.Seq, w.At.UnixMilli(), BackendS3)
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM state_index WHERE env = ? AND stack = ?", w.Env, w.Stack); err != nil {
		return false, err
	}
	for _, r := range w.Rows {
		if _, err := tx.ExecContext(ctx, `INSERT INTO state_index (env, stack, `+indexCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			w.Env, w.Stack, r.Address, r.Type, r.Provider, r.ID, r.ARN, r.Identifier, r.Region, r.Namespace, r.ModifyIndex); err != nil {
			return false, fmt.Errorf("index %s of %s@%s: %w", r.Address, w.Stack, w.Env, err)
		}
	}
	return true, nil
}

// ErrStaleRead is the error of a look at the index of a stack@env on S3
// for a read that a later read of its state follows.
var ErrStaleRead = errors.New("a later read of the state follows this one")

// S3IndexHolds reports whether the index of a stack@env on S3 holds serial
// of lineage, for the read that PutSerial or ObserveSerial numbered seq.
// It returns ErrStaleRead when seq is not the newest read of the
// stack@env, as ReplaceIndex refuses an index of it.
func (s *Store) S3IndexHolds(ctx context.Context, env, stack, lineage string, serial, seq int64) (bool, error) {
	var newest, version int64
	var backend, have string
	err := s.db.QueryRowContext(ctx, `SELECT c.read_seq, COALESCE(i.backend, ''), COALESCE(i.lineage, ''), COALESCE(i.version, 0)
		FROM state_s3_stacks c LEFT JOIN state_index_stacks i ON i.env = c.env AND i.stack = c.stack
		WHERE c.env = ? AND c.stack = ?`, env, stack).Scan(&newest, &backend, &have, &version)
	switch {
	case errors.Is(err, sqldb.ErrNoRows) || err == nil && newest != seq:
		return false, ErrStaleRead
	case err != nil:
		return false, err
	}
	return backend == BackendS3 && have == lineage && version == serial, nil
}

// Index returns the state index of one stack@env, or ErrNotFound when
// nothing indexed it yet.
func (s *Store) Index(ctx context.Context, env, stack string) (Indexed, error) {
	var ix Indexed
	var at, driftAt int64
	err := s.db.QueryRowContext(ctx, "SELECT backend, version, lineage, indexed_at, outside_serial, outside_at FROM state_index_stacks WHERE env = ? AND stack = ?", env, stack).
		Scan(&ix.Backend, &ix.Version, &ix.Lineage, &at, &ix.Drift, &driftAt)
	if errors.Is(err, sqldb.ErrNoRows) {
		return Indexed{}, ErrNotFound
	}
	if err != nil {
		return Indexed{}, err
	}
	ix.At = fromMS(at)
	if ix.Drift > 0 {
		ix.DriftAt = fromMS(driftAt)
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+indexCols+" FROM state_index WHERE env = ? AND stack = ? ORDER BY address", env, stack)
	if err != nil {
		return Indexed{}, err
	}
	defer rows.Close()
	ix.Rows = []IndexRow{}
	for rows.Next() {
		var r IndexRow
		if err := rows.Scan(&r.Address, &r.Type, &r.Provider, &r.ID, &r.ARN, &r.Identifier, &r.Region, &r.Namespace, &r.ModifyIndex); err != nil {
			return Indexed{}, err
		}
		ix.Rows = append(ix.Rows, r)
	}
	return ix, rows.Err()
}

// StaleIndexes returns up to limit stack@envs on the http backend whose
// newest state version is newer than the version their index was read
// from, or that have no index: states written before writ serve kept an
// index, by an apply that failed, or by one whose index was not stored.
// Those whose newest version has index secrets come first, so stacks a
// refresh cannot index take no turn from those it can.
func (s *Store) StaleIndexes(ctx context.Context, limit int) ([]StaleIndex, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT v.env, v.stack, v.version FROM tfstate_versions v
		WHERE v.version = (SELECT max(w.version) FROM tfstate_versions w WHERE w.env = v.env AND w.stack = v.stack)
		AND v.version > COALESCE((SELECT i.version FROM state_index_stacks i WHERE i.env = v.env AND i.stack = v.stack AND i.backend = ?), 0)
		ORDER BY v.index_secrets IS NULL, v.env, v.stack LIMIT ?`, BackendHTTP, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StaleIndex
	for rows.Next() {
		var st StaleIndex
		if err := rows.Scan(&st.Env, &st.Stack, &st.Version); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// PutIndexSecrets stores, with each kept state version of stack@env that
// run runID wrote, what seal returns for that version: the sealed secrets
// of the run's mask, which a refresh of the index from that version
// needs. It returns how many versions it sealed them for.
func (s *Store) PutIndexSecrets(ctx context.Context, env, stack, runID string, seal func(version int64) []byte) (int, error) {
	n := 0
	err := s.db.Write(ctx, func(tx *sqldb.Tx) error {
		rows, err := tx.QueryContext(ctx, "SELECT version FROM tfstate_versions WHERE env = ? AND stack = ? AND run_id = ?", env, stack, runID)
		if err != nil {
			return err
		}
		var versions []int64
		for rows.Next() {
			var v int64
			if err := rows.Scan(&v); err != nil {
				rows.Close()
				return err
			}
			versions = append(versions, v)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, v := range versions {
			if _, err := tx.ExecContext(ctx, "UPDATE tfstate_versions SET index_secrets = ? WHERE env = ? AND stack = ? AND version = ?", seal(v), env, stack, v); err != nil {
				return err
			}
		}
		n = len(versions)
		return nil
	})
	return n, err
}

// IndexSecrets returns the sealed index secrets of one state version of a
// stack@env, or ErrNotFound when the version is not kept or has none.
func (s *Store) IndexSecrets(ctx context.Context, env, stack string, version int64) ([]byte, error) {
	var sealed []byte
	err := s.db.QueryRowContext(ctx, "SELECT index_secrets FROM tfstate_versions WHERE env = ? AND stack = ? AND version = ?", env, stack, version).Scan(&sealed)
	if errors.Is(err, sqldb.ErrNoRows) || err == nil && sealed == nil {
		return nil, ErrNotFound
	}
	return sealed, err
}
