package ui

import (
	"net/http"
	"slices"

	"github.com/mr-karan/writ/internal/server"
)

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
