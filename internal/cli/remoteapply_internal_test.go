package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/server"
)

// standIn is a control plane for the client's side of an apply request.
// Its answers to POST /v1/runs are scripted, one for each request. Each
// answer of the script but "202" may still start the run afterwards, as a
// late commit does. "503-closed" and "409-closed" close the key with no
// run, as writ serve does, and every later request with a closed key gets
// 409 with key_closed, outside the script. The outcome route answers from
// outcomes, one for each request, and then from the last.
type standIn struct {
	*httptest.Server
	mu       sync.Mutex
	runs     []string
	outcomes []string
	starts   bool
	keys     []string
	asked    []string
	started  bool
	closed   map[string]bool
}

const standInRun = "LLGTGDJCKQ6LZQJWNBMA2ESMHV"

func newStandIn(t *testing.T, runs, outcomes []string, starts bool) *standIn {
	t.Helper()
	s := &standIn{runs: runs, outcomes: outcomes, starts: starts, closed: map[string]bool{}}
	info := &server.RunInfo{ID: standInRun, Plan: "d3f700d860a97483a7ad509c31fe9e28", Status: "applying", Env: "prod", Stack: "network",
		Actor: server.Principal{Display: "alice"}, ApprovedBy: []server.ApprovalInfo{}, NamedBy: "plan", StartedAt: "2026-11-11T10:00:00Z"}
	reply := func(w http.ResponseWriter, status int, doc any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(doc)
	}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/runs":
			key := r.Header.Get(server.ApplyKeyHeader)
			s.keys = append(s.keys, key)
			closed := map[string]any{"format": server.FormatRemoteRun, "ok": false, "key_closed": true,
				"diagnostics": []server.Diagnostic{{Severity: "error", Class: "state", Message: "the key " + key + " is closed with no run"}}}
			if s.closed[key] {
				reply(w, http.StatusConflict, closed)
				return
			}
			answer := s.runs[0]
			s.runs = s.runs[1:]
			switch answer {
			case "409-closed":
				s.closed[key] = true
				reply(w, http.StatusConflict, closed)
				return
			case "503-closed":
				s.closed[key] = true
				closed["outcome"] = server.OutcomeNone
				reply(w, http.StatusServiceUnavailable, closed)
				return
			case "202":
				s.started = true
				reply(w, http.StatusAccepted, server.RemoteRun{Format: server.FormatRemoteRun, OK: true, Run: info, Document: json.RawMessage("null"), Diagnostics: []server.Diagnostic{}})
				return
			case "drop":
				// The request's transaction commits after the connection
				// is gone, as a late commit does.
				s.started = s.started || s.starts
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					conn.Close()
				}
				return
			case "503-proxy":
				s.started = s.started || s.starts
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte("<html>bad gateway</html>"))
				return
			}
			outcome := strings.TrimPrefix(answer, "503-")
			s.started = s.started || s.starts && outcome == server.OutcomeUnknown
			reply(w, http.StatusServiceUnavailable, map[string]any{"format": server.FormatRemoteRun, "ok": false, "outcome": outcome,
				"diagnostics": []server.Diagnostic{{Severity: "error", Class: "state-io", Message: "the control database failed"}}})
		case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/v1/apply-requests/"):
			s.asked = append(s.asked, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/apply-requests/"), "/outcome"))
			answer := s.outcomes[0]
			if len(s.outcomes) > 1 {
				s.outcomes = s.outcomes[1:]
			}
			switch answer {
			case "503":
				reply(w, http.StatusServiceUnavailable, map[string]any{"format": server.FormatOutcome, "ok": false, "outcome": "unknown", "diagnostics": []any{}})
			case "404":
				reply(w, http.StatusNotFound, map[string]any{"format": server.FormatError, "ok": false,
					"diagnostics": []server.Diagnostic{{Severity: "error", Class: "usage", Message: "no route POST " + r.URL.Path}}})
			default:
				doc := server.ApplyOutcome{Format: server.FormatOutcome, OK: true, Key: s.asked[len(s.asked)-1], Outcome: server.OutcomeNone, Diagnostics: []server.Diagnostic{}}
				if s.started {
					doc.Outcome, doc.Run = server.OutcomeStarted, info
				}
				reply(w, http.StatusOK, doc)
			}
		case r.Method == "GET" && r.URL.Path == "/v1/runs/"+standInRun:
			done := *info
			done.Status, done.EndedAt = "applied", "2026-11-11T10:00:05Z"
			reply(w, http.StatusOK, server.RemoteRun{Format: server.FormatRemoteRun, OK: true, Run: &done, Document: json.RawMessage(`{"ok":true}`),
				Text: "Applied 1 change in 5ms.\n", Diagnostics: []server.Diagnostic{}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *standIn) seen() (keys, asked []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.keys...), append([]string(nil), s.asked...)
}

func standInApp(t *testing.T, url string, out *bytes.Buffer) *App {
	t.Helper()
	data := t.TempDir()
	return New(Options{Dir: t.TempDir(), Server: url, Stdout: out, LookupEnv: func(name string) (string, bool) {
		switch name {
		case "WRIT_ID_TOKEN":
			return "a-ci-token", true
		case "XDG_CONFIG_HOME", "XDG_DATA_HOME":
			return filepath.Join(data, name), true
		}
		return "", false
	}})
}

// After an answer that does not decide, writ apply --remote asks the
// outcome route with the key it sent, and follows a run that started, or
// says that none did. It sends POST /v1/runs again only after an answer
// that shows the request started no run, and with a new key only after an
// answer that says the key is closed with no run.
func TestRemoteApplyOutcome(t *testing.T) {
	for _, c := range []struct {
		name     string
		runs     []string
		outcomes []string
		starts   bool
		posts    int
		asks     int
		want     []string
		fails    bool
		// keys counts the keys the command sends: 1 when it is 0.
		keys int
	}{
		{"a late commit after a dropped connection", []string{"drop"}, []string{"503", "200"}, true, 1, 2,
			[]string{"got no answer", "Asking writ serve", "Run " + standInRun, "applied at"}, false, 0},
		{"503 with outcome unknown", []string{"503-unknown"}, []string{"200"}, true, 1, 1,
			[]string{"did not say that no run started", "Run " + standInRun, "applied at"}, false, 0},
		{"503 from a proxy", []string{"503-proxy"}, []string{"200"}, true, 1, 1, []string{"Run " + standInRun, "applied at"}, false, 0},
		{"503 with outcome none is sent again", []string{"503-none", "202"}, []string{"200"}, false, 2, 0, []string{"Run " + standInRun, "applied at"}, false, 0},
		{"a key closed with a 503 is replaced", []string{"503-closed", "202"}, []string{"200"}, false, 2, 0,
			[]string{"closed the key", "with a new key", "Run " + standInRun, "applied at"}, false, 2},
		{"a key closed with a 409 is replaced", []string{"409-closed", "202"}, []string{"200"}, false, 2, 0,
			[]string{"closed the key", "Run " + standInRun, "applied at"}, false, 2},
		{"503 with outcome unknown keeps the key", []string{"503-unknown"}, []string{"200"}, false, 1, 1,
			[]string{"error: state-io: no run started"}, true, 0},
		{"a dropped request that started nothing", []string{"drop"}, []string{"200"}, false, 1, 1,
			[]string{"error: state-io: no run started"}, true, 0},
		{"an outcome route that does not answer", []string{"drop"}, []string{"404"}, true, 1, 1,
			[]string{"no route", "error: state-io: writ serve did not say whether", "writ show --remote --request "}, true, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newStandIn(t, c.runs, c.outcomes, c.starts)
			var out bytes.Buffer
			a := standInApp(t, s.URL, &out)
			err := a.RemoteApply(context.Background(), RemoteApplyOptions{ID: "d3f700d860a97483a7ad509c31fe9e28"})
			if c.fails != errors.Is(err, ErrFailed) || !c.fails && err != nil {
				t.Fatalf("err %v\n%s", err, out.String())
			}
			for _, w := range c.want {
				if !strings.Contains(out.String(), w) {
					t.Errorf("the output lacks %q:\n%s", w, out.String())
				}
			}
			keys, asked := s.seen()
			if len(keys) != c.posts || len(asked) != c.asks {
				t.Fatalf("%d requests to apply and %d to the outcome route, want %d and %d", len(keys), len(asked), c.posts, c.asks)
			}
			distinct := map[string]bool{}
			for _, k := range keys {
				if _, err := server.ParseApplyKey(k); err != nil {
					t.Fatal(err)
				}
				distinct[k] = true
			}
			if want := max(c.keys, 1); len(distinct) != want {
				t.Fatalf("one command sent keys %v, want %d keys", keys, want)
			}
			for i, k := range keys[1:] {
				if k != keys[i] && !s.closed[keys[i]] {
					t.Fatalf("the command replaced the key %s, which is not closed: %v", keys[i], keys)
				}
			}
			for _, k := range asked {
				if k != keys[len(keys)-1] {
					t.Fatalf("one command sent keys %v and asked %v", keys, asked)
				}
			}
			if c.name == "an outcome route that does not answer" && !strings.Contains(out.String(), "writ show --remote --request "+keys[0]) {
				t.Fatalf("the output does not name the key:\n%s", out.String())
			}
		})
	}
}

// writ show --remote --request KEY prints the run a key started, or that
// none did.
func TestShowRequest(t *testing.T) {
	key := server.NewApplyKey(time.Now())
	for _, started := range []bool{true, false} {
		s := newStandIn(t, nil, []string{"200"}, false)
		s.started = started
		var out bytes.Buffer
		err := standInApp(t, s.URL, &out).Show(context.Background(), ShowOptions{Request: key})
		want := "started run " + standInRun
		if !started {
			want = "No run started for the apply request with the key " + key
		}
		if started != (err == nil) || !strings.Contains(out.String(), want) {
			t.Fatalf("started=%v: %v\n%s", started, err, out.String())
		}
	}
	var out bytes.Buffer
	if err := standInApp(t, "http://127.0.0.1:1", &out).Show(context.Background(), ShowOptions{Request: "short"}); ExitCode(err) != ExitUsage {
		t.Fatalf("a malformed key: %v", err)
	}
}
