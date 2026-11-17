package control

import (
	"context"
	"errors"
	"time"

	"github.com/mr-karan/writ/internal/sqldb"
)

// Carrier is how a session travels: in the CLI's session file, or in the
// control UI's cookie.
type Carrier string

const (
	CLI     Carrier = "cli"
	Browser Carrier = "browser"
)

// Session is a person's session. The table keeps only the SHA-256 of its
// token, so a copy of the database signs no one in.
type Session struct {
	ID        string
	Carrier   Carrier
	Person    string
	Display   string
	Issuer    string
	Subject   string
	Teams     []string
	CreatedAt time.Time
	EndsAt    time.Time
	// LastSeenAt is the last use, to the minute; Sessions fills it.
	LastSeenAt time.Time
}

// lastSeenEvery bounds how often a session's last use is written.
const lastSeenEvery = time.Minute

// CreateSession stores a session under the hash of its token and writes
// the audit row of the sign-in, in one transaction.
func (s *Store) CreateSession(ctx context.Context, sess Session, hash []byte, e Entry) error {
	return s.db.Write(ctx, func(tx *sqldb.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO sessions (id, hash, carrier, person, display, issuer, subject, teams, created_at, ends_at, last_seen_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sess.ID, hash, string(sess.Carrier), sess.Person, sess.Display, sess.Issuer, sess.Subject, list(sess.Teams),
			sess.CreatedAt.UnixMilli(), sess.EndsAt.UnixMilli(), sess.CreatedAt.UnixMilli())
		if err != nil {
			return err
		}
		return insertAudit(ctx, tx, e)
	})
}

// SessionByHash returns the live session of carrier whose token has hash,
// and records its use at most once a minute. A session that ended, ran
// out, or travels by the other carrier is ErrNotFound: a token from the
// CLI's session file opens no control UI page, and a cookie's token is no
// bearer token.
func (s *Store) SessionByHash(ctx context.Context, hash []byte, carrier Carrier, now time.Time) (Session, error) {
	var sess Session
	var teams string
	var created, ends int64
	err := s.db.QueryRowContext(ctx, `UPDATE sessions SET last_seen_at = CASE WHEN last_seen_at IS NULL OR last_seen_at < ? THEN ? ELSE last_seen_at END
		WHERE hash = ? AND carrier = ? AND ended_at IS NULL AND ends_at > ?
		RETURNING id, person, display, issuer, subject, teams, created_at, ends_at`, now.Add(-lastSeenEvery).UnixMilli(), now.UnixMilli(), hash, string(carrier), now.UnixMilli()).
		Scan(&sess.ID, &sess.Person, &sess.Display, &sess.Issuer, &sess.Subject, &teams, &created, &ends)
	if errors.Is(err, sqldb.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, err
	}
	if sess.Teams, err = unlist[string](teams); err != nil {
		return Session{}, err
	}
	sess.Carrier, sess.CreatedAt, sess.EndsAt = carrier, fromMS(created), fromMS(ends)
	return sess, nil
}

// maxSessions bounds a list of sessions.
const maxSessions = 1000

// Sessions returns the live sessions, newest first, at most 1,000. A
// person who is not "" narrows them to that person's.
func (s *Store) Sessions(ctx context.Context, person string, now time.Time) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, carrier, person, display, issuer, subject, teams, created_at, ends_at, last_seen_at FROM sessions
		WHERE ended_at IS NULL AND ends_at > ? AND (? = '' OR person = ?)
		ORDER BY created_at DESC, id LIMIT ?`, now.UnixMilli(), person, person, maxSessions)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Session{}
	for rows.Next() {
		var sess Session
		var carrier, teams string
		var created, ends int64
		var seen *int64
		if err := rows.Scan(&sess.ID, &carrier, &sess.Person, &sess.Display, &sess.Issuer, &sess.Subject, &teams, &created, &ends, &seen); err != nil {
			return nil, err
		}
		if sess.Teams, err = unlist[string](teams); err != nil {
			return nil, err
		}
		sess.Carrier, sess.CreatedAt, sess.EndsAt = Carrier(carrier), fromMS(created), fromMS(ends)
		if seen != nil {
			sess.LastSeenAt = fromMS(*seen)
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// EndSessionByID ends the live session with id, as by, and writes the
// audit row, in one transaction, for an admin who offboards a person. It
// returns the session it ended. A session that is not live is ErrNotFound.
func (s *Store) EndSessionByID(ctx context.Context, id, by string, now time.Time, e Entry) (Session, error) {
	var sess Session
	err := s.db.Write(ctx, func(tx *sqldb.Tx) error {
		var carrier, teams string
		var created, ends int64
		err := tx.QueryRowContext(ctx, `UPDATE sessions SET ended_at = ?, ended_by = ? WHERE id = ? AND ended_at IS NULL AND ends_at > ?
			RETURNING id, carrier, person, display, issuer, subject, teams, created_at, ends_at`, now.UnixMilli(), by, id, now.UnixMilli()).
			Scan(&sess.ID, &carrier, &sess.Person, &sess.Display, &sess.Issuer, &sess.Subject, &teams, &created, &ends)
		if errors.Is(err, sqldb.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if sess.Teams, err = unlist[string](teams); err != nil {
			return err
		}
		sess.Carrier, sess.CreatedAt, sess.EndsAt = Carrier(carrier), fromMS(created), fromMS(ends)
		return insertAudit(ctx, tx, e)
	})
	return sess, err
}

// EndSession ends the live session whose token has hash, as by, and writes
// the audit row, in one transaction. A session already ended is
// ErrNotFound.
func (s *Store) EndSession(ctx context.Context, hash []byte, by string, now time.Time, e Entry) error {
	return s.db.Write(ctx, func(tx *sqldb.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE sessions SET ended_at = ?, ended_by = ? WHERE hash = ? AND ended_at IS NULL`, now.UnixMilli(), by, hash)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return cmpErr(err, ErrNotFound)
		}
		return insertAudit(ctx, tx, e)
	})
}
