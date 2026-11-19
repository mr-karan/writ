package egress

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Hostile CONNECT authorities against an enforce proxy: each is refused,
// or dials only an address the resolver gave for a listed name. A NAT64
// address that embeds the metadata service or a private address is no way
// around the rules for either.
func TestHostileTargets(t *testing.T) {
	var mu sync.Mutex
	var dialed []string
	up := newEcho(t)
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		mu.Lock()
		dialed = append(dialed, address)
		mu.Unlock()
		return up.dial(ctx, network, address)
	}
	res := &stubResolver{answers: map[string][][]string{
		"x.amazonaws.com":      {{"52.1.1.1"}},
		"mix.amazonaws.com":    {{"52.1.1.1", "10.0.0.5"}},
		"meta.amazonaws.com":   {{"52.1.1.1", "169.254.169.254"}},
		"nat64.amazonaws.com":  {{"64:ff9b::a9fe:a9fe"}},
		"nat64p.amazonaws.com": {{"64:ff9b::a00:5"}},
		"nat64l.amazonaws.com": {{"64:ff9b:1::a9fe:a9fe"}},
		"nat64o.amazonaws.com": {{"64:ff9b:1:0:7f:0:100:0"}},
		"allowed.example.com":  {{"198.51.100.9"}},
		"169.254.169.254":      {{"169.254.169.254"}},
	}}
	p, err := New(Options{Mode: Enforce, List: mustList(t, "*.amazonaws.com", "allowed.example.com", "allowed.example.com:80"), Resolver: res, Dial: dial})
	if err != nil {
		t.Fatal(err)
	}
	addr := serveProxy(t, p)
	for _, c := range []struct{ target, status string }{
		{"169.254.169.254:80", "403 writ egress refused 169.254.169.254:80: a link-local address, where the instance metadata service is"},
		{"[::ffff:169.254.169.254]:80", "403 writ egress refused 169.254.169.254:80: a link-local address, where the instance metadata service is"},
		{"[::ffff:a9fe:a9fe]:80", "403 writ egress refused 169.254.169.254:80: a link-local address, where the instance metadata service is"},
		{"[64:ff9b::a9fe:a9fe]:80", "403 writ egress refused [64:ff9b::a9fe:a9fe]:80: a NAT64 address of a link-local address, where the instance metadata service is"},
		{"0xA9FEA9FE:80", "403 writ egress refused 0xa9fea9fe:80: not in the list"},
		{"2852039166:80", "403 writ egress refused 2852039166:80: not in the list"},
		{"0251.0376.0251.0376:80", "403 writ egress refused 0251.0376.0251.0376:80: not in the list"},
		{"169.254.169.254.:80", "403 writ egress refused 169.254.169.254:80: not in the list"},
		{"[fe80::1%25eth0]:443", "403 writ egress refused (not a host name):443: not a host name or IP address"},
		{"[fd00:ec2::254]:80", "403 writ egress refused [fd00:ec2::254]:80: the instance metadata address"},
		{"[::]:443", "403 writ egress refused [::]:443: an unspecified address"},
		{"0.0.0.0:443", "403 writ egress refused 0.0.0.0:443: an unspecified address"},
		{"0.1.2.3:443", "403 writ egress refused 0.1.2.3:443: an unspecified address"},
		{"AMAZONAWS.COM:443", "403 writ egress refused amazonaws.com:443: not in the list"},
		{"evil-amazonaws.com:443", "403 writ egress refused evil-amazonaws.com:443: not in the list"},
		{"amazonaws.com:443", "403 writ egress refused amazonaws.com:443: not in the list"},
		{"x.amazonaws.com:443:80", "400 Bad Request"},
		{"x.amazonaws.com:0", "400 Bad Request"},
		{"x.amazonaws.com:65536", "400 Bad Request"},
		{"x.amazonaws.com:-1", "400 Bad Request"},
		{"xn--80ak6aa92e.com:443", "403 writ egress refused xn--80ak6aa92e.com:443: not in the list"},
		{"allowed.exаmple.com:443", "400 Bad Request: malformed Host header"},
		{"mix.amazonaws.com:443", "403 writ egress refused mix.amazonaws.com:443: " + reasonPrivate},
		{"meta.amazonaws.com:443", "403 writ egress refused meta.amazonaws.com:443: resolves to a link-local address, where the instance metadata service is"},
		{"nat64.amazonaws.com:443", "403 writ egress refused nat64.amazonaws.com:443: resolves to a NAT64 address of a link-local address, where the instance metadata service is"},
		{"nat64p.amazonaws.com:443", "403 writ egress refused nat64p.amazonaws.com:443: " + reasonPrivate},
		{"nat64l.amazonaws.com:443", "403 writ egress refused nat64l.amazonaws.com:443: resolves to a NAT64 address of a link-local address, where the instance metadata service is"},
		{"nat64o.amazonaws.com:443", "403 writ egress refused nat64o.amazonaws.com:443: resolves to a NAT64 address of a loopback address"},
		{"allowed.example.com.evil.org:443", "403 writ egress refused allowed.example.com.evil.org:443: not in the list"},
		{"x.amazonaws.com%00.evil.org:443", "400 Bad Request"},
		// A listed name, however it is spelled, reaches the address it
		// resolves to.
		{"X.AMAZONAWS.COM.:443", "200 Connection established"},
		{"x.amazonaws.com:0443", "200 Connection established"},
	} {
		c2, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(c2, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", c.target, c.target)
		resp, err := http.ReadResponse(bufio.NewReader(c2), &http.Request{Method: http.MethodConnect})
		c2.Close()
		if err != nil {
			t.Errorf("CONNECT %q: %v", c.target, err)
			continue
		}
		if resp.Status != c.status {
			t.Errorf("CONNECT %q answered %q, want %q", c.target, resp.Status, c.status)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"52.1.1.1:443", "52.1.1.1:443"}; !slices.Equal(dialed, want) {
		t.Errorf("the proxy dialed %v, want %v", dialed, want)
	}
	if n := res.lookups("169.254.169.254") + res.lookups("2852039166"); n != 0 {
		t.Errorf("the proxy looked up an address %d times", n)
	}
}

// Plain HTTP with a hostile Host, userinfo, a mapped address, an https
// URL, and requests pipelined or smuggled behind an allowed one: the
// origin sees only the allowed host, with no X-Forwarded-For, and the
// proxy dials only its address.
func TestPlainHTTPHostile(t *testing.T) {
	var mu sync.Mutex
	var seen, dialed []string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Host+" xff="+r.Header.Get("X-Forwarded-For"))
		mu.Unlock()
		fmt.Fprint(w, "ok")
	}))
	defer origin.Close()
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		mu.Lock()
		dialed = append(dialed, address)
		mu.Unlock()
		var d net.Dialer
		return d.DialContext(ctx, network, origin.Listener.Addr().String())
	}
	res := &stubResolver{answers: map[string][][]string{"allowed.example.com": {{"198.51.100.9"}}}}
	p, err := New(Options{Mode: Enforce, List: mustList(t, "allowed.example.com:80"), Resolver: res, Dial: dial})
	if err != nil {
		t.Fatal(err)
	}
	addr := serveProxy(t, p)
	cases := []struct {
		raw  string
		want []string
	}{
		{"GET http://allowed.example.com/a HTTP/1.1\r\nHost: evil.example.org\r\nX-Forwarded-For: 192.0.2.1\r\n\r\n", []string{"HTTP/1.1 200 OK"}},
		{"GET / HTTP/1.1\r\nHost: allowed.example.com\r\n\r\n", []string{"HTTP/1.1 400 Bad Request"}},
		{"GET http://allowed.example.com@169.254.169.254/ HTTP/1.1\r\nHost: allowed.example.com\r\n\r\n", []string{"HTTP/1.1 403 Forbidden"}},
		{"GET http://[::ffff:169.254.169.254]/ HTTP/1.1\r\nHost: x\r\n\r\n", []string{"HTTP/1.1 403 Forbidden"}},
		{"GET https://allowed.example.com/ HTTP/1.1\r\nHost: x\r\n\r\n", []string{"HTTP/1.1 400 Bad Request"}},
		{"GET http://allowed.example.com/ HTTP/1.1\r\nHost: x\r\n\r\nGET http://169.254.169.254/latest HTTP/1.1\r\nHost: allowed.example.com\r\n\r\n", []string{"HTTP/1.1 200 OK"}},
		{"POST http://allowed.example.com/ HTTP/1.1\r\nHost: x\r\nContent-Length: 4\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\nGET http://169.254.169.254/ HTTP/1.1\r\nHost: x\r\n\r\n", []string{"HTTP/1.1 200 OK"}},
	}
	t.Run("each", func(t *testing.T) {
		for _, c := range cases {
			t.Run(strings.SplitN(c.raw, "\r\n", 2)[0], func(t *testing.T) {
				t.Parallel()
				conn, err := net.Dial("tcp", addr)
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				fmt.Fprint(conn, c.raw)
				_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
				b, _ := io.ReadAll(conn)
				var lines []string
				for l := range strings.SplitSeq(string(b), "\r\n") {
					if strings.HasPrefix(l, "HTTP/1.1 ") {
						lines = append(lines, l)
					}
				}
				if !slices.Equal(lines, c.want) {
					t.Errorf("answered %q, want %q", lines, c.want)
				}
			})
		}
	})
	mu.Lock()
	defer mu.Unlock()
	if want := slices.Repeat([]string{"allowed.example.com xff="}, 3); !slices.Equal(seen, want) {
		t.Errorf("the origin saw %q, want %q", seen, want)
	}
	if want := slices.Repeat([]string{"198.51.100.9:80"}, 3); !slices.Equal(dialed, want) {
		t.Errorf("the proxy dialed %v, want %v", dialed, want)
	}
}
