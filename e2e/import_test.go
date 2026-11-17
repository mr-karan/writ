//go:build e2e_import

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// importCommandTimeout bounds one tofu or writ invocation. The first tofu
// init and writ init download providers; everything else takes seconds.
const importCommandTimeout = 5 * time.Minute

// importHarness holds what every case shares: the writ binary, Writ's
// provider cache, and OpenTofu's plugin cache.
type importHarness struct {
	tofu    string
	writ    string
	cache   string
	plugins string
	tfrc    string
}

func newImportHarness(t *testing.T, tofu string) *importHarness {
	h := &importHarness{tofu: tofu, writ: filepath.Join(t.TempDir(), "writ")}
	build := exec.Command("go", "build", "-o", h.writ, "github.com/mr-karan/writ/cmd/writ")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build writ: %v\n%s", err, out)
	}
	base, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	h.cache = os.Getenv("WRIT_CACHE_DIR")
	if h.cache == "" {
		h.cache = filepath.Join(base, "writ")
	}
	h.plugins = os.Getenv("TF_PLUGIN_CACHE_DIR")
	if h.plugins == "" {
		h.plugins = filepath.Join(base, "writ-e2e", "tofu-plugins")
	}
	if err := os.MkdirAll(h.plugins, 0o755); err != nil {
		t.Fatal(err)
	}
	// An empty CLI config keeps the user's own OpenTofu settings out.
	h.tfrc = filepath.Join(t.TempDir(), "tofurc")
	if err := os.WriteFile(h.tfrc, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return h
}

// env is the environment for tofu and writ: no Nomad or Terraform settings
// from the caller, and the shared caches.
func (h *importHarness) env() []string {
	var out []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "NOMAD_") && !strings.HasPrefix(kv, "TF_") && !strings.HasPrefix(kv, "WRIT_") {
			out = append(out, kv)
		}
	}
	return append(out,
		"WRIT_CACHE_DIR="+h.cache,
		"TF_PLUGIN_CACHE_DIR="+h.plugins,
		"TF_CLI_CONFIG_FILE="+h.tfrc,
		"TF_IN_AUTOMATION=1",
		"CHECKPOINT_DISABLE=1",
	)
}

type cmdResult struct {
	argv           []string
	stdout, stderr string
	code           int
}

func (r cmdResult) String() string {
	return fmt.Sprintf("%s: exit %d\nstdout:\n%s\nstderr:\n%s", strings.Join(r.argv, " "), r.code, r.stdout, r.stderr)
}

func (h *importHarness) exec(t *testing.T, dir string, argv ...string) cmdResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), importCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir, cmd.Env = dir, h.env()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	r := cmdResult{argv: argv, stdout: stdout.String(), stderr: stderr.String()}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		r.code = exitErr.ExitCode()
	default:
		t.Fatalf("run %v: %v", argv, err)
	}
	return r
}

func (h *importHarness) must(t *testing.T, dir string, argv ...string) cmdResult {
	t.Helper()
	r := h.exec(t, dir, argv...)
	if r.code != 0 {
		t.Fatalf("%s", r)
	}
	return r
}

func (h *importHarness) writJSON(t *testing.T, v any, args ...string) {
	t.Helper()
	r := h.must(t, "", append([]string{h.writ}, args...)...)
	if err := json.Unmarshal([]byte(r.stdout), v); err != nil {
		t.Fatalf("decode JSON: %v\n%s", err, r)
	}
}

// apply runs tofu init and apply in dir.
func (h *importHarness) apply(t *testing.T, dir string) {
	t.Helper()
	h.must(t, dir, h.tofu, "init", "-input=false", "-no-color")
	h.must(t, dir, h.tofu, "apply", "-auto-approve", "-input=false", "-no-color")
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, text := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const nomadMain = `terraform {
  required_providers {
    nomad = { source = "hashicorp/nomad", version = "2.6.1" }
  }
}

provider "nomad" {
  address = "%s"
}

resource "nomad_job" "web" {
  jobspec = <<-EOT
    job "import-web" {
      datacenters = ["dc1"]
      type        = "service"

      group "web" {
        count = 1

        task "sleep" {
          driver = "raw_exec"

          config {
            command = "/bin/sh"
            args    = ["-c", "sleep 3600"]
          }

          resources {
            cpu    = 20
            memory = 32
          }
        }
      }
    }
  EOT
}
`

// startImportNomad starts a Nomad dev agent on free loopback ports with
// raw_exec enabled and returns its API address. Cleanup purges the job and
// stops the agent.
func startImportNomad(t *testing.T) string {
	t.Helper()
	var ports []int
	var ls []net.Listener
	for range 3 {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ls = append(ls, l)
		ports = append(ports, l.Addr().(*net.TCPAddr).Port)
	}
	for _, l := range ls {
		l.Close()
	}
	dir := t.TempDir()
	config := fmt.Sprintf(`bind_addr = "127.0.0.1"
data_dir  = %q
ports {
  http = %d
  rpc  = %d
  serf = %d
}
advertise {
  http = "127.0.0.1"
  rpc  = "127.0.0.1"
  serf = "127.0.0.1"
}
plugin "raw_exec" {
  config {
    enabled = true
  }
}
`, filepath.Join(dir, "data"), ports[0], ports[1], ports[2])
	configPath := filepath.Join(dir, "agent.hcl")
	if err := os.WriteFile(configPath, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "agent.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("nomad", "agent", "-dev", "-config="+configPath)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "NOMAD_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() {
		cmd.Wait()
		logFile.Close()
		close(exited)
	}()

	api := fmt.Sprintf("http://127.0.0.1:%d", ports[0])
	client := &http.Client{Timeout: 5 * time.Second}
	get := func(path string, v any) error {
		resp, err := client.Get(api + path)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("GET %s: %s", path, resp.Status)
		}
		return json.NewDecoder(resp.Body).Decode(v)
	}
	t.Cleanup(func() {
		req, err := http.NewRequest(http.MethodDelete, api+"/v1/job/import-web?purge=true", nil)
		if err == nil {
			if resp, err := client.Do(req); err == nil {
				resp.Body.Close()
			}
		}
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			var allocs []struct{ ClientStatus string }
			if get("/v1/allocations", &allocs) != nil || !slices.ContainsFunc(allocs, func(a struct{ ClientStatus string }) bool {
				return a.ClientStatus == "running" || a.ClientStatus == "pending"
			}) {
				break
			}
		}
		helpers := nomadHelpers(cmd.Process.Pid)
		cmd.Process.Signal(os.Interrupt)
		select {
		case <-exited:
		case <-time.After(30 * time.Second):
			cmd.Process.Signal(syscall.SIGKILL)
			<-exited
		}
		stopNomadHelpers(helpers)
		if t.Failed() {
			if b, err := os.ReadFile(logPath); err == nil {
				t.Logf("nomad agent log:\n%s", b)
			}
		}
	})

	for deadline := time.Now().Add(time.Minute); ; time.Sleep(200 * time.Millisecond) {
		var nodes []struct {
			Status  string
			Drivers map[string]struct{ Healthy bool }
		}
		if get("/v1/nodes", &nodes) == nil && len(nodes) == 1 && nodes[0].Status == "ready" && nodes[0].Drivers["raw_exec"].Healthy {
			return api
		}
		if time.Now().After(deadline) {
			t.Fatal("the Nomad agent has no ready node with raw_exec after a minute")
		}
	}
}
