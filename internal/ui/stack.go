package ui

import (
	"fmt"
	"net/http"
	"time"

	"github.com/mr-karan/writ/internal/server"
)

type stackData struct {
	Stack, Env, View string
	// Drift is the stack@env's drift, which the head, its meta line, and
	// the drift view show. DriftFailed is set when the drift read failed,
	// so a missing Drift says nothing of the stack's state. DriftCount is
	// the changes its last check found when it found drift, for the tab.
	Drift       *server.DriftStack
	DriftFailed bool
	DriftCount  figure
	// DriftEvery is how often writ serve checks the stack for drift, as
	// GET /v1/drift says it; blank when it runs no drift checks.
	DriftEvery span
	// DriftChecks are the stack@env's kept drift checks, newest first.
	DriftChecks []server.DriftCheckSummary
	// Adopt is the command that adopts the stack's state, when its drift
	// check says the state has no secret set.
	Adopt string
	// Resources is the resources view of the stack's state index, nil
	// when the index holds nothing of it, grouped by GroupBy and filtered
	// by Query.
	Resources      *resourcesView
	Query, GroupBy string
	Applies        *appliesTable
	Plans          *server.RemotePlans
}

// driftBox is the drift view of a stack: its drift and how often writ
// serve checks it.
type driftBox struct {
	Drift  *server.DriftStack
	Every  span
	Checks []server.DriftCheckSummary
}

// stack shows one stack@env: its resources, its drift, its runs, or its
// plans. It never shows a state value, because the state index keeps none.
func (u *UI) stack(w http.ResponseWriter, r *http.Request, s *server.Session) {
	f := u.fetcher(w, r, s)
	q := r.URL.Query()
	d := stackData{Stack: q.Get("stack"), Env: q.Get("env"), View: q.Get("view"), Query: q.Get("q"), GroupBy: q.Get("group")}
	if d.Stack == "" || d.Env == "" {
		f.badRequest("a stack's page needs ?stack= and ?env=")
		return
	}
	switch d.GroupBy {
	case "":
		d.GroupBy = "service"
	case "service", "module", "flat":
	default:
		f.badRequest("group must be service, module, or flat")
		return
	}
	switch d.View {
	case "drift", "runs", "plans":
	default:
		d.View = "resources"
	}
	p := &page{Title: d.Stack + " in " + d.Env, Section: "overview"}
	p.Here.Env, p.Here.Stack = d.Env, d.Stack
	p.Crumbs = []crumb{{d.Env, link("/ui/", "env", d.Env)}, {d.Stack, ""}}
	var drift server.DriftDoc
	driftAt := link("/v1/drift", "env", d.Env, "stack", d.Stack)
	if d.View == "drift" {
		if !f.main(driftAt, &drift) {
			return
		}
	} else {
		d.DriftFailed = !f.also(driftAt, &drift)
	}
	if every, err := time.ParseDuration(drift.Every); err == nil && every > 0 {
		d.DriftEvery = span{drift.Every, fmt.Sprintf("PT%dS", int(every.Seconds()))}
	}
	if len(drift.Stacks) == 1 {
		d.Drift, d.DriftChecks = &drift.Stacks[0], drift.Checks
		if c := d.Drift.Check; c != nil {
			d.Adopt, _ = server.AdoptCommand(c.Error, d.Stack, d.Env, d.Drift.Backend)
			if c.Status == server.DriftDrifted {
				d.DriftCount = exact(c.Outside + c.Code)
			}
		}
	}
	switch d.View {
	case "runs":
		var ok bool
		if d.Applies, ok = f.applies("Runs of "+d.Stack+" in "+d.Env+", newest first", d.Env, d.Stack); !ok {
			return
		}
		p.CLI = "writ runs --remote --env " + d.Env + " --stack " + d.Stack
	case "plans":
		d.Plans = &server.RemotePlans{}
		if !f.main(link("/v1/plans", "env", d.Env, "stack", d.Stack), d.Plans) {
			return
		}
	case "resources":
		// GET /v1/index answers 404 to a person who may read the stack
		// when its index holds nothing: writ serve keeps no state of it,
		// or has a state it has not indexed, such as one with no secret
		// set. The page then shows what the drift record knows.
		target := link("/v1/index", "env", d.Env, "stack", d.Stack)
		if status, _ := f.call(target); status == http.StatusNotFound && q.Get("as") != "json" {
			f.api = append(f.api, target)
			break
		}
		var index server.StateIndexDoc
		if !f.main(target, &index) {
			return
		}
		var check *server.DriftCheckInfo
		if d.Drift != nil {
			check = d.Drift.Check
		}
		v := groupResources(index.Resources, check, d.Query, d.GroupBy)
		v.linkDrift(link("/ui/stack", "stack", d.Stack, "env", d.Env, "view", "drift"))
		d.Resources = &v
	}
	p.Data = d
	f.render("stack", p)
}
