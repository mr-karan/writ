package ui_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/control/controltest"
	"github.com/mr-karan/writ/internal/oidc/oidctest"
	"github.com/mr-karan/writ/internal/server"
	"github.com/mr-karan/writ/internal/ui"
)

// TestNoScriptWalk signs in with a client that runs no script, opens every
// screen, and checks each page: its headers, that it holds no inline
// script, style, or event handler, its structure for a screen reader, and
// that no secret reaches it. It then sends every form, and fails when a
// route or a template is left that no request reached, so that no
// answer of the UI escapes the check for secrets.
func TestNoScriptWalk(t *testing.T) {
	f := newFixture(t)
	id := f.plan(nil)
	plansBefore, _, _ := f.store.Plans(context.Background(), control.PlanFilter{Env: "prod", Limit: 50}, time.Now())
	b := f.browser()
	b.signIn("/ui/")
	cookieValue := ""
	for _, c := range b.c.Jar.Cookies(mustURL(f.url + "/ui/")) {
		if c.Name == "__Host-writ_session" {
			cookieValue = c.Value
		}
	}
	if cookieValue == "" {
		t.Fatal("no session cookie in the jar")
	}
	pages := []struct{ path, want string }{
		{"/ui/", "1 by others · 0 yours"},
		{"/ui/?env=prod", "network"},
		{"/ui/stack?stack=network&env=prod", ui.Nones["no-state"]},
		{"/ui/stack?stack=network&env=prod&view=drift", ui.Nones["no-state"]},
		{"/ui/stack?stack=network&env=prod&view=runs", ui.Nones["no-run"]},
		{"/ui/stack?stack=network&env=prod&view=plans", short(id)},
		{"/ui/runs", ui.Nones["no-run"]},
		{"/ui/plans", short(id)},
		{"/ui/plans?env=prod&status=pending&stack=network", short(id)},
		{"/ui/plans/" + id, "Changes (2)"},
		{"/ui/plans/" + id + "?risk=replace", "1 of 2 changes match"},
		{"/ui/approvals", "Plans by others (1)"},
		{"/ui/access", "acme/platform"},
		{"/ui/whoami", bobPerson},
		{"/ui/settings", "authorization code flow with PKCE"},
		{"/ui/start", "Getting started"},
		{"/ui/repos", "acme/infra"},
		{"/ui/repos/infra", "acme/infra"},
		{"/ui/repos/new", "Add repository"},
	}
	for _, pg := range pages {
		t.Run(pg.path, func(t *testing.T) {
			p := b.get(pg.path)
			if p.status != 200 {
				t.Fatalf("%d: %s", p.status, p.body)
			}
			if !strings.Contains(text(p.doc), pg.want) {
				t.Errorf("the page lacks %q", pg.want)
			}
			checkPage(t, p, cookieValue)
		})
	}
	b2 := f.browser()
	login := b2.get("/ui/login")
	if login.status != 200 {
		t.Fatalf("login %d", login.status)
	}
	checkPage(t, login, cookieValue)

	plansAfter, _, _ := f.store.Plans(context.Background(), control.PlanFilter{Env: "prod", Limit: 50}, time.Now())
	if len(plansAfter) != len(plansBefore) || plansAfter[0].Status != plansBefore[0].Status {
		t.Fatal("a GET changed the plans")
	}

	// The forms, and the pages only a form, a run, a drift check, a state
	// with no secret set, or a failure reaches.
	if p := b.post("/ui/plans/"+id+"/approve", url.Values{}, "same-origin"); p.status != 200 || !strings.Contains(text(p.doc), "Approved by") {
		t.Fatalf("approve: %d %s", p.status, text(p.doc))
	}
	run := f.apply(id)
	f.driftCheck()
	f.noSecretSet("prod")
	unsealed := f.planEnded(nil)
	if unsealed.Status != "failed" {
		t.Fatalf("the plan of a state with no secret set: %+v", unsealed)
	}
	for _, pg := range []struct {
		path   string
		status int
		want   string
	}{
		{"/ui/plans/" + id, 200, "failed to apply this plan"},
		{"/ui/runs/" + run, 200, short(run)},
		{"/ui/runs", 200, short(run)},
		{"/ui/stack?stack=network&env=prod&view=runs", 200, short(run)},
		{"/ui/?env=prod", 200, "drifted 1 outside, 2 in code"},
		{"/ui/stack?stack=network&env=prod&view=drift", 200, "1 changed outside OpenTofu, 2 that the code would change"},
		{"/ui/stack?stack=network&env=prod", 200, "1 in the last drift check"},
		{"/ui/stack?stack=network&env=prod&group=module", 200, "tls_private_key.ca_key"},
		{"/ui/stack?stack=network&env=prod&group=flat&q=CA-KEY", 200, "1 of 1 resources match"},
		{"/ui/stack?stack=network&env=prod&q=nothing-matches", 200, "0 of 1 resources match"},
		{"/ui/stack?stack=network&env=staging", 200, "writ state adopt network --env staging --version 1 --reason REASON"},
		{"/ui/plans/" + unsealed.Ref, 200, "writ state adopt network --env prod --version 1 --reason REASON"},
		{"/ui/login/dev", http.StatusUnauthorized, "this sign-in link was used already"},
		{"/ui/no-such-page", http.StatusNotFound, "the control UI has no page /ui/no-such-page"},
	} {
		t.Run(pg.path, func(t *testing.T) {
			p := b.get(pg.path)
			if p.status != pg.status || !strings.Contains(text(p.doc), pg.want) {
				t.Fatalf("%d, the page lacks %q: %s", p.status, pg.want, text(p.doc))
			}
			checkPage(t, p, cookieValue)
		})
	}
	raw := controltest.Raw(t, f.loc)
	if _, err := raw.ExecContext(context.Background(), "ALTER TABLE drift_checks RENAME TO gone_drift_checks"); err != nil {
		t.Fatal(err)
	}
	if p := b.get("/ui/plans/" + unsealed.Ref); p.status != 200 || strings.Contains(text(p.doc), "writ state adopt network --env prod --version") {
		t.Errorf("with the drift read failed, the plan offers an adopt command whose flag it cannot know: %d %s", p.status, text(p.doc))
	}
	if _, err := raw.ExecContext(context.Background(), "ALTER TABLE gone_drift_checks RENAME TO drift_checks"); err != nil {
		t.Fatal(err)
	}
	for _, path := range staticLinks(b.get("/ui/").doc) {
		if p := b.get(path); p.status != 200 {
			t.Errorf("%s: %d", path, p.status)
		}
	}
	if p := b.get("/"); p.status != 200 || p.url.Path != "/ui/" {
		t.Errorf("GET / ended at %s with %d", p.url, p.status)
	}
	if p := b.post("/ui/repos/new", url.Values{"name": {"other"}, "forge": {"gitlab"}, "project": {"acme/other"}}, "same-origin"); p.status == http.StatusSeeOther || p.status == 200 {
		t.Errorf("a person who may not add a repository added one: %d", p.status)
	} else {
		checkPage(t, p, cookieValue)
	}
	if p := b.post("/ui/settings", url.Values{"theme": {"dark"}}, "same-origin"); p.status != 200 {
		t.Errorf("settings: %d", p.status)
	}
	if p := b.post("/ui/logout", url.Values{}, "same-origin"); p.status != 200 || p.url.Path != "/ui/login" {
		t.Errorf("sign out: %d at %s", p.status, p.url)
	}
	f.traffic.covered(t)
	for i, body := range f.traffic.bodies() {
		for _, secret := range []string{paramSecret, envSecret, clientSec, forgeToken, cookieValue, "writs_"} {
			if strings.Contains(body, secret) {
				t.Errorf("answer %d holds the secret %q: %s", i, secret, excerpt(body, secret))
			}
		}
	}
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

func short(id string) string { return id[:8] }

var onAttr = regexp.MustCompile(`^on[a-z]+$`)

// checkPage holds every page to the rules of the design: the headers, no
// inline script or style, one h1, landmarks, a skip link first, labels on
// fields, captions on tables, no secret, and checkWords.
func checkPage(t *testing.T, p page, session string) {
	t.Helper()
	for k, v := range map[string]string{"Content-Security-Policy": ui.CSP, "X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer", "Cache-Control": "no-store"} {
		if got := p.header.Get(k); got != v {
			t.Errorf("%s is %q, want %q", k, got, v)
		}
	}
	if p.doc == nil {
		t.Fatal("the page is not HTML")
	}
	checkWords(t, p.url.RequestURI(), p.doc, p.served, nil, nil)
	h1s, mains, firstLink := 0, 0, ""
	ids := map[string]bool{}
	labelFor := map[string]bool{}
	each(p.doc, func(n *html.Node) {
		if n.Data == "label" {
			if v, ok := attr(n, "for"); ok {
				labelFor[v] = true
			}
		}
	})
	each(p.doc, func(n *html.Node) {
		for _, a := range n.Attr {
			if a.Key == "style" || onAttr.MatchString(a.Key) {
				t.Errorf("<%s> has the attribute %s", n.Data, a.Key)
			}
			if (a.Key == "href" || a.Key == "src" || a.Key == "action") && strings.HasPrefix(strings.ToLower(strings.TrimSpace(a.Val)), "javascript:") {
				t.Errorf("<%s %s=%q>", n.Data, a.Key, a.Val)
			}
			if (a.Key == "src" || a.Key == "action" || a.Key == "href" && n.Data == "link") && strings.Contains(a.Val, "://") {
				t.Errorf("<%s %s=%q> leaves the page's origin", n.Data, a.Key, a.Val)
			}
			if a.Key == "id" {
				if ids[a.Val] {
					t.Errorf("the id %q appears twice", a.Val)
				}
				ids[a.Val] = true
			}
		}
		switch n.Data {
		case "script":
			if src, _ := attr(n, "src"); !strings.HasPrefix(src, "/ui/static/") || n.FirstChild != nil {
				t.Errorf("a script that is inline or from elsewhere: src %q, text %q", src, text(n))
			}
		case "style":
			t.Error("a <style> element")
		case "h1":
			h1s++
		case "main":
			mains++
		case "a":
			if firstLink == "" {
				firstLink, _ = attr(n, "href")
			}
		case "input", "select", "textarea":
			typ, _ := attr(n, "type")
			id, _ := attr(n, "id")
			if typ != "hidden" && !labelFor[id] && !inside(n, "label") {
				t.Errorf("<%s name=%q> has no label", n.Data, func() string { v, _ := attr(n, "name"); return v }())
			}
		case "table":
			if !hasChild(n, "caption") {
				t.Error("a table without a caption")
			}
		case "th":
			if _, ok := attr(n, "scope"); !ok {
				t.Errorf("<th>%s</th> has no scope", text(n))
			}
		}
	})
	if h1s != 1 || mains != 1 {
		t.Errorf("%d h1 and %d main, want one of each", h1s, mains)
	}
	if firstLink != "#main" {
		t.Errorf("the first link is %q, not the skip link", firstLink)
	}
	for _, secret := range []string{paramSecret, envSecret, clientSec, session, "writs_"} {
		if secret != "" && strings.Contains(p.body, secret) {
			t.Errorf("the page holds the secret %q", secret)
		}
	}
}

func inside(n *html.Node, tag string) bool {
	for p := n.Parent; p != nil; p = p.Parent {
		if p.Type == html.ElementNode && p.Data == tag {
			return true
		}
	}
	return false
}

func hasChild(n *html.Node, tag string) bool {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && c.Data == tag {
			return true
		}
	}
	return false
}

// TestPlanReview checks what a reviewer reads: the plan's changes, its
// author and origin, secrets never, and hostile strings as text.
func TestPlanReview(t *testing.T) {
	f := newFixture(t)
	id := f.plan(nil)
	b := f.browser()
	p := b.signIn("/ui/plans/" + id)
	body := text(p.doc)
	for _, want := range []string{
		"Changes (2)", "tls_private_key.ca_key", "<script>alert(1)</script>",
		"gitlab acme/infra #1849213 (alice)", "acme/infra !42",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("plan review lacks %q", want)
		}
	}
	each(p.doc, func(n *html.Node) {
		if n.Data == "script" && strings.Contains(text(n), "alert") {
			t.Error("a hostile string became a script")
		}
	})
	if strings.Contains(p.body, "<script>alert(1)") {
		t.Error("the page holds the hostile string unescaped")
	}
	// ?as=json is the API's document for the same person.
	b.follow = false
	j := b.get("/ui/plans/" + id + "?as=json")
	var doc server.RemotePlan
	if j.status != 200 || !strings.HasPrefix(j.header.Get("Content-Type"), "application/json") || json.Unmarshal([]byte(j.body), &doc) != nil || doc.Plan.ID != id || doc.Format != "writ.remote-plan/v0" {
		t.Fatalf("?as=json: %d %s", j.status, j.body)
	}
	if j.header.Get("Content-Security-Policy") != ui.CSP {
		t.Error("?as=json has no CSP")
	}
}

func TestSupersededAndPlanning(t *testing.T) {
	f := newFixture(t)
	old := f.plan(nil)
	newer := f.plan(nil)
	b := f.browser()
	p := b.signIn("/ui/plans/" + old)
	if !strings.Contains(text(p.doc), "A newer plan exists.") || !strings.Contains(p.body, `href="/ui/plans/`+newer+`"`) {
		t.Fatal("the older plan does not point to the newer one")
	}
	if p := b.get("/ui/plans/" + newer); strings.Contains(text(p.doc), "A newer plan exists.") {
		t.Fatal("the newest plan says a newer one exists")
	}
}

// TestDenied signs in a person whose groups match no team. The pages
// render the API's refusal with its status.
func TestDenied(t *testing.T) {
	f := newFixture(t)
	id := f.plan(nil)
	claims, info := oidctest.GitLabPerson()
	claims["sub"], claims["preferred_username"], info["sub"], info["groups"] = "3003", "eve", "3003", []string{"acme/other"}
	f.is.SetPerson(claims, info)
	b := f.browser()
	p := b.signIn("/ui/")
	if !strings.Contains(text(p.doc), "Stacks with state 0") {
		t.Errorf("a person in no team sees stacks: %s", text(p.doc))
	}
	for _, path := range []string{"/ui/plans/" + id, "/ui/access", "/ui/stack?stack=network&env=prod"} {
		p := b.get(path)
		if p.status != http.StatusForbidden || !strings.Contains(text(p.doc), "eve may not") {
			t.Errorf("%s: %d %s", path, p.status, text(p.doc))
		}
		checkPage(t, p, "")
	}
	if p := b.get("/ui/whoami"); !strings.Contains(text(p.doc), ui.Nones["no-team"]) {
		t.Errorf("whoami of a person in no team: %s", text(p.doc))
	}
}

// TestFailedReadSaysNoNone makes reads fail and holds each page that
// rests on one to say so, never that a thing is absent: with the runs
// table gone the overview answers 503 while staging holds a plan, and
// Getting started says it could not read every environment's plans, not
// "No plan yet."; the lists that pick their environment from the
// overview, and a run's page, answer with the failure; with the drift
// checks gone a stack's page does not say writ serve keeps no state.
func TestFailedReadSaysNoNone(t *testing.T) {
	f := newFixtureOn(t, controltest.SQLite, "")
	id := f.plan(nil)
	b := f.browser()
	b.signIn("/ui/")
	ctx := context.Background()
	raw := controltest.Raw(t, f.loc)
	exec := func(q string) {
		t.Helper()
		if _, err := raw.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	exec("UPDATE plans SET env = 'staging'")
	exec("ALTER TABLE runs RENAME TO gone_runs")
	if p := b.get("/ui/plans?env=staging"); p.status != 200 || !strings.Contains(p.body, short(id)) {
		t.Fatalf("staging's plans: %d", p.status)
	}
	if p := b.get("/ui/?as=json"); p.status != http.StatusServiceUnavailable {
		t.Fatalf("the overview answered %d", p.status)
	}
	start := b.get("/ui/start")
	if start.status != 200 || strings.Contains(text(start.doc), ui.Nones["no-plan-yet"]) || !strings.Contains(text(start.doc), "could not read every environment's plans") {
		t.Errorf("Getting started with the overview failed: %d %s", start.status, text(start.doc))
	}
	for _, path := range []string{"/ui/plans", "/ui/runs", "/ui/approvals", "/ui/runs/ABCDEFGHIJKLMNOPQRSTUVWXYZ"} {
		if p := b.get(path); p.status != http.StatusServiceUnavailable || !strings.Contains(text(p.doc), "the control database") {
			t.Errorf("%s with a read failed: %d %s", path, p.status, text(p.doc))
		}
	}
	exec("ALTER TABLE gone_runs RENAME TO runs")
	exec("ALTER TABLE drift_checks RENAME TO gone_drift_checks")
	stack := b.get("/ui/stack?stack=network&env=prod")
	if stack.status != 200 || strings.Contains(text(stack.doc), ui.Nones["no-state"]) || !strings.Contains(text(stack.doc), "could not read the drift record") {
		t.Errorf("a stack's page with its drift read failed: %d %s", stack.status, text(stack.doc))
	}
	checkPage(t, start, "")
	checkPage(t, stack, "")
}

// TestDevFailedReadSaysNoNone holds a dev control plane, which names its
// dev environment when it holds none yet, to the same rule: with the drift
// checks gone the overview answers 503 while staging holds a plan, and the
// lists that pick their environment from the overview answer with that
// failure, never an empty list of dev.
func TestDevFailedReadSaysNoNone(t *testing.T) {
	x := newTofuUI(t)
	p := x.planEnded(second, "staging")
	raw := controltest.Raw(t, x.cfg.Database)
	if _, err := raw.ExecContext(context.Background(), "ALTER TABLE drift_checks RENAME TO gone_drift_checks"); err != nil {
		t.Fatal(err)
	}
	if status, _, body := visit(t, x.c, "GET", x.base+"/ui/plans?env=staging", nil); status != 200 || !strings.Contains(body, short(p.Plan.ID)) {
		t.Fatalf("staging's plans: %d", status)
	}
	if status, _, _ := visit(t, x.c, "GET", x.base+"/ui/?as=json", nil); status != http.StatusServiceUnavailable {
		t.Fatalf("the overview answered %d", status)
	}
	for _, path := range []string{"/ui/plans", "/ui/runs", "/ui/approvals"} {
		if status, _, body := visit(t, x.c, "GET", x.base+path, nil); status != http.StatusServiceUnavailable || !strings.Contains(body, "the control database") || strings.Contains(body, ui.Nones["no-plan"]) {
			t.Errorf("%s with the overview failed: %d %s", path, status, tagless(body))
		}
	}
}

// TestCrossSitePost checks that a form post from another site is refused
// and changes nothing, and that a same-origin one works.
func TestCrossSitePost(t *testing.T) {
	f := newFixture(t)
	b := f.browser()
	b.signIn("/ui/")
	b.follow = false
	for _, tt := range []struct{ name, site, origin string }{
		{"Sec-Fetch-Site cross-site", "cross-site", ""},
		{"Sec-Fetch-Site same-site", "same-site", ""},
		{"Origin of another host", "", "https://evil.example.com"},
	} {
		for _, path := range []string{"/ui/logout", "/ui/settings", "/ui/plans/" + strings.Repeat("0", 32) + "/approve"} {
			req, _ := http.NewRequest("POST", f.url+path, strings.NewReader("theme=dark"))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if tt.site != "" {
				req.Header.Set("Sec-Fetch-Site", tt.site)
			}
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			p := b.do(req)
			if p.status != http.StatusForbidden || len(p.header["Set-Cookie"]) != 0 {
				t.Errorf("%s to %s: %d %v", tt.name, path, p.status, p.header["Set-Cookie"])
			}
		}
	}
	if p := b.get("/ui/"); p.status != 200 {
		t.Fatalf("a refused sign-out ended the session: %d", p.status)
	}
	p := b.post("/ui/settings", url.Values{"theme": {"dark"}}, "same-origin")
	if p.status != http.StatusSeeOther || cookie(p.header, "writ_theme") == nil {
		t.Fatalf("a same-origin post: %d", p.status)
	}
	if p := b.get("/ui/settings"); !strings.Contains(p.body, `<html lang="en" data-theme="dark">`) {
		t.Fatal("the theme cookie does not reach the page")
	}
	if p := b.post("/ui/settings", url.Values{"theme": {`"><script>`}}, "same-origin"); p.status != http.StatusBadRequest {
		t.Fatalf("a bad theme: %d", p.status)
	}
}

// TestCookiesOnlyOnUI checks that /v1 takes no cookie and sets none, and
// that a session is no bearer token.
func TestCookiesOnlyOnUI(t *testing.T) {
	f := newFixture(t)
	b := f.browser()
	b.signIn("/ui/")
	p := b.get("/v1/whoami")
	if p.status != http.StatusUnauthorized || len(p.header["Set-Cookie"]) != 0 {
		t.Fatalf("/v1 with only the session cookie: %d %v", p.status, p.header["Set-Cookie"])
	}
	var token string
	for _, c := range b.c.Jar.Cookies(mustURL(f.url + "/ui/")) {
		if c.Name == "__Host-writ_session" {
			token = c.Value
		}
	}
	req, _ := http.NewRequest("GET", f.url+"/v1/whoami", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if p := f.browser().do(req); p.status != http.StatusUnauthorized || !strings.Contains(p.body, "not one writ login gave") {
		t.Fatalf("a session as a bearer token: %d %s", p.status, p.body)
	}
	b.follow = false
	if p := b.get("/"); p.status != http.StatusSeeOther || p.header.Get("Location") != "/ui/" {
		t.Fatalf("GET /: %d %s", p.status, p.header.Get("Location"))
	}
}

func TestStaticAssets(t *testing.T) {
	f := newFixture(t)
	b := f.browser()
	p := b.signIn("/ui/")
	var css string
	each(p.doc, func(n *html.Node) {
		if h, _ := attr(n, "href"); n.Data == "link" && strings.HasSuffix(h, "/writ.css") {
			css = h
		}
	})
	req, _ := http.NewRequest("GET", f.url+css, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := (&http.Client{Transport: &http.Transport{DisableCompression: true, TLSClientConfig: &tls.Config{RootCAs: f.roots}}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	h := resp.Header
	if resp.StatusCode != 200 || h.Get("Cache-Control") != "public, max-age=31536000, immutable" || h.Get("Content-Encoding") != "gzip" ||
		!strings.HasPrefix(h.Get("Content-Type"), "text/css") || h.Get("Content-Security-Policy") != ui.CSP {
		t.Fatalf("%s: %d %v", css, resp.StatusCode, h)
	}
	if p := b.get(strings.Replace(css, "/static/", "/static/0", 1)); p.status != 404 {
		t.Fatalf("a wrong hash: %d", p.status)
	}
}

// TestNoTrustedTypes forbids the html/template types that mark text as
// safe, so every value a page shows is escaped by its context.
func TestNoTrustedTypes(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`template\.(HTML|HTMLAttr|JS|JSStr|CSS|URL|Srcset)\b`)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, _ := os.ReadFile(e.Name())
		if m := re.Find(b); m != nil {
			t.Errorf("%s uses %s", e.Name(), m)
		}
	}
}

// tofuChange is one resource change of `tofu show -json`, cut from a real
// tofu 1.13.9 plan of hashicorp/random.
func tofuChange(addr, actions, reason string) string {
	return `{"address":"` + addr + `","mode":"managed","type":"random_pet","name":"p","provider_name":"registry.opentofu.org/hashicorp/random",
  "change":{"actions":` + actions + `,"before":{"id":"artistic-katydid","length":2},"after":null,"after_unknown":{},"before_sensitive":{},"after_sensitive":false},"action_reason":"` + reason + `"}`
}

// An OpenTofu plan's page lists each change with its actions and reason,
// and the counts of OpenTofu's summary. A plan that deletes or forgets a
// resource never reads as one that changes nothing.
func TestTofuPlanPage(t *testing.T) {
	f := newFixture(t)
	b := f.browser()
	b.signIn("/ui/plans")
	noop := `{"address":"random_pet.kept","mode":"managed","type":"random_pet","name":"kept","provider_name":"registry.opentofu.org/hashicorp/random",
  "change":{"actions":["no-op"],"before":{"id":"x"},"after":{"id":"x"},"after_unknown":{},"before_sensitive":{},"after_sensitive":{}}}`
	for _, tc := range []struct {
		name, change string
		want         []string
	}{
		{"delete", tofuChange("random_pet.old", `["delete"]`, "delete_because_no_resource_config"),
			[]string{"Changes (1)", "- random_pet.old delete Actions: delete. Reason: delete_because_no_resource_config", "0 to add ~ 0 to change - 1 to destroy -/+ 0 to replace 0 to forget 0 to import"}},
		{"forget", tofuChange("random_pet.left", `["forget"]`, "delete_because_no_resource_config"),
			[]string{"Changes (1)", "~ random_pet.left forget delete Actions: forget. Reason: delete_because_no_resource_config", "0 to destroy -/+ 0 to replace 1 to forget 0 to import"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.tofu.mu.Lock()
			f.tofu.doc = func() string {
				return `{"format_version":"1.2","terraform_version":"1.13.9","resource_changes":[` + tc.change + `,` + noop + `]}`
			}
			f.tofu.mu.Unlock()
			p := b.get("/ui/plans/" + f.plan(nil))
			body := strings.Join(strings.Fields(text(p.doc)), " ")
			for _, want := range tc.want {
				if !strings.Contains(body, want) {
					t.Errorf("the page lacks %q:\n%s", want, body)
				}
			}
			if strings.Contains(body, "changes no") || strings.Contains(body, "random_pet.kept") || strings.Contains(body, "could not read") {
				t.Errorf("the page of a plan that changes a resource:\n%s", body)
			}
		})
	}
}

// TestSidebarSaysItCouldNotLoad fails the sidebar of a page whose overview
// read failed when it counts anything: it says it could not load, and
// shows no count of environments and no stacks that match the filter, on
// a dev control plane and a team's.
func TestSidebarSaysItCouldNotLoad(t *testing.T) {
	sidebar := func(t *testing.T, status int, doc *html.Node) {
		t.Helper()
		side := first(doc, classed("side"))
		if status != http.StatusOK || side == nil {
			t.Fatalf("the page answered %d with no sidebar", status)
		}
		words := strings.Join(strings.Fields(text(side)), " ")
		if !strings.Contains(words, sidebarFailed) {
			t.Errorf("the sidebar does not say it could not load: %q", words)
		}
		if n := everyOf(side, classed("n")); len(n) > 0 || strings.Contains(words, "match") {
			t.Errorf("the sidebar counts after a failed read: %q", words)
		}
	}
	t.Run("dev", func(t *testing.T) {
		x := newTofuUI(t)
		x.planEnded(second, "staging")
		breakOverview(t, x.cfg.Database)
		status, _, body := visit(t, x.c, "GET", x.base+"/ui/plans?env=staging&find=app", nil)
		doc, err := html.Parse(strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		sidebar(t, status, doc)
	})
	// A page whose own read failed names the refusal's document as the
	// record of its diagnostics, so the render check finds it.
	t.Run("failed pages", func(t *testing.T) {
		f := newFixtureOn(t, controltest.SQLite, "")
		f.plan(nil)
		b := f.browser()
		b.signIn("/ui/")
		breakOverview(t, f.loc)
		for _, path := range []string{"/ui/", "/ui/plans", "/ui/runs", "/ui/approvals"} {
			checkPage(t, b.get(path), "")
		}
	})
	t.Run("team", func(t *testing.T) {
		f := newFixtureOn(t, controltest.SQLite, "")
		f.plan(nil)
		b := f.browser()
		b.signIn("/ui/")
		breakOverview(t, f.loc)
		p := b.get("/ui/repos?find=network")
		sidebar(t, p.status, p.doc)
		checkPage(t, p, "")
	})
}

// sidebarFailed is what the sidebar says when its read failed.
const sidebarFailed = "writ serve could not read the environments."

// breakOverview makes every read of the overview fail: it reads every
// stack's last drift check.
func breakOverview(t *testing.T, database string) {
	t.Helper()
	if _, err := controltest.Raw(t, database).ExecContext(context.Background(), "ALTER TABLE drift_checks RENAME TO gone_drift_checks"); err != nil {
		t.Fatal(err)
	}
}

// TestRecordNamesWhatThePageShows fails a data-record that names a
// finished plan by its ref, which the pages that show it name by its id
// alone: the ref is nowhere in the page, and checkPage holds every
// data-record value to the page's text and links.
func TestRecordNamesWhatThePageShows(t *testing.T) {
	f := newFixtureOn(t, controltest.SQLite, "")
	p := f.planEnded(nil)
	b := f.browser()
	b.signIn("/ui/")
	for _, path := range []string{"/ui/", "/ui/plans?env=prod", "/ui/plans/" + p.ID} {
		t.Run(path, func(t *testing.T) {
			page := b.get(path)
			if page.status != http.StatusOK || !strings.Contains(page.body, p.ID) {
				t.Fatalf("%s answered %d without the plan's id", path, page.status)
			}
			if strings.Contains(page.body, p.Ref) {
				t.Errorf("%s holds the ref of a plan it names by its id", path)
			}
			checkPage(t, page, "")
		})
	}
}
