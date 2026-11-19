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

type plansData struct {
	Env, Stack, Status, StartedBy string
	// Cursor is the page's own cursor, "" on the newest page.
	Cursor        string
	Envs          []string
	StatusPick    picker
	StartedByPick picker
	Plans         []server.PlanInfo
	Next          string
}

// plans lists the plans of one environment, newest first, with filters in
// the URL.
func (u *UI) plans(w http.ResponseWriter, r *http.Request, s *server.Session) {
	f := u.fetcher(w, r, s)
	q := r.URL.Query()
	d := plansData{Stack: q.Get("stack"), Status: q.Get("status"), StartedBy: q.Get("started_by"), Cursor: q.Get("cursor")}
	var ok bool
	if d.Env, d.Envs, ok = f.listEnv(q.Get("env")); !ok {
		return
	}
	d.StatusPick = newPicker("Status", d.Status, server.PlanStatuses, func(s string) string {
		return link("/ui/plans", "env", d.Env, "stack", d.Stack, "status", s, "started_by", d.StartedBy)
	})
	d.StartedByPick = newPicker("Started by", d.StartedBy, server.PlanStartedBy, func(by string) string {
		return link("/ui/plans", "env", d.Env, "stack", d.Stack, "status", d.Status, "started_by", by)
	})
	var doc server.RemotePlans
	if !f.main(link("/v1/plans", "env", d.Env, "stack", d.Stack, "status", d.Status, "started_by", d.StartedBy, "cursor", d.Cursor), &doc) {
		return
	}
	d.Plans, d.Next = doc.Plans, doc.Next
	p := &page{Title: "Plans", Section: "plans", Data: d, Crumbs: []crumb{{Name: "Plans"}}}
	if slices.ContainsFunc(d.Plans, func(p server.PlanInfo) bool { return p.Status == server.PlanPlanning }) {
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
		changes := "/ui/plans/" + planName(doc.Plan)
		v.ActionPick = newPicker("Action", v.Filter.Action, v.Actions, func(a string) string {
			return link(changes, "action", a, "risk", v.Filter.Risk)
		})
		v.RiskPick = newPicker("Risk", v.Filter.Risk, v.Risks, func(r string) string {
			return link(changes, "action", v.Filter.Action, "risk", r)
		})
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
	if doc.Plan.Status == server.PlanPlanning {
		p.Refresh = 3
	}
	f.render("plan", p)
}

// adoptHint is the command that adopts the state a failed plan of
// stack@env read, when it failed because that state has no secret set,
// and "" otherwise. It marks the diagnostic that says so as Adopted.
// The flag that names the state depends on the stack's backend, which
// the stack's drift record names.
func (f *fetch) adoptHint(diags []docDiag, stack, env string) string {
	for i, d := range diags {
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
		diags[i].Adopted = true
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
	format, diags := refusalOf(resp)
	u.refusalPage(w, r, s, status, "approval refused", format, diags, []string{"POST /v1/plans/" + id + "/approvals"})
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
	if !f.main(link("/v1/plans", "env", d.Env, "status", server.PlanPending, "limit", "200"), &doc) {
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
	f.render("approvals", &page{Title: "Approvals", Section: "approvals", Data: d, Crumbs: []crumb{{Name: "Approvals"}}})
}
