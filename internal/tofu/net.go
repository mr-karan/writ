package tofu

import (
	"context"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"slices"
	"time"
)

// Net is a run's own network. With it, each step of the run starts in a
// new network namespace that holds only loopback: no other interface, no
// route, and so no DNS server. On 127.0.0.1 of that namespace, writ serves
// each port of Ports with its handler. The step's view makes those
// listeners inside the namespace before tofu starts and hands them to
// writ, which accepts on them from its own namespace, so no process of
// the run relays a connection and nothing of writ's is in the view.
type Net struct {
	Ports map[int]http.Handler
}

// ports are n's ports in order, as the view makes their listeners.
func (n *Net) ports() []int { return slices.Sorted(maps.Keys(n.Ports)) }

// serve serves each of lns, the listener of ports[i], with its handler,
// until the stop it returns is called. stop cancels the context of every
// request, which ends a connection a handler holds, such as a proxy's
// tunnel, and closes the listeners and the other connections. A step's
// processes are gone when it is called: tofu exited, and its pid
// namespace ended every process it left. Errors of a connection are not
// logged, since a run's own bytes cause them.
func (n *Net) serve(lns []net.Listener, ports []int) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	var srvs []*http.Server
	for i, ln := range lns {
		srv := &http.Server{
			Handler:           n.Ports[ports[i]],
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       2 * time.Minute,
			MaxHeaderBytes:    64 << 10,
			BaseContext:       func(net.Listener) context.Context { return ctx },
			ErrorLog:          log.New(io.Discard, "", 0),
		}
		srvs = append(srvs, srv)
		go func() { _ = srv.Serve(ln) }()
	}
	return func() {
		cancel()
		for _, srv := range srvs {
			_ = srv.Close()
		}
	}
}
