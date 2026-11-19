package tofu

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// netProbe runs as a fake tofu inside a step's view. It reports the
// step's interfaces, a GET of each URL in PROBE_GET, a connect to each
// address in PROBE_DIAL, and a listen on each in PROBE_LISTEN, one line
// each, with only the system call's error.
func netProbe() string {
	var b strings.Builder
	ifs, err := net.Interfaces()
	var names []string
	for _, i := range ifs {
		names = append(names, i.Name)
	}
	fmt.Fprintf(&b, "interfaces: %s %v\n", strings.Join(names, ","), err)
	short := func(err error) string {
		if op, ok := errors.AsType[*net.OpError](err); ok {
			return op.Err.Error()
		}
		return err.Error()
	}
	for _, u := range fields(os.Getenv("PROBE_GET")) {
		c := &http.Client{Timeout: 5 * time.Second}
		resp, err := c.Get(u)
		if err != nil {
			fmt.Fprintf(&b, "get %s: %s\n", u, short(err))
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		fmt.Fprintf(&b, "get %s: %d %s\n", u, resp.StatusCode, body)
	}
	for _, a := range fields(os.Getenv("PROBE_DIAL")) {
		c, err := net.DialTimeout("tcp", a, 5*time.Second)
		if err != nil {
			fmt.Fprintf(&b, "dial %s: %s\n", a, short(err))
			continue
		}
		c.Close()
		fmt.Fprintf(&b, "dial %s: ok\n", a)
	}
	for _, a := range fields(os.Getenv("PROBE_LISTEN")) {
		ln, err := net.Listen("tcp", a)
		if err != nil {
			fmt.Fprintf(&b, "listen %s: %s\n", a, short(err))
			continue
		}
		ln.Close()
		fmt.Fprintf(&b, "listen %s: ok\n", a)
	}
	return b.String()
}

func fields(s string) []string { return strings.FieldsFunc(s, func(r rune) bool { return r == ',' }) }

// TestRunNetwork runs a probe in tofu's place, as a run with Net and as
// one without. With Net, each step has only loopback, reaches the port
// writ serves there, and reaches nothing of the host: not a service on
// the host's loopback, and no route out. The port is taken before tofu
// starts. Without Net, the step reaches the host's loopback as before.
func TestRunNetwork(t *testing.T) {
	host, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	go func() {
		for {
			c, err := host.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	var served atomic.Int32
	bridged := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		fmt.Fprintf(w, "bridged %s", r.URL.Path)
	})
	const port = 8181
	probe := []string{
		"PROBE_GET=http://127.0.0.1:8181/creds",
		"PROBE_DIAL=" + host.Addr().String() + ",192.0.2.1:80",
		"PROBE_LISTEN=127.0.0.1:8181",
	}

	t.Run("own network", func(t *testing.T) {
		served.Store(0)
		r := fakeRunner(t, fakeTofu(t, "net"), time.Minute, 1<<20)
		s := fakeStack()
		s.Env = probe
		s.Net = &Net{Ports: map[int]http.Handler{port: bridged}}
		p, err := r.Plan(context.Background(), s)
		if err != nil {
			t.Fatal(err)
		}
		want := "interfaces: lo <nil>\n" +
			"get http://127.0.0.1:8181/creds: 200 bridged /creds\n" +
			"dial " + host.Addr().String() + ": connect: connection refused\n" +
			"dial 192.0.2.1:80: connect: network is unreachable\n" +
			"listen 127.0.0.1:8181: bind: address already in use\n"
		if string(p.Log) != want {
			t.Errorf("the plan step's probe reported\n%s\nwant\n%s", p.Log, want)
		}
		// init, plan, and show each got a namespace and the port.
		if n := served.Load(); n != 3 {
			t.Errorf("the bridged port served %d requests, want one for each of 3 steps", n)
		}
	})

	t.Run("host network", func(t *testing.T) {
		r := fakeRunner(t, fakeTofu(t, "net"), time.Minute, 1<<20)
		s := fakeStack()
		s.Env = []string{"PROBE_DIAL=" + host.Addr().String()}
		p, err := r.Plan(context.Background(), s)
		if err != nil {
			t.Fatal(err)
		}
		if want := "dial " + host.Addr().String() + ": ok\n"; !strings.Contains(string(p.Log), want) {
			t.Errorf("without Net the probe reported\n%s\nwant %q", p.Log, want)
		}
	})
}
