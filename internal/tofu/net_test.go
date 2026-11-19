package tofu

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// countingListener counts the connections it accepted that are still
// open.
type countingListener struct {
	net.Listener
	open atomic.Int64
}

type countedConn struct {
	net.Conn
	l    *countingListener
	done atomic.Bool
}

func (c *countedConn) Close() error {
	if c.done.CompareAndSwap(false, true) {
		c.l.open.Add(-1)
	}
	return c.Conn.Close()
}

func (l *countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.open.Add(1)
	return &countedConn{Conn: c, l: l}, nil
}

// A run's port holds at most maxPortConns connections open, however many
// the run makes: writ closes each past that at once. A connection that
// sent a request, got its answer, and idles is closed after portIdle, and
// then the port serves new ones again.
func TestRunPortBoundsConnections(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cl := &countingListener{Listener: ln}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", http.StatusBadRequest) })
	n := &Net{Ports: map[int]http.Handler{3128: h}}
	stop := n.serve([]net.Listener{cl}, []int{3128})
	defer stop()
	request := func() (net.Conn, bool) {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		fmt.Fprint(c, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, err = http.ReadResponse(bufio.NewReader(c), nil)
		return c, err == nil
	}
	const extra = 64
	answered := 0
	for range maxPortConns + extra {
		c, ok := request()
		defer c.Close()
		if ok {
			answered++
		}
		if open := cl.open.Load(); open > maxPortConns {
			t.Fatalf("writ holds %d connections open on one port, more than %d", open, maxPortConns)
		}
	}
	if answered != maxPortConns {
		t.Errorf("the port answered %d of %d connections, want %d", answered, maxPortConns+extra, maxPortConns)
	}
	deadline := time.Now().Add(portIdle + 5*time.Second)
	for cl.open.Load() > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("after %s idle, writ still holds %d connections open", portIdle+5*time.Second, cl.open.Load())
		}
		time.Sleep(100 * time.Millisecond)
	}
	if c, ok := request(); !ok {
		t.Error("once the idle connections closed, the port did not answer a new one")
	} else {
		c.Close()
	}
}
