package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
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
			if b.busy != 0 {
				b.busy--
				w.WriteHeader(http.StatusTooManyRequests)
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
