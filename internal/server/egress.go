package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/mr-karan/writ/internal/awsrole"
	"github.com/mr-karan/writ/internal/diag"
	"github.com/mr-karan/writ/internal/egress"
	"github.com/mr-karan/writ/internal/mask"
	"github.com/mr-karan/writ/internal/tofu"
)

// The ports of 127.0.0.1 that writ serves in the network namespace of a
// run under egress_mode report or enforce. Each step has a namespace of
// its own, so the ports are the same for every run.
const (
	// egressPort is the egress proxy, which HTTP_PROXY and HTTPS_PROXY
	// point to.
	egressPort = 3128
	// credsPort is the credential endpoint of awsrole.Broker.
	credsPort = 3129
	// statePort is the http state backend, /v1/tfstate/ and nothing else.
	statePort = 3130
)

// runLoopback is where a run with its own network reaches what writ
// serves.
const runLoopback = "http://127.0.0.1:"

// egressEnv points every HTTP client of a run at the egress proxy. Both
// spellings are set: curl, which git uses, reads only the lowercase
// http_proxy, and Go reads either. Loopback goes direct, to the
// credential endpoint and the state backend.
var egressEnv = func() []string {
	proxy := runLoopback + strconv.Itoa(egressPort)
	const direct = "127.0.0.1,localhost,::1"
	return []string{
		"HTTP_PROXY=" + proxy, "HTTPS_PROXY=" + proxy, "http_proxy=" + proxy, "https_proxy=" + proxy,
		"NO_PROXY=" + direct, "no_proxy=" + direct,
	}
}()

// credsURL is the credential endpoint as a run with its own network
// reaches it.
var credsURL = runLoopback + strconv.Itoa(credsPort) + awsrole.CredentialsPath

// stateBase is writ serve's URL as a run's state backend reaches it: the
// config's url, or the bridged state port under egress_mode report or
// enforce.
func (s *Server) stateBase() string {
	if !s.cfg.EgressMode.On() {
		return s.cfg.URL
	}
	return runLoopback + strconv.Itoa(statePort)
}

// initEgress makes what the state port serves: the state backend behind
// the audit row, as on writ serve's own listener, and no other route.
func (s *Server) initEgress() {
	if !s.cfg.EgressMode.On() {
		return
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/tfstate/", http.HandlerFunc(s.serveState))
	s.stateOnly = s.audit(mux)
	s.runClients = newLimiter(s.cfg.Limits.RequestsPerAddress, maxBuckets, s.now)
}

// stateHandler is the handler of the state port of run id. Every run
// reaches it from 127.0.0.1, so it counts the run's requests against
// limits.requests_per_address on a limit of the run's own, apart from
// writ serve's own listener and from every other run. A run is no
// trusted proxy, so its X-Forwarded-For is dropped and its audit rows
// show its own address.
func (s *Server) stateHandler(id string) http.Handler {
	h := s.limitBy(s.runClients, func(*http.Request) string { return id }, s.stateOnly)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Del("X-Forwarded-For")
		h.ServeHTTP(w, r)
	})
}

// newProxy is the egress proxy of a run of env, nil under egress_mode
// off.
func (s *Server) newProxy(env string) (*egress.Proxy, error) {
	if !s.cfg.EgressMode.On() {
		return nil, nil
	}
	e, _ := s.envOf(env)
	return egress.New(egress.Options{Mode: s.cfg.EgressMode, List: e.egress})
}

// net is the run's own network, nil when it has none: the egress proxy,
// the state backend, and the credential endpoint when writ serve has one.
func (r *tofuRun) net() *tofu.Net {
	if r.proxy == nil {
		return nil
	}
	n := &tofu.Net{Ports: map[int]http.Handler{egressPort: r.proxy, statePort: r.s.stateHandler(r.spec.id)}}
	if r.s.broker != nil {
		n.Ports[credsPort] = r.s.broker.Handler()
	}
	return n
}

// FormatNetwork is the document of the destinations an OpenTofu run asked
// writ's egress proxy for.
const FormatNetwork = "writ.network/v1"

// NetworkDoc is writ.network/v1: the run's egress mode, its env's egress
// list when it ran, and each destination it asked for. Every host is
// swept by the run's mask, because a run chooses its hosts and can spell
// a secret in one.
type NetworkDoc struct {
	Format       string               `json:"format"`
	Mode         string               `json:"mode"`
	Egress       []string             `json:"egress"`
	Destinations []NetworkDestination `json:"destinations"`
	// Unrecorded counts the requests past the proxy's limit of named
	// destinations.
	Unrecorded int `json:"unrecorded"`
}

// NetworkDestination is one host and port a run asked for, the verdict
// it got, allowed, unlisted, or refused, why, and how many requests got
// it.
type NetworkDestination struct {
	Host    string `json:"host"`
	Port    int    `json:"port"`
	Verdict string `json:"verdict"`
	Reason  string `json:"reason,omitempty"`
	Count   int    `json:"count"`
}

// withheldHost is a host in the record of a run whose mask could not
// read its plan.
const withheldHost = "(withheld)"

// network returns the run's NetworkDoc, nil for a run with no egress
// proxy, and logs one line of it. m sweeps each host. A nil m, of a run
// whose mask could not read its plan, withholds every host, since only
// the plan's marks could sweep one.
func (r *tofuRun) network(m *mask.Mask) *NetworkDoc {
	if r.proxy == nil {
		return nil
	}
	e, _ := r.s.envOf(r.spec.env)
	dests, unrecorded := r.proxy.Destinations()
	doc := &NetworkDoc{Format: FormatNetwork, Mode: string(r.proxy.Mode()), Egress: append([]string{}, e.Egress...), Destinations: []NetworkDestination{}, Unrecorded: unrecorded}
	var line []string
	refused := false
	for _, d := range dests {
		host := withheldHost
		if m != nil {
			host = printable(string(m.Event([]byte(d.Host))))
		}
		doc.Destinations = append(doc.Destinations, NetworkDestination{Host: host, Port: d.Port, Verdict: string(d.Verdict), Reason: d.Reason, Count: d.Count})
		line = append(line, fmt.Sprintf("%s %s x%d", destination(host, d.Port), d.Verdict, d.Count))
		refused = refused || d.Verdict == egress.Refused
	}
	log := r.s.log.Info
	if refused {
		log = r.s.log.Warn
	}
	log("run egress", "run", r.spec.id, "env", r.spec.env, "mode", doc.Mode, "destinations", strings.Join(line, ", "), "unrecorded", unrecorded)
	return doc
}

// raw is the document as the control database keeps it, nil for none.
func (d *NetworkDoc) raw() json.RawMessage {
	if d == nil {
		return nil
	}
	b, _ := json.Marshal(d)
	return b
}

func destination(host string, port int) string {
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return host + ":" + strconv.Itoa(port)
}

// egressDiagnostics say why a failed step may have failed for egress: the
// destinations the proxy refused, with the env's egress list, and a
// connection made around the proxy, which has no route in the run's
// namespace. out is the step's raw output, which is searched and never
// shown.
func egressDiagnostics(doc *NetworkDoc, env string, out ...[]byte) []Diagnostic {
	if doc == nil {
		return nil
	}
	var diags []Diagnostic
	var refused []string
	for _, d := range doc.Destinations {
		if d.Verdict == string(egress.Refused) {
			refused = append(refused, fmt.Sprintf("%s (%s)", destination(d.Host, d.Port), d.Reason))
		}
	}
	if len(refused) > 0 {
		list := "lists nothing"
		if len(doc.Egress) > 0 {
			list = "is [" + strings.Join(doc.Egress, ", ") + "]"
		}
		diags = append(diags, Diagnostic{"error", string(diag.Denied), fmt.Sprintf("writ's egress proxy refused %s; env %s's egress %s, under egress_mode %s: add what the stack needs to the env's egress list", strings.Join(refused, ", "), env, list, doc.Mode)})
	}
	if aroundProxy(out...) {
		diags = append(diags, Diagnostic{"error", string(diag.Denied), "a process of the run connected without writ's egress proxy, and the run's own network namespace has no route and no DNS: under egress_mode " + doc.Mode + ", only a client that honors HTTPS_PROXY, or HTTP_PROXY for plain HTTP, reaches the network"})
	}
	return diags
}

// noRoute matches what Go and libc print for a connection or a DNS
// lookup in a namespace with only loopback.
var noRoute = regexp.MustCompile(`network is unreachable|lookup \S+ on \S+:53|no such host|Could not resolve host`)

func aroundProxy(out ...[]byte) bool {
	for _, o := range out {
		if noRoute.Match(o) {
			return true
		}
	}
	return false
}
