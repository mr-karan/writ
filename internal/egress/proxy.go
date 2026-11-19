package egress

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Verdict is what the proxy made of a destination.
type Verdict string

const (
	// Allowed is a destination the list has.
	Allowed Verdict = "allowed"
	// Unlisted is a destination the list lacks, which report mode let
	// through.
	Unlisted Verdict = "unlisted"
	// Refused is a destination the proxy did not connect to.
	Refused Verdict = "refused"
)

// Destination is one host and port that a run asked the proxy for, the
// verdict it got and why, and how many requests got it.
type Destination struct {
	Host    string  `json:"host"`
	Port    int     `json:"port"`
	Verdict Verdict `json:"verdict"`
	// Reason says why a destination is unlisted or refused.
	Reason string `json:"reason,omitempty"`
	Count  int    `json:"count"`
}

// Resolver looks up a host name's addresses. *net.Resolver is one.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Defaults of Options.
const (
	// DefaultMaxConns bounds the connections a run has open through the
	// proxy at once.
	DefaultMaxConns = 128
	// DefaultIdle ends a connection that carried no byte either way for
	// this long.
	DefaultIdle = 10 * time.Minute
	// DefaultLife ends a connection this long after it opened. A client
	// that holds one open, as an SDK's pool does, connects again.
	DefaultLife = time.Hour
	// dialTimeout bounds each connect to one address.
	dialTimeout = 15 * time.Second
	// maxAddrs is how many of a name's addresses the proxy tries.
	maxAddrs = 4
	// MaxRecorded bounds the destinations a Proxy names. It counts the
	// requests of any more without naming them.
	MaxRecorded = 256
)

// Options configure a Proxy. Mode must be Report or Enforce.
type Options struct {
	Mode Mode
	List List
	// Resolver resolves host names. Nil uses net.DefaultResolver, which
	// is writ serve's own.
	Resolver Resolver
	// Dial connects to an address the proxy checked, written IP:PORT. Nil
	// uses a net.Dialer.
	Dial     func(ctx context.Context, network, address string) (net.Conn, error)
	MaxConns int
	Idle     time.Duration
	Life     time.Duration
}

// Proxy is one run's egress proxy, an http.Handler. It takes CONNECT
// HOST:PORT, for any port, and a request for an absolute http:// URL,
// decides the host and port before it connects, and records each.
type Proxy struct {
	mode    Mode
	list    List
	resolve Resolver
	dial    func(ctx context.Context, network, address string) (net.Conn, error)
	slots   chan struct{}
	idle    time.Duration
	life    time.Duration

	mu         sync.Mutex
	seen       map[Destination]int
	unrecorded int
}

// New returns a run's proxy.
func New(o Options) (*Proxy, error) {
	if o.Mode != Report && o.Mode != Enforce {
		return nil, fmt.Errorf("egress: a proxy runs in %s or %s mode, not %q", Report, Enforce, o.Mode)
	}
	p := &Proxy{mode: o.Mode, list: o.List, resolve: o.Resolver, dial: o.Dial,
		slots: make(chan struct{}, cmp.Or(o.MaxConns, DefaultMaxConns)),
		idle:  cmp.Or(o.Idle, DefaultIdle), life: cmp.Or(o.Life, DefaultLife),
		seen: map[Destination]int{}}
	if p.resolve == nil {
		p.resolve = net.DefaultResolver
	}
	if p.dial == nil {
		p.dial = (&net.Dialer{}).DialContext
	}
	return p, nil
}

// decision is what the proxy does with one destination: its verdict and
// why, and the addresses to connect to when it is not refused.
type decision struct {
	verdict Verdict
	reason  string
	addrs   []netip.Addr
}

// Reasons of a Destination.
const (
	reasonNotHost    = "not a host name or IP address"
	reasonNotListed  = "not in the list"
	reasonPrivate    = "resolves to a private address, which only an item of the exact name allows"
	reasonIPUnlisted = "an IP address the list does not have"
)

// decide decides the destination host:port as the request wrote it. It
// returns the host as the record names it. A refusal comes before any
// lookup, except one that needs the addresses. err is a failed lookup,
// which leaves the verdict but connects nowhere.
func (p *Proxy) decide(ctx context.Context, host string, port uint16) (string, decision, error) {
	refuse := func(why string) decision { return decision{verdict: Refused, reason: why} }
	// unlisted is the verdict of a destination the list lacks.
	unlisted := func(why string, addrs []netip.Addr) decision {
		if p.mode == Enforce {
			return refuse(why)
		}
		return decision{verdict: Unlisted, reason: why, addrs: addrs}
	}
	name := recordedHost(host)
	if a, err := netip.ParseAddr(host); err == nil {
		if a.Zone() != "" {
			return name, refuse(reasonNotHost), nil
		}
		a = a.Unmap()
		if why := never(a); why != "" {
			return name, refuse(why), nil
		}
		if listed, _ := p.list.match("", a, port); !listed {
			return name, unlisted(reasonIPUnlisted, []netip.Addr{a}), nil
		}
		return name, decision{verdict: Allowed, addrs: []netip.Addr{a}}, nil
	}
	if name == notHost {
		return name, refuse(reasonNotHost), nil
	}
	listed, exact := p.list.match(name, netip.Addr{}, port)
	if !listed && p.mode == Enforce {
		// An unlisted name is never looked up, so a lookup cannot carry
		// data out through DNS.
		return name, refuse(reasonNotListed), nil
	}
	addrs, err := p.resolve.LookupNetIP(ctx, "ip", name)
	if err != nil || len(addrs) == 0 {
		d := decision{verdict: Allowed}
		if !listed {
			d = unlisted(reasonNotListed, nil)
		}
		return name, d, fmt.Errorf("look up %s: %w", name, cmp.Or(err, errors.New("no address")))
	}
	private := false
	for i, a := range addrs {
		a = a.Unmap()
		addrs[i] = a
		if why := never(a); why != "" {
			return name, refuse("resolves to " + why), nil
		}
		private = private || isPrivate(a)
	}
	addrs = addrs[:min(len(addrs), maxAddrs)]
	switch {
	case !listed:
		return name, unlisted(reasonNotListed, addrs), nil
	case private && !exact:
		return name, unlisted(reasonPrivate, addrs), nil
	}
	return name, decision{verdict: Allowed, addrs: addrs}, nil
}

// notHost stands in the record for a host that is neither a host name
// nor an IP address, so nothing the run wrote that way is kept.
const notHost = "(not a host name)"

// recordedHost is host as the record names it: an IP address unmapped, a
// host name lowercase with no trailing dot, or notHost.
func recordedHost(host string) string {
	if a, err := netip.ParseAddr(host); err == nil {
		if a.Zone() != "" {
			return notHost
		}
		return a.Unmap().String()
	}
	if name, ok := hostName(host); ok {
		return name
	}
	return notHost
}

// ServeHTTP serves one request of the run.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rawHost, port, ok := target(r)
	if !ok {
		http.Error(w, "writ egress proxy: send CONNECT HOST:PORT, or a request for an absolute http:// URL", http.StatusBadRequest)
		return
	}
	select {
	case p.slots <- struct{}{}:
		defer func() { <-p.slots }()
	default:
		host := recordedHost(rawHost)
		why := fmt.Sprintf("the run has %d connections open through the proxy", cap(p.slots))
		p.record(host, port, decision{verdict: Refused, reason: why})
		answer(w, r, http.StatusServiceUnavailable, fmt.Sprintf("writ egress refused %s: %s", hostPort(host, port), why))
		return
	}
	host, d, err := p.decide(r.Context(), rawHost, port)
	p.record(host, port, d)
	if d.verdict == Refused {
		answer(w, r, http.StatusForbidden, fmt.Sprintf("writ egress refused %s: %s", hostPort(host, port), d.reason))
		return
	}
	if err != nil {
		answer(w, r, http.StatusBadGateway, fmt.Sprintf("writ egress could not look up %s", host))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), p.life)
	defer cancel()
	if r.Method == http.MethodConnect {
		p.connect(ctx, w, r, host, port, d.addrs)
		return
	}
	p.forward(ctx, w, r, host, port, d.addrs)
}

// target is the host and port a request asks for: the authority of a
// CONNECT, or the host of an absolute http:// URL, on port 80 when it
// names none.
func target(r *http.Request) (string, uint16, bool) {
	if r.Method == http.MethodConnect {
		h, p, err := net.SplitHostPort(r.Host)
		if err != nil {
			return "", 0, false
		}
		port, err := parsePort(p)
		return h, port, err == nil
	}
	if r.URL.Scheme != "http" || r.URL.Host == "" {
		return "", 0, false
	}
	if p := r.URL.Port(); p != "" {
		port, err := parsePort(p)
		return r.URL.Hostname(), port, err == nil
	}
	return r.URL.Hostname(), 80, true
}

// connect connects to the first of addrs that answers and tunnels the
// client's connection to it.
func (p *Proxy) connect(ctx context.Context, w http.ResponseWriter, r *http.Request, host string, port uint16, addrs []netip.Addr) {
	up, err := p.dialAny(ctx, addrs, port)
	if err != nil {
		answer(w, r, http.StatusBadGateway, fmt.Sprintf("writ egress could not connect to %s", hostPort(host, port)))
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		up.Close()
		http.Error(w, "writ egress proxy: this connection cannot be tunneled", http.StatusInternalServerError)
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		up.Close()
		return
	}
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		client.Close()
		up.Close()
		return
	}
	// A client may send its first bytes, such as a TLS ClientHello, before
	// it reads the 200, and the server may have read them already.
	if n := buf.Reader.Buffered(); n > 0 {
		early, _ := buf.Reader.Peek(n)
		if _, err := up.Write(early); err != nil {
			client.Close()
			up.Close()
			return
		}
	}
	p.tunnel(ctx, client, up)
}

// forward sends a plain HTTP request on to the first of addrs that
// answers, on a connection of its own, and copies the answer back.
func (p *Proxy) forward(ctx context.Context, w http.ResponseWriter, r *http.Request, host string, port uint16, addrs []netip.Addr) {
	rp := &httputil.ReverseProxy{
		// The request already names its absolute URL, which goes on as it
		// is. ReverseProxy drops the hop-by-hop headers, Proxy-
		// Authorization among them, and adds no X-Forwarded- header.
		Rewrite: func(*httputil.ProxyRequest) {},
		Transport: &http.Transport{
			// No proxy of writ serve's own: the destination is the address
			// that was checked.
			Proxy: nil,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return p.dialAny(ctx, addrs, port)
			},
			DisableKeepAlives:     true,
			ResponseHeaderTimeout: p.idle,
		},
		ErrorLog: log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, fmt.Sprintf("writ egress could not reach %s", hostPort(host, port)), http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(w, r.WithContext(ctx))
}

func (p *Proxy) dialAny(ctx context.Context, addrs []netip.Addr, port uint16) (net.Conn, error) {
	var errs []error
	for _, a := range addrs {
		dctx, cancel := context.WithTimeout(ctx, dialTimeout)
		c, err := p.dial(dctx, "tcp", netip.AddrPortFrom(a, port).String())
		cancel()
		if err == nil {
			return c, nil
		}
		errs = append(errs, err)
	}
	return nil, errors.Join(errs...)
}

// tunnel copies bytes both ways until both sides end, either side fails,
// no byte moved for the idle time, or ctx ends, which the proxy's life
// time and the end of the run's step end. Then it closes both.
func (p *Proxy) tunnel(ctx context.Context, client, up net.Conn) {
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	var once sync.Once
	end := func() {
		once.Do(func() {
			client.Close()
			up.Close()
		})
	}
	done := make(chan struct{})
	go func() {
		t := time.NewTimer(p.idle)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				end()
				return
			case <-t.C:
				quiet := time.Since(time.Unix(0, last.Load()))
				if quiet >= p.idle {
					end()
					return
				}
				t.Reset(p.idle - quiet)
			}
		}
	}()
	var wg sync.WaitGroup
	half := func(dst, src net.Conn) {
		defer wg.Done()
		buf := make([]byte, 32<<10)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				last.Store(time.Now().UnixNano())
				if _, werr := dst.Write(buf[:n]); werr != nil {
					end()
					return
				}
			}
			if errors.Is(err, io.EOF) {
				// Pass the half close on, so the other way can finish.
				if cw, ok := dst.(interface{ CloseWrite() error }); ok && cw.CloseWrite() == nil {
					return
				}
			}
			if err != nil {
				end()
				return
			}
		}
	}
	wg.Add(2)
	go half(up, client)
	go half(client, up)
	wg.Wait()
	close(done)
	end()
}

// answer refuses a request with code and phrase. A CONNECT gets phrase as
// the status line's reason, which a client such as Go's puts in its
// error, so a provider's diagnostic names the refusal.
func answer(w http.ResponseWriter, r *http.Request, code int, phrase string) {
	if r.Method == http.MethodConnect {
		if hj, ok := w.(http.Hijacker); ok {
			if c, _, err := hj.Hijack(); err == nil {
				fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", code, phrase)
				c.Close()
				return
			}
		}
	}
	http.Error(w, phrase, code)
}

// hostPort writes host and port as a destination, with an IPv6 address
// in brackets.
func hostPort(host string, port uint16) string {
	return net.JoinHostPort(host, strconv.Itoa(int(port)))
}

func (p *Proxy) record(host string, port uint16, d decision) {
	k := Destination{Host: host, Port: int(port), Verdict: d.verdict, Reason: d.reason}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.seen[k]; !ok && len(p.seen) >= MaxRecorded {
		p.unrecorded++
		return
	}
	p.seen[k]++
}

// Destinations are the destinations the run asked for so far, by host,
// port, and verdict, and the count of requests past MaxRecorded
// destinations, which none of them names.
func (p *Proxy) Destinations() ([]Destination, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Destination, 0, len(p.seen))
	for d, n := range p.seen {
		d.Count = n
		out = append(out, d)
	}
	slices.SortFunc(out, func(a, b Destination) int {
		return cmp.Or(cmp.Compare(a.Host, b.Host), cmp.Compare(a.Port, b.Port), cmp.Compare(a.Verdict, b.Verdict), cmp.Compare(a.Reason, b.Reason))
	})
	return out, p.unrecorded
}

// Mode is the proxy's mode.
func (p *Proxy) Mode() Mode { return p.mode }
