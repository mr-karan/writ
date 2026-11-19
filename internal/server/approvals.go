package server

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/mr-karan/writ/internal/authz"
	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/diag"
	"github.com/mr-karan/writ/internal/oidc"
)

// maxPipelinePlans bounds the plans one approval of a pipeline names.
const maxPipelinePlans = 50

// approvePlan is POST /v1/plans/{id}/approvals: the caller approves one
// plan.
func (s *Server) approvePlan(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.approveBody(w, r, false); !ok {
		return
	}
	p, ok := s.planByID(w, r, FormatApprove, r.PathValue("id"))
	if !ok {
		return
	}
	s.approve(w, r, []control.Plan{p})
}

// approvePipeline is POST /v1/approvals with pipeline: a CI job approves
// every live plan of its own pipeline, all or none, so a script on the
// author's branch cannot pick one plan and leave out another.
func (s *Server) approvePipeline(w http.ResponseWriter, r *http.Request) {
	body, ok := s.approveBody(w, r, true)
	if !ok {
		return
	}
	pr := reqOf(r).principal
	if !body.Pipeline {
		s.refuse(w, r, FormatApprove, diag.Usage, http.StatusBadRequest, "POST /v1/approvals takes pipeline: true; approve one plan with POST /v1/plans/{id}/approvals")
		return
	}
	if pr.tok == nil {
		s.refuse(w, r, FormatApprove, diag.Usage, http.StatusBadRequest, "only a CI job's token names a pipeline: approve a plan by its id")
		return
	}
	run := pr.ciRun()
	project, pipeline := run.project, run.pipeline
	if pr.tok.Kind != oidc.GitLabCI || !run.ok() {
		s.refuse(w, r, FormatApprove, diag.Usage, http.StatusBadRequest, "only a GitLab CI token that names its project_id and pipeline_id names a pipeline: approve a plan by its id")
		return
	}
	plans, err := s.store.PipelinePlans(r.Context(), control.Pipeline{Issuer: run.issuer, ProjectID: project, Pipeline: pipeline}, s.now(), maxPipelinePlans+1)
	if err != nil {
		s.dbFailed(w, r, FormatApprove, err)
		return
	}
	switch {
	case len(plans) == 0:
		s.refuse(w, r, FormatApprove, diag.Usage, http.StatusNotFound, "pipeline %s of project %s has no pending or approved plan", trunc(pipeline), trunc(project))
		return
	case len(plans) > maxPipelinePlans:
		s.refuse(w, r, FormatApprove, diag.Usage, http.StatusRequestEntityTooLarge, "pipeline %s has more than %d live plans; approve them by id", trunc(pipeline), maxPipelinePlans)
		return
	}
	s.approve(w, r, plans)
}

// approveBody reads an approval's body: none, {}, or
// writ.approve-request/v0. Only POST /v1/approvals needs one.
func (s *Server) approveBody(w http.ResponseWriter, r *http.Request, required bool) (ApproveRequest, bool) {
	var body ApproveRequest
	if r.ContentLength == 0 && !required {
		return body, true
	}
	if err := s.readBody(r, &body); err != nil {
		s.refuseBody(w, r, FormatApprove, err)
		return body, false
	}
	if body.Format != "" && body.Format != FormatApproveReq || required && body.Format == "" {
		s.refuse(w, r, FormatApprove, diag.Usage, http.StatusBadRequest, "the body's format is %q, not %s", trunc(body.Format), FormatApproveReq)
		return body, false
	}
	return body, true
}

// planByID reads a saved plan by its id for a change. A ref names a plan
// only while it plans, and nothing approves or applies it then.
func (s *Server) planByID(w http.ResponseWriter, r *http.Request, format, id string) (control.Plan, bool) {
	if !planIDRE.MatchString(id) {
		s.refuse(w, r, format, diag.Usage, http.StatusNotFound, "%q is not a plan id, 32 hex digits", trunc(id))
		return control.Plan{}, false
	}
	p, err := s.store.Plan(r.Context(), id, s.now())
	if errors.Is(err, control.ErrNotFound) {
		if !s.driftRefusal(w, r, format, id) {
			s.refuse(w, r, format, diag.Usage, http.StatusNotFound, "no plan %s", id)
		}
		return control.Plan{}, false
	}
	if err != nil {
		s.dbFailed(w, r, format, err)
		return control.Plan{}, false
	}
	reqOf(r).entry.Plan = p.ID
	return p, true
}

func (s *Server) dbFailed(w http.ResponseWriter, r *http.Request, format string, err error) {
	s.log.Error("control database", "request", reqOf(r).id, "err", err)
	s.refuse(w, r, format, diag.StateIO, http.StatusServiceUnavailable, "the control database failed; the request id is %s", reqOf(r).id)
}

// approve records the caller's approval of plans, all or none. Each must
// be a plan of a commit. The caller needs approve on each plan's
// stack@env, must be a person, and through a CI token may approve only
// plans of the token's own pipeline.
func (s *Server) approve(w http.ResponseWriter, r *http.Request, plans []control.Plan) {
	req := reqOf(r)
	pr := req.principal
	e := &req.entry
	e.Op, e.Env, e.Stacks = string(authz.Approve), plans[0].Env, nil
	var ids []string
	var grants []int
	for _, p := range plans {
		e.Stacks = append(e.Stacks, p.Stack)
		ids = append(ids, p.ID)
		d := s.cfg.Policy.Decide(pr.teams, authz.Approve, []string{p.Stack}, p.Env, nil)
		if !d.Allowed {
			s.refuse(w, r, FormatApprove, diag.Denied, http.StatusForbidden, "%s may not approve plan %s: %s", pr.info.Display, p.ID, d.Reason)
			return
		}
		grants = append(grants, oneBased(d.Grants)...)
		if p.Source.Commit == "" {
			s.refuse(w, r, FormatApprove, diag.State, http.StatusConflict, "plan %s is a speculative plan, of files a client sent, which writ serve never approves: push the commit and plan it", p.ID)
			return
		}
		if pr.tok == nil {
			continue
		}
		owns, err := s.ownsPlan(r.Context(), pr, p)
		if err != nil {
			s.dbFailed(w, r, FormatApprove, err)
			return
		}
		if !owns {
			theirs, mine := originRun(p.Origin), pr.ciRun()
			s.refuse(w, r, FormatApprove, diag.Denied, http.StatusForbidden, "a CI job approves only the plans of its own pipeline: plan %s is of pipeline %s of project %s at %s, and the job's is %s of %s at %s",
				p.ID, cmpOr(trunc(theirs.pipeline), "-"), cmpOr(trunc(theirs.project), "-"), cmpOr(trunc(theirs.issuer), "-"), trunc(mine.pipeline), trunc(mine.project), trunc(mine.issuer))
			return
		}
	}
	e.Plan = ""
	if len(plans) == 1 {
		e.Plan = plans[0].ID
	}
	if pr.info.Person == "" {
		s.refuse(w, r, FormatApprove, diag.Denied, http.StatusForbidden, "an approval is a person's, and %s names no person", pr.info.Display)
		return
	}
	// A person id of an identity the config does not define was made by
	// another config or an older build, and may name the plan's author
	// under another id. Neither side of the self-approval check may hold one.
	if !s.cfg.Dev {
		if !s.knownIdentity(pr.info.Person) {
			s.refuse(w, r, FormatApprove, diag.Denied, http.StatusForbidden, "%s is person %s, of no identity the config defines: sign in again", pr.info.Display, trunc(pr.info.Person))
			return
		}
		for _, p := range plans {
			if p.Author.Person != "" && !s.knownIdentity(p.Author.Person) {
				s.refuse(w, r, FormatApprove, diag.State, http.StatusConflict, "plan %s was made by person %s, of no identity the config defines, so its author cannot be told from an approver: plan again", p.ID, trunc(p.Author.Person))
				return
			}
		}
	}
	now := s.now()
	e.Decision, e.Grants, e.At, e.Status = control.Allowed, grants, req.start, http.StatusOK
	e.Elapsed = now.Sub(req.start)
	approver := control.Approval{Person: pr.info.Person, Display: pr.info.Display, Issuer: pr.info.Issuer, Subject: pr.info.Subject, Via: pr.via(), ApprovedAt: now}
	out, err := s.store.Approve(r.Context(), approver, ids, s.rules(), pr.use("approve"), *e)
	var ae *control.ApprovalError
	switch {
	case errors.Is(err, control.ErrReplay):
		s.authFail(w, r, FormatApprove, "replay", "this token already approved; each CI token approves once")
		return
	case errors.As(err, &ae) && ae.Self:
		s.refuse(w, r, FormatApprove, diag.Denied, http.StatusForbidden, "%s", ae.Msg)
		return
	case errors.As(err, &ae):
		s.refuse(w, r, FormatApprove, diag.State, http.StatusConflict, "%s", ae.Msg)
		return
	case err != nil:
		s.dbFailed(w, r, FormatApprove, err)
		return
	}
	req.audited = true
	doc, err := s.approveDoc(r, out)
	if err != nil {
		s.dbFailed(w, r, FormatApprove, err)
		return
	}
	for _, p := range doc.Plans {
		s.log.Info("approved", "request", req.id, "plan", p.ID, "person", pr.info.Person, "via", pr.via(), "approvals", fmt.Sprintf("%d/%d", len(p.ApprovedBy), p.Need), "status", p.Status)
	}
	s.reply(w, http.StatusOK, doc)
}

// withdraw is DELETE /v1/plans/{id}/approvals/mine: the caller takes back
// their approval, until the plan starts applying.
func (s *Server) withdraw(w http.ResponseWriter, r *http.Request) {
	p, ok := s.planByID(w, r, FormatApprove, r.PathValue("id"))
	if !ok {
		return
	}
	req := reqOf(r)
	pr := req.principal
	if !s.decide(w, r, FormatApprove, authz.Approve, p.Stack, p.Env) {
		return
	}
	now := s.now()
	e := req.entry
	e.At, e.Status, e.Reason, e.Elapsed = req.start, http.StatusOK, "withdrawn", now.Sub(req.start)
	got, err := s.store.Withdraw(r.Context(), p.ID, pr.info.Person, pr.info.Display, s.rules(), now, e)
	var ae *control.ApprovalError
	switch {
	case errors.As(err, &ae) && ae.Status.Live():
		s.refuse(w, r, FormatApprove, diag.Usage, http.StatusNotFound, "%s", ae.Msg)
		return
	case errors.As(err, &ae):
		s.refuse(w, r, FormatApprove, diag.State, http.StatusConflict, "%s", ae.Msg)
		return
	case err != nil:
		s.dbFailed(w, r, FormatApprove, err)
		return
	}
	req.audited = true
	s.log.Info("approval withdrawn", "request", req.id, "plan", p.ID, "person", pr.info.Person, "status", got.Status)
	doc, err := s.approveDoc(r, []control.Plan{got})
	if err != nil {
		s.dbFailed(w, r, FormatApprove, err)
		return
	}
	s.reply(w, http.StatusOK, doc)
}

func (s *Server) approveDoc(r *http.Request, plans []control.Plan) (ApproveDoc, error) {
	ids := make([]string, len(plans))
	for i, p := range plans {
		ids[i] = p.ID
	}
	approvals, err := s.store.Approvals(r.Context(), ids)
	if err != nil {
		return ApproveDoc{}, err
	}
	doc := ApproveDoc{Format: FormatApprove, OK: true, Plans: []ApprovedPlan{}, Diagnostics: []Diagnostic{}}
	rules := s.rules()
	for _, p := range plans {
		t := rules.Tally(p, approvals[p.ID])
		doc.Plans = append(doc.Plans, ApprovedPlan{ID: p.ID, Env: p.Env, Stack: p.Stack, Status: string(t.Status), Need: t.Need, ApprovedBy: approvalInfos(t.Approvals)})
	}
	return doc, nil
}

// use is the token use of a write of kind op: a CI token makes each kind
// of write once. A principal with no token records none.
func (p *principal) use(op string) control.TokenUse {
	if p.tok == nil {
		return control.TokenUse{}
	}
	return control.TokenUse{Issuer: issuerURL(p.tok), JTI: p.tok.ID, Op: op, Expires: p.tok.Expires.Add(time.Minute)}
}

// claim is one of the token's claims as text, "" without a token or the
// claim.
func (p *principal) claim(name string) string {
	if p.tok == nil {
		return ""
	}
	v, _ := p.tok.Claims.String(name)
	return v
}
