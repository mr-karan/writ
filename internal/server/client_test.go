package server_test

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/control/controltest"
	"github.com/mr-karan/writ/internal/server"
)

// syncBuffer is a log sink that handlers write to from many goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// frozen stops the server's clock, so no bucket fills while a test runs,
// and keeps its log.
func frozen(log *syncBuffer) func(*server.Options) {
	at := time.Now()
	return func(o *server.Options) {
		o.Now = func() time.Time { return at }
		o.Log = slog.New(slog.NewTextHandler(log, nil))
	}
}

func withConfig(extra string) func(string) string {
	return func(s string) string { return s + extra + "\n" }
}

// 500 requests with a bad token from one address: the address limit
// answers 429 past its burst, and those requests leave no audit row.
func TestBadTokensFromOneAddress(t *testing.T) {
	var log syncBuffer
	f := newFixture(t, nil, frozen(&log))
	statuses := map[int]int{}
	for range 500 {
		statuses[f.do("GET", "/v1/whoami", "not-a-token", nil).status]++
	}
	limit := server.DefaultRequestsPerAddress
	if statuses[401] != limit || statuses[429] != 500-limit {
		t.Fatalf("statuses %v, want %d 401 and %d 429", statuses, limit, 500-limit)
	}
	rows := f.audit(controltest.AuditFilter{Limit: 1000})
	failed := 0
	for _, e := range rows {
		if e.Route == "/v1/whoami" {
			failed++
		}
	}
	if failed != limit {
		t.Errorf("%d audit rows for /v1/whoami, want %d: one for each request under the limit", failed, limit)
	}
	if n := f.srv.Shed(); n != int64(500-limit) {
		t.Errorf("shed counts %d, want %d", n, 500-limit)
	}
	if n := strings.Count(log.String(), "requests over the address limit"); n != 1 {
		t.Errorf("%d log lines for the refused requests in one instant, want 1:\n%s", n, log.String())
	}
	if !strings.Contains(log.String(), "refused=1 total=1") {
		t.Errorf("the first refusal is not logged with its count:\n%s", log.String())
	}
}

// Behind a trusted proxy, each address in X-Forwarded-For has its own
// bucket, and audit rows name it.
func TestTrustedProxyClientsHaveTheirOwnLimit(t *testing.T) {
	var log syncBuffer
	f := newFixture(t, withConfig("limits {\n  requests_per_address = 2\n}\n"+`trusted_proxies = ["127.0.0.0/8"]`), frozen(&log))
	get := func(xff string) int {
		return f.do("GET", "/v1/whoami", "not-a-token", nil, "X-Forwarded-For", xff).status
	}
	for i, want := range []int{401, 401, 429} {
		if got := get("203.0.113.7"); got != want {
			t.Fatalf("request %d from 203.0.113.7: %d, want %d", i+1, got, want)
		}
	}
	for i, want := range []int{401, 401, 429} {
		if got := get("192.0.2.66, 203.0.113.8"); got != want {
			t.Fatalf("request %d from 203.0.113.8: %d, want %d", i+1, got, want)
		}
	}
	remotes := map[string]int{}
	for _, e := range f.audit(controltest.AuditFilter{Limit: 100}) {
		if e.Route == "/v1/whoami" {
			remotes[e.Remote]++
		}
	}
	if len(remotes) != 2 || remotes["203.0.113.7"] != 2 || remotes["203.0.113.8"] != 2 {
		t.Errorf("audit rows by remote %v, want two each for 203.0.113.7 and 203.0.113.8", remotes)
	}
}

// A peer that is not a trusted proxy cannot pick its bucket with
// X-Forwarded-For.
func TestUntrustedPeerForwardedForIgnored(t *testing.T) {
	var log syncBuffer
	f := newFixture(t, withConfig("limits {\n  requests_per_address = 2\n}\n"+`trusted_proxies = ["10.0.0.0/8"]`), frozen(&log))
	for i, want := range []int{401, 401, 429, 429} {
		xff := []string{"203.0.113.1", "203.0.113.2", "203.0.113.3", "203.0.113.4"}[i]
		if got := f.do("GET", "/v1/whoami", "not-a-token", nil, "X-Forwarded-For", xff).status; got != want {
			t.Fatalf("request %d naming %s: %d, want %d", i+1, xff, got, want)
		}
	}
	for _, e := range f.audit(controltest.AuditFilter{Limit: 100}) {
		if e.Route == "/v1/whoami" && e.Remote != "127.0.0.1" {
			t.Errorf("an audit row names %s, which the untrusted peer wrote", e.Remote)
		}
	}
}
