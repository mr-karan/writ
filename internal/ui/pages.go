package ui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"

	"github.com/mr-karan/writ/internal/mask"
	"github.com/mr-karan/writ/internal/server"
)

func (f *fetch) badRequest(msg string) {
	f.u.errorPage(f.w, f.r, f.sess, http.StatusBadRequest, "bad request", []server.Diagnostic{{Severity: "error", Class: "usage", Message: msg}}, nil)
}

// envNames lists the environments the signed-in person may read, from the
// overview document the sidebar reads too, and reports whether it came.
func (f *fetch) envNames() ([]string, bool) {
	a := f.activity(true)
	if a == nil {
		return nil, false
	}
	return f.envsOf(a), true
}

// listEnv is the environment a list page shows, env or else the first the
// signed-in person may read, and the environments of its tabs. With no
// env and the overview failed, it renders the overview's refusal and
// reports false.
func (f *fetch) listEnv(env string) (string, []string, bool) {
	envs, read := f.envNames()
	if env == "" && !read && f.failed("/v1/overview") {
		return "", nil, false
	}
	if env == "" && len(envs) > 0 {
		env = envs[0]
	}
	return env, envs, true
}

// failed renders the refusal of target's read and reports true, unless
// target came or answered 404, which the page reads as none.
func (f *fetch) failed(target string) bool {
	status, body := f.call(target)
	if status == http.StatusOK || status == http.StatusNotFound {
		return false
	}
	f.refused(status, body)
	return true
}

// overview answers what waits, what works, what went wrong, and what each
// stack@env holds.
func (u *UI) overview(w http.ResponseWriter, r *http.Request, s *server.Session) {
	f := u.fetcher(w, r, s)
	env := r.URL.Query().Get("env")
	doc := &server.OverviewDoc{}
	if !f.main("/v1/overview", doc) {
		return
	}
	d := f.newOverview(env, doc)
	if env != "" && !slices.Contains(d.Envs, env) {
		f.badRequest("writ serve has no environment " + env + " that you may read")
		return
	}
	p := &page{Title: "Overview", Section: "overview", Data: d}
	if env != "" {
		p.Crumbs = []crumb{{"Overview", "/ui/"}, {env, ""}}
	}
	if d.Planning.N > 0 || d.Applying.N > 0 {
		p.Refresh = 30
	}
	f.render("overview", p)
}

type stackData struct {
	Stack, Env, View string
	// Drift is the stack@env's drift, and Index its state index, which
	// the resources view shows. DriftFailed is set when the drift read
	// failed, so a missing Drift says nothing of the stack's state.
	Drift       *server.DriftStack
	DriftFailed bool
	// Adopt is the command that adopts the stack's state, when its drift
	// check says the state has no secret set.
	Adopt   string
	Index   *server.StateIndexDoc
	Applies *appliesTable
	Plans   *server.RemotePlans
}

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

// stack shows one stack@env: its resources, its runs, or its plans. It
// never shows a state value, because the state index keeps none.
func (u *UI) stack(w http.ResponseWriter, r *http.Request, s *server.Session) {
	f := u.fetcher(w, r, s)
	q := r.URL.Query()
	d := stackData{Stack: q.Get("stack"), Env: q.Get("env"), View: q.Get("view")}
	if d.Stack == "" || d.Env == "" {
		f.badRequest("a stack's page needs ?stack= and ?env=")
		return
	}
	p := &page{Title: d.Stack + " in " + d.Env, Section: "overview"}
	p.Here.Env, p.Here.Stack = d.Env, d.Stack
	p.Crumbs = []crumb{{d.Env, link("/ui/", "env", d.Env)}, {d.Stack, ""}}
	if d.View == "runs" || d.View == "plans" {
		p.Crumbs[1].Href = link("/ui/stack", "stack", d.Stack, "env", d.Env)
		p.Crumbs = append(p.Crumbs, crumb{Name: map[string]string{"runs": "Runs", "plans": "Plans"}[d.View]})
	}
	var drift server.DriftDoc
	d.DriftFailed = !f.also(link("/v1/drift", "env", d.Env, "stack", d.Stack), &drift)
	if len(drift.Stacks) == 1 {
		d.Drift = &drift.Stacks[0]
		if c := d.Drift.Check; c != nil {
			d.Adopt, _ = server.AdoptCommand(c.Error, d.Stack, d.Env, d.Drift.Backend)
		}
	}
	switch d.View {
	case "runs":
		var ok bool
		if d.Applies, ok = f.applies("Runs of "+d.Stack+" in "+d.Env+" that writ serve started, newest first", d.Env, d.Stack); !ok {
			return
		}
		p.CLI = "writ runs --remote --env " + d.Env + " --stack " + d.Stack
	case "plans":
		d.Plans = &server.RemotePlans{}
		if !f.main(link("/v1/plans", "env", d.Env, "stack", d.Stack), d.Plans) {
			return
		}
	default:
		d.View = "resources"
		// GET /v1/index answers 404 to a person who may read the stack
		// when its index holds nothing: writ serve keeps no state of it,
		// or has a state it has not indexed, such as one with no secret
		// set. The page then shows what the drift record knows.
		target := link("/v1/index", "env", d.Env, "stack", d.Stack)
		if status, _ := f.call(target); status == http.StatusNotFound && r.URL.Query().Get("as") != "json" {
			f.api = append(f.api, target)
			break
		}
		d.Index = &server.StateIndexDoc{}
		if !f.main(target, d.Index) {
			return
		}
	}
	p.Data = d
	f.render("stack", p)
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

type plansData struct {
	Env, Stack, Status string
	// Cursor is the page's own cursor, "" on the newest page.
	Cursor   string
	Envs     []string
	Statuses []string
	Plans    []server.PlanInfo
	Next     string
}

// plans lists the plans of one environment, newest first, with filters in
// the URL.
func (u *UI) plans(w http.ResponseWriter, r *http.Request, s *server.Session) {
	f := u.fetcher(w, r, s)
	q := r.URL.Query()
	d := plansData{Stack: q.Get("stack"), Status: q.Get("status"), Cursor: q.Get("cursor"),
		Statuses: []string{"planning", "pending", "approved", "applying", "applied", "refused", "failed", "expired", "superseded"}}
	var ok bool
	if d.Env, d.Envs, ok = f.listEnv(q.Get("env")); !ok {
		return
	}
	var doc server.RemotePlans
	if !f.main(link("/v1/plans", "env", d.Env, "stack", d.Stack, "status", d.Status, "cursor", d.Cursor), &doc) {
		return
	}
	d.Plans, d.Next = doc.Plans, doc.Next
	p := &page{Title: "Plans", Section: "plans", Data: d, Crumbs: []crumb{{"Plans", "/ui/plans"}, {d.Env, ""}}}
	if slices.ContainsFunc(d.Plans, func(p server.PlanInfo) bool { return p.Status == "planning" }) {
		p.Refresh = 30
	}
	f.render("plans", p)
}

// plan is plan review: the dangerous changes first, each with its cause
// and the line of code behind it.
func (u *UI) plan(w http.ResponseWriter, r *http.Request, s *server.Session) {
	f := u.fetcher(w, r, s)
	var doc server.RemotePlan
	if !f.main("/v1/plans/"+r.PathValue("id"), &doc) {
		return
	}
	q := r.URL.Query()
	pageNo, _ := strconv.Atoi(q.Get("page"))
	v := newPlanView(doc.Plan, doc.Text, pageNo)
	v.Filter.Action, v.Filter.Risk = q.Get("action"), q.Get("risk")
	var tofu *mask.Plan
	var docErr error
	if len(doc.Document) > 0 && string(doc.Document) != "null" {
		tofu, v.Diagnostics, v.Older, docErr = readPlanDoc(doc.Document)
	}
	if tofu != nil {
		v.Tofu, docErr = newTofuView(tofu, v)
	}
	if docErr != nil {
		v.DocError = docErr.Error()
	}
	if len(doc.Policy) > 0 && string(doc.Policy) != "null" {
		v.Policy = &policyView{}
		if err := json.Unmarshal(doc.Policy, v.Policy); err != nil {
			v.DocError = fmt.Sprintf("read the plan's policy decision: %v", err)
		}
	}
	if len(doc.Plan.Stacks) == 1 {
		v.Adopt = f.adoptHint(v.Diagnostics, doc.Plan.Stacks[0].Stack, doc.Plan.Env)
	}
	_ = json.Unmarshal(doc.Plan.Origin, &v.Origin)
	name := doc.Plan.ID
	if name == "" {
		name = doc.Plan.Ref
	}
	p := &page{Title: "Plan " + short(name), Section: "plans", CLI: "writ show --remote " + name, Data: v}
	p.Crumbs = []crumb{{doc.Plan.Env, link("/ui/", "env", doc.Plan.Env)}}
	if len(doc.Plan.Stacks) == 1 {
		st := doc.Plan.Stacks[0].Stack
		p.Here.Env, p.Here.Stack = doc.Plan.Env, st
		p.Crumbs = append(p.Crumbs, crumb{st, link("/ui/stack", "stack", st, "env", doc.Plan.Env)})
	}
	p.Crumbs = append(p.Crumbs, crumb{Name: p.Title})
	if doc.Plan.Status == "planning" {
		p.Refresh = 3
	}
	f.render("plan", p)
}

// adoptHint is the command that adopts the state a failed plan of
// stack@env read, when it failed because that state has no secret set,
// and "" otherwise. The flag that names the state depends on the stack's
// backend, which the stack's drift record names.
func (f *fetch) adoptHint(diags []docDiag, stack, env string) string {
	for _, d := range diags {
		if _, ok := server.AdoptCommand(d.Message, stack, env, ""); !ok {
			continue
		}
		// The flag depends on the backend, so with no drift record read
		// the page offers no command rather than one that may be wrong.
		var drift server.DriftDoc
		if !f.also(link("/v1/drift", "env", env, "stack", stack), &drift) {
			return ""
		}
		backend := ""
		if len(drift.Stacks) == 1 {
			backend = drift.Stacks[0].Backend
		}
		cmd, _ := server.AdoptCommand(d.Message, stack, env, backend)
		return cmd
	}
	return ""
}

// approve sends the signed-in person's approval of a plan to POST
// /v1/plans/{id}/approvals, which decides it as it decides a CI job's.
// Approved, the browser goes back to the plan's page; refused, the page
// says why.
func (u *UI) approve(w http.ResponseWriter, r *http.Request, s *server.Session) {
	id := r.PathValue("id")
	status, resp := u.api(r, s, http.MethodPost, "/v1/plans/"+url.PathEscape(id)+"/approvals", nil)
	if status == http.StatusOK {
		http.Redirect(w, r, "/ui/plans/"+url.PathEscape(id), http.StatusSeeOther)
		return
	}
	var refusal struct {
		Format      string              `json:"format"`
		Diagnostics []server.Diagnostic `json:"diagnostics"`
	}
	_ = json.Unmarshal(resp, &refusal)
	u.refusalPage(w, r, s, status, "approval refused", refusal.Format, refusal.Diagnostics, []string{"POST /v1/plans/" + id + "/approvals"})
}

type approvalsData struct {
	Env    string
	Envs   []string
	Mine   []server.PlanInfo
	Others []server.PlanInfo
	// Read counts the plans that wait writ serve read, and Next is the
	// cursor of those it left out, "" when it read every one. With Next
	// set, MineN and OthersN are floors.
	Read           int
	Next           string
	MineN, OthersN figure
}

// approvals lists the plans that wait, split by whether the signed-in
// person made them. Each plan's page has the approve button. It reads the
// newest 200, and says so and links the rest when more wait.
func (u *UI) approvals(w http.ResponseWriter, r *http.Request, s *server.Session) {
	f := u.fetcher(w, r, s)
	var d approvalsData
	var ok bool
	if d.Env, d.Envs, ok = f.listEnv(r.URL.Query().Get("env")); !ok {
		return
	}
	var doc server.RemotePlans
	if !f.main(link("/v1/plans", "env", d.Env, "status", "pending", "limit", "200"), &doc) {
		return
	}
	for _, p := range doc.Plans {
		if p.Author.Person == s.Person {
			d.Mine = append(d.Mine, p)
		} else {
			d.Others = append(d.Others, p)
		}
	}
	d.Read, d.Next = len(doc.Plans), doc.Next
	d.MineN, d.OthersN = figure{len(d.Mine), d.Next != ""}, figure{len(d.Others), d.Next != ""}
	f.render("approvals", &page{Title: "Approvals", Section: "approvals", Data: d, Crumbs: []crumb{{"Approvals", "/ui/approvals"}, {d.Env, ""}}})
}

type accessData struct {
	Me     server.Whoami
	Config server.ConfigDoc
}

// access shows who may do what: the signed-in person, every team's rule,
// and every grant.
func (u *UI) access(w http.ResponseWriter, r *http.Request, s *server.Session) {
	f := u.fetcher(w, r, s)
	var d accessData
	if !f.main("/v1/config", &d.Config) {
		return
	}
	f.also("/v1/whoami", &d.Me)
	f.render("access", &page{Title: "Access", Section: "access", CLI: "writ serve --check", Data: d})
}

// whoami shows writ.whoami/v0 for the signed-in person.
func (u *UI) whoami(w http.ResponseWriter, r *http.Request, s *server.Session) {
	f := u.fetcher(w, r, s)
	var d server.Whoami
	if !f.main("/v1/whoami", &d) {
		return
	}
	f.render("whoami", &page{Title: "Who you are", Section: "access", CLI: "writ whoami --remote", Data: d, Crumbs: []crumb{{"Access", "/ui/access"}, {"Who you are", ""}}})
}

type settingsData struct {
	Server     server.ServerInfo
	Health     server.Health
	HealthCode int
	Config     *server.ConfigDoc
	License    string
}

// settings shows how writ serve is set up and whether its parts answer,
// and the theme form, the one setting a browser keeps.
func (u *UI) settings(w http.ResponseWriter, r *http.Request, s *server.Session) {
	f := u.fetcher(w, r, s)
	d := settingsData{License: u.assets.url("oat-license.txt")}
	if !f.main("/v1/server", &d.Server) {
		return
	}
	f.api = append(f.api, "/healthz")
	var body []byte
	d.HealthCode, body = f.call("/healthz")
	_ = json.Unmarshal(body, &d.Health)
	var cfg server.ConfigDoc
	if f.also("/v1/config", &cfg) {
		d.Config = &cfg
	}
	f.render("settings", &page{Title: "Settings", Section: "settings", CLI: "writ serve --check", Data: d})
}

// saveSettings keeps the theme in a cookie, so the server renders it and
// no script runs before the page paints.
func (u *UI) saveSettings(w http.ResponseWriter, r *http.Request, s *server.Session) {
	theme := r.PostFormValue("theme")
	if theme != "light" && theme != "dark" && theme != "system" {
		u.errorPage(w, r, s, http.StatusBadRequest, "bad request", []server.Diagnostic{{Severity: "error", Class: "usage", Message: "theme is system, light, or dark"}}, nil)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: themeCookie, Value: theme, Path: "/", MaxAge: 365 * 24 * 3600, HttpOnly: true, Secure: u.secure, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/ui/settings", http.StatusSeeOther)
}
