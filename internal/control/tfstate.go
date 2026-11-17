package control

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mr-karan/writ/internal/sqldb"
)

// StateScope is what a run's state token may do.
type StateScope string

const (
	// StatePlan reads the state of its stack.
	StatePlan StateScope = "plan"
	// StateApply reads, locks, and writes the state of its stack.
	StateApply StateScope = "apply"
)

// StateToken is a run's token for the http state backend. The table keeps
// only the SHA-256 of the token itself.
type StateToken struct {
	RunID, Env, Stack string
	Scope             StateScope
	ExpiresAt         time.Time
}

// StateVersion is one state a run wrote. Sealed is the state's bytes
// sealed under the environment's key, bound to Version; the store never
// opens it.
type StateVersion struct {
	Version int64
	Serial  int64
	Lineage string
	SHA256  string
	RunID   string
	At      time.Time
	Sealed  []byte
}

// StateAuth is the token a request came with. Every call that locks or
// writes checks it again inside its own transaction, so a request that
// was authorized before its run ended cannot act after EndStateRun.
type StateAuth struct {
	Hash  []byte
	RunID string
	Now   time.Time
}

// StateLock is the lock of one stack@env. Info is the lock's JSON as
// OpenTofu sent it.
type StateLock struct {
	ID, RunID, Info string
	At              time.Time
}

// StatePut is a write of a new state version.
type StatePut struct {
	Env, Stack string
	Auth       StateAuth
	// LockID is the id of the lock the writing run holds.
	LockID string
	// Version is the state to store. Its Version and Sealed are ignored:
	// PutState numbers the version and calls Seal with that number, so the
	// sealed bytes are bound to the row that holds them.
	Version StateVersion
	Seal    func(version int64) []byte
	// Keep is how many versions of the stack@env to keep, the new one
	// included.
	Keep int
	// Audit is the audit row of the write. PutState sets its Reason to
	// the version it stored, or the version a repeat matched, and commits
	// it with the write.
	Audit Entry
}

var (
	// ErrStateLocked means another run, or another lock of the same run,
	// holds the stack's lock.
	ErrStateLocked = errors.New("the state is locked")
	// ErrStateNotLocked means a write came without the lock it needs.
	ErrStateNotLocked = errors.New("the write does not hold the state's lock")
	// ErrStateRunEnded means the request's token expired, or its run
	// ended, after the request was authorized.
	ErrStateRunEnded = errors.New("the state token's run ended")
	// ErrStateStale means a write's lineage is not the stored state's, or
	// its serial is not above the stored state's.
	ErrStateStale = errors.New("the write is older than the stored state")
)

// AddStateToken stores a run's token under its hash, and deletes every
// token that expired, so the table holds only live tokens.
func (s *Store) AddStateToken(ctx context.Context, hash []byte, t StateToken, now time.Time) error {
	if t.Scope != StatePlan && t.Scope != StateApply {
		return fmt.Errorf("unknown state token scope %q", t.Scope)
	}
	return s.db.Write(ctx, func(tx *sqldb.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM tfstate_tokens WHERE expires_at <= ?", now.UnixMilli()); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO tfstate_tokens (hash, run_id, env, stack, scope, created_at, expires_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, hash, t.RunID, t.Env, t.Stack, string(t.Scope), now.UnixMilli(), t.ExpiresAt.UnixMilli())
		return err
	})
}

// StateTokenByHash returns the live token whose hash is hash. A token that
// expired, or whose run ended, is ErrNotFound.
func (s *Store) StateTokenByHash(ctx context.Context, hash []byte, now time.Time) (StateToken, error) {
	var t StateToken
	var expires int64
	err := s.db.QueryRowContext(ctx, `SELECT run_id, env, stack, scope, expires_at FROM tfstate_tokens WHERE hash = ? AND expires_at > ?`,
		hash, now.UnixMilli()).Scan(&t.RunID, &t.Env, &t.Stack, &t.Scope, &expires)
	if errors.Is(err, sqldb.ErrNoRows) {
		return StateToken{}, ErrNotFound
	}
	if err != nil {
		return StateToken{}, err
	}
	t.ExpiresAt = fromMS(expires)
	return t, nil
}

// EndStateRun deletes the tokens of a run and releases the locks it holds.
func (s *Store) EndStateRun(ctx context.Context, runID string) error {
	return s.db.Write(ctx, func(tx *sqldb.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM tfstate_tokens WHERE run_id = ?", runID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "DELETE FROM tfstate_locks WHERE run_id = ?", runID)
		return err
	})
}

// orphanRuns selects the state tokens and locks of runs that no process
// runs: neither a run applying, a plan planning, nor a drift check planning.
const orphanRuns = "run_id NOT IN (SELECT id FROM runs WHERE status = 'applying') AND run_id NOT IN (SELECT ref FROM plans WHERE status = 'planning') AND run_id NOT IN (SELECT ref FROM drift_checks WHERE status = 'planning')"

// EndOrphanState deletes the state tokens, and releases the locks, of
// runs that no process runs, and returns each stack@env it unlocked. A
// crash skips the end of each run's state access, as can a failed write
// of that end, and a lock never expires, so a process calls it at its
// start, after it failed the runs and plans the crash left, and then at
// intervals. It is idempotent.
func (s *Store) EndOrphanState(ctx context.Context, now time.Time) ([]string, error) {
	var unlocked []string
	err := s.db.Write(ctx, func(tx *sqldb.Tx) error {
		var err error
		unlocked, err = forceEndState(ctx, tx, orphanRuns, nil, now, "no process runs it, as after a crash")
		return err
	})
	return unlocked, err
}

// forceEndState deletes the state tokens and releases the locks whose
// run_id matches where, for runs that are over, and writes an
// audit row of kind state-unlock for each lock, which says why. It
// returns each stack@env it unlocked. The tokens go first: on Postgres a
// LockState in flight holds its token's row, so the delete waits for it
// and the read of the locks sees its lock, and a LockState after the
// delete finds no token.
func forceEndState(ctx context.Context, tx *sqldb.Tx, where string, args []any, now time.Time, why string) ([]string, error) {
	if _, err := tx.ExecContext(ctx, "DELETE FROM tfstate_tokens WHERE "+where, args...); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT env, stack, lock_id, run_id FROM tfstate_locks WHERE "+where+" ORDER BY env, stack", args...)
	if err != nil {
		return nil, err
	}
	var locks []struct{ env, stack, id, run string }
	for rows.Next() {
		var l struct{ env, stack, id, run string }
		if err := rows.Scan(&l.env, &l.stack, &l.id, &l.run); err != nil {
			rows.Close()
			return nil, err
		}
		locks = append(locks, l)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	var unlocked []string
	for _, l := range locks {
		e := Entry{At: now, Request: l.run, Kind: "state-unlock", Op: "state-unlock", Env: l.env, Stacks: []string{l.stack}, Run: l.run,
			Decision: NoDecision, Reason: fmt.Sprintf("writ serve released lock %s of run %s on %s@%s: %s", l.id, l.run, l.stack, l.env, why)}
		if err := insertAudit(ctx, tx, e); err != nil {
			return nil, err
		}
		unlocked = append(unlocked, l.stack+"@"+l.env)
	}
	_, err = tx.ExecContext(ctx, "DELETE FROM tfstate_locks WHERE "+where, args...)
	return unlocked, err
}

// liveToken checks, inside tx, that a's token is still live for its run.
// On Postgres it holds the token's row until tx ends, so every end of a
// run's state access, which deletes the token first, waits for tx and
// then sees its rows.
func liveToken(ctx context.Context, tx *sqldb.Tx, a StateAuth) error {
	var run string
	err := tx.QueryRowContext(ctx, "SELECT run_id FROM tfstate_tokens WHERE hash = ? AND expires_at > ?"+forUpdate(tx),
		a.Hash, a.Now.UnixMilli()).Scan(&run)
	if errors.Is(err, sqldb.ErrNoRows) || err == nil && run != a.RunID {
		return ErrStateRunEnded
	}
	return err
}

// LatestState returns the newest state version of a stack@env, or
// ErrNotFound when no run wrote one.
func (s *Store) LatestState(ctx context.Context, env, stack string) (StateVersion, error) {
	v, err := scanVersion(s.db.QueryRowContext(ctx, `SELECT version, serial, lineage, sha256, run_id, created_at, sealed
		FROM tfstate_versions WHERE env = ? AND stack = ? ORDER BY version DESC LIMIT 1`, env, stack))
	if errors.Is(err, sqldb.ErrNoRows) {
		return StateVersion{}, ErrNotFound
	}
	return v, err
}

// LatestVersion returns the number of the newest state version of a
// stack@env, or ErrNotFound when no run wrote one. Unlike LatestState, it
// reads none of the state.
func (s *Store) LatestVersion(ctx context.Context, env, stack string) (int64, error) {
	var version int64
	err := s.db.QueryRowContext(ctx, "SELECT version FROM tfstate_versions WHERE env = ? AND stack = ? ORDER BY version DESC LIMIT 1",
		env, stack).Scan(&version)
	if errors.Is(err, sqldb.ErrNoRows) {
		return 0, ErrNotFound
	}
	return version, err
}

// SealedState returns the sealed bytes of one state version of a
// stack@env, or ErrNotFound when the version is not kept.
func (s *Store) SealedState(ctx context.Context, env, stack string, version int64) ([]byte, error) {
	var sealed []byte
	err := s.db.QueryRowContext(ctx, "SELECT sealed FROM tfstate_versions WHERE env = ? AND stack = ? AND version = ?",
		env, stack, version).Scan(&sealed)
	if errors.Is(err, sqldb.ErrNoRows) {
		return nil, ErrNotFound
	}
	return sealed, err
}

func scanVersion(r scanner) (StateVersion, error) {
	var v StateVersion
	var at int64
	if err := r.Scan(&v.Version, &v.Serial, &v.Lineage, &v.SHA256, &v.RunID, &at, &v.Sealed); err != nil {
		return StateVersion{}, err
	}
	v.At = fromMS(at)
	return v, nil
}

// LockState takes the lock of a stack@env for l.RunID and commits audit
// with it. When the lock is held already, it returns the held lock with
// ErrStateLocked, unless the same run holds it with the same id, which
// makes a repeat succeed.
func (s *Store) LockState(ctx context.Context, a StateAuth, env, stack string, l StateLock, audit Entry) (StateLock, error) {
	var held StateLock
	err := s.db.Write(ctx, func(tx *sqldb.Tx) error {
		if err := liveToken(ctx, tx, a); err != nil {
			return err
		}
		// On Postgres a lock released between the insert and the read
		// leaves nothing to read, so the insert is tried again.
		for range 3 {
			res, err := tx.ExecContext(ctx, `INSERT INTO tfstate_locks (env, stack, lock_id, run_id, info, created_at)
				VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (env, stack) DO NOTHING`, env, stack, l.ID, l.RunID, l.Info, l.At.UnixMilli())
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if n == 1 {
				return insertAudit(ctx, tx, audit)
			}
			held, err = heldLock(ctx, tx, env, stack)
			if errors.Is(err, sqldb.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			if held.RunID == l.RunID && held.ID == l.ID {
				return insertAudit(ctx, tx, audit)
			}
			return ErrStateLocked
		}
		return fmt.Errorf("the lock of %s@%s changed during the request; try again", stack, env)
	})
	return held, err
}

// UnlockState releases the lock that runID holds on a stack@env and
// commits audit with the release. An empty id matches any lock of the
// run, because OpenTofu sends no lock id with a force-unlock. A lock of
// another run, or of another id, is left as it is and returned with
// ErrStateLocked. With no lock, it releases nothing.
func (s *Store) UnlockState(ctx context.Context, a StateAuth, env, stack, id string, audit Entry) (StateLock, error) {
	var held StateLock
	runID := a.RunID
	err := s.db.Write(ctx, func(tx *sqldb.Tx) error {
		if err := liveToken(ctx, tx, a); err != nil {
			return err
		}
		var err error
		held, err = heldLock(ctx, tx, env, stack)
		if errors.Is(err, sqldb.ErrNoRows) {
			return insertAudit(ctx, tx, audit)
		}
		if err != nil {
			return err
		}
		if held.RunID != runID || id != "" && held.ID != id {
			return ErrStateLocked
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM tfstate_locks WHERE env = ? AND stack = ?", env, stack); err != nil {
			return err
		}
		return insertAudit(ctx, tx, audit)
	})
	return held, err
}

func heldLock(ctx context.Context, tx *sqldb.Tx, env, stack string) (StateLock, error) {
	var l StateLock
	var at int64
	err := tx.QueryRowContext(ctx, "SELECT lock_id, run_id, info, created_at FROM tfstate_locks WHERE env = ? AND stack = ?"+forUpdate(tx),
		env, stack).Scan(&l.ID, &l.RunID, &l.Info, &at)
	l.At = fromMS(at)
	return l, err
}

// PutState stores a new state version of a stack@env. The writing run must
// hold the stack's lock with p.LockID. The version's lineage must be the
// stored lineage and its serial above the stored serial; a repeat of the
// newest version, as a retried request sends it, succeeds and stores
// nothing. Versions beyond p.Keep are deleted, oldest first. Each write
// that succeeds, a repeat too, commits p.Audit.
func (s *Store) PutState(ctx context.Context, p StatePut) error {
	if p.Keep < 1 {
		return errors.New("a state must keep at least one version")
	}
	if p.Seal == nil {
		return errors.New("a state write needs a Seal")
	}
	v := p.Version
	return s.db.Write(ctx, func(tx *sqldb.Tx) error {
		if err := liveToken(ctx, tx, p.Auth); err != nil {
			return err
		}
		held, err := heldLock(ctx, tx, p.Env, p.Stack)
		if errors.Is(err, sqldb.ErrNoRows) {
			return fmt.Errorf("%w: %s@%s is not locked", ErrStateNotLocked, p.Stack, p.Env)
		}
		if err != nil {
			return err
		}
		if held.RunID != v.RunID || held.ID != p.LockID {
			return fmt.Errorf("%w: the lock is %s of run %s", ErrStateNotLocked, held.ID, held.RunID)
		}
		var last StateVersion
		err = tx.QueryRowContext(ctx, `SELECT version, serial, lineage, sha256 FROM tfstate_versions
			WHERE env = ? AND stack = ? ORDER BY version DESC LIMIT 1`, p.Env, p.Stack).Scan(&last.Version, &last.Serial, &last.Lineage, &last.SHA256)
		switch {
		case errors.Is(err, sqldb.ErrNoRows):
		case err != nil:
			return err
		case last.SHA256 == v.SHA256 && last.Serial == v.Serial && last.Lineage == v.Lineage:
			e := p.Audit
			e.Reason = fmt.Sprintf("a repeat of state version %d, serial %d, lineage %s, sha256 %s; nothing stored", last.Version, v.Serial, v.Lineage, v.SHA256)
			return insertAudit(ctx, tx, e)
		case last.Lineage != v.Lineage:
			return fmt.Errorf("%w: lineage %s is not the stored lineage %s", ErrStateStale, v.Lineage, last.Lineage)
		case v.Serial <= last.Serial:
			return fmt.Errorf("%w: serial %d is not above the stored serial %d", ErrStateStale, v.Serial, last.Serial)
		}
		next := last.Version + 1
		if _, err := tx.ExecContext(ctx, `INSERT INTO tfstate_versions (env, stack, version, serial, lineage, sha256, run_id, created_at, sealed)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, p.Env, p.Stack, next, v.Serial, v.Lineage, v.SHA256, v.RunID, v.At.UnixMilli(), p.Seal(next)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM tfstate_versions WHERE env = ? AND stack = ? AND version <= ?", p.Env, p.Stack, next-int64(p.Keep)); err != nil {
			return err
		}
		e := p.Audit
		e.Reason = fmt.Sprintf("state version %d, serial %d, lineage %s, sha256 %s", next, v.Serial, v.Lineage, v.SHA256)
		return insertAudit(ctx, tx, e)
	})
}
