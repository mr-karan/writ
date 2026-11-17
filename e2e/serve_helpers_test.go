//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/oidc/oidctest"
)

const (
	// childWritEnv names the writ a test's child process runs in place of
	// building its own.
	childWritEnv = "WRIT_E2E_CHILD_WRIT"
	// servedClientSecret is the people issuer's client secret of a team's
	// writ serve, which no output may hold.
	servedClientSecret = "oidc-client-secret-e2e-5d0b"
	// forgeToken is the read token of the forge stand-in.
	forgeToken = "glpat-E2E-forge-read-token-0123"
)

// served is a running writ serve with a test issuer in front of it.
type served struct {
	t      *testing.T
	is     *oidctest.Issuer
	url    string
	db     string
	data   string
	log    *syncBuffer
	stack  *stackDir
	outs   []string // every client stdout and API body, for the secret check
	outsMu sync.Mutex
}

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

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// token is a GitLab merge request job's token for writ serve.
func (s *served) token(edits map[string]any) string {
	c := oidctest.GitLabJob(s.is.URL, s.url, time.Now())
	for k, v := range edits {
		c[k] = v
	}
	return oidctest.Sign(s.is.Keys()[0], c)
}

// writ runs the client in the stack directory with a fresh token.
func (s *served) writ(want int, token string, args ...string) result {
	s.t.Helper()
	r := s.stack.exec([]string{"WRIT_SERVER=" + s.url, "WRIT_ID_TOKEN=" + token}, args...)
	s.outsMu.Lock()
	s.outs = append(s.outs, r.stdout, r.stderr)
	s.outsMu.Unlock()
	if r.code != want {
		s.t.Fatalf("want exit %d\n%s\nserve log:\n%s", want, r, s.log.String())
	}
	return r
}

// api sends one request and returns the status and the decoded document.
func (s *served) api(method, path, token string, body []byte) (int, map[string]any) {
	s.t.Helper()
	req, err := http.NewRequest(method, s.url+path, bytes.NewReader(body))
	if err != nil {
		s.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	s.outsMu.Lock()
	s.outs = append(s.outs, string(b))
	s.outsMu.Unlock()
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		s.t.Fatalf("%s %s: not JSON: %s", method, path, b)
	}
	return resp.StatusCode, doc
}

// signIn runs writ login against s with env, types the code it prints at
// the test issuer, and returns what it printed.
func signIn(t *testing.T, s *served, env []string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, writBin, "login", s.url)
	cmd.Dir = s.stack.dir
	cmd.Env = append(append(os.Environ(), s.stack.env...), env...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	codeRE := regexp.MustCompile(`enter the code (\S+)\.`)
	var out strings.Builder
	var code string
	rd := bufio.NewReader(stdout)
	for {
		line, err := rd.ReadString('\n')
		out.WriteString(line)
		if m := codeRE.FindStringSubmatch(line); m != nil {
			code = m[1]
		}
		if strings.HasPrefix(line, "The code expires") && !s.is.ApproveDevice(code) {
			t.Fatalf("the issuer does not know the code %q", code)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("writ login: %v\n%s%s\nserve log:\n%s", err, out.String(), stderr.String(), s.log.String())
	}
	return out.String() + stderr.String()
}

// browserSignIn runs writ login against s with env, follows the URL it
// prints as a browser does, through the test issuer, which signs bob in at
// once, to writ login's loopback listener, and returns what writ login
// printed.
func browserSignIn(t *testing.T, s *served, env []string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, writBin, "login", s.url)
	cmd.Dir = s.stack.dir
	cmd.Env = append(append(os.Environ(), s.stack.env...), env...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	authURL := regexp.MustCompile(`To sign in, open (\S+) in a browser on this computer\.`)
	var out strings.Builder
	var page string
	rd := bufio.NewReader(stdout)
	for {
		line, err := rd.ReadString('\n')
		out.WriteString(line)
		if m := authURL.FindStringSubmatch(line); m != nil {
			resp, err := s.is.Client().Get(m[1])
			if err != nil {
				t.Fatalf("the browser: %v\n%s", err, out.String())
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			page = string(b)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("writ login: %v\n%s%s\nserve log:\n%s", err, out.String(), stderr.String(), s.log.String())
	}
	if !strings.Contains(page, "Writ has the sign-in") {
		t.Fatalf("writ login's listener answered the browser with %q", page)
	}
	s.outsMu.Lock()
	s.outs = append(s.outs, out.String(), stderr.String(), page)
	s.outsMu.Unlock()
	return out.String() + stderr.String()
}

// sessionTokenIn is the one session token writ login keeps under the
// config home.
func sessionTokenIn(t *testing.T, config string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(config, "writ", "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`"token": "(writs_[^"]+)"`).FindSubmatch(b)
	if m == nil {
		t.Fatal("no session token in sessions.json")
	}
	return string(m[1])
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Customer A", "GIT_AUTHOR_EMAIL=user@example.com", "GIT_COMMITTER_NAME=Customer A",
		"GIT_COMMITTER_EMAIL=user@example.com", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

type devOutput struct {
	mu     sync.Mutex
	buffer bytes.Buffer
	ready  chan struct{}
	once   sync.Once
}

// devServer is a writ serve --dev the test started, with its data under
// home/writ/dev, where the client finds it.
type devServer struct {
	home    string
	url     string
	cmd     *exec.Cmd
	out     devOutput
	exited  chan struct{}
	exitErr error
	done    chan error
}

// startDevAt starts writ serve --dev with its data under home, which an
// earlier one may have used, and env added to its environment.
func startDevAt(t *testing.T, home string, env []string, more ...string) *devServer {
	t.Helper()
	d := &devServer{home: home, done: make(chan error, 1), exited: make(chan struct{}), out: devOutput{ready: make(chan struct{})}}
	d.cmd = exec.Command(writBin, append([]string{"serve", "--dev", "--listen", "127.0.0.1:0"}, more...)...)
	d.cmd.Env = append(append(os.Environ(), "XDG_DATA_HOME="+d.home, "WRIT_CACHE_DIR="+cacheDir), env...)
	// Providers of an apply run in the server's working directory, where
	// a local.File with a relative name lands.
	d.cmd.Dir = d.home
	d.cmd.Stdout, d.cmd.Stderr = &d.out, &d.out
	if err := d.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { d.exitErr = d.cmd.Wait(); close(d.exited); d.done <- d.exitErr }()
	t.Cleanup(func() { d.stop(t) })
	client := filepath.Join(d.home, "writ", "dev", "client.json")

	select {
	case <-d.out.ready:
	case <-d.exited:
		t.Fatalf("writ serve --dev exited before readiness: %v\n%s", d.exitErr, d.out.String())
	case <-time.After(2 * time.Minute):
		_ = d.cmd.Process.Signal(syscall.SIGQUIT)
		select {
		case <-d.exited:
		case <-time.After(10 * time.Second):
			_ = d.cmd.Process.Kill()
			<-d.exited
		}
		t.Fatalf("writ serve --dev stalled before readiness: %v\n%s", d.exitErr, d.out.String())
	}
	var c struct{ URL string }
	b, err := os.ReadFile(client)
	if err != nil || json.Unmarshal(b, &c) != nil || !answersDev(c.URL) {
		t.Fatalf("ready server has no working client address: %v\n%s", err, d.out.String())
	}
	d.url = c.URL
	return d
}

func answersDev(url string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url+"/v1/server", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var info struct{ Dev bool }
	return json.NewDecoder(resp.Body).Decode(&info) == nil && info.Dev
}

// stop interrupts the server and waits for it to exit.
func (d *devServer) stop(t *testing.T) {
	t.Helper()
	select {
	case <-d.exited:
		return
	default:
	}
	_ = d.cmd.Process.Signal(syscall.SIGINT)
	select {
	case <-d.exited:
	case <-time.After(40 * time.Second):
		_ = d.cmd.Process.Kill()
		<-d.exited
		t.Errorf("writ serve --dev did not stop on SIGINT:\n%s", d.out.String())
	}
}

func (b *devOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, err := b.buffer.Write(p)
	if strings.Contains(b.buffer.String(), "ready in ") {
		b.once.Do(func() { close(b.ready) })
	}
	return n, err
}

func (b *devOutput) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.buffer.String() }
