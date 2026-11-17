package server_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/control/controltest"
	"github.com/mr-karan/writ/internal/oidc"
	"github.com/mr-karan/writ/internal/server"
	"github.com/mr-karan/writ/internal/sqldb"
	"github.com/mr-karan/writ/internal/ui"
)

// holdAudit holds a write lock that keeps every audit row out of the
// control database at loc, as a busy disk's commits do, until the test
// ends or release is called.
func holdAudit(t *testing.T, loc string) (release func()) {
	t.Helper()
	ctx := context.Background()
	tx, err := controltest.Raw(t, loc).BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	// SQLite's BEGIN IMMEDIATE already holds the write lock; Postgres
	// needs the table's.
	if sqldb.IsPostgres(loc) {
		if _, err := tx.ExecContext(ctx, "LOCK TABLE audit IN ACCESS EXCLUSIVE MODE"); err != nil {
			t.Fatal(err)
		}
	}
	release = func() { _ = tx.Rollback() }
	t.Cleanup(release)
	return release
}

func rowsOf(t *testing.T, loc string, r response) int {
	t.Helper()
	id := r.header.Get("X-Request-Id")
	if id == "" {
		t.Fatalf("no X-Request-Id: %s", r.body)
	}
	return len(controltest.Audit(t, loc, controltest.AuditFilter{Request: id}))
}

// A request whose audit row cannot be written does not succeed: it
// answers 503 state-io in its route's format, with none of the answer it
// would have had.
func TestNoAnswerWithoutItsAuditRow(t *testing.T) {
	controltest.Each(t, func(t *testing.T, e controltest.Engine) {
		f := newFixtureOn(t, e, nil)
		tok := f.token(nil)
		release := holdAudit(t, f.loc)
		var held []response
		for _, c := range []struct {
			name, token string
		}{{"read", tok}, {"refusal", ""}} {
			t.Run(c.name, func(t *testing.T) {
				r := f.do("GET", "/v1/whoami", c.token, nil)
				held = append(held, r)
				if r.status != http.StatusServiceUnavailable || r.class() != "state-io" || r.doc["format"] != "writ.whoami/v0" || r.doc["ok"] != false {
					t.Fatalf("whoami while the audit table is locked: %d %s", r.status, r.body)
				}
				if _, ok := r.doc["principal"]; ok || r.header.Get("WWW-Authenticate") != "" {
					t.Errorf("the 503 carries the answer it replaced: %v %s", r.header, r.body)
				}
			})
		}
		release()
		for _, r := range held {
			if n := rowsOf(t, f.loc, r); n != 0 {
				t.Errorf("request %s: %d audit rows, want none", r.header.Get("X-Request-Id"), n)
			}
		}
		r := f.do("GET", "/v1/whoami", tok, nil)
		if r.status != http.StatusOK || rowsOf(t, f.loc, r) != 1 {
			t.Fatalf("whoami once the lock is gone: %d %s", r.status, r.body)
		}
	})
}

// GET /healthz and GET /v1/server write no audit row, so they answer at
// once while the control database's writes wait.
func TestHealthAndDiscoveryAreNotAudited(t *testing.T) {
	controltest.Each(t, func(t *testing.T, e controltest.Engine) {
		f := newFixtureOn(t, e, nil)
		for _, path := range []string{"/healthz", "/v1/server"} {
			t.Run(strings.TrimPrefix(path, "/"), func(t *testing.T) {
				r := f.do("GET", path, "", nil)
				if r.status != http.StatusOK {
					t.Fatalf("%s: %d %s", path, r.status, r.body)
				}
				if n := rowsOf(t, f.loc, r); n != 0 {
					t.Errorf("%s: %d audit rows, want none", path, n)
				}
				release := holdAudit(t, f.loc)
				defer release()
				start := time.Now()
				r = f.do("GET", path, "", nil)
				if took := time.Since(start); r.status != http.StatusOK || took > 2*time.Second {
					t.Errorf("%s while the audit table is locked: %d after %s: %s", path, r.status, took, r.body)
				}
			})
		}
		r := f.do("POST", "/healthz", "", nil)
		if r.status != http.StatusMethodNotAllowed || rowsOf(t, f.loc, r) != 1 {
			t.Errorf("POST /healthz: %d, want 405 and its audit row: %s", r.status, r.body)
		}
	})
}

// handle sends req to the handler writ serve listens with and returns its
// answer, which need not be JSON.
func handle(f *fixture, req *http.Request) response {
	f.t.Helper()
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	out := response{status: rec.Code, header: rec.Result().Header, body: rec.Body.Bytes()}
	_ = json.Unmarshal(out.body, &out.doc)
	return out
}

func mountUI(t *testing.T, srv *server.Server) {
	t.Helper()
	u, err := ui.New(ui.Options{Server: srv})
	if err != nil {
		t.Fatal(err)
	}
	u.Mount()
}

func cookieOf(r response, name string) *http.Cookie {
	for _, c := range (&http.Response{Header: r.header}).Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func isHTML(r response) bool { return strings.HasPrefix(r.header.Get("Content-Type"), "text/html") }

// browserSession signs the issuer's person in to the control UI and
// returns the session's cookie. The callback's answer names the sign-in's
// audit row, as every answer names its own.
func browserSession(t *testing.T, pf *peopleFixture) *http.Cookie {
	t.Helper()
	start := handle(pf.fixture, httptest.NewRequest("GET", "/ui/login/start?issuer=sso", nil))
	login := cookieOf(start, "writ_login")
	if start.status != http.StatusSeeOther || login == nil {
		t.Fatalf("the sign-in's start: %d %v", start.status, start.header)
	}
	c := pf.is.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := c.Get(start.header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	back, err := url.Parse(res.Header.Get("Location"))
	if err != nil || back.Query().Get("code") == "" {
		t.Fatalf("the issuer sent back %q", res.Header.Get("Location"))
	}
	req := httptest.NewRequest("GET", back.RequestURI(), nil)
	req.AddCookie(login)
	cb := handle(pf.fixture, req)
	sess := cookieOf(cb, "writ_session")
	if cb.status != http.StatusSeeOther || sess == nil || sess.Value == "" {
		t.Fatalf("the callback: %d %v %s", cb.status, cb.header, cb.body)
	}
	rows := controltest.Audit(t, pf.loc, controltest.AuditFilter{Request: cb.header.Get("X-Request-Id")})
	if len(rows) != 1 || rows[0].Kind != "sign-in" || rows[0].Decision != control.Allowed || rows[0].Route != "/ui/callback" {
		t.Fatalf("the callback's audit rows: %+v", rows)
	}
	return sess
}

// A browser logout whose audit transaction fails answers 503 state-io as
// a page, keeps the cookie, and leaves the session live. It does not tell
// the person they signed out.
func TestLogoutWithoutItsAuditRow(t *testing.T) {
	controltest.Each(t, func(t *testing.T, e controltest.Engine) {
		pf := newPeopleFixtureOn(t, e)
		mountUI(t, pf.srv)
		sess := browserSession(t, pf)
		logout := func() response {
			req := httptest.NewRequest("POST", "/ui/logout", nil)
			req.AddCookie(sess)
			return handle(pf.fixture, req)
		}
		release := holdAudit(t, pf.loc)
		r := logout()
		release()
		if r.status != http.StatusServiceUnavailable || !isHTML(r) || !strings.Contains(string(r.body), "<strong>state-io</strong>") {
			t.Fatalf("logout while the audit table is locked: %d %v\n%s", r.status, r.header, r.body)
		}
		if r.header.Get("Location") != "" || r.header.Get("Set-Cookie") != "" {
			t.Errorf("the 503 redirects or sets a cookie: %v", r.header)
		}
		if _, err := pf.srv.Session(context.Background(), sess.Value); err != nil {
			t.Fatalf("the session ended without its audit row: %v", err)
		}
		if n := rowsOf(t, pf.loc, r); n != 0 {
			t.Errorf("%d audit rows of the refused logout", n)
		}
		r = logout()
		if r.status != http.StatusSeeOther || !strings.Contains(r.header.Get("Location"), "signed_out=1") || rowsOf(t, pf.loc, r) != 1 {
			t.Fatalf("logout once the lock is gone: %d %v", r.status, r.header)
		}
		if _, err := pf.srv.Session(context.Background(), sess.Value); !errors.Is(err, server.ErrNoSession) {
			t.Fatalf("the session after logout: %v", err)
		}
	})
}

// A control UI answer that sets a cookie waits for its audit row: while
// the row cannot be written the theme form answers a 503 page that sets
// no cookie, and once it can, the row names the person.
func TestUICookieWaitsForItsAuditRow(t *testing.T) {
	controltest.Each(t, func(t *testing.T, e controltest.Engine) {
		pf := newPeopleFixtureOn(t, e)
		mountUI(t, pf.srv)
		sess := browserSession(t, pf)
		save := func() response {
			req := httptest.NewRequest("POST", "/ui/settings", strings.NewReader("theme=dark"))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.AddCookie(sess)
			return handle(pf.fixture, req)
		}
		release := holdAudit(t, pf.loc)
		r := save()
		release()
		if r.status != http.StatusServiceUnavailable || !isHTML(r) || !strings.Contains(string(r.body), "<strong>state-io</strong>") {
			t.Fatalf("the theme form while the audit table is locked: %d %v\n%s", r.status, r.header, r.body)
		}
		if r.header.Get("Location") != "" || r.header.Get("Set-Cookie") != "" || r.header.Get("Content-Security-Policy") != ui.CSP {
			t.Errorf("the 503's headers: %v", r.header)
		}
		if n := rowsOf(t, pf.loc, r); n != 0 {
			t.Errorf("%d audit rows of the refused form", n)
		}
		r = save()
		if c := cookieOf(r, "writ_theme"); r.status != http.StatusSeeOther || c == nil || c.Value != "dark" {
			t.Fatalf("the theme form once the lock is gone: %d %v", r.status, r.header)
		}
		rows := controltest.Audit(t, pf.loc, controltest.AuditFilter{Request: r.header.Get("X-Request-Id")})
		if len(rows) != 1 || rows[0].Route != "/ui/settings" || rows[0].Method != "POST" || rows[0].Status != http.StatusSeeOther ||
			rows[0].Person != "gitlab/user/2002" || rows[0].Via != "browser session" {
			t.Fatalf("the form's audit rows: %+v", rows)
		}
	})
}

// Every path is audited at the outermost handler, so the mux's own
// redirect of an uncleaned path and an escaped path that reaches an
// exempt handler each write a row. Only the exact GET /healthz and
// GET /v1/server write none.
func TestEveryPathIsAudited(t *testing.T) {
	controltest.Each(t, func(t *testing.T, e controltest.Engine) {
		f := newFixtureOn(t, e, nil)
		for _, c := range []struct {
			method, path string
			status, rows int
		}{
			{"GET", "//v1/whoami", http.StatusTemporaryRedirect, 1},
			{"GET", "//healthz", http.StatusTemporaryRedirect, 1},
			{"GET", "/v1/./server", http.StatusTemporaryRedirect, 1},
			{"GET", "/%68ealthz", http.StatusOK, 1},
			{"HEAD", "/healthz", http.StatusMethodNotAllowed, 1},
			{"GET", "/healthz?probe=1", http.StatusOK, 0},
			{"GET", "/healthz", http.StatusOK, 0},
			{"GET", "/v1/server", http.StatusOK, 0},
		} {
			t.Run(c.method+" "+c.path, func(t *testing.T) {
				r := handle(f, httptest.NewRequest(c.method, c.path, nil))
				if r.status != c.status {
					t.Errorf("status %d, want %d: %v %s", r.status, c.status, r.header, r.body)
				}
				if n := rowsOf(t, f.loc, r); n != c.rows {
					t.Errorf("%d audit rows, want %d", n, c.rows)
				}
			})
		}
	})
}

// panicHealth is a verifier whose health check panics.
type panicHealth struct{ server.Verifier }

func (panicHealth) Health() []oidc.Health { panic("health check bug") }

// A panic in an exempt route answers 500 in the API's format with its
// request id, as in any other route, and still writes no audit row.
func TestExemptRoutePanicIsRecovered(t *testing.T) {
	controltest.Each(t, func(t *testing.T, e controltest.Engine) {
		f := newFixtureOn(t, e, nil, func(o *server.Options) { o.Verifier = panicHealth{o.Verifier} })
		r := f.do("GET", "/healthz", "", nil)
		if r.status != http.StatusInternalServerError || r.class() != "internal" || !strings.Contains(string(r.body), r.header.Get("X-Request-Id")) {
			t.Fatalf("GET /healthz that panics: %d %v %s", r.status, r.header, r.body)
		}
		if n := rowsOf(t, f.loc, r); n != 0 {
			t.Errorf("%d audit rows of the exempt route", n)
		}
		if r := f.do("GET", "/v1/server", "", nil); r.status != http.StatusOK {
			t.Fatalf("GET /v1/server after the panic: %d %s", r.status, r.body)
		}
	})
}

// A request the mux redirects to reach the control UI, or the state
// backend, fails in that handler's format while its audit row cannot be
// written: a page for the control UI and plain text for the state
// backend, not the API's document.
func TestRedirectFailsInItsMountsFormat(t *testing.T) {
	controltest.Each(t, func(t *testing.T, e controltest.Engine) {
		f := newFixtureOn(t, e, nil)
		mountUI(t, f.srv)
		const page, plain, doc = "page", "plain", "doc"
		cases := []struct{ path, format string }{
			{"/ui", page}, {"//ui/login", page}, {"/ui/", page}, {"//", page},
			{"//v1/tfstate/network", plain}, {"//v1/whoami", doc},
		}
		for _, c := range cases {
			if r := handle(f, httptest.NewRequest("GET", c.path, nil)); r.status/100 != 3 || r.header.Get("Location") == "" {
				t.Fatalf("GET %s: %d %v, want a redirect", c.path, r.status, r.header)
			}
		}
		release := holdAudit(t, f.loc)
		held := make([]response, len(cases))
		var wg sync.WaitGroup
		for i, c := range cases {
			wg.Go(func() { held[i] = handle(f, httptest.NewRequest("GET", c.path, nil)) })
		}
		wg.Wait()
		release()
		for i, c := range cases {
			t.Run(c.path, func(t *testing.T) {
				r := held[i]
				if r.status != http.StatusServiceUnavailable || r.header.Get("Location") != "" || rowsOf(t, f.loc, r) != 0 {
					t.Fatalf("while the audit table is locked: %d %v, %d rows", r.status, r.header, rowsOf(t, f.loc, r))
				}
				switch c.format {
				case page:
					if !isHTML(r) || !strings.Contains(string(r.body), "<strong>state-io</strong>") || r.header.Get("Content-Security-Policy") != ui.CSP {
						t.Errorf("want the control UI's 503 page: %v\n%s", r.header, r.body)
					}
				case plain:
					if !strings.HasPrefix(r.header.Get("Content-Type"), "text/plain") || !strings.Contains(string(r.body), "did not record this request") {
						t.Errorf("want the state backend's plain 503: %v\n%s", r.header, r.body)
					}
				case doc:
					if r.class() != "state-io" || r.doc["format"] != "writ.error/v0" {
						t.Errorf("want the API's 503 document: %v\n%s", r.header, r.body)
					}
				}
			})
		}
	})
}

// stateToken stores a state token of an apply of network@prod and
// returns it.
func stateToken(t *testing.T, f *fixture, run string) string {
	t.Helper()
	tok := rand.Text()
	hash := sha256.Sum256([]byte(tok))
	if err := f.store.AddStateToken(context.Background(), hash[:], control.StateToken{RunID: run, Env: "prod", Stack: "network", Scope: control.StateApply,
		ExpiresAt: time.Now().Add(time.Hour)}, time.Now()); err != nil {
		t.Fatal(err)
	}
	return tok
}

// stateRequest is a request to network's state backend with tok.
func stateRequest(method, path, body, tok string) *http.Request {
	req := httptest.NewRequest(method, "/v1/tfstate/network"+path, strings.NewReader(body))
	if tok != "" {
		req.SetBasicAuth("writ", tok)
	}
	return req
}

// A state read commits its audit row before it reads the state, so a read
// that waits for its row holds none of a large state in memory, and once
// the row commits the whole state is served.
func TestStateReadHoldsNoStateWhileItsRowWaits(t *testing.T) {
	const size = 32 << 20
	controltest.Each(t, func(t *testing.T, e controltest.Engine) {
		f := newFixtureOn(t, e, nil)
		tok := stateToken(t, f, "run-large")
		state := `{"version":4,"serial":1,"lineage":"lineage-a","pad":"` + strings.Repeat("x", size) + `"}`
		for _, c := range []struct{ method, path, body string }{
			{"LOCK", "/lock", `{"ID":"lock-1"}`}, {"POST", "?ID=lock-1", state}, {"UNLOCK", "/lock", `{"ID":"lock-1"}`},
		} {
			if r := handle(f, stateRequest(c.method, c.path, c.body, tok)); r.status != http.StatusOK {
				t.Fatalf("%s: %d %s", c.method, r.status, r.body)
			}
		}
		before := liveHeap()
		release := holdAudit(t, f.loc)
		done := make(chan response, 1)
		go func() { done <- handle(f, stateRequest("GET", "", "", tok)) }()
		// The read reaches its audit row within a few milliseconds and
		// waits there for up to 5 seconds.
		time.Sleep(1500 * time.Millisecond)
		held := int64(liveHeap()) - int64(before)
		release()
		r := <-done
		if held > size/4 {
			t.Errorf("a state read waiting for its audit row holds %d bytes, want none of the %d byte state", held, size)
		}
		if r.status != http.StatusOK || string(r.body) != state || rowsOf(t, f.loc, r) != 1 {
			t.Fatalf("the read once the lock is gone: %d, %d bytes, want %d", r.status, len(r.body), len(state))
		}
	})
}

// liveHeap is the bytes of the heap's live objects.
func liveHeap() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// Every request of the state backend leaves one audit row before its
// answer: a refusal with no token, a read, a lock, a write and its retry,
// and an unlock. A write, lock, or unlock commits its row with the change
// and writes no second one, and no row holds the state. While the row
// cannot be written, a read answers 503 with no state, and a lock answers
// 503 and takes no lock.
func TestStateBackendRequestsAreAudited(t *testing.T) {
	const canary = "tfstate-canary-5d1e"
	controltest.Each(t, func(t *testing.T, e controltest.Engine) {
		f := newFixtureOn(t, e, nil)
		tok := stateToken(t, f, "run-audit")
		state := `{"version":4,"serial":1,"lineage":"lineage-a","outputs":{"secret":{"value":"` + canary + `"}}}`
		send := func(method, path, body string, auth bool) response {
			if !auth {
				return handle(f, stateRequest(method, path, body, ""))
			}
			return handle(f, stateRequest(method, path, body, tok))
		}
		for _, c := range []struct {
			name, method, path, body string
			auth                     bool
			status                   int
			kind, op                 string
			decision                 control.Decision
			reason                   string
		}{
			{"no token", "GET", "", "", false, http.StatusUnauthorized, "request", "state-read", control.AuthFailed, "this route needs a run's state token"},
			{"read nothing", "GET", "", "", true, http.StatusNotFound, "request", "state-read", control.Allowed, "no state yet"},
			{"lock", "LOCK", "/lock", `{"ID":"lock-1"}`, true, http.StatusOK, "request", "state-lock", control.Allowed, ""},
			{"write", "POST", "?ID=lock-1", state, true, http.StatusOK, "state-write", "state-write", control.Allowed, "state version 1, serial 1, lineage lineage-a, sha256 "},
			{"retry", "POST", "?ID=lock-1", state, true, http.StatusOK, "state-write", "state-write", control.Allowed, "a repeat of state version 1, "},
			{"read", "GET", "", "", true, http.StatusOK, "request", "state-read", control.Allowed, "state version 1"},
			{"unlock", "UNLOCK", "/lock", `{"ID":"lock-1"}`, true, http.StatusOK, "request", "state-unlock", control.Allowed, ""},
		} {
			t.Run(c.name, func(t *testing.T) {
				r := send(c.method, c.path, c.body, c.auth)
				if r.status != c.status {
					t.Fatalf("status %d, want %d: %s", r.status, c.status, r.body)
				}
				rows := controltest.Audit(t, f.loc, controltest.AuditFilter{Request: r.header.Get("X-Request-Id")})
				if len(rows) != 1 {
					t.Fatalf("%d audit rows, want 1: %+v", len(rows), rows)
				}
				row := rows[0]
				run, stacks := "run-audit", []string{"network"}
				if !c.auth {
					run, stacks = "", nil
				}
				if row.Kind != c.kind || row.Op != c.op || row.Method != c.method || row.Decision != c.decision || row.Status != c.status ||
					!strings.HasPrefix(row.Reason, c.reason) || row.Run != run || !slices.Equal(row.Stacks, stacks) || !strings.HasPrefix(row.Route, "/v1/tfstate/") {
					t.Fatalf("the audit row: %+v", row)
				}
			})
		}
		release := holdAudit(t, f.loc)
		var held [2]response
		var wg sync.WaitGroup
		wg.Go(func() { held[0] = send("GET", "", "", true) })
		wg.Go(func() { held[1] = send("LOCK", "/lock", `{"ID":"lock-held"}`, true) })
		wg.Wait()
		release()
		for _, r := range held {
			if r.status != http.StatusServiceUnavailable || strings.Contains(string(r.body), canary) || rowsOf(t, f.loc, r) != 0 {
				t.Errorf("a request while the audit table is locked: %d, %d rows: %s", r.status, rowsOf(t, f.loc, r), r.body)
			}
		}
		if r := send("LOCK", "/lock", `{"ID":"lock-2"}`, true); r.status != http.StatusOK {
			t.Fatalf("a lock after the refused one: %d %s", r.status, r.body)
		}
		for _, row := range controltest.Audit(t, f.loc, controltest.AuditFilter{Limit: 1000}) {
			if b, _ := json.Marshal(row); strings.Contains(string(b), canary) {
				t.Errorf("an audit row holds the state: %s", b)
			}
		}
	})
}

// Every route writ serve mounts writes an audit row for each method, ones
// it does not take included, and for the mux's redirect of its uncleaned
// path. Only GET of /healthz and of /v1/server is exempt.
func TestEveryRouteIsAudited(t *testing.T) {
	controltest.Each(t, func(t *testing.T, e controltest.Engine) {
		f := newFixtureOn(t, e, func(text string) string {
			return text + "\nlimits {\n  requests_per_address = 10000\n}\n"
		})
		mountUI(t, f.srv)
		routes := f.srv.Routes()
		// The state backend and the control UI's two mounts.
		routes["/v1/tfstate/{stack}"] = []string{"GET", "POST"}
		routes["/v1/tfstate/{stack}/lock"] = []string{"LOCK", "UNLOCK"}
		routes["/ui/{page}"] = []string{"GET", "POST"}
		routes["/{$}"] = []string{"GET"}
		wild := regexp.MustCompile(`\{[a-z]+(\.\.\.)?\}`)
		for pattern, methods := range routes {
			path := wild.ReplaceAllString(strings.ReplaceAll(pattern, "{$}", ""), "x")
			if path == "" {
				path = "/"
			}
			for _, m := range slices.Concat(methods, []string{"PATCH", "GET //"}) {
				method, target := m, path
				if m == "GET //" {
					method, target = "GET", "/"+path
				}
				want := 1
				if method == "GET" && (target == "/healthz" || target == "/v1/server") {
					want = 0
				}
				r := handle(f, httptest.NewRequest(method, target, nil))
				if n := rowsOf(t, f.loc, r); n != want {
					t.Errorf("%s %s (%s): %d %d audit rows, want %d", method, target, pattern, r.status, n, want)
				}
			}
		}
	})
}
