package ui_test

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"golang.org/x/net/html"

	"github.com/mr-karan/writ/internal/ui"
)

// traffic is every answer a test's clients got from the control UI, and
// every template the UI rendered. A leak check reads every answer, and
// covered fails it when a route or a template is missing, so a new page
// or part of one cannot escape it.
type traffic struct {
	routes, templates []string

	mu       sync.Mutex
	asked    []*http.Request
	answers  []string
	rendered map[string]bool
	// fetched are the answers writ serve gave the handler of each request
	// that carries pageHeader, by its value.
	fetched map[string][][]byte
	pages   int
}

// pageHeader carries the id of a test's request for a page, by which
// the test gets the documents the page's handler read.
const pageHeader = "X-Writ-Test-Page"

// watch traces u's templates. Call it before u serves.
func watch(t *testing.T, u *ui.UI) *traffic {
	t.Helper()
	tr := &traffic{routes: u.Routes(), rendered: map[string]bool{}, fetched: map[string][][]byte{}}
	u.TraceFetches(func(r *http.Request, body []byte) {
		if id := r.Header.Get(pageHeader); id != "" {
			tr.mu.Lock()
			defer tr.mu.Unlock()
			tr.fetched[id] = append(tr.fetched[id], body)
		}
	})
	templates, err := u.TraceTemplates(func(name string) {
		tr.mu.Lock()
		defer tr.mu.Unlock()
		tr.rendered[name] = true
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.routes) == 0 || len(templates) == 0 {
		t.Fatalf("the UI has %d routes and %d templates", len(tr.routes), len(templates))
	}
	tr.templates = templates
	return tr
}

// transport records each exchange with the server at base that goes
// through next.
func (tr *traffic) transport(base string, next http.RoundTripper) http.RoundTripper {
	u, err := url.Parse(base)
	if err != nil {
		panic(err)
	}
	return recorder{tr: tr, host: u.Host, next: next}
}

type recorder struct {
	tr   *traffic
	host string
	next http.RoundTripper
}

func (r recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	// A client sends a redirect's next request with the same headers, so
	// each request starts its page's documents afresh.
	if id := req.Header.Get(pageHeader); id != "" {
		r.tr.mu.Lock()
		delete(r.tr.fetched, id)
		r.tr.mu.Unlock()
	}
	resp, err := r.next.RoundTrip(req)
	if err != nil || req.URL.Host != r.host {
		return resp, err
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	r.tr.mu.Lock()
	defer r.tr.mu.Unlock()
	r.tr.asked = append(r.tr.asked, &http.Request{Method: req.Method, URL: req.URL, Host: req.URL.Host})
	r.tr.answers = append(r.tr.answers, string(body))
	return resp, nil
}

// page marks req as a request for a page, and returns what gets the
// documents its handler read, once the answer came.
func (tr *traffic) page(req *http.Request) func() [][]byte {
	tr.mu.Lock()
	tr.pages++
	id := strconv.Itoa(tr.pages)
	tr.mu.Unlock()
	req.Header.Set(pageHeader, id)
	return func() [][]byte {
		tr.mu.Lock()
		defer tr.mu.Unlock()
		docs := tr.fetched[id]
		delete(tr.fetched, id)
		return docs
	}
}

// bodies are the answers so far, headers aside.
func (tr *traffic) bodies() []string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return slices.Clone(tr.answers)
}

// covered fails t for each route no request reached and each template
// that never rendered.
func (tr *traffic) covered(t *testing.T) {
	t.Helper()
	mux := http.NewServeMux()
	for _, p := range tr.routes {
		mux.Handle(p, http.NotFoundHandler())
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	reached := map[string]bool{}
	for _, req := range tr.asked {
		_, p := mux.Handler(req)
		reached[p] = true
	}
	for _, p := range tr.routes {
		if !reached[p] {
			t.Errorf("no request of the leak check reached the route %q", p)
		}
	}
	for _, name := range tr.templates {
		if !tr.rendered[name] {
			t.Errorf("the leak check never rendered the template %q", name)
		}
	}
}

// staticLinks are the paths of the stylesheets and scripts a page loads.
func staticLinks(doc *html.Node) []string {
	var out []string
	each(doc, func(n *html.Node) {
		for _, key := range []string{"href", "src"} {
			if v, ok := attr(n, key); ok && (n.Data == "link" || n.Data == "script") && strings.HasPrefix(v, "/ui/static/") {
				out = append(out, v)
			}
		}
	})
	return out
}
