package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mr-karan/writ/internal/authz"
	"github.com/mr-karan/writ/internal/awsrole"
	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/diag"
	"github.com/mr-karan/writ/internal/forge"
	"github.com/mr-karan/writ/internal/oidc"
)

// ApplyOutput is a finished apply.
type ApplyOutput struct {
	Document []byte // writ.tofu-apply/v0
	Text     string // the apply as writ apply prints it
	// Network is the run's writ.network/v1, nil when it had no egress
	// proxy.
	Network json.RawMessage
}

var runIDRE = regexp.MustCompile(`^[A-Z2-7]{26}$`)

// createRun is POST /v1/runs: check the request, begin the run in the
// control database, and apply in the background. It answers 202 with the
// run. The request's key is read before every other check: a repeat of a
// key that started a run answers with that run.
func (s *Server) createRun(w http.ResponseWriter, r *http.Request) {
	req := reqOf(r)
	req.entry.Op = string(authz.Apply)
	var body RunRequest
	if err := s.readBody(r, &body); err != nil {
		s.refuseBody(w, r, FormatRemoteRun, err)
		return
	}
	if body.Format != FormatRunRequest {
		s.refuse(w, r, FormatRemoteRun, diag.Usage, http.StatusBadRequest, "the body's format is %q, not %s", trunc(body.Format), FormatRunRequest)
		return
	}
	key, ok := s.requestKey(w, r, body)
	if !ok {
		return
	}
	switch row, err := s.store.Key(r.Context(), key.Key); {
	case err == nil:
		s.answerKey(w, r, row, key)
		return
	case !errors.Is(err, control.ErrNotFound):
		s.dbFailed(w, r, FormatRemoteRun, err)
		return
	}
	var plan control.Plan
	var merged mergedApply
	switch {
	case body.Plan != "" && body.Merged != nil:
		s.refuse(w, r, FormatRemoteRun, diag.Usage, http.StatusBadRequest, "a run names a plan by its id, plan, or by a merged commit, merged, not both")
		return
	case body.Plan != "":
		if body.Env != "" || len(body.Stacks) > 0 {
			s.refuse(w, r, FormatRemoteRun, diag.Usage, http.StatusBadRequest, "a run by plan id takes no env or stacks: the plan has them")
			return
		}
		var ok bool
		if plan, ok = s.planByID(w, r, FormatRemoteRun, body.Plan); !ok || !s.byPlan(w, r, plan, body.Override) {
			return
		}
	case body.Merged != nil:
		if body.Override != "" {
			s.refuse(w, r, FormatRemoteRun, diag.Usage, http.StatusBadRequest, "an override names a plan by its id: an apply after a merge applies only an approved plan")
			return
		}
		var ok bool
		if merged, ok = s.byMerged(w, r, body); !ok {
			return
		}
		plan = merged.plan
	default:
		s.refuse(w, r, FormatRemoteRun, diag.Usage, http.StatusBadRequest, "a run needs plan, a plan's id, or merged, a commit of the default branch")
		return
	}
	pr := req.principal
	e := &req.entry
	e.Env, e.Stacks = plan.Env, []string{plan.Stack}
	if merged.commit == "" {
		e.Plan, e.Risks = plan.ID, plan.Risks
	}
	env, ok := s.envOf(plan.Env)
	if !ok {
		s.refuse(w, r, FormatRemoteRun, diag.Usage, http.StatusConflict, "plan %s is of env %s, which this config no longer serves", plan.ID, trunc(plan.Env))
		return
	}
	d := s.cfg.Policy.Decide(pr.teams, authz.Apply, []string{plan.Stack}, plan.Env, plan.Risks)
	if !d.Allowed {
		if merged.commit != "" {
			s.refuse(w, r, FormatRemoteRun, diag.Denied, http.StatusForbidden, "%s may not apply to %s@%s", pr.info.Display, plan.Stack, plan.Env)
			return
		}
		s.refuse(w, r, FormatRemoteRun, diag.Denied, http.StatusForbidden, "%s may not apply plan %s: %s", pr.info.Display, plan.ID, d.Reason)
		return
	}
	e.Plan, e.Risks = plan.ID, plan.Risks
	if refusal := merged.refusal; refusal != nil {
		s.refuse(w, r, FormatRemoteRun, refusal.class, refusal.status, "%s", refusal.msg)
		return
	}
	// A plan of an OpenTofu stack made before writ serve kept saved plans
	// has none.
	if saved, err := s.store.SavedPlan(r.Context(), plan.ID); err != nil {
		s.dbFailed(w, r, FormatRemoteRun, err)
		return
	} else if len(saved) == 0 && plan.Status.Live() {
		s.refuse(w, r, FormatRemoteRun, diag.SavedPlan, http.StatusConflict, "plan %s keeps no saved plan, so it cannot apply: plan its commit again", plan.ID)
		return
	}
	grants := oneBased(d.Grants)
	if plan.Status == control.NoChanges || merged.appliedBy != "" {
		s.nothingToApply(w, r, plan, merged.commit, merged.appliedBy, grants)
		return
	}
	if body.Override != "" {
		o := s.cfg.Policy.Decide(pr.teams, authz.Override, []string{plan.Stack}, plan.Env, nil)
		if !o.Allowed {
			s.refuse(w, r, FormatRemoteRun, diag.Denied, http.StatusForbidden, "%s may not override the approval rules of plan %s: %s", pr.info.Display, plan.ID, o.Reason)
			return
		}
		grants = append(grants, oneBased(o.Grants)...)
	}
	decision, judged, ok := s.judgeApply(w, r, plan, pr)
	if !ok {
		return
	}
	if !s.claimApply() {
		s.refuse(w, r, FormatRemoteRun, diag.Usage, http.StatusTooManyRequests, "writ serve is running %d applies; try again when one ends", s.cfg.Limits.Applies)
		return
	}
	runID := newID()
	now := s.now()
	rules := s.rules()
	named := control.ByPlan
	if merged.commit != "" {
		named = control.ByMerged
	}
	e.Run, e.Decision, e.Grants, e.At, e.Status = runID, control.Allowed, grants, req.start, http.StatusAccepted
	e.Elapsed = now.Sub(req.start)
	if body.Override != "" {
		e.Reason = "override: " + body.Override
	}
	s.pend(runID)
	run, err := s.store.BeginApply(r.Context(), control.ApplyRequest{
		RunID: runID, PlanID: plan.ID, Via: pr.via(), Override: body.Override, Rules: rules, NamedBy: named, MergedCommit: merged.commit, Now: now,
		Actor: control.Principal{Issuer: pr.info.Issuer, Subject: pr.info.Subject, Person: pr.info.Person, Display: pr.info.Display, Detail: pr.detail()},
		Use:   pr.use("apply"), Audit: *e,
		RoleSession: s.roleSessionOf(plan.Env, runID, pr),
		Key:         key, Owner: s.boot, Commit: s.applyCommit, Policy: decision, PolicyApprovals: judged,
	})
	work := func() { s.work(run, plan, env, pr) }
	switch {
	case errors.Is(err, control.ErrCommitUnknown):
		if !s.settleStart(w, r, key, run, work) {
			return
		}
	case errors.Is(err, control.ErrKeyUsed):
		s.releaseApply()
		s.unpend(runID)
		row, err := s.store.Key(r.Context(), key.Key)
		if err != nil {
			s.dbFailed(w, r, FormatRemoteRun, err)
			return
		}
		s.answerKey(w, r, row, key)
		return
	case errors.Is(err, control.ErrKeyClosed):
		s.releaseApply()
		s.unpend(runID)
		req.keyClosed = true
		s.refuse(w, r, FormatRemoteRun, diag.State, http.StatusConflict, "the key %s is closed with no run: writ serve deleted the rows of keys made at or before its time; run the command again", key.Key)
		return
	case err != nil:
		s.releaseApply()
		s.unpend(runID)
		s.refuseApply(w, r, plan, err)
		return
	}
	req.audited = true
	work()
	attrs := []any{"request", req.id, "run", runID, "plan", plan.ID, "env", plan.Env, "stack", plan.Stack, "actor", pr.info.Person,
		"named_by", named, "approvals", len(run.Approvals), "need", rules.Tally(plan, nil).Need}
	if body.Override != "" {
		s.log.Warn("apply started with an override", append(attrs, "reason", body.Override)...)
	} else {
		s.log.Info("apply started", attrs...)
	}
	w.Header().Set("Location", "/v1/runs/"+runID)
	s.reply(w, http.StatusAccepted, s.remoteRun(run))
}

// byPlan checks an apply by plan id: a speculative plan applies only under
// --dev, an override is a person's, a CI job applies only a plan of its
// own pipeline, and the plan's ref must not have moved since the plan.
func (s *Server) byPlan(w http.ResponseWriter, r *http.Request, p control.Plan, override string) bool {
	req := reqOf(r)
	pr := req.principal
	// Apply on the stack@env comes before any call to the forge, with
	// writ serve's token; the plan's risks are checked after. The audit
	// row names the target even when this refuses.
	req.entry.Env, req.entry.Stacks = p.Env, []string{p.Stack}
	if d := s.cfg.Policy.Decide(pr.teams, authz.Apply, []string{p.Stack}, p.Env, nil); !d.Allowed {
		s.refuse(w, r, FormatRemoteRun, diag.Denied, http.StatusForbidden, "%s may not apply plan %s: %s", pr.info.Display, p.ID, d.Reason)
		return false
	}
	if !s.rules().Approvable(p) {
		s.refuse(w, r, FormatRemoteRun, diag.SavedPlan, http.StatusConflict, "plan %s is a speculative plan, of files a client sent, which a team's control plane never applies: push the commit and plan it", p.ID)
		return false
	}
	if override != "" {
		if pr.tok != nil {
			s.refuse(w, r, FormatRemoteRun, diag.Denied, http.StatusForbidden, "an override is a person's: a CI job cannot give one")
			return false
		}
		if len(override) < control.MinOverride || len(override) > 500 {
			s.refuse(w, r, FormatRemoteRun, diag.Usage, http.StatusBadRequest, "an override's reason needs %d to 500 characters, such as the ticket and why the approvers are away", control.MinOverride)
			return false
		}
	}
	if pr.tok != nil {
		owns, err := s.ownsPlan(r.Context(), pr, p)
		if err != nil {
			s.dbFailed(w, r, FormatRemoteRun, err)
			return false
		}
		if !owns {
			s.refuse(w, r, FormatRemoteRun, diag.Denied, http.StatusForbidden, "a CI job applies by id only a plan of its own pipeline; after a merge, apply with merged")
			return false
		}
	}
	if !p.Status.Live() || p.Source.Commit == "" || p.Source.Ref == "" {
		return true
	}
	why, err := s.refMoved(r.Context(), p)
	if err != nil {
		var re *repoError
		if errors.As(err, &re) {
			re.retry(w)
			s.refuse(w, r, FormatRemoteRun, re.class, re.status, "writ serve could not check that %s has not moved since plan %s, so it refuses the apply: %s", p.Source.Ref, p.ID, re.msg)
			return false
		}
		s.refuse(w, r, FormatRemoteRun, diag.SavedPlan, http.StatusConflict, "%v", err)
		return false
	}
	if why != "" {
		if err := s.store.Supersede(r.Context(), p.ID, why, s.now()); err != nil {
			s.dbFailed(w, r, FormatRemoteRun, err)
			return false
		}
		s.refuse(w, r, FormatRemoteRun, diag.SavedPlan, http.StatusConflict, "plan %s is superseded: %s; plan the new commit", p.ID, why)
		return false
	}
	return true
}

// refMoved reports why a commit plan's ref no longer holds what the plan
// was made from: the branch is gone, or its head now has other sources for
// the plan's stack. A branch whose head moved with no change to the
// stack's sources has not moved for the plan. A ref that is not a branch
// cannot be read, so it is an error.
func (s *Server) refMoved(ctx context.Context, p control.Plan) (string, error) {
	branch, ok := strings.CutPrefix(p.Source.Ref, "refs/heads/")
	if !ok {
		return "", fmt.Errorf("plan %s was made from %s, which is not a branch, so writ serve cannot tell whether it moved: apply it after the merge with merged", p.ID, p.Source.Ref)
	}
	rp, err := s.store.Repo(ctx, p.Source.Repo)
	if err != nil {
		return "", &repoError{status: http.StatusConflict, class: diag.SavedPlan, msg: fmt.Sprintf("repository %s: %v", p.Source.Repo, err)}
	}
	c, err := s.clientOf(rp)
	if err != nil {
		return "", &repoError{status: http.StatusConflict, class: diag.SavedPlan, msg: err.Error()}
	}
	cctx, cancel := context.WithTimeout(ctx, forge.CallTimeout)
	head, err := c.Head(cctx, rp.ProjectID, branch)
	cancel()
	if errors.Is(err, forge.ErrNotFound) {
		return fmt.Sprintf("branch %s of repository %s is gone", branch, rp.Name), nil
	}
	if err != nil {
		return "", forgeRefusal(err)
	}
	if head == p.Source.Commit {
		return "", nil
	}
	sources, err := s.commitSources(ctx, rp, c, head, p.Stack)
	if err != nil {
		return "", forgeRefusal(err)
	}
	if sources.Digest == p.SourceDigest {
		return "", nil
	}
	return fmt.Sprintf("%s moved to %s, whose sources of %s differ from the plan's, made at %s", p.Source.Ref, forge.Short(head), p.Stack, forge.Short(p.Source.Commit)), nil
}

type mergedApply struct {
	plan      control.Plan
	commit    string
	appliedBy string
	refusal   *repoError
}

// byMerged finds the plan an apply after a merge names. Refusals that
// describe the selected plan or its state wait for its apply grant.
func (s *Server) byMerged(w http.ResponseWriter, r *http.Request, body RunRequest) (mergedApply, bool) {
	ctx := r.Context()
	pr := reqOf(r).principal
	if len(body.Stacks) != 1 {
		s.refuse(w, r, FormatRemoteRun, diag.Usage, http.StatusBadRequest, "an apply after a merge names exactly one stack")
		return mergedApply{}, false
	}
	stack := body.Stacks[0]
	e, ok := s.target(w, r, FormatRemoteRun, body.Env, stack)
	if !ok {
		return mergedApply{}, false
	}
	env := e.Name
	// Apply on the stack@env comes before any call to the forge, with
	// writ serve's token, so a principal that may not apply there reads
	// nothing of the repository; the plan's risks are checked after. The
	// audit row names the target even when this refuses.
	reqOf(r).entry.Env, reqOf(r).entry.Stacks = env, []string{stack}
	if d := s.cfg.Policy.Decide(pr.teams, authz.Apply, []string{stack}, env, nil); !d.Allowed {
		s.refuse(w, r, FormatRemoteRun, diag.Denied, http.StatusForbidden, "%s may not apply to %s@%s", pr.info.Display, stack, env)
		return mergedApply{}, false
	}
	in, rerr := s.mergedSource(ctx, pr, body.Merged)
	if rerr != nil {
		rerr.retry(w)
		s.refuse(w, r, FormatRemoteRun, rerr.class, rerr.status, "%s", rerr.msg)
		return mergedApply{}, false
	}
	rp, c, commit := in.repo, in.client, in.source.Commit
	if _, ok := stackDir(rp, stack); !ok {
		s.refuse(w, r, FormatRemoteRun, diag.Usage, http.StatusBadRequest, "stack %s is not a stack name of repository %s", stack, rp.Name)
		return mergedApply{}, false
	}
	sources, err := s.commitSources(ctx, rp, c, commit, stack)
	if err != nil {
		rerr := forgeRefusal(err)
		rerr.retry(w)
		s.refuse(w, r, FormatRemoteRun, rerr.class, rerr.status, "read %s at %s: %s", rp.Name, forge.Short(commit), rerr.msg)
		return mergedApply{}, false
	}
	now := s.now()
	p, t, err := s.store.MergedPlan(ctx, rp.Name, env, stack, sources.Digest, s.rules(), now)
	// The run reads the selected plan's commit through rp and checks
	// the sources again, even if the plan's repository was removed.
	p.Source.Repo = rp.Name
	match := mergedApply{plan: p, commit: commit}
	switch {
	case err == nil && t.Status == control.NoChanges:
		why, err := s.store.StateMoved(ctx, p)
		if err != nil {
			s.dbFailed(w, r, FormatRemoteRun, err)
			return mergedApply{}, false
		}
		if why == "" {
			return match, true
		}
		match.refusal = &repoError{status: http.StatusConflict, class: diag.State, msg: fmt.Sprintf(
			"plan %s of %s@%s matches the sources at %s and changes no resource, but %s: plan again",
			p.ID, stack, env, forge.Short(commit), why)}
	case err == nil && t.Status == control.Approved:
		return match, true
	case err == nil:
		match.refusal = &repoError{status: http.StatusForbidden, class: diag.Denied, msg: fmt.Sprintf(
			"plan %s of %s@%s matches the sources at %s and needs %s; approve it with writ approve --remote %s",
			p.ID, stack, env, forge.Short(commit), approvalWords(t.Need), p.ID)}
		return match, true
	case !errors.Is(err, control.ErrNotFound):
		s.dbFailed(w, r, FormatRemoteRun, err)
		return mergedApply{}, false
	}

	// A stale no-changes plan can predate a revert that applied the
	// same sources. In either case, check the newest run.
	last, run, moved, err := s.store.NewestRunApplied(ctx, env, stack, now)
	switch {
	case err == nil && last.SourceDigest == sources.Digest:
		last.Source.Repo = rp.Name
		retry := mergedApply{plan: last, commit: commit, appliedBy: run}
		if sources.Backend == nil || sources.Backend.Type != control.BackendHTTP {
			moved = "writ serve cannot verify that state outside its http backend stayed at the run's version"
		}
		if moved != "" {
			retry.refusal = &repoError{status: http.StatusConflict, class: diag.State, msg: fmt.Sprintf(
				"run %s applied plan %s of %s@%s, which has the sources of %s at %s, but %s: plan again",
				run, last.ID, stack, env, rp.Name, forge.Short(commit), moved)}
		}
		return retry, true
	case err != nil && !errors.Is(err, control.ErrNotFound):
		s.dbFailed(w, r, FormatRemoteRun, err)
		return mergedApply{}, false
	case match.refusal != nil:
		return match, true
	}
	s.refuse(w, r, FormatRemoteRun, diag.SavedPlan, http.StatusConflict, "no approved plan of %s@%s matches the sources of %s at %s: plan the merge request again after a rebase, and approve the new plan",
		stack, env, rp.Name, forge.Short(commit))
	return mergedApply{}, false
}

// mergedSource finds the repository and commit of an apply after a merge,
// and checks the commit is on the repository's default branch.
func (s *Server) mergedSource(ctx context.Context, pr *principal, m *MergedRequest) (planInput, *repoError) {
	var in planInput
	commit := m.Commit
	if pr.tok != nil {
		if pr.tok.Kind != oidc.GitLabCI {
			return in, refusedRepo(http.StatusBadRequest, "an apply after a merge from a %s token is not built yet", pr.tok.Kind)
		}
		repos, err := s.store.ReposOfProject(ctx, control.GitLab, issuerURL(pr.tok), pr.claim("project_id"))
		if err != nil {
			return in, &repoError{status: http.StatusServiceUnavailable, class: diag.StateIO, msg: "the control database failed"}
		}
		for _, rp := range repos {
			if m.Repo == "" && len(repos) == 1 || rp.Name == m.Repo {
				in.repo = rp
			}
		}
		if in.repo.Name == "" {
			return in, &repoError{status: http.StatusForbidden, class: diag.Denied, msg: fmt.Sprintf("project %s (id %s) has no repository %s of this control plane; name one with --repo when it has several", trunc(pr.claim("project_path")), trunc(pr.claim("project_id")), trunc(m.Repo))}
		}
		sha := pr.claim("sha")
		if commit != "" && commit != sha {
			return in, &repoError{status: http.StatusForbidden, class: diag.Denied, msg: fmt.Sprintf("a CI job applies its own commit: the token names %s, not %s", forge.Short(sha), forge.Short(commit))}
		}
		commit = sha
		if ref := pr.claim("ref_path"); ref != "refs/heads/"+in.repo.DefaultBranch {
			return in, &repoError{status: http.StatusForbidden, class: diag.Denied, msg: fmt.Sprintf("an apply after a merge runs on the default branch %s, and the job runs on %s", in.repo.DefaultBranch, trunc(ref))}
		}
	} else {
		if m.Repo == "" || !forge.IsSHA(commit) {
			return in, refusedRepo(http.StatusBadRequest, "an apply after a merge names the repository and the merged commit's full id: merged.repo and merged.commit")
		}
		rp, err := s.store.Repo(ctx, m.Repo)
		if errors.Is(err, control.ErrNotFound) {
			return in, refusedRepo(http.StatusNotFound, "no repository %s", trunc(m.Repo))
		}
		if err != nil {
			return in, &repoError{status: http.StatusServiceUnavailable, class: diag.StateIO, msg: "the control database failed"}
		}
		in.repo = rp
	}
	c, err := s.clientOf(in.repo)
	if err != nil {
		return in, refusedRepo(http.StatusConflict, "%v", err)
	}
	cctx, cancel := context.WithTimeout(ctx, forge.CallTimeout)
	defer cancel()
	full, err := c.Commit(cctx, in.repo.ProjectID, commit)
	if err != nil {
		return in, forgeRefusal(err)
	}
	ref := "refs/heads/" + in.repo.DefaultBranch
	on, err := c.OnRef(cctx, in.repo.ProjectID, full, ref)
	if err != nil {
		rerr := forgeRefusal(err)
		rerr.msg = fmt.Sprintf("writ serve could not check that commit %s is on %s, so it refuses the apply: %v", forge.Short(full), ref, err)
		return in, rerr
	}
	if !on {
		return in, &repoError{status: http.StatusForbidden, class: diag.Denied, msg: fmt.Sprintf("commit %s is not on %s of repository %s, so it is not merged", forge.Short(full), ref, in.repo.Name)}
	}
	in.client = c
	in.source = control.PlanSource{Repo: in.repo.Name, Ref: ref, Commit: full}
	return in, nil
}

// nothingToApply answers an apply of plan p, which changes no resource,
// or which run appliedBy applied already. By id it is a refusal. After a
// merge, whose job runs on every merge and can be retried, it is a 200
// that names the plan and starts no run.
func (s *Server) nothingToApply(w http.ResponseWriter, r *http.Request, p control.Plan, merged, appliedBy string, grants []int) {
	if merged == "" {
		s.refuse(w, r, FormatRemoteRun, diag.State, http.StatusConflict, "plan %s changes no resource; there is nothing to apply", p.ID)
		return
	}
	e := &reqOf(r).entry
	e.Decision, e.Grants = control.Allowed, grants
	e.Reason = fmt.Sprintf("nothing to apply: plan %s changes no resource", p.ID)
	text := fmt.Sprintf("Plan %s of %s@%s has the sources of %s at %s and changes no resource: there is nothing to apply.\n", p.ID, p.Stack, p.Env, p.Source.Repo, forge.Short(merged))
	if appliedBy != "" {
		e.Run = appliedBy
		e.Reason = fmt.Sprintf("nothing to apply: run %s applied plan %s, of the same sources, and the state has not moved since", appliedBy, p.ID)
		text = fmt.Sprintf("Run %s applied plan %s of %s@%s, which has the sources of %s at %s, and the state has not moved since: there is nothing to apply.\n",
			appliedBy, p.ID, p.Stack, p.Env, p.Source.Repo, forge.Short(merged))
	}
	s.log.Info("nothing to apply", "request", reqOf(r).id, "plan", p.ID, "env", p.Env, "stack", p.Stack, "merged", merged, "applied_by", appliedBy)
	s.reply(w, http.StatusOK, RemoteRun{Format: FormatRemoteRun, OK: true, Plan: p.ID, Diagnostics: []Diagnostic{}, Text: text})
}

// refuseApply answers a BeginApply that did not begin.
func (s *Server) refuseApply(w http.ResponseWriter, r *http.Request, p control.Plan, err error) {
	var stale *control.StaleError
	var na *control.NotApplicableError
	switch {
	case errors.Is(err, control.ErrReplay):
		s.authFail(w, r, FormatRemoteRun, "replay", "this token already applied; each CI token applies once")
	case errors.As(err, &stale):
		s.refuse(w, r, FormatRemoteRun, diag.State, http.StatusConflict, "%s", stale.Reason)
	case errors.As(err, &na) && na.Need > na.Have:
		s.refuse(w, r, FormatRemoteRun, diag.Denied, http.StatusForbidden, "%s: approve it with writ approve --remote %s, or apply with an override", na.Msg, p.ID)
	case errors.As(err, &na) && na.Status.Live():
		s.refuse(w, r, FormatRemoteRun, diag.Usage, http.StatusBadRequest, "%s", na.Msg)
	case errors.As(err, &na):
		s.refuse(w, r, FormatRemoteRun, diag.State, http.StatusConflict, "%s", na.Msg)
	case errors.Is(err, control.ErrBusy):
		s.refuse(w, r, FormatRemoteRun, diag.State, http.StatusConflict, "another run is applying to %s@%s; try again when it ends", p.Stack, p.Env)
	case errors.Is(err, control.ErrApprovalsChanged):
		s.refuse(w, r, FormatRemoteRun, diag.State, http.StatusConflict, "%v", err)
	default:
		s.dbFailed(w, r, FormatRemoteRun, err)
	}
}

// approvalWords counts approvals in words: 1 approval, 2 approvals.
func approvalWords(n int) string {
	if n == 1 {
		return "1 approval"
	}
	return fmt.Sprintf("%d approvals", n)
}

// claimApply counts a running apply, within limits.applies.
func (s *Server) claimApply() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.applies >= s.cfg.Limits.Applies {
		return false
	}
	s.applies++
	return true
}

func (s *Server) releaseApply() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applies--
}

// roleSessionOf is the role session name a run's providers get, "" when
// the environment names no apply role.
func (s *Server) roleSessionOf(env, run string, p *principal) string {
	e, ok := s.cfg.Envs[env]
	if !ok || e.role(applyRole) == "" {
		return ""
	}
	return awsrole.SessionName(p.sessionName(), run)
}

// work starts the apply of a run that began, in the background.
func (s *Server) work(run control.Run, plan control.Plan, env Env, p *principal) {
	done := make(chan struct{})
	s.mu.Lock()
	s.runDone[run.ID] = done
	delete(s.pending, run.ID)
	s.mu.Unlock()
	s.wg.Go(func() { s.runApply(run, plan, env, p, done) })
}

// runApply applies one run's saved plan and records how it ended. It is
// the goroutine createRun starts; Close waits for it. Cancelling the
// server's context, or limits.apply_timeout, interrupts OpenTofu, which
// starts no more changes and writes the state it reached.
func (s *Server) runApply(run control.Run, plan control.Plan, env Env, p *principal, done chan struct{}) {
	defer func() {
		s.releaseApply()
		s.mu.Lock()
		delete(s.runDone, run.ID)
		s.mu.Unlock()
		close(done)
	}()
	res := control.RunResult{Status: control.RunFailed}
	applyCtx, cancelApply := s.withLimit(s.ctx, "the apply", "limits.apply_timeout", s.cfg.Limits.ApplyTimeout)
	defer cancelApply()
	out, err := s.apply(applyCtx, run, plan, env, p)
	if err != nil {
		s.log.Error("apply failed", "run", run.ID, "err", err)
		class := diag.Internal
		if ce, ok := errors.AsType[*classedError](err); ok {
			class = ce.class
		}
		res.Document, _ = json.Marshal(applyRefusal{Format: "writ.apply/v0", Status: "apply-failed", Diagnostics: []Diagnostic{{"error", string(class), err.Error()}}})
		res.Text = "writ: " + err.Error() + "\n"
	} else {
		res.Document, res.Text, res.Network = out.Document, out.Text, out.Network
		var head struct {
			OK bool `json:"ok"`
		}
		if json.Unmarshal(out.Document, &head) == nil && head.OK {
			res.Status = control.RunApplied
		}
	}
	res.EndedAt = s.now()
	e := control.Entry{At: res.EndedAt, Request: run.ID, Kind: "apply-finished", Issuer: p.info.Issuer, Subject: p.info.Subject, Person: p.info.Person,
		Principal: p.info.Display, PrincipalKind: p.info.Kind, Via: p.via(), Teams: p.teams, Op: string(authz.Apply), Env: plan.Env, Stacks: []string{plan.Stack},
		Plan: plan.ID, Run: run.ID, Risks: plan.Risks, Decision: control.NoDecision, Reason: string(res.Status), ConfigID: s.configID}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	voided, err := s.store.FinishRun(ctx, run.ID, res, e)
	if err != nil {
		s.log.Error("record the end of a run", "run", run.ID, "err", err)
		return
	}
	s.log.Info("apply ended", "run", run.ID, "plan", plan.ID, "status", res.Status, "elapsed", res.EndedAt.Sub(run.StartedAt).String(), "superseded", voided)
	s.notifyRun(run, plan, res, p)
}

// applyRefusal is writ.apply/v0 for an apply that could not start.
type applyRefusal struct {
	Format      string       `json:"format"`
	OK          bool         `json:"ok"`
	Applied     bool         `json:"applied"`
	Status      string       `json:"status"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// apply applies the run's saved plan, with a start bounded by
// limits.apply_start_timeout.
func (s *Server) apply(ctx context.Context, run control.Run, plan control.Plan, env Env, p *principal) (ApplyOutput, error) {
	// No change starts before the start limit, so ending there leaves
	// state as it was.
	startCtx, cancelStart := s.withLimit(ctx, "the apply's start", "limits.apply_start_timeout", s.cfg.Limits.ApplyStartTimeout)
	defer cancelStart()
	out, err := s.startApply(startCtx, ctx, run, plan, env, p)
	if te := timedOut(startCtx); te != nil && err != nil && !errors.Is(err, te) {
		err = &classedError{diag.Usage, fmt.Errorf("%w: %w", te, err)}
	}
	return out, err
}

// startApply prepares the apply under startCtx and runs the saved plan
// under ctx.
func (s *Server) startApply(startCtx, ctx context.Context, run control.Run, plan control.Plan, env Env, p *principal) (ApplyOutput, error) {
	if plan.Tofu == nil {
		return ApplyOutput{}, &classedError{diag.Usage, fmt.Errorf("plan %s was made by the engine of Writ's retired language, which writ serve no longer runs: plan the stack again as an OpenTofu stack", plan.ID)}
	}
	return s.applyTofu(startCtx, ctx, run, plan, p)
}

// remoteRun is writ.remote-run/v0 of a run.
func (s *Server) remoteRun(run control.Run) RemoteRun {
	doc := RemoteRun{Format: FormatRemoteRun, OK: run.Status != control.RunFailed, Document: run.Document, Text: run.Text, Policy: run.Policy, Network: run.Network, Diagnostics: []Diagnostic{}, Run: runInfo(run)}
	if doc.Document == nil {
		doc.Document = json.RawMessage("null")
	}
	if doc.Policy == nil {
		doc.Policy = json.RawMessage("null")
	}
	if doc.Network == nil {
		doc.Network = json.RawMessage("null")
	}
	return doc
}

func runInfo(run control.Run) *RunInfo {
	var actor Principal
	_ = json.Unmarshal(run.Actor.Detail, &actor)
	return &RunInfo{ID: run.ID, Plan: run.PlanID, Status: string(run.Status), Env: run.Env, Stack: run.Stack, Actor: actor,
		ApprovedBy: approvalInfos(run.Approvals), Override: run.Override, NamedBy: string(run.NamedBy), MergedCommit: run.MergedCommit,
		RoleSession: run.RoleSession, StartedAt: stamp(run.StartedAt), EndedAt: stamp(run.EndedAt)}
}

// listRuns is GET /v1/applies: the runs writ serve started in an
// environment, or of one stack there, newest first, only those of the
// stacks the principal may read.
func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	env, stack, limit := q.Get("env"), q.Get("stack"), 50
	if l := q.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 || n > 200 {
			s.refuse(w, r, FormatRemoteRuns, diag.Usage, http.StatusBadRequest, "limit must be 1 to 200")
			return
		}
		limit = n
	}
	if env == "" {
		s.refuse(w, r, FormatRemoteRuns, diag.Usage, http.StatusBadRequest, "applies needs ?env=")
		return
	}
	if _, ok := s.target(w, r, FormatRemoteRuns, env, cmpOr(stack, "x")); !ok {
		return
	}
	req := reqOf(r)
	runs, more, err := s.store.Runs(r.Context(), env, stack, limit, s.readable(req.principal))
	if err != nil {
		s.dbFailed(w, r, FormatRemoteRuns, err)
		return
	}
	req.entry.Op, req.entry.Env, req.entry.Decision = string(authz.Read), env, control.Allowed
	if stack != "" {
		req.entry.Stacks = []string{stack}
	}
	doc := RemoteRuns{Format: FormatRemoteRuns, OK: true, Runs: []RunInfo{}, More: more, Diagnostics: []Diagnostic{}}
	for _, run := range runs {
		info := runInfo(run.Run)
		info.Sources = run.Sources
		doc.Runs = append(doc.Runs, *info)
	}
	s.reply(w, http.StatusOK, doc)
}

// readable is the rule every list of runs keeps runs by: only those of a
// stack@env p may read.
func (s *Server) readable(p *principal) func(control.Run) bool {
	return func(run control.Run) bool {
		return s.cfg.Policy.Decide(p.teams, authz.Read, []string{run.Stack}, run.Env, nil).Allowed
	}
}

// getRun is GET /v1/runs/{id}. With ?wait=DURATION it holds a running
// run until it ends or the wait passes.
func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !runIDRE.MatchString(id) {
		s.refuse(w, r, FormatRemoteRun, diag.Usage, http.StatusNotFound, "%q is not a run id", trunc(id))
		return
	}
	var wait time.Duration
	if v := r.URL.Query().Get("wait"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			s.refuse(w, r, FormatRemoteRun, diag.Usage, http.StatusBadRequest, "wait must be a duration such as 60s")
			return
		}
		wait = min(d, MaxWait)
	}
	run, err := s.store.GetRun(r.Context(), id)
	if errors.Is(err, control.ErrNotFound) {
		s.refuse(w, r, FormatRemoteRun, diag.Usage, http.StatusNotFound, "no run %s", id)
		return
	}
	if err != nil {
		s.dbFailed(w, r, FormatRemoteRun, err)
		return
	}
	reqOf(r).entry.Run, reqOf(r).entry.Plan = run.ID, run.PlanID
	if !s.decide(w, r, FormatRemoteRun, authz.Read, run.Stack, run.Env) {
		return
	}
	if run.Status == control.RunApplying && wait > 0 {
		s.mu.Lock()
		done := s.runDone[run.ID]
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
		if run, err = s.store.GetRun(r.Context(), id); err != nil {
			s.dbFailed(w, r, FormatRemoteRun, err)
			return
		}
	}
	s.reply(w, http.StatusOK, s.remoteRun(run))
}
