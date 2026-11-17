package server

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/mr-karan/writ/internal/diag"
)

// shedLogEvery is the shortest time between two log lines about requests
// refused over the address limit.
const shedLogEvery = 10 * time.Second

// clientAddr is the address of the client that sent r. It is the peer's
// address, unless the peer is one of trusted_proxies: then it is the
// right-most address of X-Forwarded-For that is not a trusted proxy. A
// client can write any X-Forwarded-For, but only the part a trusted proxy
// appended is read. ok is false when the peer's address cannot be read.
func (s *Server) clientAddr(r *http.Request) (netip.Addr, bool) {
	peer, ok := parseHop(r.RemoteAddr)
	if !ok {
		return netip.Addr{}, false
	}
	if !s.trustedProxy(peer) {
		return peer, true
	}
	client := peer
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		a, ok := parseHop(hops[i])
		if !ok {
			// A trusted proxy wrote what cannot be read, so the last
			// address read is the best one known.
			break
		}
		client = a
		if !s.trustedProxy(a) {
			break
		}
	}
	return client, true
}

func (s *Server) trustedProxy(a netip.Addr) bool {
	for _, p := range s.cfg.TrustedProxies {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// parseHop reads an address, with or without a port.
func parseHop(raw string) (netip.Addr, bool) {
	raw = strings.TrimSpace(raw)
	if ap, err := netip.ParseAddrPort(raw); err == nil {
		return ap.Addr().Unmap(), true
	}
	if host, _, err := net.SplitHostPort(raw); err == nil {
		raw = host
	}
	a, err := netip.ParseAddr(raw)
	if err != nil {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

// remoteOf is the client's address for audit rows and logs.
func (s *Server) remoteOf(r *http.Request) string {
	if a, ok := s.clientAddr(r); ok {
		return a.String()
	}
	return r.RemoteAddr
}

// clientKey is what the address limit counts by: an IPv4 address, or the
// /64 an IPv6 address is in, since one host often holds a whole /64.
func (s *Server) clientKey(r *http.Request) string {
	a, ok := s.clientAddr(r)
	switch {
	case !ok:
		return r.RemoteAddr
	case a.Is6():
		p, _ := a.Prefix(64)
		return p.String()
	}
	return a.String()
}

// limitClients counts every request against the limit of its client's
// address before any other work, and answers 429 past it. A refused
// request leaves no audit row, so a client that sends bad tokens fast
// cannot fill the control database: shed counts those requests instead.
func (s *Server) limitClients(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.clients.allow(s.clientKey(r)) {
			next.ServeHTTP(w, r)
			return
		}
		s.shed.add(s, s.remoteOf(r))
		_, pattern := s.api.Handler(r)
		w.Header().Set("Retry-After", "1")
		msg := fmt.Sprintf("more than %d requests a second from %s", s.cfg.Limits.RequestsPerAddress, s.remoteOf(r))
		s.reply(w, http.StatusTooManyRequests, refusal{Format: formatOf(pattern, r.Method), Diagnostics: []Diagnostic{{"error", string(diag.Usage), msg}}})
	})
}

// shed counts the requests refused over the address limit, and logs them
// at most once in shedLogEvery.
type shed struct {
	mu sync.Mutex
	// total counts every refused request since the start, and since the
	// ones after the last log line.
	total, since int64
	logged       time.Time
}

func (sh *shed) add(s *Server, remote string) {
	sh.mu.Lock()
	sh.total++
	sh.since++
	now := s.now()
	if !sh.logged.IsZero() && now.Sub(sh.logged) < shedLogEvery {
		sh.mu.Unlock()
		return
	}
	since, total := sh.since, sh.total
	sh.since, sh.logged = 0, now
	sh.mu.Unlock()
	s.log.Warn("requests over the address limit refused without audit rows", "remote", remote, "refused", since, "total", total, "limit", s.cfg.Limits.RequestsPerAddress)
}

// count is how many requests were refused over the address limit.
func (sh *shed) count() int64 {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return sh.total
}
