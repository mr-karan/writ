package control

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/mr-karan/writ/internal/sqldb"
)

// AuditScan is the most audit rows one call of AuditRows reads. A caller
// that may see few rows gets a short page with next set, rather than a
// read of the whole log.
const AuditScan = 5000

// AuditFilter narrows AuditRows. Empty fields match every row.
type AuditFilter struct {
	// Plan matches the rows that name the plan by its id or its ref, and
	// the rows of its run, such as the run's state writes.
	Plan string
	Run  string
	// Env matches the rows of one environment, and Stack, with it, the
	// rows that name the stack there.
	Env, Stack string
	Person     string
	Op         string
	// Since keeps rows at or after it, Until rows before it.
	Since, Until time.Time
	Limit        int
	// Cursor is the opaque value AuditRows returned as next.
	Cursor string
	// Keep, when set, drops each row it reports false for before the
	// limit, so a row it drops neither takes a place nor shows in the
	// page. It sees a row before its teams, addrs, risks, and grants are
	// read.
	Keep func(Entry) bool
}

// AuditRows lists audit rows newest first. next is the cursor of the
// following page, "" on the last. A page holds fewer than f.Limit rows
// with next set when AuditScan rows were read before the page filled.
func (s *Store) AuditRows(ctx context.Context, f AuditFilter) (entries []Entry, next string, err error) {
	where := []string{"1 = 1"}
	var args []any
	add := func(cond string, v ...any) {
		where, args = append(where, cond), append(args, v...)
	}
	if f.Plan != "" {
		keys, runOf, err := s.planKeys(ctx, f.Plan)
		if err != nil {
			return nil, "", err
		}
		cond := "plan IN (" + marks(len(keys)) + ")"
		v := make([]any, 0, len(keys)+1)
		for _, k := range keys {
			v = append(v, k)
		}
		if runOf != "" {
			cond += " OR run IN (SELECT id FROM runs WHERE plan_id = ?)"
			v = append(v, runOf)
		}
		add("("+cond+")", v...)
	}
	if f.Run != "" {
		add("run = ?", f.Run)
	}
	if f.Env != "" {
		add("env = ?", f.Env)
	}
	if f.Stack != "" {
		add("id IN (SELECT audit_id FROM audit_stacks WHERE stack = ?)", f.Stack)
	}
	if f.Person != "" {
		add("person = ?", f.Person)
	}
	if f.Op != "" {
		add("op = ?", f.Op)
	}
	if !f.Since.IsZero() {
		add("at >= ?", f.Since.UnixMilli())
	}
	if !f.Until.IsZero() {
		add("at < ?", f.Until.UnixMilli())
	}
	if f.Cursor != "" {
		at, rest, err := decodeCursor(f.Cursor)
		if err != nil {
			return nil, "", err
		}
		id, err := strconv.ParseInt(rest, 10, 64)
		if err != nil {
			return nil, "", ErrBadCursor
		}
		add("(at < ? OR at = ? AND id < ?)", at, at, id)
	}
	scan := AuditScan
	if f.Keep == nil {
		scan = f.Limit + 1
	}
	q := `SELECT id, at, request, kind, method, route, status, remote, issuer, subject, person, principal, principal_kind, via, teams, op, env,
		stacks, plan, run, addrs, risks, decision, check_failed, reason, grants, coalesce(config_id, 0), elapsed_ms FROM audit WHERE ` +
		strings.Join(where, " AND ") + " ORDER BY at DESC, id DESC LIMIT ?"
	rows, err := s.db.QueryContext(ctx, q, append(args, scan)...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	// lastID and lastAt are of the row read last, keptID of the row the
	// page took last.
	var lastID, lastAt, keptID int64
	read := 0
	for rows.Next() {
		var e Entry
		var id, at, ms int64
		var d, teams, stacks, addrs, risks, grants string
		err := rows.Scan(&id, &at, &e.Request, &e.Kind, &e.Method, &e.Route, &e.Status, &e.Remote, &e.Issuer, &e.Subject, &e.Person, &e.Principal,
			&e.PrincipalKind, &e.Via, &teams, &e.Op, &e.Env, &stacks, &e.Plan, &e.Run, &addrs, &risks, &d, &e.Check, &e.Reason, &grants, &e.ConfigID, &ms)
		if err != nil {
			return nil, "", err
		}
		read++
		lastID, lastAt = id, at
		e.At, e.Decision, e.Elapsed = fromMS(at), Decision(d), time.Duration(ms)*time.Millisecond
		if e.Stacks, err = unlist[string](stacks); err != nil {
			return nil, "", err
		}
		if f.Keep != nil && !f.Keep(e) {
			continue
		}
		if len(entries) == f.Limit {
			return entries, encodeCursor(entries[len(entries)-1].At, strconv.FormatInt(keptID, 10)), rows.Close()
		}
		if e.Teams, err = unlist[string](teams); err != nil {
			return nil, "", err
		}
		if e.Addrs, err = unlist[string](addrs); err != nil {
			return nil, "", err
		}
		if e.Risks, err = unlist[string](risks); err != nil {
			return nil, "", err
		}
		if e.Grants, err = unlist[int](grants); err != nil {
			return nil, "", err
		}
		entries, keptID = append(entries, e), id
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if read == scan && f.Keep != nil {
		next = encodeCursor(fromMS(lastAt), strconv.FormatInt(lastID, 10))
	}
	return entries, next, nil
}

// planKeys are the values an audit row's plan column holds for the plan
// key names, its id or its ref: both when the plan is kept, and key alone
// when it is not. runOf is the plan's id when it has one, for the rows of
// its run.
func (s *Store) planKeys(ctx context.Context, key string) (keys []string, runOf string, err error) {
	var ref string
	var id sqldb.NullString
	err = s.db.QueryRowContext(ctx, "SELECT ref, id FROM plans WHERE ref = ? OR id = ?", key, key).Scan(&ref, &id)
	switch {
	case errors.Is(err, sqldb.ErrNoRows):
		return []string{key}, "", nil
	case err != nil:
		return nil, "", err
	case id.String == "":
		return []string{ref}, "", nil
	}
	return []string{ref, id.String}, id.String, nil
}

// marks is n comma-separated placeholders.
func marks(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}
