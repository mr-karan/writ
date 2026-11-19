package tofu

import (
	"context"
	"errors"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"slices"
	"sync"
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

const (
	// maxPortConns bounds the connections writ holds open on each port of
	// a run: past it, writ closes a new connection at once. The egress
	// proxy's bound of its requests at once is within it.
	maxPortConns = 256
	// portIdle closes a connection that waits this long for a request's
	// header, or after an answer for its next request.
	portIdle = 10 * time.Second
)

// ports are n's ports in order, as the view makes their listeners.
func (n *Net) ports() []int { return slices.Sorted(maps.Keys(n.Ports)) }

// serve serves each of lns, the listener of ports[i], with its handler,
// until the stop it returns is called. Each holds at most maxPortConns
// connections open, and closes one idle for portIdle. stop cancels the context of every
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
			ReadHeaderTimeout: portIdle,
			IdleTimeout:       portIdle,
			MaxHeaderBytes:    64 << 10,
			BaseContext:       func(net.Listener) context.Context { return ctx },
			ErrorLog:          log.New(io.Discard, "", 0),
		}
		srvs = append(srvs, srv)
		bounded := &boundedListener{Listener: ln, slots: make(chan struct{}, maxPortConns)}
		go func() { _ = srv.Serve(bounded) }()
	}
	return func() {
		cancel()
		for _, srv := range srvs {
			_ = srv.Close()
		}
	}
}

// boundedListener closes each connection it accepts past cap(slots) open
// at once. A connection, tunneled by the proxy or not, frees its slot
// when it closes.
type boundedListener struct {
	net.Listener
	slots chan struct{}
}

func (l *boundedListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.slots <- struct{}{}:
			return &slotConn{Conn: c, free: sync.OnceFunc(func() { <-l.slots })}, nil
		default:
			c.Close()
		}
	}
}

// slotConn is a connection that holds a slot of a boundedListener.
type slotConn struct {
	net.Conn
	free func()
}

func (c *slotConn) Close() error {
	err := c.Conn.Close()
	c.free()
	return err
}

// CloseWrite passes a tunnel's half close on to the connection.
func (c *slotConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return errors.ErrUnsupported
}
