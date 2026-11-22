package cli

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/server"
)

// busyServer is a writ serve whose plan slots are busy for the first
// busy plan requests, which it answers 429, and which then takes the plan
// and answers its reads with a pending plan. It records the token of each
// plan request.
type busyServer struct {
	*httptest.Server
	mu     sync.Mutex
	busy   int
	tokens []string
	// times are when each plan request came.
	times []time.Time
	// status and retryAfter are the refusal's status, 429 when 0, and its
	// Retry-After, none when "".
	status     int
	retryAfter string
	// unmarked leaves out X-Writ-Retry, as a proxy's 429 or 503 would.
	unmarked bool
}

func newBusyServer(t *testing.T, busy int) *busyServer {
	t.Helper()
	b := &busyServer{busy: busy}
	b.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/plans":
			b.tokens = append(b.tokens, r.Header.Get("Authorization"))
			b.times = append(b.times, time.Now())
			if b.busy != 0 {
				b.busy--
				if b.retryAfter != "" {
					w.Header().Set("Retry-After", b.retryAfter)
				}
				if !b.unmarked {
					w.Header().Set(server.RetryHeader, server.RetrySlots)
				}
				w.WriteHeader(cmp.Or(b.status, http.StatusTooManyRequests))
				_, _ = w.Write([]byte(`{"format":"writ.remote-plan/v0","ok":false,"diagnostics":[{"severity":"error","class":"usage","message":"writ serve is running 4 plans for github acme/web or 8 in all; try again when one ends"}]}`))
				return
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"format":"writ.remote-plan/v0","ok":true,"plan":{"ref":"REF","status":"planning"},"diagnostics":[]}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/plans/REF"):
			_, _ = w.Write([]byte(`{"format":"writ.remote-plan/v0","ok":true,"plan":{"ref":"REF","id":"0123","status":"pending"},"diagnostics":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(b.Close)
	return b
}

func (b *busyServer) postTimes() []time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]time.Time(nil), b.times...)
}

func (b *busyServer) posts() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.tokens...)
}

var slotPlan = RemotePlanOptions{RemoteOptions: RemoteOptions{Stack: "app"}, Repo: "web", Commit: strings.Repeat("a", 40), JSON: true}

// TestSlotWait checks that writ plan --remote --slot-wait asks again
// while writ serve's plan slots are busy, and plans once one frees, with
// a fresh GitHub Actions token on each try.
func TestSlotWait(t *testing.T) {
	writ := newBusyServer(t, 3)
	actions := newActionsEndpoint(t)
	env := actions.env()
	env["WRIT_ENV"] = "prod"
	a := precedenceApp(t, t.TempDir(), writ.URL, env)
	var errOut bytes.Buffer
	a.opts.Stderr = &errOut
	o := slotPlan
	o.SlotWait = 4 * time.Second
	_, doc, err := a.sendRemotePlan(context.Background(), o)
	if err != nil || !strings.Contains(string(doc), `"status":"pending"`) {
		t.Fatalf("got %s, %v\n%s", doc, err, errOut.String())
	}
	if got := writ.posts(); len(got) != 4 {
		t.Fatalf("%d plan requests, want 3 refused and 1 taken", len(got))
	}
	if len(actions.audiences) != 4 {
		t.Fatalf("asked Actions for %d tokens, want one a try", len(actions.audiences))
	}
	if n := strings.Count(errOut.String(), "plan slots are busy"); n != 3 {
		t.Fatalf("said the slots were busy %d times:\n%s", n, errOut.String())
	}
}

// TestSlotWaitGivesUp checks that the wait is bounded: when the slots stay
// busy, the plan is refused soon after the wait ends, with writ serve's
// refusal.
func TestSlotWaitGivesUp(t *testing.T) {
	writ := newBusyServer(t, -1)
	a := precedenceApp(t, t.TempDir(), writ.URL, map[string]string{tokenCI: "ci-token", "WRIT_ENV": "prod"})
	var out, errOut bytes.Buffer
	a.opts.Stdout, a.opts.Stderr = &out, &errOut
	o := slotPlan
	o.SlotWait = 800 * time.Millisecond
	start := time.Now()
	_, _, err := a.sendRemotePlan(context.Background(), o)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrFailed) || !strings.Contains(out.String(), "writ serve is running 4 plans") || !strings.Contains(errOut.String(), "still refused the plan with 429 after waiting 800ms") {
		t.Fatalf("got %v\n%s\n%s", err, out.String(), errOut.String())
	}
	if elapsed > 3*time.Second || len(writ.posts()) < 2 {
		t.Fatalf("gave up after %s and %d requests", elapsed, len(writ.posts()))
	}
	o.SlotWait = 2 * time.Hour
	if _, _, err := a.sendRemotePlan(context.Background(), o); err == nil || !strings.Contains(err.Error(), "--slot-wait 2h0m0s must be between 0 and 1h0m0s") {
		t.Fatalf("a wait over an hour: %v", err)
	}
}

// TestSlotWaitHonorsRetryAfter checks that a refusal's Retry-After, as
// writ serve sends when the forge limits the rate of calls, sets the next
// try, and that one past the wait's end gives up at once.
func TestSlotWaitHonorsRetryAfter(t *testing.T) {
	writ := newBusyServer(t, 1)
	writ.status, writ.retryAfter = http.StatusServiceUnavailable, "1"
	a := precedenceApp(t, t.TempDir(), writ.URL, map[string]string{tokenCI: "ci-token", "WRIT_ENV": "prod"})
	a.opts.Stderr = &bytes.Buffer{}
	o := slotPlan
	o.SlotWait = 3 * time.Second
	if _, _, err := a.sendRemotePlan(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if times := writ.postTimes(); len(times) != 2 || times[1].Sub(times[0]) < time.Second {
		t.Fatalf("tries at %v, want the second Retry-After's second after the first", times)
	}

	far := newBusyServer(t, -1)
	far.status, far.retryAfter = http.StatusServiceUnavailable, "60"
	a = precedenceApp(t, t.TempDir(), far.URL, map[string]string{tokenCI: "ci-token", "WRIT_ENV": "prod"})
	a.opts.Stdout, a.opts.Stderr = &bytes.Buffer{}, &bytes.Buffer{}
	start := time.Now()
	if _, _, err := a.sendRemotePlan(context.Background(), o); !errors.Is(err, ErrFailed) || len(far.posts()) != 1 || time.Since(start) > time.Second {
		t.Fatalf("a Retry-After past the wait: %v after %d tries and %s", err, len(far.posts()), time.Since(start))
	}
}

// TestSlotWaitIsBounded checks that the wait starts before the first
// GitHub Actions token is fetched, and that no plan request starts after
// it ends, though each token takes a while to come.
func TestSlotWaitIsBounded(t *testing.T) {
	writ := newBusyServer(t, -1)
	actions := newActionsEndpoint(t)
	actions.delay = 350 * time.Millisecond
	env := actions.env()
	env["WRIT_ENV"] = "prod"
	a := precedenceApp(t, t.TempDir(), writ.URL, env)
	a.opts.Stdout, a.opts.Stderr = &bytes.Buffer{}, &bytes.Buffer{}
	o := slotPlan
	o.SlotWait = 800 * time.Millisecond
	start := time.Now()
	_, _, err := a.sendRemotePlan(context.Background(), o)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrFailed) {
		t.Fatalf("got %v", err)
	}
	times := writ.postTimes()
	deadline := start.Add(o.SlotWait)
	for i, at := range times {
		if at.After(deadline) {
			t.Fatalf("try %d started %v after the wait ended", i+1, at.Sub(deadline))
		}
	}
	if elapsed > o.SlotWait+300*time.Millisecond {
		t.Fatalf("the wait of %s took %s", o.SlotWait, elapsed)
	}
}

// TestSlotWaitAsksAgainOnlyWhenMarked checks that a 429 or 503 without
// X-Writ-Retry, as a proxy may answer after writ serve took the plan, is
// not asked again, and a marked one is.
func TestSlotWaitAsksAgainOnlyWhenMarked(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		unmarked bool
		posts    int
	}{
		{"a proxy's 503", http.StatusServiceUnavailable, true, 1},
		{"a proxy's 429", http.StatusTooManyRequests, true, 1},
		{"writ serve's 503", http.StatusServiceUnavailable, false, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writ := newBusyServer(t, 1)
			writ.status, writ.unmarked = tc.status, tc.unmarked
			a := precedenceApp(t, t.TempDir(), writ.URL, map[string]string{tokenCI: "ci-token", "WRIT_ENV": "prod"})
			a.opts.Stdout, a.opts.Stderr = &bytes.Buffer{}, &bytes.Buffer{}
			o := slotPlan
			o.SlotWait = 3 * time.Second
			_, _, err := a.sendRemotePlan(context.Background(), o)
			if got := len(writ.posts()); got != tc.posts || (tc.posts == 1) != errors.Is(err, ErrFailed) {
				t.Fatalf("%d plan requests, %v", got, err)
			}
		})
	}
}

// TestSlotWaitRetryAfterForms checks a Retry-After of more seconds than a
// duration holds, which gives up at once rather than overflowing, and one
// that is an HTTP-date, which sets the next try.
func TestSlotWaitRetryAfterForms(t *testing.T) {
	huge := newBusyServer(t, -1)
	huge.retryAfter = "9223372036854775807"
	a := precedenceApp(t, t.TempDir(), huge.URL, map[string]string{tokenCI: "ci-token", "WRIT_ENV": "prod"})
	a.opts.Stdout, a.opts.Stderr = &bytes.Buffer{}, &bytes.Buffer{}
	o := slotPlan
	o.SlotWait = 3 * time.Second
	start := time.Now()
	if _, _, err := a.sendRemotePlan(context.Background(), o); !errors.Is(err, ErrFailed) || len(huge.posts()) != 1 || time.Since(start) > time.Second {
		t.Fatalf("a huge Retry-After: %v after %d tries and %s", err, len(huge.posts()), time.Since(start))
	}

	dated := newBusyServer(t, 1)
	dated.retryAfter = time.Now().Add(2 * time.Second).UTC().Format(http.TimeFormat)
	a = precedenceApp(t, t.TempDir(), dated.URL, map[string]string{tokenCI: "ci-token", "WRIT_ENV": "prod"})
	a.opts.Stdout, a.opts.Stderr = &bytes.Buffer{}, &bytes.Buffer{}
	o.SlotWait = 5 * time.Second
	if _, _, err := a.sendRemotePlan(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if times := dated.postTimes(); len(times) != 2 || times[1].Sub(times[0]) < time.Second {
		t.Fatalf("tries at %v, want the second at the Retry-After date", times)
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		v    string
		wait time.Duration
		past bool
	}{
		{"", 0, false},
		{"5", 5 * time.Second, false},
		{"61", 0, true},
		{"9223372036854775807", 0, true},
		{"99999999999999999999999", 0, true},
		{"Sun, 22 Nov 2026 12:00:30 GMT", 30 * time.Second, false},
		{"Sun, 22 Nov 2026 12:05:00 GMT", 5 * time.Minute, true},
		{"Sun, 22 Nov 2026 11:59:00 GMT", 0, false},
		{"-5", 0, false},
		{"soon", 0, false},
	} {
		if wait, past := retryAfter(tc.v, now, time.Minute); wait != tc.wait || past != tc.past {
			t.Errorf("retryAfter(%q) = %s, %v; want %s, %v", tc.v, wait, past, tc.wait, tc.past)
		}
	}
}
