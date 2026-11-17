package server

import (
	"fmt"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func TestClientAddr(t *testing.T) {
	s := &Server{cfg: &Config{TrustedProxies: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8")}}}
	tests := []struct {
		name, peer string
		xff        []string
		remote     string
		key        string
	}{
		{"untrusted peer, no header", "203.0.113.5:4000", nil, "203.0.113.5", "203.0.113.5"},
		{"untrusted peer's header is ignored", "203.0.113.5:4000", []string{"198.51.100.1"}, "203.0.113.5", "203.0.113.5"},
		{"trusted peer, no header", "127.0.0.1:4000", nil, "127.0.0.1", "127.0.0.1"},
		{"trusted peer names the client", "127.0.0.1:4000", []string{"198.51.100.1"}, "198.51.100.1", "198.51.100.1"},
		{"a client's own entry left of the proxy's is ignored", "127.0.0.1:4000", []string{"192.0.2.66, 198.51.100.1"}, "198.51.100.1", "198.51.100.1"},
		{"trusted hops are skipped", "127.0.0.1:4000", []string{"198.51.100.1, 10.1.2.3", "10.9.9.9"}, "198.51.100.1", "198.51.100.1"},
		{"every hop trusted gives the left-most", "127.0.0.1:4000", []string{"10.1.2.3, 10.4.5.6"}, "10.1.2.3", "10.1.2.3"},
		{"an unreadable hop stops at the last address read", "127.0.0.1:4000", []string{"198.51.100.1, unknown, 10.1.2.3"}, "10.1.2.3", "10.1.2.3"},
		{"a hop with a port", "127.0.0.1:4000", []string{"198.51.100.1:5555"}, "198.51.100.1", "198.51.100.1"},
		{"an IPv6 client counts by its /64", "127.0.0.1:4000", []string{"[2001:db8:1:2:aaaa::1]:443"}, "2001:db8:1:2:aaaa::1", "2001:db8:1:2::/64"},
		{"an IPv4-mapped peer", "[::ffff:127.0.0.1]:4000", []string{"198.51.100.1"}, "198.51.100.1", "198.51.100.1"},
		{"an unreadable peer", "pipe", nil, "pipe", "pipe"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/v1/whoami", nil)
			r.RemoteAddr = tt.peer
			for _, v := range tt.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := s.remoteOf(r); got != tt.remote {
				t.Errorf("remote %q, want %q", got, tt.remote)
			}
			if got := s.clientKey(r); got != tt.key {
				t.Errorf("key %q, want %q", got, tt.key)
			}
		})
	}
}

func TestLimiterStaysUnderItsCap(t *testing.T) {
	now := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	l := newLimiter(2, 100, func() time.Time { return now })
	for i := range 50 {
		l.allow(fmt.Sprintf("old-%d", i))
	}
	now = now.Add(time.Second)
	for l.allow("hot") {
	}
	for i := range 10000 {
		l.allow(fmt.Sprintf("new-%d", i))
		if n := l.size(); n > 100 {
			t.Fatalf("after %d new keys the limiter holds %d buckets, over its cap of 100", i+1, n)
		}
	}
	if l.size() != 100 {
		t.Errorf("the limiter holds %d buckets, want 100", l.size())
	}
	for i := range 50 {
		if _, ok := l.buckets[fmt.Sprintf("old-%d", i)]; ok {
			t.Fatalf("old-%d, full again, outlived the eviction", i)
		}
	}
	l2 := newLimiter(2, 100, func() time.Time { return now })
	for i := range 50 {
		l2.allow(fmt.Sprintf("old-%d", i))
	}
	now = now.Add(time.Second)
	for l2.allow("hot") {
	}
	for i := range 49 {
		l2.allow(fmt.Sprintf("new-%d", i))
	}
	l2.allow("one-more")
	if l2.allow("hot") {
		t.Error("a key past its limit got a request once idle buckets were dropped to make room")
	}
}
