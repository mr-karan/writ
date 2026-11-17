package ui_test

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/control/controltest"
	"github.com/mr-karan/writ/internal/oidc"
	"github.com/mr-karan/writ/internal/oidc/oidctest"
	"github.com/mr-karan/writ/internal/server"
)

// cookie finds a Set-Cookie of the response by name.
func cookie(h http.Header, name string) *http.Cookie {
	for _, c := range (&http.Response{Header: h}).Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// TestSignInFlow walks the authorization code flow with PKCE one redirect
// at a time, and checks each cookie's attributes.
func TestSignInFlow(t *testing.T) {
	f := newFixture(t)
	b := f.browser()
	b.follow = false

	p := b.get("/ui/plans?env=prod")
	if p.status != http.StatusSeeOther || p.header.Get("Location") != "/ui/login?next=%2Fui%2Fplans%3Fenv%3Dprod" {
		t.Fatalf("a page without a session: %d %s", p.status, p.header.Get("Location"))
	}
	login := b.get(p.header.Get("Location"))
	start := ""
	each(login.doc, func(n *html.Node) {
		if h, _ := attr(n, "href"); strings.HasPrefix(h, "/ui/login/start") {
			start = h
		}
	})
	if login.status != 200 || start != "/ui/login/start?issuer=sso&next=%2Fui%2Fplans%3Fenv%3Dprod" {
		t.Fatalf("the sign-in page links %q: %s", start, login.body)
	}

	p = b.get(start)
	to, err := url.Parse(p.header.Get("Location"))
	if p.status != http.StatusSeeOther || err != nil || !strings.HasPrefix(to.String(), f.is.URL+"/oauth/authorize?") {
		t.Fatalf("start: %d %s", p.status, p.header.Get("Location"))
	}
	q := to.Query()
	if q.Get("code_challenge_method") != "S256" || len(q.Get("code_challenge")) != 43 || q.Get("redirect_uri") != f.url+"/ui/callback" ||
		q.Get("client_id") != "writ-client" || q.Get("response_type") != "code" || len(q.Get("state")) != 43 || len(q.Get("nonce")) != 43 {
		t.Fatalf("authorize URL %s", to)
	}
	lc := cookie(p.header, "__Host-writ_login")
	if lc == nil || !lc.HttpOnly || !lc.Secure || lc.SameSite != http.SameSiteLaxMode || lc.Path != "/" || lc.Domain != "" || lc.MaxAge != 600 {
		t.Fatalf("login cookie %+v", lc)
	}
	if strings.Contains(to.String(), clientSec) {
		t.Fatal("the authorize URL holds the client secret")
	}

	p = b.get2(to.String())
	cb, _ := url.Parse(p.header.Get("Location"))
	if p.status != http.StatusFound || cb.Path != "/ui/callback" || cb.Query().Get("state") != q.Get("state") {
		t.Fatalf("issuer: %d %s", p.status, cb)
	}

	p = b.get(cb.RequestURI())
	sc := cookie(p.header, "__Host-writ_session")
	if p.status != http.StatusSeeOther || p.header.Get("Location") != "/ui/plans?env=prod" || sc == nil {
		t.Fatalf("callback: %d %s %v", p.status, p.header.Get("Location"), p.header["Set-Cookie"])
	}
	if !sc.HttpOnly || !sc.Secure || sc.SameSite != http.SameSiteLaxMode || sc.Path != "/" || sc.Domain != "" || sc.MaxAge < 11*3600 || sc.MaxAge > 12*3600 || !strings.HasPrefix(sc.Value, "writs_") {
		t.Fatalf("session cookie %+v", sc)
	}
	if lc := cookie(p.header, "__Host-writ_login"); lc == nil || lc.MaxAge >= 0 {
		t.Fatalf("the callback left the login cookie: %+v", lc)
	}

	p = b.get("/ui/plans?env=prod")
	if p.status != 200 || !strings.Contains(p.body, "bob") {
		t.Fatalf("after sign-in: %d", p.status)
	}

	// The sign-in left an allowed audit row with the person's teams.
	rows := controltest.Audit(t, f.loc, controltest.AuditFilter{Person: bobPerson})
	var signIn *control.Entry
	for i := range rows {
		if rows[i].Kind == "sign-in" {
			signIn = &rows[i]
		}
		if rows[i].Kind == "request" && rows[i].Via != "browser session" {
			t.Errorf("a page's API call has via %q", rows[i].Via)
		}
	}
	if signIn == nil || signIn.Decision != control.Allowed || strings.Join(signIn.Teams, ",") != "platform" || signIn.Principal != "bob" {
		t.Fatalf("sign-in audit %+v", signIn)
	}
	sess, err := f.srv.Session(context.Background(), sc.Value)
	if err != nil || sess.Person != bobPerson || sess.Display != "bob" || strings.Join(sess.Teams, ",") != "platform" {
		t.Fatalf("session %+v %v", sess, err)
	}
}

// get2 sends a GET to an absolute URL, such as the issuer's.
func (b *browser) get2(u string) page {
	b.f.t.Helper()
	req, _ := http.NewRequest("GET", u, nil)
	return b.do(req)
}

// bobPerson is bob's person id: the identity of the sso issuer, and
// his user id.
const bobPerson = "gitlab/user/2002"

// startSignIn follows the sign-in up to the issuer's redirect back, and
// returns the callback's path and query.
func startSignIn(t *testing.T, b *browser, next string) string {
	t.Helper()
	b.follow = false
	p := b.get("/ui/login/start?issuer=sso&next=" + url.QueryEscape(next))
	p = b.get2(p.header.Get("Location"))
	cb, err := url.Parse(p.header.Get("Location"))
	if err != nil || cb.Path != "/ui/callback" {
		t.Fatalf("issuer redirect %s", p.header.Get("Location"))
	}
	return cb.RequestURI()
}

func TestSignInRefusals(t *testing.T) {
	f := newFixture(t)
	tests := []struct {
		name string
		run  func(*browser) page
		want string
	}{
		{"wrong state", func(b *browser) page {
			cb := startSignIn(t, b, "/ui/")
			u, _ := url.Parse(cb)
			q := u.Query()
			q.Set("state", "forged")
			return b.get("/ui/callback?" + q.Encode())
		}, "state does not match"},
		{"no sign-in cookie", func(b *browser) page {
			cb := startSignIn(t, b, "/ui/")
			return f.browser().get(cb)
		}, "did not start in this browser"},
		{"callback used twice", func(b *browser) page {
			cb := startSignIn(t, b, "/ui/")
			if p := b.get(cb); p.status != http.StatusSeeOther {
				t.Fatalf("first callback %d", p.status)
			}
			return b.get(cb)
		}, "did not start in this browser"},
		{"refused consent", func(b *browser) page {
			f.is.SetPerson(nil, nil)
			defer f.is.SetPerson(personOf())
			return b.get(startSignIn(t, b, "/ui/"))
		}, "refused the sign-in: authorization endpoint: access_denied"},
		{"wrong nonce", func(b *browser) page {
			f.is.EditIDToken(func(c map[string]any) { c["nonce"] = "forged" })
			defer f.is.EditIDToken(nil)
			return b.get(startSignIn(t, b, "/ui/"))
		}, "nonce"},
		{"other audience", func(b *browser) page {
			f.is.EditIDToken(func(c map[string]any) { c["aud"] = "another-client" })
			defer f.is.EditIDToken(nil)
			return b.get(startSignIn(t, b, "/ui/"))
		}, "audience"},
		{"unknown issuer", func(b *browser) page {
			b.follow = false
			return b.get("/ui/login/start?issuer=gitlab")
		}, "not one people sign in with"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := f.browser()
			p := tt.run(b)
			if p.status != http.StatusUnauthorized && p.status != http.StatusBadRequest || !strings.Contains(p.body, tt.want) {
				t.Fatalf("%d %s, want %q", p.status, p.body, tt.want)
			}
			if c := cookie(p.header, "__Host-writ_session"); c != nil && c.Value != "" {
				t.Fatal("a refused sign-in set a session")
			}
		})
	}
	rows := controltest.Audit(t, f.loc, controltest.AuditFilter{Decision: control.AuthFailed})
	checks := map[string]bool{}
	for _, r := range rows {
		if r.Kind == "sign-in" {
			checks[r.Check] = true
		}
	}
	if !checks[oidc.CheckNonce.String()] || !checks[oidc.CheckAudience.String()] {
		t.Fatalf("failed sign-ins in the audit log: %v", checks)
	}
}

func personOf() (map[string]any, map[string]any) { return oidctest.GitLabPerson() }

// TestNextStaysUnderUI sends sign-ins with a next that tries to leave
// /ui/; each lands on /ui/.
func TestNextStaysUnderUI(t *testing.T) {
	f := newFixture(t)
	for _, next := range []string{"https://evil.example.com/ui/", "//evil.example.com/ui/", "/v1/plans", "/ui/login", "/ui/callback?code=x", "javascript:alert(1)", "/ui\\@evil.example.com", "", "/ui/../v1/config", "/ui/%2e%2e/v1/config", "/ui/./../v1"} {
		t.Run(next, func(t *testing.T) {
			b := f.browser()
			p := b.get(startSignIn(t, b, next))
			if p.status != http.StatusSeeOther || p.header.Get("Location") != "/ui/" && !strings.HasPrefix(p.header.Get("Location"), "/ui/") {
				t.Fatalf("next %q landed on %q", next, p.header.Get("Location"))
			}
			if loc := p.header.Get("Location"); strings.Contains(loc, "evil") || strings.Contains(loc, "/v1/") || strings.Contains(loc, "..") {
				t.Fatalf("next %q landed on %q", next, loc)
			}
		})
	}
	b := f.browser()
	if p := b.get(startSignIn(t, b, "/ui/plans?env=prod&status=pending")); p.header.Get("Location") != "/ui/plans?env=prod&status=pending" {
		t.Fatalf("a good next became %q", p.header.Get("Location"))
	}
}

func TestSignOutAndEnd(t *testing.T) {
	f := newFixture(t)
	b := f.browser()
	b.signIn("/ui/")
	b.follow = false
	p := b.post("/ui/logout", nil, "same-origin")
	if p.status != http.StatusSeeOther || !strings.HasPrefix(p.header.Get("Location"), "/ui/login?signed_out=1") {
		t.Fatalf("sign-out %d %s", p.status, p.header.Get("Location"))
	}
	if c := cookie(p.header, "__Host-writ_session"); c == nil || c.MaxAge >= 0 {
		t.Fatalf("sign-out left the cookie: %+v", c)
	}
	if p := b.get("/ui/"); p.status != http.StatusSeeOther || !strings.HasPrefix(p.header.Get("Location"), "/ui/login") {
		t.Fatalf("after sign-out: %d %s", p.status, p.header.Get("Location"))
	}
	rows := controltest.Audit(t, f.loc, controltest.AuditFilter{Person: bobPerson})
	if rows[0].Kind != "sign-out" && rows[1].Kind != "sign-out" {
		t.Fatalf("no sign-out row: %+v", rows[:2])
	}

	// A session past its end sends a GET to sign-in and back, and drops a
	// POST: the person lands on the page the form was on.
	b = f.browser()
	b.signIn("/ui/settings")
	f.clock.Add(12*time.Hour + time.Minute)
	b.follow = false
	if p := b.get("/ui/access"); p.status != http.StatusSeeOther || p.header.Get("Location") != "/ui/login?next=%2Fui%2Faccess" {
		t.Fatalf("an ended session: %d %s", p.status, p.header.Get("Location"))
	}
	req, _ := http.NewRequest("POST", f.url+"/ui/settings", strings.NewReader("theme=dark"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	p = b.do(req)
	if p.status != http.StatusSeeOther || p.header.Get("Location") != "/ui/login?next=%2Fui%2Fsettings" || cookie(p.header, "writ_theme") != nil {
		t.Fatalf("a post without a session: %d %s", p.status, p.header.Get("Location"))
	}
}

// TestCallbackChecksNextAgain changes the next in the browser's sign-in
// cookie, which the browser holds, before the callback; the callback lands
// on /ui/ and not on the cookie's next.
func TestCallbackChecksNextAgain(t *testing.T) {
	f := newFixture(t)
	for _, next := range []string{"//evil.example.com/", "https://evil.example.com/ui/", "/\\evil.example.com", "/v1/plans"} {
		t.Run(next, func(t *testing.T) {
			b := f.browser()
			cb := startSignIn(t, b, "/ui/plans")
			u, _ := url.Parse(f.url + "/ui/")
			var lc *http.Cookie
			for _, c := range b.c.Jar.Cookies(u) {
				if c.Name == "__Host-writ_login" {
					lc = c
				}
			}
			if lc == nil {
				t.Fatal("no sign-in cookie")
			}
			raw, err := base64.RawURLEncoding.DecodeString(lc.Value)
			if err != nil {
				t.Fatal(err)
			}
			var st map[string]any
			if err := json.Unmarshal(raw, &st); err != nil {
				t.Fatal(err)
			}
			st["next"] = next
			raw, _ = json.Marshal(st)
			b.c.Jar.SetCookies(u, []*http.Cookie{{Name: lc.Name, Value: base64.RawURLEncoding.EncodeToString(raw), Path: "/", Secure: true}})
			p := b.get(cb)
			if p.status != http.StatusSeeOther || p.header.Get("Location") != "/ui/" {
				t.Fatalf("a cookie next %q landed on %d %q", next, p.status, p.header.Get("Location"))
			}
		})
	}
}

// GitLab.com's discovery document lists no device_authorization_endpoint.
// With such a people issuer, writ login is refused, and a person still
// signs in to the control UI and approves a CI job's plan there.
func TestApproveWithoutDeviceFlow(t *testing.T) {
	f := newFixture(t, func(is *oidctest.Issuer) { is.SetNoDevice(true) })
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: f.roots}}}
	resp, err := c.Post(f.url+"/v1/auth/device", "application/json", strings.NewReader(`{"issuer":""}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(body), "names no device_authorization_endpoint") {
		t.Fatalf("writ login with no device flow: %d %s", resp.StatusCode, body)
	}

	id := f.plan(nil)
	b := f.browser()
	b.signIn("/ui/plans/" + id)
	if p := b.post("/ui/plans/"+id+"/approve", url.Values{}, "same-origin"); p.status != 200 || p.url.Path != "/ui/plans/"+id {
		t.Fatalf("approve in the control UI: %d %s %s", p.status, p.url, text(p.doc))
	}
	b.follow = false
	var doc server.RemotePlan
	if j := b.get("/ui/plans/" + id + "?as=json"); j.status != 200 || json.Unmarshal([]byte(j.body), &doc) != nil || doc.Plan == nil {
		t.Fatalf("the plan: %d %s", j.status, j.body)
	}
	if by := doc.Plan.Approvals.ApprovedBy; doc.Plan.Status != "approved" || len(by) != 1 || by[0].Person != "gitlab/user/2002" {
		t.Fatalf("the plan after bob's approval in the control UI: %s %+v", doc.Plan.Status, by)
	}
}
