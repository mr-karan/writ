package control

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/mr-karan/writ/internal/sqldb"
)

// event is who or what moved a plan's status, and why, for plan_events.
type event struct {
	At              time.Time
	Person, Display string
	Run, Reason     string
}

// move changes the status of the plan ref from from to to and records the
// change in plan_events. The update is conditional on the status, so a
// caller that read the plan before another writer moved it gets a
// TransitionError and changes nothing.
func move(ctx context.Context, tx *sqldb.Tx, ref string, from, to PlanStatus, ev event) error {
	if err := mustMoves([]PlanStatus{from}, to); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, "UPDATE plans SET status = ? WHERE ref = ? AND status = ?", string(to), ref, string(from))
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return cmpErr(err, &TransitionError{Plan: ref, To: to})
	}
	if to == Superseded {
		if _, err := tx.ExecContext(ctx, "UPDATE plans SET superseded_why = ? WHERE ref = ?", ev.Reason, ref); err != nil {
			return err
		}
	}
	// The saved plan of an OpenTofu stack holds every value the plan
	// read, so it is kept only while the plan can still apply.
	if !to.Live() && to != Applying {
		if _, err := tx.ExecContext(ctx, "UPDATE plans SET saved = NULL WHERE ref = ? AND tofu_state IS NOT NULL", ref); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO plan_events (plan_ref, at, from_status, to_status, person, display, run, reason)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, ref, ev.At.UnixMilli(), string(from), string(to), ev.Person, ev.Display, ev.Run, ev.Reason)
	return err
}

// forUpdate locks the rows a query reads until the transaction ends. On
// SQLite a write transaction already holds the whole database.
func forUpdate(tx *sqldb.Tx) string {
	if tx.Postgres() {
		return " FOR UPDATE"
	}
	return ""
}

// strings1 runs a query of one text column.
func strings1(ctx context.Context, tx *sqldb.Tx, q string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// overlappingRun returns a run on stack@env, other than one of planID,
// that was applying at or after since, "" when none. A plan requested at
// since may have read state that such a run changed: the control plane is
// the only writer of state, so these runs are every change state had.
func overlappingRun(ctx context.Context, tx *sqldb.Tx, env, stack, planID string, since int64) (string, error) {
	var run string
	err := tx.QueryRowContext(ctx, `SELECT id FROM runs WHERE env = ? AND stack = ? AND plan_id <> ? AND (ended_at IS NULL OR ended_at >= ?)
		ORDER BY started_at DESC LIMIT 1`, env, stack, planID, since).Scan(&run)
	if errors.Is(err, sqldb.ErrNoRows) {
		return "", nil
	}
	return run, err
}

// expire moves every live plan past its expiry to expired.
func (s *Store) expire(ctx context.Context, now time.Time) (int64, error) {
	var n int64
	err := s.db.Write(ctx, func(tx *sqldb.Tx) error {
		rows, err := tx.QueryContext(ctx, "SELECT ref, status FROM plans WHERE status IN ('pending', 'approved') AND expires_at <= ?"+forUpdate(tx), now.UnixMilli())
		if err != nil {
			return err
		}
		type live struct {
			ref    string
			status PlanStatus
		}
		var plans []live
		for rows.Next() {
			var p live
			if err := rows.Scan(&p.ref, &p.status); err != nil {
				rows.Close()
				return err
			}
			plans = append(plans, p)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, p := range plans {
			if err := move(ctx, tx, p.ref, p.status, Expired, event{At: now, Reason: "plan_ttl passed"}); err != nil {
				return err
			}
		}
		n = int64(len(plans))
		return nil
	})
	return n, err
}

// deletePlans deletes the plans that can never be applied, requested
// before cutoff, that no run or approval names, with their events.
func (s *Store) deletePlans(ctx context.Context, cutoff time.Time) (int64, error) {
	const gone = `status IN ('refused', 'failed', 'expired', 'superseded') AND requested_at < ?
		AND NOT EXISTS (SELECT 1 FROM runs WHERE runs.plan_id = plans.id)
		AND NOT EXISTS (SELECT 1 FROM approvals WHERE approvals.plan_id = plans.id)`
	var n int64
	err := s.db.Write(ctx, func(tx *sqldb.Tx) error {
		at := cutoff.UnixMilli()
		if _, err := tx.ExecContext(ctx, "UPDATE plans SET superseded_by = NULL WHERE superseded_by IN (SELECT id FROM plans WHERE "+gone+")", at); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM plan_events WHERE plan_ref IN (SELECT ref FROM plans WHERE "+gone+")", at); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, "DELETE FROM plans WHERE "+gone, at)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}

// Approval is one person's approval of a plan.
type Approval struct {
	// ID is the approval's row, which a new approval of the same person
	// after a withdrawal does not share.
	ID      int64
	PlanID  string
	Person  string
	Display string
	Issuer  string
	Subject string
	// Via says how the person proved it: "gitlab job 9922140", "browser
	// session", "dev token".
	Via         string
	ApprovedAt  time.Time
	WithdrawnAt time.Time // zero while it counts
}

// ApprovalsDigest is the hex SHA-256 of the ids and approvers of as, in
// the order of their ids. Equal digests mean the same approvals.
func ApprovalsDigest(as []Approval) string {
	type key struct {
		ID                      int64
		Issuer, Subject, Person string
	}
	keys := make([]key, len(as))
	for i, a := range as {
		keys[i] = key{a.ID, a.Issuer, a.Subject, a.Person}
	}
	slices.SortFunc(keys, func(a, b key) int { return cmp.Compare(a.ID, b.ID) })
	b, _ := json.Marshal(keys)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Rules are the current config's approval rules. The store counts a
// plan's approvals with Tally in each place they count: an approval, a
// withdrawal, the choice of a merged plan, and the start of a run. The
// server reports a plan's status and approvals with it too.
type Rules struct {
	// Counts reports whether an approval by person counts. The server
	// counts a person of an identity its config defines. An approval kept
	// under an identity the config no longer defines may be by a person
	// who has since approved again under the identity's new name, so it
	// stops counting, and stays in the control database as history.
	Counts func(person string) bool
	// Need is the approvals the current rules ask of a plan.
	Need func(Plan) int
	// Speculative is set on a dev server, which approves and applies a
	// speculative plan, of files a client sent. A team's control plane
	// never applies one, so its status stays pending whatever its
	// approvals.
	Speculative bool
}

// Approvable reports whether p takes approvals and applies under the
// rules: a plan of a commit always, a speculative plan only on a dev
// server.
func (r Rules) Approvable(p Plan) bool { return p.Source.Commit != "" || r.Speculative }

// Tally is what a plan's approvals give it under the current rules.
type Tally struct {
	// Approvals are the approvals that count, oldest first.
	Approvals []Approval
	// Need is the larger of what the plan's rules asked when it was made
	// and what the current rules ask.
	Need int
	// Status is the plan's status under the approvals that count. A live
	// plan is approved when it is approvable and the approvals that count
	// reach Need, and pending if not, whatever status is stored: a change
	// of config can make an approval count again, or stop counting, with
	// no write. Any other status is the stored one. Every decision to
	// apply asks this status, and the next approval, withdrawal, or run of
	// the plan settles the stored one to it.
	Status PlanStatus
}

// Enough reports whether the approvals that count reach Need.
func (t Tally) Enough() bool { return len(t.Approvals) >= t.Need }

// Tally counts approvals of p, the ones not withdrawn, under the rules.
func (r Rules) Tally(p Plan, approvals []Approval) Tally {
	t := Tally{Need: max(p.Need, r.Need(p)), Status: p.Status}
	for _, a := range approvals {
		if a.WithdrawnAt.IsZero() && r.Counts(a.Person) {
			t.Approvals = append(t.Approvals, a)
		}
	}
	if p.Status.Live() {
		t.Status = Pending
		if r.Approvable(p) && t.Enough() {
			t.Status = Approved
		}
	}
	return t
}

// settle moves a live plan's stored status to status, with ev's reason,
// when the two differ.
func settle(ctx context.Context, tx *sqldb.Tx, p *Plan, status PlanStatus, ev event) error {
	if !p.Status.Live() || p.Status == status {
		return nil
	}
	if err := move(ctx, tx, p.Ref, p.Status, status, ev); err != nil {
		return err
	}
	p.Status = status
	return nil
}

// ApprovalError is an approval the store refuses: the plan is not live,
// it expired, or the approver made it.
type ApprovalError struct {
	Plan   string
	Status PlanStatus
	// Self is set when the approver made the plan.
	Self bool
	Msg  string
}

func (e *ApprovalError) Error() string { return e.Msg }

// Approve records approver's approval of each plan in ids, in one
// transaction, with the token's use and the audit row: all of them or
// none. rules count each plan's approvals. A plan whose tally is
// already approved is skipped, so a second approver's request still
// passes. Each plan's stored status moves to its tally's status. The
// author of a plan cannot approve it.
func (s *Store) Approve(ctx context.Context, approver Approval, ids []string, rules Rules, use TokenUse, e Entry) ([]Plan, error) {
	if approver.Person == "" {
		return nil, errors.New("an approval needs a person")
	}
	var out []Plan
	err := s.db.Write(ctx, func(tx *sqldb.Tx) error {
		if err := useToken(ctx, tx, use, e.At); err != nil {
			return err
		}
		now := approver.ApprovedAt
		for _, id := range ids {
			p, err := lockPlan(ctx, tx, id, now)
			if err != nil {
				return err
			}
			if why, _ := liveCheck(p); why != "" {
				return &ApprovalError{Plan: id, Status: p.Status, Msg: why}
			}
			stored, err := liveApprovals(ctx, tx, id)
			if err != nil {
				return err
			}
			t := rules.Tally(p, stored)
			ev := event{At: now, Person: approver.Person, Display: approver.Display, Reason: fmt.Sprintf("%d of %d approvals", len(t.Approvals), t.Need)}
			if t.Status == Approved {
				if err := settle(ctx, tx, &p, Approved, ev); err != nil {
					return err
				}
				out = append(out, p)
				continue
			}
			if p.Author.Person == approver.Person {
				return &ApprovalError{Plan: id, Status: p.Status, Self: true, Msg: fmt.Sprintf("%s made plan %s and cannot approve it: it needs %s from someone else", approver.Display, id, approvals(t.Need))}
			}
			res, err := tx.ExecContext(ctx, `INSERT INTO approvals (plan_id, person, display, issuer, subject, via, approved_at)
				VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
				id, approver.Person, approver.Display, approver.Issuer, approver.Subject, approver.Via, now.UnixMilli())
			if err != nil {
				return err
			}
			if added, err := res.RowsAffected(); err != nil {
				return err
			} else if added == 1 {
				stored = append(stored, approver)
			}
			t = rules.Tally(p, stored)
			ev.Reason = fmt.Sprintf("%d of %d approvals", len(t.Approvals), t.Need)
			if err := settle(ctx, tx, &p, t.Status, ev); err != nil {
				return err
			}
			out = append(out, p)
		}
		return insertAudit(ctx, tx, e)
	})
	return out, err
}

// Withdraw takes back person's approval of the plan with id. A plan that
// falls below what it needs, with its approvals counted under rules, moves
// back to pending. A plan that started applying keeps its approvals.
func (s *Store) Withdraw(ctx context.Context, id, person, display string, rules Rules, now time.Time, e Entry) (Plan, error) {
	var p Plan
	err := s.db.Write(ctx, func(tx *sqldb.Tx) error {
		var err error
		if p, err = lockPlan(ctx, tx, id, now); err != nil {
			return err
		}
		if !p.Status.Live() {
			return &ApprovalError{Plan: id, Status: p.Status, Msg: fmt.Sprintf("plan %s is %s: an approval can be withdrawn only until the plan starts applying", id, p.Status)}
		}
		res, err := tx.ExecContext(ctx, "UPDATE approvals SET withdrawn_at = ? WHERE plan_id = ? AND person = ? AND withdrawn_at IS NULL", now.UnixMilli(), id, person)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return cmpErr(err, &ApprovalError{Plan: id, Status: p.Status, Msg: fmt.Sprintf("%s has no approval of plan %s to withdraw", display, id)})
		}
		stored, err := liveApprovals(ctx, tx, id)
		if err != nil {
			return err
		}
		t := rules.Tally(p, stored)
		ev := event{At: now, Person: person, Display: display, Reason: fmt.Sprintf("approval withdrawn: %d of %d approvals", len(t.Approvals), t.Need)}
		if err := settle(ctx, tx, &p, t.Status, ev); err != nil {
			return err
		}
		return insertAudit(ctx, tx, e)
	})
	return p, err
}

// lockPlan reads the plan with id for a change, and locks its row on
// Postgres. A live plan past its expiry reads as expired.
func lockPlan(ctx context.Context, tx *sqldb.Tx, id string, now time.Time) (Plan, error) {
	p, err := scanPlan(tx.QueryRowContext(ctx, "SELECT "+planColumns+" FROM plans WHERE id = ?"+forUpdate(tx), now.UnixMilli(), id))
	if errors.Is(err, sqldb.ErrNoRows) {
		return Plan{}, ErrNotFound
	}
	return p, err
}

// liveApprovals are the approvals of a plan that were not withdrawn,
// oldest first.
func liveApprovals(ctx context.Context, tx *sqldb.Tx, id string) ([]Approval, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+approvalColumns+` FROM approvals
		WHERE plan_id = ? AND withdrawn_at IS NULL ORDER BY approved_at, id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// approvalColumns are the columns scanApproval reads, in its order.
const approvalColumns = "id, plan_id, person, display, issuer, subject, via, approved_at"

func scanApproval(rows *sqldb.Rows) (Approval, error) {
	var a Approval
	var at int64
	err := rows.Scan(&a.ID, &a.PlanID, &a.Person, &a.Display, &a.Issuer, &a.Subject, &a.Via, &at)
	a.ApprovedAt = fromMS(at)
	return a, err
}

// Approvals returns the approvals not withdrawn of each plan in ids, keyed
// by plan id, in one query. Rules.Tally says which of them count.
func (s *Store) Approvals(ctx context.Context, ids []string) (map[string][]Approval, error) {
	out := map[string][]Approval{}
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+approvalColumns+` FROM approvals
		WHERE withdrawn_at IS NULL AND plan_id IN (?`+strings.Repeat(", ?", len(ids)-1)+`) ORDER BY approved_at, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, s.db.Error(err)
		}
		out[a.PlanID] = append(out[a.PlanID], a)
	}
	return out, s.db.Error(rows.Err())
}

// PlanEvent is one change of a plan's status.
type PlanEvent struct {
	At              time.Time
	From, To        PlanStatus
	Person, Display string
	Run, Reason     string
}

// PlanEvents returns the changes of the plan ref's status, oldest first.
func (s *Store) PlanEvents(ctx context.Context, ref string) ([]PlanEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT at, from_status, to_status, person, display, run, reason FROM plan_events
		WHERE plan_ref = ? ORDER BY id`, ref)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PlanEvent
	for rows.Next() {
		var ev PlanEvent
		var at int64
		if err := rows.Scan(&at, &ev.From, &ev.To, &ev.Person, &ev.Display, &ev.Run, &ev.Reason); err != nil {
			return nil, s.db.Error(err)
		}
		ev.At = fromMS(at)
		out = append(out, ev)
	}
	return out, s.db.Error(rows.Err())
}

// Pipeline names one CI pipeline: the issuer's URL, and the project and
// pipeline ids on GitLab, or the repository and run ids on GitHub, which
// are unique only within that issuer.
type Pipeline struct {
	Issuer, ProjectID, Pipeline string
}

// ok reports whether the pipeline is named in full. One named in part
// has no plans.
func (p Pipeline) ok() bool { return p.Issuer != "" && p.ProjectID != "" && p.Pipeline != "" }

const ofPipeline = "origin_issuer = ? AND origin_project_id = ? AND origin_pipeline = ?"

// PipelinePlans returns the live plans that requests of one pipeline stand
// for, for an approval from a job of that pipeline, oldest first, at most
// limit.
func (s *Store) PipelinePlans(ctx context.Context, p Pipeline, now time.Time, limit int) ([]Plan, error) {
	if !p.ok() {
		return nil, nil
	}
	return s.scanPlans(ctx, `SELECT `+planColumns+` FROM plans WHERE `+plansOfRequests(ofPipeline)+`
		AND status IN ('pending', 'approved') AND expires_at > ? ORDER BY requested_at, ref LIMIT ?`,
		now.UnixMilli(), p.Issuer, p.ProjectID, p.Pipeline, now.UnixMilli(), limit)
}

// PipelineHas reports whether a request of pipeline p stands for the plan
// with id: the request that made it, or one whose plan was the same.
func (s *Store) PipelineHas(ctx context.Context, p Pipeline, id string) (bool, error) {
	if !p.ok() {
		return false, nil
	}
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM plans WHERE id = ? AND "+plansOfRequests(ofPipeline), id, p.Issuer, p.ProjectID, p.Pipeline).Scan(&n)
	return n > 0, s.db.Error(err)
}

// mergedPage is how many candidate plans MergedPlan reads in one query.
// It bounds the memory of one read, not the search: MergedPlan reads
// page after page until a plan qualifies or none is left. The live plans
// of one source digest are bounded by plan_ttl and by the voiding rule,
// which supersedes them all when a run on the stack@env ends.
const mergedPage = 100

// MergedPlan returns the plan an apply after a merge chooses, with its
// tally under rules: of the live commit plans of a repository's stack@env
// whose source digest is digest, the newest one whose tally is approved,
// or else the newest one. It returns ErrNotFound when none matches. The
// digest is matched in SQL, so no number of plans of other sources hides
// a match, and every live plan of the digest is weighed, so no number of
// newer plans whose approvals do not count hides an older one whose
// approvals do. BeginApply counts the chosen plan's approvals again.
func (s *Store) MergedPlan(ctx context.Context, repo, env, stack, digest string, rules Rules, now time.Time) (Plan, Tally, error) {
	var newest *Plan
	var newestTally Tally
	after := ""
	args := []any{now.UnixMilli(), repo, env, stack, digest, now.UnixMilli()}
	for {
		plans, err := s.scanPlans(ctx, `SELECT `+planColumns+` FROM plans WHERE `+plansOfRequests(`repo = ? AND env = ? AND stack = ? AND source_digest = ?
			AND source_commit <> ''`)+` AND status IN ('pending', 'approved') AND expires_at > ?`+after+`
			ORDER BY made_at DESC, ref DESC LIMIT ?`, append(args, mergedPage)...)
		if err != nil {
			return Plan{}, Tally{}, err
		}
		ids := make([]string, len(plans))
		for i, p := range plans {
			ids[i] = p.ID
		}
		stored, err := s.Approvals(ctx, ids)
		if err != nil {
			return Plan{}, Tally{}, err
		}
		for _, p := range plans {
			t := rules.Tally(p, stored[p.ID])
			if t.Status == Approved {
				return p, t, nil
			}
			if newest == nil {
				newest, newestTally = &p, t
			}
		}
		if len(plans) < mergedPage {
			break
		}
		last := plans[len(plans)-1]
		after = " AND (made_at < ? OR made_at = ? AND ref < ?)"
		args = []any{now.UnixMilli(), repo, env, stack, digest, now.UnixMilli(), last.MadeAt.UnixMilli(), last.MadeAt.UnixMilli(), last.Ref}
	}
	if newest == nil {
		return Plan{}, Tally{}, ErrNotFound
	}
	return *newest, newestTally, nil
}

// LastApplied returns the newest plan of stack@env whose run applied it,
// or ErrNotFound when none did.
func (s *Store) LastApplied(ctx context.Context, env, stack string, now time.Time) (Plan, error) {
	plans, err := s.scanPlans(ctx, `SELECT `+planColumns+` FROM plans WHERE id = (SELECT plan_id FROM runs
		WHERE env = ? AND stack = ? AND status = 'applied' ORDER BY ended_at DESC LIMIT 1)`, now.UnixMilli(), env, stack)
	if err != nil {
		return Plan{}, err
	}
	if len(plans) == 0 {
		return Plan{}, ErrNotFound
	}
	return plans[0], nil
}

// Supersede moves the live plan with id to superseded, for a check outside
// the store that found what it would change is no longer what it showed,
// such as its ref moving. A plan no longer live is left as it is.
func (s *Store) Supersede(ctx context.Context, id, reason string, now time.Time) error {
	return s.db.Write(ctx, func(tx *sqldb.Tx) error {
		p, err := lockPlan(ctx, tx, id, now)
		if err != nil || !p.Status.Live() {
			return err
		}
		return move(ctx, tx, p.Ref, p.Status, Superseded, event{At: now, Reason: reason})
	})
}

// RunStatus is where a run is.
type RunStatus string

const (
	RunApplying RunStatus = "applying"
	RunApplied  RunStatus = "applied"
	RunFailed   RunStatus = "failed"
)

// NamedBy is how a run's request named its plan.
type NamedBy string

const (
	// ByPlan named the plan's id.
	ByPlan NamedBy = "plan"
	// ByMerged named a merged commit whose sources equal the plan's.
	ByMerged NamedBy = "merged"
)

// Run is one apply the control plane ran.
type Run struct {
	ID         string
	PlanID     string
	Env, Stack string
	Status     RunStatus
	Actor      Principal
	Via        string
	// Approvals are the approvals that counted when the run started.
	Approvals []Approval
	// Override is the reason given for an apply that skipped its
	// approval rules, "" for none.
	Override     string
	NamedBy      NamedBy
	MergedCommit string
	RoleSession  string
	StartedAt    time.Time
	EndedAt      time.Time // zero while applying
	Document     json.RawMessage
	Text         string
	// Policy is the decision of the apply point, nil when no policy
	// judged the run.
	Policy json.RawMessage
}

// ApplyRequest is a run to begin.
type ApplyRequest struct {
	RunID  string
	PlanID string
	Actor  Principal
	Via    string
	// Override is the reason to apply without the approvals the rules
	// need. It is refused for a plan that has them.
	Override string
	// Rules count the plan's approvals.
	Rules        Rules
	NamedBy      NamedBy
	MergedCommit string
	RoleSession  string
	Use          TokenUse
	Now          time.Time
	Audit        Entry
	// Key is the request's key, whose started row commits with the run.
	// A key with a row already ends the transaction with ErrKeyUsed.
	Key RequestKey
	// Owner is the id of the process that begins the run, which does its
	// work.
	Owner string
	// Commit, when set, wraps the transaction's commit. Tests use it to
	// hold a commit or to fail one.
	Commit func(commit func() error) error
	// Policy is the decision of the apply point that allowed the run, nil
	// when no policy judged it.
	Policy json.RawMessage
	// PolicyApprovals is the ApprovalsDigest of the approvals the apply
	// point judged, "" when no policy judged the run. The run begins only
	// when the approvals it records have the same digest.
	PolicyApprovals string
}

// StaleError is an apply refused because the plan no longer shows what
// it would change: it expired, or state changed after it was made. The
// plan was moved to expired or superseded.
type StaleError struct {
	Plan   string
	Status PlanStatus
	Reason string
}

func (e *StaleError) Error() string { return e.Reason }

// NotApplicableError is an apply refused by the plan's status or its
// approvals. Nothing changed.
type NotApplicableError struct {
	Plan   string
	Status PlanStatus
	// Have and Need count the approvals, for a plan that lacks them.
	Have, Need int
	Msg        string
}

func (e *NotApplicableError) Error() string { return e.Msg }

// ErrBusy means another run applies to the same stack@env.
var ErrBusy = errors.New("another run is applying to this stack@env")

// ErrApprovalsChanged means the approvals that count changed after the
// apply point's policy judged them.
var ErrApprovalsChanged = errors.New("approvals changed since policy was evaluated; apply again")

// ErrCommitUnknown means the commit of BeginApply returned an error, so
// the run may have begun or not. CloseKey on the request's key tells.
var ErrCommitUnknown = errors.New("the commit of the run's start returned an error")

// BeginApply starts a run of a plan, in one transaction: it records the
// token's use, locks the plan, checks its status, expiry, and approvals,
// that they are the ones the apply point's policy judged, and that no run
// changed state since the plan was requested, moves the plan to applying
// with a conditional update, inserts the run, and writes the plan's event
// and the audit row. Two requests for one plan, or for
// two plans of one stack@env, cannot both begin: the plan's status and the
// runs table's unique indexes decide which one does.
// After it locks the plan it takes the stack@env's turn, as AdoptState
// does, so it waits for an adoption of the stack@env that holds it.
//
// A plan found stale is moved to expired or superseded, that move is
// committed, and the error is a *StaleError. The key's row and the
// token's use are deleted before that commit, so the refusal spends no
// token and the key can start a run later. Every other refusal rolls back.
//
// The key's insert is the first statement, so a request whose key has a
// row checks nothing and spends no token: the error is ErrKeyUsed. On
// Postgres the insert waits for a transaction that inserted the same key
// and has not ended, and on SQLite the write lock does the same.
// A key at or below the floor of pruned keys ends it with ErrKeyClosed.
//
// When the commit returns an error, the error wraps ErrCommitUnknown, and
// the Run is the one the transaction began if it committed.
//
// A later check that must hold when the plan starts applying, such as a
// fresh second factor, belongs in this transaction, beside the approvals.
func (s *Store) BeginApply(ctx context.Context, r ApplyRequest) (Run, error) {
	if r.Override != "" && len(r.Override) < MinOverride {
		return Run{}, fmt.Errorf("an override's reason needs at least %d characters", MinOverride)
	}
	run := Run{ID: r.RunID, PlanID: r.PlanID, Status: RunApplying, Actor: r.Actor, Via: r.Via, Override: r.Override, NamedBy: r.NamedBy,
		MergedCommit: r.MergedCommit, RoleSession: r.RoleSession, StartedAt: r.Now, Policy: r.Policy}
	var stale *StaleError
	var step sqldb.CommitStep
	if r.Commit != nil {
		step = sqldb.CommitStep(r.Commit)
	}
	err := s.db.WriteCommit(ctx, func(tx *sqldb.Tx) error {
		if err := insertKey(ctx, tx, r); err != nil {
			return err
		}
		if err := useToken(ctx, tx, r.Use, r.Now); err != nil {
			return err
		}
		p, err := lockPlan(ctx, tx, r.PlanID, r.Now)
		if err != nil {
			return err
		}
		if err := takeTurn(ctx, tx, p.Env, p.Stack); err != nil {
			return err
		}
		run.Env, run.Stack = p.Env, p.Stack
		ev := event{At: r.Now, Person: r.Actor.Person, Display: r.Actor.Display, Run: r.RunID}
		if p.Status == Expired {
			var stored PlanStatus
			if err := tx.QueryRowContext(ctx, "SELECT status FROM plans WHERE ref = ?", p.Ref).Scan(&stored); err != nil {
				return err
			}
			if stored.Live() {
				ev.Reason = "plan_ttl passed"
				if err := move(ctx, tx, p.Ref, stored, Expired, ev); err != nil {
					return err
				}
			}
			stale = &StaleError{Plan: p.ID, Status: Expired, Reason: fmt.Sprintf("plan %s expired at %s: plan again", p.ID, p.ExpiresAt.Format(time.RFC3339))}
			return spendNothing(ctx, tx, r)
		}
		// staleNow moves the plan to superseded for why and refuses the
		// apply with that move committed.
		staleNow := func(why string) error {
			ev.Run, ev.Reason = "", why
			if err := move(ctx, tx, p.Ref, p.Status, Superseded, ev); err != nil {
				return err
			}
			stale = &StaleError{Plan: p.ID, Status: Superseded, Reason: staleReason(p.ID, why)}
			return spendNothing(ctx, tx, r)
		}
		if why, newer := liveCheck(p); newer != "" {
			return staleNow(newer)
		} else if why != "" {
			return &NotApplicableError{Plan: p.ID, Status: p.Status, Msg: why}
		}
		stored, err := liveApprovals(ctx, tx, p.ID)
		if err != nil {
			return err
		}
		t := r.Rules.Tally(p, stored)
		have, need := t.Approvals, t.Need
		switch {
		case !r.Rules.Approvable(p):
			return &NotApplicableError{Plan: p.ID, Status: t.Status, Have: len(have), Need: need,
				Msg: fmt.Sprintf("plan %s is a speculative plan, of files a client sent, which a team's control plane never applies", p.ID)}
		case r.Override == "" && t.Status != Approved:
			return &NotApplicableError{Plan: p.ID, Status: t.Status, Have: len(have), Need: need,
				Msg: fmt.Sprintf("plan %s has %d of the %s its rules need", p.ID, len(have), approvals(need))}
		case r.Override != "" && t.Status == Approved:
			return &NotApplicableError{Plan: p.ID, Status: t.Status, Have: len(have), Need: need,
				Msg: fmt.Sprintf("plan %s has the %s its rules need, so an override skips nothing: apply it without one", p.ID, approvals(need))}
		case r.PolicyApprovals != "" && ApprovalsDigest(have) != r.PolicyApprovals:
			return ErrApprovalsChanged
		}
		if other, err := overlappingRun(ctx, tx, p.Env, p.Stack, p.ID, p.RequestedAt.UnixMilli()); err != nil {
			return err
		} else if other != "" {
			ev.Run, ev.Reason = other, fmt.Sprintf("run %s applied to %s@%s after the plan was requested", other, p.Stack, p.Env)
			if err := move(ctx, tx, p.Ref, p.Status, Superseded, ev); err != nil {
				return err
			}
			stale = &StaleError{Plan: p.ID, Status: Superseded, Reason: fmt.Sprintf("state of %s@%s changed after plan %s was made: %s; plan again", p.Stack, p.Env, p.ID, ev.Reason)}
			return spendNothing(ctx, tx, r)
		}
		if p.Tofu != nil {
			why, err := staleTofu(ctx, tx, p)
			if err != nil {
				return err
			}
			if why != "" {
				return staleNow(why)
			}
		}
		ev.Reason = fmt.Sprintf("%d of %d approvals", len(have), need)
		// The stored status takes the tally's first, so the plan's history
		// shows each status it had: a plan whose approvals count again after
		// a change of config moves to approved, and one whose approvals
		// stopped counting moves to pending, before it moves to applying.
		if err := settle(ctx, tx, &p, t.Status, ev); err != nil {
			return err
		}
		if r.Override != "" {
			ev.Reason = "override: " + r.Override
		}
		if err := move(ctx, tx, p.Ref, p.Status, Applying, ev); err != nil {
			return err
		}
		run.Approvals = have
		approvals, _ := json.Marshal(approvalRows(have))
		res, err := tx.ExecContext(ctx, `INSERT INTO runs (id, plan_id, env, stack, status, actor_issuer, actor_subject, actor_person, actor_display, actor,
				via, approvals, override, named_by, merged_commit, role_session, started_at, owner, policy)
			VALUES (?, ?, ?, ?, 'applying', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
			r.RunID, p.ID, p.Env, p.Stack, r.Actor.Issuer, r.Actor.Subject, r.Actor.Person, r.Actor.Display, jsonOr(r.Actor.Detail, "{}"),
			r.Via, string(approvals), r.Override, string(r.NamedBy), r.MergedCommit, r.RoleSession, r.Now.UnixMilli(), r.Owner, rawOrNil(r.Policy))
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return cmpErr(err, ErrBusy)
		}
		return insertAudit(ctx, tx, r.Audit)
	}, step)
	if _, ok := errors.AsType[*sqldb.CommitError](err); ok && stale == nil {
		return run, fmt.Errorf("%w: %w", ErrCommitUnknown, err)
	}
	if err != nil {
		return Run{}, err
	}
	if stale != nil {
		return Run{}, stale
	}
	return run, nil
}

// staleTofu says why a plan of an OpenTofu stack may no longer apply, ""
// when it may: a newer plan of its stack@env was made, by the order of
// seq, which has no ties, or the newest version of the stack's state is
// not the one the plan read. OpenTofu refuses a stale saved plan too, but
// only after it starts.
func staleTofu(ctx context.Context, tx *sqldb.Tx, p Plan) (string, error) {
	var newer string
	err := tx.QueryRowContext(ctx, `SELECT id FROM plans WHERE env = ? AND stack = ? AND ref <> ? AND id IS NOT NULL
		AND seq > (SELECT seq FROM plans WHERE ref = ?) ORDER BY seq DESC LIMIT 1`, p.Env, p.Stack, p.Ref, p.Ref).Scan(&newer)
	switch {
	case err == nil:
		return newerPlan(p, newer), nil
	case !errors.Is(err, sqldb.ErrNoRows):
		return "", err
	}
	var version int64
	err = tx.QueryRowContext(ctx, "SELECT version FROM tfstate_versions WHERE env = ? AND stack = ? ORDER BY version DESC LIMIT 1", p.Env, p.Stack).Scan(&version)
	if err != nil && !errors.Is(err, sqldb.ErrNoRows) {
		return "", err
	}
	if version != p.Tofu.StateVersion {
		return fmt.Sprintf("the state of %s@%s is at version %d, and the plan read version %d", p.Stack, p.Env, version, p.Tofu.StateVersion), nil
	}
	return "", nil
}

// liveCheck is the check Approve and BeginApply share that plan p can
// still be approved or applied: its status is pending or approved, and no
// newer, different plan of its merge request superseded it. It returns
// "" when p can, and otherwise why not. newer is set when such a plan
// superseded p, and is the reason BeginApply records as it moves p to
// superseded.
func liveCheck(p Plan) (why, newer string) {
	switch {
	case p.Status == Superseded:
		return fmt.Sprintf("plan %s is superseded: %s; plan again", p.ID, p.SupersededWhy), ""
	case !p.Status.Live():
		return fmt.Sprintf("plan %s is %s, and only a pending or approved plan is approved or applied", p.ID, p.Status), ""
	case p.SupersededBy != "" && p.SupersededBy != p.ID:
		newer = newerPlan(p, p.SupersededBy)
		return staleReason(p.ID, newer), newer
	}
	return "", ""
}

// newerPlan is the reason plan p is stale when plan id of its stack@env
// is newer.
func newerPlan(p Plan, id string) string {
	return fmt.Sprintf("plan %s of %s@%s is newer", id, p.Stack, p.Env)
}

// staleReason is the refusal of plan id, stale for why.
func staleReason(id, why string) string {
	return fmt.Sprintf("plan %s is stale: %s; plan again", id, why)
}

// spendNothing deletes what BeginApply wrote before a refusal it commits:
// the key's row and the token's use.
func spendNothing(ctx context.Context, tx *sqldb.Tx, r ApplyRequest) error {
	if err := dropKey(ctx, tx, r.Key.Key); err != nil {
		return err
	}
	return dropToken(ctx, tx, r.Use)
}

// approvals counts approvals in words: 1 approval, 2 approvals.
func approvals(n int) string {
	if n == 1 {
		return "1 approval"
	}
	return fmt.Sprintf("%d approvals", n)
}

// MinOverride is the shortest reason an override takes.
const MinOverride = 10

type approvalRow struct {
	Person  string `json:"person"`
	Display string `json:"display"`
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
	Via     string `json:"via"`
	At      int64  `json:"at"`
}

func approvalRows(as []Approval) []approvalRow {
	out := make([]approvalRow, len(as))
	for i, a := range as {
		out[i] = approvalRow{a.Person, a.Display, a.Issuer, a.Subject, a.Via, a.ApprovedAt.UnixMilli()}
	}
	return out
}

// RunResult is how a run ended.
type RunResult struct {
	Status   RunStatus // applied or failed
	EndedAt  time.Time
	Document json.RawMessage
	Text     string
}

// FinishRun records how a run ended, in one transaction: the run's row,
// its plan's move to applied or failed, the voiding rule: every other
// live plan of the stack@env is superseded, since state may have changed
// under it, and the end of the run's state tokens and locks, which a
// failed EndStateRun left. It returns the ids of the plans it superseded.
func (s *Store) FinishRun(ctx context.Context, id string, r RunResult, e Entry) ([]string, error) {
	if r.Status != RunApplied && r.Status != RunFailed {
		return nil, fmt.Errorf("a run cannot end %s", r.Status)
	}
	var voided []string
	err := s.db.Write(ctx, func(tx *sqldb.Tx) error {
		var err error
		voided, err = finishRun(ctx, tx, id, r)
		if err != nil {
			return err
		}
		if _, err := forceEndState(ctx, tx, "run_id = ?", []any{id}, r.EndedAt, endedWithLock); err != nil {
			return err
		}
		return insertAudit(ctx, tx, e)
	})
	return voided, err
}

func finishRun(ctx context.Context, tx *sqldb.Tx, id string, r RunResult) ([]string, error) {
	var planID, env, stack, person, display string
	err := tx.QueryRowContext(ctx, "SELECT plan_id, env, stack, actor_person, actor_display FROM runs WHERE id = ? AND status = 'applying'"+forUpdate(tx), id).
		Scan(&planID, &env, &stack, &person, &display)
	if errors.Is(err, sqldb.ErrNoRows) {
		return nil, fmt.Errorf("run %s: %w", id, errNotApplying)
	}
	if err != nil {
		return nil, err
	}
	var doc any
	if len(r.Document) > 0 {
		doc = string(r.Document)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE runs SET status = ?, ended_at = ?, document = ?, text = ? WHERE id = ?",
		string(r.Status), r.EndedAt.UnixMilli(), doc, r.Text, id); err != nil {
		return nil, err
	}
	var ref string
	if err := tx.QueryRowContext(ctx, "SELECT ref FROM plans WHERE id = ?", planID).Scan(&ref); err != nil {
		return nil, err
	}
	to := Applied
	if r.Status == RunFailed {
		to = Failed
	}
	if err := move(ctx, tx, ref, Applying, to, event{At: r.EndedAt, Person: person, Display: display, Run: id, Reason: "run " + string(r.Status)}); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT ref, coalesce(id, ''), status FROM plans WHERE env = ? AND stack = ? AND status IN ('pending', 'approved') AND ref <> ?"+forUpdate(tx),
		env, stack, ref)
	if err != nil {
		return nil, err
	}
	type live struct {
		ref, id string
		status  PlanStatus
	}
	var others []live
	for rows.Next() {
		var l live
		if err := rows.Scan(&l.ref, &l.id, &l.status); err != nil {
			rows.Close()
			return nil, err
		}
		others = append(others, l)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	var voided []string
	for _, o := range others {
		ev := event{At: r.EndedAt, Run: id, Reason: fmt.Sprintf("run %s %s plan %s to %s@%s", id, r.Status, planID, stack, env)}
		if err := move(ctx, tx, o.ref, o.status, Superseded, ev); err != nil {
			return nil, err
		}
		voided = append(voided, o.id)
	}
	return voided, nil
}

// endedWithLock is why FinishRun and FinishPlan release a lock: the
// run's own end of its state access failed.
const endedWithLock = "its run ended while it held the lock"

// errNotApplying means a run already ended.
var errNotApplying = errors.New("the run is not applying")

// FailApplying ends each run left applying with no process to do its work
// as failed, with the voiding rule, and returns the runs it ended. A run
// has no such process when owner, the caller's process, did not begin it,
// or began it and handled reports that it has no work for the run. A
// process calls it at its start and then at intervals: a transaction of an
// earlier process can commit a run after this one started, and this
// process can lose a run whose start or end it could not record. The
// state's journal holds what each run did: the next plan reads what was
// left in doubt. Each run it ends loses its state tokens and locks, as
// EndOrphanState releases them.
func (s *Store) FailApplying(ctx context.Context, now time.Time, owner string, handled func(run string) bool) ([]string, error) {
	var ended []string
	err := s.db.Write(ctx, func(tx *sqldb.Tx) error {
		rows, err := tx.QueryContext(ctx, "SELECT id, owner FROM runs WHERE status = 'applying'")
		if err != nil {
			return err
		}
		type left struct{ id, owner string }
		var runs []left
		for rows.Next() {
			var l left
			if err := rows.Scan(&l.id, &l.owner); err != nil {
				rows.Close()
				return err
			}
			runs = append(runs, l)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, l := range runs {
			why := "the control plane stopped during this run; the next plan reads what it left in doubt"
			if l.owner == owner {
				if handled(l.id) {
					continue
				}
				why = "the control plane lost this run: it could not record the run's start or end; the next plan reads what it left in doubt"
			}
			doc, _ := json.Marshal(map[string]any{"format": "writ.apply/v0", "ok": false, "applied": false, "status": "apply-failed",
				"diagnostics": []map[string]string{{"severity": "error", "class": "state", "message": why}}})
			r := RunResult{Status: RunFailed, EndedAt: now, Document: doc, Text: "writ: " + why + "\n"}
			_, err := finishRun(ctx, tx, l.id, r)
			if errors.Is(err, errNotApplying) {
				continue
			}
			if err != nil {
				return err
			}
			if _, err := forceEndState(ctx, tx, "run_id = ?", []any{l.id}, now, "the run failed with no process to end it"); err != nil {
				return err
			}
			ended = append(ended, l.id)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ended, nil
}

// runFacts are the columns of a run before its document and text.
const runFacts = `id, plan_id, env, stack, status, actor_issuer, actor_subject, actor_person, actor_display, actor, via, approvals, override,
	named_by, merged_commit, role_session, started_at, ended_at`

const runColumns = runFacts + `, coalesce(document, 'null'), coalesce(text, ''), coalesce(policy, 'null')`

func scanRun(r scanner) (Run, error) {
	var run Run
	var actor, approvals, doc, policy string
	var started int64
	var ended sqldb.NullInt64
	err := r.Scan(&run.ID, &run.PlanID, &run.Env, &run.Stack, &run.Status, &run.Actor.Issuer, &run.Actor.Subject, &run.Actor.Person,
		&run.Actor.Display, &actor, &run.Via, &approvals, &run.Override, &run.NamedBy, &run.MergedCommit, &run.RoleSession, &started, &ended, &doc, &run.Text, &policy)
	if err != nil {
		return Run{}, err
	}
	var rows []approvalRow
	if err := json.Unmarshal([]byte(approvals), &rows); err != nil {
		return Run{}, fmt.Errorf("read run %s's approvals: %w", run.ID, err)
	}
	for _, a := range rows {
		run.Approvals = append(run.Approvals, Approval{PlanID: run.PlanID, Person: a.Person, Display: a.Display, Issuer: a.Issuer, Subject: a.Subject, Via: a.Via, ApprovedAt: fromMS(a.At)})
	}
	run.Actor.Detail = json.RawMessage(actor)
	run.StartedAt = fromMS(started)
	if ended.Valid {
		run.EndedAt = fromMS(ended.Int64)
	}
	if doc != "null" {
		run.Document = json.RawMessage(doc)
	}
	if policy != "null" {
		run.Policy = json.RawMessage(policy)
	}
	return run, nil
}

// GetRun returns the run with id.
func (s *Store) GetRun(ctx context.Context, id string) (Run, error) {
	run, err := scanRun(s.db.QueryRowContext(ctx, "SELECT "+runColumns+" FROM runs WHERE id = ?", id))
	if errors.Is(err, sqldb.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	return run, err
}

// ListedRun is a run as a list of runs shows it, with the source digest
// of the plan it applied, "" once the plan is pruned.
type ListedRun struct {
	Run
	Sources string
}

// Runs lists the runs of env, and only of stack when it is not "", newest
// first, at most limit of them, without their documents and texts. Only
// runs that keep accepts count, so a run the caller may not see neither
// takes a place in the limit nor shows in more. more reports whether
// older runs that keep accepts were left out.
func (s *Store) Runs(ctx context.Context, env, stack string, limit int, keep func(Run) bool) (runs []ListedRun, more bool, err error) {
	query := "SELECT " + runFacts + `, 'null', '', 'null', coalesce((SELECT source_digest FROM plans WHERE plans.id = runs.plan_id), '')
		FROM runs WHERE env = ?`
	args := []any{env}
	if stack != "" {
		query, args = query+" AND stack = ?", append(args, stack)
	}
	rows, err := s.db.QueryContext(ctx, query+" ORDER BY started_at DESC, id DESC", args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	for len(runs) <= limit && rows.Next() {
		var r ListedRun
		if r.Run, err = scanRun(withSources{rows, &r.Sources}); err != nil {
			return nil, false, s.db.Error(err)
		}
		if keep(r.Run) {
			runs = append(runs, r)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, s.db.Error(err)
	}
	if len(runs) > limit {
		return runs[:limit], true, nil
	}
	return runs, false, nil
}

// FailedRuns returns the runs of env that failed and ended after since,
// newest first, at most limit of them, without their documents and
// texts, and how many of them keep accepts in all.
func (s *Store) FailedRuns(ctx context.Context, env string, since time.Time, limit int, keep func(Run) bool) (runs []Run, total int, err error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+runFacts+`, 'null', '', 'null' FROM runs
		WHERE env = ? AND status = ? AND ended_at > ? ORDER BY ended_at DESC, id DESC`, env, string(RunFailed), since.UnixMilli())
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, 0, s.db.Error(err)
		}
		if !keep(run) {
			continue
		}
		total++
		if len(runs) < limit {
			runs = append(runs, run)
		}
	}
	return runs, total, s.db.Error(rows.Err())
}

// PlanRuns returns the run of each plan of ids that has one, by plan id,
// without its document and texts.
func (s *Store) PlanRuns(ctx context.Context, ids []string) (map[string]Run, error) {
	out := map[string]Run{}
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+runFacts+`, 'null', '', 'null' FROM runs WHERE plan_id IN (?`+strings.Repeat(", ?", len(ids)-1)+")", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, s.db.Error(err)
		}
		out[run.PlanID] = run
	}
	return out, s.db.Error(rows.Err())
}

// withSources scans a run's columns and then the digest after them.
type withSources struct {
	r       scanner
	sources *string
}

func (w withSources) Scan(dest ...any) error { return w.r.Scan(append(dest, w.sources)...) }

// PlanRun returns the run of the plan with id, ErrNotFound when it has
// none: a plan applies at most once.
func (s *Store) PlanRun(ctx context.Context, planID string) (Run, error) {
	run, err := scanRun(s.db.QueryRowContext(ctx, "SELECT "+runColumns+" FROM runs WHERE plan_id = ?", planID))
	if errors.Is(err, sqldb.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	return run, err
}
