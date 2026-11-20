package control

import (
	"context"
	"errors"
	"time"

	"github.com/mr-karan/writ/internal/sqldb"
)

// NotificationStatus is the persisted delivery summary for one configured
// notifier: counts, last event attempted, last successful delivery, and last
// error (cleared once a subsequent delivery succeeds).
type NotificationStatus struct {
	Name        string
	Delivered   int64
	Failed      int64
	LastEvent   string
	LastOKAt    time.Time
	LastErrorAt time.Time
	LastError   string
}

// NotificationResult is how one delivery to a notifier ended, or how many
// events it dropped. Err is writ's own short text, never what the receiver
// answered.
type NotificationResult struct {
	Name, Event string
	OK          bool
	// Failed counts the events that did not arrive: 1 for a delivery that
	// gave up, more for events dropped from a full queue.
	Failed int64
	Err    string
	At     time.Time
}

// RecordNotification records how a delivery to a notifier ended. One that
// arrived adds to delivered, sets last_ok_at, and clears last_error; one
// that did not adds r.Failed to failed and sets last_error and
// last_error_at.
func (s *Store) RecordNotification(ctx context.Context, r NotificationResult) error {
	name, event, errText, ms := r.Name, r.Event, r.Err, r.At.UnixMilli()
	return s.db.Write(ctx, func(tx *sqldb.Tx) error {
		if r.OK {
			_, err := tx.ExecContext(ctx, `INSERT INTO notification_status (name, delivered, failed, last_event, last_ok_at, last_error_at, last_error)
				VALUES (?, 1, 0, ?, ?, 0, '')
				ON CONFLICT (name) DO UPDATE SET
					delivered = notification_status.delivered + 1,
					last_event = excluded.last_event,
					last_ok_at = excluded.last_ok_at,
					last_error_at = 0,
					last_error = ''`,
				name, event, ms)
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO notification_status (name, delivered, failed, last_event, last_ok_at, last_error_at, last_error)
			VALUES (?, 0, ?, ?, 0, ?, ?)
			ON CONFLICT (name) DO UPDATE SET
				failed = notification_status.failed + excluded.failed,
				last_event = excluded.last_event,
				last_error_at = excluded.last_error_at,
				last_error = excluded.last_error`,
			name, r.Failed, event, ms, errText)
		return err
	})
}

// NotificationStatuses returns the persisted delivery status for every
// notifier that has recorded at least one delivery, keyed by notifier name.
func (s *Store) NotificationStatuses(ctx context.Context) (map[string]NotificationStatus, error) {
	return queryNotificationStatuses(ctx, s.db)
}

// ErrNotMigrated means a read of writ serve --check found the table it
// reads missing: the control database is at a schema version before the
// migration that adds it, which writ serve applies when it starts.
var ErrNotMigrated = errors.New("the control database lacks a table a migration adds")

// PeekNotificationStatuses reads the delivery statuses in the Postgres
// database at location without migrating it, for writ serve --check. It
// returns ErrNotMigrated before the migration that adds them.
func PeekNotificationStatuses(ctx context.Context, location string) (map[string]NotificationStatus, error) {
	if err := CheckURL(location); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	db, err := sqldb.OpenPostgres(location, sqldb.PostgresOptions{SearchPath: Schema, AppName: "writ serve --check", MaxConns: 1, What: what, ConnectTimeout: 2 * time.Second})
	if err != nil {
		return nil, err
	}
	defer db.Close()
	// The catalog includes tables the connecting role cannot read.
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = ? AND c.relname = 'notification_status' AND c.relkind IN ('r', 'p'))`, Schema).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotMigrated
	}
	return queryNotificationStatuses(ctx, db)
}

func queryNotificationStatuses(ctx context.Context, db *sqldb.DB) (map[string]NotificationStatus, error) {
	rows, err := db.QueryContext(ctx, `SELECT name, delivered, failed, last_event, last_ok_at, last_error_at, last_error FROM notification_status ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]NotificationStatus{}
	for rows.Next() {
		var st NotificationStatus
		var okAt, errAt int64
		if err := rows.Scan(&st.Name, &st.Delivered, &st.Failed, &st.LastEvent, &okAt, &errAt, &st.LastError); err != nil {
			return nil, err
		}
		if okAt > 0 {
			st.LastOKAt = fromMS(okAt)
		}
		if errAt > 0 {
			st.LastErrorAt = fromMS(errAt)
		}
		out[st.Name] = st
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
