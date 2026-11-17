package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// precedenceApp is an App in dir with only the variables in env, a config
// home of its own, and a fixed clock.
func precedenceApp(t *testing.T, dir, server string, env map[string]string) *App {
	t.Helper()
	config := filepath.Join(t.TempDir(), "config")
	data := t.TempDir()
	return New(Options{Dir: dir, Server: server, Now: func() time.Time { return t0 }, LookupEnv: func(name string) (string, bool) {
		switch name {
		case "XDG_CONFIG_HOME":
			return config, true
		case "XDG_DATA_HOME":
			return data, true
		}
		v, ok := env[name]
		return v, ok
	}})
}

var t0 = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

func keep(t *testing.T, a *App, sessions map[string]savedSession) {
	t.Helper()
	path, doc, err := a.readSessions()
	if err != nil {
		t.Fatal(err)
	}
	for u, s := range sessions {
		doc.Sessions[u] = s
	}
	if err := a.writeSessions(path, doc); err != nil {
		t.Fatal(err)
	}
}

func live(token string) savedSession {
	return savedSession{Token: token, ID: "S" + token, Display: "bob", EndsAt: t0.Add(time.Hour)}
}

// The server is the first of --server, WRIT_SERVER, and the one session;
// with none the dev control plane is next. A writ.estate names none.
func TestServerPrecedence(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "writ.estate"), []byte(`{ server: "https://estate.example.com" }`), 0o644); err != nil {
		t.Fatal(err)
	}
	stack := filepath.Join(root, "network")
	if err := os.MkdirAll(stack, 0o755); err != nil {
		t.Fatal(err)
	}
	plain := t.TempDir()
	tests := []struct {
		name, dir, flag string
		env             map[string]string
		sessions        map[string]savedSession
		want, from      string
		err             string
	}{
		{name: "flag", dir: stack, flag: "https://flag.example.com", env: map[string]string{"WRIT_SERVER": "https://env.example.com"}, want: "https://flag.example.com", from: "--server"},
		{name: "variable", dir: stack, env: map[string]string{"WRIT_SERVER": "https://env.example.com"}, want: "https://env.example.com", from: "WRIT_SERVER"},
		{name: "a writ.estate", dir: stack, sessions: map[string]savedSession{"https://session.example.com": live("a")}, want: "https://session.example.com", from: "your session"},
		{name: "one session", dir: plain, sessions: map[string]savedSession{"https://session.example.com": live("a")}, want: "https://session.example.com", from: "your session"},
		{name: "an ended session counts for nothing", dir: plain, sessions: map[string]savedSession{"https://old.example.com": {Token: "x", EndsAt: t0.Add(-time.Minute)}}, want: ""},
		{name: "two sessions", dir: plain, sessions: map[string]savedSession{"https://a.example.com": live("a"), "https://b.example.com": live("b")},
			err: "you hold sessions for https://a.example.com and https://b.example.com"},
		{name: "none", dir: plain, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := precedenceApp(t, tt.dir, tt.flag, tt.env)
			if tt.sessions != nil {
				keep(t, a, tt.sessions)
			}
			got, from, err := a.serverURL()
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("err %v, want %q", err, tt.err)
				}
				return
			}
			if err != nil || got != tt.want || from != tt.from {
				t.Fatalf("got %q from %q, %v; want %q from %q", got, from, err, tt.want, tt.from)
			}
		})
	}
}

// The token for a server is WRIT_ID_TOKEN, else the session for it.
func TestTokenPrecedence(t *testing.T) {
	ctx := context.Background()
	a := precedenceApp(t, t.TempDir(), "https://writ.example.com", nil)
	keep(t, a, map[string]savedSession{"https://writ.example.com": live("writs_mine")})
	r, err := a.remote(ctx)
	if err != nil || r.token != "writs_mine" || r.tokenFrom != tokenSession || r.from != "--server" {
		t.Fatalf("remote %+v, %v", r, err)
	}
	a.opts.LookupEnv = wrapEnv(a.opts.LookupEnv, "WRIT_ID_TOKEN", "ci-token")
	if r, err := a.remote(ctx); err != nil || r.token != "ci-token" || r.tokenFrom != tokenCI {
		t.Fatalf("remote with WRIT_ID_TOKEN %+v, %v", r, err)
	}

	b := precedenceApp(t, t.TempDir(), "https://writ.example.com", nil)
	keep(t, b, map[string]savedSession{"https://writ.example.com": {Token: "writs_old", EndsAt: t0.Add(time.Hour)}})
	b.opts.Now = func() time.Time { return t0.Add(2 * time.Hour) }
	if _, err := b.remote(ctx); err == nil || !strings.Contains(err.Error(), "your session for https://writ.example.com ended at") {
		t.Fatalf("an ended session: %v", err)
	}
	c := precedenceApp(t, t.TempDir(), "", nil)
	if _, err := c.remote(ctx); err == nil || !strings.Contains(err.Error(), "no control plane: start one with writ serve --dev, or sign in to your team's with writ login URL") {
		t.Fatalf("no control plane: %v", err)
	}
}

func wrapEnv(next func(string) (string, bool), name, value string) func(string) (string, bool) {
	return func(n string) (string, bool) {
		if n == name {
			return value, true
		}
		return next(n)
	}
}

// A session file others may read is refused, and one writ writes is
// readable by its user alone.
func TestSessionsFileMode(t *testing.T) {
	a := precedenceApp(t, t.TempDir(), "", nil)
	keep(t, a, map[string]savedSession{"https://writ.example.com": live("writs_x")})
	path, err := a.sessionsPath()
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, %v", info, err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.readSessions(); err == nil || !strings.Contains(err.Error(), "others may read it (mode 0644): run chmod 600") {
		t.Fatalf("a readable session file: %v", err)
	}
	// Sessions past their end are dropped when the file is written.
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	keep(t, a, map[string]savedSession{"https://old.example.com": {Token: "writs_old", EndsAt: t0.Add(-time.Second)}})
	_, doc, err := a.readSessions()
	if err != nil || len(doc.Sessions) != 1 {
		t.Fatalf("sessions %+v, %v", doc.Sessions, err)
	}
}

func TestCheckCallback(t *testing.T) {
	const host, state = "127.0.0.1:43123", "the-state-of-this-sign-in"
	for _, tc := range []struct {
		name, remote, host, target, want string
	}{
		{"good", "127.0.0.1:50000", host, "/callback?code=c0de&state=" + state, ""},
		{"from off the machine", "192.0.2.10:50000", host, "/callback?code=c0de&state=" + state, "not from this computer"},
		{"a DNS rebinding page", "127.0.0.1:50000", "evil.example.com:43123", "/callback?code=c0de&state=" + state, "named host"},
		{"another path", "127.0.0.1:50000", host, "/favicon.ico", "not GET /callback"},
		{"a wrong state", "127.0.0.1:50000", host, "/callback?code=c0de&state=another", "state is not the one"},
		{"no state", "127.0.0.1:50000", host, "/callback?code=c0de", "state is not the one"},
		{"an error", "127.0.0.1:50000", host, "/callback?error=access_denied&error_description=%1b%5b31m&state=" + state, "refused the sign-in: authorization endpoint: access_denied"},
		{"no code", "127.0.0.1:50000", host, "/callback?state=" + state, "no authorization code"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://"+tc.host+tc.target, nil)
			req.RemoteAddr = tc.remote
			code, err := checkCallback(req, host, state)
			switch {
			case tc.want == "" && (err != nil || code != "c0de"):
				t.Fatalf("got %q, %v", code, err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want) || code != ""):
				t.Fatalf("got %q, %v; want an error with %q", code, err, tc.want)
			}
		})
	}
}
