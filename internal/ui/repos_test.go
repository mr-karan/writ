package ui_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/server"
	"github.com/mr-karan/writ/internal/ui"
)

// devUI runs a dev control plane with its control UI on loopback HTTP.
func devUI(t *testing.T, setup ...func(*control.Store, *server.Config)) (*server.Server, string) {
	t.Helper()
	ctx := context.Background()
	data := t.TempDir()
	store, err := control.Open(ctx, control.Options{Location: filepath.Join(data, "control.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	ts := httptest.NewUnstartedServer(nil)
	url := "http://" + ts.Listener.Addr().String()
	cfg := server.DevConfig(server.DevOptions{URL: url, Listen: ts.Listener.Addr().String(), Database: filepath.Join(data, "control.db"), Data: data})
	srv, err := server.New(ctx, server.Options{Config: cfg, Store: store, Tofu: &recordedTofu{t: t, rec: second}, DevToken: strings.Repeat("d", 40), DevUser: "tester", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	for _, f := range setup {
		f(store, cfg)
	}
	u, err := ui.New(ui.Options{Server: srv})
	if err != nil {
		t.Fatal(err)
	}
	u.Mount()
	ts.Config.Handler = srv.Handler()
	ts.Start()
	t.Cleanup(ts.Close)
	return srv, url
}

func visit(t *testing.T, c *http.Client, method, target string, form url.Values) (int, string, string) {
	t.Helper()
	var resp *http.Response
	var err error
	if method == http.MethodPost {
		resp, err = c.PostForm(target, form)
	} else {
		resp, err = c.Get(target)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Request.URL.Path, string(b)
}

// devSession is a browser session of the dev user, made with a sign-in
// link.
func devSession(t *testing.T, srv *server.Server) *server.Session {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", srv.DevLink().URL, nil))
	for _, c := range rec.Result().Cookies() {
		if c.Name == "writ_session" && c.Value != "" {
			sess, err := srv.Session(context.Background(), c.Value)
			if err != nil {
				t.Fatal(err)
			}
			return sess
		}
	}
	t.Fatalf("the sign-in link set no session cookie: %d %s", rec.Code, rec.Body)
	return nil
}

func gitHead(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// The dev user signs in with the link writ serve --dev prints, adds the
// Git repository with the form, and sees the stacks found.
func TestDevAddRepository(t *testing.T) {
	srv, base := devUI(t)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	if status, at, body := visit(t, c, "GET", base+"/ui/repos", nil); status != 200 || at != "/ui/login" || !strings.Contains(body, "run <code>writ ui</code>") {
		t.Fatalf("signed out: %d %s %s", status, at, body)
	}
	link := srv.DevLink()
	status, at, body := visit(t, c, "GET", link.URL, nil)
	if status != 200 || at != "/ui/start" || !strings.Contains(body, "Getting started") || !strings.Contains(body, "writ repo add .") {
		t.Fatalf("sign-in: %d %s %s", status, at, body)
	}
	// Getting started is no section of the navigation, so none is current.
	nav, _, _ := strings.Cut(body[strings.Index(body, `<nav class="sections nav"`):], "</nav>")
	if strings.Contains(nav, `aria-current="page"`) {
		t.Errorf("getting started marks a section of the navigation as current:\n%s", body)
	}
	// The link works once.
	other := &http.Client{}
	if status, _, body := visit(t, other, "GET", link.URL, nil); status != http.StatusUnauthorized || !strings.Contains(body, "run writ ui for a new one") {
		t.Fatalf("a spent link: %d %s", status, body)
	}

	dir := tofuStack(t)
	form := url.Values{"name": {"demo"}, "forge": {"dir"}, "project": {dir + "/nope<b>"}, "path": {""}}
	status, at, body = visit(t, c, "POST", base+"/ui/repos/new", form)
	if status != http.StatusNotFound || at != "/ui/repos/new" || !strings.Contains(body, "not in a Git repository") || !strings.Contains(body, "nope&lt;b&gt;") {
		t.Fatalf("a refused add: %d %s %s", status, at, body)
	}
	form.Set("project", dir)
	status, at, body = visit(t, c, "POST", base+"/ui/repos/new", form)
	if status != 200 || at != "/ui/repos/demo" || !strings.Contains(tagless(body), "Stacks found 1") || !strings.Contains(body, "app") {
		t.Fatalf("add: %d %s %s", status, at, body)
	}
	if _, _, body := visit(t, c, "GET", base+"/ui/repos", nil); !strings.Contains(body, `<a href="/ui/repos/demo">demo</a>`) {
		t.Fatalf("repositories: %s", body)
	}
	if _, _, body := visit(t, c, "GET", base+"/ui/start", nil); !strings.Contains(tagless(body), "1 stack, 1 served") {
		t.Fatalf("getting started after the add: %s", body)
	}
	// A plan of the commit shows on the Plans page, under the environment
	// that holds the stack.
	status, resp := srv.Post(httptest.NewRequest("POST", "/", nil), devSession(t, srv), "/v1/plans",
		[]byte(`{"format":"writ.plan-request/v1","stacks":["app"],"source":{"repo":"demo","commit":"`+gitHead(t, dir)+`"}}`))
	if status != http.StatusAccepted {
		t.Fatalf("plan: %d %s", status, resp)
	}
	var created struct{ Plan struct{ Ref string } }
	if err := json.Unmarshal(resp, &created); err != nil {
		t.Fatal(err)
	}
	var finished server.RemotePlan
	for deadline := time.Now().Add(5 * time.Minute); ; {
		status, doc := srv.Call(httptest.NewRequest("GET", "/", nil), devSession(t, srv), "/v1/plans/"+created.Plan.Ref+"?wait=120s")
		if status != http.StatusOK || json.Unmarshal(doc, &finished) != nil || finished.Plan == nil {
			t.Fatalf("wait for plan: %d %s", status, doc)
		}
		if finished.Plan.Status != "planning" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the plan did not finish: %s", doc)
		}
	}
	if finished.Plan.Status != "approved" {
		t.Fatalf("plan finished as %s: %s", finished.Plan.Status, finished.Document)
	}
	status, _, body = visit(t, c, "GET", base+"/ui/plans", nil)
	if status != http.StatusOK || !strings.Contains(body, `href="/ui/plans/`+finished.Plan.ID+`"`) || strings.Contains(body, "status-planning") {
		t.Fatalf("the plan is not on the Plans page: %d %s", status, body)
	}
	// The plan's page says it needs no approval, and the dev user made it,
	// so it has no approve button; the approve form's route still asks the
	// API, which passes a plan that has its approvals.
	_, at, body = visit(t, c, "GET", base+"/ui/plans/"+created.Plan.Ref, nil)
	if !strings.Contains(body, "No approval rule matches this plan.") || strings.Contains(body, "Approve this plan") || !strings.Contains(body, "You made this plan") {
		t.Fatalf("the plan's page: %s %s", at, body)
	}
	id := regexp.MustCompile(`/ui/plans/([0-9a-f]{32})`).FindStringSubmatch(at)
	if id == nil {
		id = regexp.MustCompile(`<code class="id-full">([0-9a-f]{32})</code>`).FindStringSubmatch(body)
	}
	if id == nil {
		t.Fatalf("no plan id on the page: %s", body)
	}
	if status, at, _ := visit(t, c, "POST", base+"/ui/plans/"+id[1]+"/approve", url.Values{}); status != 200 || at != "/ui/plans/"+id[1] {
		t.Fatalf("approve: %d %s", status, at)
	}
	if status, _, body := visit(t, c, "POST", base+"/ui/plans/"+strings.Repeat("0", 32)+"/approve", url.Values{}); status != http.StatusNotFound || !strings.Contains(body, "no plan") {
		t.Fatalf("approve an unknown plan: %d %s", status, body)
	}
}

func TestDevPlansBeforeStatePreparation(t *testing.T) {
	srv, base := devUI(t, func(store *control.Store, cfg *server.Config) {
		ctx := context.Background()
		now := time.Now()
		id, err := store.RecordConfig(ctx, cfg.Text, cfg.URL, "test", &cfg.Policy, now)
		if err != nil {
			t.Fatal(err)
		}
		err = store.CreatePlan(ctx, control.Plan{Ref: "AAAAAAAAAAAAAAAAAAAAAAAAAA", Env: "staging", Stack: "certs", ConfigID: id, RequestedAt: now}, control.TokenUse{}, control.Entry{At: now, Request: "ordering", Kind: "request", Decision: control.Allowed})
		if err != nil {
			t.Fatal(err)
		}
	})
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	visit(t, c, "GET", srv.DevLink().URL, nil)
	status, _, body := visit(t, c, "GET", base+"/ui/plans", nil)
	if status != 200 || !strings.Contains(body, "staging") || !strings.Contains(body, "status-planning") {
		t.Fatalf("plans before state: %d %s", status, body)
	}
}

// tagless is body with its tags dropped, so a check reads the words a
// reader reads.
func tagless(body string) string { return regexp.MustCompile(`<[^>]*>`).ReplaceAllString(body, "") }

// TestRepoWebhook checks that the Repositories page and a repository's
// page show that the repository has a webhook, and when its last delivery
// came, and never the webhook's secret.
func TestRepoWebhook(t *testing.T) {
	f := newFixture(t)
	b := f.browser()
	b.signIn("/ui/repos")
	if p := b.get("/ui/repos"); p.status != 200 || !strings.Contains(text(p.doc), "webhook") || strings.Contains(text(p.doc), "webhook last") {
		t.Fatalf("before a delivery: %d %s", p.status, text(p.doc))
	}
	req, _ := http.NewRequest("POST", f.url+"/v1/hooks/gitlab/infra", strings.NewReader(`{"object_kind":"push"}`))
	req.Header.Set("X-Gitlab-Token", hookSecret)
	req.Header.Set("X-Gitlab-Event", "Push Hook")
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: f.roots}}}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("delivery: %d", resp.StatusCode)
	}
	for _, pg := range []struct{ path, want string }{
		{"/ui/repos", "webhook last"},
		{"/ui/repos/infra", "Webhookset last delivery"},
	} {
		p := b.get(pg.path)
		if p.status != 200 || !strings.Contains(strings.Join(strings.Fields(text(p.doc)), " "), pg.want) || strings.Contains(p.body, hookSecret) {
			t.Fatalf("%s: %d, the page lacks %q: %s", pg.path, p.status, pg.want, text(p.doc))
		}
		checkPage(t, p, "")
	}
}
