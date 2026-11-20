package ui

import (
	"cmp"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/mr-karan/writ/internal/server"
)

// activity gets GET /v1/overview once per page: what writ serve does in
// every environment the signed-in person may read. The sidebar and the
// overview read it, so both count the same. It is nil when the document
// did not come. record lists it in the page's footer.
func (f *fetch) activity(record bool) *server.OverviewDoc {
	get := f.quiet
	if record {
		get = f.also
	}
	var doc server.OverviewDoc
	if !get("/v1/overview", &doc) {
		return nil
	}
	return &doc
}

// envsOf names the environments of doc, an overview that came, or the
// dev environment on a dev control plane that holds none yet.
func (f *fetch) envsOf(doc *server.OverviewDoc) []string {
	var out []string
	for _, e := range doc.Envs {
		out = append(out, e.Name)
	}
	if len(out) == 0 && f.u.srv.Dev() {
		out = []string{server.DevEnv}
	}
	return out
}

// figure is a number the sidebar or the overview shows: exact, or a floor
// that the page marks with a +, because writ serve read only some of the
// rows it counts. Every number of the two goes through template n.
type figure struct {
	N     int
	Floor bool
}

// None reports whether the figure is known to be 0, which a floor never is.
func (f figure) None() bool { return f.N == 0 && !f.Floor }

func exact(n int) figure { return figure{N: n} }

// noun is one or many, as f counts it: a floor of 1 is many.
func noun(f figure, one, many string) string {
	if f.N == 1 && !f.Floor {
		return one
	}
	return many
}

// nones are the sentences of the pages that say a thing is absent or that
// all is well. A template says one only through none, and the render
// checks hold each to what the reader may read.
var nones = map[string]string{
	"nothing":   "Nothing waits and Writ keeps no stack yet.",
	"calm":      "No drift, no failed run in the last 24 hours, no plan about to expire, and no failed repository poll.",
	"steady":    "No drift, no failed run in the last 24 hours, and no failed repository poll.",
	"applying":  "No plan is applying.",
	"waiting":   "No plan waits for approval.",
	"stateless": "Writ keeps the state of no OpenTofu stack yet. A stack appears here after its first apply.",
	"no-stack":  "No stack yet.",
	"no-repo":   "No repository yet",
	"no-poll":   "No repository polled yet",

	"no-others":     "No plan by someone else waits here.",
	"no-mine":       "No plan of yours waits here.",
	"no-plan":       "No plan matches.",
	"no-run":        "Writ has started no run here.",
	"no-stack-plan": "No plan of this stack.",
	"no-state":      "Writ keeps or reads no state of this stack yet.",
	"unindexed":     "Writ has a state of this stack but has not indexed it yet, so it lists no resource.",
	"no-resource":   "The state index holds no resource of this stack.",
	"unchecked":     "No drift check of this stack has run.",
	"no-secret":     "No secret set. Writ keeps no detail of a plan of this state and approves none.",
	"no-found":      "The commit read holds no stack: no directory under the path has a committed .terraform.lock.hcl and .tf or .tofu files.",
	"no-team":       "None. No team's rule matches your claims, so you may read nothing.",
	"no-grant":      "No grant names one of your teams.",
	"no-plan-yet":   "No plan yet.",
	"no-audit":      "No audit entry matches.",
}

// none is the sentence of nones under key when each figure of on is known
// to be 0, and "" when any is not, so the page says something else.
func none(key string, on ...figure) (string, error) {
	text, ok := nones[key]
	if !ok {
		return "", fmt.Errorf("no sentence %q in nones", key)
	}
	for _, f := range on {
		if !f.None() {
			return "", nil
		}
	}
	return text, nil
}

// shell is the sidebar: the sections with their counts, each
// environment's stacks, and when the repositories were last polled.
type shell struct {
	Counts map[string]count
	Envs   []envNode
	// Find filters the stacks by name. FindAction is the page the filter
	// form sends to, and Keep the rest of its query. Matches counts the
	// listed stacks that match, a floor when the tree left some out.
	Find       string
	FindAction string
	Keep       [][2]string
	FindClear  string
	Matches    figure
	StacksMore bool
	Repos      *repoHealth
	// Failed reports that the overview's read failed, so the sidebar
	// shows no count and no tree.
	Failed bool
}

// count is a number beside a section, and what it counts for a reader
// who does not see the layout.
type count struct {
	Fig  figure
	What string
}

// envNode is one environment of the sidebar's tree. Its tone comes from
// the environment's counts, not from the stacks the tree lists. Count is
// its stacks with state or a live plan, and More those with state that the
// tree leaves out.
type envNode struct {
	Name   string
	Tone   string
	Stacks []stackNode
	Count  figure
	More   figure
}

// stackNode is one stack@env: its look, and what it waits on in words, so
// the dot is never the only sign. Quiet is set when all is well with it,
// so only a screen reader hears its note.
type stackNode struct {
	Name, Env, Tone string
	Note            []part
	Current, Quiet  bool
}

// part is one piece of a tree note: a count and what it counts, such as
// 2+ pending, or words alone, such as clean.
type part struct {
	N    *figure
	Word string
}

func counted(f figure, word string) part { return part{N: &f, Word: word} }

// repoHealth is the sidebar's footer: the newest poll of a repository, and
// how many polls failed.
type repoHealth struct {
	Count    figure
	PolledAt string
	Failed   figure
}

func (f *fetch) shell(p *page) *shell {
	a := f.activity(false)
	q := f.r.URL.Query()
	sh := &shell{Counts: map[string]count{}, Find: strings.TrimSpace(q.Get("find")), FindAction: f.r.URL.Path}
	for k, vs := range q {
		if k == "find" || k == "as" {
			continue
		}
		for _, v := range vs {
			sh.Keep = append(sh.Keep, [2]string{k, v})
		}
	}
	slices.SortFunc(sh.Keep, func(x, y [2]string) int { return strings.Compare(x[0], y[0]) })
	q.Del("find")
	sh.FindClear = f.r.URL.Path
	if len(q) > 0 {
		sh.FindClear += "?" + q.Encode()
	}
	if a == nil {
		sh.Failed = true
		return sh
	}
	sh.StacksMore = a.StacksMore
	more := plansMore(a, "")
	sh.Counts["approvals"] = count{figure{len(a.Pending), more}, "waiting"}
	sh.Counts["runs"] = count{figure{len(a.Applying), more}, server.PlanApplying}
	if r := a.Repos; r != nil {
		sh.Repos = &repoHealth{Count: exact(r.Count), PolledAt: r.PolledAt, Failed: exact(len(r.Failed))}
		sh.Counts["repos"] = count{exact(r.Count), "repositories"}
	}
	sh.Envs = tree(a, f.envsOf(a), p.Here.Env, p.Here.Stack, sh.Find)
	// A stack a plan names before its first apply is listed only when
	// writ serve read that plan.
	sh.Matches.Floor = a.StacksMore || more
	for _, e := range sh.Envs {
		sh.Matches.N += len(e.Stacks)
	}
	return sh
}

// plansMore reports whether writ serve read only some of the live plans
// of env, or of any environment when env is "", so their counts are
// floors.
func plansMore(a *server.OverviewDoc, env string) bool {
	return slices.ContainsFunc(a.Envs, func(e server.OverviewEnv) bool { return e.PlansMore && (env == "" || e.Name == env) })
}

// tree lists each environment's stacks: those whose state writ serve keeps
// or reads, and those a live plan names before their first apply. With
// find, it keeps an environment only when a listed stack matches or the
// tree may have left one of its stacks out.
func tree(a *server.OverviewDoc, envs []string, hereEnv, hereStack, find string) []envNode {
	var out []envNode
	for _, env := range envs {
		i := slices.IndexFunc(a.Envs, func(e server.OverviewEnv) bool { return e.Name == env })
		var e server.OverviewEnv
		if i >= 0 {
			e = a.Envs[i]
		}
		n := envNode{Name: env, Tone: envTone(e), Count: figure{e.Stacks, e.PlansMore}, More: exact(e.Stacks)}
		for _, st := range a.Stacks {
			if st.Env != env {
				continue
			}
			if st.Drift != nil {
				n.More.N--
			} else {
				n.Count.N++
			}
			if find != "" && !strings.Contains(strings.ToLower(st.Stack), strings.ToLower(find)) {
				continue
			}
			tone, note := look(st, e.PlansMore)
			n.Stacks = append(n.Stacks, stackNode{Name: st.Stack, Env: env, Tone: tone, Note: note, Current: env == hereEnv && st.Stack == hereStack, Quiet: tone == "ok"})
		}
		if find != "" && len(n.Stacks) == 0 && n.More.None() && !e.PlansMore {
			continue
		}
		out = append(out, n)
	}
	return out
}

// envTone is the tone that asks most of a reader among an environment's
// stacks: a failed drift check, then work in progress, then a wait on a
// person or drift, then checked. Checked is all clear, so it needs every
// live plan read.
func envTone(e server.OverviewEnv) string {
	switch {
	case e.CheckFailed > 0:
		return "bad"
	case e.Plans.Planning > 0 || e.Plans.Applying > 0:
		return "run"
	case e.Plans.Pending > 0 || e.Plans.Approved > 0 || e.Drifted > 0:
		return "wait"
	case e.Checked > 0 && !e.PlansMore:
		return "ok"
	}
	return "none"
}

// look is a stack's tone and its words: what writ serve does with it,
// then what waits on a person, then its drift. more marks its plan
// counts as floors, so a stack none of whose plans was read is never all
// clear and says so.
func look(st server.OverviewStack, more bool) (tone string, note []part) {
	note = liveNote(st.Plans, more)
	var check *server.DriftCheckInfo
	if st.Drift != nil {
		check = st.Drift.Check
	}
	switch {
	case st.Plans.Planning > 0 || st.Plans.Applying > 0:
		tone = "run"
	case check != nil && check.Status == server.DriftFailed:
		tone = "bad"
	case st.Plans.Pending > 0 || st.Plans.Approved > 0:
		tone = "wait"
	case check != nil && check.Status == server.DriftDrifted:
		tone = "wait"
	case check != nil && !more:
		tone = "ok"
	default:
		tone = "none"
	}
	if len(note) == 0 {
		if more {
			note = append(note, counted(figure{0, true}, "plans"))
		}
		var word string
		switch {
		case st.Drift == nil:
			word = "no state yet"
		case check == nil:
			word = "not checked"
		case check.Status == server.DriftFailed:
			word = "check failed"
		default:
			word = check.Status
		}
		note = append(note, part{Word: word})
	}
	return tone, note
}

// liveNote counts plans by live status, such as 1 applying · 1 pending, or
// 1+ applying when more marks the counts as floors. It leaves a status of
// no plan read out.
func liveNote(n server.LiveCounts, more bool) []part {
	var parts []part
	for _, c := range []struct {
		n    int
		word string
	}{{n.Applying, server.PlanApplying}, {n.Planning, server.PlanPlanning}, {n.Pending, server.PlanPending}, {n.Approved, server.PlanApproved}} {
		if c.n > 0 {
			parts = append(parts, counted(figure{c.n, more}, c.word))
		}
	}
	return parts
}

// overviewData is the overview: the counts of what waits and works, the
// alerts, and the lists behind them.
type overviewData struct {
	Env  string
	Envs []string
	// Stacks are the stacks of Env, or of every environment, whose state
	// writ serve keeps or reads, with their drift. StackCount is how many
	// have state, and StacksLeft how many of them writ serve did not list.
	Stacks     []server.DriftStack
	StackCount figure
	StacksLeft figure
	// Waiting are the plans that wait for approval, Mine how many of them
	// the signed-in person made and Others how many the rest made,
	// Applying and Planning the plans writ serve works on, and Live every
	// plan of a live status, approved ones included. PlansMore is set when
	// writ serve read only some of the plans, so these are floors and the
	// alerts may miss a plan about to expire.
	Waiting, Applying, Planning figure
	Mine, Others, Live          figure
	PlansMore                   bool
	// Failed counts the runs that failed in the last day, Drifted the
	// stacks whose last check found drift, of Checked.
	Failed  figure
	Drifted figure
	Checked figure
	// AlertsMore is set when writ serve listed only some of the drift
	// alerts or failed runs of the environments shown. AlertCount counts
	// the alerts, and Steady those that are not of a plan about to expire.
	Alerts     []alert
	AlertsMore bool
	AlertCount figure
	Steady     figure
	// The overview's lists, one row a plan.
	ApplyingRows, WaitingRows, PlanningRows []listRow
}

// listRow is one row of a list on the overview: a dot, the stack@env, the
// commit and the author, and how long ago it began.
type listRow struct {
	Href, Tone, Title, Commit, Author string
	// Record names the record that holds the author, and Field the
	// author's field in it.
	Record, Field string
	At, AtWord    string
	Mine          bool
}

func (f *fetch) row(p server.PlanInfo, tone, href, at, atWord string) listRow {
	r := listRow{Href: href, Tone: tone, Title: planWhere(p), Author: p.Author.Display, Record: planRecord(&p), Field: "author.display", At: at, AtWord: atWord, Mine: p.Author.Person == f.sess.Person}
	if p.Source.Commit != "" {
		r.Commit = short(p.Source.Commit)
	}
	return r
}

// alert is one row of the overview's alerts: what happened, to what, and
// when writ serve saw it.
type alert struct {
	Tone, Kind, Subject, Detail, Href string
	// Record names the alert's record, whose message is Detail.
	Record string
	// At is an RFC 3339 time, and AtWord says what it is the time of.
	At, AtWord string
}

// alertOf is the row of one alert of writ.overview/v0.
func alertOf(a server.Alert) alert {
	r := alert{Subject: a.Stack + "@" + a.Env, Href: link("/ui/stack", "stack", a.Stack, "env", a.Env), Detail: a.Message, At: a.At,
		Record: record("kind", a.Kind, "env", a.Env, "stack", a.Stack)}
	switch a.Kind {
	case server.AlertDrifted:
		r.Tone, r.Kind, r.AtWord = "wait", "Drift found", "first found"
		if a.Drift != "" {
			r.Href = "/ui/drift/" + url.PathEscape(a.Drift)
		}
	case server.AlertDriftCheckFailed:
		r.Tone, r.Kind, r.AtWord = "bad", "Drift check failed", "checked"
		if a.Drift != "" {
			r.Href = "/ui/drift/" + url.PathEscape(a.Drift)
		}
	case server.AlertOutsideWrite:
		r.Tone, r.Kind, r.AtWord = "wait", "State written outside Writ", "seen"
	case server.AlertRunFailed:
		r.Tone, r.Kind, r.AtWord, r.Href, r.Record = "bad", "Run failed", "ended", "/ui/runs/"+a.Run, record("kind", a.Kind, "run", a.Run)
	case server.AlertPlanExpiring:
		r.Tone, r.Kind, r.AtWord, r.Href, r.Record = "wait", "Plan expires soon", "expires", "/ui/plans/"+a.Plan, record("kind", a.Kind, "plan", a.Plan)
	case server.AlertRepoPollFailed:
		r.Tone, r.Kind, r.AtWord, r.Subject, r.Href, r.Record = "bad", "Repository poll failed", "failed", a.Repo, "/ui/repos/"+a.Repo, record("kind", a.Kind, "repo", a.Repo)
	}
	return r
}

// newOverview sorts the activity of env, or of every environment when env
// is "", into the overview.
func (f *fetch) newOverview(env string, a *server.OverviewDoc) overviewData {
	in := func(e string) bool { return env == "" || e == env }
	more := plansMore(a, env)
	d := overviewData{Env: env, Envs: f.envsOf(a), PlansMore: more}
	for _, fig := range []*figure{&d.Waiting, &d.Applying, &d.Planning, &d.Mine, &d.Others, &d.Live} {
		fig.Floor = more
	}
	for _, e := range a.Envs {
		if in(e.Name) {
			d.Failed.N += e.Failed
			d.StackCount.N += e.Stacks
			d.Drifted.N += e.Drifted
			d.Checked.N += e.Checked
			d.AlertsMore = d.AlertsMore || e.AlertsMore
			d.Live.N += e.Plans.Planning + e.Plans.Pending + e.Plans.Approved + e.Plans.Applying
		}
	}
	for _, st := range a.Stacks {
		if st.Drift != nil && in(st.Env) {
			d.Stacks = append(d.Stacks, *st.Drift)
		}
	}
	d.StacksLeft = exact(d.StackCount.N - len(d.Stacks))
	for _, lp := range a.Applying {
		if p := lp.Plan; in(p.Env) {
			d.Applying.N++
			row := f.row(p, "run", "/ui/plans/"+planName(&p), "", "started")
			// The row links the run, so it names the plan by its run.
			if r := lp.Run; r != nil && r.Status == server.RunApplying {
				row.Href, row.At = "/ui/runs/"+r.ID, r.StartedAt
				row.Record, row.Field = record("run.id", r.ID), "plan.author.display"
			}
			d.ApplyingRows = append(d.ApplyingRows, row)
		}
	}
	for _, lp := range a.Pending {
		if p := lp.Plan; in(p.Env) {
			d.Waiting.N++
			if p.Author.Person == f.sess.Person {
				d.Mine.N++
			} else {
				d.Others.N++
			}
			d.WaitingRows = append(d.WaitingRows, f.row(p, "wait", "/ui/plans/"+planName(&p), cmp.Or(p.MadeAt, p.RequestedAt), "made"))
		}
	}
	for _, lp := range a.Planning {
		if p := lp.Plan; in(p.Env) {
			d.Planning.N++
			d.PlanningRows = append(d.PlanningRows, f.row(p, "run", "/ui/plans/"+planName(&p), p.RequestedAt, "asked"))
		}
	}
	for _, al := range a.Alerts {
		if al.Repo != "" || in(al.Env) {
			d.Alerts = append(d.Alerts, alertOf(al))
			if al.Kind != server.AlertPlanExpiring {
				d.Steady.N++
			}
		}
	}
	d.AlertCount = figure{len(d.Alerts), d.AlertsMore || more}
	d.Steady.Floor = d.AlertsMore
	return d
}

// planWhere names the stacks of a plan in its environment, such as
// "app@prod".
func planWhere(p server.PlanInfo) string {
	var names []string
	for _, s := range p.Stacks {
		names = append(names, s.Stack)
	}
	if len(names) == 0 {
		return p.Env
	}
	return strings.Join(names, ", ") + "@" + p.Env
}
