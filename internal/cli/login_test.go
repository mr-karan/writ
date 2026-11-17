package cli_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/cli"
	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/oidc/oidctest"
	"github.com/mr-karan/writ/internal/server"
)

// team is a team's writ serve in the test's process, with a test issuer
// that people sign in with through the device flow.
type team struct {
	url string
	is  *oidctest.Issuer
}

// startTeam starts a team's writ serve, with setup run on the issuer
// before writ serve reads its discovery document.
func startTeam(t *testing.T, setup ...func(*oidctest.Issuer)) *team {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	is := oidctest.New(t)
	is.SetClient("writ-cli", "cli-client-secret")
	is.SetPerson(oidctest.GitLabPerson())
	for _, f := range setup {
		f(is)
	}
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ca := write("issuer.pem", is.CAPEM())
	secret := write("client_secret", []byte("cli-client-secret\n"))
	hs := httptest.NewUnstartedServer(nil)
	url := "http://" + hs.Listener.Addr().String()
	text := fmt.Sprintf(`url      = %q
listen   = %q
database = "postgres://writ@localhost/unused"
data     = %q

issuer "sso" {
  kind               = "people"
  identity           = "gitlab"
  url                = %q
  ca_file            = %q
  client_id          = "writ-cli"
  client_secret_file = %q
  groups             = "userinfo"
}

team "platform" {
  issuer = "sso"
  match  = { groups = ["acme/platform"] }
}

grant {
  teams  = ["platform"]
  stacks = ["*"]
  envs   = ["prod"]
  can    = ["read", "plan"]
}

env "prod" {
  key = "prod.key"
}

`, url, hs.Listener.Addr().String(), dir, is.URL, ca, secret)
	cfg, diags := server.ParseConfig(filepath.Join(dir, "serve"), []byte(text))
	if diags.HasErrors() {
		t.Fatal(diags)
	}
	store, err := control.Open(ctx, control.Options{Location: filepath.Join(dir, "control.db")})
	if err != nil {
		t.Fatal(err)
	}
	people, err := server.People(ctx, cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(ctx, server.Options{Config: cfg, Store: store, People: people, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	hs.Config.Handler = srv.Handler()
	hs.Start()
	t.Cleanup(func() {
		hs.Close()
		srv.Close()
		store.Close()
	})
	return &team{url: url, is: is}
}

// syncBuffer is a command's output that the test reads while it runs.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// laptop is a person's environment: a config home and a data home of its
// own, and the variables in env.
type laptop struct {
	config, data string
	env          map[string]string
}

func newLaptop(t *testing.T) *laptop {
	return &laptop{config: filepath.Join(t.TempDir(), "config"), data: t.TempDir(), env: map[string]string{}}
}

func (l *laptop) app(dir string, out *syncBuffer) *cli.App {
	return cli.New(cli.Options{Dir: dir, Stdout: out, Stderr: out, LookupEnv: func(name string) (string, bool) {
		switch name {
		case "XDG_CONFIG_HOME":
			return l.config, true
		case "XDG_DATA_HOME":
			return l.data, true
		}
		v, ok := l.env[name]
		return v, ok
	}})
}

var codeRE = regexp.MustCompile(`enter the code (\S+)\.`)

// login runs writ login and approves its code at the issuer, as the person
// at the browser does.
func (tm *team) login(t *testing.T, l *laptop, dir string, o cli.LoginOptions) string {
	t.Helper()
	out := &syncBuffer{}
	done := make(chan error, 1)
	go func() { done <- l.app(dir, out).Login(context.Background(), o) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if m := codeRE.FindStringSubmatch(out.String()); m != nil {
			if !tm.is.ApproveDevice(m[1]) {
				t.Fatalf("the issuer does not know the code %s", m[1])
			}
			break
		}
		select {
		case err := <-done:
			t.Fatalf("login stopped before it printed a code: %v\n%s", err, out.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("no code printed:\n%s", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := <-done; err != nil {
		t.Fatalf("login: %v\n%s", err, out.String())
	}
	return out.String()
}

func sessionToken(t *testing.T, l *laptop) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(l.config, "writ", cli.SessionsFile))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`"token": "(writs_[^"]+)"`).FindSubmatch(b)
	if m == nil {
		t.Fatalf("no token in the session file:\n%s", b)
	}
	return string(m[1])
}

func TestLoginWhoamiLogout(t *testing.T) {
	tm := startTeam(t)
	l := newLaptop(t)
	ctx := context.Background()
	dir := t.TempDir()

	got := tm.login(t, l, dir, cli.LoginOptions{URL: tm.url})
	for _, want := range []string{"Signing in to " + tm.url + " through issuer sso.", "To sign in, open " + tm.is.URL + "/oauth/device and enter the code", "The code expires in 5 minutes.", "Signed in to " + tm.url + " as bob (", "Teams: platform", "Session ends "} {
		if !strings.Contains(got, want) {
			t.Errorf("login output lacks %q:\n%s", want, got)
		}
	}
	path := filepath.Join(l.config, "writ", cli.SessionsFile)
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("session file %v, %v; want mode 0600", info, err)
	}
	if d, err := os.Stat(filepath.Dir(path)); err != nil || d.Mode().Perm() != 0o700 {
		t.Fatalf("session directory %v, %v; want mode 0700", d, err)
	}
	token := sessionToken(t, l)
	outs := []string{got}

	// With no flag, no variable, and no writ.estate, the one session names
	// the control plane.
	out := &syncBuffer{}
	if err := l.app(dir, out).Whoami(ctx, false); err != nil {
		t.Fatalf("whoami: %v\n%s", err, out)
	}
	if want := tm.url + " (from your session), session of bob ("; !strings.HasPrefix(out.String(), want) || !strings.Contains(out.String(), "teams: platform") {
		t.Fatalf("whoami:\n%s\nwant it to start %q", out, want)
	}
	outs = append(outs, out.String())

	out = &syncBuffer{}
	if err := l.app(dir, out).Logout(ctx, cli.LogoutOptions{}); err != nil {
		t.Fatalf("logout: %v\n%s", err, out)
	}
	if !strings.HasPrefix(out.String(), "Signed out of "+tm.url+": session ") {
		t.Fatalf("logout: %s", out)
	}
	outs = append(outs, out.String())
	if b, _ := os.ReadFile(path); strings.Contains(string(b), token) {
		t.Fatal("logout left the token in the session file")
	}
	for _, o := range outs {
		if strings.Contains(o, token) {
			t.Fatalf("a command printed the session token:\n%s", o)
		}
	}

	// The control plane refuses the token after logout.
	req, _ := http.NewRequest(http.MethodGet, tm.url+"/v1/whoami", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the token after logout: %d", resp.StatusCode)
	}
	l.env["WRIT_SERVER"] = tm.url
	out = &syncBuffer{}
	err = l.app(dir, out).Whoami(ctx, false)
	var usage *cli.UsageError
	if !errors.As(err, &usage) || !strings.Contains(err.Error(), "no token for "+tm.url+": sign in with writ login") {
		t.Fatalf("whoami after logout: %v", err)
	}
}

func TestLoginRefusals(t *testing.T) {
	ctx := context.Background()
	t.Run("denied", func(t *testing.T) {
		tm := startTeam(t)
		l := newLaptop(t)
		out := &syncBuffer{}
		done := make(chan error, 1)
		go func() { done <- l.app(t.TempDir(), out).Login(ctx, cli.LoginOptions{URL: tm.url}) }()
		for codeRE.FindStringSubmatch(out.String()) == nil {
			time.Sleep(10 * time.Millisecond)
		}
		tm.is.DenyDevice(codeRE.FindStringSubmatch(out.String())[1])
		if err := <-done; !errors.Is(err, cli.ErrFailed) || !strings.Contains(out.String(), "refused the sign-in") {
			t.Fatalf("a refused consent: %v\n%s", err, out)
		}
		if _, err := os.Stat(filepath.Join(l.config, "writ", cli.SessionsFile)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a refused sign-in wrote a session file: %v", err)
		}
	})
	t.Run("no URL", func(t *testing.T) {
		l := newLaptop(t)
		err := l.app(t.TempDir(), &syncBuffer{}).Login(ctx, cli.LoginOptions{})
		if !strings.Contains(fmt.Sprint(err), "writ login needs the control plane's URL") {
			t.Fatalf("login with no URL: %v", err)
		}
	})
	t.Run("plain HTTP", func(t *testing.T) {
		l := newLaptop(t)
		err := l.app(t.TempDir(), &syncBuffer{}).Login(ctx, cli.LoginOptions{URL: "http://writ.example.com"})
		if !strings.Contains(fmt.Sprint(err), "want https://") {
			t.Fatalf("login over plain HTTP: %v", err)
		}
	})
	t.Run("logout with no session", func(t *testing.T) {
		l := newLaptop(t)
		err := l.app(t.TempDir(), &syncBuffer{}).Logout(ctx, cli.LogoutOptions{URL: "https://writ.example.com"})
		if !strings.Contains(fmt.Sprint(err), "you hold no session for https://writ.example.com") {
			t.Fatalf("logout with no session: %v", err)
		}
	})
}
