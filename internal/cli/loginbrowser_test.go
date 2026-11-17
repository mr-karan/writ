package cli_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/cli"
	"github.com/mr-karan/writ/internal/oidc/oidctest"
	"github.com/mr-karan/writ/internal/server"
)

// gitlabCom is an issuer like GitLab.com: no device flow, and the
// loopback redirect URI registered at the application.
func gitlabCom(is *oidctest.Issuer) {
	is.SetNoDevice(true)
	is.RegisterRedirects("http://127.0.0.1/callback")
}

var authURLRE = regexp.MustCompile(`To sign in, open (\S+) in a browser on this computer\.`)

// waitFor waits until out matches re, or login ends first.
func waitFor(t *testing.T, out *syncBuffer, re *regexp.Regexp, done <-chan error) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if m := re.FindStringSubmatch(out.String()); m != nil {
			return m
		}
		select {
		case err := <-done:
			t.Fatalf("login stopped first: %v\n%s", err, out)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %s in:\n%s", re, out)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// browse stands in for the person's browser: it opens authURL at the
// issuer, which signs the person in at once, and follows the redirect to
// the loopback listener. It returns the loopback URL and what the listener
// answered.
func (tm *team) browse(t *testing.T, authURL string) (*url.URL, int, string) {
	t.Helper()
	var last *url.URL
	c := tm.is.Client()
	c.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		last = req.URL
		return nil
	}
	resp, err := c.Get(authURL)
	if err != nil {
		t.Fatalf("the browser: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return last, resp.StatusCode, string(b)
}

func startLogin(l *laptop, dir string, o cli.LoginOptions) (*syncBuffer, <-chan error) {
	out := &syncBuffer{}
	done := make(chan error, 1)
	go func() { done <- l.app(dir, out).Login(context.Background(), o) }()
	return out, done
}

func TestBrowserLogin(t *testing.T) {
	tm := startTeam(t, gitlabCom)
	l := newLaptop(t)
	dir := t.TempDir()
	out, done := startLogin(l, dir, cli.LoginOptions{URL: tm.url})
	authURL := waitFor(t, out, authURLRE, done)[1]
	if !strings.HasPrefix(authURL, tm.is.URL+"/oauth/authorize?") {
		t.Fatalf("auth URL %s", authURL)
	}
	back, status, page := tm.browse(t, authURL)
	if back == nil || back.Scheme != "http" || back.Hostname() != "127.0.0.1" || back.Path != "/callback" || status != http.StatusOK || !strings.Contains(page, "Writ has the sign-in") {
		t.Fatalf("the loopback listener at %v answered %d %q", back, status, page)
	}
	if err := <-done; err != nil {
		t.Fatalf("login: %v\n%s", err, out)
	}
	for _, want := range []string{"Signing in to " + tm.url + " through issuer sso.", "The sign-in expires in 10 minutes.", "Signed in to " + tm.url + " as bob (gitlab/user/2002).", "Teams: platform"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("login output lacks %q:\n%s", want, out)
		}
	}
	token := sessionToken(t, l)
	if strings.Contains(out.String(), token) || strings.Contains(page, token) {
		t.Fatal("login showed the session token")
	}
	// The listener served one request and closed.
	if resp, err := http.Get(back.String()); err == nil {
		resp.Body.Close()
		t.Fatalf("the loopback listener still answers after the sign-in: %d", resp.StatusCode)
	}
	who := &syncBuffer{}
	if err := l.app(dir, who).Whoami(context.Background(), false); err != nil || !strings.Contains(who.String(), "session of bob (gitlab/user/2002, issuer sso)") {
		t.Fatalf("whoami after a browser sign-in: %v\n%s", err, who)
	}
}

func TestBrowserLoginJSON(t *testing.T) {
	tm := startTeam(t, gitlabCom)
	l := newLaptop(t)
	stdout, stderr := &syncBuffer{}, &syncBuffer{}
	done := make(chan error, 1)
	app := cli.New(cli.Options{Dir: t.TempDir(), Stdout: stdout, Stderr: stderr, LookupEnv: func(name string) (string, bool) {
		if name == "XDG_CONFIG_HOME" {
			return l.config, true
		}
		return "", false
	}})
	go func() { done <- app.Login(context.Background(), cli.LoginOptions{URL: tm.url, JSON: true}) }()
	tm.browse(t, waitFor(t, stderr, authURLRE, done)[1])
	if err := <-done; err != nil {
		t.Fatalf("login --json: %v\n%s%s", err, stdout, stderr)
	}
	var rep struct {
		Format  string              `json:"format"`
		OK      bool                `json:"ok"`
		Session *server.SessionInfo `json:"session"`
	}
	if err := json.Unmarshal([]byte(stdout.String()), &rep); err != nil || rep.Format != "writ.login/v0" || !rep.OK || rep.Session == nil || rep.Session.Carrier != "cli" {
		t.Fatalf("login --json printed %s", stdout)
	}
	if token := sessionToken(t, l); strings.Contains(stdout.String()+stderr.String(), token) {
		t.Fatal("login --json printed the session token")
	}
}

func TestLoginFlowFlags(t *testing.T) {
	t.Run("--browser with a device flow on offer", func(t *testing.T) {
		tm := startTeam(t)
		out, done := startLogin(newLaptop(t), t.TempDir(), cli.LoginOptions{URL: tm.url, Browser: true})
		tm.browse(t, waitFor(t, out, authURLRE, done)[1])
		if err := <-done; err != nil || strings.Contains(out.String(), "enter the code") {
			t.Fatalf("login --browser: %v\n%s", err, out)
		}
	})
	t.Run("--device with no device flow", func(t *testing.T) {
		tm := startTeam(t, gitlabCom)
		out := &syncBuffer{}
		err := newLaptop(t).app(t.TempDir(), out).Login(context.Background(), cli.LoginOptions{URL: tm.url, Device: true})
		if !errors.Is(err, cli.ErrFailed) || !strings.Contains(out.String(), "run writ login without --device to sign in through a browser") {
			t.Fatalf("login --device: %v\n%s", err, out)
		}
	})
}

func TestBrowserLoginRefusals(t *testing.T) {
	noSession := func(t *testing.T, l *laptop) {
		t.Helper()
		if _, err := os.Stat(filepath.Join(l.config, "writ", cli.SessionsFile)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a failed sign-in wrote a session file: %v", err)
		}
	}
	t.Run("refused consent", func(t *testing.T) {
		tm := startTeam(t, gitlabCom)
		tm.is.SetPerson(nil, nil)
		l := newLaptop(t)
		out, done := startLogin(l, t.TempDir(), cli.LoginOptions{URL: tm.url})
		_, status, page := tm.browse(t, waitFor(t, out, authURLRE, done)[1])
		if status != http.StatusBadRequest || !strings.Contains(page, "refused the sign-in: authorization endpoint: access_denied") {
			t.Fatalf("the listener answered a refusal with %d %q", status, page)
		}
		if err := <-done; err == nil || !strings.Contains(err.Error(), "the identity provider refused the sign-in") {
			t.Fatalf("login after a refusal: %v", err)
		}
		noSession(t, l)
	})
	t.Run("state mismatch", func(t *testing.T) {
		tm := startTeam(t, gitlabCom)
		l := newLaptop(t)
		out, done := startLogin(l, t.TempDir(), cli.LoginOptions{URL: tm.url})
		authURL, _ := url.Parse(waitFor(t, out, authURLRE, done)[1])
		forged := authURL.Query().Get("redirect_uri") + "?code=a-code-of-the-attacker&state=a-state-another-page-made"
		resp, err := http.Get(forged)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if err := <-done; err == nil || !strings.Contains(err.Error(), "state is not the one this sign-in made") {
			t.Fatalf("login after a forged callback: %v", err)
		}
		noSession(t, l)
		// The forged request was the one request: the real one finds no
		// listener.
		c := tm.is.Client()
		if resp, err := c.Get(authURL.String()); err == nil {
			resp.Body.Close()
			t.Fatalf("the listener answered after it served a request: %d", resp.StatusCode)
		}
	})
	t.Run("callback after the timeout", func(t *testing.T) {
		// A control plane whose sign-in ends in a second.
		redirects := make(chan string, 1)
		fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/v1/server":
				json.NewEncoder(w).Encode(server.ServerInfo{Format: server.FormatServer, OK: true, Login: true, DeviceIssuers: []string{},
					Issuers: []server.IssuerInfo{{Name: "sso", Kind: "people", URL: "https://idp.example.com"}}})
			case "/v1/auth/browser":
				var req server.BrowserRequest
				json.NewDecoder(r.Body).Decode(&req)
				redirects <- req.RedirectURI
				json.NewEncoder(w).Encode(server.Browser{Format: server.FormatBrowser, OK: true, Issuer: "sso", AuthURL: "https://idp.example.com/oauth/authorize",
					Handle: "h", ExpiresAt: time.Now().Add(time.Second).UTC().Format(time.RFC3339)})
			default:
				t.Errorf("an unexpected %s %s", r.Method, r.URL.Path)
				http.NotFound(w, r)
			}
		}))
		defer fake.Close()
		l := newLaptop(t)
		start := time.Now()
		err := l.app(t.TempDir(), &syncBuffer{}).Login(context.Background(), cli.LoginOptions{URL: fake.URL})
		if err == nil || !strings.Contains(err.Error(), "did not come back with the sign-in in time") || time.Since(start) > 5*time.Second {
			t.Fatalf("login past its end: %v after %s", err, time.Since(start))
		}
		if resp, err := http.Get(<-redirects + "?code=late&state=late"); err == nil {
			resp.Body.Close()
			t.Fatalf("the listener took a callback after the timeout: %d", resp.StatusCode)
		}
		noSession(t, l)
	})
	t.Run("a sign-in URL that is not HTTPS", func(t *testing.T) {
		fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/server" {
				json.NewEncoder(w).Encode(server.ServerInfo{Format: server.FormatServer, OK: true, Login: true, DeviceIssuers: []string{},
					Issuers: []server.IssuerInfo{{Name: "sso", Kind: "people"}}})
				return
			}
			json.NewEncoder(w).Encode(server.Browser{Format: server.FormatBrowser, OK: true, AuthURL: "file:///etc/passwd", Handle: "h"})
		}))
		defer fake.Close()
		err := newLaptop(t).app(t.TempDir(), &syncBuffer{}).Login(context.Background(), cli.LoginOptions{URL: fake.URL})
		if err == nil || !strings.Contains(fmt.Sprint(err), "not https://") {
			t.Fatalf("login with a file URL: %v", err)
		}
	})
}

// oldServer stands in for a writ serve from before writ login in a
// browser. GET /v1/server sends login and a people issuer but no
// device_issuers, the device flow signs bob in at once, and any other
// route answers 404 writ.error/v0, as its catch-all route did. It records
// each route it is asked.
func oldServer(t *testing.T) (string, *syncBuffer) {
	t.Helper()
	asked := &syncBuffer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(asked, "%s %s\n", r.Method, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/server":
			io.WriteString(w, `{"format":"writ.server/v0","ok":true,"version":"0.0.0","url":"","dev":false,"issuers":[{"name":"sso","kind":"people","url":"https://gitlab.example.com","identity":"gitlab"}],"login":true,"diagnostics":[]}`)
		case "POST /v1/auth/device":
			io.WriteString(w, `{"format":"writ.device/v0","ok":true,"issuer":"sso","user_code":"WDJB-MJHT","verification_uri":"https://gitlab.example.com/oauth/device","handle":"h","interval_seconds":1,"expires_at":"2099-01-01T00:00:00Z","diagnostics":[]}`)
		case "POST /v1/auth/device/token":
			io.WriteString(w, `{"format":"writ.session/v0","ok":true,"status":"signed-in","token":"writs_old","session":{"id":"S1","carrier":"cli","person":"gitlab/user/2002","display":"bob","issuer":"sso","subject":"2002","teams":[],"ends_at":"2099-01-01T00:00:00Z"},"diagnostics":[]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, `{"format":"writ.error/v0","ok":false,"diagnostics":[{"severity":"error","class":"usage","message":"no route %s %s"}]}`, r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, asked
}

// A writ serve that sends no device_issuers predates writ login in a
// browser, so writ login keeps the device flow with it, and --browser says
// the server is too old.
func TestLoginAgainstAnOldServer(t *testing.T) {
	url, asked := oldServer(t)
	out := &syncBuffer{}
	if err := newLaptop(t).app(t.TempDir(), out).Login(context.Background(), cli.LoginOptions{URL: url}); err != nil {
		t.Fatalf("login: %v\n%s\nasked:\n%s", err, out, asked)
	}
	if !strings.Contains(out.String(), "enter the code WDJB-MJHT") || strings.Contains(asked.String(), "/v1/auth/browser") {
		t.Fatalf("login did not use the device flow:\n%s\nasked:\n%s", out, asked)
	}

	out = &syncBuffer{}
	err := newLaptop(t).app(t.TempDir(), out).Login(context.Background(), cli.LoginOptions{URL: url, Browser: true})
	if err == nil || !strings.Contains(err.Error(), "too old for writ login in a browser") || !strings.Contains(err.Error(), "writ login --device") {
		t.Fatalf("login --browser: %v\n%s", err, out)
	}
}
