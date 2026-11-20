package egress

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubResolver answers from a table, in turn when a name has several
// answers, and counts the lookups of each name.
type stubResolver struct {
	mu      sync.Mutex
	answers map[string][][]string
	calls   map[string]int
}

func (r *stubResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.calls == nil {
		r.calls = map[string]int{}
	}
	n := r.calls[host]
	r.calls[host]++
	answers := r.answers[host]
	if len(answers) == 0 {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	var out []netip.Addr
	for _, s := range answers[min(n, len(answers)-1)] {
		out = append(out, netip.MustParseAddr(s))
	}
	return out, nil
}

func (r *stubResolver) lookups(host string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[host]
}

func mustList(t *testing.T, items ...string) List {
	t.Helper()
	l, err := ParseList(items)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestDecide(t *testing.T) {
	res := &stubResolver{answers: map[string][][]string{
		"sts.amazonaws.com":      {{"52.94.0.1"}},
		"vpc.amazonaws.com":      {{"10.1.2.3"}},
		"ula.amazonaws.com":      {{"fd12:3456::1"}},
		"cgnat.amazonaws.com":    {{"100.64.0.9"}},
		"db.example.internal":    {{"10.0.0.9"}},
		"rebind.amazonaws.com":   {{"52.1.1.1", "127.0.0.1"}},
		"meta.amazonaws.com":     {{"169.254.169.254"}},
		"meta6.amazonaws.com":    {{"fd00:ec2::254"}},
		"mapped.amazonaws.com":   {{"::ffff:127.0.0.1"}},
		"localhost":              {{"127.0.0.1", "::1"}},
		"evil.example.org":       {{"198.51.100.66"}},
		"internal.example.org":   {{"192.168.1.10"}},
		"many.amazonaws.com":     {{"52.0.0.1", "52.0.0.2", "52.0.0.3", "52.0.0.4", "52.0.0.5", "52.0.0.6"}},
		"nomad.example.internal": {{"10.0.0.20"}},
		"nat64.amazonaws.com":    {{"64:ff9b::a9fe:a9fe"}},
		"nat64p.amazonaws.com":   {{"64:ff9b::a00:5"}},
		"nat64pub.amazonaws.com": {{"64:ff9b::3401:101"}},
		"nat64l.amazonaws.com":   {{"64:ff9b:1::3401:101"}},
	}}
	list := mustList(t, "*.amazonaws.com", "nomad.example.internal:80", "db.example.internal:5432", "203.0.113.7:9000", "localhost", "gone.example.net")
	type want struct {
		host    string
		verdict Verdict
		reason  string
		addrs   []string
		err     bool
		lookup  bool
	}
	cases := []struct {
		mode Mode
		host string
		port uint16
		want want
	}{
		{Enforce, "sts.amazonaws.com", 443, want{"sts.amazonaws.com", Allowed, "", []string{"52.94.0.1"}, false, true}},
		{Enforce, "STS.AmazonAWS.com.", 443, want{"sts.amazonaws.com", Allowed, "", []string{"52.94.0.1"}, false, true}},
		// An unlisted name is refused before any lookup.
		{Enforce, "evil.example.org", 443, want{"evil.example.org", Refused, reasonNotListed, nil, false, false}},
		{Enforce, "sts.amazonaws.com", 80, want{"sts.amazonaws.com", Refused, reasonNotListed, nil, false, false}},
		// A wildcard does not lead into a private network...
		{Enforce, "vpc.amazonaws.com", 443, want{"vpc.amazonaws.com", Refused, reasonPrivate, nil, false, true}},
		{Enforce, "ula.amazonaws.com", 443, want{"ula.amazonaws.com", Refused, reasonPrivate, nil, false, true}},
		{Enforce, "cgnat.amazonaws.com", 443, want{"cgnat.amazonaws.com", Refused, reasonPrivate, nil, false, true}},
		// ...and an item of the exact name does.
		{Enforce, "db.example.internal", 5432, want{"db.example.internal", Allowed, "", []string{"10.0.0.9"}, false, true}},
		{Enforce, "nomad.example.internal", 80, want{"nomad.example.internal", Allowed, "", []string{"10.0.0.20"}, false, true}},
		// A name that resolves to loopback or the metadata service is
		// refused in every mode, even when it is listed.
		{Enforce, "rebind.amazonaws.com", 443, want{"rebind.amazonaws.com", Refused, "resolves to a loopback address", nil, false, true}},
		{Enforce, "mapped.amazonaws.com", 443, want{"mapped.amazonaws.com", Refused, "resolves to a loopback address", nil, false, true}},
		{Enforce, "meta.amazonaws.com", 443, want{"meta.amazonaws.com", Refused, "resolves to a link-local address, where the instance metadata service is", nil, false, true}},
		{Enforce, "meta6.amazonaws.com", 443, want{"meta6.amazonaws.com", Refused, "resolves to the instance metadata address", nil, false, true}},
		{Enforce, "localhost", 443, want{"localhost", Refused, "resolves to a loopback address", nil, false, true}},
		{Report, "localhost", 443, want{"localhost", Refused, "resolves to a loopback address", nil, false, true}},
		// IP addresses.
		{Enforce, "203.0.113.7", 9000, want{"203.0.113.7", Allowed, "", []string{"203.0.113.7"}, false, false}},
		{Enforce, "203.0.113.7", 443, want{"203.0.113.7", Refused, reasonIPUnlisted, nil, false, false}},
		{Enforce, "198.51.100.66", 443, want{"198.51.100.66", Refused, reasonIPUnlisted, nil, false, false}},
		{Report, "198.51.100.66", 443, want{"198.51.100.66", Unlisted, reasonIPUnlisted, []string{"198.51.100.66"}, false, false}},
		{Report, "127.0.0.1", 443, want{"127.0.0.1", Refused, "a loopback address", nil, false, false}},
		{Report, "::ffff:127.0.0.1", 443, want{"127.0.0.1", Refused, "a loopback address", nil, false, false}},
		{Report, "169.254.169.254", 80, want{"169.254.169.254", Refused, "a link-local address, where the instance metadata service is", nil, false, false}},
		{Report, "fd00:ec2::254", 80, want{"fd00:ec2::254", Refused, "the instance metadata address", nil, false, false}},
		{Report, "0.0.0.0", 80, want{"0.0.0.0", Refused, "an unspecified address", nil, false, false}},
		{Report, "fe80::1%eth0", 80, want{notHost, Refused, reasonNotHost, nil, false, false}},
		{Report, "bad host!", 443, want{notHost, Refused, reasonNotHost, nil, false, false}},
		// Report mode lets the unlisted through and says why.
		{Report, "evil.example.org", 443, want{"evil.example.org", Unlisted, reasonNotListed, []string{"198.51.100.66"}, false, true}},
		{Report, "vpc.amazonaws.com", 443, want{"vpc.amazonaws.com", Unlisted, reasonPrivate, []string{"10.1.2.3"}, false, true}},
		{Report, "internal.example.org", 443, want{"internal.example.org", Unlisted, reasonNotListed, []string{"192.168.1.10"}, false, true}},
		// A failed lookup is no allowed destination, and connects nowhere.
		{Enforce, "gone.example.net", 443, want{"gone.example.net", Unresolved, reasonUnresolved, nil, true, true}},
		{Report, "nowhere.example.net", 443, want{"nowhere.example.net", Unresolved, reasonUnlistedUnresolved, nil, true, true}},
		// A NAT64 address is checked by the IPv4 address it embeds.
		{Enforce, "nat64.amazonaws.com", 443, want{"nat64.amazonaws.com", Refused, "resolves to a NAT64 address of a link-local address, where the instance metadata service is", nil, false, true}},
		{Enforce, "nat64p.amazonaws.com", 443, want{"nat64p.amazonaws.com", Refused, reasonPrivate, nil, false, true}},
		{Enforce, "nat64pub.amazonaws.com", 443, want{"nat64pub.amazonaws.com", Allowed, "", []string{"64:ff9b::3401:101"}, false, true}},
		{Enforce, "nat64l.amazonaws.com", 443, want{"nat64l.amazonaws.com", Refused, reasonPrivate, nil, false, true}},
		{Report, "64:ff9b::7f00:1", 443, want{"64:ff9b::7f00:1", Refused, "a NAT64 address of a loopback address", nil, false, false}},
		// Only the first few addresses are tried.
		{Enforce, "many.amazonaws.com", 443, want{"many.amazonaws.com", Allowed, "", []string{"52.0.0.1", "52.0.0.2", "52.0.0.3", "52.0.0.4"}, false, true}},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%s %s:%d", c.mode, c.host, c.port), func(t *testing.T) {
			p, err := New(Options{Mode: c.mode, List: list, Resolver: res})
			if err != nil {
				t.Fatal(err)
			}
			name, _ := hostName(c.host)
			before := res.lookups(name)
			host, d, err := p.decide(context.Background(), c.host, c.port)
			var addrs []string
			for _, a := range d.addrs {
				addrs = append(addrs, a.String())
			}
			got := want{host, d.verdict, d.reason, addrs, err != nil, res.lookups(name) > before}
			if got.host != c.want.host || got.verdict != c.want.verdict || got.reason != c.want.reason || !slices.Equal(got.addrs, c.want.addrs) || got.err != c.want.err || got.lookup != c.want.lookup {
				t.Errorf("decide = %+v (err %v), want %+v", got, err, c.want)
			}
		})
	}
}

// A provider install's proxy has no list: its registry sends it to
// download hosts of the registry's choosing. It allows any public
// destination, on any port, in either mode, and refuses a private one as
// well as what no proxy connects to.
func TestDecideInstall(t *testing.T) {
	res := &stubResolver{answers: map[string][][]string{
		"registry.opentofu.org":  {{"198.51.100.20"}},
		"releases.example.com":   {{"203.0.113.30", "2001:db8::30"}},
		"vpc.example.com":        {{"198.51.100.21", "10.1.2.3"}},
		"ula.example.com":        {{"fd12:3456::1"}},
		"meta.example.com":       {{"169.254.169.254"}},
		"nat64p.example.com":     {{"64:ff9b::a00:5"}},
		"nomad.example.internal": {{"10.0.0.20"}},
	}}
	const private = "resolves to " + reasonInstallPrivate
	for _, mode := range []Mode{Report, Enforce} {
		p, err := New(Options{Mode: mode, Install: true, List: mustList(t, "nomad.example.internal:80"), Resolver: res})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct {
			host    string
			port    uint16
			verdict Verdict
			reason  string
		}{
			{"registry.opentofu.org", 443, Allowed, ""},
			{"releases.example.com", 8443, Allowed, ""},
			{"203.0.113.40", 443, Allowed, ""},
			{"vpc.example.com", 443, Refused, private},
			{"ula.example.com", 443, Refused, private},
			{"nat64p.example.com", 443, Refused, private},
			// The list is no exception for an install.
			{"nomad.example.internal", 80, Refused, private},
			{"10.0.0.5", 443, Refused, reasonInstallPrivate},
			{"meta.example.com", 443, Refused, "resolves to a link-local address, where the instance metadata service is"},
			{"169.254.169.254", 80, Refused, "a link-local address, where the instance metadata service is"},
			{"127.0.0.1", 8080, Refused, "a loopback address"},
			{"gone.example.com", 443, Unresolved, reasonUnresolved},
		} {
			_, d, _ := p.decide(context.Background(), c.host, c.port)
			if d.verdict != c.verdict || d.reason != c.reason {
				t.Errorf("%s: decide(%s:%d) = %s %q, want %s %q", mode, c.host, c.port, d.verdict, d.reason, c.verdict, c.reason)
			}
		}
	}
}

// upstream is a destination for the proxy: an echo server, and a dial
// that sends every address the proxy checked to it and records them.
type upstream struct {
	ln     net.Listener
	mu     sync.Mutex
	dialed []string
}

func newEcho(t *testing.T) *upstream {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return &upstream{ln: ln}
}

func (u *upstream) dial(ctx context.Context, network, address string) (net.Conn, error) {
	u.mu.Lock()
	u.dialed = append(u.dialed, address)
	u.mu.Unlock()
	var d net.Dialer
	return d.DialContext(ctx, network, u.ln.Addr().String())
}

func (u *upstream) addrs() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return slices.Clone(u.dialed)
}

// serveProxy serves p as the run's namespace would, and returns its
// address.
func serveProxy(t *testing.T, p *Proxy) string {
	t.Helper()
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

// connect sends CONNECT target through the proxy at addr and returns the
// connection and the status line's code and reason.
func connect(t *testing.T, addr, target string) (net.Conn, *bufio.Reader, string) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	return c, br, resp.Status
}

func TestConnect(t *testing.T) {
	up := newEcho(t)
	res := &stubResolver{answers: map[string][][]string{"sts.amazonaws.com": {{"52.94.0.1"}}}}
	p, err := New(Options{Mode: Enforce, List: mustList(t, "*.amazonaws.com", "203.0.113.7:5432"), Resolver: res, Dial: up.dial})
	if err != nil {
		t.Fatal(err)
	}
	addr := serveProxy(t, p)

	c, br, status := connect(t, addr, "sts.amazonaws.com:443")
	if status != "200 Connection established" {
		t.Fatalf("CONNECT to a listed host: %s", status)
	}
	fmt.Fprint(c, "ping\n")
	if line, err := br.ReadString('\n'); err != nil || line != "ping\n" {
		t.Fatalf("through the tunnel: %q, %v", line, err)
	}
	// Any port, when the item names it.
	c2, br2, status := connect(t, addr, "203.0.113.7:5432")
	if status != "200 Connection established" {
		t.Fatalf("CONNECT to a listed address and port: %s", status)
	}
	fmt.Fprint(c2, "pong\n")
	if line, err := br2.ReadString('\n'); err != nil || line != "pong\n" {
		t.Fatalf("through the tunnel: %q, %v", line, err)
	}
	_, _, status = connect(t, addr, "evil.example.org:443")
	if status != "403 writ egress refused evil.example.org:443: not in the list" {
		t.Errorf("CONNECT to an unlisted host: %s", status)
	}
	_, _, status = connect(t, addr, "127.0.0.1:5432")
	if status != "403 writ egress refused 127.0.0.1:5432: a loopback address" {
		t.Errorf("CONNECT to loopback: %s", status)
	}
	if got := up.addrs(); !slices.Equal(got, []string{"52.94.0.1:443", "203.0.113.7:5432"}) {
		t.Errorf("the proxy dialed %v, want only the checked addresses", got)
	}
	dests, unrecorded := p.Destinations()
	want := []Destination{
		{Host: "127.0.0.1", Port: 5432, Verdict: Refused, Reason: "a loopback address", Count: 1},
		{Host: "203.0.113.7", Port: 5432, Verdict: Allowed, Count: 1},
		{Host: "evil.example.org", Port: 443, Verdict: Refused, Reason: reasonNotListed, Count: 1},
		{Host: "sts.amazonaws.com", Port: 443, Verdict: Allowed, Count: 1},
	}
	if !slices.Equal(dests, want) || unrecorded != 0 {
		t.Errorf("Destinations = %+v, %d; want %+v", dests, unrecorded, want)
	}
}

// A client such as Go's, and so a provider, puts the refusal in its
// error.
func TestRefusalReachesTheClientsError(t *testing.T) {
	p, err := New(Options{Mode: Enforce, Resolver: &stubResolver{}})
	if err != nil {
		t.Fatal(err)
	}
	proxy, _ := url.Parse("http://" + serveProxy(t, p))
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxy)}, Timeout: 10 * time.Second}
	_, err = c.Get("https://sts.eu-west-1.amazonaws.com/")
	if err == nil || !strings.Contains(err.Error(), "writ egress refused sts.eu-west-1.amazonaws.com:443: not in the list") {
		t.Fatalf("GET through the proxy: %v, want the refusal in the error", err)
	}
}

// The resolver answers a public address once and a private one after, as
// a rebinding name does. The proxy looks the name up once and connects to
// the address it checked.
func TestConnectPinsTheCheckedAddress(t *testing.T) {
	up := newEcho(t)
	res := &stubResolver{answers: map[string][][]string{"rebind.amazonaws.com": {{"52.1.1.1"}, {"10.0.0.5"}}}}
	p, err := New(Options{Mode: Enforce, List: mustList(t, "*.amazonaws.com"), Resolver: res, Dial: up.dial})
	if err != nil {
		t.Fatal(err)
	}
	addr := serveProxy(t, p)
	if _, _, status := connect(t, addr, "rebind.amazonaws.com:443"); status != "200 Connection established" {
		t.Fatalf("first CONNECT: %s", status)
	}
	if _, _, status := connect(t, addr, "rebind.amazonaws.com:443"); status != "403 writ egress refused rebind.amazonaws.com:443: "+reasonPrivate {
		t.Fatalf("second CONNECT, once the name resolves to a private address: %s", status)
	}
	if got := up.addrs(); !slices.Equal(got, []string{"52.1.1.1:443"}) {
		t.Errorf("the proxy dialed %v, want only 52.1.1.1:443", got)
	}
	if n := res.lookups("rebind.amazonaws.com"); n != 2 {
		t.Errorf("%d lookups for two CONNECTs, want one each", n)
	}
}

func TestPlainHTTP(t *testing.T) {
	var got *http.Request
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		fmt.Fprint(w, "origin answered")
	}))
	defer origin.Close()
	var dialed []string
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		dialed = append(dialed, address)
		var d net.Dialer
		return d.DialContext(ctx, network, origin.Listener.Addr().String())
	}
	res := &stubResolver{answers: map[string][][]string{"nomad.example.internal": {{"10.0.0.20"}}}}
	p, err := New(Options{Mode: Enforce, List: mustList(t, "nomad.example.internal:80"), Resolver: res, Dial: dial})
	if err != nil {
		t.Fatal(err)
	}
	proxy, _ := url.Parse("http://" + serveProxy(t, p))
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxy)}, Timeout: 10 * time.Second}
	req, _ := http.NewRequest("GET", "http://nomad.example.internal/v1/jobs?prefix=web", nil)
	req.Header.Set("Proxy-Authorization", "Basic c2VjcmV0")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "origin answered" {
		t.Fatalf("GET through the proxy: %s %q", resp.Status, body)
	}
	if got.Host != "nomad.example.internal" || got.URL.RequestURI() != "/v1/jobs?prefix=web" || got.Header.Get("Proxy-Authorization") != "" || got.Header.Get("X-Forwarded-For") != "" {
		t.Errorf("the origin got Host %q, URI %q, headers %v", got.Host, got.URL.RequestURI(), got.Header)
	}
	if !slices.Equal(dialed, []string{"10.0.0.20:80"}) {
		t.Errorf("the proxy dialed %v", dialed)
	}

	resp, err = c.Get("http://evil.example.org/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "writ egress refused evil.example.org:80: not in the list") {
		t.Errorf("GET of an unlisted host: %s %q", resp.Status, body)
	}
	// A request that is not for a proxy, as one to the port itself.
	resp, err = http.Get("http://" + proxy.Host + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a request with no absolute URL: %s", resp.Status)
	}
	dests, _ := p.Destinations()
	want := []Destination{
		{Host: "evil.example.org", Port: 80, Verdict: Refused, Reason: reasonNotListed, Count: 1},
		{Host: "nomad.example.internal", Port: 80, Verdict: Allowed, Count: 1},
	}
	if !slices.Equal(dests, want) {
		t.Errorf("Destinations = %+v, want %+v", dests, want)
	}
}

func TestReportLetsTheUnlistedThrough(t *testing.T) {
	up := newEcho(t)
	res := &stubResolver{answers: map[string][][]string{"registry.opentofu.org": {{"198.51.100.20"}}}}
	p, err := New(Options{Mode: Report, Resolver: res, Dial: up.dial})
	if err != nil {
		t.Fatal(err)
	}
	addr := serveProxy(t, p)
	for range 3 {
		if _, _, status := connect(t, addr, "registry.opentofu.org:443"); status != "200 Connection established" {
			t.Fatalf("CONNECT in report mode: %s", status)
		}
	}
	if _, _, status := connect(t, addr, "[::1]:443"); status != "403 writ egress refused [::1]:443: a loopback address" {
		t.Errorf("CONNECT to loopback in report mode: %s", status)
	}
	dests, _ := p.Destinations()
	want := []Destination{
		{Host: "::1", Port: 443, Verdict: Refused, Reason: "a loopback address", Count: 1},
		{Host: "registry.opentofu.org", Port: 443, Verdict: Unlisted, Reason: reasonNotListed, Count: 3},
	}
	if !slices.Equal(dests, want) {
		t.Errorf("Destinations = %+v, want %+v", dests, want)
	}
}

func TestTunnelTimeouts(t *testing.T) {
	res := &stubResolver{answers: map[string][][]string{"api.example.com": {{"203.0.113.9"}}}}
	list := mustList(t, "api.example.com")
	closed := func(t *testing.T, c net.Conn, br *bufio.Reader, within time.Duration) {
		t.Helper()
		_ = c.SetReadDeadline(time.Now().Add(within))
		_, err := br.ReadByte()
		if !errors.Is(err, io.EOF) {
			t.Fatalf("the tunnel is still open after %s: %v", within, err)
		}
	}
	t.Run("idle", func(t *testing.T) {
		up := newEcho(t)
		p, _ := New(Options{Mode: Enforce, List: list, Resolver: res, Dial: up.dial, Idle: 300 * time.Millisecond})
		c, br, status := connect(t, serveProxy(t, p), "api.example.com:443")
		if status != "200 Connection established" {
			t.Fatal(status)
		}
		// Bytes keep it open past the idle time...
		for range 6 {
			fmt.Fprint(c, "x")
			if b, err := br.ReadByte(); err != nil || b != 'x' {
				t.Fatalf("while active: %v", err)
			}
			time.Sleep(100 * time.Millisecond)
		}
		// ...and quiet ends it.
		closed(t, c, br, 2*time.Second)
	})
	t.Run("life", func(t *testing.T) {
		up := newEcho(t)
		p, _ := New(Options{Mode: Enforce, List: list, Resolver: res, Dial: up.dial, Life: 400 * time.Millisecond})
		c, br, status := connect(t, serveProxy(t, p), "api.example.com:443")
		if status != "200 Connection established" {
			t.Fatal(status)
		}
		start := time.Now()
		for time.Since(start) < 3*time.Second {
			if _, err := fmt.Fprint(c, "x"); err != nil {
				break
			}
			if _, err := br.ReadByte(); err != nil {
				if since := time.Since(start); since < 400*time.Millisecond {
					t.Fatalf("the tunnel ended after %s, before its life", since)
				}
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("an active tunnel outlived its life")
	})
	t.Run("the step ends", func(t *testing.T) {
		up := newEcho(t)
		p, _ := New(Options{Mode: Enforce, List: list, Resolver: res, Dial: up.dial})
		ctx, cancel := context.WithCancel(context.Background())
		srv := httptest.NewUnstartedServer(p)
		srv.Config.BaseContext = func(net.Listener) context.Context { return ctx }
		srv.Start()
		defer srv.Close()
		c, br, status := connect(t, srv.Listener.Addr().String(), "api.example.com:443")
		if status != "200 Connection established" {
			t.Fatal(status)
		}
		cancel()
		closed(t, c, br, 2*time.Second)
	})
}

func TestConnectionsAreBounded(t *testing.T) {
	up := newEcho(t)
	res := &stubResolver{answers: map[string][][]string{"api.example.com": {{"203.0.113.9"}}}}
	p, _ := New(Options{Mode: Enforce, List: mustList(t, "api.example.com"), Resolver: res, Dial: up.dial, MaxConns: 1})
	addr := serveProxy(t, p)
	c, _, status := connect(t, addr, "api.example.com:443")
	if status != "200 Connection established" {
		t.Fatal(status)
	}
	if _, _, status := connect(t, addr, "api.example.com:443"); status != "503 writ egress refused api.example.com:443: the run has as many connections open through the proxy as it may" {
		t.Fatalf("a second CONNECT past MaxConns: %s", status)
	}
	c.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, _, status := connect(t, addr, "api.example.com:443")
		if status == "200 Connection established" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the first tunnel closed: %s", status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRecordIsBounded(t *testing.T) {
	p, _ := New(Options{Mode: Enforce, Resolver: &stubResolver{}})
	proxy, _ := url.Parse("http://" + serveProxy(t, p))
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxy)}}
	for i := range MaxRecorded + 4 {
		resp, err := c.Get(fmt.Sprintf("http://h%d.example.org/", i))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	resp, err := c.Get("http://h0.example.org/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	dests, unrecorded := p.Destinations()
	if len(dests) != MaxRecorded || unrecorded != 4 {
		t.Fatalf("%d destinations and %d unrecorded, want %d and 4", len(dests), unrecorded, MaxRecorded)
	}
	if i := slices.IndexFunc(dests, func(d Destination) bool { return d.Host == "h0.example.org" }); i < 0 || dests[i].Count != 2 {
		t.Errorf("h0.example.org was not counted twice: %+v", dests)
	}
}

func TestNewRefusesOff(t *testing.T) {
	if _, err := New(Options{Mode: Off}); err == nil {
		t.Fatal("New in off mode: no error")
	}
}
