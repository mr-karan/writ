package server_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/control/controltest"
	"github.com/mr-karan/writ/internal/oidc/oidctest"
	"github.com/mr-karan/writ/internal/server"
)

// clock is a server clock a test moves, so a poll need not wait out the
// interval.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// lockedBuffer is a log both the server and the test touch.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type peopleFixture struct {
	*fixture
	clock *clock
	log   *lockedBuffer
	db    string
}

const clientSecret = "people-client-secret-3c9e"

// newPeopleFixture is newFixture with a people issuer, sso, whose
// acme/platform members are the team platform, and admins, who may end
// sessions. Sessions last session_ttl 1h.
func newPeopleFixture(t *testing.T) *peopleFixture {
	t.Helper()
	return newPeopleFixtureOn(t, controltest.Default())
}

// newPeopleFixtureOn is newPeopleFixture with its control database on e.
func newPeopleFixtureOn(t *testing.T, e controltest.Engine) *peopleFixture {
	t.Helper()
	pf := &peopleFixture{clock: &clock{t: time.Now()}, log: &lockedBuffer{}}
	var secretFile string
	pf.fixture = newFixtureOn(t, e, func(text string) string {
		dir := t.TempDir()
		secretFile = filepath.Join(dir, "oidc_client_secret")
		if err := os.WriteFile(secretFile, []byte(clientSecret+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		ca := text[strings.Index(text, "ca_file = \"")+len("ca_file = \""):]
		ca = ca[:strings.Index(ca, "\"")]
		url := text[strings.Index(text, "url = \"https://")+len("url = \""):]
		url = url[:strings.Index(url, "\"")]
		text += fmt.Sprintf(`
session_ttl = "1h"

issuer "sso" {
  kind = "people"
  identity = "gitlab"
  url = %q
  ca_file = %q
  client_id = "writ-client"
  client_secret_file = %q
  groups = "userinfo"
}

team "platform" {
  issuer = "sso"
  match = { groups = ["acme/platform"] }
}

team "admins" {
  issuer = "sso"
  match = { groups = ["acme/admins"] }
}
`, url, ca, secretFile)
		return strings.Replace(text, "grant {", `grant {
  teams = ["platform"]
  stacks = ["*"]
  envs = ["prod"]
  can = ["read", "plan"]
}

grant {
  teams = ["admins"]
  stacks = ["*"]
  envs = ["*"]
  can = ["admin"]
}

grant {`, 1)
	}, func(o *server.Options) {
		o.Log = slog.New(slog.NewTextHandler(pf.log, &slog.HandlerOptions{Level: slog.LevelDebug}))
		people, err := server.People(context.Background(), o.Config, o.Log, nil)
		if err != nil {
			t.Fatal(err)
		}
		o.People, o.Now = people, pf.clock.Now
	})
	pf.is.SetClient("writ-client", clientSecret)
	// writ serve polls the issuer each second, the shortest interval it
	// keeps, so a sign-in finishes within a second of its approval.
	pf.is.SetDevice(time.Second, 5*time.Minute)
	pf.is.SetPerson(oidctest.GitLabPerson())
	pf.db = pf.loc
	return pf
}

func jsonBody(s string) *strings.Reader { return strings.NewReader(s) }

// start begins a sign-in and returns the handle and the user code.
func (pf *peopleFixture) start() (handle, userCode string) {
	pf.t.Helper()
	r := pf.do("POST", "/v1/auth/device", "", jsonBody(`{"issuer":""}`))
	if r.status != http.StatusOK || r.doc["format"] != server.FormatDevice {
		pf.t.Fatalf("start: %d %s", r.status, r.body)
	}
	return r.doc["handle"].(string), r.doc["user_code"].(string)
}

func (pf *peopleFixture) poll(handle string) response {
	pf.t.Helper()
	return pf.do("POST", "/v1/auth/device/token", "", jsonBody(`{"handle":"`+handle+`"}`))
}

// settle polls until the sign-in is no longer pending, as writ login does,
// and returns the answer that ended it. Each poll moves the server's clock
// a second, so the rate limit never refuses one.
func (pf *peopleFixture) settle(handle string) response {
	pf.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		pf.clock.Add(time.Second)
		r := pf.poll(handle)
		if r.status != http.StatusOK || r.doc["status"] != server.SessionPending {
			return r
		}
		if time.Now().After(deadline) {
			pf.t.Fatalf("the sign-in still waits after 15 seconds: %s", r.body)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// signIn runs a whole writ login and returns the session token.
func (pf *peopleFixture) signIn() string {
	pf.t.Helper()
	handle, code := pf.start()
	if !pf.is.ApproveDevice(code) {
		pf.t.Fatal("the issuer does not know the user code")
	}
	r := pf.settle(handle)
	if r.status != http.StatusOK || r.doc["status"] != server.SessionSignedIn {
		pf.t.Fatalf("poll: %d %s", r.status, r.body)
	}
	return r.doc["token"].(string)
}

// dump is every row of every table of the control database, as text.
func (pf *peopleFixture) dump() string {
	pf.t.Helper()
	raw := controltest.Raw(pf.t, pf.db)
	var b strings.Builder
	for _, table := range []string{"sessions", "audit", "configs"} {
		rows, err := raw.QueryContext(context.Background(), "SELECT * FROM "+table)
		if err != nil {
			pf.t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				pf.t.Fatal(err)
			}
			for _, v := range vals {
				if bs, ok := v.([]byte); ok {
					v = string(bs)
				}
				fmt.Fprintf(&b, "%v\t", v)
			}
			b.WriteString("\n")
		}
		rows.Close()
	}
	return b.String()
}

func TestDeviceLogin(t *testing.T) {
	pf := newPeopleFixture(t)
	var bodies []string
	start := time.Now()
	handle, code := pf.start()

	// However often the client polls, writ serve asks the issuer at most
	// once a second, the interval the issuer set.
	for range 25 {
		pf.clock.Add(time.Second)
		r := pf.poll(handle)
		if r.status != http.StatusOK || r.doc["status"] != server.SessionPending || r.doc["token"] != nil {
			t.Fatalf("a poll before the person signed in: %d %s", r.status, r.body)
		}
	}
	until(t, "two issuer polls", func() bool { return pf.is.DevicePolls() >= 2 })
	if polls, most := pf.is.DevicePolls(), int(time.Since(start)/time.Second); polls > most {
		t.Fatalf("25 client polls in %s made %d issuer polls, want at most %d", time.Since(start), polls, most)
	}
	pf.is.ApproveDevice(code)
	r := pf.settle(handle)
	if r.status != http.StatusOK || r.doc["status"] != server.SessionSignedIn {
		t.Fatalf("poll after sign-in: %d %s", r.status, r.body)
	}
	token := r.doc["token"].(string)
	sess := r.doc["session"].(map[string]any)
	if !strings.HasPrefix(token, server.SessionPrefix) || sess["person"] != pf.personOf("2002") || sess["carrier"] != "cli" {
		t.Fatalf("session %v", r.doc)
	}
	ends, _ := time.Parse(time.RFC3339, sess["ends_at"].(string))
	if want := pf.clock.Now().Add(time.Hour); ends.Sub(want) > time.Second || want.Sub(ends) > 10*time.Second {
		t.Errorf("the session ends %s, want session_ttl after the sign-in, %s", ends, want)
	}
	if r := pf.poll(handle); r.status != http.StatusUnauthorized || r.class() != "auth" {
		t.Fatalf("a handle used twice: %d %s", r.status, r.body)
	}

	who := pf.do("GET", "/v1/whoami", token, nil)
	bodies = append(bodies, string(who.body))
	p := who.doc["principal"].(map[string]any)
	if who.status != http.StatusOK || p["kind"] != "person" || p["display"] != "bob" || p["session"] != sess["id"] {
		t.Fatalf("whoami with a session: %d %s", who.status, who.body)
	}
	if teams := who.doc["teams"].([]any); len(teams) != 1 || teams[0] != "platform" {
		t.Fatalf("teams %v", teams)
	}
	// The session may plan: the request passes the grant and is refused
	// only because a person's plan names its repository and commit.
	plan := pf.do("POST", "/v1/plans", token, planRequest(nil))
	bodies = append(bodies, string(plan.body))
	if plan.status != http.StatusBadRequest || !strings.Contains(string(plan.body), "names the repository and the commit's full id") {
		t.Fatalf("a plan with a session: %d %s", plan.status, plan.body)
	}

	// Teams are fixed for the session's life: leaving the group at the
	// identity provider changes nothing until the session ends.
	claims, info := oidctest.GitLabPerson()
	info["groups"] = []string{"acme"}
	pf.is.SetPerson(claims, info)
	if who := pf.do("GET", "/v1/whoami", token, nil); len(who.doc["teams"].([]any)) != 1 {
		t.Fatalf("teams changed during the session: %s", who.body)
	}

	if r := pf.do("GET", "/v1/sessions", token, nil); r.status != http.StatusForbidden || r.class() != "denied" {
		t.Fatalf("a person without admin lists sessions: %d %s", r.status, r.body)
	}
	if r := pf.do("DELETE", "/v1/auth/session", pf.token(nil), nil); r.status != http.StatusBadRequest {
		t.Fatalf("a CI token ends a session: %d %s", r.status, r.body)
	}

	out := pf.do("DELETE", "/v1/auth/session", token, nil)
	bodies = append(bodies, string(out.body))
	if out.status != http.StatusOK || out.doc["status"] != server.SessionEnded {
		t.Fatalf("logout: %d %s", out.status, out.body)
	}
	if r := pf.do("GET", "/v1/whoami", token, nil); r.status != http.StatusUnauthorized || r.class() != "auth" {
		t.Fatalf("a call after logout: %d %s", r.status, r.body)
	}
	if r := pf.do("DELETE", "/v1/auth/session", token, nil); r.status != http.StatusUnauthorized {
		t.Fatalf("a second logout: %d %s", r.status, r.body)
	}

	// The token appears in the one answer that issued it and nowhere else.
	for name, text := range map[string]string{"the server's log": pf.log.String(), "the control database": pf.dump(), "another answer": strings.Join(bodies, "\n")} {
		if strings.Contains(text, token) || strings.Contains(text, strings.TrimPrefix(token, server.SessionPrefix)) {
			t.Errorf("%s holds the session token", name)
		}
		if strings.Contains(text, handle) {
			t.Errorf("%s holds the sign-in handle", name)
		}
	}
	if strings.Contains(pf.log.String(), clientSecret) || strings.Contains(pf.dump(), clientSecret) {
		t.Error("the client secret leaked")
	}
	var signIns, signOuts int
	for _, e := range pf.audit(controltest.AuditFilter{}) {
		switch {
		case e.Kind == "sign-in" && e.Via == "cli session" && e.Decision == control.Allowed:
			signIns++
		case e.Route == "/v1/auth/session" && e.Decision == control.Allowed:
			signOuts++
		}
	}
	if signIns != 1 || signOuts != 1 {
		t.Errorf("audit: %d sign-ins and %d sign-outs, want 1 and 1", signIns, signOuts)
	}
}

func TestDeviceLoginRefusals(t *testing.T) {
	t.Run("denied", func(t *testing.T) {
		pf := newPeopleFixture(t)
		handle, code := pf.start()
		pf.is.DenyDevice(code)
		if r := pf.settle(handle); r.status != http.StatusUnauthorized || !strings.Contains(string(r.body), "refused") {
			t.Fatalf("denied consent: %d %s", r.status, r.body)
		}
		if r := pf.poll(handle); r.status != http.StatusUnauthorized || !strings.Contains(string(r.body), "unknown") {
			t.Fatalf("a refused sign-in's handle: %d %s", r.status, r.body)
		}
	})
	t.Run("expired", func(t *testing.T) {
		pf := newPeopleFixture(t)
		handle, _ := pf.start()
		pf.clock.Add(6 * time.Minute)
		if r := pf.poll(handle); r.status != http.StatusUnauthorized || !strings.Contains(string(r.body), "ran out") {
			t.Fatalf("an expired code: %d %s", r.status, r.body)
		}
	})
	t.Run("unknown handle", func(t *testing.T) {
		pf := newPeopleFixture(t)
		if r := pf.poll("not-a-handle"); r.status != http.StatusUnauthorized || r.class() != "auth" {
			t.Fatalf("an unknown handle: %d %s", r.status, r.body)
		}
		if r := pf.poll(""); r.status != http.StatusUnauthorized {
			t.Fatalf("an empty handle: %d %s", r.status, r.body)
		}
	})
	t.Run("ID token for another client", func(t *testing.T) {
		pf := newPeopleFixture(t)
		pf.is.EditIDToken(func(c map[string]any) { c["aud"] = "other-client" })
		handle, code := pf.start()
		pf.is.ApproveDevice(code)
		if r := pf.settle(handle); r.status != http.StatusUnauthorized || !strings.Contains(string(r.body), "aud") {
			t.Fatalf("a token of another audience: %d %s", r.status, r.body)
		}
		if rows := pf.audit(controltest.AuditFilter{Decision: control.AuthFailed}); len(rows) == 0 || rows[0].Check != "audience" {
			t.Fatalf("audit %+v", rows)
		}
	})
	t.Run("no people issuer", func(t *testing.T) {
		f := newFixture(t, nil)
		r := f.do("POST", "/v1/auth/device", "", nil)
		if r.status != http.StatusNotFound || !strings.Contains(string(r.body), "no people issuer") {
			t.Fatalf("login with no people issuer: %d %s", r.status, r.body)
		}
		if info := f.do("GET", "/v1/server", "", nil); info.doc["login"] != false {
			t.Fatalf("server info says login is set up: %s", info.body)
		}
	})
	t.Run("a browser session is no bearer token", func(t *testing.T) {
		pf := newPeopleFixture(t)
		token := pf.signIn()
		hash := hashOf(token)
		raw := controltest.Raw(t, pf.db)
		if _, err := raw.ExecContext(context.Background(), "UPDATE sessions SET carrier = 'browser' WHERE hash = ?", hash); err != nil {
			t.Fatal(err)
		}
		if r := pf.do("GET", "/v1/whoami", token, nil); r.status != http.StatusUnauthorized {
			t.Fatalf("a browser session as a bearer token: %d %s", r.status, r.body)
		}
	})
}

// TestDeviceStartsHoldTheBound sends four sign-ins at once with one place
// left under the bound, and lets them reach the issuer together. Only one
// may take the place; the others are refused before writ serve asks the
// issuer for a code.
func TestDeviceStartsHoldTheBound(t *testing.T) {
	pf := newPeopleFixture(t)
	pf.srv.FillLogins(server.MaxLogins - 1)
	pf.is.GateDeviceStarts(4, 2*time.Second)
	before := pf.is.DeviceStarts()
	statuses := make([]int, 4)
	var wg sync.WaitGroup
	for i := range statuses {
		wg.Go(func() {
			resp, err := http.Post(pf.http.URL+"/v1/auth/device", "application/json", jsonBody(`{}`))
			if err != nil {
				t.Error(err)
				return
			}
			resp.Body.Close()
			statuses[i] = resp.StatusCode
		})
	}
	wg.Wait()
	var ok, full int
	for _, s := range statuses {
		switch s {
		case http.StatusOK:
			ok++
		case http.StatusTooManyRequests:
			full++
		}
	}
	if ok != 1 || full != 3 {
		t.Errorf("four starts with one place left: %v, want one 200 and three 429", statuses)
	}
	if got := pf.srv.Logins(); got != server.MaxLogins {
		t.Errorf("%d sign-ins wait, over the bound of %d", got, server.MaxLogins)
	}
	if got := pf.is.DeviceStarts() - before; got != 1 {
		t.Errorf("the issuer was asked for %d codes, want 1", got)
	}
}

// TestDeviceStartFailureFreesItsPlace checks that a start the issuer
// refuses gives its place back.
func TestDeviceStartFailureFreesItsPlace(t *testing.T) {
	pf := newPeopleFixture(t)
	pf.srv.FillLogins(server.MaxLogins - 1)
	pf.is.SetClient("writ-client", "rotated")
	if r := pf.do("POST", "/v1/auth/device", "", jsonBody(`{}`)); r.status != http.StatusBadGateway {
		t.Fatalf("a start the issuer refuses: %d %s", r.status, r.body)
	}
	if got := pf.srv.Logins(); got != server.MaxLogins-1 {
		t.Fatalf("%d sign-ins wait after a failed start, want %d", got, server.MaxLogins-1)
	}
	pf.is.SetClient("writ-client", clientSecret)
	pf.start()
}

func TestSessionsByAdmin(t *testing.T) {
	pf := newPeopleFixture(t)
	bob := pf.signIn()
	claims, info := oidctest.GitLabPerson()
	claims["sub"], claims["preferred_username"], info["sub"], info["preferred_username"] = "3003", "carol", "3003", "carol"
	info["groups"] = []string{"acme/admins"}
	pf.is.SetPerson(claims, info)
	carol := pf.signIn()

	r := pf.do("GET", "/v1/sessions", carol, nil)
	if r.status != http.StatusOK || r.doc["format"] != server.FormatSessions {
		t.Fatalf("list: %d %s", r.status, r.body)
	}
	list := r.doc["sessions"].([]any)
	if len(list) != 2 {
		t.Fatalf("sessions %s", r.body)
	}
	if strings.Contains(string(r.body), bob) || strings.Contains(string(r.body), carol) {
		t.Fatal("GET /v1/sessions holds a token")
	}
	var bobID string
	for _, s := range list {
		if s := s.(map[string]any); s["display"] == "bob" {
			bobID = s["id"].(string)
		}
	}
	if r := pf.do("GET", "/v1/sessions?person="+pf.personOf("2002"), carol, nil); len(r.doc["sessions"].([]any)) != 1 {
		t.Fatalf("sessions of one person: %s", r.body)
	}
	if r := pf.do("DELETE", "/v1/sessions/"+bobID, bob, nil); r.status != http.StatusForbidden {
		t.Fatalf("bob ends his own session through the admin route: %d %s", r.status, r.body)
	}
	if r := pf.do("DELETE", "/v1/sessions/"+bobID, carol, nil); r.status != http.StatusOK {
		t.Fatalf("an admin ends a session: %d %s", r.status, r.body)
	}
	if r := pf.do("GET", "/v1/whoami", bob, nil); r.status != http.StatusUnauthorized {
		t.Fatalf("an ended session still works: %d %s", r.status, r.body)
	}
	if r := pf.do("DELETE", "/v1/sessions/"+bobID, carol, nil); r.status != http.StatusNotFound {
		t.Fatalf("a session ended twice: %d %s", r.status, r.body)
	}
	pf.clock.Add(time.Hour)
	if r := pf.do("GET", "/v1/whoami", carol, nil); r.status != http.StatusUnauthorized {
		t.Fatalf("a session past session_ttl: %d %s", r.status, r.body)
	}
}

func hashOf(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// personOf is the person id of a subject of the test issuer, whose
// identity is gitlab.
func (pf *peopleFixture) personOf(sub string) string {
	return "gitlab/user/" + sub
}

func until(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(time.Minute); !cond(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}
