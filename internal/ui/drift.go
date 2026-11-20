package ui

import (
	"cmp"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/mr-karan/writ/internal/server"
)

type driftData struct {
	ID, Env, Stack, Backend string
	Every                   span
	Check                   *server.DriftCheckInfo
	Took                    span
	Adopt                   string
	Diagnostics             []server.Diagnostic
	Network                 *networkView
	DocError                string
}

// driftCheck shows one drift check: its head, its changes (outside OpenTofu
// vs in the code), its diagnostics if it failed, and its network record if
// any.
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
	d := driftData{
		ID:          driftName(c),
		Env:         doc.Env,
		Stack:       doc.Stack,
		Backend:     doc.Backend,
		Check:       c,
		Diagnostics: doc.Diagnostics,
	}
	if every, err := time.ParseDuration(doc.Every); err == nil && every > 0 {
		d.Every = span{doc.Every, fmt.Sprintf("PT%dS", int(every.Seconds()))}
	}
	if c.StartedAt != "" && c.CheckedAt != "" {
		d.Took.Text, d.Took.ISO = took(c.StartedAt, c.CheckedAt)
	}
	if c.Status == server.DriftFailed && c.Error != "" {
		d.Adopt, _ = server.AdoptCommand(c.Error, doc.Stack, doc.Env, doc.Backend)
	}
	var netErr error
	if d.Network, netErr = readNetwork(doc.Network); netErr != nil {
		d.DocError = netErr.Error()
	}
	p := &page{
		Title:   "Drift check " + short(d.ID),
		Section: "overview",
		Data:    d,
	}
	p.Here.Env, p.Here.Stack = doc.Env, doc.Stack
	p.Crumbs = []crumb{
		{doc.Env, link("/ui/", "env", doc.Env)},
		{doc.Stack, link("/ui/stack", "stack", doc.Stack, "env", doc.Env)},
		{"Drift", link("/ui/stack", "stack", doc.Stack, "env", doc.Env, "view", "drift")},
		{Name: p.Title},
	}
	if c.Status == "planning" {
		p.Refresh = 5
	}
	f.render("drift-check", p)
}

// driftName is a drift check's stable identifier for URLs: its ref, or its
// plan id when ref is absent.
func driftName(c *server.DriftCheckInfo) string {
	if c == nil {
		return ""
	}
	return cmp.Or(c.Ref, c.Plan)
}
