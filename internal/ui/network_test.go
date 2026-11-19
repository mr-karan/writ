package ui_test

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mr-karan/writ/internal/egress"
	"github.com/mr-karan/writ/internal/server"
	"github.com/mr-karan/writ/internal/tofu"
	"golang.org/x/net/html"
)

// Under egress_mode enforce, a plan's page and its run's page show what
// the run asked writ's egress proxy for: each destination with its
// verdict and count, and the env's egress list. The section is folded,
// and open when the proxy refused a destination.
func TestTofuNetworkSection(t *testing.T) {
	x := newTofuUI(t, func(c *server.Config) { c.EgressMode = egress.Enforce })
	var status string
	x.tofu.mu.Lock()
	x.tofu.egress = func(s tofu.Stack) {
		for _, h := range s.Net.Ports {
			if _, ok := h.(*egress.Proxy); !ok {
				continue
			}
			srv := httptest.NewServer(h)
			defer srv.Close()
			c, err := net.Dial("tcp", srv.Listener.Addr().String())
			if err != nil {
				t.Error(err)
				return
			}
			defer c.Close()
			fmt.Fprint(c, "CONNECT registry.example.org:443 HTTP/1.1\r\nHost: registry.example.org:443\r\n\r\n")
			if resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: http.MethodConnect}); err == nil {
				status = resp.Status
			}
		}
	}
	x.tofu.mu.Unlock()
	p := x.plan(second)
	if status != "403 writ egress refused registry.example.org:443: not in the list" {
		t.Fatalf("the run's proxy answered %q", status)
	}
	_, doc := x.page("/ui/plans/" + p.Plan.ID)
	section := networkSection(t, doc)
	for _, want := range []string{"Network 1 egress enforce", "registry.example.org:443 refused not in the list 1", "The env's egress list is empty."} {
		if !strings.Contains(words(section), want) {
			t.Errorf("the plan's network section lacks %q:\n%s", want, words(section))
		}
	}
	if _, open := attr(firstTag(section, "details"), "open"); !open {
		t.Error("a network section with a refusal is folded")
	}

	x.tofu.mu.Lock()
	x.tofu.egress = nil
	x.tofu.mu.Unlock()
	run := x.apply(x.plan(second).Plan.ID)
	_, doc = x.page("/ui/runs/" + run.Run.ID)
	section = networkSection(t, doc)
	if got := words(section); !strings.Contains(got, "Network 0 egress enforce") {
		t.Errorf("the run's network section: %s", got)
	}
	if _, open := attr(firstTag(section, "details"), "open"); open {
		t.Error("a network section with no refusal is open")
	}
}

// networkSection is the card whose summary starts with Network.
func networkSection(t *testing.T, doc *html.Node) *html.Node {
	t.Helper()
	var found *html.Node
	each(doc, func(n *html.Node) {
		if found == nil && n.Type == html.ElementNode && n.Data == "summary" && strings.HasPrefix(words(n), "Network") {
			found = up(n, isTag("section"))
		}
	})
	if found == nil {
		t.Fatal("the page has no network section")
	}
	return found
}

func firstTag(n *html.Node, tag string) *html.Node {
	var found *html.Node
	each(n, func(c *html.Node) {
		if found == nil && c.Type == html.ElementNode && c.Data == tag {
			found = c
		}
	})
	return found
}
