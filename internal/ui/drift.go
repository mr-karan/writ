package ui

import (
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/mr-karan/writ/internal/server"
)

type driftData struct {
	Env, Stack string
	Every      span
	Check      *server.DriftCheckInfo
	Took       span
	Adopt      string
}

// driftCheck shows one drift check of GET /v1/drift/{id}: what started
// it, its commit and outcome, and the changes it found or its error.
func (u *UI) driftCheck(w http.ResponseWriter, r *http.Request, s *server.Session) {
	f := u.fetcher(w, r, s)
	var doc server.DriftCheckDoc
	if !f.main("/v1/drift/"+url.PathEscape(r.PathValue("id")), &doc) {
		return
	}
	if doc.Check == nil {
		u.errorPage(w, r, s, http.StatusInternalServerError, "internal error", []server.Diagnostic{{Severity: "error", Class: "internal", Message: "Writ sent a drift check document with no check"}}, f.api)
		return
	}
	c := doc.Check
	d := driftData{Env: doc.Env, Stack: doc.Stack, Check: c}
	if every, err := time.ParseDuration(doc.Every); err == nil && every > 0 {
		d.Every = span{doc.Every, fmt.Sprintf("PT%dS", int(every.Seconds()))}
	}
	if c.CheckedAt != "" {
		d.Took.Text, d.Took.ISO = took(c.StartedAt, c.CheckedAt)
	}
	d.Adopt, _ = server.AdoptCommand(c.Error, doc.Stack, doc.Env, doc.Backend)
	p := &page{Title: "Drift check " + short(c.Ref), Section: "overview", Data: d}
	p.Here.Env, p.Here.Stack = doc.Env, doc.Stack
	p.Crumbs = []crumb{
		{doc.Env, link("/ui/", "env", doc.Env)},
		{doc.Stack, link("/ui/stack", "stack", doc.Stack, "env", doc.Env, "view", "drift")},
		{Name: p.Title},
	}
	if c.Status == "planning" {
		p.Refresh = 5
	}
	f.render("drift-check", p)
}
