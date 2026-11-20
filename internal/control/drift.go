package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mr-karan/writ/internal/sqldb"
)

// keepDriftChecks is how many drift checks each stack@env keeps.
const keepDriftChecks = 20

// DriftStatus is where a drift check is, or what it found.
type DriftStatus string

const (
	// DriftPlanning: the check's plan runs.
	DriftPlanning DriftStatus = "planning"
	// DriftClean: the plan changes nothing.
	DriftClean DriftStatus = "clean"
	// DriftDrifted: the plan found a resource changed outside OpenTofu, or
	// the code at the head of the default branch would change something.
	DriftDrifted DriftStatus = "drifted"
	// DriftFailed: the check made no plan, or OpenTofu refused it.
	DriftFailed DriftStatus = "failed"
)

// The kinds of a drift check's change.
const (
	// DriftOutside is a resource that OpenTofu read changed outside
	// OpenTofu: its resource_drift.
	DriftOutside = "outside"
	// DriftCode is a change an apply of the code would make: its
	// resource_changes.
	DriftCode = "code"
)

// DriftChange is one resource a drift check found changed, from its
// masked plan: never a value.
type DriftChange struct {
	Address string `json:"address"`
	// Kind is DriftOutside or DriftCode.
	Kind string `json:"kind"`
	// Action is what OpenTofu's plan does, as writ serve names it:
	// create, update, delete, replace, forget, or import.
	Action string `json:"action"`
	// Attributes names the top-level attributes whose values differ, or
	// are known only after apply, for an update or a replace.
	Attributes []string `json:"attributes,omitempty"`
}

// DriftCheck is one drift check of a stack@env.
type DriftCheck struct {
	Ref        string
	Env, Stack string
	Status     DriftStatus
	StartedAt  time.Time
	// MadeAt is when the check ended, zero while it plans.
	MadeAt time.Time
	// PlanID is the id of the masked plan the check made, "" when it
	// made none.
	PlanID string
	// Repo and Commit are the commit the check planned.
	Repo, Commit string
	// Outside counts the resources changed outside OpenTofu, Code the
	// changes an apply would make. Changes lists them, at most a bound
	// of them.
	Outside, Code int
	Changes       []DriftChange
	// Error says why a failed check made no plan, swept by the mask.
	Error string
	// DriftedSince is when the first check of an unbroken run of drifted
	// checks ended, zero for a check that is not drifted.
	DriftedSince time.Time
}

// StackEnv names one stack@env.
type StackEnv struct{ Env, Stack string }

// driftTargets are the stack@envs drift checks plan: each OpenTofu
// stack@env whose state writ serve indexed, keeps on the http backend, or
// read from S3.
const driftTargets = `SELECT env, stack FROM state_index_stacks UNION SELECT env, stack FROM tfstate_versions UNION SELECT env, stack FROM state_s3_stacks`

// lastCheck is the start of the newest drift check of t's stack@env.
const lastCheck = `COALESCE((SELECT max(d.started_at) FROM drift_checks d WHERE d.env = t.env AND d.stack = t.stack), 0)`

// DriftDue returns up to limit stack@envs of envs, or of any environment
// when envs is nil, whose drift check is due, those checked longest ago
// first, so every stack gets a turn. A check is due when the stack@env
// has none that started after before, or none since an apply of writ
// serve on it ended, writ serve read a state on S3 that no apply of it
// wrote, or a person adopted its state. A stack@env with a check that
// plans or a run that applies is not due.
func (s *Store) DriftDue(ctx context.Context, before time.Time, envs []string, limit int) ([]StackEnv, error) {
	q := `SELECT t.env, t.stack, ` + lastCheck + ` AS last FROM (` + driftTargets + `) t
		WHERE NOT EXISTS (SELECT 1 FROM drift_checks p WHERE p.env = t.env AND p.stack = t.stack AND p.status = 'planning')
		AND NOT EXISTS (SELECT 1 FROM runs r WHERE r.env = t.env AND r.stack = t.stack AND r.ended_at IS NULL)
		AND (` + lastCheck + ` < ?
			OR ` + lastCheck + ` < COALESCE((SELECT max(r.ended_at) FROM runs r WHERE r.env = t.env AND r.stack = t.stack), 0)
			OR ` + lastCheck + ` < COALESCE((SELECT i.outside_at FROM state_index_stacks i WHERE i.env = t.env AND i.stack = t.stack), 0)
			OR ` + lastCheck + ` < COALESCE((SELECT i.adopted_at FROM state_index_stacks i WHERE i.env = t.env AND i.stack = t.stack), 0))`
	args := []any{before.UnixMilli()}
	if envs != nil {
		if len(envs) == 0 {
			return nil, nil
		}
		q += " AND t.env IN (" + strings.TrimSuffix(strings.Repeat("?, ", len(envs)), ", ") + ")"
		for _, e := range envs {
			args = append(args, e)
		}
	}
	rows, err := s.db.QueryContext(ctx, q+" ORDER BY last, t.env, t.stack LIMIT ?", append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StackEnv
	for rows.Next() {
		var st StackEnv
		var last int64
		if err := rows.Scan(&st.Env, &st.Stack, &last); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// BeginDrift stores a drift check of stack@env in status planning. While
// it plans, its state token lives, and DriftDue skips the stack@env.
func (s *Store) BeginDrift(ctx context.Context, ref, env, stack string, at time.Time) error {
	return s.db.Write(ctx, func(tx *sqldb.Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO drift_checks (ref, env, stack, status, started_at) VALUES (?, ?, ?, 'planning', ?)", ref, env, stack, at.UnixMilli())
		return err
	})
}

// FinishDrift records how a drift check ended, and keeps the newest
// checks of its stack@env. It ends the check's state tokens. A check
// during which an apply of the stack@env ran may have read a state that
// apply was changing, so it is deleted instead, and FinishDrift reports
// false: DriftDue finds the stack@env due again. It also returns the
// status of the stack@env's newest earlier check that ended clean or
// drifted, "" for none: a failed check says nothing about drift, so
// drifted, failed, drifted is no change.
func (s *Store) FinishDrift(ctx context.Context, c DriftCheck) (bool, DriftStatus, error) {
	switch c.Status {
	case DriftClean, DriftDrifted, DriftFailed:
	default:
		return false, "", fmt.Errorf("a drift check cannot end %s", c.Status)
	}
	changes, err := json.Marshal(orNone(c.Changes))
	if err != nil {
		return false, "", err
	}
	stored := false
	var prevStatus DriftStatus
	err = s.db.Write(ctx, func(tx *sqldb.Tx) error {
		var env, stack string
		var started int64
		err := tx.QueryRowContext(ctx, "SELECT env, stack, started_at FROM drift_checks WHERE ref = ? AND status = 'planning'"+forUpdate(tx), c.Ref).Scan(&env, &stack, &started)
		if errors.Is(err, sqldb.ErrNoRows) {
			return fmt.Errorf("drift check %s is not planning", c.Ref)
		}
		if err != nil {
			return err
		}
		if _, err := forceEndState(ctx, tx, "run_id = ?", []any{c.Ref}, c.MadeAt, endedWithLock); err != nil {
			return err
		}
		var overlapped int
		err = tx.QueryRowContext(ctx, "SELECT count(*) FROM runs WHERE env = ? AND stack = ? AND (started_at >= ? OR ended_at IS NULL OR ended_at >= ?)",
			env, stack, started, started).Scan(&overlapped)
		if err != nil {
			return err
		}
		if overlapped > 0 {
			_, err := tx.ExecContext(ctx, "DELETE FROM drift_checks WHERE ref = ?", c.Ref)
			return err
		}
		err = tx.QueryRowContext(ctx, `SELECT status FROM drift_checks WHERE env = ? AND stack = ? AND status IN ('clean', 'drifted') AND ref <> ?
			ORDER BY started_at DESC, ref DESC LIMIT 1`, env, stack, c.Ref).Scan(&prevStatus)
		if err != nil && !errors.Is(err, sqldb.ErrNoRows) {
			return err
		}
		var since int64
		if c.Status == DriftDrifted {
			since = c.MadeAt.UnixMilli()
			var last DriftStatus
			var lastSince int64
			err := tx.QueryRowContext(ctx, `SELECT status, drifted_since FROM drift_checks WHERE env = ? AND stack = ? AND status <> 'planning' AND ref <> ?
				ORDER BY started_at DESC, ref DESC LIMIT 1`, env, stack, c.Ref).Scan(&last, &lastSince)
			switch {
			case err == nil && last == DriftDrifted && lastSince > 0:
				since = lastSince
			case err != nil && !errors.Is(err, sqldb.ErrNoRows):
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE drift_checks SET status = ?, made_at = ?, plan_id = ?, repo = ?, source_commit = ?, outside = ?, code = ?,
				changes = ?, error = ?, drifted_since = ? WHERE ref = ?`,
			string(c.Status), c.MadeAt.UnixMilli(), c.PlanID, c.Repo, c.Commit, c.Outside, c.Code, string(changes), c.Error, since, c.Ref)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM drift_checks WHERE env = ? AND stack = ? AND status <> 'planning' AND ref NOT IN
			(SELECT ref FROM drift_checks WHERE env = ? AND stack = ? AND status <> 'planning' ORDER BY started_at DESC, ref DESC LIMIT ?)`,
			env, stack, env, stack, keepDriftChecks)
		stored = err == nil
		return err
	})
	return stored, prevStatus, err
}

func orNone(cs []DriftChange) []DriftChange {
	if cs == nil {
		return []DriftChange{}
	}
	return cs
}

// FailDriftChecks marks every drift check left planning as failed, for a
// start after a crash, and returns how many it marked. It runs before
// EndOrphanState, which then ends their state tokens.
func (s *Store) FailDriftChecks(ctx context.Context, now time.Time) (int64, error) {
	var n int64
	err := s.db.Write(ctx, func(tx *sqldb.Tx) error {
		res, err := tx.ExecContext(ctx, "UPDATE drift_checks SET status = 'failed', made_at = ?, error = ? WHERE status = 'planning'",
			now.UnixMilli(), "the control plane stopped while the check planned")
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}

const driftColumns = `ref, env, stack, status, started_at, made_at, plan_id, repo, source_commit, outside, code, changes, error, drifted_since`

// driftRow is a drift check's columns as the store keeps them.
type driftRow struct {
	ref, env, stack, status, plan, repo, commit, changes, errText string
	started, made, outside, code, since                           int64
}

func (r driftRow) check() (DriftCheck, error) {
	c := DriftCheck{Ref: r.ref, Env: r.env, Stack: r.stack, Status: DriftStatus(r.status), StartedAt: fromMS(r.started), PlanID: r.plan,
		Repo: r.repo, Commit: r.commit, Outside: int(r.outside), Code: int(r.code), Error: r.errText}
	if r.made > 0 {
		c.MadeAt = fromMS(r.made)
	}
	if r.since > 0 {
		c.DriftedSince = fromMS(r.since)
	}
	if err := json.Unmarshal([]byte(r.changes), &c.Changes); err != nil {
		return c, fmt.Errorf("the changes of drift check %s: %w", r.ref, err)
	}
	return c, nil
}

func scanDrift(sc scanner) (DriftCheck, error) {
	var r driftRow
	if err := sc.Scan(&r.ref, &r.env, &r.stack, &r.status, &r.started, &r.made, &r.plan, &r.repo, &r.commit, &r.outside, &r.code, &r.changes, &r.errText, &r.since); err != nil {
		return DriftCheck{}, err
	}
	return r.check()
}

// DriftPlan returns the drift check that made the plan with id, or
// ErrNotFound when no kept check made it.
func (s *Store) DriftPlan(ctx context.Context, id string) (DriftCheck, error) {
	c, err := scanDrift(s.db.QueryRowContext(ctx, "SELECT "+driftColumns+" FROM drift_checks WHERE plan_id = ? AND plan_id <> '' ORDER BY started_at DESC LIMIT 1", id))
	if errors.Is(err, sqldb.ErrNoRows) {
		return DriftCheck{}, ErrNotFound
	}
	return c, s.db.Error(err)
}

// DriftCheck returns the drift check named by key, its ref or the id of
// the plan it made, or ErrNotFound when no kept check matches.
func (s *Store) DriftCheck(ctx context.Context, key string) (DriftCheck, error) {
	c, err := scanDrift(s.db.QueryRowContext(ctx, "SELECT "+driftColumns+" FROM drift_checks WHERE ref = ? OR (plan_id = ? AND plan_id <> '') ORDER BY started_at DESC, ref DESC LIMIT 1", key, key))
	if errors.Is(err, sqldb.ErrNoRows) {
		return DriftCheck{}, ErrNotFound
	}
	return c, s.db.Error(err)
}

// DriftHistory returns the kept drift checks of stack@env, newest first.
func (s *Store) DriftHistory(ctx context.Context, env, stack string) ([]DriftCheck, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+driftColumns+" FROM drift_checks WHERE env = ? AND stack = ? ORDER BY started_at DESC, ref DESC LIMIT ?", env, stack, keepDriftChecks)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DriftCheck
	for rows.Next() {
		c, err := scanDrift(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DriftStack is one stack@env that drift checks plan: what its state
// index holds, the drift writ serve read on S3, and its newest check.
type DriftStack struct {
	Env, Stack string
	// Backend is the stack's state backend, BackendHTTP or BackendS3.
	Backend string
	// Version is the state version or serial the index holds, 0 when it
	// holds none, and Resources how many resources it lists.
	Version   int64
	IndexedAt time.Time
	Resources int
	// OutsideSerial is the newest serial of a state on S3 that writ serve
	// read and no apply of it wrote, and OutsideAt when it read it. 0
	// when it read none.
	OutsideSerial int64
	OutsideAt     time.Time
	// Checking reports a drift check that plans now.
	Checking bool
	// Last is the newest drift check that ended, nil when none did.
	Last *DriftCheck
}

// DriftStacks returns the stack@envs that drift checks plan, of env and
// of stack when they are not "", by env and stack, keeping only those
// keep reports true, at most limit of them, and whether it left more out.
func (s *Store) DriftStacks(ctx context.Context, env, stack string, limit int, keep func(env, stack string) bool) ([]DriftStack, bool, error) {
	return s.driftStacks(ctx, "", env, stack, limit, keep)
}

// DriftAlerts returns the stack@envs of env that drift checks plan and
// that need a person: the newest check that ended drifted or failed, or
// writ serve read a state on S3 that no apply of it wrote. It returns them
// as DriftStacks does.
func (s *Store) DriftAlerts(ctx context.Context, env string, limit int, keep func(env, stack string) bool) ([]DriftStack, bool, error) {
	return s.driftStacks(ctx, "(d.status IN ('drifted', 'failed') OR COALESCE(i.outside_serial, 0) > 0)", env, "", limit, keep)
}

// DriftTarget is one stack@env that drift checks plan, and the status of
// its newest check that ended, "" when none did.
type DriftTarget struct {
	Env, Stack string
	Last       DriftStatus
}

// DriftTargets returns every stack@env that drift checks plan and keep
// reports true, in no order. It reads one narrow row a stack, so a caller
// can count what DriftStacks stops listing at its limit.
func (s *Store) DriftTargets(ctx context.Context, keep func(env, stack string) bool) ([]DriftTarget, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.env, t.stack, COALESCE((SELECT e.status FROM drift_checks e WHERE e.env = t.env AND e.stack = t.stack
			AND e.status <> 'planning' ORDER BY e.started_at DESC, e.ref DESC LIMIT 1), '')
		FROM (`+driftTargets+`) t`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DriftTarget
	for rows.Next() {
		var t DriftTarget
		if err := rows.Scan(&t.Env, &t.Stack, &t.Last); err != nil {
			return nil, err
		}
		if keep(t.Env, t.Stack) {
			out = append(out, t)
		}
	}
	return out, rows.Err()
}

func (s *Store) driftStacks(ctx context.Context, cond, env, stack string, limit int, keep func(env, stack string) bool) ([]DriftStack, bool, error) {
	if cond == "" {
		cond = "1 = 1"
	}
	cols := strings.Split(driftColumns, ", ")
	for i, c := range cols {
		cols[i] = "d." + c
	}
	q := `SELECT t.env, t.stack,
			COALESCE(i.backend, CASE WHEN EXISTS (SELECT 1 FROM state_s3_stacks c WHERE c.env = t.env AND c.stack = t.stack) THEN 's3' ELSE 'http' END),
			COALESCE(i.version, 0), COALESCE(i.indexed_at, 0), COALESCE(i.outside_serial, 0), COALESCE(i.outside_at, 0),
			(SELECT count(*) FROM state_index x WHERE x.env = t.env AND x.stack = t.stack),
			EXISTS (SELECT 1 FROM drift_checks p WHERE p.env = t.env AND p.stack = t.stack AND p.status = 'planning'),
			d.ref IS NOT NULL, ` + strings.Join(cols, ", ") + `
		FROM (` + driftTargets + `) t LEFT JOIN state_index_stacks i ON i.env = t.env AND i.stack = t.stack
		LEFT JOIN drift_checks d ON d.ref = (SELECT e.ref FROM drift_checks e WHERE e.env = t.env AND e.stack = t.stack AND e.status <> 'planning'
			ORDER BY e.started_at DESC, e.ref DESC LIMIT 1)
		WHERE (? = '' OR t.env = ?) AND (? = '' OR t.stack = ?) AND ` + cond + `
		ORDER BY t.env, t.stack`
	rows, err := s.db.QueryContext(ctx, q, env, env, stack, stack)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []DriftStack
	for len(out) <= limit && rows.Next() {
		var st DriftStack
		var indexed, outsideAt int64
		var checked bool
		var last nullDrift
		dest := []any{&st.Env, &st.Stack, &st.Backend, &st.Version, &indexed, &st.OutsideSerial, &outsideAt, &st.Resources, &st.Checking, &checked}
		if err := rows.Scan(append(dest, last.dest()...)...); err != nil {
			return nil, false, err
		}
		if !keep(st.Env, st.Stack) {
			continue
		}
		if indexed > 0 {
			st.IndexedAt = fromMS(indexed)
		}
		if st.OutsideSerial > 0 {
			st.OutsideAt = fromMS(outsideAt)
		}
		if checked {
			c, err := last.row().check()
			if err != nil {
				return nil, false, err
			}
			st.Last = &c
		}
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

// nullDrift holds the columns of a drift check that a LEFT JOIN may leave
// NULL.
type nullDrift struct {
	ref, env, stack, status, plan, repo, commit, changes, errText sqldb.NullString
	started, made, outside, code, since                           sqldb.NullInt64
}

func (n *nullDrift) dest() []any {
	return []any{&n.ref, &n.env, &n.stack, &n.status, &n.started, &n.made, &n.plan, &n.repo, &n.commit, &n.outside, &n.code, &n.changes, &n.errText, &n.since}
}

func (n *nullDrift) row() driftRow {
	return driftRow{ref: n.ref.String, env: n.env.String, stack: n.stack.String, status: n.status.String, plan: n.plan.String, repo: n.repo.String,
		commit: n.commit.String, changes: n.changes.String, errText: n.errText.String, started: n.started.Int64, made: n.made.Int64,
		outside: n.outside.Int64, code: n.code.Int64, since: n.since.Int64}
}
