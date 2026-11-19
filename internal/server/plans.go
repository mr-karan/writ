package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/mr-karan/writ/internal/authz"
	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/diag"
)

// MaxWait bounds ?wait= on GET /v1/plans/{id}.
const MaxWait = 120 * time.Second

var (
	planIDRE  = regexp.MustCompile(`^[0-9a-f]{32}$`)
	planRefRE = regexp.MustCompile(`^[A-Z2-7]{26}$`)
)

// createPlan is POST /v1/plans: check the request, keep a plan in status
// planning, and plan in the background. It answers 202 with the plan's
// ref; the plan's id is known when the plan ends.
func (s *Server) createPlan(w http.ResponseWriter, r *http.Request) {
	req := reqOf(r)
	p := req.principal
	var body PlanRequest
	if err := s.readBody(r, &body); err != nil {
		s.refuseBody(w, r, FormatRemotePlan, err)
		return
	}
	if body.Format != FormatPlanRequest {
		s.refuse(w, r, FormatRemotePlan, diag.Usage, http.StatusBadRequest, "the body's format is %q, not %s", trunc(body.Format), FormatPlanRequest)
		return
	}
	if len(body.Stacks) != 1 {
		s.refuse(w, r, FormatRemotePlan, diag.Usage, http.StatusBadRequest, "a plan names exactly one stack; plans of several stacks wait for estates")
		return
	}
	stack := body.Stacks[0]
	e, ok := s.target(w, r, FormatRemotePlan, body.Env, stack)
	if !ok || !s.decide(w, r, FormatRemotePlan, authz.Plan, stack, e.Name) {
		return
	}
	env := e.Name
	if body.Source == nil {
		s.refuse(w, r, FormatRemotePlan, diag.Usage, http.StatusBadRequest, "a plan needs source, the commit to plan")
		return
	}
	in, rerr := s.planSource(r.Context(), p, body.Source, stack)
	if rerr != nil {
		s.refuse(w, r, FormatRemotePlan, rerr.class, rerr.status, "%s", rerr.msg)
		return
	}
	if !s.claim(p.key()) {
		s.refuse(w, r, FormatRemotePlan, diag.Usage, http.StatusTooManyRequests,
			"writ serve is running %d plans for %s or %d in all; try again when one ends", s.cfg.Limits.PlansPerPrincipal, p.info.Display, s.cfg.Limits.Plans)
		return
	}
	ref := newID()
	now := s.now()
	plan := control.Plan{
		Ref: ref, Env: env, Stack: stack,
		Author: control.Principal{Issuer: p.info.Issuer, Subject: p.info.Subject, Person: p.info.Person, Display: p.info.Display, Detail: p.detail()},
		Teams:  p.teams, Origin: p.origin(body.Hints), RequestedAt: now, ConfigID: s.configID, Source: in.source,
	}
	req.entry.At, req.entry.Status, req.entry.Plan = req.start, http.StatusAccepted, ref
	req.entry.Elapsed = s.now().Sub(req.start)
	if err := s.store.CreatePlan(r.Context(), plan, p.use("plan"), req.entry); err != nil {
		s.release(p.key())
		if errors.Is(err, control.ErrReplay) {
			req.entry.Plan = ""
			s.authFail(w, r, FormatRemotePlan, "replay", "this token already made a plan; each CI token makes one")
			return
		}
		s.log.Error("create plan", "request", req.id, "err", err)
		s.refuse(w, r, FormatRemotePlan, diag.StateIO, http.StatusServiceUnavailable, "the control database failed; the request id is %s", req.id)
		return
	}
	req.audited = true
	done := make(chan struct{})
	s.mu.Lock()
	s.done[ref] = done
	s.mu.Unlock()
	s.wg.Go(func() { s.runPlan(ref, env, stack, in, p, done) })
	s.log.Info("plan started", "request", req.id, "plan", ref, "env", env, "stack", stack, "author", p.info.Person, "repo", in.source.Repo, "commit", in.source.Commit)
	w.Header().Set("Location", "/v1/plans/"+ref)
	plan.Status = control.Planning
	s.reply(w, http.StatusAccepted, s.remotePlan(plan, nil, nil))
}

// claim counts a running plan for key, within the limits.
func (s *Server) claim(key string) bool {
	return s.claimSpare(key, 0)
}

// claimSpare claims a plan slot for key while spare slots stay free after
// it.
func (s *Server) claimSpare(key string, spare int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running[key] >= s.cfg.Limits.PlansPerPrincipal || s.total+spare >= s.cfg.Limits.Plans {
		return false
	}
	s.running[key]++
	s.total++
	return true
}

func (s *Server) release(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.total--
	if s.running[key]--; s.running[key] <= 0 {
		delete(s.running, key)
	}
}

// runPlan runs one plan and records how it ended. It is the goroutine
// createPlan starts; Close waits for it.
func (s *Server) runPlan(ref, env, stack string, in planInput, p *principal, done chan struct{}) {
	defer func() {
		s.release(p.key())
		s.mu.Lock()
		delete(s.done, ref)
		s.mu.Unlock()
		close(done)
	}()
	res := control.PlanResult{Status: control.Failed}
	planCtx, cancelPlan := s.withLimit(s.ctx, "the plan", "limits.plan_timeout", s.cfg.Limits.PlanTimeout)
	defer cancelPlan()
	text, err := s.plan(planCtx, ref, env, stack, in, p)
	if te := timedOut(planCtx); te != nil && !errors.Is(err, te) {
		// A plan that ran out of time may still return a document, which
		// can read as a plan. It is not one: the plan fails.
		err = &classedError{diag.Usage, te}
	}
	if err != nil {
		s.log.Error("plan failed", "plan", ref, "err", err)
		class := diag.Internal
		if ce, ok := errors.AsType[*classedError](err); ok {
			class = ce.class
		}
		res.Document, _ = json.Marshal(refusal{Format: "writ.plan.json/v2", Diagnostics: []Diagnostic{{"error", string(class), err.Error()}}})
		res.Text = "writ: " + err.Error() + "\n"
		res.SourceDigest = text.SourceDigest
	} else {
		res = text
	}
	res.MadeAt = s.now()
	if res.ID != "" && res.Status == control.Pending {
		res.ExpiresAt = res.MadeAt.Add(s.cfg.PlanTTL)
		res.Need, res.NeedRules = s.cfg.Policy.Need(stack, env, res.Risks, res.Changes)
		res.NeedRules = oneBased(res.NeedRules)
		if res.Need == 0 {
			res.Status = control.Approved
		}
	}
	e := control.Entry{At: res.MadeAt, Request: ref, Kind: "plan-finished", Issuer: p.info.Issuer, Subject: p.info.Subject, Person: p.info.Person,
		Principal: p.info.Display, PrincipalKind: p.info.Kind, Via: p.via(), Teams: p.teams, Op: "plan", Env: env, Stacks: []string{stack},
		Plan: res.ID, Risks: res.Risks, Decision: control.NoDecision, Reason: string(res.Status), ConfigID: s.configID}
	if e.Plan == "" {
		e.Plan = ref
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.store.FinishPlan(ctx, ref, res, e); err != nil {
		s.log.Error("record the end of a plan", "plan", ref, "err", err)
		const msg = "the control database could not record the plan; the server's log has the error"
		failed := control.PlanResult{Status: control.Failed, MadeAt: res.MadeAt, SourceDigest: res.SourceDigest, Text: "writ: " + msg + "\n"}
		failed.Document, _ = json.Marshal(refusal{Format: "writ.plan.json/v2", Diagnostics: []Diagnostic{{"error", string(diag.StateIO), msg}}})
		e.Plan, e.Risks, e.Reason = ref, nil, string(control.Failed)
		// The first write may have spent its deadline. If this one fails
		// too, the next start's FailPlanning ends the plan.
		failCtx, cancelFail := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelFail()
		if err := s.store.FinishPlan(failCtx, ref, failed, e); err != nil {
			s.log.Error("record the failure of a plan", "plan", ref, "err", err)
		}
		return
	}
	s.log.Info("plan saved", "plan", ref, "id", res.ID, "status", res.Status, "env", env, "stack", stack, "author", p.info.Person, "changes", res.Changes, "risks", res.Risks)
}

// plan reads the commit of the plan and plans the OpenTofu stack in it.
func (s *Server) plan(ctx context.Context, ref, env, stack string, in planInput, p *principal) (control.PlanResult, error) {
	snap, err := s.commitSnapshot(ctx, in.repo, in.client, in.source.Commit)
	if err != nil {
		return control.PlanResult{}, err
	}
	return s.planTofu(ctx, ref, env, stack, in, snap, p)
}

// remotePlan is the API's document of a kept plan, with its approvals.
func (s *Server) remotePlan(p control.Plan, approvals []control.Approval, diags []Diagnostic) RemotePlan {
	ok := p.Status != control.RefusedPlan && p.Status != control.Failed
	if diags == nil {
		diags = []Diagnostic{}
	}
	doc := RemotePlan{Format: FormatRemotePlan, OK: ok, Plan: s.planInfo(p, approvals), Document: p.Document, Text: p.Text, Policy: p.Policy, Diagnostics: diags}
	if doc.Document == nil {
		doc.Document = json.RawMessage("null")
	}
	if doc.Policy == nil {
		doc.Policy = json.RawMessage("null")
	}
	return doc
}

func (s *Server) planInfo(p control.Plan, approvals []control.Approval) *PlanInfo {
	var author Principal
	_ = json.Unmarshal(p.Author.Detail, &author)
	t := s.rules().Tally(p, approvals)
	info := &PlanInfo{
		Ref: p.Ref, ID: p.ID, Status: string(t.Status), Env: p.Env,
		Stacks:      []PlanStack{{Stack: p.Stack, Changes: p.Changes, SourceDigest: p.SourceDigest}},
		Author:      author, StartedBy: control.StartedByOf(author.Kind),
		RequestedAt: stamp(p.RequestedAt), MadeAt: stamp(p.MadeAt), ExpiresAt: stamp(p.ExpiresAt),
		Risks: orEmpty(p.Risks), TeamsThatMayApply: []string{}, Origin: p.Origin, SupersededBy: p.SupersededBy, SupersededWhy: p.SupersededWhy,
		Approvals: PlanApprovals{Need: t.Need, Rules: orEmptyInts(p.NeedRules), ApprovedBy: approvalInfos(t.Approvals), TeamsThatMayApprove: []string{}},
	}
	if info.Origin == nil {
		info.Origin = json.RawMessage("{}")
	}
	info.Source = PlanSourceInfo{Kind: SourceSpeculative}
	if p.Source.Commit != "" {
		info.Source = PlanSourceInfo{Kind: SourceCommit, Repo: p.Source.Repo, Ref: p.Source.Ref, Commit: p.Source.Commit, RefCheck: string(p.Source.RefCheck)}
	}
	if p.Status.Live() {
		info.TeamsThatMayApply = s.cfg.Policy.TeamsWith(authz.Apply, p.Stack, p.Env, p.Risks)
		info.Approvals.TeamsThatMayApprove = s.cfg.Policy.TeamsWith(authz.Approve, p.Stack, p.Env, nil)
	}
	return info
}

func approvalInfos(as []control.Approval) []ApprovalInfo {
	out := make([]ApprovalInfo, len(as))
	for i, a := range as {
		out[i] = ApprovalInfo{Person: a.Person, Display: a.Display, Via: a.Via, At: stamp(a.ApprovedAt)}
	}
	return out
}

func orEmptyInts(xs []int) []int {
	if xs == nil {
		return []int{}
	}
	return xs
}

// getPlan is GET /v1/plans/{id}, by id or by ref. With ?wait=DURATION it
// holds a planning plan until it ends or the wait passes.
func (s *Server) getPlan(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("id")
	if !planIDRE.MatchString(key) && !planRefRE.MatchString(key) {
		s.refuse(w, r, FormatRemotePlan, diag.Usage, http.StatusNotFound, "%q is neither a plan id nor a plan ref", trunc(key))
		return
	}
	var wait time.Duration
	if v := r.URL.Query().Get("wait"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			s.refuse(w, r, FormatRemotePlan, diag.Usage, http.StatusBadRequest, "wait must be a duration such as 60s")
			return
		}
		wait = min(d, MaxWait)
	}
	p, err := s.store.Plan(r.Context(), key, s.now())
	if errors.Is(err, control.ErrNotFound) {
		s.refuse(w, r, FormatRemotePlan, diag.Usage, http.StatusNotFound, "no plan %s", key)
		return
	}
	if err != nil {
		s.refuse(w, r, FormatRemotePlan, diag.StateIO, http.StatusServiceUnavailable, "the control database failed; the request id is %s", reqOf(r).id)
		return
	}
	reqOf(r).entry.Plan = p.Ref
	if p.ID != "" {
		reqOf(r).entry.Plan = p.ID
	}
	if !s.decide(w, r, FormatRemotePlan, authz.Read, p.Stack, p.Env) {
		return
	}
	if p.Status == control.Planning && wait > 0 {
		s.mu.Lock()
		done := s.done[p.Ref]
		s.mu.Unlock()
		if done != nil {
			t := time.NewTimer(wait)
			select {
			case <-done:
			case <-t.C:
			case <-r.Context().Done():
			}
			t.Stop()
		}
		if p, err = s.store.Plan(r.Context(), key, s.now()); err != nil {
			s.refuse(w, r, FormatRemotePlan, diag.StateIO, http.StatusServiceUnavailable, "the control database failed; the request id is %s", reqOf(r).id)
			return
		}
	}
	doc, err := s.planDoc(r.Context(), p)
	if err != nil {
		s.refuse(w, r, FormatRemotePlan, diag.StateIO, http.StatusServiceUnavailable, "the control database failed; the request id is %s", reqOf(r).id)
		return
	}
	s.reply(w, http.StatusOK, doc)
}

// planDoc is GET /v1/plans/{id}'s document: the plan, its approvals, its
// history, and its run.
func (s *Server) planDoc(ctx context.Context, p control.Plan) (RemotePlan, error) {
	var approvals []control.Approval
	if p.ID != "" {
		got, err := s.store.Approvals(ctx, []string{p.ID})
		if err != nil {
			return RemotePlan{}, err
		}
		approvals = got[p.ID]
	}
	doc := s.remotePlan(p, approvals, nil)
	evs, err := s.store.PlanEvents(ctx, p.Ref)
	if err != nil {
		return RemotePlan{}, err
	}
	for _, ev := range evs {
		doc.Plan.History = append(doc.Plan.History, PlanEvent{At: stamp(ev.At), From: string(ev.From), To: string(ev.To), Person: ev.Person, By: ev.Display, Run: ev.Run, Reason: ev.Reason})
	}
	if p.ID != "" {
		run, err := s.store.PlanRun(ctx, p.ID)
		switch {
		case err == nil:
			doc.Plan.Run = run.ID
		case !errors.Is(err, control.ErrNotFound):
			return RemotePlan{}, err
		}
	}
	return doc, nil
}

// listPlans is GET /v1/plans?env=&stack=&status=&started_by=&limit=&cursor=.
// It lists only the plans the principal may read.
func (s *Server) listPlans(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := control.PlanFilter{Env: q.Get("env"), Stack: q.Get("stack"), Status: q.Get("status"), StartedBy: q.Get("started_by"), Rules: s.rules(), Cursor: q.Get("cursor"), Limit: 50}
	switch f.StartedBy {
	case "", StartedByPerson, StartedByCI:
	case StartedByDrift:
		s.refuse(w, r, FormatRemotePlans, diag.Usage, http.StatusBadRequest, "no plan is started by drift: GET /v1/drift lists the drift checks")
		return
	default:
		s.refuse(w, r, FormatRemotePlans, diag.Usage, http.StatusBadRequest, "started_by must be person or ci, not %q", trunc(f.StartedBy))
		return
	}
	if l := q.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 || n > 200 {
			s.refuse(w, r, FormatRemotePlans, diag.Usage, http.StatusBadRequest, "limit must be 1 to 200")
			return
		}
		f.Limit = n
	}
	if f.Env == "" {
		s.refuse(w, r, FormatRemotePlans, diag.Usage, http.StatusBadRequest, "plans needs ?env=")
		return
	}
	if _, ok := s.target(w, r, FormatRemotePlans, f.Env, cmpOr(f.Stack, "x")); !ok {
		return
	}
	teams := reqOf(r).principal.teams
	// The page and its cursor hold only plans the caller may read, so a
	// plan of a stack it can't read neither fills a page nor sets next.
	f.Keep = func(env, stack string) bool {
		return s.cfg.Policy.Decide(teams, authz.Read, []string{stack}, env, nil).Allowed
	}
	plans, next, err := s.store.Plans(r.Context(), f, s.now())
	if errors.Is(err, control.ErrBadCursor) {
		s.refuse(w, r, FormatRemotePlans, diag.Usage, http.StatusBadRequest, "%v", err)
		return
	}
	if err != nil {
		s.refuse(w, r, FormatRemotePlans, diag.StateIO, http.StatusServiceUnavailable, "the control database failed; the request id is %s", reqOf(r).id)
		return
	}
	req := reqOf(r)
	req.entry.Op, req.entry.Env, req.entry.Decision = string(authz.Read), f.Env, control.Allowed
	if f.Stack != "" {
		req.entry.Stacks = []string{f.Stack}
	}
	doc := RemotePlans{Format: FormatRemotePlans, OK: true, Plans: []PlanInfo{}, Next: next, Diagnostics: []Diagnostic{}}
	var ids []string
	for _, p := range plans {
		if p.ID != "" {
			ids = append(ids, p.ID)
		}
	}
	approvals, err := s.store.Approvals(r.Context(), ids)
	if err != nil {
		s.refuse(w, r, FormatRemotePlans, diag.StateIO, http.StatusServiceUnavailable, "the control database failed; the request id is %s", reqOf(r).id)
		return
	}
	for _, p := range plans {
		doc.Plans = append(doc.Plans, *s.planInfo(p, approvals[p.ID]))
	}
	s.reply(w, http.StatusOK, doc)
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
