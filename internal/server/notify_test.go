package server_test

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/control/controltest"
	"github.com/mr-karan/writ/internal/diag"
	"github.com/mr-karan/writ/internal/forge/forgetest"
	"github.com/mr-karan/writ/internal/notify"
	"github.com/mr-karan/writ/internal/server"
	"github.com/mr-karan/writ/internal/tofu"
)

func TestNotifyConfig(t *testing.T) {
	base := `
url      = "https://writ.example.com"
listen   = "127.0.0.1:8443"
database = "postgres://writ@localhost/writ"
provider_uids = "61000-61063"

tofu {
  binary       = "/usr/bin/tofu"
  plugin_cache = "/var/cache/writ/plugins"
  scratch_dir  = "/var/tmp/writ/runs"
}

drift {
  every = "6h"
}

issuer "gitlab" {
  kind     = "gitlab-ci"
  identity = "gitlab"
  url      = "https://gitlab.example.com"
}

team "eng" {
  issuer = "gitlab"
  match  = { project_id = ["4242"] }
}

env "prod" {
  key = "keys/prod.key"
}

env "staging" {
  key = "keys/staging.key"
}

grant {
  teams  = ["eng"]
  stacks = ["*"]
  envs   = ["*"]
  can    = ["read", "plan"]
}
`

	t.Run("slack and webhook", func(t *testing.T) {
		cfg, diags := server.ParseConfig("/etc/writ/serve", []byte(base+`
notify "ops-slack" {
  kind     = "slack"
  url_file = "secrets/slack_url"
  events   = ["drift", "plan-failed", "apply-failed", "applied", "outside-write", "approval-needed"]
  envs     = ["prod"]
  stacks   = ["payments/**"]
}

notify "pager" {
  kind        = "webhook"
  url_file    = "secrets/pager_url"
  secret_file = "secrets/pager_secret"
  events      = ["drift", "apply-failed"]
  drift_mode  = "every-check"
}
`))
		if diags.HasErrors() {
			t.Fatal(diags)
		}
		want := []server.Notifier{
			{Name: "ops-slack", Kind: notify.KindSlack, URLFile: "/etc/writ/secrets/slack_url", Events: notify.Events, Envs: []string{"prod"}, Stacks: []string{"payments/**"}, DriftMode: notify.DriftOnChange},
			{Name: "pager", Kind: notify.KindWebhook, URLFile: "/etc/writ/secrets/pager_url", SecretFile: "/etc/writ/secrets/pager_secret",
				Events: []notify.EventKind{notify.EventDrift, notify.EventApplyFailed}, DriftMode: notify.DriftEveryCheck},
		}
		if !reflect.DeepEqual(cfg.Notifiers, want) {
			t.Errorf("notifiers = %+v\nwant %+v", cfg.Notifiers, want)
		}
	})

	for _, tc := range []struct {
		name, attrs, want string
	}{
		{"name", "label=Ops", "lowercase letters"},
		{"no kind", "-kind", "needs kind"},
		{"kind", `kind = "email"`, "not slack or webhook"},
		{"no url_file", "-url_file", "needs url_file"},
		{"secret_file on slack", `kind = "slack"` + "\n" + `secret_file = "s"`, "secret_file goes with kind webhook"},
		{"no events", "-events", "needs events"},
		{"empty events", `events = []`, "events needs at least one"},
		{"unknown event", `events = ["bogus"]`, `"bogus" is not one of drift, plan-failed`},
		{"event twice", `events = ["drift", "drift"]`, "lists drift twice"},
		{"unknown env", `envs = ["dev"]`, "names env dev, which no env block defines"},
		{"empty envs", `envs = []`, "envs needs at least one env"},
		{"stack glob", `stacks = ["a/**/b/**"]`, `the stack pattern "a/**/b/**" holds ** more than once`},
		{"empty stacks", `stacks = []`, "stacks needs at least one pattern"},
		{"drift_mode", `drift_mode = "always"`, "not on-change or every-check"},
		{"drift_mode without drift", `drift_mode = "every-check"` + "\n" + `events = ["applied"]`, "sets drift_mode, and its events do not list drift"},
		{"unknown attribute", `url = "https://hooks.example.com"`, "url"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attrs := map[string]string{"kind": `kind = "webhook"`, "url_file": `url_file = "u"`, "events": `events = ["drift"]`}
			label := "ops"
			for _, line := range strings.Split(tc.attrs, "\n") {
				switch {
				case strings.HasPrefix(line, "label="):
					label = strings.TrimPrefix(line, "label=")
				case strings.HasPrefix(line, "-"):
					delete(attrs, line[1:])
				default:
					attrs[strings.Fields(line)[0]] = line
				}
			}
			block := "notify \"" + label + "\" {\n"
			for _, k := range slices.Sorted(maps.Keys(attrs)) {
				block += "  " + attrs[k] + "\n"
			}
			_, diags := server.ParseConfig("/etc/writ/serve", []byte(base+block+"}\n"))
			if !diags.HasErrors() {
				t.Fatalf("%s parsed", block)
			}
			if !strings.Contains(diagText(diags), tc.want) {
				t.Errorf("errors = %v, want %q", diags, tc.want)
			}
		})
	}

	t.Run("drift without a drift block", func(t *testing.T) {
		_, diags := server.ParseConfig("/etc/writ/serve", []byte(strings.Replace(base, "drift {\n  every = \"6h\"\n}\n", "", 1)+`
notify "ops" {
  kind     = "slack"
  url_file = "u"
  events   = ["drift"]
}
`))
		if diags.HasErrors() || !strings.Contains(diagText(diags), "notify ops subscribes to drift, and this config has no drift block") {
			t.Errorf("diags = %v", diags)
		}
	})
}

func TestLoadNotifiers(t *testing.T) {
	cfg := func(url, secret string) *server.Config {
		n := server.Notifier{Name: "ops", Kind: notify.KindWebhook, URLFile: writeSecretFile(t, "url", url), Events: []notify.EventKind{notify.EventApplied}}
		if secret != "-" {
			n.SecretFile = writeSecretFile(t, "secret", secret)
		}
		return &server.Config{Notifiers: []server.Notifier{n}}
	}
	for _, tc := range []struct {
		name, url, secret, want string
	}{
		{"https", "https://hooks.example.com/T000/B000/XXXX\n", "-", ""},
		{"loopback http", "http://127.0.0.1:9/hook", "s3cret\n", ""},
		{"http elsewhere", "http://hooks.example.com/T000/B000/XXXX", "-", "must use https"},
		{"userinfo", "https://user:pass@hooks.example.com/x", "-", "must not contain userinfo"},
		{"empty url", "\n", "-", "webhook url is empty"},
		{"empty secret", "https://hooks.example.com/x", " \n", "secret_file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			targets, err := server.LoadNotifiers(cfg(tc.url, tc.secret))
			switch {
			case tc.want == "" && err != nil:
				t.Fatal(err)
			case tc.want == "":
				if len(targets) != 1 || targets[0].URL != strings.TrimSpace(tc.url) {
					t.Errorf("targets = %+v", targets)
				}
			case err == nil || !strings.Contains(err.Error(), tc.want):
				t.Errorf("err = %v, want %q", err, tc.want)
			default:
				for _, part := range []string{"XXXX", "pass", "/x"} {
					if strings.Contains(err.Error(), part) {
						t.Errorf("the error quotes the URL: %v", err)
					}
				}
			}
		})
	}
	if _, err := server.LoadNotifiers(&server.Config{Notifiers: []server.Notifier{{Name: "ops", URLFile: filepath.Join(t.TempDir(), "missing")}}}); err == nil || !strings.Contains(err.Error(), "notify ops: read url_file") {
		t.Errorf("a missing url_file: %v", err)
	}
}

func diagText(l diag.List) string {
	var b strings.Builder
	for _, d := range l {
		b.WriteString(d.Message + "\n")
	}
	return b.String()
}

type receivedHook struct {
	headers http.Header
	raw     []byte
	event   notify.Event
}

// hookSink is a receiver of notifications that answers status, 200 when 0.
type hookSink struct {
	t      *testing.T
	srv    *httptest.Server
	status atomic.Int32
	mu     sync.Mutex
	got    []receivedHook
	ch     chan receivedHook
}

func newHookSink(t *testing.T) *hookSink {
	t.Helper()
	hs := &hookSink{t: t, ch: make(chan receivedHook, 256)}
	hs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var ev notify.Event
		_ = json.Unmarshal(raw, &ev)
		var slack struct{ Text string }
		if json.Unmarshal(raw, &slack) == nil && strings.HasPrefix(slack.Text, "[") {
			kind, _, _ := strings.Cut(slack.Text[1:], "]")
			ev.Event = notify.EventKind(kind)
		}
		h := receivedHook{headers: r.Header.Clone(), raw: raw, event: ev}
		hs.mu.Lock()
		hs.got = append(hs.got, h)
		hs.mu.Unlock()
		hs.ch <- h
		if code := hs.status.Load(); code != 0 {
			w.WriteHeader(int(code))
		}
	}))
	t.Cleanup(hs.srv.Close)
	return hs
}

// next waits for the next delivery.
func (h *hookSink) next() receivedHook {
	h.t.Helper()
	select {
	case rec := <-h.ch:
		return rec
	case <-time.After(10 * time.Second):
		h.t.Fatal("no notification came in 10 seconds")
		return receivedHook{}
	}
}

// waitFor waits for the next delivery and requires it to be of kind.
func (h *hookSink) waitFor(kind notify.EventKind) receivedHook {
	h.t.Helper()
	rec := h.next()
	if rec.event.Event != kind {
		h.t.Fatalf("got a %s notification, want %s: %s", rec.event.Event, kind, rec.raw)
	}
	return rec
}

// quiet requires no delivery for a moment.
func (h *hookSink) quiet() {
	h.t.Helper()
	select {
	case rec := <-h.ch:
		h.t.Fatalf("an unexpected notification: %s", rec.raw)
	case <-time.After(200 * time.Millisecond):
	}
}

func (h *hookSink) all() []receivedHook {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.got)
}

func writeSecretFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// withReviewers adds the grant by which bob approves and applies app in
// prod.
func withReviewers(text string) string {
	return text + `grant {
  teams = ["reviewers"]
  stacks = ["app"]
  envs = ["prod"]
  can = ["read", "apply"]
  risks = ["create", "update", "replace", "delete", "data-loss"]
}
`
}

// TestNotifyEvents drives each event through its real code path, a plan,
// an apply, a drift check, and a read of a state on S3, and holds each to
// one delivery to each notifier that matches, a signature that verifies, a
// link to its page, and no secret.
func TestNotifyEvents(t *testing.T) {
	controltest.Each(t, func(t *testing.T, e controltest.Engine) {
		secret := "notify-hmac-secret-9876"
		webhook, slack, every, other := newHookSink(t), newHookSink(t), newHookSink(t), newHookSink(t)
		webhookURL := webhook.srv.URL + "/hooks/WEBHOOK_SECRET_PATH"
		urlFile := func(name, u string) string { return writeSecretFile(t, name, u+"\n") }
		notifyHCL := fmt.Sprintf(`
notify "ops-webhook" {
  kind        = "webhook"
  url_file    = %q
  secret_file = %q
  events      = ["drift", "plan-failed", "apply-failed", "applied", "outside-write", "approval-needed"]
  envs        = ["prod"]
  stacks      = ["app"]
}

notify "ops-slack" {
  kind     = "slack"
  url_file = %q
  events   = ["drift", "plan-failed", "apply-failed", "applied", "outside-write", "approval-needed"]
}

notify "drift-every" {
  kind       = "webhook"
  url_file   = %q
  events     = ["drift"]
  drift_mode = "every-check"
}

notify "other-stacks" {
  kind     = "webhook"
  url_file = %q
  events   = ["drift", "plan-failed", "apply-failed", "applied", "outside-write", "approval-needed"]
  envs     = ["*"]
  stacks   = ["other/**"]
}
`, urlFile("webhook_url", webhookURL), writeSecretFile(t, "webhook_secret", secret+"\n"), urlFile("slack_url", slack.srv.URL+"/services/SLACK_SECRET_PATH"),
			urlFile("every_url", every.srv.URL), urlFile("other_url", other.srv.URL))
		runner := &fakeTofu{}
		f := tofuFixtureOn(t, e, runner, `provider_uids = "61000-61063"`+"\n"+notifyHCL, withApprovals(t, withReviewers))
		f.gitlab.Commit(4242, "main", tofuFiles())
		if r := f.do("POST", "/v1/repos", admin(f), repoRequest(nil)); r.status != http.StatusCreated {
			t.Fatalf("add repo: %d %s", r.status, r.body)
		}
		both := func(kind notify.EventKind) receivedHook {
			t.Helper()
			slack.waitFor(kind)
			return webhook.waitFor(kind)
		}

		p1, _ := f.planCommit(tofuFiles(), "mr-1")
		if p1.Plan.Status != "pending" {
			t.Fatalf("plan 1 is %s, want pending", p1.Plan.Status)
		}
		got := both(notify.EventApprovalNeeded)
		ev := got.event
		if ev.Format != notify.FormatNotification || ev.Stack != "app" || ev.Env != "prod" || ev.Plan != p1.Plan.ID || ev.Status != "pending" ||
			ev.Summary != "plan needs 1 approval: 2 changes, risks create, replace" || ev.Counts == nil || ev.Counts.Need != 1 || ev.StartedBy.Kind != "ci" || ev.Server != f.cfg.URL || ev.Server == "" {
			t.Errorf("approval-needed: %s", got.raw)
		}
		ts, err := strconv.ParseInt(got.headers.Get("X-Writ-Timestamp"), 10, 64)
		if err != nil || !notify.Verify([]byte(secret), ts, got.raw, got.headers.Get("X-Writ-Signature-256")) || got.headers.Get("X-Writ-Event") != "approval-needed" {
			t.Errorf("approval-needed headers do not verify: %v", got.headers)
		}

		if r := f.do("POST", "/v1/plans/"+p1.Plan.ID+"/approvals", bob(f), nil); r.status != http.StatusOK {
			t.Fatalf("approve: %d %s", r.status, r.body)
		}
		run1 := f.tofuRun(bob(f), map[string]any{"plan": p1.Plan.ID})
		if run1.Run.Status != "applied" {
			t.Fatalf("run 1 is %s, want applied", run1.Run.Status)
		}
		got = both(notify.EventApplied)
		if ev := got.event; ev.Run != run1.Run.ID || ev.Plan != p1.Plan.ID || ev.Status != "applied" || ev.StartedBy.Kind != "ci" || !strings.HasPrefix(ev.Summary, "applied") {
			t.Errorf("applied: %s", got.raw)
		}

		// A plan whose OpenTofu output quotes the provider_env secret.
		runner.mu.Lock()
		runner.plan = func() (tofu.Plan, error) {
			line := `{"type":"diagnostic","diagnostic":{"severity":"error","summary":"provider error with secret ` + tofuEnvSecret + ` and ` + tofuCanary + `"}}`
			return tofu.Plan{}, &tofu.StepError{Step: "plan", Code: 1, Stdout: []byte(line + "\n")}
		}
		runner.mu.Unlock()
		files := tofuFiles()
		files["infra/app/extra.tf"] = forgetest.File{Content: "# a second commit\n"}
		p2, _ := f.planCommit(files, "mr-2")
		got = both(notify.EventPlanFailed)
		key := p2.Plan.ID
		if key == "" {
			key = p2.Plan.Ref
		}
		if ev := got.event; ev.Plan != key || ev.Status != p2.Plan.Status || ev.Summary != "plan "+p2.Plan.Status+" (usage)" {
			t.Errorf("plan-failed for a %s plan: %s", p2.Plan.Status, got.raw)
		}

		// An apply whose OpenTofu output quotes it.
		runner.mu.Lock()
		runner.plan = nil
		runner.apply = func(context.Context, stateCall) ([]byte, error) {
			line := `{"type":"diagnostic","diagnostic":{"severity":"error","summary":"apply error quoting ` + tofuEnvSecret + `"}}`
			return nil, &tofu.StepError{Step: "apply", Code: 1, Stdout: []byte(line + "\n")}
		}
		runner.mu.Unlock()
		files["infra/app/extra.tf"] = forgetest.File{Content: "# a third commit\n"}
		p3, _ := f.planCommit(files, "mr-3")
		both(notify.EventApprovalNeeded)
		if r := f.do("POST", "/v1/plans/"+p3.Plan.ID+"/approvals", bob(f), nil); r.status != http.StatusOK {
			t.Fatalf("approve 3: %d %s", r.status, r.body)
		}
		run3 := f.tofuRun(bob(f), map[string]any{"plan": p3.Plan.ID})
		if run3.Run.Status != "failed" {
			t.Fatalf("run 3 is %s, want failed", run3.Run.Status)
		}
		got = both(notify.EventApplyFailed)
		if ev := got.event; ev.Run != run3.Run.ID || ev.Status != "failed" || !strings.HasPrefix(ev.Summary, "apply failed") {
			t.Errorf("apply-failed: %s", got.raw)
		}

		// Drift: clean to drifted, drifted again, drifted to clean, clean
		// again. on-change hears two, every-check three.
		runner.mu.Lock()
		runner.apply = nil
		runner.plan = driftPlan(true)
		runner.mu.Unlock()
		look := func() {
			time.Sleep(5 * time.Millisecond)
			f.srv.LookForDrift(time.Millisecond)
		}
		look()
		got = both(notify.EventDrift)
		if ev := got.event; ev.Status != "drifted" || ev.Counts == nil || ev.Counts.Outside != 1 || ev.Counts.Code != 2 || ev.StartedBy.Kind != "drift" ||
			ev.Summary != "drifted: 1 change outside Writ, 2 changes in code" {
			t.Errorf("drift: %s", got.raw)
		}
		every.waitFor(notify.EventDrift)
		look()
		every.waitFor(notify.EventDrift)
		webhook.quiet()
		runner.mu.Lock()
		runner.plan = driftPlan(false)
		runner.mu.Unlock()
		look()
		got = both(notify.EventDrift)
		if ev := got.event; ev.Status != "clean" || ev.Summary != "drift cleared: the state matches the code" {
			t.Errorf("drift cleared: %s", got.raw)
		}
		every.waitFor(notify.EventDrift)
		look()
		every.quiet()
		webhook.quiet()

		// A serial no apply of writ serve wrote is judged once. The reads
		// start after the last apply ended, or the store judges nothing.
		time.Sleep(5 * time.Millisecond)
		since := time.Now().UTC()
		now := since.Add(time.Second)
		observe := func(serial int64) {
			t.Helper()
			if _, err := f.srv.ObserveS3("prod", "app", tofu.Serial{Lineage: "lin-1", Serial: serial}, since, func() time.Time { return now }); err != nil {
				t.Fatal(err)
			}
		}
		observe(1)
		observe(2)
		got = both(notify.EventOutsideWrite)
		if ev := got.event; ev.Counts == nil || ev.Counts.Serial != 2 || ev.StartedBy.Kind != "outside" || ev.Summary != "state serial 2 was written outside Writ" {
			t.Errorf("outside-write: %s", got.raw)
		}
		observe(2)
		webhook.quiet()
		slack.quiet()
		if n := len(other.all()); n != 0 {
			t.Errorf("other-stacks heard %d events of app", n)
		}

		canaries := []string{tofuCanary, tofuEnvSecret, tofuOutside, secret, "WEBHOOK_SECRET_PATH", "SLACK_SECRET_PATH", gitlabToken}
		for _, rec := range slices.Concat(webhook.all(), slack.all(), every.all()) {
			for _, c := range canaries {
				for _, enc := range []string{c, base64.StdEncoding.EncodeToString([]byte(c)), hex.EncodeToString([]byte(c)), url.QueryEscape(c)} {
					if strings.Contains(string(rec.raw), enc) || strings.Contains(fmt.Sprint(rec.headers), enc) {
						t.Errorf("a notification holds %q: %s", c, rec.raw)
					}
				}
			}
		}
		for _, rec := range slack.all() {
			var doc struct {
				Text   string `json:"text"`
				Blocks []struct {
					Type string `json:"type"`
				} `json:"blocks"`
			}
			if err := json.Unmarshal(rec.raw, &doc); err != nil || doc.Text == "" || len(doc.Blocks) != 1 || rec.headers.Get("X-Writ-Signature-256") != "" {
				t.Errorf("a Slack message: %s %v", rec.raw, rec.headers)
			}
		}

		r := f.do("GET", "/v1/config", admin(f), nil)
		if r.status != http.StatusOK {
			t.Fatalf("GET /v1/config: %d %s", r.status, r.body)
		}
		for _, leaked := range slices.Concat(canaries, []string{"webhook_url", "webhook_secret"}) {
			if strings.Contains(string(r.body), leaked) {
				t.Errorf("GET /v1/config holds %q", leaked)
			}
		}
		var doc server.ConfigDoc
		if err := json.Unmarshal(r.body, &doc); err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, n := range doc.Notifiers {
			names = append(names, n.Name)
			if n.Name == "ops-webhook" && (n.Kind != "webhook" || n.Delivered != 8 || n.Failed != 0 || n.LastOKAt == "" || n.Error != "" || n.DriftMode != "on-change") {
				t.Errorf("ops-webhook in GET /v1/config: %+v", n)
			}
		}
		if !slices.Equal(names, []string{"drift-every", "ops-slack", "ops-webhook", "other-stacks"}) {
			t.Errorf("notifiers in GET /v1/config: %v", names)
		}
	})
}

// TestNotifyGivesUp holds a receiver that keeps failing to the retries
// writ serve allows, then to a failure the config document shows.
func TestNotifyGivesUp(t *testing.T) {
	sink := newHookSink(t)
	sink.status.Store(http.StatusBadGateway)
	notifyHCL := fmt.Sprintf(`
notify "flaky" {
  kind     = "webhook"
  url_file = %q
  events   = ["approval-needed"]
}
`, writeSecretFile(t, "url", sink.srv.URL))
	f := tofuFixtureEdit(t, &fakeTofu{}, `provider_uids = "61000-61063"`+"\n"+notifyHCL, withApprovals(t), func(o *server.Options) {
		o.NotifyBackoff = []time.Duration{time.Millisecond, 2 * time.Millisecond}
	})
	f.gitlab.Commit(4242, "main", tofuFiles())
	if r := f.do("POST", "/v1/repos", admin(f), repoRequest(nil)); r.status != http.StatusCreated {
		t.Fatalf("add repo: %d %s", r.status, r.body)
	}
	f.planCommit(tofuFiles(), "mr-1")
	var ids []string
	for range 3 {
		ids = append(ids, sink.waitFor(notify.EventApprovalNeeded).headers.Get("X-Writ-Delivery"))
	}
	sink.quiet()
	if ids[0] == "" || ids[0] != ids[1] || ids[1] != ids[2] {
		t.Errorf("the retries carry deliveries %v, want one id", ids)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var doc server.ConfigDoc
		r := f.do("GET", "/v1/config", admin(f), nil)
		if err := json.Unmarshal(r.body, &doc); err != nil || len(doc.Notifiers) != 1 {
			t.Fatalf("GET /v1/config: %d %s", r.status, r.body)
		}
		n := doc.Notifiers[0]
		if n.Failed == 1 {
			if n.Delivered != 0 || n.Error != "http status 502" || n.LastErrorAt == "" || n.LastEvent != "approval-needed" {
				t.Errorf("flaky: %+v", n)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("flaky never failed: %+v", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestNotifyFullQueueDoesNotBlockPlans holds plans to finishing while the
// one receiver hangs and its queue is full.
func TestNotifyFullQueueDoesNotBlockPlans(t *testing.T) {
	block := make(chan struct{})
	reached := make(chan struct{}, 8)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached <- struct{}{}
		<-block
	}))
	defer slow.Close()
	defer close(block)
	notifyHCL := fmt.Sprintf(`
notify "slow" {
  kind     = "webhook"
  url_file = %q
  events   = ["approval-needed"]
}
`, writeSecretFile(t, "url", slow.URL))
	f := tofuFixtureEdit(t, &fakeTofu{}, `provider_uids = "61000-61063"`+"\n"+notifyHCL, withApprovals(t), func(o *server.Options) {
		o.NotifyQueueSize = 1
		o.NotifyTimeout = time.Minute
	})
	f.gitlab.Commit(4242, "main", tofuFiles())
	if r := f.do("POST", "/v1/repos", admin(f), repoRequest(nil)); r.status != http.StatusCreated {
		t.Fatalf("add repo: %d %s", r.status, r.body)
	}
	for i := range 4 {
		files := tofuFiles()
		files["infra/app/extra.tf"] = forgetest.File{Content: "# plan " + strconv.Itoa(i) + "\n"}
		start := time.Now()
		p, _ := f.planCommit(files, "queue-"+strconv.Itoa(i))
		if p.Plan.Status != "pending" || time.Since(start) > 5*time.Second {
			t.Fatalf("plan %d is %s after %v", i, p.Plan.Status, time.Since(start))
		}
		if i == 0 {
			<-reached
		}
	}
}
