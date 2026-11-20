//go:build e2e && e2e_tofu && linux

package e2e

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/control/pgtest"
	"github.com/mr-karan/writ/internal/forge/forgetest"
	"github.com/mr-karan/writ/internal/oidc/oidctest"
)

// TestTofuEgress runs a team's writ serve under egress_mode enforce, with
// real tofu as a provider uid. env prod's egress lists one module host. A
// plan of a stack that calls a module from that host fetches it through
// writ's egress proxy, reads its state through the state port of its own
// network, and records the host as allowed. A plan of a module on an
// unlisted host is refused, and its diagnostics name the host and the
// env's egress list. The two hosts are addresses on the namespace's
// loopback, so nothing leaves it.
func TestTofuEgress(t *testing.T) {
	if os.Getenv(tofuChildEnv) != "" {
		tofuEgressChild(t)
		return
	}
	base, stop, err := pgtest.Start()
	if err != nil {
		t.Fatalf("this test needs Postgres's initdb, pg_ctl, and createdb on PATH, as just e2e gives: %v", err)
	}
	t.Cleanup(stop)
	inTofuNamespace(t, "TestTofuEgress", tofuTeamNamespace, tofuPGEnv+"="+pgtest.Database(t, base))
}

// moduleZip is a module that writes one output, as an archive tofu
// fetches over HTTP.
func moduleZip(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	w, err := zw.Create("main.tf")
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(w, "output \"v\" {\n  value = \"fetched\"\n}\n")
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func tofuEgressChild(t *testing.T) {
	db := os.Getenv(tofuPGEnv)
	u, err := url.Parse(db)
	if err != nil || u.Query().Get("host") == "" {
		t.Fatalf("the database %q names no socket directory", db)
	}
	// The module hosts stand for the network writ serve reaches: two
	// addresses on loopback, which the run's own namespace does not have.
	for _, args := range [][]string{{"link", "set", "lo", "up"}, {"addr", "add", "192.0.2.10/32", "dev", "lo"}, {"addr", "add", "192.0.2.11/32", "dev", "lo"}} {
		if out, err := exec.Command("ip", args...).CombinedOutput(); err != nil {
			t.Fatalf("ip %v: %v %s", args, err, out)
		}
	}
	lock := tofuLock(t)
	bin, shared := tofuChildRoot(t, u.Query().Get("host"))
	zipped := moduleZip(t)
	for _, addr := range []string{"192.0.2.10:8080", "192.0.2.11:8080"} {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ln.Close() })
		go func() {
			_ = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/zip")
				_, _ = w.Write(zipped)
			}))
		}()
	}

	top, err := os.MkdirTemp("/tmp", "writ-tofu-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(top) })
	cache, scratch := filepath.Join(top, "plugins"), filepath.Join(top, "runs")
	if err := os.CopyFS(cache, os.DirFS(shared)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(scratch, 0o755); err != nil {
		t.Fatal(err)
	}
	for d, mode := range map[string]os.FileMode{top: 0o755, scratch: os.ModeSticky | 0o777} {
		if err := os.Chmod(d, mode); err != nil {
			t.Fatal(err)
		}
	}

	is := oidctest.New(t)
	g := forgetest.Handler(forgeToken)
	g.URL = is.URL
	is.Mount("/api/v4/", g)
	data := t.TempDir()
	write := func(name, text string) string {
		p := filepath.Join(data, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ca := write("issuer.pem", string(is.CAPEM()))
	tokenFile := write("secrets/gitlab_read", forgeToken+"\n")
	port := freePort(t)
	srvURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	cfg := write("serve", fmt.Sprintf(`url           = %[1]q
listen        = "127.0.0.1:%[2]d"
database      = %[3]q
data          = %[4]q
provider_uids = "61000-61063"
egress_mode   = "enforce"
trusted_proxies = ["127.0.0.1/32"]

limits {
  requests_per_second  = 100
  requests_per_address = 1000
}

tofu {
  binary       = %[5]q
  plugin_cache = %[6]q
  scratch_dir  = %[7]q
}

issuer "gitlab" {
  kind     = "gitlab-ci"
  identity = "gitlab"
  url      = %[8]q
  ca_file  = %[9]q
}

forge "gitlab" {
  kind            = "gitlab"
  url             = %[8]q
  read_token_file = %[10]q
  ca_file         = %[9]q
}

repo {
  name    = "infra"
  forge   = "gitlab"
  project = "acme/infra"
  path    = "infra"
}

team "infra_mr" {
  issuer = "gitlab"
  match  = { project_id = ["4242"], pipeline_source = ["merge_request_event"] }
}

grant {
  teams  = ["infra_mr"]
  stacks = ["app"]
  envs   = ["prod"]
  can    = ["read", "plan"]
}

env "prod" {
  key    = "keys/prod.key"
  egress = ["192.0.2.10:8080"]
}
`, srvURL, port, db, data, bin, cache, scratch, is.URL, ca, tokenFile))
	files := func(host string) map[string]forgetest.File {
		return map[string]forgetest.File{
			"infra/app/.terraform.lock.hcl": {Content: string(lock)},
			"infra/app/main.tf": {Content: "terraform {\n  backend \"http\" {}\n" + tofuProviders + `}

module "fetched" {
  source = "http://` + host + `/m.zip"
}

resource "random_pet" "name" {}

output "from_module" {
  value = module.fetched.v
}
`},
		}
	}
	g.AddProject(4242, "acme/infra", "main")
	g.Commit(4242, "main", files("192.0.2.10:8080"))
	jobDir := filepath.Join(t.TempDir(), "app")
	if err := os.Mkdir(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	s := &served{t: t, is: is, url: srvURL, db: db, data: data, log: &syncBuffer{}, stack: &stackDir{t: t, dir: jobDir}}
	check := s.stack.exec(nil, "serve", "--config", cfg, "--check")
	if check.code != 0 {
		t.Fatalf("writ serve --check: %s", check)
	}
	for _, want := range []string{
		"egress: enforce; each OpenTofu run reaches the network only through writ's egress proxy, which refuses a destination its env's egress does not list\n",
		"egress: env prod allows 192.0.2.10:8080\n",
		"egress: providers install only from registry.opentofu.org, registry.terraform.io\n",
	} {
		if !strings.Contains(check.stdout, want) {
			t.Errorf("writ serve --check printed\n%s\nwithout %q", check.stdout, want)
		}
	}
	serveAgain(t, s, cfg)
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if code, doc := s.api("GET", "/v1/repos/infra", s.token(nil), nil); code == 200 && doc["commit"] != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the pinned repository was not read:\n%s", s.log.String())
		}
	}

	pipeline := 1849500
	plan := func(branch, host string) map[string]any {
		t.Helper()
		head := g.Commit(4242, branch, files(host))
		pipeline++
		tok := s.token(map[string]any{"sha": head, "ref": branch, "ref_path": "refs/heads/" + branch, "pipeline_id": fmt.Sprint(pipeline)})
		body, _ := json.Marshal(map[string]any{"format": "writ.plan-request/v1", "env": "prod", "stacks": []string{"app"}, "source": map[string]any{}})
		code, doc := s.api("POST", "/v1/plans", tok, body)
		if code != http.StatusAccepted {
			t.Fatalf("plan at %s: %d %v", branch, code, doc)
		}
		ref := doc["plan"].(map[string]any)["ref"].(string)
		for deadline := time.Now().Add(3 * time.Minute); ; {
			_, doc = s.api("GET", "/v1/plans/"+ref+"?wait=30s", tok, nil)
			if doc["plan"].(map[string]any)["status"] != "planning" {
				t.Logf("the plan at %s:\n%s\nnetwork %v", branch, doc["text"], doc["network"])
				return doc
			}
			if time.Now().After(deadline) {
				t.Fatalf("the plan at %s did not end:\n%s", branch, s.log.String())
			}
		}
	}
	destinations := func(doc map[string]any) []string {
		t.Helper()
		n, _ := doc["network"].(map[string]any)
		if n["format"] != "writ.network/v1" || n["mode"] != "enforce" {
			t.Fatalf("the plan's network: %v", doc["network"])
		}
		var out []string
		for _, d := range n["destinations"].([]any) {
			m := d.(map[string]any)
			out = append(out, fmt.Sprintf("%v:%v %v", m["host"], m["port"], m["verdict"]))
		}
		return out
	}

	listed := plan("listed", "192.0.2.10:8080")
	if status := listed["plan"].(map[string]any)["status"]; status != "approved" {
		t.Fatalf("the plan of a module on the listed host is %v:\n%s\n%s", status, listed["text"], s.log.String())
	}
	if !strings.Contains(listed["text"].(string), "from_module") {
		t.Errorf("the plan's text lacks the module's output:\n%s", listed["text"])
	}
	if got := destinations(listed); len(got) != 1 || got[0] != "192.0.2.10:8080 allowed" {
		t.Errorf("the plan's destinations %v, want 192.0.2.10:8080 allowed", got)
	}

	unlisted := plan("unlisted", "192.0.2.11:8080")
	if status := unlisted["plan"].(map[string]any)["status"]; status != "refused" {
		t.Fatalf("the plan of a module on an unlisted host is %v:\n%s", status, unlisted["text"])
	}
	const refusal = "writ's egress proxy refused 192.0.2.11:8080 (an IP address the list does not have); env prod's egress is [192.0.2.10:8080], under egress_mode enforce"
	if !strings.Contains(unlisted["text"].(string), refusal) {
		t.Errorf("the refused plan's text lacks the refusal:\n%s", unlisted["text"])
	}
	if got := destinations(unlisted); len(got) != 1 || got[0] != "192.0.2.11:8080 refused" {
		t.Errorf("the refused plan's destinations %v", got)
	}
	if !strings.Contains(s.log.String(), `"msg":"run egress"`) || !strings.Contains(s.log.String(), `"destinations":"192.0.2.11:8080 refused x`) {
		t.Errorf("writ serve logged no line of the run's egress:\n%s", s.log.String())
	}
}
