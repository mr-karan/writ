package control

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mr-karan/writ/internal/authz"
	"github.com/mr-karan/writ/internal/sqldb"
)

// RecordConfig stores the config text with its teams and grants, once for
// each distinct text, and returns its id. A start with a text already
// stored moves its last_loaded time.
func (s *Store) RecordConfig(ctx context.Context, text, serverURL, loadedBy string, p *authz.Policy, now time.Time) (int64, error) {
	sum := sha256.Sum256([]byte(text))
	digest := hex.EncodeToString(sum[:])
	var id int64
	err := s.db.Write(ctx, func(tx *sqldb.Tx) error {
		err := tx.QueryRowContext(ctx, `UPDATE configs SET last_loaded = ?, loaded_by = ? WHERE digest = ? RETURNING id`, now.UnixMilli(), loadedBy, digest).Scan(&id)
		if !errors.Is(err, sqldb.ErrNoRows) {
			return err
		}
		if err := tx.QueryRowContext(ctx, `INSERT INTO configs (digest, url, text, first_loaded, last_loaded, loaded_by)
			VALUES (?, ?, ?, ?, ?, ?) RETURNING id`, digest, serverURL, text, now.UnixMilli(), now.UnixMilli(), loadedBy).Scan(&id); err != nil {
			return err
		}
		return recordPolicy(ctx, tx, id, p)
	})
	return id, err
}

func recordPolicy(ctx context.Context, tx *sqldb.Tx, id int64, p *authz.Policy) error {
	for _, t := range p.Teams {
		type rule struct {
			Claim  string   `json:"claim"`
			Values []string `json:"values"`
		}
		rules := make([]rule, len(t.Match))
		for i, r := range t.Match {
			rules[i] = rule{r.Claim, r.Values}
		}
		match, _ := json.Marshal(rules)
		if _, err := tx.ExecContext(ctx, "INSERT INTO teams (config_id, name, issuer, match) VALUES (?, ?, ?, ?)", id, t.Name, t.Issuer, string(match)); err != nil {
			return err
		}
	}
	for i, g := range p.Grants {
		can := make([]string, len(g.Can))
		for j, op := range g.Can {
			can[j] = string(op)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO grants (config_id, ordinal, stacks, envs, can, risks) VALUES (?, ?, ?, ?, ?, ?)",
			id, i, list(g.Stacks), list(g.Envs), list(can), list(g.Risks)); err != nil {
			return err
		}
		for _, team := range uniq(g.Teams) {
			if _, err := tx.ExecContext(ctx, "INSERT INTO grant_teams (config_id, ordinal, team) VALUES (?, ?, ?)", id, i, team); err != nil {
				return err
			}
		}
	}
	return nil
}

// list is a list as the store keeps it: a JSON array, [] for none.
func list[T any](xs []T) string {
	if len(xs) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(xs)
	return string(b)
}

// unlist reads a list the store kept.
func unlist[T any](s string) ([]T, error) {
	out := []T{}
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("read a list from the control database: %w", err)
	}
	return out, nil
}

func uniq(xs []string) []string {
	var out []string
	for _, x := range xs {
		if !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}

// Principal is who made a plan, as the plans table keeps it.
type Principal struct {
	Issuer  string
	Subject string
	// Person is the stable id of the person behind the principal, such as
	// gitlab.com/user/1001, or "" when the token names none.
	Person  string
	Display string
	// Detail is the principal as a JSON document, for the author column.
	Detail json.RawMessage
}

// Plan is one kept plan.
type Plan struct {
	Ref          string
	ID           string // "" while planning
	Status       PlanStatus
	Env, Stack   string
	Author       Principal
	Teams        []string
	Origin       json.RawMessage
	RequestedAt  time.Time
	MadeAt       time.Time // zero while planning
	ExpiresAt    time.Time // zero until pending
	Risks        []string
	Changes      int
	SourceDigest string
	ConfigID     int64
	Document     json.RawMessage
	Text         string
	// SupersededBy is the id of a newer pending plan of the same stack@env
	// from the same merge request, "" when none. On a request whose plan
	// was the same as an earlier request's, which has no id, it is that
	// plan's id.
	SupersededBy string
	// Source is the commit the plan was made from. Its zero value is a
	// plan of sources the client sent.
	Source PlanSource
	// Need is how many approvals the rules asked for when the plan was
	// made, and NeedRules the 1-based positions of those rules.
	Need      int
	NeedRules []int
	// SupersededWhy says why the plan was superseded, "" otherwise.
	SupersededWhy string
	// Tofu is set on a plan of an OpenTofu stack, nil on one of Writ's
	// engine.
	Tofu *TofuPlan
	// Policy is the decision of the plan point, nil when no policy judged
	// the plan.
	Policy json.RawMessage
	// Network is the writ.network/v1 of the plan's run, nil when it had
	// no egress proxy.
	Network json.RawMessage
}

// TofuPlan is what a plan of an OpenTofu stack adds to a plan.
type TofuPlan struct {
	// StateVersion is the version of the stack's state on the http
	// backend that the plan read, 0 when it had none. An apply begins
	// only while it is still the newest.
	StateVersion int64
}

// PlanSource is the commit of a repository a plan was made from, and the
// ref it was asked for by, such as refs/heads/main, with how the ref was
// checked.
type PlanSource struct {
	Repo, Ref, Commit string
	RefCheck          RefCheck
}

// RefCheck is how a commit plan's ref was checked.
type RefCheck string

const (
	// OnRef means the forge showed, when the plan was made, that the
	// commit is the ref's commit or an ancestor of it.
	OnRef RefCheck = "on-ref"
	// NoRef means the plan names no ref, so there was nothing to check.
	NoRef RefCheck = "no-ref"
)

// TokenUse is a write a token id makes, kept until the token expires. The
// zero TokenUse records nothing, for a principal whose token has no id,
// such as the dev control plane's.
type TokenUse struct {
	Issuer, JTI, Op string
	Expires         time.Time
}

// CreatePlan stores a plan in status planning, records the token's use,
// and writes the audit row of the request, in one transaction. A token id
// that already made a plan returns ErrReplay and stores nothing.
func (s *Store) CreatePlan(ctx context.Context, p Plan, use TokenUse, e Entry) error {
	o, err := parseOrigin(p.Origin)
	if err != nil {
		return err
	}
	return s.db.Write(ctx, func(tx *sqldb.Tx) error {
		if err := useToken(ctx, tx, use, e.At); err != nil {
			return err
		}
		var seq int64
		err := tx.QueryRowContext(ctx, `INSERT INTO plan_seqs (env, stack, seq) VALUES (?, ?, 1)
			ON CONFLICT (env, stack) DO UPDATE SET seq = plan_seqs.seq + 1 RETURNING seq`, p.Env, p.Stack).Scan(&seq)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO plans (ref, status, env, stack, seq, author_issuer, author_subject, author_person, author_display, author,
				teams, origin, origin_issuer, origin_project_id, origin_pipeline, origin_merge_request, requested_at, source_digest, config_id, repo, source_ref, source_commit, source_ref_check)
			VALUES (?, 'planning', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			p.Ref, p.Env, p.Stack, seq, p.Author.Issuer, p.Author.Subject, p.Author.Person, p.Author.Display, jsonOr(p.Author.Detail, "{}"),
			list(p.Teams), jsonOr(p.Origin, "{}"), o.Issuer, o.ProjectID, o.Pipeline, o.mergeRequest(), p.RequestedAt.UnixMilli(), p.SourceDigest, p.ConfigID,
			p.Source.Repo, p.Source.Ref, p.Source.Commit, string(p.Source.RefCheck))
		if err != nil {
			return err
		}
		return insertAudit(ctx, tx, e)
	})
}

// origin is the part of a plan's origin document that queries filter on.
// A GitHub run's repository and run fill the project and pipeline, so one
// query finds the requests of a CI run of either forge.
type origin struct {
	Kind         string `json:"kind"`
	Issuer       string `json:"issuer"`
	ProjectID    string `json:"project_id"`
	Pipeline     string `json:"pipeline"`
	MergeRequest int64  `json:"merge_request"`
	RepositoryID string `json:"repository_id"`
	Run          string `json:"run"`
}

func parseOrigin(doc json.RawMessage) (origin, error) {
	var o origin
	if len(doc) == 0 {
		return o, nil
	}
	if err := json.Unmarshal(doc, &o); err != nil {
		return o, fmt.Errorf("a plan's origin is not the document the control database keeps: %w", err)
	}
	if o.Kind == "github-run" {
		o.ProjectID, o.Pipeline = o.RepositoryID, o.Run
	}
	return o, nil
}

// mergeRequest is the merge request column: NULL for a plan of none.
func (o origin) mergeRequest() any {
	if o.MergeRequest == 0 {
		return nil
	}
	return o.MergeRequest
}

func useToken(ctx context.Context, tx *sqldb.Tx, use TokenUse, now time.Time) error {
	if use == (TokenUse{}) {
		return nil
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM token_uses WHERE exp < ?", now.UnixMilli()); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO token_uses (issuer, jti, op, exp, used_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT DO NOTHING`, use.Issuer, use.JTI, use.Op, use.Expires.UnixMilli(), now.UnixMilli())
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return cmpErr(err, ErrReplay)
	}
	return nil
}

// dropToken deletes the use that useToken recorded earlier in the same
// transaction, for a refusal that commits.
func dropToken(ctx context.Context, tx *sqldb.Tx, use TokenUse) error {
	if use == (TokenUse{}) {
		return nil
	}
	_, err := tx.ExecContext(ctx, "DELETE FROM token_uses WHERE issuer = ? AND jti = ? AND op = ?", use.Issuer, use.JTI, use.Op)
	return err
}

// cmpErr is err, or other when err is nil.
func cmpErr(err, other error) error {
	if err != nil {
		return err
	}
	return other
}

func jsonOr(b json.RawMessage, empty string) string {
	if len(b) == 0 {
		return empty
	}
	return string(b)
}

// rawOrNil is a JSON document as text, or NULL for none.
func rawOrNil(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

// msOrNil is a time as the store keeps it: NULL for the zero time.
func msOrNil(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

// PlanResult is how a plan ended.
type PlanResult struct {
	// ID is the saved plan's signature, "" for a plan that could not be
	// saved.
	ID string
	// Status is Pending, Refused, or Failed. A pending plan whose rules
	// need no approval is stored Approved, and one an apply on its
	// stack@env overlapped is stored Superseded.
	Status    PlanStatus
	MadeAt    time.Time
	ExpiresAt time.Time
	Risks     []string
	Changes   int
	Document  json.RawMessage
	Text      string
	Saved     []byte
	// SourceDigest replaces the plan's digest when set: a plan of a commit
	// knows it only once the commit is read.
	SourceDigest string
	// Need and NeedRules are what the approval rules ask of the plan.
	Need      int
	NeedRules []int
	// Tofu is set on a plan of an OpenTofu stack.
	Tofu *TofuPlan
	// Policy is the decision of the plan point, nil when no policy judged
	// the plan.
	Policy json.RawMessage
	// Network is the writ.network/v1 of the plan's run, nil when it had
	// no egress proxy.
	Network json.RawMessage
}

// FinishPlan records how a plan ended, ends its state tokens, and writes
// its audit row, in one transaction. A plan that would be live is
// superseded instead when an apply on its stack@env ran while it
// planned: it may have read state that apply was changing. A plan whose
// id another request's plan already has is that plan, so the request is
// superseded by it and reads as it.
func (s *Store) FinishPlan(ctx context.Context, ref string, r PlanResult, e Entry) error {
	switch r.Status {
	case Pending, Approved, RefusedPlan, Failed:
	default:
		return fmt.Errorf("a plan cannot end %s", r.Status)
	}
	return s.db.Write(ctx, func(tx *sqldb.Tx) error {
		var env, stack string
		var requested int64
		err := tx.QueryRowContext(ctx, "SELECT env, stack, requested_at FROM plans WHERE ref = ? AND status = 'planning'"+forUpdate(tx), ref).
			Scan(&env, &stack, &requested)
		if errors.Is(err, sqldb.ErrNoRows) {
			return fmt.Errorf("plan %s is not planning", ref)
		}
		if err != nil {
			return err
		}
		ev := event{At: r.MadeAt, Person: e.Person, Display: e.Principal}
		var same string
		if r.ID != "" {
			err := tx.QueryRowContext(ctx, "SELECT ref FROM plans WHERE id = ?", r.ID).Scan(&same)
			if err != nil && !errors.Is(err, sqldb.ErrNoRows) {
				return err
			}
		}
		if same != "" {
			// OpenTofu wrote the same saved plan and document from the same
			// commit as request same, so it is that plan: this request
			// points at it, and its approvals and policy decision count for
			// both. Its digest is stored as on any request, since a lookup
			// of the plan by this request's repository matches the
			// request's own digest.
			if _, err := tx.ExecContext(ctx, `UPDATE plans SET made_at = ?, superseded_by = ?,
					source_digest = CASE WHEN ? = '' THEN source_digest ELSE ? END,
					policy = (SELECT policy FROM plans WHERE ref = ?), network = ? WHERE ref = ?`,
				r.MadeAt.UnixMilli(), r.ID, r.SourceDigest, r.SourceDigest, same, rawOrNil(r.Network), ref); err != nil {
				return err
			}
			ev.Reason = "the same plan as " + same
			if err := move(ctx, tx, ref, Planning, Superseded, ev); err != nil {
				return err
			}
			e.Reason = string(Superseded)
		} else if err := finishOwn(ctx, tx, ref, env, stack, requested, r, ev); err != nil {
			return err
		}
		if _, err := forceEndState(ctx, tx, "run_id = ?", []any{ref}, r.MadeAt, endedWithLock); err != nil {
			return err
		}
		return insertAudit(ctx, tx, e)
	})
}

// finishOwn records the end of request ref as a plan of its own.
func finishOwn(ctx context.Context, tx *sqldb.Tx, ref, env, stack string, requested int64, r PlanResult, ev event) error {
	var id any
	if r.ID != "" {
		id = r.ID
	}
	var doc any
	if len(r.Document) > 0 {
		doc = string(r.Document)
	}
	to := r.Status
	if to.Live() {
		run, err := overlappingRun(ctx, tx, env, stack, "", requested)
		if err != nil {
			return err
		}
		if run != "" {
			to, ev.Run, ev.Reason = Superseded, run, fmt.Sprintf("run %s applied to %s@%s while the plan was made", run, stack, env)
		}
	}
	var tofuState any
	if r.Tofu != nil {
		tofuState = r.Tofu.StateVersion
	}
	_, err := tx.ExecContext(ctx, `UPDATE plans SET id = ?, made_at = ?, expires_at = ?, risks = ?, changes = ?,
				document = ?, text = ?, saved = ?, source_digest = CASE WHEN ? = '' THEN source_digest ELSE ? END,
				need = ?, need_rules = ?, lock = ?, tofu_state = ?, policy = ?, network = ?
			WHERE ref = ?`,
		id, r.MadeAt.UnixMilli(), msOrNil(r.ExpiresAt), list(r.Risks), r.Changes, doc, r.Text, r.Saved, r.SourceDigest, r.SourceDigest,
		r.Need, list(r.NeedRules), "", tofuState, rawOrNil(r.Policy), rawOrNil(r.Network), ref)
	if err != nil {
		return err
	}
	if err := move(ctx, tx, ref, Planning, to, ev); err != nil {
		return err
	}
	if r.ID != "" {
		if err := supersede(ctx, tx, ref, r.ID); err != nil {
			return err
		}
	}
	return nil
}

// supersede points the older plans of ref's stack@env from the same merge
// request at the plan with id.
func supersede(ctx context.Context, tx *sqldb.Tx, ref, id string) error {
	var env, stack, issuer, project string
	var mr sqldb.NullInt64
	var at int64
	err := tx.QueryRowContext(ctx, "SELECT env, stack, origin_issuer, origin_project_id, origin_merge_request, requested_at FROM plans WHERE ref = ?", ref).
		Scan(&env, &stack, &issuer, &project, &mr, &at)
	if err != nil || !mr.Valid || issuer == "" || project == "" {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE plans SET superseded_by = ?
		WHERE ref <> ? AND id IS NOT NULL AND superseded_by IS NULL AND env = ? AND stack = ? AND requested_at <= ?
		AND origin_issuer = ? AND origin_project_id = ? AND origin_merge_request = ?`, id, ref, env, stack, at, issuer, project, mr.Int64)
	return err
}

// FailPlanning marks every plan left planning as failed, for a start after
// a crash, and returns how many it marked.
func (s *Store) FailPlanning(ctx context.Context, now time.Time) (int64, error) {
	var n int64
	err := s.db.Write(ctx, func(tx *sqldb.Tx) error {
		refs, err := strings1(ctx, tx, "SELECT ref FROM plans WHERE status = 'planning'")
		if err != nil {
			return err
		}
		for _, ref := range refs {
			if _, err := tx.ExecContext(ctx, "UPDATE plans SET made_at = ? WHERE ref = ?", now.UnixMilli(), ref); err != nil {
				return err
			}
			if err := move(ctx, tx, ref, Planning, Failed, event{At: now, Reason: "the control plane stopped while the plan was made"}); err != nil {
				return err
			}
		}
		n = int64(len(refs))
		return nil
	})
	return n, err
}

// planColumns are a plan's columns; the query's first parameter is the time
// against which a pending plan reads as expired.
const planColumns = `ref, coalesce(id, ''),
	` + liveStatus + `,
	env, stack, author_issuer, author_subject, author_person, author_display, author, teams, origin,
	requested_at, made_at, expires_at, risks, changes, source_digest, config_id, coalesce(document, 'null'), coalesce(text, ''),
	coalesce(superseded_by, ''), repo, source_ref, source_commit, source_ref_check, need, need_rules, superseded_why, tofu_state,
	coalesce(policy, 'null'), coalesce(network, 'null')`

// liveStatus is a plan's status as a reader sees it: a live plan past its
// expiry reads as expired before Prune marks it. Its parameter is the time.
const liveStatus = `CASE WHEN status IN ('pending', 'approved') AND expires_at <= ? THEN 'expired' ELSE status END`

// scanner is a row or a set of rows.
type scanner interface{ Scan(dest ...any) error }

func scanPlan(r scanner) (Plan, error) {
	var p Plan
	var made, expires, tofuState sqldb.NullInt64
	var requested int64
	var author, teams, origin, risks, doc, needRules, policy, network string
	err := r.Scan(&p.Ref, &p.ID, &p.Status, &p.Env, &p.Stack, &p.Author.Issuer, &p.Author.Subject, &p.Author.Person, &p.Author.Display,
		&author, &teams, &origin, &requested, &made, &expires, &risks, &p.Changes, &p.SourceDigest, &p.ConfigID, &doc, &p.Text, &p.SupersededBy,
		&p.Source.Repo, &p.Source.Ref, &p.Source.Commit, &p.Source.RefCheck, &p.Need, &needRules, &p.SupersededWhy, &tofuState, &policy, &network)
	if err != nil {
		return Plan{}, err
	}
	if tofuState.Valid {
		p.Tofu = &TofuPlan{StateVersion: tofuState.Int64}
	}
	if p.NeedRules, err = unlist[int](needRules); err != nil {
		return Plan{}, err
	}
	if p.Teams, err = unlist[string](teams); err != nil {
		return Plan{}, err
	}
	if p.Risks, err = unlist[string](risks); err != nil {
		return Plan{}, err
	}
	p.Author.Detail, p.Origin = json.RawMessage(author), json.RawMessage(origin)
	if doc != "null" {
		p.Document = json.RawMessage(doc)
	}
	if policy != "null" {
		p.Policy = json.RawMessage(policy)
	}
	if network != "null" {
		p.Network = json.RawMessage(network)
	}
	if made.Valid {
		p.MadeAt = fromMS(made.Int64)
	}
	if expires.Valid {
		p.ExpiresAt = fromMS(expires.Int64)
	}
	p.RequestedAt = fromMS(requested)
	return p, nil
}

// requestPlan is the SQL of the id of the plan a request's row stands for.
// FinishPlan stores a request whose plan was the same as an earlier
// request's with no id of its own and that plan's id in superseded_by. A
// plan superseded by a newer, different plan keeps its id, so it stands
// for itself. A request still planning, or that ended with no plan, stands
// for none.
const requestPlan = "coalesce(id, superseded_by)"

// plansOfRequests is the SQL condition that a plan is one that a request
// matching the condition where stands for. Every lookup of a plan by its
// requests, such as by pipeline or commit, goes through it, so a request
// whose plan was the same as another's finds that plan.
func plansOfRequests(where string) string {
	return "id IN (SELECT " + requestPlan + " FROM plans WHERE " + where + ")"
}

// Plan returns the plan whose id or ref is key, without its saved form.
// A pending plan past its expiry reads as expired. A request reads as the
// plan it stands for, and one still planning or with no plan as itself.
func (s *Store) Plan(ctx context.Context, key string, now time.Time) (Plan, error) {
	p, err := scanPlan(s.db.QueryRowContext(ctx, "SELECT "+planColumns+" FROM plans WHERE id = ? OR ref = ? AND "+requestPlan+" IS NULL OR "+plansOfRequests("ref = ?"),
		now.UnixMilli(), key, key, key))
	if errors.Is(err, sqldb.ErrNoRows) {
		return Plan{}, ErrNotFound
	}
	return p, err
}

// SavedPlan returns the saved form of the plan with id, and the writ.lock
// it was made with.
func (s *Store) SavedPlan(ctx context.Context, id string) (saved []byte, err error) {
	err = s.db.QueryRowContext(ctx, "SELECT saved FROM plans WHERE id = ?", id).Scan(&saved)
	if errors.Is(err, sqldb.ErrNoRows) {
		return nil, ErrNotFound
	}
	return saved, err
}

// PlanFilter narrows Plans. Empty fields match every plan.
type PlanFilter struct {
	Env, Stack string
	// Status matches the status each plan shows: a live plan's is the
	// status Rules tally from its approvals, which a change of config can
	// move with no write, and any other plan's is the stored one.
	Status string
	// Rules tally a live plan's status. A Status of pending or approved
	// needs them.
	Rules Rules
	Limit int
	// Cursor is the opaque value Plans returned as next.
	Cursor string
	// Keep, when set, drops each plan of a stack@env it reports false for
	// before the limit, so a plan it drops neither takes a place nor sets
	// next.
	Keep func(env, stack string) bool
	// StartedBy, when set, keeps only the plans StartedByOf gives it:
	// StartedByPerson or StartedByCI.
	StartedBy string
}

// What started a plan or a drift check.
const (
	// StartedByPerson is a plan of a person's session or of the dev user.
	StartedByPerson = "person"
	// StartedByCI is a plan of a CI job's token.
	StartedByCI = "ci"
	// StartedByDrift is a drift check, which writ serve starts on a timer.
	// No plan of the plans table is one.
	StartedByDrift = "drift"
)

// CIJob is the kind of a CI job's principal, as a plan's author document
// names it. Every other kind, a session's person or the dev user, is a
// person.
const CIJob = "ci-job"

// StartedByOf is what started a plan whose author is a principal of kind.
func StartedByOf(kind string) string {
	if kind == CIJob {
		return StartedByCI
	}
	return StartedByPerson
}

// authorKind is the kind of a plan's author, read from its author
// document, "" for a document with none, as StartedByOf reads it.
func authorKind(pg bool) string {
	if pg {
		return "COALESCE(plans.author::jsonb ->> 'kind', '')"
	}
	return "COALESCE(json_extract(plans.author, '$.kind'), '')"
}

// Plans lists plans newest first, without their saved form. next is the
// cursor of the following page, "" on the last.
func (s *Store) Plans(ctx context.Context, f PlanFilter, now time.Time) (plans []Plan, next string, err error) {
	var where []string
	var args []any
	add := func(cond string, v ...any) {
		args = append(args, v...)
		where = append(where, cond)
	}
	add("1 = 1")
	if f.Env != "" {
		add("env = ?", f.Env)
	}
	if f.Stack != "" {
		add("stack = ?", f.Stack)
	}
	switch f.StartedBy {
	case "":
	case StartedByCI:
		add(authorKind(s.db.Postgres())+" = ?", CIJob)
	case StartedByPerson:
		add(authorKind(s.db.Postgres())+" <> ?", CIJob)
	default:
		return nil, "", fmt.Errorf("a list of plans started by %q: plans are started by %s or %s", f.StartedBy, StartedByPerson, StartedByCI)
	}
	tally := PlanStatus(f.Status).Live()
	switch {
	case tally && (f.Rules.Need == nil || f.Rules.Counts == nil):
		return nil, "", fmt.Errorf("a list of %s plans needs the approval rules", f.Status)
	case tally:
		add("("+liveStatus+") IN ('pending', 'approved')", now.UnixMilli())
	case f.Status != "":
		add("("+liveStatus+") = ?", now.UnixMilli(), f.Status)
	}
	if f.Cursor != "" {
		at, ref, err := decodeCursor(f.Cursor)
		if err != nil {
			return nil, "", err
		}
		add("(requested_at < ? OR requested_at = ? AND ref < ?)", at, at, ref)
	}
	if f.Keep != nil || tally {
		return s.keptPlans(ctx, strings.Join(where, " AND "), args, f, tally, now)
	}
	q := "SELECT " + planColumns + " FROM plans WHERE " + strings.Join(where, " AND ") + " ORDER BY requested_at DESC, ref DESC LIMIT ?"
	plans, err = s.scanPlans(ctx, q, append(append([]any{now.UnixMilli()}, args...), f.Limit+1)...)
	if err != nil {
		return nil, "", err
	}
	if len(plans) > f.Limit {
		plans = plans[:f.Limit]
		last := plans[len(plans)-1]
		next = encodeCursor(last.RequestedAt, last.Ref)
	}
	return plans, next, nil
}

// keptPlans pages the plans that match where, that f.Keep reports true
// for, and, with tally, whose tally under f.Rules is f.Status, newest
// first. The first pass reads only the columns these need, so a caller who
// may read few plans costs no document or text, and decides the page and
// its cursor. With tally it reads one row per approval that is not
// withdrawn, so a plan comes as a run of rows. The second pass reads that
// page's rows by ref alone: filtering again would see a plan whose status
// changed in between drop out of the page while the cursor, set from the
// first pass, passed it.
func (s *Store) keptPlans(ctx context.Context, where string, args []any, f PlanFilter, tally bool, now time.Time) ([]Plan, string, error) {
	q := "SELECT ref, requested_at, env, stack FROM plans WHERE " + where + " ORDER BY requested_at DESC, ref DESC"
	if tally {
		q = "SELECT ref, requested_at, env, stack, " + liveStatus + ", need, source_commit, risks, changes, person FROM plans" +
			" LEFT JOIN approvals ON plan_id = plans.id AND withdrawn_at IS NULL WHERE " + where + " ORDER BY requested_at DESC, ref DESC"
		args = append([]any{now.UnixMilli()}, args...)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var refs []any
	var next string
	var lastAt int64
	// take decides the plan of the run of rows just read, and reports
	// whether the page is full.
	var p Plan
	var approvals []Approval
	// readable is whether the caller may read p. An unreadable plan keeps
	// no approvals, so its rows cost nothing but the scan.
	readable := false
	take := func() bool {
		if !readable || tally && f.Rules.Tally(p, approvals).Status != PlanStatus(f.Status) {
			return false
		}
		if len(refs) == f.Limit {
			next = encodeCursor(fromMS(lastAt), refs[f.Limit-1].(string))
			return true
		}
		refs, lastAt = append(refs, p.Ref), p.RequestedAt.UnixMilli()
		return false
	}
	full := false
	for rows.Next() {
		var row Plan
		var at int64
		var risks string
		var person sqldb.NullString
		dest := []any{&row.Ref, &at, &row.Env, &row.Stack}
		if tally {
			dest = append(dest, &row.Status, &row.Need, &row.Source.Commit, &risks, &row.Changes, &person)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, "", err
		}
		if p.Ref == row.Ref {
			if readable {
				approvals = append(approvals, Approval{Person: person.String})
			}
			continue
		}
		if p.Ref != "" && take() {
			full = true
			break
		}
		row.RequestedAt, approvals = fromMS(at), nil
		readable = f.Keep == nil || f.Keep(row.Env, row.Stack)
		if tally && readable {
			if row.Risks, err = unlist[string](risks); err != nil {
				return nil, "", err
			}
			if person.Valid {
				approvals = []Approval{{Person: person.String}}
			}
		}
		p = row
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if !full && p.Ref != "" {
		take()
	}
	rows.Close()
	if len(refs) == 0 {
		return nil, "", nil
	}
	q = "SELECT " + planColumns + " FROM plans WHERE ref IN (" + strings.TrimSuffix(strings.Repeat("?, ", len(refs)), ", ") + ") ORDER BY requested_at DESC, ref DESC"
	plans, err := s.scanPlans(ctx, q, append([]any{now.UnixMilli()}, refs...)...)
	return plans, next, err
}

func (s *Store) scanPlans(ctx context.Context, q string, args ...any) ([]Plan, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var plans []Plan
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, s.db.Error(err)
		}
		plans = append(plans, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return plans, nil
}

// ErrBadCursor means a list cursor was not one Plans made.
var ErrBadCursor = errors.New("the cursor is not one this server made")

func encodeCursor(at time.Time, ref string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(at.UnixMilli(), 10) + "|" + ref))
}

func decodeCursor(c string) (int64, string, error) {
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return 0, "", ErrBadCursor
	}
	ms, ref, ok := strings.Cut(string(b), "|")
	at, err := strconv.ParseInt(ms, 10, 64)
	if !ok || err != nil {
		return 0, "", ErrBadCursor
	}
	return at, ref, nil
}

// Decision is the outcome an audit row records.
type Decision string

const (
	Allowed    Decision = "allowed"
	Denied     Decision = "denied"      // a grant or a rule refused it
	AuthFailed Decision = "auth-failed" // no token, or a token that failed a check
	Refused    Decision = "refused"     // a malformed request, a limit, a replayed token
	Errored    Decision = "error"       // the server failed
	NoDecision Decision = "none"        // a call that needs no decision, such as /v1/server
)

// Entry is one audit row.
type Entry struct {
	At        time.Time
	Request   string
	Kind      string
	Method    string
	Route     string
	Status    int
	Remote    string
	Issuer    string
	Subject   string
	Person    string
	Principal string
	// PrincipalKind is ci-job, or person for a session.
	PrincipalKind string
	// Via says how the principal proved itself, such as "gitlab job 9922101".
	Via      string
	Teams    []string
	Op       string
	Env      string
	Stacks   []string
	Plan     string
	Run      string
	Addrs    []string
	Risks    []string
	Decision Decision
	Check    string
	Reason   string
	Grants   []int
	ConfigID int64
	Elapsed  time.Duration
}

// Audit writes audit rows, all of them or none.
func (s *Store) Audit(ctx context.Context, es ...Entry) error {
	return s.db.Write(ctx, func(tx *sqldb.Tx) error { return insertAudits(ctx, tx, es) })
}

func insertAudits(ctx context.Context, tx *sqldb.Tx, es []Entry) error {
	for _, e := range es {
		if err := insertAudit(ctx, tx, e); err != nil {
			return err
		}
	}
	return nil
}

func insertAudit(ctx context.Context, tx *sqldb.Tx, e Entry) error {
	var cfg any
	if e.ConfigID != 0 {
		cfg = e.ConfigID
	}
	var id int64
	err := tx.QueryRowContext(ctx, `INSERT INTO audit (at, request, kind, method, route, status, remote, issuer, subject, person, principal,
			principal_kind, via, teams, op, env, stacks, plan, run, addrs, risks, decision, check_failed, reason, grants, config_id, elapsed_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
		e.At.UnixMilli(), e.Request, e.Kind, e.Method, e.Route, e.Status, e.Remote, e.Issuer, e.Subject, e.Person, e.Principal,
		e.PrincipalKind, e.Via, list(e.Teams), e.Op, e.Env, list(e.Stacks), e.Plan, e.Run, list(e.Addrs), list(e.Risks),
		string(e.Decision), e.Check, e.Reason, list(e.Grants), cfg, e.Elapsed.Milliseconds()).Scan(&id)
	if err != nil {
		return fmt.Errorf("write audit row: %w", err)
	}
	for _, st := range uniq(e.Stacks) {
		if _, err := tx.ExecContext(ctx, "INSERT INTO audit_stacks (audit_id, stack) VALUES (?, ?)", id, st); err != nil {
			return fmt.Errorf("write audit row: %w", err)
		}
	}
	return nil
}

// Retention says how long rows are kept. Zero keeps them forever.
type Retention struct {
	// Audit is the age past which audit rows are deleted.
	Audit time.Duration
	// Plans is the age past which plans that can no longer be applied
	// (refused, failed, expired, superseded) are deleted, with their saved form.
	Plans time.Duration
}

// Pruned counts what Prune deleted.
type Pruned struct {
	Expired, Plans, Audit, Tokens, RoleSessions, Keys int64
}

// Prune marks live plans past their expiry as expired, deletes token
// ids past their token's expiry, role sessions two weeks past theirs, and
// the none rows of apply requests' keys an hour past their KeyLife, and
// deletes what the retention says.
// Plans that a run or an approval names are kept.
func (s *Store) Prune(ctx context.Context, now time.Time, r Retention) (Pruned, error) {
	var out Pruned
	exec := func(n *int64, q string, args ...any) error {
		res, err := s.db.ExecContext(ctx, q, args...)
		if err != nil {
			return err
		}
		*n, err = res.RowsAffected()
		return err
	}
	var err error
	if out.Expired, err = s.expire(ctx, now); err != nil {
		return out, err
	}
	if err := exec(&out.Tokens, "DELETE FROM token_uses WHERE exp < ?", now.UnixMilli()); err != nil {
		return out, err
	}
	if r.Plans > 0 {
		if out.Plans, err = s.deletePlans(ctx, now.Add(-r.Plans)); err != nil {
			return out, err
		}
	}
	if out.Keys, err = pruneKeys(ctx, s.db, now); err != nil {
		return out, err
	}
	if err := exec(&out.RoleSessions, "DELETE FROM role_sessions WHERE expires_at < ?", now.Add(-roleSessionKeep).UnixMilli()); err != nil {
		return out, err
	}
	if r.Audit > 0 {
		if err := exec(&out.Audit, "DELETE FROM audit WHERE at < ?", now.Add(-r.Audit).UnixMilli()); err != nil {
			return out, err
		}
	}
	return out, nil
}
