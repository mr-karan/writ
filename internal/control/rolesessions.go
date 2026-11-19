package control

import (
	"context"
	"time"
)

// RoleSession is a role session the control plane assumed. From is when
// its first credentials were issued, and Until when its last ones expire.
type RoleSession struct {
	Role           string
	Name           string
	SourceIdentity string
	From, Until    time.Time
}

// roleSessionKeep is how long a role session is kept after it expires:
// SQS's longest retention, so a message delivered late still finds the
// session it was made with.
const roleSessionKeep = 14 * 24 * time.Hour

// RecordRoleSession records that the control plane assumed a role session.
// Recording the same session again keeps its first time and its latest
// expiry.
func (s *Store) RecordRoleSession(ctx context.Context, r RoleSession) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO role_sessions (role, session, source_identity, first_at, expires_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (role, session, source_identity) DO UPDATE SET
			first_at = CASE WHEN role_sessions.first_at < excluded.first_at THEN role_sessions.first_at ELSE excluded.first_at END,
			expires_at = CASE WHEN role_sessions.expires_at > excluded.expires_at THEN role_sessions.expires_at ELSE excluded.expires_at END`,
		r.Role, r.Name, r.SourceIdentity, r.From.UnixMilli(), r.Until.UnixMilli())
	return s.db.Error(err)
}

// RoleSessions returns the recorded sessions of a role with a session
// name, whatever their source identity.
func (s *Store) RoleSessions(ctx context.Context, role, name string) ([]RoleSession, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT source_identity, first_at, expires_at FROM role_sessions
		WHERE role = ? AND session = ? ORDER BY first_at`, role, name)
	if err != nil {
		return nil, s.db.Error(err)
	}
	defer rows.Close()
	var out []RoleSession
	for rows.Next() {
		r := RoleSession{Role: role, Name: name}
		var from, until int64
		if err := rows.Scan(&r.SourceIdentity, &from, &until); err != nil {
			return nil, s.db.Error(err)
		}
		r.From, r.Until = fromMS(from), fromMS(until)
		out = append(out, r)
	}
	return out, s.db.Error(rows.Err())
}
