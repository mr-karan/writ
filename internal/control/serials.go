package control

import (
	"context"
	"errors"
	"time"

	"github.com/mr-karan/writ/internal/sqldb"
)

// keepSerials is how many serials state_serials keeps for each stack@env.
const keepSerials = 20

// StateSerial is one serial of a stack@env's state on S3 that writ serve
// read.
type StateSerial struct {
	Env, Stack string
	Lineage    string
	Serial     int64
	// RunID is the apply that wrote the serial, "" when none did.
	RunID string
	// From is, for an apply, the serial its plan read: it wrote each
	// serial of Lineage above From up to Serial. For a serial no apply
	// wrote, it is Serial.
	From   int64
	SeenAt time.Time
}

// SerialWriter is who wrote a serial, as JudgeSerial finds it.
type SerialWriter int

const (
	// SerialBaseline: writ serve has read no serial of the stack@env
	// before, so this one is where it starts, and no drift.
	SerialBaseline SerialWriter = iota
	// SerialSeen: writ serve read this serial before.
	SerialSeen
	// SerialWrit: an apply of writ serve wrote it: it is the serial the
	// apply ended at, or one between its plan's and that.
	SerialWrit
	// SerialOutside: no apply of writ serve wrote it: drift.
	SerialOutside
)

// Judged is what writ serve knows of one serial.
type Judged struct {
	Writer SerialWriter
	// RunID is the apply that wrote it, for SerialWrit, and for
	// SerialSeen when an apply did.
	RunID string
}

// JudgeSerial says who wrote a serial of a stack@env's state on S3.
func (s *Store) JudgeSerial(ctx context.Context, env, stack, lineage string, serial int64) (Judged, error) {
	return judgeSerial(ctx, s.db, env, stack, lineage, serial)
}

// rowQuerier is a pool or a transaction.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, q string, args ...any) sqldb.Row
}

func judgeSerial(ctx context.Context, q rowQuerier, env, stack, lineage string, serial int64) (Judged, error) {
	var run string
	err := q.QueryRowContext(ctx, "SELECT run_id FROM state_serials WHERE env = ? AND stack = ? AND lineage = ? AND serial = ?",
		env, stack, lineage, serial).Scan(&run)
	switch {
	case err == nil:
		return Judged{Writer: SerialSeen, RunID: run}, nil
	case !errors.Is(err, sqldb.ErrNoRows):
		return Judged{}, err
	}
	err = q.QueryRowContext(ctx, `SELECT run_id FROM state_serials WHERE env = ? AND stack = ? AND lineage = ? AND run_id <> ''
		AND from_serial < ? AND serial >= ? ORDER BY serial LIMIT 1`, env, stack, lineage, serial, serial).Scan(&run)
	switch {
	case err == nil:
		return Judged{Writer: SerialWrit, RunID: run}, nil
	case !errors.Is(err, sqldb.ErrNoRows):
		return Judged{}, err
	}
	var n int
	if err := q.QueryRowContext(ctx, "SELECT count(*) FROM state_serials WHERE env = ? AND stack = ?", env, stack).Scan(&n); err != nil {
		return Judged{}, err
	}
	if n == 0 {
		return Judged{Writer: SerialBaseline}, nil
	}
	return Judged{Writer: SerialOutside}, nil
}

// SerialRead is a serial of a stack@env's state on S3 that a plan or a
// refresh of writ serve read: no apply claims it.
type SerialRead struct {
	Env, Stack, Lineage string
	Serial              int64
	// At is when the run read the serial, and Since when it started.
	At, Since time.Time
}

// errApplyRan rolls back an ObserveSerial whose read an apply overlapped.
var errApplyRan = errors.New("an apply of the stack ran since the read started")

// ObserveSerial records r, judged as JudgeSerial judges it: a serial no
// apply of writ serve wrote is marked as the stack@env's drift, and one an
// apply passed through is recorded as that apply's. It returns the
// judgement and the read's place among the reads of the stack@env.
//
// It records nothing, and returns place 0, when an apply of the stack@env
// ran at or after r.Since or runs now: the apply may have written the
// serial, and the next read judges it. The check and the place are taken
// in one transaction that first locks the stack@env's row of
// state_s3_stacks, which an apply's PutSerial locks too, so an apply that
// ends while this read is recorded either is seen by the check or takes a
// later place than it.
func (s *Store) ObserveSerial(ctx context.Context, r SerialRead) (Judged, int64, error) {
	var j Judged
	var seq int64
	err := s.db.Write(ctx, func(tx *sqldb.Tx) error {
		var err error
		if seq, err = nextRead(ctx, tx, r.Env, r.Stack); err != nil {
			return err
		}
		var ran int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM runs WHERE env = ? AND stack = ? AND (ended_at IS NULL OR ended_at >= ?)",
			r.Env, r.Stack, r.Since.UnixMilli()).Scan(&ran); err != nil {
			return err
		}
		if ran > 0 {
			return errApplyRan
		}
		if j, err = judgeSerial(ctx, tx, r.Env, r.Stack, r.Lineage, r.Serial); err != nil {
			return err
		}
		if j.Writer == SerialOutside {
			if _, err := tx.ExecContext(ctx, `INSERT INTO state_index_stacks (env, stack, backend, version, lineage, indexed_at, outside_serial, outside_at)
				VALUES (?, ?, ?, 0, '', 0, ?, ?)
				ON CONFLICT (env, stack) DO UPDATE SET outside_serial = excluded.outside_serial, outside_at = excluded.outside_at`,
				r.Env, r.Stack, BackendS3, r.Serial, r.At.UnixMilli()); err != nil {
				return err
			}
		}
		// A serial read before keeps its writer and its sealed secrets;
		// this read only takes a newer place.
		v := StateSerial{Env: r.Env, Stack: r.Stack, Lineage: r.Lineage, Serial: r.Serial, From: r.Serial, SeenAt: r.At}
		if j.Writer == SerialWrit {
			v.RunID = j.RunID
		}
		return putSerial(ctx, tx, v, nil, seq)
	})
	if errors.Is(err, errApplyRan) {
		return Judged{}, 0, nil
	}
	return j, seq, err
}

// PutSerial records a serial writ serve read, with the sealed index
// secrets of the apply that wrote it, nil for none, and returns the
// read's place among the reads of the stack@env, which an index read from
// it takes as its IndexWrite.Seq. A serial recorded already keeps its
// writer, unless this one names an apply. The stack@env keeps its newest
// keepSerials rows that applies wrote, and its newest keepSerials others.
// An apply records the serial it wrote with it.
func (s *Store) PutSerial(ctx context.Context, v StateSerial, secrets []byte) (int64, error) {
	var seq int64
	err := s.db.Write(ctx, func(tx *sqldb.Tx) error {
		var err error
		if seq, err = nextRead(ctx, tx, v.Env, v.Stack); err != nil {
			return err
		}
		return putSerial(ctx, tx, v, secrets, seq)
	})
	return seq, err
}

// nextRead takes the next place among the reads of a stack@env. The
// upsert locks the stack@env's row until the transaction ends, so reads
// and index writes of one stack@env take their turns in the order of
// their places.
func nextRead(ctx context.Context, tx *sqldb.Tx, env, stack string) (int64, error) {
	var seq int64
	err := tx.QueryRowContext(ctx, `INSERT INTO state_s3_stacks (env, stack, read_seq) VALUES (?, ?, 1)
		ON CONFLICT (env, stack) DO UPDATE SET read_seq = state_s3_stacks.read_seq + 1 RETURNING read_seq`, env, stack).Scan(&seq)
	return seq, err
}

func putSerial(ctx context.Context, tx *sqldb.Tx, v StateSerial, secrets []byte, seq int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO state_serials (env, stack, lineage, serial, run_id, from_serial, seen_at, read_seq, index_secrets)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (env, stack, lineage, serial) DO UPDATE SET read_seq = excluded.read_seq,
			run_id = CASE WHEN excluded.run_id <> '' THEN excluded.run_id ELSE state_serials.run_id END,
			from_serial = CASE WHEN excluded.run_id <> '' THEN excluded.from_serial ELSE state_serials.from_serial END,
			seen_at = CASE WHEN excluded.run_id <> '' THEN excluded.seen_at ELSE state_serials.seen_at END,
			index_secrets = CASE WHEN excluded.run_id <> '' THEN excluded.index_secrets ELSE state_serials.index_secrets END`,
		v.Env, v.Stack, v.Lineage, v.Serial, v.RunID, v.From, v.SeenAt.UnixMilli(), seq, secrets)
	if err != nil {
		return err
	}
	// The serials applies wrote are kept apart from the others, so a run
	// of writes outside Writ does not prune the apply a refresh reads the
	// stack's files from.
	for _, cmp := range []string{"=", "<>"} {
		_, err = tx.ExecContext(ctx, `DELETE FROM state_serials WHERE env = ? AND stack = ? AND run_id `+cmp+` '' AND seen_at <
			(SELECT seen_at FROM state_serials WHERE env = ? AND stack = ? AND run_id `+cmp+` '' ORDER BY seen_at DESC LIMIT 1 OFFSET ?)`,
			v.Env, v.Stack, v.Env, v.Stack, keepSerials-1)
		if err != nil {
			return err
		}
	}
	return nil
}

// SerialSecrets returns the sealed index secrets of one serial of a
// stack@env's state, or ErrNotFound when the serial is not kept or has
// none.
func (s *Store) SerialSecrets(ctx context.Context, env, stack, lineage string, serial int64) ([]byte, error) {
	var sealed []byte
	err := s.db.QueryRowContext(ctx, "SELECT index_secrets FROM state_serials WHERE env = ? AND stack = ? AND lineage = ? AND serial = ?",
		env, stack, lineage, serial).Scan(&sealed)
	if errors.Is(err, sqldb.ErrNoRows) || err == nil && sealed == nil {
		return nil, ErrNotFound
	}
	return sealed, err
}

// SealedSet is the sealed index secrets of one state version on the
// http backend, or of one serial on S3, which OpenIndexSecrets opens with
// that number.
type SealedSet struct {
	Version int64
	Sealed  []byte
}

// SecretSets returns every secret set that writ serve keeps for a
// stack@env: those of its kept serials on S3, of any lineage, and those
// of its kept state versions on the http backend, from before the stack
// moved to S3. putSerial and the http backend's own pruning bound the
// rows.
func (s *Store) SecretSets(ctx context.Context, env, stack string) ([]SealedSet, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT serial, index_secrets FROM state_serials WHERE env = ? AND stack = ? AND index_secrets IS NOT NULL
		UNION ALL SELECT version, index_secrets FROM tfstate_versions WHERE env = ? AND stack = ? AND index_secrets IS NOT NULL`, env, stack, env, stack)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SealedSet
	for rows.Next() {
		var set SealedSet
		if err := rows.Scan(&set.Version, &set.Sealed); err != nil {
			return nil, s.db.Error(err)
		}
		out = append(out, set)
	}
	return out, s.db.Error(rows.Err())
}

// S3Stack is a stack@env on S3 that an apply of writ serve wrote, with
// the newest such apply.
type S3Stack struct {
	Env, Stack string
	RunID      string
}

// S3Stacks returns up to limit stack@envs on S3 whose state an apply of
// writ serve wrote, each with the newest such apply, leaving out those
// with an apply running: its own read of the state follows it. Those
// never checked come first, then those CheckedS3 recorded longest ago.
func (s *Store) S3Stacks(ctx context.Context, limit int) ([]S3Stack, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT c.env, c.stack,
			(SELECT v.run_id FROM state_serials v WHERE v.env = c.env AND v.stack = c.stack AND v.run_id <> '' ORDER BY v.seen_at DESC, v.read_seq DESC LIMIT 1)
		FROM state_s3_stacks c
		WHERE EXISTS (SELECT 1 FROM state_serials v WHERE v.env = c.env AND v.stack = c.stack AND v.run_id <> '')
		AND NOT EXISTS (SELECT 1 FROM runs r WHERE r.env = c.env AND r.stack = c.stack AND r.ended_at IS NULL)
		ORDER BY c.checked_at, c.env, c.stack LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []S3Stack
	for rows.Next() {
		var st S3Stack
		if err := rows.Scan(&st.Env, &st.Stack, &st.RunID); err != nil {
			return nil, s.db.Error(err)
		}
		out = append(out, st)
	}
	return out, s.db.Error(rows.Err())
}

// CheckedS3 records that the timer read the state of a stack@env on S3
// at at, whatever the read found, so S3Stacks gives the others their turn
// first.
func (s *Store) CheckedS3(ctx context.Context, env, stack string, at time.Time) error {
	return s.db.Write(ctx, func(tx *sqldb.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE state_s3_stacks SET checked_at = ? WHERE env = ? AND stack = ?", at.UnixMilli(), env, stack)
		return err
	})
}
