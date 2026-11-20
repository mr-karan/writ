package notify_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/notify"
)

func TestCheckURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"https hook", "https://hooks.example.com/services/a/b/c", false},
		{"https generic with query", "https://hooks.example.com/writ?channel=ops", false},
		{"http loopback ipv4", "http://127.0.0.1:8080/hook", false},
		{"http loopback ipv6", "http://[::1]:8080/hook", false},
		{"http localhost", "http://localhost:9000/hook", false},
		{"http non-loopback refused", "http://hooks.example.com/writ", true},
		{"userinfo refused", "https://user:pass@hooks.example.com/writ", true},
		{"ftp refused", "ftp://hooks.example.com/writ", true},
		{"empty refused", "", true},
		{"no host refused", "https:///path", true},
		{"fragment refused", "https://hooks.example.com/writ#frag", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := notify.CheckURL(tt.url)
			if (err != nil) != tt.wantErr {
				t.Fatalf("CheckURL(%q) err = %v, wantErr %v", tt.url, err, tt.wantErr)
			}
			if err != nil && strings.Contains(tt.url, "pass") && strings.Contains(err.Error(), "pass") {
				t.Errorf("error leaked credential: %v", err)
			}
		})
	}
}

func TestSignatureVerification(t *testing.T) {
	secret := []byte("test-signing-secret-0123456789")
	body := []byte(`{"format":"writ.notification/v0","event":"applied"}`)
	ts := int64(1763620200)

	sig := notify.Sign(secret, ts, body)
	if !strings.HasPrefix(sig, "sha256=") || len(sig) != len("sha256=")+64 {
		t.Fatalf("unexpected signature format: %q", sig)
	}
	if !notify.Verify(secret, ts, body, sig) {
		t.Fatal("valid signature failed verification")
	}
	if notify.Verify([]byte("wrong-secret"), ts, body, sig) {
		t.Error("wrong secret verified")
	}
	if notify.Verify(secret, ts+1, body, sig) {
		t.Error("wrong timestamp verified")
	}
	if notify.Verify(secret, ts, append(body, ' '), sig) {
		t.Error("modified body verified")
	}
	if notify.Verify(secret, ts, body, "sha256=not-hex") {
		t.Error("malformed signature verified")
	}
	if notify.Verify(secret, ts, body, "md5=deadbeef") {
		t.Error("wrong prefix verified")
	}
}

func TestWebhookPayloadAndSignatureHeader(t *testing.T) {
	secret := []byte("webhook-hmac-secret-value")
	var gotBody []byte
	var gotHeaders http.Header
	done := make(chan struct{}, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		done <- struct{}{}
	}))
	defer srv.Close()

	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	d := notify.New(notify.Options{
		Targets: []notify.Target{{
			Name:   "ops-hook",
			Kind:   notify.KindWebhook,
			URL:    srv.URL + "/receive",
			Secret: secret,
			Events: []notify.EventKind{notify.EventApplied},
		}},
		Now: func() time.Time { return now },
	})
	defer d.Close()

	ev := notify.Event{
		ID:        "01ABCDEFGHJKLMNPQRSTVWXYZ0",
		Event:     notify.EventApplied,
		At:        now,
		Server:    "https://writ.example.com",
		Stack:     "network",
		Env:       "prod",
		Status:    "applied",
		Summary:   "applied network@prod: 2 added, 1 changed, 0 destroyed",
		StartedBy: notify.Actor{Kind: "person", Display: "alice", Person: "gitlab/user/1001"},
		Plan:      "0123456789abcdef0123456789abcdef",
		Run:       "01RUN000000000000000000000",
		Counts:    &notify.Counts{Added: 2, Changed: 1},
		URL:       "https://writ.example.com/ui/runs/01RUN000000000000000000000",
	}
	d.Emit(ev)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for webhook delivery")
	}

	if ct := gotHeaders.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if ge := gotHeaders.Get("X-Writ-Event"); ge != "applied" {
		t.Errorf("X-Writ-Event = %q, want applied", ge)
	}
	if gd := gotHeaders.Get("X-Writ-Delivery"); gd != ev.ID {
		t.Errorf("X-Writ-Delivery = %q, want %q", gd, ev.ID)
	}
	tsStr := gotHeaders.Get("X-Writ-Timestamp")
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil || ts != now.Unix() {
		t.Fatalf("X-Writ-Timestamp = %q, want %d", tsStr, now.Unix())
	}
	sig := gotHeaders.Get("X-Writ-Signature-256")
	if !notify.Verify(secret, ts, gotBody, sig) {
		t.Errorf("signature %q did not verify against body %s", sig, gotBody)
	}

	var decoded map[string]any
	if err := json.Unmarshal(gotBody, &decoded); err != nil {
		t.Fatalf("invalid JSON body: %v", err)
	}
	if decoded["format"] != notify.FormatNotification {
		t.Errorf("format = %v, want %s", decoded["format"], notify.FormatNotification)
	}
	if decoded["stack"] != "network" || decoded["env"] != "prod" {
		t.Errorf("unexpected stack/env: %v", decoded)
	}
}

func TestSlackPayloadEscapesMrkdwn(t *testing.T) {
	ev := notify.Event{
		ID:        "01ABCDEFGHJKLMNPQRSTVWXYZ1",
		Event:     notify.EventPlanFailed,
		At:        time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC),
		Server:    "https://writ.example.com",
		Stack:     "app",
		Env:       "prod",
		Status:    "failed",
		Summary:   "plan failed: <!channel> <https://evil.example|click> & error",
		StartedBy: notify.Actor{Kind: "ci", Display: "ci <bot>"},
		URL:       "https://writ.example.com/ui/plans/abc",
	}
	body, err := notify.FormatSlack(ev)
	if err != nil {
		t.Fatalf("FormatSlack: %v", err)
	}
	var doc struct {
		Text   string `json:"text"`
		Blocks []struct {
			Type string `json:"type"`
			Text struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"text"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("invalid slack JSON: %v", err)
	}
	if doc.Text == "" || len(doc.Blocks) == 0 {
		t.Fatalf("expected text and blocks, got %+v", doc)
	}
	for _, s := range []string{doc.Text, doc.Blocks[0].Text.Text} {
		if strings.Contains(s, "<!channel>") {
			t.Errorf("slack payload did not escape <!channel>: %q", s)
		}
		if strings.Contains(s, "<https://evil.example|click>") {
			t.Errorf("slack payload did not escape injected link: %q", s)
		}
	}
	if !strings.Contains(doc.Blocks[0].Text.Text, "<https://writ.example.com/ui/plans/abc|View in Writ>") {
		t.Errorf("slack payload missing UI link: %q", doc.Blocks[0].Text.Text)
	}
}

func TestBodySizeCap(t *testing.T) {
	ev := notify.Event{
		ID:      "BIG1",
		Event:   notify.EventPlanFailed,
		Status:  "failed",
		Summary: strings.Repeat("a", notify.MaxBodyBytes+100),
	}
	if _, err := notify.FormatWebhook(ev); err == nil {
		t.Error("FormatWebhook did not refuse oversized body")
	}
	if _, err := notify.FormatSlack(ev); err == nil {
		t.Error("FormatSlack did not refuse oversized body")
	}
}

func TestRetryBackoffAndGiveUp(t *testing.T) {
	t.Run("succeeds after 5xx retries", func(t *testing.T) {
		var attempts atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if attempts.Add(1) < 3 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		resCh := make(chan notify.Result, 1)
		d := notify.New(notify.Options{
			Targets: []notify.Target{{
				Name:   "flaky",
				Kind:   notify.KindWebhook,
				URL:    srv.URL,
				Events: []notify.EventKind{notify.EventDrift},
			}},
			Backoff: []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond},
			OnResult: func(r notify.Result) {
				resCh <- r
			},
		})
		defer d.Close()

		d.Emit(notify.Event{ID: "ID1", Event: notify.EventDrift, Stack: "app", Env: "prod", Status: "drifted", DriftStateChanged: true, Summary: "drift"})
		select {
		case res := <-resCh:
			if !res.OK {
				t.Fatalf("expected OK after retries, got err %q", res.Err)
			}
			if got := attempts.Load(); got != 3 {
				t.Errorf("attempts = %d, want 3", got)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out")
		}
	})

	t.Run("gives up after exhausting retries and logs without URL secret", func(t *testing.T) {
		const secretPath = "/services/SUPER_SECRET_SLACK_TOKEN_999"
		var attempts atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts.Add(1)
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("UNTRUSTED_UPSTREAM_BODY_TEXT"))
		}))
		defer srv.Close()

		var logBuf bytes.Buffer
		log := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))
		resCh := make(chan notify.Result, 1)
		d := notify.New(notify.Options{
			Targets: []notify.Target{{
				Name:   "broken",
				Kind:   notify.KindSlack,
				URL:    srv.URL + secretPath,
				Events: []notify.EventKind{notify.EventApplyFailed},
			}},
			Backoff: []time.Duration{time.Millisecond, time.Millisecond},
			Log:     log,
			OnResult: func(r notify.Result) {
				resCh <- r
			},
		})
		defer d.Close()

		d.Emit(notify.Event{ID: "ID2", Event: notify.EventApplyFailed, Stack: "app", Env: "prod", Status: "failed", Summary: "apply failed"})
		select {
		case res := <-resCh:
			if res.OK {
				t.Fatal("expected failure")
			}
			if got := attempts.Load(); got != 3 {
				t.Errorf("attempts = %d, want 3 (1 initial + 2 retries)", got)
			}
			if strings.Contains(res.Err, "SUPER_SECRET_SLACK_TOKEN_999") {
				t.Errorf("result error leaked webhook URL secret: %q", res.Err)
			}
			if strings.Contains(res.Err, "UNTRUSTED_UPSTREAM_BODY_TEXT") {
				t.Errorf("result error leaked response body: %q", res.Err)
			}
			if !strings.Contains(res.Err, "502") {
				t.Errorf("expected HTTP status 502 in error, got %q", res.Err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out")
		}
		if strings.Contains(logBuf.String(), "SUPER_SECRET_SLACK_TOKEN_999") {
			t.Errorf("log leaked webhook URL secret: %s", logBuf.String())
		}
		if strings.Contains(logBuf.String(), "UNTRUSTED_UPSTREAM_BODY_TEXT") {
			t.Errorf("log leaked response body: %s", logBuf.String())
		}
	})

	t.Run("does not retry 4xx", func(t *testing.T) {
		var attempts atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts.Add(1)
			w.WriteHeader(http.StatusBadRequest)
		}))
		defer srv.Close()

		resCh := make(chan notify.Result, 1)
		d := notify.New(notify.Options{
			Targets: []notify.Target{{
				Name:   "client-err",
				Kind:   notify.KindWebhook,
				URL:    srv.URL,
				Events: []notify.EventKind{notify.EventApplied},
			}},
			Backoff:  []time.Duration{time.Millisecond, time.Millisecond},
			OnResult: func(r notify.Result) { resCh <- r },
		})
		defer d.Close()

		d.Emit(notify.Event{ID: "ID3", Event: notify.EventApplied, Stack: "app", Env: "prod", Status: "applied", Summary: "ok"})
		select {
		case res := <-resCh:
			if res.OK {
				t.Fatal("expected failure on 400")
			}
			if got := attempts.Load(); got != 1 {
				t.Errorf("attempts = %d, want 1 (no retry on 400)", got)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out")
		}
	})

	t.Run("retries on timeout and gives up without leaking URL", func(t *testing.T) {
		const secretPath = "/hooks/SECRET_TIMEOUT_TOKEN"
		var attempts atomic.Int32
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts.Add(1)
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}))
		defer func() {
			close(release)
			srv.Close()
		}()

		resCh := make(chan notify.Result, 1)
		d := notify.New(notify.Options{
			Targets: []notify.Target{{
				Name:   "slow",
				Kind:   notify.KindWebhook,
				URL:    srv.URL + secretPath,
				Events: []notify.EventKind{notify.EventDrift},
			}},
			Timeout:  30 * time.Millisecond,
			Backoff:  []time.Duration{time.Millisecond},
			OnResult: func(r notify.Result) { resCh <- r },
		})
		defer d.Close()

		d.Emit(notify.Event{ID: "ID4", Event: notify.EventDrift, Stack: "app", Env: "prod", Status: "drifted", DriftStateChanged: true, Summary: "drift"})
		select {
		case res := <-resCh:
			if res.OK {
				t.Fatal("expected timeout failure")
			}
			if got := attempts.Load(); got != 2 {
				t.Errorf("attempts = %d, want 2", got)
			}
			if strings.Contains(res.Err, "SECRET_TIMEOUT_TOKEN") {
				t.Errorf("timeout error leaked URL secret: %q", res.Err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out")
		}
	})
}

func TestRedirectRefused(t *testing.T) {
	var otherHits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer other.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	err := notify.SendTest(context.Background(), notify.Target{
		Name:   "redir",
		Kind:   notify.KindWebhook,
		URL:    srv.URL + "/SECRET_HOOK_PATH",
		Secret: []byte("secret"),
	}, notify.Event{ID: "T1", Event: notify.EventTest, Status: "ok", Summary: "test"}, 2*time.Second)
	if err == nil {
		t.Fatal("expected redirect to be refused")
	}
	if otherHits.Load() != 0 {
		t.Fatalf("redirect target received %d requests, want 0", otherHits.Load())
	}
	if !strings.Contains(err.Error(), "redirect") {
		t.Errorf("expected redirect error, got %q", err.Error())
	}
	if strings.Contains(err.Error(), "SECRET_HOOK_PATH") {
		t.Errorf("error leaked webhook URL path: %q", err.Error())
	}
}

func TestQueueFullDoesNotBlock(t *testing.T) {
	block := make(chan struct{})
	inHandler := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case inHandler <- struct{}{}:
		default:
		}
		<-block
		w.WriteHeader(http.StatusOK)
	}))
	var unblock sync.Once
	release := func() { unblock.Do(func() { close(block) }) }
	defer func() {
		release()
		srv.Close()
	}()

	var mu sync.Mutex
	var results []notify.Result
	reported := make(chan struct{}, 8)
	d := notify.New(notify.Options{
		Targets: []notify.Target{{
			Name:   "bounded",
			Kind:   notify.KindWebhook,
			URL:    srv.URL,
			Events: []notify.EventKind{notify.EventApplied},
		}},
		QueueSize: 1,
		OnResult: func(r notify.Result) {
			mu.Lock()
			results = append(results, r)
			mu.Unlock()
			reported <- struct{}{}
		},
	})
	defer d.Close()

	// First event starts delivering and blocks inside handler.
	d.Emit(notify.Event{ID: "E1", Event: notify.EventApplied, Stack: "app", Env: "prod", Status: "applied", Summary: "1"})
	select {
	case <-inHandler:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never started first request")
	}

	// Second fills queue (capacity 1); third and fourth must drop immediately without blocking.
	start := time.Now()
	d.Emit(notify.Event{ID: "E2", Event: notify.EventApplied, Stack: "app", Env: "prod", Status: "applied", Summary: "2"})
	d.Emit(notify.Event{ID: "E3", Event: notify.EventApplied, Stack: "app", Env: "prod", Status: "applied", Summary: "3"})
	d.Emit(notify.Event{ID: "E4", Event: notify.EventApplied, Stack: "app", Env: "prod", Status: "applied", Summary: "4"})
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("Emit blocked for %v when queue was full", elapsed)
	}

	mu.Lock()
	early := len(results)
	mu.Unlock()
	if early != 0 {
		t.Fatalf("OnResult ran %d times inside Emit; the worker reports drops", early)
	}

	// The worker reports E3 and E4 as one drop after E1's delivery.
	release()
	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		var dropped int64
		delivered := 0
		for _, r := range results {
			if r.OK {
				delivered++
			}
			dropped += r.Dropped
		}
		mu.Unlock()
		if delivered == 2 && dropped == 2 {
			break
		}
		select {
		case <-reported:
		case <-deadline:
			t.Fatalf("delivered %d, dropped %d; want 2 and 2", delivered, dropped)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for _, r := range results {
		if r.Dropped > 0 && (r.OK || r.Err != "queue full: dropped 2 events") {
			t.Errorf("drop result = %+v", r)
		}
	}
}

func TestDriftDedupeMode(t *testing.T) {
	var mu sync.Mutex
	var gotOnChange, gotEveryCheck []notify.Event
	onChangeDone := make(chan struct{}, 10)
	everyCheckDone := make(chan struct{}, 10)

	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev notify.Event
		_ = json.NewDecoder(r.Body).Decode(&ev)
		mu.Lock()
		gotOnChange = append(gotOnChange, ev)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		onChangeDone <- struct{}{}
	}))
	defer srv1.Close()

	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev notify.Event
		_ = json.NewDecoder(r.Body).Decode(&ev)
		mu.Lock()
		gotEveryCheck = append(gotEveryCheck, ev)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		everyCheckDone <- struct{}{}
	}))
	defer srv2.Close()

	d := notify.New(notify.Options{
		Targets: []notify.Target{
			{Name: "dedupe", Kind: notify.KindWebhook, URL: srv1.URL, Events: []notify.EventKind{notify.EventDrift}, DriftMode: notify.DriftOnChange},
			{Name: "every", Kind: notify.KindWebhook, URL: srv2.URL, Events: []notify.EventKind{notify.EventDrift}, DriftMode: notify.DriftEveryCheck},
		},
	})
	defer d.Close()

	// 1. clean -> drifted (state changed)
	d.Emit(notify.Event{ID: "D1", Event: notify.EventDrift, Stack: "app", Env: "prod", Status: "drifted", DriftStateChanged: true, Summary: "drifted 1"})
	// 2. drifted -> drifted (no state change)
	d.Emit(notify.Event{ID: "D2", Event: notify.EventDrift, Stack: "app", Env: "prod", Status: "drifted", DriftStateChanged: false, Summary: "drifted 2"})
	// 3. drifted -> clean (state changed)
	d.Emit(notify.Event{ID: "D3", Event: notify.EventDrift, Stack: "app", Env: "prod", Status: "clean", DriftStateChanged: true, Summary: "clean"})

	waitN := func(ch chan struct{}, n int) {
		for range n {
			select {
			case <-ch:
			case <-time.After(5 * time.Second):
				t.Fatalf("timed out waiting for %d deliveries", n)
			}
		}
	}
	waitN(onChangeDone, 2)
	waitN(everyCheckDone, 3)

	// Give a brief window to ensure no extra delivery arrives on dedupe.
	select {
	case <-onChangeDone:
		t.Fatal("on-change notifier received unexpected third delivery")
	case <-time.After(50 * time.Millisecond):
	}

	mu.Lock()
	defer mu.Unlock()
	if len(gotOnChange) != 2 || gotOnChange[0].ID != "D1" || gotOnChange[1].ID != "D3" {
		t.Errorf("on-change received %+v, want [D1, D3]", gotOnChange)
	}
	if len(gotEveryCheck) != 3 || gotEveryCheck[0].ID != "D1" || gotEveryCheck[1].ID != "D2" || gotEveryCheck[2].ID != "D3" {
		t.Errorf("every-check received %+v, want [D1, D2, D3]", gotEveryCheck)
	}
}

// A display name a person chose can't add a link, a mention, or a line of
// its own to a Slack or Mattermost message: it goes in a code span with
// its control characters made spaces.
func TestSlackDisplayNameStaysInert(t *testing.T) {
	ev := notify.Event{Event: notify.EventApplied, Stack: "app", Env: "prod", Summary: "applied",
		StartedBy: notify.Actor{Display: "eve https://phish.example/login @channel\n*[apply-failed]* `app@prod`: apply failed x"}}
	b, err := notify.FormatSlack(ev)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Text   string `json:"text"`
		Blocks []struct {
			Text struct{ Text string } `json:"text"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	for _, blk := range doc.Blocks {
		msg := blk.Text.Text
		if strings.Count(msg, "\n") > 1 || strings.Contains(msg, " ") {
			t.Fatalf("the display name started a line of its own: %q", msg)
		}
		i := strings.Index(msg, "Started by: `")
		if i < 0 {
			t.Fatalf("the display name is not in a code span: %q", msg)
		}
		rest := msg[i+len("Started by: `"):]
		end := strings.Index(rest, "`")
		if end < 0 || !strings.Contains(rest[:end], "https://phish.example") || !strings.Contains(rest[:end], "@channel") {
			t.Fatalf("the link or mention is outside the code span: %q", msg)
		}
	}
}

// A certificate the client does not trust is permanent: one attempt, and
// an error that says tls, where it was "connection failed" and tried again
// for four minutes.
func TestTLSFailuresArePermanent(t *testing.T) {
	tlsSrv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	tlsSrv.Config.ErrorLog = log.New(io.Discard, "", 0)
	tlsSrv.StartTLS()
	defer tlsSrv.Close()
	for _, c := range []struct{ url, want string }{
		{tlsSrv.URL, "tls: certificate signed by an unknown authority"},
	} {
		results := make(chan notify.Result, 4)
		d := notify.New(notify.Options{
			Targets: []notify.Target{{Name: "t", Kind: notify.KindWebhook, URL: c.url, Events: []notify.EventKind{notify.EventApplied}}},
			Backoff: []time.Duration{time.Millisecond, time.Millisecond},
			OnResult: func(r notify.Result) {
				results <- r
			},
		})
		d.Emit(notify.Event{ID: "E1", Event: notify.EventApplied, Stack: "app", Env: "prod", Status: "applied", Summary: "s"})
		select {
		case r := <-results:
			if r.OK || r.Attempts != 1 || r.Err != c.want {
				t.Errorf("%s: %+v, want one attempt and %q", c.url, r, c.want)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: no result", c.url)
		}
		d.Close()
	}
}

// A 429's Retry-After holds the next attempt back for as long as it asks,
// beyond the backoff.
func TestRetryAfterOn429(t *testing.T) {
	var mu sync.Mutex
	var at []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		at = append(at, time.Now())
		n := len(at)
		mu.Unlock()
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
		}
	}))
	defer srv.Close()
	results := make(chan notify.Result, 4)
	d := notify.New(notify.Options{
		Targets:  []notify.Target{{Name: "t", Kind: notify.KindWebhook, URL: srv.URL, Events: []notify.EventKind{notify.EventApplied}}},
		Backoff:  []time.Duration{time.Millisecond},
		OnResult: func(r notify.Result) { results <- r },
	})
	defer d.Close()
	d.Emit(notify.Event{ID: "E1", Event: notify.EventApplied, Stack: "app", Env: "prod", Status: "applied", Summary: "s"})
	select {
	case r := <-results:
		if !r.OK || r.Attempts != 2 {
			t.Fatalf("result %+v, want delivered on the second attempt", r)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no result")
	}
	mu.Lock()
	defer mu.Unlock()
	if gap := at[1].Sub(at[0]); gap < 900*time.Millisecond {
		t.Errorf("the second attempt came %v after the 429, want about the second Retry-After asked for", gap)
	}
}

// A drop is reported under the kind of each event dropped, not the kind
// of the event delivered before the report.
func TestQueueDropsReportTheirOwnKind(t *testing.T) {
	block := make(chan struct{})
	inHandler := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case inHandler <- struct{}{}:
		default:
		}
		<-block
	}))
	var once sync.Once
	release := func() { once.Do(func() { close(block) }) }
	defer func() { release(); srv.Close() }()
	results := make(chan notify.Result, 8)
	d := notify.New(notify.Options{
		Targets: []notify.Target{{Name: "t", Kind: notify.KindWebhook, URL: srv.URL,
			Events: []notify.EventKind{notify.EventApplied, notify.EventApplyFailed, notify.EventPlanFailed}}},
		QueueSize: 1,
		OnResult:  func(r notify.Result) { results <- r },
	})
	defer d.Close()
	ev := func(id string, kind notify.EventKind) notify.Event {
		return notify.Event{ID: id, Event: kind, Stack: "app", Env: "prod", Status: "s", Summary: id}
	}
	d.Emit(ev("E1", notify.EventApplied))
	select {
	case <-inHandler:
	case <-time.After(5 * time.Second):
		t.Fatal("the first delivery never started")
	}
	d.Emit(ev("E2", notify.EventApplied))
	d.Emit(ev("E3", notify.EventApplyFailed))
	d.Emit(ev("E4", notify.EventPlanFailed))
	d.Emit(ev("E5", notify.EventApplyFailed))
	release()
	drops := map[notify.EventKind]int64{}
	delivered := 0
	for delivered < 2 || len(drops) < 2 {
		select {
		case r := <-results:
			if r.Dropped > 0 {
				drops[r.Event] += r.Dropped
			} else if r.OK {
				delivered++
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("delivered %d, drops %v", delivered, drops)
		}
	}
	if drops[notify.EventApplyFailed] != 2 || drops[notify.EventPlanFailed] != 1 || len(drops) != 2 {
		t.Errorf("drops by kind %v, want apply-failed 2 and plan-failed 1", drops)
	}
}

func TestRetryAfterHoldsNextDelivery(t *testing.T) {
	requests := make(chan time.Time, 3)
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- time.Now()
		switch attempts.Add(1) {
		case 1:
			w.WriteHeader(http.StatusServiceUnavailable)
		case 2:
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
		}
	}))
	defer srv.Close()
	results := make(chan notify.Result, 2)
	d := notify.New(notify.Options{
		Targets:  []notify.Target{{Name: "ops", Kind: notify.KindWebhook, URL: srv.URL, Events: []notify.EventKind{notify.EventApplied}}},
		Backoff:  []time.Duration{time.Millisecond},
		OnResult: func(r notify.Result) { results <- r },
	})
	defer d.Close()
	for _, id := range []string{"FIRST", "SECOND"} {
		d.Emit(notify.Event{ID: id, Event: notify.EventApplied, Stack: "app", Env: "prod", Status: "applied"})
	}
	for i, wantOK := range []bool{false, true} {
		select {
		case r := <-results:
			if r.OK != wantOK || r.Attempts != 2-i {
				t.Fatalf("delivery %d: %+v", i+1, r)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("delivery %d did not finish", i+1)
		}
	}
	<-requests
	lastFirst, firstSecond := <-requests, <-requests
	if gap := firstSecond.Sub(lastFirst); gap < 900*time.Millisecond {
		t.Errorf("second event sent %v after the first event's 429, want a one-second wait", gap)
	}
}

func TestHTTPSOnPlaintextListenerIsPermanent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("plaintext handler received a request")
	}))
	defer srv.Close()
	results := make(chan notify.Result, 1)
	d := notify.New(notify.Options{
		Targets: []notify.Target{{Name: "ops", Kind: notify.KindWebhook, URL: "https" + strings.TrimPrefix(srv.URL, "http"),
			Events: []notify.EventKind{notify.EventApplied}}},
		Backoff:  []time.Duration{time.Millisecond},
		OnResult: func(r notify.Result) { results <- r },
	})
	defer d.Close()
	d.Emit(notify.Event{ID: "RUN", Event: notify.EventApplied, Stack: "app", Env: "prod", Status: "applied"})
	select {
	case r := <-results:
		if r.OK || r.Attempts != 1 || !strings.HasPrefix(r.Err, "tls:") {
			t.Fatalf("delivery: %+v, want a permanent TLS error", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("delivery did not finish")
	}
}

// An https URL whose listener answers with something other than TLS or
// HTTP, such as a mail server's banner, is wrong until an operator fixes
// the URL, so it is not tried again.
func TestHTTPSOnNonTLSListenerIsPermanent(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("220 mail.example ESMTP ready\r\n"))
			c.Close()
		}
	}()
	results := make(chan notify.Result, 1)
	d := notify.New(notify.Options{
		Targets: []notify.Target{{Name: "ops", Kind: notify.KindWebhook, URL: "https://" + ln.Addr().String() + "/hook",
			Events: []notify.EventKind{notify.EventApplied}}},
		Backoff:  []time.Duration{time.Millisecond},
		OnResult: func(r notify.Result) { results <- r },
	})
	defer d.Close()
	d.Emit(notify.Event{ID: "RUN", Event: notify.EventApplied, Stack: "app", Env: "prod", Status: "applied"})
	select {
	case r := <-results:
		if r.OK || r.Attempts != 1 || !strings.HasPrefix(r.Err, "tls:") {
			t.Fatalf("delivery: %+v, want a permanent TLS error", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("delivery did not finish")
	}
}
