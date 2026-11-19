package server

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mr-karan/writ/internal/authz"
	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/diag"
)

// FormatAudit is GET /v1/audit.
const FormatAudit = "writ.audit/v0"

// AuditDoc is GET /v1/audit: a page of the audit log, newest first, only
// the rows the principal may see.
type AuditDoc struct {
	Format string     `json:"format"`
	OK     bool       `json:"ok"`
	Rows   []AuditRow `json:"rows"`
	// Next is the cursor of the following page, "" on the last.
	Next        string       `json:"next"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// AuditRow is one audit row as the API shows it. It has no client
// address, and the reason only of a row that was allowed or needed no
// decision, as auditReason says.
type AuditRow struct {
	At      string `json:"at"`
	Request string `json:"request"`
	Kind    string `json:"kind"`
	Method  string `json:"method"`
	Route   string `json:"route"`
	Status  int    `json:"status"`
	// Principal is the principal's display name.
	Principal     string   `json:"principal"`
	PrincipalKind string   `json:"principal_kind"`
	Person        string   `json:"person"`
	Issuer        string   `json:"issuer"`
	Subject       string   `json:"subject"`
	Via           string   `json:"via"`
	Teams         []string `json:"teams"`
	Op            string   `json:"op"`
	Env           string   `json:"env"`
	Stacks        []string `json:"stacks"`
	Plan          string   `json:"plan"`
	Run           string   `json:"run"`
	Risks         []string `json:"risks"`
	Decision      string   `json:"decision"`
	Check         string   `json:"check"`
	Reason        string   `json:"reason"`
	// ReasonWithheld is true when the row has a reason the API does not
	// show.
	ReasonWithheld bool  `json:"reason_withheld"`
	Grants         []int `json:"grants"`
	ElapsedMS      int64 `json:"elapsed_ms"`
}

// listAudit is GET /v1/audit?plan=&run=&env=&stack=&person=&op=&since=&until=&limit=&cursor=.
// Every filter value it records on its own row is one it checked first,
// so the row quotes nothing else the request sent.
func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	bad := func(msg string) { s.refuse(w, r, FormatAudit, diag.Usage, http.StatusBadRequest, "%s", msg) }
	f := control.AuditFilter{Plan: q.Get("plan"), Run: q.Get("run"), Env: q.Get("env"), Stack: q.Get("stack"),
		Person: q.Get("person"), Op: q.Get("op"), Cursor: q.Get("cursor"), Limit: 50}
	if l := q.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 || n > 200 {
			bad("limit must be 1 to 200")
			return
		}
		f.Limit = n
	}
	switch {
	case f.Stack != "" && f.Env == "":
		bad("stack needs env: a stack is named within an environment")
		return
	case f.Plan != "" && !planIDRE.MatchString(f.Plan) && !planRefRE.MatchString(f.Plan):
		bad("plan must be a plan id, 32 hex digits, or a plan ref, 26 letters A to Z and digits 2 to 7")
		return
	case f.Run != "" && !runIDRE.MatchString(f.Run):
		bad("run must be a run id, 26 letters A to Z and digits 2 to 7")
		return
	case len(f.Person) > 256:
		bad("person is over 256 bytes")
		return
	case f.Op != "" && !slices.Contains(auditOps(), f.Op):
		bad("op must be one of " + strings.Join(auditOps(), ", "))
		return
	}
	if f.Env != "" {
		if _, ok := s.target(w, r, FormatAudit, f.Env, cmpOr(f.Stack, "x")); !ok {
			return
		}
	}
	for _, t := range []struct {
		name string
		to   *time.Time
	}{{"since", &f.Since}, {"until", &f.Until}} {
		if v := q.Get(t.name); v != "" {
			at, err := time.Parse(time.RFC3339, v)
			if err != nil {
				bad(t.name + " must be an RFC 3339 time, such as 2026-11-19T09:30:00Z")
				return
			}
			*t.to = at
		}
	}
	if !f.Since.IsZero() && !f.Until.IsZero() && !f.Until.After(f.Since) {
		bad("until must be after since")
		return
	}
	req := reqOf(r)
	teams := req.principal.teams
	every := s.cfg.Policy.DecideEvery(teams, authz.Admin, maxStackParts).Allowed
	f.Keep = func(e control.Entry) bool { return s.auditVisible(teams, every, e) }
	rows, next, err := s.store.AuditRows(r.Context(), f)
	if errors.Is(err, control.ErrBadCursor) {
		bad(err.Error())
		return
	}
	if err != nil {
		s.dbFailed(w, r, FormatAudit, err)
		return
	}
	e := &req.entry
	e.Op, e.Env, e.Plan, e.Run, e.Decision = string(authz.Read), f.Env, f.Plan, f.Run, control.Allowed
	if f.Stack != "" {
		e.Stacks = []string{f.Stack}
	}
	doc := AuditDoc{Format: FormatAudit, OK: true, Rows: []AuditRow{}, Next: next, Diagnostics: []Diagnostic{}}
	for _, row := range rows {
		doc.Rows = append(doc.Rows, auditRow(row))
	}
	s.reply(w, http.StatusOK, doc)
}

// auditVisible reports whether a principal in teams may see e. A row of a
// stack@env needs read on every stack it names there, and a row of an
// environment and no stack, read on some stack there, as an
// environment's other records do. A row of no environment, or of every
// one, such as a sign-in, the server's start, or a repository's add,
// needs admin on every stack in every environment: every is that
// decision. every also sees every other row.
func (s *Server) auditVisible(teams []string, every bool, e control.Entry) bool {
	switch {
	case every:
		return true
	case e.Env == "" || e.Env == "*":
		return false
	case len(e.Stacks) == 0:
		return s.cfg.Policy.InEnv(teams, authz.Read, e.Env)
	}
	return s.cfg.Policy.Decide(teams, authz.Read, e.Stacks, e.Env, nil).Allowed
}

// auditOps are the operations an audit row can record: those of the
// grants, and those of the state backend's rows, which internal/tfstate
// writes. GET /v1/audit takes only these, so no op filter reads the whole
// log for a value no row holds.
func auditOps() []string {
	var ops []string
	for _, op := range authz.Ops {
		ops = append(ops, string(op))
	}
	return append(ops, "state-read", "state-write", "state-lock", "state-unlock")
}

func auditRow(e control.Entry) AuditRow {
	reason := auditReason(e)
	return AuditRow{At: stamp(e.At), Request: e.Request, Kind: e.Kind, Method: e.Method, Route: e.Route, Status: e.Status,
		Principal: e.Principal, PrincipalKind: e.PrincipalKind, Person: e.Person, Issuer: e.Issuer, Subject: e.Subject, Via: e.Via,
		Teams: orEmpty(e.Teams), Op: e.Op, Env: e.Env, Stacks: orEmpty(e.Stacks), Plan: e.Plan, Run: e.Run, Risks: orEmpty(e.Risks),
		Decision: string(e.Decision), Check: e.Check, Reason: reason, ReasonWithheld: reason != e.Reason,
		Grants: orEmptyInts(e.Grants), ElapsedMS: e.Elapsed.Milliseconds()}
}

// auditReason is the reason the API shows of e. A refusal's reason is the
// message the request got, and that message can quote what the request
// sent, such as a path, a field, or a name, in which a client may have
// put a secret: only that client saw it. So only the reason of a row that
// was allowed or needed no decision is shown. writ serve composes those
// from what it holds, except an override's or an adoption's reason, which
// a person wrote for this log.
func auditReason(e control.Entry) string {
	if e.Decision == control.Allowed || e.Decision == control.NoDecision {
		return e.Reason
	}
	return ""
}
