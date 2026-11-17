//go:build tofu

package tfstate_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/control/controltest"
	"github.com/mr-karan/writ/internal/tfstate"
)

// TestGate runs real OpenTofu against the backend's routes. The parent
// fills the provider plugin cache, which needs the registry, and then runs
// this test again inside a user and network namespace where only loopback
// exists. There the cache is tofu's filesystem mirror, so nothing reaches
// a network.
const (
	gateChildEnv  = "WRIT_TFSTATE_GATE_CHILD"
	gateMirrorEnv = "WRIT_TFSTATE_GATE_MIRROR"
	tofuVersion   = "1.13.9"
)

const gateStack = `terraform {
  backend "http" {}
  required_providers {
    random = {
      source  = "hashicorp/random"
      version = "3.9.1"
    }
    local = {
      source  = "hashicorp/local"
      version = "2.9.1"
    }
  }
}

variable "out" {
  type = string
}

variable "words" {
  type    = number
  default = 2
}

resource "random_pet" "p" {
  length = var.words
}

resource "local_file" "f" {
  filename = var.out
  content  = random_pet.p.id
}
`

func TestGate(t *testing.T) {
	if os.Getenv(gateChildEnv) != "" {
		gateChild(t)
		return
	}
	bin, err := exec.LookPath("tofu")
	if err != nil {
		t.Fatalf("this gate needs tofu %s on PATH: %v", tofuVersion, err)
	}
	out, err := exec.Command(bin, "version", "-json").Output()
	var v struct {
		Version string `json:"terraform_version"`
	}
	if err != nil || json.Unmarshal(out, &v) != nil || v.Version != tofuVersion {
		t.Fatalf("tofu version -json: %v %s, want %s", err, out, tofuVersion)
	}
	cache := os.Getenv("TF_PLUGIN_CACHE_DIR")
	if cache == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			t.Fatal(err)
		}
		cache = filepath.Join(base, "writ-e2e", "tofu-plugins")
	}
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(gateStack), 0o644); err != nil {
		t.Fatal(err)
	}
	fill := exec.Command(bin, "init", "-input=false", "-backend=false", "-no-color")
	fill.Dir = dir
	fill.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "CHECKPOINT_DISABLE=1", "TF_CLI_CONFIG_FILE=" + os.DevNull, "TF_PLUGIN_CACHE_DIR=" + cache}
	if out, err := fill.CombinedOutput(); err != nil {
		t.Fatalf("fill the plugin cache: %v\n%s", err, out)
	}

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// A new network namespace starts with loopback down.
	cmd := exec.Command("unshare", "-rn", "sh", "-c", `ip link set lo up && exec "$0" "$@"`,
		self, "-test.run=^TestGate$", "-test.v", "-test.count=1", "-test.timeout=10m")
	cmd.Env = append(os.Environ(), gateChildEnv+"=1", gateMirrorEnv+"="+cache)
	out, err = cmd.CombinedOutput()
	t.Logf("in the namespace:\n%s", out)
	if err != nil || !bytes.Contains(out, []byte("--- PASS: TestGate")) {
		t.Fatalf("the gate failed in the namespace: %v", err)
	}
}

// gate holds one child run's backend and the stack's shared files.
type gate struct {
	*fixture
	cliConfig, home, out string
	runs                 int
}

// run is one OpenTofu run: a scratch copy of the stack, initialized with
// the run's token.
type run struct {
	g         *gate
	id, token string
	dir       string
	extraEnv  []string
}

func gateChild(t *testing.T) {
	// /proc/net/dev lists the interfaces of this network namespace.
	dev, err := os.ReadFile("/proc/net/dev")
	lines := strings.Split(strings.TrimSpace(string(dev)), "\n")
	if err != nil || len(lines) != 3 || !strings.HasPrefix(strings.TrimSpace(lines[2]), "lo:") {
		t.Fatalf("the child's network interfaces: %v\n%s\nwant only lo", err, dev)
	}
	g := &gate{fixture: newFixture(t, controltest.SQLite, tfstate.Options{}), home: t.TempDir(), out: filepath.Join(t.TempDir(), "pet.txt")}
	g.now = time.Now()
	g.cliConfig = filepath.Join(t.TempDir(), "tofu.rc")
	rc := fmt.Sprintf("provider_installation {\n  filesystem_mirror {\n    path = %q\n  }\n}\n", os.Getenv(gateMirrorEnv))
	if err := os.WriteFile(g.cliConfig, []byte(rc), 0o644); err != nil {
		t.Fatal(err)
	}
	app := g.addr("app")

	// The first plan's saved plan, applied below.
	p1 := g.newRun(t, control.StatePlan)
	if code, out := p1.tofu(t, "plan", "-input=false", "-lock=false", "-detailed-exitcode", "-out=plan.bin"); code != 2 {
		t.Fatalf("the first plan exited %d, want 2 for changes:\n%s", code, out)
	}

	t.Run("a plan's token cannot lock or write", func(t *testing.T) {
		g.want("POST", app+"?ID=x", p1.token, stateJSON(1, "l", "x"), http.StatusForbidden)
		g.want("LOCK", app+"/lock", p1.token, lockJSON("x"), http.StatusForbidden)
		g.want("UNLOCK", app+"/lock", p1.token, lockJSON("x"), http.StatusForbidden)
		code, out := p1.tofu(t, "plan", "-input=false", "-no-color")
		if code == 0 || !strings.Contains(out, "HTTP remote state endpoint invalid auth") {
			t.Errorf("a plan that takes the lock with a plan's token exited %d:\n%s", code, out)
		}
	})

	var state1 string
	t.Run("an apply writes a sealed version", func(t *testing.T) {
		a1 := g.newRun(t, control.StateApply)
		a1.copyPlan(t, p1)
		if code, out := a1.tofu(t, "apply", "-input=false", "plan.bin"); code != 0 {
			t.Fatalf("the apply exited %d:\n%s", code, out)
		}
		g.end(t, a1)
		v, err := g.store.LatestState(context.Background(), env, "app")
		if err != nil {
			t.Fatal(err)
		}
		if v.RunID != a1.id || v.Lineage == "" || v.Serial < 1 {
			t.Errorf("the stored version is run %q, lineage %q, serial %d; want run %s", v.RunID, v.Lineage, v.Serial, a1.id)
		}
		state1 = g.want("GET", app, p1.token, "", http.StatusOK)
	})

	t.Run("plan again shows no changes", func(t *testing.T) {
		p2 := g.newRun(t, control.StatePlan)
		if code, out := p2.tofu(t, "plan", "-input=false", "-lock=false", "-detailed-exitcode"); code != 0 {
			t.Fatalf("the second plan exited %d, want 0 for no changes:\n%s", code, out)
		}
	})

	t.Run("a second apply is refused while an apply holds the lock", func(t *testing.T) {
		p3 := g.newRun(t, control.StatePlan, "TF_VAR_words=3")
		if code, out := p3.tofu(t, "plan", "-input=false", "-lock=false", "-detailed-exitcode", "-out=plan.bin"); code != 2 {
			t.Fatalf("the plan of a change exited %d, want 2:\n%s", code, out)
		}
		holder := g.newRun(t, control.StateApply)
		g.want("LOCK", app+"/lock", holder.token, lockJSON("held-by-the-first-apply"), http.StatusOK)
		before := g.versions("app")
		second := g.newRun(t, control.StateApply, "TF_VAR_words=3")
		second.copyPlan(t, p3)
		code, out := second.tofu(t, "apply", "-input=false", "-no-color", "plan.bin")
		if code == 0 || !strings.Contains(out, "held-by-the-first-apply") {
			t.Fatalf("the second apply exited %d, want a refusal that names the held lock:\n%s", code, out)
		}
		if after := g.versions("app"); !slices.Equal(before, after) {
			t.Fatalf("the refused apply changed the versions from %v to %v", before, after)
		}
		g.end(t, holder)
		if code, out := second.tofu(t, "apply", "-input=false", "plan.bin"); code != 0 {
			t.Fatalf("the apply after the lock was released exited %d:\n%s", code, out)
		}
		g.end(t, second)
	})

	t.Run("a stale write is refused", func(t *testing.T) {
		before := g.versions("app")
		w := g.newRun(t, control.StateApply)
		old := filepath.Join(w.dir, "old.tfstate")
		if err := os.WriteFile(old, []byte(state1), 0o600); err != nil {
			t.Fatal(err)
		}
		// tofu skips its own lineage and serial checks with -force, so the
		// server's refusal is what stops the write.
		if code, out := w.tofu(t, "state", "push", "-force", "-no-color", "old.tfstate"); code == 0 || !strings.Contains(out, "HTTP error: 409") {
			t.Errorf("a forced push of the first state exited %d, want the server's 409:\n%s", code, out)
		}
		g.want("LOCK", app+"/lock", w.token, lockJSON("stale"), http.StatusOK)
		latest := g.want("GET", app, w.token, "", http.StatusOK)
		var doc map[string]any
		if err := json.Unmarshal([]byte(latest), &doc); err != nil {
			t.Fatal(err)
		}
		doc["lineage"] = "00000000-0000-0000-0000-000000000000"
		doc["serial"] = doc["serial"].(float64) + 1
		other, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		g.want("POST", app+"?ID=stale", w.token, string(other), http.StatusConflict)
		g.end(t, w)
		if after := g.versions("app"); !slices.Equal(before, after) {
			t.Fatalf("the stale writes changed the versions from %v to %v", before, after)
		}
		p4 := g.newRun(t, control.StatePlan, "TF_VAR_words=3")
		if code, out := p4.tofu(t, "plan", "-input=false", "-lock=false", "-detailed-exitcode"); code != 0 {
			t.Fatalf("the plan after the refused writes exited %d, want 0:\n%s", code, out)
		}
	})

	t.Run("the database holds no state or token in the clear", func(t *testing.T) {
		pet, err := os.ReadFile(g.out)
		if err != nil || len(pet) == 0 {
			t.Fatalf("the applied pet: %v", err)
		}
		files, err := filepath.Glob(g.loc + "*")
		if err != nil || len(files) == 0 {
			t.Fatalf("the control database's files: %v %v", files, err)
		}
		for _, file := range files {
			b, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(b, pet) || bytes.Contains(b, []byte(p1.token)) {
				t.Errorf("%s holds the pet's name or a token in the clear", file)
			}
		}
	})
}

// newRun issues a token for a new run and initializes a scratch copy of
// the stack with it.
func (g *gate) newRun(t *testing.T, scope control.StateScope, env ...string) *run {
	t.Helper()
	g.runs++
	r := &run{g: g, id: fmt.Sprintf("run-%d", g.runs), dir: t.TempDir(), extraEnv: env}
	r.token = g.token(r.id, "app", scope)
	if err := os.WriteFile(filepath.Join(r.dir, "main.tf"), []byte(gateStack), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out := r.tofu(t, "init", "-input=false"); code != 0 {
		t.Fatalf("init of %s exited %d:\n%s", r.id, code, out)
	}
	return r
}

func (r *run) copyPlan(t *testing.T, from *run) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(from.dir, "plan.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.dir, "plan.bin"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// tofu runs one tofu command and returns its exit code and output.
func (r *run) tofu(t *testing.T, args ...string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tofu", args...)
	cmd.Dir = r.dir
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + r.g.home, "TMPDIR=" + os.TempDir(), "CHECKPOINT_DISABLE=1",
		"TF_IN_AUTOMATION=1", "TF_CLI_CONFIG_FILE=" + r.g.cliConfig, "TF_VAR_out=" + r.g.out,
	}, tfstate.ClientEnv(r.g.srv.URL, "app", r.token)...)
	cmd.Env = append(cmd.Env, r.extraEnv...)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		return exit.ExitCode(), string(out)
	case err != nil:
		t.Fatalf("tofu %v: %v\n%s", args, err, out)
	}
	return 0, string(out)
}

func (g *gate) end(t *testing.T, r *run) {
	t.Helper()
	if err := g.b.End(context.Background(), r.id); err != nil {
		t.Fatal(err)
	}
}
