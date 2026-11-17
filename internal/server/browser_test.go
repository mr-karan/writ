package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/control/controltest"
	"github.com/mr-karan/writ/internal/server"
)

const loopback = "http://127.0.0.1:43123/callback"

// browserLogin is one writ login in a browser, as the CLI holds it.
type browserLogin struct {
	verifier, state, handle, authURL string
}

func newBrowserLogin() *browserLogin {
	return &browserLogin{verifier: oauth2.GenerateVerifier(), state: "state-of-the-cli-0123456789"}
}

func browserStart(redirect, state, challenge string) *strings.Reader {
	b, _ := json.Marshal(server.BrowserRequest{RedirectURI: redirect, State: state, CodeChallenge: challenge})
	return strings.NewReader(string(b))
}

// startBrowser starts bl at writ serve.
func (pf *peopleFixture) startBrowser(bl *browserLogin) {
	pf.t.Helper()
	r := pf.do("POST", "/v1/auth/browser", "", browserStart(loopback, bl.state, oauth2.S256ChallengeFromVerifier(bl.verifier)))
	if r.status != http.StatusOK || r.doc["format"] != server.FormatBrowser {
		pf.t.Fatalf("start: %d %s", r.status, r.body)
	}
	bl.handle, bl.authURL = r.doc["handle"].(string), r.doc["auth_url"].(string)
}

// consent stands in for the browser: it opens authURL at the issuer,
// which signs the person in at once, and returns the query of the
// redirect back to the loopback listener, which it does not follow.
func (pf *peopleFixture) consent(authURL string) url.Values {
	pf.t.Helper()
	c := pf.is.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Get(authURL)
	if err != nil {
		pf.t.Fatal(err)
	}
	resp.Body.Close()
	to, err := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusFound || err != nil || to.Scheme+"://"+to.Host+to.Path != loopback {
		pf.t.Fatalf("the issuer answered %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	return to.Query()
}

func (pf *peopleFixture) finishBrowser(handle, code, state, verifier string) response {
	pf.t.Helper()
	b, _ := json.Marshal(server.BrowserTokenRequest{Handle: handle, Code: code, State: state, CodeVerifier: verifier})
	return pf.do("POST", "/v1/auth/browser/token", "", strings.NewReader(string(b)))
}

func TestBrowserLogin(t *testing.T) {
	pf := newPeopleFixture(t)
	// The operator registers one loopback redirect URI, and the issuer
	// takes it on any port.
	pf.is.RegisterRedirects("http://127.0.0.1/callback")
	if info := pf.do("GET", "/v1/server", "", nil); info.doc["login"] != true || len(info.doc["device_issuers"].([]any)) != 1 {
		t.Fatalf("server info: %s", info.body)
	}
	bl := newBrowserLogin()
	pf.startBrowser(bl)
	u, err := url.Parse(bl.authURL)
	if err != nil || !strings.HasPrefix(bl.authURL, pf.is.URL+"/oauth/authorize?") {
		t.Fatalf("auth URL %q", bl.authURL)
	}
	q := u.Query()
	if q.Get("redirect_uri") != loopback || q.Get("state") != bl.state || q.Get("code_challenge_method") != "S256" ||
		q.Get("code_challenge") != oauth2.S256ChallengeFromVerifier(bl.verifier) || len(q.Get("nonce")) < 32 || q.Get("client_id") != "writ-client" {
		t.Fatalf("auth URL query %v", q)
	}
	back := pf.consent(bl.authURL)
	if back.Get("state") != bl.state || back.Get("code") == "" {
		t.Fatalf("the redirect back: %v", back)
	}
	r := pf.finishBrowser(bl.handle, back.Get("code"), bl.state, bl.verifier)
	if r.status != http.StatusOK || r.doc["status"] != server.SessionSignedIn {
		t.Fatalf("finish: %d %s", r.status, r.body)
	}
	token := r.doc["token"].(string)
	sess := r.doc["session"].(map[string]any)
	if !strings.HasPrefix(token, server.SessionPrefix) || sess["person"] != pf.personOf("2002") || sess["carrier"] != "cli" {
		t.Fatalf("session %v", r.doc)
	}
	ends, _ := time.Parse(time.RFC3339, sess["ends_at"].(string))
	if want := pf.clock.Now().Add(time.Hour); ends.Sub(want) > time.Second || want.Sub(ends) > time.Second {
		t.Errorf("the session ends %s, want session_ttl after the sign-in, %s", ends, want)
	}
	if r := pf.finishBrowser(bl.handle, back.Get("code"), bl.state, bl.verifier); r.status != http.StatusUnauthorized || r.class() != "auth" {
		t.Fatalf("a handle used twice: %d %s", r.status, r.body)
	}
	who := pf.do("GET", "/v1/whoami", token, nil)
	p := who.doc["principal"].(map[string]any)
	if who.status != http.StatusOK || p["kind"] != "person" || p["session"] != sess["id"] || len(who.doc["teams"].([]any)) != 1 {
		t.Fatalf("whoami with the session: %d %s", who.status, who.body)
	}
	for name, text := range map[string]string{"the server's log": pf.log.String(), "the control database": pf.dump(), "whoami": string(who.body)} {
		for _, secret := range []string{token, strings.TrimPrefix(token, server.SessionPrefix), bl.handle, bl.verifier, back.Get("code"), clientSecret} {
			if strings.Contains(text, secret) {
				t.Errorf("%s holds a secret of the sign-in: %.12s...", name, secret)
			}
		}
	}
	var started, signedIn int
	for _, e := range pf.audit(controltest.AuditFilter{}) {
		switch {
		case e.Kind == "sign-in-started" && e.Route == "/v1/auth/browser" && e.Decision == control.Allowed:
			started++
		case e.Kind == "sign-in" && e.Via == "cli session" && e.Person == pf.personOf("2002") && e.Decision == control.Allowed:
			signedIn++
		}
	}
	if started != 1 || signedIn != 1 {
		t.Errorf("audit: %d starts and %d sign-ins, want 1 and 1", started, signedIn)
	}
}

func TestBrowserLoginRefusals(t *testing.T) {
	// begin starts a sign-in and returns the code the issuer sent back.
	begin := func(pf *peopleFixture) (*browserLogin, string) {
		bl := newBrowserLogin()
		pf.startBrowser(bl)
		return bl, pf.consent(bl.authURL).Get("code")
	}
	failed := func(t *testing.T, pf *peopleFixture, r response, check string) {
		t.Helper()
		if r.status != http.StatusUnauthorized || r.class() != "auth" {
			t.Fatalf("want 401 auth: %d %s", r.status, r.body)
		}
		if rows := pf.audit(controltest.AuditFilter{Decision: control.AuthFailed, Limit: 1}); len(rows) == 0 || rows[0].Check != check || rows[0].Route != "/v1/auth/browser/token" {
			t.Fatalf("audit %+v, want check %s", rows, check)
		}
	}
	t.Run("state mismatch", func(t *testing.T) {
		pf := newPeopleFixture(t)
		bl, code := begin(pf)
		failed(t, pf, pf.finishBrowser(bl.handle, code, "a-state-another-page-made-01", bl.verifier), "state")
		// The handle died with the refusal, so the right state is too late.
		failed(t, pf, pf.finishBrowser(bl.handle, code, bl.state, bl.verifier), "handle")
	})
	t.Run("replayed code", func(t *testing.T) {
		pf := newPeopleFixture(t)
		bl, code := begin(pf)
		if r := pf.finishBrowser(bl.handle, code, bl.state, bl.verifier); r.status != http.StatusOK {
			t.Fatalf("first use: %d %s", r.status, r.body)
		}
		// A second sign-in with the same verifier and state reaches the
		// issuer with the used code, which it refuses.
		again := &browserLogin{verifier: bl.verifier, state: bl.state}
		pf.startBrowser(again)
		failed(t, pf, pf.finishBrowser(again.handle, code, bl.state, bl.verifier), "token endpoint")
	})
	t.Run("missing PKCE", func(t *testing.T) {
		pf := newPeopleFixture(t)
		for _, challenge := range []string{"", "plain-verifier-is-not-a-challenge", oauth2.S256ChallengeFromVerifier("x") + "="} {
			if r := pf.do("POST", "/v1/auth/browser", "", browserStart(loopback, "state-of-the-cli-0123456789", challenge)); r.status != http.StatusBadRequest || !strings.Contains(string(r.body), "code_challenge") {
				t.Fatalf("a start with challenge %q: %d %s", challenge, r.status, r.body)
			}
		}
		bl, code := begin(pf)
		failed(t, pf, pf.finishBrowser(bl.handle, code, bl.state, ""), "pkce")
		bl, code = begin(pf)
		failed(t, pf, pf.finishBrowser(bl.handle, code, bl.state, oauth2.GenerateVerifier()), "pkce")
	})
	t.Run("non-loopback redirect", func(t *testing.T) {
		pf := newPeopleFixture(t)
		for _, uri := range []string{
			"", "https://writ.example.com/callback", pf.http.URL + "/ui/callback", "http://localhost:43123/callback",
			"http://[::1]:43123/callback", "http://127.0.0.1/callback", "http://127.0.0.1:0/callback", "http://127.0.0.1:65536/callback",
			"http://127.0.0.1:043123/callback", "http://127.0.0.1:43123/other", "http://127.0.0.1:43123/callback?next=x",
			"http://127.0.0.1:43123/callback#x", "http://user@127.0.0.1:43123/callback", "http://127.0.0.1.example.com:43123/callback",
			"https://127.0.0.1:43123/callback",
		} {
			pf.clock.Add(time.Second)
			r := pf.do("POST", "/v1/auth/browser", "", browserStart(uri, "state-of-the-cli-0123456789", oauth2.S256ChallengeFromVerifier(oauth2.GenerateVerifier())))
			if r.status != http.StatusBadRequest || !strings.Contains(string(r.body), "redirect_uri") {
				t.Errorf("redirect_uri %q: %d %s", uri, r.status, r.body)
			}
		}
	})
	t.Run("callback after the timeout", func(t *testing.T) {
		pf := newPeopleFixture(t)
		bl, code := begin(pf)
		pf.clock.Add(11 * time.Minute)
		r := pf.finishBrowser(bl.handle, code, bl.state, bl.verifier)
		failed(t, pf, r, "handle")
		if !strings.Contains(string(r.body), "ran out") {
			t.Fatalf("a late callback: %s", r.body)
		}
	})
	t.Run("ID token for another audience", func(t *testing.T) {
		pf := newPeopleFixture(t)
		pf.is.EditIDToken(func(c map[string]any) { c["aud"] = "other-client" })
		bl, code := begin(pf)
		failed(t, pf, pf.finishBrowser(bl.handle, code, bl.state, bl.verifier), "audience")
	})
	t.Run("ID token with another nonce", func(t *testing.T) {
		pf := newPeopleFixture(t)
		pf.is.EditIDToken(func(c map[string]any) { c["nonce"] = "the-nonce-of-another-sign-in" })
		bl, code := begin(pf)
		failed(t, pf, pf.finishBrowser(bl.handle, code, bl.state, bl.verifier), "nonce")
	})
	t.Run("unknown handle", func(t *testing.T) {
		pf := newPeopleFixture(t)
		failed(t, pf, pf.finishBrowser("not-a-handle", "code", "state-of-the-cli-0123456789", oauth2.GenerateVerifier()), "handle")
	})
	t.Run("no people issuer", func(t *testing.T) {
		f := newFixture(t, nil)
		r := f.do("POST", "/v1/auth/browser", "", browserStart(loopback, "state-of-the-cli-0123456789", oauth2.S256ChallengeFromVerifier(oauth2.GenerateVerifier())))
		if r.status != http.StatusNotFound || !strings.Contains(string(r.body), "no people issuer") {
			t.Fatalf("a browser sign-in with no people issuer: %d %s", r.status, r.body)
		}
	})
}

// startFrom starts a sign-in of flow, "device" or "browser", from the
// client address addr.
func (pf *peopleFixture) startFrom(addr, flow string, bl *browserLogin) response {
	pf.t.Helper()
	body := jsonBody(`{}`)
	if flow == "browser" {
		body = browserStart(loopback, bl.state, oauth2.S256ChallengeFromVerifier(bl.verifier))
	}
	req := httptest.NewRequest("POST", "/v1/auth/"+flow, body)
	req.RemoteAddr = addr + ":40000"
	return handle(pf.fixture, req)
}

// One address that starts sign-ins and never finishes them fills its own
// share of places, in both flows together, and no more: another address
// still signs in, and the first gets a place back when one of its
// sign-ins ends.
func TestLoginsPerSource(t *testing.T) {
	pf := newPeopleFixture(t)
	pf.is.RegisterRedirects("http://127.0.0.1/callback")
	const prober, person = "198.51.100.7", "203.0.113.9"
	var held *browserLogin
	for i := range server.MaxLoginsPerSource {
		pf.clock.Add(time.Second)
		flow, bl := "device", newBrowserLogin()
		if i%2 == 0 {
			flow = "browser"
		}
		r := pf.startFrom(prober, flow, bl)
		if r.status != http.StatusOK {
			t.Fatalf("start %d (%s) from %s: %d %s", i, flow, prober, r.status, r.body)
		}
		if flow == "browser" {
			bl.handle, bl.authURL = r.doc["handle"].(string), r.doc["auth_url"].(string)
			held = bl
		}
	}
	for _, flow := range []string{"device", "browser"} {
		pf.clock.Add(time.Second)
		r := pf.startFrom(prober, flow, newBrowserLogin())
		if r.status != http.StatusTooManyRequests || !strings.Contains(string(r.body), "from "+prober) {
			t.Fatalf("a %s start past the share of %s: %d %s", flow, prober, r.status, r.body)
		}
	}
	if got := pf.srv.Logins(); got != server.MaxLoginsPerSource {
		t.Fatalf("%d sign-ins wait, want %d", got, server.MaxLoginsPerSource)
	}

	bl := newBrowserLogin()
	r := pf.startFrom(person, "browser", bl)
	if r.status != http.StatusOK {
		t.Fatalf("a start from %s while %s holds its share: %d %s", person, prober, r.status, r.body)
	}
	bl.handle = r.doc["handle"].(string)
	code := pf.consent(r.doc["auth_url"].(string)).Get("code")
	if r := pf.finishBrowser(bl.handle, code, bl.state, bl.verifier); r.status != http.StatusOK || r.doc["status"] != server.SessionSignedIn {
		t.Fatalf("the sign-in of %s: %d %s", person, r.status, r.body)
	}

	code = pf.consent(held.authURL).Get("code")
	if r := pf.finishBrowser(held.handle, code, held.state, held.verifier); r.status != http.StatusOK {
		t.Fatalf("a sign-in of %s: %d %s", prober, r.status, r.body)
	}
	pf.clock.Add(time.Second)
	if r := pf.startFrom(prober, "browser", newBrowserLogin()); r.status != http.StatusOK {
		t.Fatalf("a start from %s once one of its sign-ins ended: %d %s", prober, r.status, r.body)
	}
}

// A sign-in start whose audit row cannot be written answers 503 with no
// handle, and gives back the place it took.
func TestLoginStartWithoutItsAuditRow(t *testing.T) {
	controltest.Each(t, func(t *testing.T, e controltest.Engine) {
		pf := newPeopleFixtureOn(t, e)
		release := holdAudit(t, pf.loc)
		for _, flow := range []string{"device", "browser"} {
			r := pf.startFrom("198.51.100.7", flow, newBrowserLogin())
			if r.status != http.StatusServiceUnavailable || r.class() != "state-io" || r.doc["handle"] != nil {
				t.Errorf("a %s start while the audit table is locked: %d %s", flow, r.status, r.body)
			}
		}
		release()
		if got := pf.srv.Logins(); got != 0 {
			t.Fatalf("%d sign-ins wait after starts that were not recorded", got)
		}
	})
}
