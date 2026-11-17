// Package ui is the control UI: the HTML pages that writ serve renders
// under /ui/ for people signed in with the team's identity provider.
//
// Each page renders one or two of the API's versioned documents. A page
// gets them through server.Call, which runs the /v1 handler itself with
// the browser session as the principal, so a page and the API decide and
// answer the same. The UI keeps no state of its own beyond sessions, which
// the server stores. It never imports the control store, state, or the
// engine; internal/archtest enforces that.
//
// Reading works without JavaScript. Every response carries a strict
// Content Security Policy, and http.CrossOriginProtection guards every
// form post.
package ui

import (
	"bytes"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/mr-karan/writ/internal/server"
)

// CSP is the Content Security Policy of every response under /ui/: no
// inline script or style, nothing from another origin, no framing.
const CSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

// Options configures New.
type Options struct {
	Server *server.Server
	Log    *slog.Logger
	Now    func() time.Time
}

// UI serves the control UI.
type UI struct {
	srv    *server.Server
	log    *slog.Logger
	now    func() time.Time
	tmpl   *template.Template
	assets *assets
	// secure is true when writ serve's URL is HTTPS. The cookies then
	// carry Secure and the __Host- prefix. Only a loopback http:// URL,
	// for a test or a local proxy, leaves both out.
	secure      bool
	redirectURI string
	// fetched, when set, sees each answer writ serve gives a page's
	// handler. Only the render check sets it, through TraceFetches.
	fetched func(r *http.Request, body []byte)
}

// New parses the templates and loads the assets.
func New(o Options) (*UI, error) {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	a, err := loadAssets()
	if err != nil {
		return nil, err
	}
	u := &UI{srv: o.Server, log: o.Log, now: o.Now, assets: a, secure: strings.HasPrefix(o.Server.URL(), "https://"), redirectURI: o.Server.URL() + "/ui/callback"}
	if u.tmpl, err = parseTemplates(u); err != nil {
		return nil, err
	}
	return u, nil
}

// Mount serves every route of routes beside the API, behind the headers
// and the cross-origin check. server.MountUI takes the table once and is
// the only way a route under /ui/ reaches the server, so the leak checks,
// which walk routes, miss none.
func (u *UI) Mount() {
	cop := http.NewCrossOriginProtection()
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.log.Warn("cross-origin request refused", "method", r.Method, "path", r.URL.Path, "origin", r.Header.Get("Origin"), "sec_fetch_site", r.Header.Get("Sec-Fetch-Site"))
		u.errorPage(w, r, nil, http.StatusForbidden, "refused", []server.Diagnostic{{Severity: "error", Class: "denied", Message: "writ serve refuses a form sent from another site"}}, nil)
	}))
	var table []server.UIRoute
	for _, rt := range u.routes() {
		table = append(table, server.UIRoute{Pattern: rt.pattern, Handler: u.headers(cop.Handler(rt.handler))})
	}
	u.srv.MountUI(table, u.failed)
}

// failed is the page of a request that failed apart from its handler:
// its audit row was not written, or the handler panicked.
func (u *UI) failed(w http.ResponseWriter, r *http.Request, status int, d server.Diagnostic) {
	setHeaders(w.Header())
	heading := "unavailable"
	if status == http.StatusInternalServerError {
		heading = "internal error"
	}
	u.errorPage(w, r, nil, status, heading, []server.Diagnostic{d}, nil)
}

// route is one route of the control UI: a ServeMux pattern and its
// handler. The handler is a func, not any http.Handler, so a router such
// as a ServeMux cannot sit in the table and serve paths it never lists.
type route struct {
	pattern string
	handler http.HandlerFunc
}

// routes are every route of the control UI: /ui/, and / redirects there.
func (u *UI) routes() []route {
	return []route{
		{"GET /{$}", http.RedirectHandler("/ui/", http.StatusSeeOther).ServeHTTP},
		{"GET /ui/static/{hash}/{file}", u.assets.serve},
		{"GET /ui/login", u.login},
		{"GET /ui/login/start", u.loginStart},
		{"GET /ui/callback", u.callback},
		{"GET /ui/login/dev", u.devLogin},
		{"POST /ui/logout", u.authed(u.logout)},
		{"GET /ui/{$}", u.authed(u.overview)},
		{"GET /ui/stack", u.authed(u.stack)},
		{"GET /ui/runs", u.authed(u.runs)},
		{"GET /ui/runs/{run}", u.authed(u.run)},
		{"GET /ui/plans", u.authed(u.plans)},
		{"GET /ui/plans/{id}", u.authed(u.plan)},
		{"POST /ui/plans/{id}/approve", u.authed(u.approve)},
		{"GET /ui/approvals", u.authed(u.approvals)},
		{"GET /ui/access", u.authed(u.access)},
		{"GET /ui/whoami", u.authed(u.whoami)},
		{"GET /ui/start", u.authed(u.start)},
		{"GET /ui/repos", u.authed(u.repos)},
		{"GET /ui/repos/new", u.authed(u.newRepo)},
		{"POST /ui/repos/new", u.authed(u.addRepo)},
		{"GET /ui/repos/{name}", u.authed(u.repo)},
		{"GET /ui/settings", u.authed(u.settings)},
		{"POST /ui/settings", u.authed(u.saveSettings)},
		{"/ui/", u.authed(u.notFound)},
	}
}

// headers sets the headers every response of the control UI carries.
func (u *UI) headers(next http.Handler) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setHeaders(w.Header())
		next.ServeHTTP(w, r)
	})
}

func setHeaders(h http.Header) {
	h.Set("Content-Security-Policy", CSP)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
}

// cookieName gives name the __Host- prefix when the cookies are Secure. A
// browser accepts a __Host- cookie only with Secure, Path=/, and no
// Domain, so no other host can set or read it.
func (u *UI) cookieName(name string) string {
	if u.secure {
		return "__Host-" + name
	}
	return name
}

func (u *UI) setCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: u.cookieName(name), Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: u.secure, SameSite: http.SameSiteLaxMode,
	})
}

const (
	sessionCookie = "writ_session"
	loginCookie   = "writ_login"
	themeCookie   = "writ_theme"
)

// handler is a page for a signed-in person.
type handler func(w http.ResponseWriter, r *http.Request, s *server.Session)

// authed finds the session of the request's cookie. Without one, a GET
// goes to sign-in and comes back to the same page, and a POST is dropped:
// the person signs in and lands on the page the form was on.
func (u *UI) authed(h handler) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var sess *server.Session
		if c, err := r.Cookie(u.cookieName(sessionCookie)); err == nil {
			s, err := u.srv.Session(r.Context(), c.Value)
			if err != nil && !errors.Is(err, server.ErrNoSession) {
				u.log.Error("read a session", "err", err)
				u.errorPage(w, r, nil, http.StatusServiceUnavailable, "unavailable", []server.Diagnostic{{Severity: "error", Class: "state-io", Message: "the control database failed"}}, nil)
				return
			}
			sess = s
		}
		if sess == nil {
			u.setCookie(w, sessionCookie, "", -1)
			// Each form posts to the page it is on, so a dropped post
			// returns there. The post itself is never replayed.
			next := r.URL.RequestURI()
			if r.Method != http.MethodGet {
				next = r.URL.Path
			}
			http.Redirect(w, r, "/ui/login?next="+url.QueryEscape(safeNext(next)), http.StatusSeeOther)
			return
		}
		u.srv.Attribute(r, sess)
		if !u.srv.Allow(sess) {
			u.errorPage(w, r, sess, http.StatusTooManyRequests, "too many requests", []server.Diagnostic{{Severity: "error", Class: "usage", Message: "too many requests a second from " + sess.Display + "; wait a moment and load the page again"}}, nil)
			return
		}
		h(w, r, sess)
	})
}

// safeNext keeps a page to return to after sign-in only when it is a path
// under /ui/ on this server. Anything else, such as another host or the
// sign-in pages themselves, becomes /ui/.
func safeNext(next string) string {
	p, err := url.Parse(next)
	if err != nil {
		return "/ui/"
	}
	clean := path.Clean(p.Path)
	if strings.HasSuffix(p.Path, "/") && clean != "/" {
		clean += "/"
	}
	p.Path, p.RawPath = clean, ""
	if p.Scheme != "" || p.Host != "" || p.User != nil || !strings.HasPrefix(p.Path, "/ui/") ||
		strings.ContainsAny(next, "\\\r\n\t") || strings.HasPrefix(next, "//") ||
		p.Path == "/ui/login" || strings.HasPrefix(p.Path, "/ui/login/") || p.Path == "/ui/callback" || p.Path == "/ui/logout" {
		return "/ui/"
	}
	return p.RequestURI()
}

// page is what every template gets: the frame and one page's data.
type page struct {
	Title   string
	Section string
	Session *server.Session
	Theme   string
	// API lists the requests behind the page, CLI the command that prints
	// the same document, and JSON the link to the page's main document.
	API  []string
	CLI  string
	JSON string
	// Refresh reloads the page every so many seconds while something on
	// it is in progress; 0 for never.
	Refresh int
	// Crumbs is the trail in the top bar, ending at this page, and Here
	// the stack@env the page is about, which the sidebar marks.
	Crumbs []crumb
	Here   struct{ Env, Stack string }
	// Shell is the sidebar's data, nil on a page that fetched nothing.
	Shell *shell
	// Now is when the page was rendered, as RFC 3339.
	Now  string
	Data any
}

// crumb is one step of the trail; the last is the page itself.
type crumb struct{ Name, Href string }

func (u *UI) theme(r *http.Request) string {
	if c, err := r.Cookie(themeCookie); err == nil && (c.Value == "light" || c.Value == "dark") {
		return c.Value
	}
	return "system"
}

func (u *UI) render(w http.ResponseWriter, r *http.Request, status int, name string, p *page) {
	p.Theme = u.theme(r)
	p.Now = u.now().UTC().Format(time.RFC3339)
	if len(p.Crumbs) == 0 && p.Title != "" {
		p.Crumbs = []crumb{{Name: p.Title}}
	}
	var b bytes.Buffer
	if err := u.tmpl.ExecuteTemplate(&b, name, p); err != nil {
		u.log.Error("render a page", "template", name, "err", err)
		http.Error(w, "writ serve failed to render this page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(b.Bytes())
}

// errorData is the page of a refused or failed request.
type errorData struct {
	Status  int
	Heading string
	// Format is the format of the document the diagnostics came in:
	// writ.error/v0 for a refusal the UI makes itself.
	Format      string
	Diagnostics []server.Diagnostic
}

func (u *UI) errorPage(w http.ResponseWriter, r *http.Request, sess *server.Session, status int, heading string, diags []server.Diagnostic, api []string) {
	u.refusalPage(w, r, sess, status, heading, server.FormatError, diags, api)
}

// refusalPage is errorPage for diagnostics that came in a document of
// format, which the page names as their record.
func (u *UI) refusalPage(w http.ResponseWriter, r *http.Request, sess *server.Session, status int, heading, format string, diags []server.Diagnostic, api []string) {
	if format == "" {
		format = server.FormatError
	}
	u.render(w, r, status, "error", &page{Title: heading, Session: sess, API: api, Data: errorData{Status: status, Heading: heading, Format: format, Diagnostics: diags}})
}

func (u *UI) notFound(w http.ResponseWriter, r *http.Request, s *server.Session) {
	u.errorPage(w, r, s, http.StatusNotFound, "not found", []server.Diagnostic{{Severity: "error", Class: "usage", Message: "the control UI has no page " + r.URL.Path}}, nil)
}

// fetch is one page's request for its documents.
type fetch struct {
	u    *UI
	w    http.ResponseWriter
	r    *http.Request
	sess *server.Session
	api  []string
	// got keeps each answer by its target, so the sidebar and the page
	// ask writ serve for a document once.
	got map[string]answer
}

type answer struct {
	status int
	body   []byte
}

func (u *UI) fetcher(w http.ResponseWriter, r *http.Request, s *server.Session) *fetch {
	return &fetch{u: u, w: w, r: r, sess: s, got: map[string]answer{}}
}

// call answers GET target once per page.
func (f *fetch) call(target string) (int, []byte) {
	if a, ok := f.got[target]; ok {
		return a.status, a.body
	}
	status, body := f.u.api(f.r, f.sess, http.MethodGet, target, nil)
	f.got[target] = answer{status, body}
	return status, body
}

// api sends a request of the page r asks for to writ serve's API.
func (u *UI) api(r *http.Request, s *server.Session, method, target string, body []byte) (int, []byte) {
	var status int
	var out []byte
	if method == http.MethodGet {
		status, out = u.srv.Call(r, s, target)
	} else {
		status, out = u.srv.Post(r, s, target, body)
	}
	if u.fetched != nil {
		u.fetched(r, out)
	}
	return status, out
}

// quiet gets a document for the sidebar. It stays out of the page's
// footer, which lists the requests behind the page's own content.
func (f *fetch) quiet(target string, into any) bool {
	status, body := f.call(target)
	return status == http.StatusOK && json.Unmarshal(body, into) == nil
}

// main gets the page's main document. With ?as=json it answers with the
// document as the API sent it and returns false. A refusal renders the
// API's diagnostics with its status, and returns false.
func (f *fetch) main(target string, into any) bool {
	f.api = append(f.api, target)
	status, body := f.call(target)
	if f.r.URL.Query().Get("as") == "json" {
		f.w.Header().Set("Content-Type", "application/json")
		f.w.WriteHeader(status)
		_, _ = f.w.Write(body)
		return false
	}
	if status != http.StatusOK {
		f.refused(status, body)
		return false
	}
	if err := json.Unmarshal(body, into); err != nil {
		f.u.log.Error("decode a document", "target", target, "err", err)
		f.u.errorPage(f.w, f.r, f.sess, http.StatusInternalServerError, "internal error", []server.Diagnostic{{Severity: "error", Class: "internal", Message: "writ serve sent a document the control UI cannot read"}}, f.api)
		return false
	}
	return true
}

// also gets a document the page can do without. It reports whether the
// document came.
func (f *fetch) also(target string, into any) bool {
	if !slices.Contains(f.api, target) {
		f.api = append(f.api, target)
	}
	return f.quiet(target, into)
}

func (f *fetch) refused(status int, body []byte) {
	var doc struct {
		Format      string              `json:"format"`
		Diagnostics []server.Diagnostic `json:"diagnostics"`
	}
	_ = json.Unmarshal(body, &doc)
	if len(doc.Diagnostics) == 0 {
		doc.Format = ""
		doc.Diagnostics = []server.Diagnostic{{Severity: "error", Class: "internal", Message: http.StatusText(status)}}
	}
	heading := map[int]string{403: "denied", 404: "not found", 400: "bad request", 409: "conflict", 429: "too many requests"}[status]
	if heading == "" {
		heading = "failed"
	}
	f.u.refusalPage(f.w, f.r, f.sess, status, heading, doc.Format, doc.Diagnostics, f.api)
}

// render renders the page with the requests it made in its footer.
func (f *fetch) render(name string, p *page) { f.renderStatus(http.StatusOK, name, p) }

func (f *fetch) renderStatus(status int, name string, p *page) {
	p.Session, p.API = f.sess, f.api
	if p.JSON == "" {
		q := f.r.URL.Query()
		q.Del("find")
		q.Set("as", "json")
		p.JSON = f.r.URL.Path + "?" + q.Encode()
	}
	p.Shell = f.shell(p)
	f.u.render(f.w, f.r, status, name, p)
}
