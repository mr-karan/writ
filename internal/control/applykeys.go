package control

import (
	"context"
	"errors"
	"time"

	"github.com/mr-karan/writ/internal/sqldb"
)

// KeyLife is how long an apply request's key may start a run, from the
// time it carries. A key older than that is refused, so its none row can
// be deleted once no request can use the key.
const KeyLife = 24 * time.Hour

// keyKeep is how long past its life a key's none row is kept. It is far
// longer than any transaction of BeginApply waits before the key's
// insert, so no request that passed the check of the key's life can
// insert the key after its none row is gone.
const keyKeep = time.Hour

// KeyOutcome is the final outcome of an apply request's key.
type KeyOutcome string

const (
	// KeyStarted is a key whose request began a run.
	KeyStarted KeyOutcome = "started"
	// KeyNone is a key closed with no run.
	KeyNone KeyOutcome = "none"
)

// RequestKey is the key of an apply request.
type RequestKey struct {
	Key      string
	IssuedAt time.Time
	// Request is the SHA-256 of the request's fields, so a repeat of the
	// key can be told from another request with it.
	Request string
}

// KeyRow is the row of a key.
type KeyRow struct {
	Key     string
	Outcome KeyOutcome
	// RunID, Issuer, Subject, and Request are those of a started key, ""
	// for none.
	RunID           string
	Issuer, Subject string
	Request         string
	IssuedAt, At    time.Time
}

// ErrKeyUsed means an apply request's key already has a row, so
// BeginApply committed nothing.
var ErrKeyUsed = errors.New("the request's key already has an outcome")

// ErrKeyClosed means an apply request's key is at or below the time of a
// key whose none row was deleted, so it starts no run, whatever the clock
// says. BeginApply committed nothing.
var ErrKeyClosed = errors.New("the request's key is closed with no run")

// insertKey inserts the started row of a request's key, the first write of
// BeginApply. It then refuses a key at or below the floor of pruned keys.
// On Postgres the floor's read is a statement after the insert, so when
// the insert waited for the prune that deleted the key's none row, the
// read sees the floor that prune wrote.
func insertKey(ctx context.Context, tx *sqldb.Tx, r ApplyRequest) error {
	if r.Key.Key == "" {
		return nil
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO apply_requests (request_key, outcome, run_id, issuer, subject, request, issued_at, at)
		VALUES (?, 'started', ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		r.Key.Key, r.RunID, r.Actor.Issuer, r.Actor.Subject, r.Key.Request, r.Key.IssuedAt.UnixMilli(), r.Now.UnixMilli())
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return cmpErr(err, ErrKeyUsed)
	}
	var floor int64
	switch err := tx.QueryRowContext(ctx, "SELECT issued_at FROM apply_key_floor WHERE id = 1").Scan(&floor); {
	case errors.Is(err, sqldb.ErrNoRows):
		return nil
	case err != nil:
		return err
	case r.Key.IssuedAt.UnixMilli() <= floor:
		return ErrKeyClosed
	}
	return nil
}

// dropKey deletes the row insertKey made, for a BeginApply that commits a
// refusal and no run.
func dropKey(ctx context.Context, tx *sqldb.Tx, key string) error {
	if key == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, "DELETE FROM apply_requests WHERE request_key = ?", key)
	return err
}

const keyColumns = "request_key, outcome, coalesce(run_id, ''), issuer, subject, request, issued_at, at"

func scanKey(r scanner) (KeyRow, error) {
	var k KeyRow
	var issued, at int64
	if err := r.Scan(&k.Key, &k.Outcome, &k.RunID, &k.Issuer, &k.Subject, &k.Request, &issued, &at); err != nil {
		return KeyRow{}, err
	}
	k.IssuedAt, k.At = fromMS(issued), fromMS(at)
	return k, nil
}

// Key returns the row of an apply request's key, ErrNotFound when it has
// none.
func (s *Store) Key(ctx context.Context, key string) (KeyRow, error) {
	k, err := scanKey(s.db.QueryRowContext(ctx, "SELECT "+keyColumns+" FROM apply_requests WHERE request_key = ?", key))
	if errors.Is(err, sqldb.ErrNoRows) {
		return KeyRow{}, ErrNotFound
	}
	return k, err
}

// CloseKey settles a key: it inserts a none row when the key has no row,
// and returns the row the key then has. The insert waits for a
// transaction that inserted the key and has not ended, so a none it
// returns is final: no later BeginApply with the key commits. It returns
// only after its own commit. On Postgres the read is a statement of its
// own under Read Committed, so it sees the row of a transaction the
// insert waited for.
func (s *Store) CloseKey(ctx context.Context, key string, issued, now time.Time) (KeyRow, error) {
	var k KeyRow
	err := s.db.Write(ctx, func(tx *sqldb.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO apply_requests (request_key, outcome, issued_at, at) VALUES (?, 'none', ?, ?)
			ON CONFLICT DO NOTHING`, key, issued.UnixMilli(), now.UnixMilli()); err != nil {
			return err
		}
		var err error
		k, err = scanKey(tx.QueryRowContext(ctx, "SELECT "+keyColumns+" FROM apply_requests WHERE request_key = ?", key))
		return err
	})
	if err != nil {
		return KeyRow{}, err
	}
	return k, nil
}

// pruneKeys deletes the none rows of keys past their life and keyKeep,
// and in the same transaction raises the floor to the greatest time of a
// key it deleted, so a clock moved back cannot reopen one.
func pruneKeys(ctx context.Context, db *sqldb.DB, now time.Time) (int64, error) {
	var n int64
	err := db.Write(ctx, func(tx *sqldb.Tx) error {
		n = 0
		rows, err := tx.QueryContext(ctx, "DELETE FROM apply_requests WHERE outcome = 'none' AND issued_at < ? RETURNING issued_at",
			now.Add(-KeyLife-keyKeep).UnixMilli())
		if err != nil {
			return err
		}
		defer rows.Close()
		var top int64
		for rows.Next() {
			var issued int64
			if err := rows.Scan(&issued); err != nil {
				return err
			}
			n, top = n+1, max(top, issued)
		}
		if err := rows.Err(); err != nil || n == 0 {
			return err
		}
		rows.Close()
		_, err = tx.ExecContext(ctx, `INSERT INTO apply_key_floor (id, issued_at) VALUES (1, ?)
			ON CONFLICT (id) DO UPDATE SET issued_at = CASE WHEN excluded.issued_at > apply_key_floor.issued_at
				THEN excluded.issued_at ELSE apply_key_floor.issued_at END`, top)
		return err
	})
	return n, err
}
