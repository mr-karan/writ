package server

import (
	"cmp"
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mr-karan/writ/internal/authz"
	"github.com/mr-karan/writ/internal/control"
)

// FormatOverview is GET /v1/overview.
const FormatOverview = "writ.overview/v0"

const (
	// overviewLimit bounds each list the overview reads: the stacks, the
	// drift alerts and failed runs of each environment, and the plans of
	// each live status in each environment.
	overviewLimit = 200
	// failedWindow is how far back the overview counts failed runs.
	failedWindow = 24 * time.Hour
	// expiringWindow is how soon a plan that waits must expire to be an
	// alert, at most a quarter of the plan lifetime.
	expiringWindow = 2 * time.Hour
)

// OverviewDoc is GET /v1/overview: what writ serve does in every
// environment the caller may read, in one answer, so the control UI's
// sidebar and overview cost one read. It holds only what GET /v1/drift,
// /v1/plans, /v1/applies, and /v1/repos would answer the caller.
type OverviewDoc struct {
	Format string        `json:"format"`
	OK     bool          `json:"ok"`
	Envs   []OverviewEnv `json:"envs"`
	// Stacks are the stacks of every environment in Envs, by env and then
	// stack: the first 200 whose state writ serve keeps or reads, and
	// those a live plan names that have no state. StacksMore is set when
	// more than 200 stacks have state; a stack with state past the 200
	// is left out, never listed as one with none.
	Stacks     []OverviewStack `json:"stacks"`
	StacksMore bool            `json:"stacks_more"`
	// Applying, Pending, and Planning are the plans of each status as
	// GET /v1/plans/{id} shows it, newest first.
	Applying []LivePlan `json:"applying"`
	Pending  []LivePlan `json:"pending"`
	Planning []LivePlan `json:"planning"`
	// Alerts lists, of each environment, the drift alerts of at most 200
	// stacks and the newest 200 failed runs, then the plans that expire
	// soon and the repositories whose poll failed. Each environment's
	// alerts_more says whether it left its own out.
	Alerts []Alert      `json:"alerts"`
	Repos  *RepoPolls   `json:"repos"`
	Diags  []Diagnostic `json:"diagnostics"`
}

// OverviewEnv is one environment the caller may read, with its counts.
type OverviewEnv struct {
	Name string `json:"name"`
	// Plans counts the plans of each live status, at most 200 of each
	// stored status that the caller may read. PlansMore is set when the
	// caller may read more, so the counts are floors and a plan about to
	// expire may be missing from the alerts.
	Plans     LiveCounts `json:"plans"`
	PlansMore bool       `json:"plans_more"`
	// AlertsMore is set when the alerts leave out drift alerts or failed
	// runs of this environment that the caller may read.
	AlertsMore bool `json:"alerts_more"`
	// Failed counts the runs that failed in the last 24 hours.
	Failed int `json:"failed"`
	// Stacks counts the stacks with state, Checked those with a drift
	// check that ended, Drifted those whose last check found drift, and
	// CheckFailed those whose last check failed.
	Stacks      int `json:"stacks"`
	Checked     int `json:"checked"`
	Drifted     int `json:"drifted"`
	CheckFailed int `json:"check_failed"`
}

// LiveCounts counts plans by live status.
type LiveCounts struct {
	Planning int `json:"planning"`
	Pending  int `json:"pending"`
	Approved int `json:"approved"`
	Applying int `json:"applying"`
}

func (c *LiveCounts) add(status string) {
	switch status {
	case PlanPlanning:
		c.Planning++
	case PlanPending:
		c.Pending++
	case PlanApproved:
		c.Approved++
	case PlanApplying:
		c.Applying++
	}
}

// OverviewStack is one stack@env and the live plans that name it.
type OverviewStack struct {
	Env   string     `json:"env"`
	Stack string     `json:"stack"`
	Plans LiveCounts `json:"plans"`
	// Drift is the stack's row of GET /v1/drift, null for a stack with no
	// state yet.
	Drift *DriftStack `json:"drift"`
}

// LivePlan is a plan and its run, null before one began.
type LivePlan struct {
	Plan PlanInfo `json:"plan"`
	Run  *RunInfo `json:"run"`
}

// Alert is something that needs a person: what kind, what it is about,
// and when writ serve saw it. Message says what happened in words; the
// ids are in the fields.
type Alert struct {
	// Kind is one of the Alert kinds below.
	Kind    string `json:"kind"`
	Env     string `json:"env"`
	Stack   string `json:"stack"`
	Plan    string `json:"plan"`
	Run     string `json:"run"`
	Drift   string `json:"drift,omitempty"`
	Repo    string `json:"repo"`
	Message string `json:"message"`
	At      string `json:"at"`
}

// The kinds of an Alert.
const (
	AlertDrifted          = "drifted"
	AlertDriftCheckFailed = "drift-check-failed"
	AlertOutsideWrite     = "outside-write"
	AlertRunFailed        = "run-failed"
	AlertPlanExpiring     = "plan-expiring"
	AlertRepoPollFailed   = "repo-poll-failed"
)

// RepoPolls is how the repositories' polls went.
type RepoPolls struct {
	Count int `json:"count"`
	// PolledAt is the newest poll of any repository, "" before the first.
	PolledAt string `json:"polled_at"`
	// Failed are the repositories whose last poll failed, by name.
	Failed []RepoPoll `json:"failed"`
}

// RepoPoll is one repository's failed poll.
type RepoPoll struct {
	Name    string `json:"name"`
	Error   string `json:"error"`
	ErrorAt string `json:"error_at"`
}

// overview is GET /v1/overview. It keeps each stack, plan, and run by the
// rule of the route that lists it: read on its stack@env. Every read below
// drops what the caller may not read before it counts or cuts, so nothing
// the caller may not read changes the answer.
func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	req := reqOf(r)
	p := req.principal
	ctx := r.Context()
	envs, err := s.environments(ctx)
	if err != nil {
		s.dbFailed(w, r, FormatOverview, err)
		return
	}
	doc := OverviewDoc{Format: FormatOverview, OK: true, Envs: []OverviewEnv{}, Stacks: []OverviewStack{},
		Applying: []LivePlan{}, Pending: []LivePlan{}, Planning: []LivePlan{}, Alerts: []Alert{}, Diags: []Diagnostic{}}
	byEnv := map[string]*OverviewEnv{}
	for _, name := range envs {
		if s.cfg.Policy.InEnv(p.teams, authz.Read, name) {
			doc.Envs = append(doc.Envs, OverviewEnv{Name: name})
		}
	}
	for i := range doc.Envs {
		byEnv[doc.Envs[i].Name] = &doc.Envs[i]
	}
	reads := func(env, stack string) bool {
		_, served := s.envOf(env)
		return byEnv[env] != nil && served && s.cfg.Policy.Decide(p.teams, authz.Read, []string{stack}, env, nil).Allowed
	}
	readsRun := func(run control.Run) bool { return reads(run.Env, run.Stack) }

	targets, err := s.store.DriftTargets(ctx, reads)
	if err != nil {
		s.dbFailed(w, r, FormatOverview, err)
		return
	}
	type key struct{ env, stack string }
	hasState := map[key]bool{}
	for _, t := range targets {
		hasState[key{t.Env, t.Stack}] = true
		e := byEnv[t.Env]
		e.Stacks++
		if t.Last != "" {
			e.Checked++
		}
		switch t.Last {
		case control.DriftDrifted:
			e.Drifted++
		case control.DriftFailed:
			e.CheckFailed++
		}
	}

	drift, more, err := s.store.DriftStacks(ctx, "", "", overviewLimit, reads)
	if err != nil {
		s.dbFailed(w, r, FormatOverview, err)
		return
	}
	doc.StacksMore = more
	stacks := map[key]*OverviewStack{}
	for _, st := range drift {
		stacks[key{st.Env, st.Stack}] = &OverviewStack{Env: st.Env, Stack: st.Stack, Drift: driftRow(st)}
	}
	// at is the listed stack a plan names, or nil for a stack with state
	// that the list left out.
	at := func(env, stack string) *OverviewStack {
		k := key{env, stack}
		if stacks[k] == nil && !hasState[k] {
			stacks[k] = &OverviewStack{Env: env, Stack: stack}
		}
		return stacks[k]
	}

	for i := range doc.Envs {
		e := &doc.Envs[i]
		needs, more, err := s.store.DriftAlerts(ctx, e.Name, overviewLimit, reads)
		if err != nil {
			s.dbFailed(w, r, FormatOverview, err)
			return
		}
		e.AlertsMore = more
		for _, st := range needs {
			d := driftRow(st)
			alert := Alert{Env: st.Env, Stack: st.Stack}
			if c := d.Check; c != nil {
				switch c.Status {
				case DriftDrifted:
					a := alert
					a.Kind, a.Drift, a.At = AlertDrifted, c.Ref, cmp.Or(c.DriftedSince, c.CheckedAt)
					a.Message = plural(c.Outside, "change") + " outside OpenTofu, " + plural(c.Code, "change") + " the code would make"
					doc.Alerts = append(doc.Alerts, a)
				case DriftFailed:
					a := alert
					a.Kind, a.Drift, a.At, a.Message = AlertDriftCheckFailed, c.Ref, c.CheckedAt, c.Error
					doc.Alerts = append(doc.Alerts, a)
				}
			}
			if o := d.OutsideWrite; o != nil {
				a := alert
				a.Kind, a.At, a.Message = AlertOutsideWrite, o.SeenAt, "serial "+strconv.FormatInt(o.Serial, 10)+" of the state was written outside writ serve"
				doc.Alerts = append(doc.Alerts, a)
			}
		}
	}

	// Each environment is read as GET /v1/plans?env=&status= reads it, so
	// another environment's plans never crowd its own out. A plan whose
	// status changes between two reads comes back from both, so each is
	// kept once, as the later read found it.
	now, rules := s.now(), s.rules()
	var plans []control.Plan
	seen := map[string]int{}
	for i := range doc.Envs {
		e := &doc.Envs[i]
		for _, status := range []string{PlanPlanning, PlanPending, PlanApproved, PlanApplying} {
			got, next, err := s.store.Plans(ctx, control.PlanFilter{Env: e.Name, Status: status, Rules: rules, Limit: overviewLimit,
				Keep: reads}, now)
			if err != nil {
				s.dbFailed(w, r, FormatOverview, err)
				return
			}
			for _, pl := range got {
				if j, ok := seen[pl.Ref]; ok {
					plans[j] = pl
					continue
				}
				seen[pl.Ref] = len(plans)
				plans = append(plans, pl)
			}
			e.PlansMore = e.PlansMore || next != ""
		}
	}
	slices.SortStableFunc(plans, func(x, y control.Plan) int {
		return cmp.Or(y.RequestedAt.Compare(x.RequestedAt), strings.Compare(y.Ref, x.Ref))
	})
	var ids []string
	for _, pl := range plans {
		if pl.ID != "" {
			ids = append(ids, pl.ID)
		}
	}
	approvals, err := s.store.Approvals(ctx, ids)
	if err != nil {
		s.dbFailed(w, r, FormatOverview, err)
		return
	}
	runs, err := s.store.PlanRuns(ctx, ids)
	if err != nil {
		s.dbFailed(w, r, FormatOverview, err)
		return
	}
	live := map[string][]PlanInfo{}
	for _, pl := range plans {
		info := s.planInfo(pl, approvals[pl.ID])
		live[info.Status] = append(live[info.Status], *info)
		byEnv[pl.Env].Plans.add(info.Status)
		if st := at(pl.Env, pl.Stack); st != nil {
			st.Plans.add(info.Status)
		}
	}
	for status, into := range map[string]*[]LivePlan{PlanApplying: &doc.Applying, PlanPending: &doc.Pending, PlanPlanning: &doc.Planning} {
		for _, pl := range live[status] {
			lp := LivePlan{Plan: pl}
			if run, ok := runs[pl.ID]; ok {
				lp.Run = runInfo(run)
			}
			*into = append(*into, lp)
		}
	}

	since := now.Add(-failedWindow)
	var failed []RunInfo
	for i := range doc.Envs {
		e := &doc.Envs[i]
		got, total, err := s.store.FailedRuns(ctx, e.Name, since, overviewLimit, readsRun)
		if err != nil {
			s.dbFailed(w, r, FormatOverview, err)
			return
		}
		e.Failed = total
		e.AlertsMore = e.AlertsMore || total > len(got)
		for _, run := range got {
			failed = append(failed, *runInfo(run))
		}
	}

	for _, st := range stacks {
		doc.Stacks = append(doc.Stacks, *st)
	}
	slices.SortFunc(doc.Stacks, func(a, b OverviewStack) int { return cmp.Or(cmp.Compare(a.Env, b.Env), cmp.Compare(a.Stack, b.Stack)) })
	slices.SortStableFunc(failed, func(x, y RunInfo) int { return strings.Compare(y.EndedAt, x.EndedAt) })
	for _, run := range failed {
		doc.Alerts = append(doc.Alerts, Alert{Kind: AlertRunFailed, Env: run.Env, Stack: run.Stack, Plan: run.Plan, Run: run.ID, At: run.EndedAt,
			Message: "the apply by " + run.Actor.Display + " failed"})
	}
	soon := min(expiringWindow, s.cfg.PlanTTL/4)
	for _, pl := range append(slices.Clone(live[PlanPending]), live[PlanApproved]...) {
		exp, err := time.Parse(time.RFC3339, pl.ExpiresAt)
		if err != nil || exp.Sub(now) > soon {
			continue
		}
		doc.Alerts = append(doc.Alerts, Alert{Kind: AlertPlanExpiring, Env: pl.Env, Stack: pl.Stacks[0].Stack, Plan: cmp.Or(pl.ID, pl.Ref), At: pl.ExpiresAt,
			Message: "the " + pl.Status + " plan of " + pl.Author.Display + " expires"})
	}

	if s.mayAny(p, authz.Read) {
		repos, err := s.store.Repos(ctx)
		if err != nil {
			s.dbFailed(w, r, FormatOverview, err)
			return
		}
		doc.Repos = &RepoPolls{Count: len(repos), Failed: []RepoPoll{}}
		for _, rp := range repos {
			doc.Repos.PolledAt = max(doc.Repos.PolledAt, stamp(rp.PolledAt))
			if rp.Error != "" {
				doc.Repos.Failed = append(doc.Repos.Failed, RepoPoll{Name: rp.Name, Error: rp.Error, ErrorAt: stamp(rp.ErrorAt)})
				doc.Alerts = append(doc.Alerts, Alert{Kind: AlertRepoPollFailed, Repo: rp.Name, Message: rp.Error, At: stamp(cmp.Or(rp.ErrorAt, rp.PolledAt))})
			}
		}
	}

	req.entry.Op, req.entry.Decision = string(authz.Read), control.Allowed
	for _, st := range doc.Stacks {
		req.entry.Stacks = append(req.entry.Stacks, st.Stack)
	}
	for _, a := range doc.Alerts {
		if a.Stack != "" {
			req.entry.Stacks = append(req.entry.Stacks, a.Stack)
		}
	}
	slices.Sort(req.entry.Stacks)
	req.entry.Stacks = slices.Compact(req.entry.Stacks)
	s.reply(w, http.StatusOK, doc)
}

// environments are the environments writ serve plans in. The dev control
// plane serves every environment, so it lists the ones that hold state or
// a plan.
func (s *Server) environments(ctx context.Context) ([]string, error) {
	envs := s.envNames()
	if !s.cfg.Dev {
		return envs, nil
	}
	known, err := s.store.Environments(ctx)
	if err != nil {
		return nil, err
	}
	for _, env := range known {
		if !slices.Contains(envs, env) {
			envs = append(envs, env)
		}
	}
	return envs, nil
}

func plural(n int, one string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + one + "s"
}
