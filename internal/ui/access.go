package ui

import (
	"encoding/json"
	"net/http"

	"github.com/mr-karan/writ/internal/server"
)

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
