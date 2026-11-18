package ui

import (
	"net/http"
	"net/url"

	"github.com/mr-karan/writ/internal/server"
)

// appliesTable is the data of the applies-table template: the runs writ
// serve started.
type appliesTable struct {
	Caption string
	Runs    []server.RunInfo
	More    bool
}

// applies gets the runs writ serve started in env, of stack when it is
// not "", as the page's main document.
func (f *fetch) applies(caption, env, stack string) (*appliesTable, bool) {
	var doc server.RemoteRuns
	if !f.main(link("/v1/applies", "env", env, "stack", stack), &doc) {
		return nil, false
	}
	return &appliesTable{Caption: caption, Runs: doc.Runs, More: doc.More}, true
}

type runsData struct {
	Stack, Env string
	Envs       []string
	Applies    *appliesTable
}

// runs lists the runs writ serve started in an environment, of one stack
// or of every one.
func (u *UI) runs(w http.ResponseWriter, r *http.Request, s *server.Session) {
	f := u.fetcher(w, r, s)
	q := r.URL.Query()
	d := runsData{Stack: q.Get("stack")}
	var ok bool
	if d.Env, d.Envs, ok = f.listEnv(q.Get("env")); !ok {
		return
	}
	p := &page{Title: "Runs", Section: "runs", Crumbs: []crumb{{"Runs", "/ui/runs"}, {d.Env, ""}}}
	if d.Stack != "" {
		p.Crumbs[1].Href = link("/ui/runs", "env", d.Env)
		p.Crumbs = append(p.Crumbs, crumb{Name: d.Stack})
	}
	caption := "Runs writ serve started in " + d.Env + ", newest first"
	if d.Stack != "" {
		caption = "Runs of " + d.Stack + " in " + d.Env + " that writ serve started, newest first"
	}
	if d.Applies, ok = f.applies(caption, d.Env, d.Stack); !ok {
		return
	}
	p.CLI = "writ runs --remote --env " + d.Env
	if d.Stack != "" {
		p.CLI += " --stack " + d.Stack
	}
	p.Data = d
	f.render("runs", p)
}

// run shows one run writ serve started, from its writ.remote-run/v0, with
// its masked events and diagnostics.
func (u *UI) run(w http.ResponseWriter, r *http.Request, s *server.Session) {
	f := u.fetcher(w, r, s)
	if u.tofuRun(f, r.PathValue("run")) {
		return
	}
	u.errorPage(w, r, s, http.StatusNotFound, "not found", []server.Diagnostic{{Severity: "error", Class: "usage", Message: "writ serve started no run " + r.PathValue("run") + " of an OpenTofu plan"}}, f.api)
}

// tofuRun renders the page of a run writ serve started of an OpenTofu
// plan, and reports whether id is one. Whether a run applied an OpenTofu
// plan is known from its plan's document, since a run that applies has no
// document of its own yet.
func (u *UI) tofuRun(f *fetch, id string) bool {
	target := "/v1/runs/" + url.PathEscape(id)
	var rr server.RemoteRun
	if !f.also(target, &rr) || rr.Run == nil {
		return f.failed(target)
	}
	var plan server.RemotePlan
	planTarget := "/v1/plans/" + url.PathEscape(rr.Run.Plan)
	if !f.also(planTarget, &plan) {
		return f.failed(planTarget)
	}
	if !isTofuPlan(plan.Document) {
		return false
	}
	if f.r.URL.Query().Get("as") == "json" {
		f.main(target, &rr)
		return true
	}
	p := &page{Title: "Run " + short(id), Section: "runs", CLI: "writ runs --remote --env " + rr.Run.Env + " --stack " + rr.Run.Stack, Data: newTofuRun(rr, plan.Plan)}
	p.Here.Env, p.Here.Stack = rr.Run.Env, rr.Run.Stack
	p.Crumbs = []crumb{{rr.Run.Env, link("/ui/", "env", rr.Run.Env)}, {rr.Run.Stack, link("/ui/stack", "stack", rr.Run.Stack, "env", rr.Run.Env)}, {"Run " + short(id), ""}}
	if rr.Run.Status == "applying" {
		p.Refresh = 5
	}
	f.render("tofu-run", p)
	return true
}
