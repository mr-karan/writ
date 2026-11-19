package tofu

import (
	"bufio"
	"context"
	"encoding/json"
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
// step's interfaces, a GET of each URL in PROBE_GET, through HTTP_PROXY
// as Go's clients send one, a CONNECT of each host:port in PROBE_CONNECT
// sent to HTTPS_PROXY, a GET of the credential endpoint with its token
// when PROBE_CREDS is set, a connect to each address in PROBE_DIAL, and a
// listen on each in PROBE_LISTEN, one line each, with only the system
// call's error.
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
	for _, target := range fields(os.Getenv("PROBE_CONNECT")) {
		fmt.Fprintf(&b, "connect %s: %s\n", target, connectStatus(strings.TrimPrefix(os.Getenv("HTTPS_PROXY"), "http://"), target))
	}
	if os.Getenv("PROBE_CREDS") != "" {
		req, _ := http.NewRequest("GET", os.Getenv("AWS_CONTAINER_CREDENTIALS_FULL_URI"), nil)
		req.Header.Set("Authorization", os.Getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN"))
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			fmt.Fprintf(&b, "creds: %s\n", short(err))
		} else {
			var doc struct{ AccessKeyId string }
			_ = json.NewDecoder(resp.Body).Decode(&doc)
			resp.Body.Close()
			fmt.Fprintf(&b, "creds: %d %s\n", resp.StatusCode, doc.AccessKeyId)
		}
	}
	for _, a := range fields(os.Getenv("PROBE_DIAL")) {
		c, err := net.DialTimeout("tcp", a, 2*time.Second)
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

// connectStatus sends CONNECT target to the proxy at addr and returns the
// status line, or the error.
func connectStatus(addr, target string) string {
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return err.Error()
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: http.MethodConnect})
	if err != nil {
		return err.Error()
	}
	return resp.Status
}

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
