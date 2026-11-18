package ui

import (
	"net/http"

	"github.com/mr-karan/writ/internal/server"
)

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
		d.Applies.Stack = true
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
