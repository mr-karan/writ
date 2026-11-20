package control

import (
	"context"
	"os"
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

// RecordNotification records the outcome of one notification delivery attempt
// for the notifier named name. On success it increments delivered, updates
// last_ok_at, and clears last_error; on failure it increments failed and
// records errText and last_error_at.
func (s *Store) RecordNotification(ctx context.Context, name, event string, ok bool, errText string, at time.Time) error {
	ms := at.UnixMilli()
	return s.db.Write(ctx, func(tx *sqldb.Tx) error {
		if ok {
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
			VALUES (?, 0, 1, ?, 0, ?, ?)
			ON CONFLICT (name) DO UPDATE SET
				failed = notification_status.failed + 1,
				last_event = excluded.last_event,
				last_error_at = excluded.last_error_at,
				last_error = excluded.last_error`,
			name, event, ms, errText)
		return err
	})
}

// NotificationStatuses returns the persisted delivery status for every
// notifier that has recorded at least one delivery, keyed by notifier name.
func (s *Store) NotificationStatuses(ctx context.Context) (map[string]NotificationStatus, error) {
	return queryNotificationStatuses(ctx, s.db)
}

// PeekNotificationStatuses reads persisted delivery statuses from location
// without creating files or running migrations, for writ serve --check. It
// returns nil if the database is unreachable or has no notification_status
// table yet.
func PeekNotificationStatuses(ctx context.Context, location string) map[string]NotificationStatus {
	if location == "" {
		return nil
	}
	pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var db *sqldb.DB
	var err error
	if sqldb.IsPostgres(location) {
		if CheckURL(location) != nil {
			return nil
		}
		db, err = sqldb.OpenPostgres(location, sqldb.PostgresOptions{SearchPath: Schema, AppName: "writ check", MaxConns: 2, What: what, ConnectTimeout: time.Second})
	} else {
		if _, statErr := os.Stat(location); statErr != nil {
			return nil
		}
		db, err = sqldb.OpenSQLite(location, sqldb.ReadOnly, what)
	}
	if err != nil {
		return nil
	}
	defer db.Close()
	out, err := queryNotificationStatuses(pctx, db)
	if err != nil {
		return nil
	}
	return out
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
