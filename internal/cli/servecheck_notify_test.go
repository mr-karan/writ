package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mr-karan/writ/internal/notify"
)

// TestServeCheckNotify holds writ serve --check to listing each notifier
// and sending nothing, and --check --notify-test to one signed test message
// to each, failing when one does not arrive.
func TestServeCheckNotify(t *testing.T) {
	var status atomic.Int32
	got := make(chan *http.Request, 8)
	bodies := make(chan []byte, 8)
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- r
		bodies <- b
		if s := status.Load(); s != 0 {
			w.WriteHeader(int(s))
		}
	}))
	defer sink.Close()
	dir := t.TempDir()
	write := func(name, text string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	const secret = "check-signing-secret"
	config := write("serve.hcl", fmt.Sprintf(`
url      = "https://writ.example.com"
listen   = "127.0.0.1:8443"
database = "postgres://writ@127.0.0.1:1/writ?connect_timeout=1"

issuer "gitlab" {
  kind     = "gitlab-ci"
  identity = "gitlab"
  url      = "https://gitlab.example.com"
}

team "eng" {
  issuer = "gitlab"
  match  = { project_id = ["4242"] }
}

env "prod" {
  key = "prod.key"
}

grant {
  teams  = ["eng"]
  stacks = ["*"]
  envs   = ["*"]
  can    = ["read", "plan"]
}

notify "ops" {
  kind        = "webhook"
  url_file    = %q
  secret_file = %q
  events      = ["applied", "drift"]
  envs        = ["prod"]
  stacks      = ["payments/**"]
}
`, write("url", sink.URL+"/hook"), write("secret", secret)))
	run := func(o ServeOptions) (string, error) {
		var out bytes.Buffer
		o.Config = config
		err := New(Options{Stdout: &out, Stderr: &out}).Serve(context.Background(), o)
		return out.String(), err
	}

	out, err := run(ServeOptions{Check: true})
	if err != nil {
		t.Fatalf("--check: %v\n%s", err, out)
	}
	for _, want := range []string{
		"notify ops subscribes to drift, and this config has no drift block",
		"notify: delivery counts not read: ",
		"notify: ops (webhook) hears applied drift in prod for payments/**, drift on-change; 0 delivered, 0 failed\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("--check printed:\n%s\nwant %q", out, want)
		}
	}
	if len(got) != 0 {
		t.Fatal("--check sent a message")
	}

	out, err = run(ServeOptions{Check: true, NotifyTest: true})
	if err != nil || !strings.Contains(out, "notify: ops: test message sent\n") {
		t.Fatalf("--check --notify-test: %v\n%s", err, out)
	}
	r, body := <-got, <-bodies
	var ev notify.Event
	if err := json.Unmarshal(body, &ev); err != nil || ev.Event != notify.EventTest || ev.Server != "https://writ.example.com" || ev.Summary != "a test message: notify ops reaches this channel" {
		t.Errorf("the test message: %s", body)
	}
	ts, _ := strconv.ParseInt(r.Header.Get("X-Writ-Timestamp"), 10, 64)
	if !notify.Verify([]byte(secret), ts, body, r.Header.Get("X-Writ-Signature-256")) {
		t.Errorf("the test message's signature does not verify: %v", r.Header)
	}

	status.Store(http.StatusInternalServerError)
	out, err = run(ServeOptions{Check: true, NotifyTest: true})
	if !errors.Is(err, ErrFailed) || !strings.Contains(out, "notify: ops: test message FAILED: http status 500\n") {
		t.Errorf("a failing test message: %v\n%s", err, out)
	}
	if len(got) != 1 {
		t.Errorf("the failing test message was sent %d times, want once", len(got))
	}

	if _, err := run(ServeOptions{NotifyTest: true}); err == nil || !strings.Contains(err.Error(), "--notify-test goes with --check") {
		t.Errorf("--notify-test alone: %v", err)
	}
}
