package server_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"

	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/mr-karan/writ/internal/awsrole"
	"github.com/mr-karan/writ/internal/awsrole/ststest"
	"github.com/mr-karan/writ/internal/forge/forgetest"
	"github.com/mr-karan/writ/internal/server"
	"github.com/mr-karan/writ/internal/tofu"
)

// withEgress sets egress_mode and env prod's egress list, and the tofu
// block egress_mode needs.
func withEgress(mode, list string) func(string) string {
	return func(text string) string {
		text = strings.Replace(text, `  key = "prod.key"`, "  key = \"prod.key\"\n  egress = "+list, 1)
		return text + "tofu {\n  binary = \"tofu\"\n  plugin_cache = \"/var/lib/writ-tofu/plugins\"\n  scratch_dir = \"/var/lib/writ-tofu/runs\"\n}\negress_mode = \"" + mode + "\"\n"
	}
}

// serveRunPort serves h as a port of the run's own network serves it.
func serveRunPort(t *testing.T, h http.Handler) string {
	t.Helper()
	if h == nil {
		t.Fatal("the run's network serves no handler on this port")
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

// connectVia sends CONNECT target to the proxy at addr, as a provider in
// the run's namespace does, and returns the status line.
func connectVia(t *testing.T, addr, target string) string {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	return resp.Status
}

func getVia(t *testing.T, addr, path, token string) int {
	t.Helper()
	req, _ := http.NewRequest("GET", "http://"+addr+path, nil)
	if token != "" {
		req.SetBasicAuth("writ", token)
	}
	req.Header.Set("X-Forwarded-For", "203.0.113.50")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func networkOf(t *testing.T, raw json.RawMessage) server.NetworkDoc {
	t.Helper()
	var doc server.NetworkDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("network %s: %v", raw, err)
	}
	return doc
}

// Under egress_mode enforce, a plan's run gets its own network: the proxy,
// which refuses what env prod's egress lacks, and the state backend on
// ports of its own loopback. A refusal fails the plan with a diagnostic
// that names it and the list, and the plan and its run keep a record of
// each destination, with hosts swept by the mask.
func TestTofuEgressEnforce(t *testing.T) {
	runner := &fakeTofu{}
	f := tofuApplyFixtureEdit(t, runner, func(text string) string {
		// writ serve trusts loopback as a proxy, as behind a local one,
		// and the run's own loopback is no proxy of its.
		return withEgress("enforce", `["*.amazonaws.com", "nomad.example.internal:80"]`)(text) + "trusted_proxies = [\"127.0.0.1/32\"]\n"
	})
	var mu sync.Mutex
	var statuses []string
	var stateGot, otherGot int
	var seen tofu.Stack
	runner.plan = func() (tofu.Plan, error) {
		runner.mu.Lock()
		s := runner.stacks[len(runner.stacks)-1]
		runner.mu.Unlock()
		proxy := serveRunPort(t, s.Net.Ports[3128])
		state := serveRunPort(t, s.Net.Ports[3130])
		mu.Lock()
		defer mu.Unlock()
		seen = s
		for _, target := range []string{"evil.example.org:443", "127.0.0.1:5432", tofuEnvSecret + ".example.org:443", "evil.example.org:443"} {
			statuses = append(statuses, connectVia(t, proxy, target))
		}
		stateGot = getVia(t, state, "/v1/tfstate/app", envValue(s.Env, "TF_HTTP_PASSWORD"))
		otherGot = getVia(t, state, "/v1/plans", "")
		out := []byte(`{"type":"diagnostic","diagnostic":{"severity":"error","summary":"Post \"https://evil.example.org/\": writ egress refused evil.example.org:443: not in the list"}}` + "\n")
		return tofu.Plan{}, &tofu.StepError{Step: "plan", Code: 1, Stdout: out}
	}
	p, _ := f.planCommit(tofuFiles(), "egress")
	mu.Lock()
	defer mu.Unlock()

	if seen.Net == nil || len(seen.Net.Ports) != 2 || seen.Net.Ports[3128] == nil || seen.Net.Ports[3130] == nil {
		t.Fatalf("the run's network %+v, want the proxy and the state backend", seen.Net)
	}
	env := envMap(seen.Env)
	for k, v := range map[string]string{"HTTPS_PROXY": "http://127.0.0.1:3128", "https_proxy": "http://127.0.0.1:3128", "HTTP_PROXY": "http://127.0.0.1:3128",
		"NO_PROXY": "127.0.0.1,localhost,::1", "TF_HTTP_ADDRESS": "http://127.0.0.1:3130/v1/tfstate/app"} {
		if env[k] != v {
			t.Errorf("the run's %s = %q, want %q", k, env[k], v)
		}
	}
	want := []string{
		"403 writ egress refused evil.example.org:443: not in the list",
		"403 writ egress refused 127.0.0.1:5432: a loopback address",
		"403 writ egress refused " + tofuEnvSecret + ".example.org:443: not in the list",
		"403 writ egress refused evil.example.org:443: not in the list",
	}
	if !slices.Equal(statuses, want) {
		t.Errorf("the proxy answered %q, want %q", statuses, want)
	}
	if stateGot != http.StatusNotFound || otherGot != http.StatusNotFound {
		t.Errorf("the state port answered the stack's state with %d and /v1/plans with %d, want 404 for a stack with no state and 404 for a route it does not serve", stateGot, otherGot)
	}

	if p.Plan.Status != "refused" {
		t.Fatalf("plan %+v\n%s", p.Plan, p.Text)
	}
	const refusal = "writ's egress proxy refused 127.0.0.1:5432 (a loopback address), (sensitive).example.org:443 (not in the list), evil.example.org:443 (not in the list); env prod's egress is [*.amazonaws.com, nomad.example.internal:80], under egress_mode enforce"
	if !strings.Contains(p.Text, "error: "+refusal) {
		t.Errorf("the refused plan's text lacks the refusal:\n%s", p.Text)
	}
	net := networkOf(t, p.Network)
	wantDests := []server.NetworkDestination{
		{Host: "127.0.0.1", Port: 5432, Verdict: "refused", Reason: "a loopback address", Count: 1},
		{Host: "(sensitive).example.org", Port: 443, Verdict: "refused", Reason: "not in the list", Count: 1},
		{Host: "evil.example.org", Port: 443, Verdict: "refused", Reason: "not in the list", Count: 2},
	}
	if net.Format != "writ.network/v1" || net.Mode != "enforce" || !slices.Equal(net.Egress, []string{"*.amazonaws.com", "nomad.example.internal:80"}) || !slices.Equal(net.Destinations, wantDests) {
		t.Errorf("the plan's network %+v, want %+v", net, wantDests)
	}
	served := f.do("GET", "/v1/plans/"+p.Plan.Ref, f.token(nil), nil)
	if strings.Contains(string(served.body), tofuEnvSecret) {
		t.Errorf("the plan the API serves holds %s:\n%s", tofuEnvSecret, served.body)
	}
	// The state port's requests are audited with the run's own address,
	// not one its X-Forwarded-For names.
	for _, e := range auditKind(f, "request") {
		if e.Remote == "203.0.113.50" {
			t.Errorf("an audit row of the state port names the address the run sent: %+v", e)
		}
	}

	// A plan that runs, and its apply, keep a record of no destination.
	runner.mu.Lock()
	runner.plan = nil
	runner.mu.Unlock()
	id := approvedTofuPlan(t, f, "clean")
	got := f.do("GET", "/v1/plans/"+id, f.token(nil), nil)
	if n := networkOf(t, json.RawMessage(mustJSON(t, got.doc["network"]))); n.Mode != "enforce" || len(n.Destinations) != 0 {
		t.Errorf("a clean plan's network %+v", n)
	}
	run := f.tofuRun(bob(f), map[string]any{"plan": id})
	if run.Run.Status != "applied" {
		t.Fatalf("run %+v\n%s", run.Run, run.Text)
	}
	if n := networkOf(t, run.Network); n.Mode != "enforce" || len(n.Destinations) != 0 {
		t.Errorf("the run's network %+v", n)
	}
	applied := runner.appliedSoFar()
	if a := applied[len(applied)-1].stack; a.Net == nil || envValue(a.Env, "HTTPS_PROXY") != "http://127.0.0.1:3128" {
		t.Errorf("the apply's run got network %+v and env %v", a.Net, a.Env)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Under egress_mode off a run and its provider install keep the host's
// network, as before egress: no network of their own, no proxy, the state
// backend at writ serve's URL, and no record.
func TestTofuEgressOff(t *testing.T) {
	runner := &fakeTofu{}
	f := tofuApplyFixture(t, runner)
	p, _ := f.planCommit(tofuFiles(), "off")
	if p.Plan.Status != "pending" || string(p.Network) != "null" {
		t.Fatalf("plan %+v, network %s", p.Plan, p.Network)
	}
	if runner.fillNets[0] != nil || runner.fillEnvs[0] != nil {
		t.Fatalf("under egress_mode off the provider install got network %+v and env %v", runner.fillNets[0], runner.fillEnvs[0])
	}
	s := runner.stacks[0]
	if s.Net != nil || envValue(s.Env, "HTTPS_PROXY") != "" || envValue(s.Env, "HTTP_PROXY") != "" || !strings.HasPrefix(envValue(s.Env, "TF_HTTP_ADDRESS"), f.cfg.URL+"/v1/tfstate/") {
		t.Fatalf("under egress_mode off the run got network %+v and env %v", s.Net, s.Env)
	}
}

// Under egress_mode report, the run's own network lets an unlisted host
// through, as the record says.
func TestTofuEgressReport(t *testing.T) {
	runner := &fakeTofu{}
	f := tofuApplyFixtureEdit(t, runner, withEgress("report", `[]`))
	var status string
	runner.plan = func() (tofu.Plan, error) {
		runner.mu.Lock()
		s := runner.stacks[len(runner.stacks)-1]
		runner.mu.Unlock()
		// An IP address needs no lookup, and nothing listens there.
		status = connectVia(t, serveRunPort(t, s.Net.Ports[3128]), "[::1]:9")
		return createOnly()
	}
	p, _ := f.planCommit(tofuFiles(), "report")
	if status != "403 writ egress refused [::1]:9: a loopback address" {
		t.Errorf("report mode answered loopback with %q", status)
	}
	if n := networkOf(t, p.Network); n.Mode != "report" || len(n.Destinations) != 1 || n.Destinations[0].Verdict != "refused" {
		t.Errorf("the plan's network %+v", n)
	}
}

// Under egress_mode enforce, a run's providers still get the role's
// credentials, from the credential endpoint on a port of their own
// loopback, with the run's token.
func TestTofuEgressCredentials(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIACONTROLPLANE0001")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "control-plane-secret")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	st := ststest.New()
	defer st.Close()
	assumer, err := awsrole.NewSTS(context.Background(), awsrole.STSOptions{Region: "eu-west-1", Endpoint: st.URL})
	if err != nil {
		t.Fatal(err)
	}
	const role = "arn:aws:iam::222222222222:role/writ-plan"
	runner := &fakeTofu{}
	var fetched map[string]string
	var credsURL string
	runner.plan = func() (tofu.Plan, error) {
		runner.mu.Lock()
		s := runner.stacks[len(runner.stacks)-1]
		runner.mu.Unlock()
		env := envMap(s.Creds)
		credsURL = env["AWS_CONTAINER_CREDENTIALS_FULL_URI"]
		req, _ := http.NewRequest("GET", "http://"+serveRunPort(t, s.Net.Ports[3129])+"/v1/credentials", nil)
		req.Header.Set("Authorization", env["AWS_CONTAINER_AUTHORIZATION_TOKEN"])
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			return createOnly()
		}
		defer resp.Body.Close()
		_ = json.NewDecoder(resp.Body).Decode(&fetched)
		return createOnly()
	}
	f := tofuFixtureEdit(t, runner, `provider_uids = "61000-61063"`+"\n", func(text string) string {
		text = withForge(t, "")(text)
		text = strings.Replace(text, "key = \"prod.key\"\n", fmt.Sprintf("key = \"prod.key\"\n  plan_role = %q\n", role), 1)
		text = withEgress("enforce", `[]`)(text)
		return text + fmt.Sprintf("aws {\n  region = \"eu-west-1\"\n  sts_endpoint = %q\n}\n", st.URL)
	}, func(o *server.Options) { o.Assumer = assumer })
	files := tofuFiles()
	f.gitlab.Commit(4242, "main", files)
	if r := f.do("POST", "/v1/repos", admin(f), repoRequest(nil)); r.status != http.StatusCreated {
		t.Fatalf("add: %d %s", r.status, r.body)
	}
	p, _ := f.planCommit(files, "creds")
	if p.Plan.Status == "failed" || p.Plan.Status == "refused" {
		t.Fatalf("plan %+v\n%s", p.Plan, p.Text)
	}
	if credsURL != "http://127.0.0.1:3129/v1/credentials" {
		t.Errorf("the run's credential endpoint is %q", credsURL)
	}
	issued, ok := st.Issued(fetched["AccessKeyId"])
	if !ok || issued.SessionName != "writ-ci-"+p.Plan.Ref || fetched["Token"] != issued.SessionToken {
		t.Fatalf("the run's own credential port served %v, which STS did not issue for this run", fetched)
	}

}

// A plan's network record is made after the mask learns the plan's
// marks: a host that spells a variable only the plan marks sensitive is
// swept in the record the API serves and in the run egress log line.
func TestTofuEgressPlanMarks(t *testing.T) {
	var log syncBuffer
	runner := &fakeTofu{}
	f := tofuApplyFixtureEdit(t, runner, withEgress("enforce", `["*.amazonaws.com"]`), func(o *server.Options) {
		o.Log = slog.New(slog.NewTextHandler(&log, nil))
	})
	runner.plan = func() (tofu.Plan, error) {
		runner.mu.Lock()
		s := runner.stacks[len(runner.stacks)-1]
		runner.mu.Unlock()
		connectVia(t, serveRunPort(t, s.Net.Ports[3128]), tofuCanary+".example.org:443")
		return tofu.Plan{File: []byte("saved"), JSON: []byte(tofuPlanJSON)}, nil
	}
	p, _ := f.planCommit(tofuFiles(), "egress-mask")
	if p.Plan.Status != "pending" {
		t.Fatalf("plan %+v\n%s", p.Plan, p.Text)
	}
	want := []server.NetworkDestination{{Host: "(sensitive).example.org", Port: 443, Verdict: "refused", Reason: "not in the list", Count: 1}}
	if n := networkOf(t, p.Network); !slices.Equal(n.Destinations, want) {
		t.Errorf("the plan's network %+v, want %+v", n.Destinations, want)
	}
	if served := f.do("GET", "/v1/plans/"+p.Plan.Ref, f.token(nil), nil); strings.Contains(string(served.body), tofuCanary) {
		t.Errorf("the plan the API serves holds %s:\n%s", tofuCanary, served.body)
	}
	logged := log.String()
	if !strings.Contains(logged, `msg="run egress"`) || strings.Contains(logged, tofuCanary) {
		t.Errorf("writ serve's log, which should hold a run egress line without %s:\n%s", tofuCanary, logged)
	}
}

// The state port counts a run's requests against
// limits.requests_per_address on a limit of its own, keyed by the run.
// Every run reaches it from 127.0.0.1, so a run that floods it is
// refused, and neither writ serve's own listener, which a client on
// loopback reaches, nor another run's state port counts its requests.
func TestTofuEgressStatePortLimit(t *testing.T) {
	var log syncBuffer
	runner := &fakeTofu{}
	f := tofuApplyFixtureEdit(t, runner, func(text string) string {
		return strings.Replace(withEgress("enforce", `[]`)(text), "requests_per_address = 1000", "requests_per_address = 20", 1)
	}, frozen(&log))
	var flood []int
	var own, other int
	runner.plan = func() (tofu.Plan, error) {
		runner.mu.Lock()
		s, n := runner.stacks[len(runner.stacks)-1], len(runner.stacks)
		runner.mu.Unlock()
		state := serveRunPort(t, s.Net.Ports[3130])
		if n > 1 {
			other = getVia(t, state, "/v1/tfstate/app", "not-a-token")
			return createOnly()
		}
		for range 40 {
			flood = append(flood, getVia(t, state, "/v1/tfstate/app", "not-a-token"))
		}
		own = f.do("GET", "/v1/whoami", f.token(nil), nil).status
		return createOnly()
	}
	f.planCommit(tofuFiles(), "flood")
	f.planCommit(tofuFiles(), "after")
	refused := 0
	for _, c := range flood {
		if c == http.StatusTooManyRequests {
			refused++
		}
	}
	if len(flood) != 40 || flood[0] == http.StatusTooManyRequests || refused != 20 {
		t.Errorf("the state port answered %v, want the 20 requests past the run's limit of 20 refused", flood)
	}
	if own != http.StatusOK {
		t.Errorf("after a run's 40 requests to its state port, writ serve's own listener answered a client on loopback %d", own)
	}
	if other == 0 || other == http.StatusTooManyRequests {
		t.Errorf("after one run's 40 requests to its state port, the next run's state port answered %d", other)
	}
}

// A plan or an apply whose mask cannot read the plan keeps none of
// tofu's output, and still keeps its network record and logs its run
// egress line, with each host withheld, since only the plan's marks
// could sweep it.
func TestTofuEgressMaskRefusesPlan(t *testing.T) {
	const unreadable = `{"format_version":"9.0"}`
	want := []server.NetworkDestination{{Host: "(withheld)", Port: 443, Verdict: "refused", Reason: "not in the list", Count: 1}}
	setup := func(t *testing.T) (*fakeTofu, *fixture, *syncBuffer) {
		var log syncBuffer
		runner := &fakeTofu{}
		f := tofuApplyFixtureEdit(t, runner, withEgress("enforce", `[]`), func(o *server.Options) {
			o.Log = slog.New(slog.NewTextHandler(&log, nil))
		})
		return runner, f, &log
	}
	check := func(t *testing.T, f *fixture, log *syncBuffer, id string, network json.RawMessage, path string) {
		t.Helper()
		if n := networkOf(t, network); n.Mode != "enforce" || !slices.Equal(n.Destinations, want) {
			t.Errorf("the network %+v, want %+v", n, want)
		}
		if served := f.do("GET", path, f.token(nil), nil); strings.Contains(string(served.body), tofuCanary) {
			t.Errorf("%s holds %s:\n%s", path, tofuCanary, served.body)
		}
		logged := log.String()
		if !strings.Contains(logged, `msg="run egress" run=`+id) || !strings.Contains(logged, "(withheld):443 refused x1") || strings.Contains(logged, tofuCanary) {
			t.Errorf("writ serve's log, which should hold the run's egress line with the host withheld:\n%s", logged)
		}
	}

	t.Run("plan", func(t *testing.T) {
		runner, f, log := setup(t)
		runner.plan = func() (tofu.Plan, error) {
			runner.mu.Lock()
			s := runner.stacks[len(runner.stacks)-1]
			runner.mu.Unlock()
			connectVia(t, serveRunPort(t, s.Net.Ports[3128]), tofuCanary+".example.org:443")
			return tofu.Plan{File: []byte("saved"), JSON: []byte(unreadable)}, nil
		}
		p, _ := f.planCommit(tofuFiles(), "withheld")
		if p.Plan.Status != "failed" || !strings.Contains(p.Text, "the mask refused the plan") {
			t.Fatalf("plan %+v\n%s", p.Plan, p.Text)
		}
		check(t, f, log, p.Plan.Ref, p.Network, "/v1/plans/"+p.Plan.Ref)
	})

	t.Run("apply", func(t *testing.T) {
		runner, f, log := setup(t)
		id := approvedTofuPlan(t, f, "withheld")
		runner.mu.Lock()
		runner.applyShow = []byte(unreadable)
		runner.apply = func(context.Context, stateCall) ([]byte, error) {
			applied := runner.appliedSoFar()
			connectVia(t, serveRunPort(t, applied[len(applied)-1].stack.Net.Ports[3128]), tofuCanary+".example.org:443")
			return nil, nil
		}
		runner.mu.Unlock()
		run := f.tofuRun(bob(f), map[string]any{"plan": id})
		if !strings.Contains(run.Text, "the mask could not read the saved plan") {
			t.Fatalf("run %+v\n%s", run.Run, run.Text)
		}
		check(t, f, log, run.Run.ID, run.Network, "/v1/runs/"+run.Run.ID)
	})
}

// Under egress_mode report or enforce, a provider install asks only the
// registries provider_registries lists: a plan whose lock file pins a
// provider from another fails before the install, with a diagnostic that
// names the registry and the key. Under egress_mode off a lock file's
// registry is not checked, as before.
func TestTofuEgressProviderRegistries(t *testing.T) {
	lockFrom := func(host string) map[string]forgetest.File {
		files := tofuFiles()
		files["infra/app/.terraform.lock.hcl"] = forgetest.File{Content: strings.Replace(tofuLockFile, "registry.opentofu.org", host, 1)}
		return files
	}
	refusal := func(host, mode, list string) string {
		return "writ: install the providers of .terraform.lock.hcl into the plugin cache: provider " + host + "/hashicorp/random is from registry " + host +
			", which provider_registries does not list: under egress_mode " + mode + ", writ serve installs providers only from " + list + "; add " + host + " to provider_registries to install from it\n"
	}
	listed := "provider_registries = [\"registry.example.com\"]\n"
	tests := []struct {
		name, mode, extra, host, want string
	}{
		{"enforce refuses an unlisted registry", "enforce", "", "localhost", refusal("localhost", "enforce", "registry.opentofu.org, registry.terraform.io")},
		{"report refuses an unlisted registry", "report", "", "registry.example.com", refusal("registry.example.com", "report", "registry.opentofu.org, registry.terraform.io")},
		{"a listed registry", "enforce", listed, "registry.example.com", ""},
		{"the default list is replaced", "enforce", listed, "registry.opentofu.org", refusal("registry.opentofu.org", "enforce", "registry.example.com")},
		{"terraform's registry", "enforce", "", "registry.terraform.io", ""},
		{"off", "off", "", "localhost", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &fakeTofu{}
			f := tofuApplyFixtureEdit(t, runner, func(text string) string {
				if tt.mode != "off" {
					text = withEgress(tt.mode, `[]`)(text)
				}
				return text + tt.extra
			})
			p, _ := f.planCommit(lockFrom(tt.host), "registry")
			runner.mu.Lock()
			fills := len(runner.fills)
			runner.mu.Unlock()
			if tt.want == "" {
				if p.Plan.Status != "pending" || fills != 1 {
					t.Fatalf("plan %+v after %d installs\n%s", p.Plan, fills, p.Text)
				}
				return
			}
			if p.Plan.Status != "failed" || p.Text != tt.want || fills != 0 {
				t.Fatalf("plan %+v after %d installs\n%s\nwant\n%s", p.Plan, fills, p.Text, tt.want)
			}
		})
	}
}

// Under egress_mode report or enforce, the provider install runs in a
// network namespace of its own, whose one port is an egress proxy of its
// own: it allows a public destination whatever the env's list, and
// refuses the metadata service and a private address. The run's record
// names the install's destinations first, marked install, and keeps them
// when the install fails.
func TestTofuEgressInstall(t *testing.T) {
	for _, mode := range []string{"report", "enforce"} {
		t.Run(mode, func(t *testing.T) {
			var log syncBuffer
			runner := &fakeTofu{}
			f := tofuApplyFixtureEdit(t, runner, withEgress(mode, `["*.amazonaws.com"]`), func(o *server.Options) {
				o.Log = slog.New(slog.NewTextHandler(&log, nil))
			})
			var statuses []string
			runner.fill = func(n *tofu.Net) error {
				if n == nil || len(n.Ports) != 1 {
					return fmt.Errorf("the install's network %+v, want one port", n)
				}
				proxy := serveRunPort(t, n.Ports[3128])
				for _, target := range []string{"169.254.169.254:80", "10.0.0.10:443", "[fd00::10]:443", "127.0.0.1:5432"} {
					statuses = append(statuses, connectVia(t, proxy, target))
				}
				return nil
			}
			runner.plan = func() (tofu.Plan, error) {
				runner.mu.Lock()
				s := runner.stacks[len(runner.stacks)-1]
				runner.mu.Unlock()
				connectVia(t, serveRunPort(t, s.Net.Ports[3128]), "127.0.0.1:5432")
				return createOnly()
			}
			p, _ := f.planCommit(tofuFiles(), "install")
			if p.Plan.Status != "pending" {
				t.Fatalf("plan %+v\n%s", p.Plan, p.Text)
			}
			wantStatuses := []string{
				"403 writ egress refused 169.254.169.254:80: a link-local address, where the instance metadata service is",
				"403 writ egress refused 10.0.0.10:443: a private address, which a provider install never connects to",
				"403 writ egress refused [fd00::10]:443: a private address, which a provider install never connects to",
				"403 writ egress refused 127.0.0.1:5432: a loopback address",
			}
			if !slices.Equal(statuses, wantStatuses) {
				t.Errorf("the install's proxy answered %q, want %q", statuses, wantStatuses)
			}
			env := envMap(runner.fillEnvs[0])
			for k, v := range map[string]string{"HTTPS_PROXY": "http://127.0.0.1:3128", "https_proxy": "http://127.0.0.1:3128", "HTTP_PROXY": "http://127.0.0.1:3128", "NO_PROXY": "127.0.0.1,localhost,::1"} {
				if env[k] != v {
					t.Errorf("the install's %s = %q, want %q", k, env[k], v)
				}
			}
			if len(env) != 6 {
				t.Errorf("the install's environment is %v, want only the proxy's variables", runner.fillEnvs[0])
			}
			want := []server.NetworkDestination{
				{Step: "install", Host: "10.0.0.10", Port: 443, Verdict: "refused", Reason: "a private address, which a provider install never connects to", Count: 1},
				{Step: "install", Host: "127.0.0.1", Port: 5432, Verdict: "refused", Reason: "a loopback address", Count: 1},
				{Step: "install", Host: "169.254.169.254", Port: 80, Verdict: "refused", Reason: "a link-local address, where the instance metadata service is", Count: 1},
				{Step: "install", Host: "fd00::10", Port: 443, Verdict: "refused", Reason: "a private address, which a provider install never connects to", Count: 1},
				{Host: "127.0.0.1", Port: 5432, Verdict: "refused", Reason: "a loopback address", Count: 1},
			}
			if n := networkOf(t, p.Network); !slices.Equal(n.Destinations, want) {
				t.Errorf("the plan's network %+v, want %+v", n.Destinations, want)
			}
			if !strings.Contains(log.String(), `destinations="install 10.0.0.10:443 refused x1, install 127.0.0.1:5432 refused x1, install 169.254.169.254:80 refused x1, install [fd00::10]:443 refused x1, 127.0.0.1:5432 refused x1"`) {
				t.Errorf("writ serve's log lacks the run egress line with the install's destinations:\n%s", log.String())
			}

			// A failed install keeps the record of what it asked for.
			runner.mu.Lock()
			runner.fill = func(n *tofu.Net) error {
				connectVia(t, serveRunPort(t, n.Ports[3128]), "169.254.169.254:80")
				return errors.New("tofu init exited with code 1")
			}
			runner.mu.Unlock()
			p, _ = f.planCommit(tofuFiles(), "install-fails")
			want = []server.NetworkDestination{{Step: "install", Host: "169.254.169.254", Port: 80, Verdict: "refused", Reason: "a link-local address, where the instance metadata service is", Count: 1}}
			if n := networkOf(t, p.Network); p.Plan.Status != "failed" || !slices.Equal(n.Destinations, want) {
				t.Errorf("a failed install's plan %+v kept network %+v, want %+v\n%s", p.Plan, n.Destinations, want, p.Text)
			}
		})
	}
}
