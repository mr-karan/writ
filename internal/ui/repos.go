package ui

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/mr-karan/writ/internal/server"
)

// devLogin spends a sign-in code of writ serve --dev and starts a session
// of its one user.
func (u *UI) devLogin(w http.ResponseWriter, r *http.Request) {
	token, sess, err := u.srv.DevSignIn(r.Context(), r, r.URL.Query().Get("code"))
	if err != nil {
		u.render(w, r, http.StatusUnauthorized, "login", &page{Title: "Sign in", Data: loginData{Dev: u.srv.Dev(), Next: "/ui/", Message: err.Error()}})
		return
	}
	u.setCookie(w, sessionCookie, token, int(sess.EndsAt.Sub(u.now())/1e9))
	http.Redirect(w, r, "/ui/start", http.StatusSeeOther)
}

type startData struct {
	Server server.ServerInfo
	Repos  []server.RepoInfo
	// Stacks and Served are floors when a repository's read failed, and
	// Plans when an environment holds more plans than writ serve read or
	// a read of the environments or their plans failed.
	Stacks, Served, Plans figure
	FirstEnv              string
}

// start is Getting started: what the control plane has, step by step, and
// the command or page for the next step.
func (u *UI) start(w http.ResponseWriter, r *http.Request, s *server.Session) {
	f := u.fetcher(w, r, s)
	var repos server.ReposDoc
	if !f.main("/v1/repos", &repos) {
		return
	}
	d := startData{Repos: repos.Repos}
	f.also("/v1/server", &d.Server)
	for _, rp := range repos.Repos {
		var doc server.RepoDoc
		if !f.also("/v1/repos/"+rp.Name, &doc) {
			d.Stacks.Floor, d.Served.Floor = true, true
			continue
		}
		for _, st := range doc.Stacks {
			d.Stacks.N++
			if len(st.Served) > 0 {
				d.Served.N++
				if d.FirstEnv == "" {
					d.FirstEnv = st.Served[0]
				}
			}
		}
	}
	envs, read := f.envNames()
	if len(envs) == 0 && d.FirstEnv != "" {
		envs = []string{d.FirstEnv}
	}
	plans, cut := f.plansIn(envs, "")
	d.Plans = figure{len(plans), cut || !read}
	f.render("start", &page{Title: "Getting started", Data: d})
}

func (u *UI) repos(w http.ResponseWriter, r *http.Request, s *server.Session) {
	f := u.fetcher(w, r, s)
	var doc server.ReposDoc
	if !f.main("/v1/repos", &doc) {
		return
	}
	f.render("repos", &page{Title: "Repositories", Section: "repos", CLI: "writ repo list", Data: doc})
}

// repo shows one repository and the stacks found at its head.
func (u *UI) repo(w http.ResponseWriter, r *http.Request, s *server.Session) {
	f := u.fetcher(w, r, s)
	name := r.PathValue("name")
	var doc server.RepoDoc
	if !f.main("/v1/repos/"+name, &doc) {
		return
	}
	for _, st := range doc.Stacks {
		for i := range st.Providers {
			st.Providers[i].Source = shortProvider(st.Providers[i].Source)
		}
	}
	f.render("repo", &page{Title: name, Section: "repos", CLI: "writ repo show " + name, Data: doc, Crumbs: []crumb{{"Repositories", "/ui/repos"}, {name, ""}}})
}

var repoNewCrumbs = []crumb{{"Repositories", "/ui/repos"}, {"Add repository", ""}}

type newRepoData struct {
	Dev     bool
	Pinned  bool
	Form    server.RepoRequest
	Refusal []server.Diagnostic
}

// newRepo is the Add repository form.
func (u *UI) newRepo(w http.ResponseWriter, r *http.Request, s *server.Session) {
	f := u.fetcher(w, r, s)
	var doc server.ReposDoc
	f.also("/v1/repos", &doc)
	f.render("repo-new", &page{Title: "Add repository", Section: "repos", CLI: "writ repo add NAME HOST/PROJECT --path PATH", Data: newRepoData{Dev: u.srv.Dev(), Pinned: doc.Pinned}, Crumbs: repoNewCrumbs})
}

// addRepo sends the form to POST /v1/repos. Added, the browser goes to the
// repository's page, which shows the stacks found; refused, the form comes
// back with the refusal and what was typed.
func (u *UI) addRepo(w http.ResponseWriter, r *http.Request, s *server.Session) {
	if err := r.ParseForm(); err != nil {
		u.errorPage(w, r, s, http.StatusBadRequest, "bad request", []server.Diagnostic{{Severity: "error", Class: "usage", Message: "the form does not parse"}}, nil)
		return
	}
	field := func(k string) string { return strings.TrimSpace(r.PostForm.Get(k)) }
	req := server.RepoRequest{Format: server.FormatRepoRequest, Name: field("name"), Forge: field("forge"), Project: field("project"), Path: strings.Trim(field("path"), "/"), Prefix: field("prefix")}
	body, _ := json.Marshal(req)
	status, resp := u.api(r, s, http.MethodPost, "/v1/repos", body)
	if status == http.StatusCreated {
		http.Redirect(w, r, "/ui/repos/"+req.Name, http.StatusSeeOther)
		return
	}
	_, refusal := refusalOf(resp)
	f := u.fetcher(w, r, s)
	f.api = append(f.api, "POST /v1/repos")
	f.renderStatus(status, "repo-new", &page{Title: "Add repository", Section: "repos", Data: newRepoData{Dev: u.srv.Dev(), Form: req, Refusal: refusal}, Crumbs: repoNewCrumbs})
}

// plansIn lists the newest 200 plans of each environment in status, or in
// any status when it is empty. cut reports that an environment holds more
// or that a read failed, so the list is not all of them.
func (f *fetch) plansIn(envs []string, status string) (plans []server.PlanInfo, cut bool) {
	for _, env := range envs {
		var doc server.RemotePlans
		if !f.also(link("/v1/plans", "env", env, "status", status, "limit", "200"), &doc) {
			cut = true
			continue
		}
		plans = append(plans, doc.Plans...)
		cut = cut || doc.Next != ""
	}
	return plans, cut
}
