package control

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mr-karan/writ/internal/sqldb"
)

// NewestState is the newest state of a stack@env that writ serve knows:
// the newest version on the http backend, or the serial of the newest
// read on S3.
type NewestState struct {
	// Version is the state version on the http backend, 0 on S3.
	Version int64
	Serial  int64
	Lineage string
	// HasSet reports a secret set sealed with it.
	HasSet bool
	// Applying is the id of a run that applies to the stack@env, "" when
	// none does.
	Applying string
}

// StateAdopt seals a secret set with the newest state of a stack@env, which
// no apply of writ serve sealed one with.
type StateAdopt struct {
	Env, Stack, Backend string
	// Version names the state version on the http backend. On S3,
	// Lineage and Serial name the serial.
	Version int64
	Lineage string
	Serial  int64
	// Sealed is the secret set, sealed for Version on the http backend
	// and for Serial on S3.
	Sealed []byte
	// Rows are the state index the adoption read from the state, and
	// Seq, on S3, the place of that read, as IndexWrite.Seq.
	Rows []IndexRow
	Seq  int64
	At   time.Time
	// Audit is the adoption's audit row, written with the set.
	Audit Entry
}

var (
	// ErrNotNewest means the adopted state is not the newest that writ
	// serve knows of its stack@env.
	ErrNotNewest = errors.New("the state is not the newest writ serve knows")
	// ErrHasSet means a secret set is sealed with the state already.
	ErrHasSet = errors.New("a secret set is sealed with the state already")
	// ErrApplyRuns means an apply of the stack@env runs, which may write
	// a newer state or seal a set with the one it wrote.
	ErrApplyRuns = errors.New("an apply of the stack runs")
)

// NewestStateOf returns the newest state of a stack@env on backend and
// the apply that runs, or ErrNotFound when writ serve keeps no version of
// it, or has read no serial of it on S3.
func (s *Store) NewestStateOf(ctx context.Context, env, stack, backend string) (NewestState, error) {
	return newestState(ctx, s.db, env, stack, backend, "")
}

func newestState(ctx context.Context, q rowQuerier, env, stack, backend, lock string) (NewestState, error) {
	var n NewestState
	var err error
	switch backend {
	case BackendHTTP:
		err = q.QueryRowContext(ctx, `SELECT version, serial, lineage, index_secrets IS NOT NULL FROM tfstate_versions
			WHERE env = ? AND stack = ? ORDER BY version DESC LIMIT 1`+lock, env, stack).Scan(&n.Version, &n.Serial, &n.Lineage, &n.HasSet)
	case BackendS3:
		err = q.QueryRowContext(ctx, `SELECT serial, lineage, index_secrets IS NOT NULL FROM state_serials
			WHERE env = ? AND stack = ? ORDER BY read_seq DESC LIMIT 1`+lock, env, stack).Scan(&n.Serial, &n.Lineage, &n.HasSet)
	default:
		return NewestState{}, fmt.Errorf("state backend %q keeps no state writ serve reads", backend)
	}
	if errors.Is(err, sqldb.ErrNoRows) {
		return NewestState{}, ErrNotFound
	}
	if err != nil {
		return NewestState{}, err
	}
	err = q.QueryRowContext(ctx, "SELECT id FROM runs WHERE env = ? AND stack = ? AND ended_at IS NULL LIMIT 1", env, stack).Scan(&n.Applying)
	if err != nil && !errors.Is(err, sqldb.ErrNoRows) {
		return NewestState{}, err
	}
	return n, nil
}

// takeTurn locks the stack@env's row of stack_turns until the transaction
// ends. BeginApply and AdoptState take it, so an adoption that holds it
// sees each apply that began before it, and an apply that starts after
// waits for the adoption to commit.
func takeTurn(ctx context.Context, tx *sqldb.Tx, env, stack string) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO stack_turns (env, stack) VALUES (?, ?) ON CONFLICT (env, stack) DO UPDATE SET env = excluded.env", env, stack)
	return err
}

// AdoptState stores a.Sealed with the state a names, replaces the state
// index with a.Rows, and writes a.Audit, in one transaction, when that
// state is still the newest of its stack@env, has no secret set, and no
// apply of the stack@env runs. It marks the stack@env adopted at a.At,
// which makes its drift check due. It reports whether the index took the
// rows: as ReplaceIndex, it keeps an index of a newer read.
func (s *Store) AdoptState(ctx context.Context, a StateAdopt) (bool, error) {
	if len(a.Sealed) == 0 {
		return false, errors.New("an adoption needs a sealed secret set")
	}
	w := IndexWrite{Env: a.Env, Stack: a.Stack, Backend: a.Backend, Version: a.Version, At: a.At, Rows: a.Rows}
	if a.Backend == BackendS3 {
		w.Version, w.Lineage, w.Seq = a.Serial, a.Lineage, a.Seq
	}
	if err := w.check(); err != nil {
		return false, err
	}
	var indexed bool
	err := s.db.Write(ctx, func(tx *sqldb.Tx) error {
		if err := takeTurn(ctx, tx, a.Env, a.Stack); err != nil {
			return err
		}
		if a.Backend == BackendS3 {
			// Each read of the stack@env takes its place under this row's
			// lock, so the newest read cannot change before the commit.
			var seq int64
			err := tx.QueryRowContext(ctx, "SELECT read_seq FROM state_s3_stacks WHERE env = ? AND stack = ?"+forUpdate(tx), a.Env, a.Stack).Scan(&seq)
			if errors.Is(err, sqldb.ErrNoRows) {
				return ErrNotNewest
			}
			if err != nil {
				return err
			}
		}
		n, err := newestState(ctx, tx, a.Env, a.Stack, a.Backend, forUpdate(tx))
		switch {
		case errors.Is(err, ErrNotFound):
			return ErrNotNewest
		case err != nil:
			return err
		case a.Backend == BackendHTTP && n.Version != a.Version, a.Backend == BackendS3 && (n.Serial != a.Serial || n.Lineage != a.Lineage):
			return ErrNotNewest
		case n.HasSet:
			return ErrHasSet
		case n.Applying != "":
			return ErrApplyRuns
		}
		var res sqldb.Result
		if a.Backend == BackendHTTP {
			res, err = tx.ExecContext(ctx, "UPDATE tfstate_versions SET index_secrets = ? WHERE env = ? AND stack = ? AND version = ? AND index_secrets IS NULL",
				a.Sealed, a.Env, a.Stack, a.Version)
		} else {
			res, err = tx.ExecContext(ctx, "UPDATE state_serials SET index_secrets = ? WHERE env = ? AND stack = ? AND lineage = ? AND serial = ? AND index_secrets IS NULL",
				a.Sealed, a.Env, a.Stack, a.Lineage, a.Serial)
		}
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return errors.Join(err, ErrHasSet)
		}
		if indexed, err = replaceIndex(ctx, tx, w); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO state_index_stacks (env, stack, backend, version, lineage, indexed_at, adopted_at)
			VALUES (?, ?, ?, 0, '', 0, ?) ON CONFLICT (env, stack) DO UPDATE SET adopted_at = excluded.adopted_at`,
			a.Env, a.Stack, a.Backend, a.At.UnixMilli()); err != nil {
			return err
		}
		return insertAudit(ctx, tx, a.Audit)
	})
	return indexed, err
}
