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
	"github.com/mr-karan/writ/internal/runner"
)

// credsURL is the credential endpoint as a run with its own network
// reaches it.
var credsURL = runner.Loopback + strconv.Itoa(runner.CredsPort) + awsrole.CredentialsPath

// stateBase is writ serve's URL as a run's state backend reaches it: the
// config's url, or the bridged state port under egress_mode report or
// enforce.
func (s *Server) stateBase() string {
	if !s.cfg.EgressMode.On() {
		return s.cfg.URL
	}
	return runner.Loopback + strconv.Itoa(runner.StatePort)
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

// relay is how the own network of run id reaches writ serve: the state
// port, and the credential endpoint when writ serve has one.
func (s *Server) relay(id string) (state, creds http.Handler) {
	if s.broker != nil {
		creds = s.broker.Handler()
	}
	return s.stateHandler(id), creds
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
// it got, allowed, unlisted, refused, or unresolved, why, and how many
// requests got it.
type NetworkDestination struct {
	// Step is installStep for a destination of the run's provider
	// install, and empty for one of its tofu steps.
	Step    string `json:"step,omitempty"`
	Host    string `json:"host"`
	Port    int    `json:"port"`
	Verdict string `json:"verdict"`
	Reason  string `json:"reason,omitempty"`
	Count   int    `json:"count"`
}

// withheldHost is a host in the record of a run whose mask could not
// read its plan.
const withheldHost = "(withheld)"

// installStep marks a destination of a run's provider install.
const installStep = "install"

// network returns the NetworkDoc of the run's job, nil for a job with no
// egress proxy, and logs one line of it. m sweeps each host. A nil m, of
// a run whose mask could not read its plan, withholds every host, since
// only the plan's marks could sweep one.
func (r *tofuRun) network(m *mask.Mask) *NetworkDoc {
	n := r.net
	if n == nil {
		return nil
	}
	e, _ := r.s.envOf(r.spec.env)
	doc := &NetworkDoc{Format: FormatNetwork, Mode: string(n.Mode), Egress: append([]string{}, e.Egress...), Destinations: []NetworkDestination{}, Unrecorded: n.Unrecorded}
	var line []string
	refused := false
	record := func(dests []egress.Destination, step string) {
		for _, d := range dests {
			host := withheldHost
			if m != nil {
				host = printable(string(m.Event([]byte(d.Host))))
			}
			doc.Destinations = append(doc.Destinations, NetworkDestination{Step: step, Host: host, Port: d.Port, Verdict: string(d.Verdict), Reason: d.Reason, Count: d.Count})
			line = append(line, strings.TrimSpace(fmt.Sprintf("%s %s %s x%d", step, destination(host, d.Port), d.Verdict, d.Count)))
			refused = refused || d.Verdict == egress.Refused
		}
	}
	// The install runs before every step of the run.
	record(n.Install, installStep)
	record(n.Run, "")
	log := r.s.log.Info
	if refused {
		log = r.s.log.Warn
	}
	log("run egress", "run", r.spec.id, "env", r.spec.env, "mode", doc.Mode, "destinations", strings.Join(line, ", "), "unrecorded", doc.Unrecorded)
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
// destinations the run's proxy refused, with the env's egress list, and a
// connection made around the proxy, which has no route in the run's
// namespace. The env's list has no say over the provider install, so its
// destinations are left out. out is the step's raw output, which is
// searched and never shown.
func egressDiagnostics(doc *NetworkDoc, env string, out ...[]byte) []Diagnostic {
	if doc == nil {
		return nil
	}
	var diags []Diagnostic
	var refused []string
	for _, d := range doc.Destinations {
		if d.Verdict == string(egress.Refused) && d.Step == "" {
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
