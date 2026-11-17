//go:build tofu

package tofu

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	"github.com/mr-karan/writ/internal/runas"
)

// TestGate runs real OpenTofu with hashicorp/random, local, and tls. The
// parent fills the shared plugin cache, which needs the registry, and builds
// testdata/hold, and then runs this test again inside user, network, mount,
// and pid namespaces, where only loopback exists and /proc shows only the
// test's own processes.
const (
	gateChildEnv = "WRIT_TOFU_GATE_CHILD"
	gateCacheEnv = "WRIT_TOFU_GATE_CACHE"
	gateBinEnv   = "WRIT_TOFU_GATE_BIN"
	gateRootEnv  = "WRIT_TOFU_GATE_ROOT"
	gateHoldEnv  = "WRIT_TOFU_GATE_HOLD"
)

// runUser is a uid that the namespace maps and nothing else uses, as a
// provider uid of writ serve is.
var runUser = &User{UID: 61000, GID: 61000}

var namespaceArgs = []string{"--user", "--map-root-user", "--map-users=auto", "--map-groups=auto", "--net", "--pid", "--fork", "--mount-proc"}

func TestGate(t *testing.T) {
	if os.Getenv(gateChildEnv) != "" {
		gateChild(t)
		return
	}
	bin, err := exec.LookPath("tofu")
	if err != nil {
		t.Fatalf("this gate needs tofu %s on PATH: %v", Version, err)
	}
	base, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	cache := os.Getenv("TF_PLUGIN_CACHE_DIR")
	if cache == "" {
		cache = filepath.Join(base, "writ-e2e", "tofu-plugins")
	}
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	r := newRunner(t, bin, cache, t.TempDir(), 10*time.Minute, 64<<20)
	if _, err := r.Plan(context.Background(), stack(t, providersTF)); err != nil {
		t.Fatalf("fill the plugin cache: %v", stepText(err))
	}

	// The run's uid can read nothing of this user's, so its copy of the
	// cache goes where any user may read it, as a server's cache must be.
	shared := sharedDir(t, 0o755)
	for _, p := range []string{"random/3.9.1", "local/2.9.1", "tls/4.4.2"} {
		rel := filepath.Join("registry.opentofu.org", "hashicorp", p)
		if err := os.CopyFS(filepath.Join(shared, rel), os.DirFS(filepath.Join(cache, rel))); err != nil {
			t.Fatal(err)
		}
	}
	hold := filepath.Join(t.TempDir(), "hold")
	build := exec.Command("go", "build", "-o", hold, "./testdata/hold")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build testdata/hold: %v\n%s", err, out)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("unshare", append(slices.Clone(namespaceArgs), self, "-test.run=^TestGate$", "-test.v", "-test.count=1", "-test.timeout=10m")...)
	cmd.Env = append(os.Environ(), gateChildEnv+"=1", gateCacheEnv+"="+shared, gateBinEnv+"="+bin, gateRootEnv+"="+t.TempDir(), gateHoldEnv+"="+hold)
	out, err := cmd.CombinedOutput()
	t.Logf("in the namespaces:\n%s", out)
	if err != nil || !strings.Contains(string(out), "--- PASS: TestGate") {
		t.Fatalf("the gate failed in the namespaces: %v", err)
	}
}

func gateChild(t *testing.T) {
	if os.Getpid() != 1 {
		t.Fatalf("the child runs as pid %d, not 1 of its own pid namespace", os.Getpid())
	}
	// /proc/net/dev lists the interfaces of this process's network namespace.
	dev, err := os.ReadFile("/proc/net/dev")
	lines := strings.Split(strings.TrimSpace(string(dev)), "\n")
	if err != nil || len(lines) != 3 || !strings.HasPrefix(strings.TrimSpace(lines[2]), "lo:") {
		t.Fatalf("the child's network interfaces: %v\n%s\nwant only lo", err, dev)
	}
	bin, cache := os.Getenv(gateBinEnv), os.Getenv(gateCacheEnv)
	tools := lookTools(t, "ip", "git", "sleep")
	if out, err := exec.Command(tools["ip"], "link", "set", "lo", "up").CombinedOutput(); err != nil {
		t.Fatalf("ip link set lo up: %v %s", err, out)
	}
	hold, err := os.ReadFile(os.Getenv(gateHoldEnv))
	if err != nil {
		t.Fatal(err)
	}
	enterOwnRoot(t, os.Getenv(gateRootEnv), cache)
	scratch := sharedDir(t, os.ModeSticky|0o777)
	ctx := context.Background()
	r := newRunnerOpts(t, Options{Binary: bin, Git: tools["git"], PluginCache: cache, ScratchRoot: scratch, Timeout: 5 * time.Minute, ApplyTimeout: 5 * time.Minute, MaxOutput: 64 << 20})
	users := []struct {
		name string
		user *User
	}{{"as writ", nil}, {"as the run's uid", runUser}}

	t.Run("only OpenTofu "+Version+" is accepted", func(t *testing.T) {
		for _, first := range []string{"OpenTofu v1.13.8", "OpenTofu v1.13.10", "Terraform v" + Version} {
			fake := filepath.Join(t.TempDir(), "tofu")
			script := fmt.Sprintf("#!/bin/sh\necho '%s'\necho 'on linux_amd64'\n", first)
			if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			_, err := New(ctx, Options{Binary: fake, PluginCache: cache, ScratchRoot: scratch, Timeout: time.Minute, ApplyTimeout: time.Minute, MaxOutput: 1 << 20})
			if !errors.Is(err, ErrVersion) {
				t.Errorf("a binary that reports %q: %v, want ErrVersion", first, err)
			}
		}
	})

	t.Run("plan, apply, and plan again with no changes", func(t *testing.T) {
		for _, u := range users {
			t.Run(u.name, func(t *testing.T) {
				state := newStateServer(t)
				s := stack(t, mainTF, state.env())
				s.User = u.user
				p, err := r.Plan(ctx, s)
				if err != nil {
					t.Fatalf("plan: %v", stepText(err))
				}
				want := map[string]string{"random_pet.p": "create", "tls_private_key.k": "create", "local_file.f": "create"}
				if got := actions(t, p.JSON); !maps.Equal(got, want) {
					t.Fatalf("the first plan's changes: %v, want %v", got, want)
				}
				if len(p.File) == 0 || !bytes.Contains(p.Log, []byte(`"type":"change_summary"`)) {
					t.Fatalf("the plan returned a %d-byte saved plan and this event stream:\n%s", len(p.File), p.Log)
				}
				a, err := r.Apply(ctx, s, p.File)
				if err != nil {
					t.Fatalf("apply: %v", stepText(err))
				}
				if got := actions(t, a.Plan); !maps.Equal(got, want) {
					t.Fatalf("the apply's show of the saved plan has changes %v, want %v", got, want)
				}
				if !bytes.Contains(state.get(), []byte(`"random_pet"`)) {
					t.Fatalf("the apply wrote no state with random_pet.p to the http backend:\n%s", state.get())
				}
				resources := []string{"local_file.f", "random_pet.p", "tls_private_key.k"}
				if a.ShowErr != nil || !slices.Equal(stateResources(t, a.State.Unmasked), resources) {
					t.Fatalf("the apply's show of the state it left: %v\n%s", stepText(a.ShowErr), a.State.Unmasked)
				}
				// Show reads the state from a file, with no backend to reach.
				shown, err := r.Show(ctx, Stack{Files: s.Files, Dir: s.Dir, User: u.user}, state.get())
				if err != nil || !slices.Equal(stateResources(t, shown.Unmasked), resources) {
					t.Fatalf("show of the stored state: %v\n%s", stepText(err), shown.Unmasked)
				}
				again, err := r.Plan(ctx, s)
				if err != nil {
					t.Fatalf("plan again: %v", stepText(err))
				}
				// local_file.f's file was in the scratch directory of the
				// apply, which is gone, so OpenTofu plans it again.
				want = map[string]string{"random_pet.p": "no-op", "tls_private_key.k": "no-op", "local_file.f": "create"}
				if got := actions(t, again.JSON); !maps.Equal(got, want) {
					t.Fatalf("the plan after the apply: %v, want %v", got, want)
				}
			})
		}
	})

	t.Run("a scratch root whose path is too long for a socket in it plans and applies", func(t *testing.T) {
		// Each provider makes its go-plugin socket in TMPDIR, and a
		// socket's path must fit sun_path's 108 bytes.
		long := filepath.Join(sharedDir(t, 0o755), strings.Repeat("d", 100))
		if err := os.Mkdir(long, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(long, os.ModeSticky|0o777); err != nil {
			t.Fatal(err)
		}
		r := newRunnerOpts(t, Options{Binary: bin, PluginCache: cache, ScratchRoot: long, Timeout: 5 * time.Minute, ApplyTimeout: 5 * time.Minute, MaxOutput: 64 << 20})
		for _, u := range users {
			t.Run(u.name, func(t *testing.T) {
				state := newStateServer(t)
				s := stack(t, mainTF, state.env())
				s.User = u.user
				p, err := r.Plan(ctx, s)
				if err != nil {
					t.Fatalf("plan: %v", stepText(err))
				}
				a, err := r.Apply(ctx, s, p.File)
				if err != nil {
					t.Fatalf("apply: %v", stepText(err))
				}
				if a.ShowErr != nil || !slices.Equal(stateResources(t, a.State.Unmasked), []string{"local_file.f", "random_pet.p", "tls_private_key.k"}) {
					t.Fatalf("the apply's show of the state it left: %v\n%s", stepText(a.ShowErr), a.State.Unmasked)
				}
			})
		}
	})

	t.Run("a stack whose backend needs the run's credentials plans, applies, and pulls with them", func(t *testing.T) {
		for _, u := range users {
			t.Run(u.name, func(t *testing.T) {
				state := newStateServer(t)
				// The backend's address is a credential here: init that
				// fetches gets none, and the backend's init gets it.
				s := stack(t, mainTF)
				s.Creds, s.StateCreds, s.User = []string{state.env()}, true, u.user
				p, err := r.Plan(ctx, s)
				if err != nil {
					t.Fatalf("plan: %v", stepText(err))
				}
				if read, err := PlanSerial(p.File); err != nil || read.Serial != 0 {
					t.Fatalf("the first plan read state %+v, %v; want none", read, err)
				}
				a, err := r.Apply(ctx, s, p.File)
				if err != nil || a.ShowErr != nil {
					t.Fatalf("apply: %v, then the read of its state: %v", stepText(err), stepText(a.ShowErr))
				}
				var stored struct {
					Lineage string `json:"lineage"`
					Serial  int64  `json:"serial"`
				}
				if err := json.Unmarshal(state.get(), &stored); err != nil {
					t.Fatal(err)
				}
				resources := []string{"local_file.f", "random_pet.p", "tls_private_key.k"}
				if a.State.Serial != (Serial{stored.Lineage, stored.Serial}) || stored.Serial == 0 || !slices.Equal(stateResources(t, a.State.Unmasked), resources) {
					t.Fatalf("the apply read state %+v, want the stored %+v:\n%s", a.State.Serial, stored, a.State.Unmasked)
				}
				pulled, err := r.Pull(ctx, s)
				if err != nil || pulled.Serial != a.State.Serial || !slices.Equal(stateResources(t, pulled.Unmasked), resources) {
					t.Fatalf("pull: %v, serial %+v, want %+v\n%s", stepText(err), pulled.Serial, a.State.Serial, pulled.Unmasked)
				}
				again, err := r.Plan(ctx, s)
				if err != nil {
					t.Fatalf("plan again: %v", stepText(err))
				}
				if read, err := PlanSerial(again.File); err != nil || read != a.State.Serial {
					t.Fatalf("the plan after the apply read state %+v, %v; want %+v", read, err, a.State.Serial)
				}
				// Without StateCreds the address reaches no init, and
				// the backend cannot be configured.
				s.StateCreds = false
				if _, err := r.Pull(ctx, s); err == nil || !strings.Contains(stepText(err), "init") {
					t.Fatalf("a pull whose init has no address: %v, want init to fail", stepText(err))
				}
			})
		}
	})

	t.Run("a stack with no state pulls nothing", func(t *testing.T) {
		state := newStateServer(t)
		s := stack(t, mainTF)
		s.Creds, s.StateCreds = []string{state.env()}, true
		pulled, err := r.Pull(ctx, s)
		if err != nil || pulled.Serial != (Serial{}) || pulled.Unmasked != nil {
			t.Fatalf("pull of no state: %v, %+v, %d bytes", stepText(err), pulled.Serial, len(pulled.Unmasked))
		}
	})

	t.Run("a run's uid plans with the providers Fill put in the cache", func(t *testing.T) {
		// The copy has the modes a service with UMask=0077 gives what it
		// writes, which the run's uid cannot read.
		own := sharedDir(t, 0o755)
		if err := os.CopyFS(own, os.DirFS(cache)); err != nil {
			t.Fatal(err)
		}
		err := filepath.WalkDir(own, func(p string, d os.DirEntry, err error) error {
			if err != nil || p == own {
				return err
			}
			return os.Chmod(p, 0o700)
		})
		if err != nil {
			t.Fatal(err)
		}
		fr := newRunner(t, bin, own, scratch, 5*time.Minute, 64<<20)
		s := stack(t, mainTF, newStateServer(t).env())
		s.User = runUser
		if _, err := fr.Plan(ctx, s); err == nil {
			t.Fatal("the run's uid planned from a cache it cannot read")
		}
		lock, err := os.ReadFile("testdata/lock.hcl")
		if err != nil {
			t.Fatal(err)
		}
		if err := fr.Fill(ctx, lock); err != nil {
			t.Fatalf("fill: %v", stepText(err))
		}
		if _, err := fr.Plan(ctx, s); err != nil {
			t.Fatalf("plan as the run's uid after Fill: %v", stepText(err))
		}
		err = runas.AsUser(*runUser, func() error { return os.WriteFile(filepath.Join(own, "planted"), nil, 0o644) })
		if err == nil {
			t.Fatal("the run's uid wrote into the plugin cache")
		}
	})

	t.Run("a stale saved plan is refused", func(t *testing.T) {
		state := newStateServer(t)
		s := stack(t, mainTF, state.env())
		first, err := r.Plan(ctx, s)
		if err != nil {
			t.Fatal(stepText(err))
		}
		second, err := r.Plan(ctx, s)
		if err != nil {
			t.Fatal(stepText(err))
		}
		if _, err := r.Apply(ctx, s, second.File); err != nil {
			t.Fatalf("apply the second plan: %v", stepText(err))
		}
		before := state.get()
		_, err = r.Apply(ctx, s, first.File)
		var se *StepError
		if !errors.Is(err, ErrStale) || !errors.As(err, &se) {
			t.Fatalf("apply the first plan after the state changed: %v, want ErrStale", stepText(err))
		}
		t.Logf("OpenTofu %s refused the stale plan: %s", Version, diagnostics(se.Stdout))
		if after := state.get(); !bytes.Equal(before, after) {
			t.Fatal("the refused apply changed the state")
		}
	})

	t.Run("a timeout kills the whole process tree", func(t *testing.T) {
		for _, u := range users {
			t.Run(u.name, func(t *testing.T) {
				// The provisioner holds the apply while the local provider
				// that made local_file.f runs.
				s := holdStack(t, hold, `resource "local_file" "f" {
  filename = "${path.module}/out.txt"
  content  = "x"

  provisioner "local-exec" {
    command     = "sleep"
    interpreter = ["${path.module}/hold"]
  }
}
`)
				s.User = u.user
				p, err := r.Plan(ctx, s)
				if err != nil {
					t.Fatalf("plan: %v", stepText(err))
				}
				const timeout = 20 * time.Second
				short := newRunner(t, bin, cache, scratch, timeout, 64<<20)
				seen := make(chan map[int]map[string]string, 1)
				stop := make(chan struct{})
				go func() { seen <- watchTree(stop, "tofu", "local", "hold") }()
				start := time.Now()
				_, err = short.Apply(ctx, s, p.File)
				elapsed := time.Since(start)
				close(stop)
				tree := <-seen
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("apply: %v, want a timeout", stepText(err))
				}
				if elapsed > timeout+5*time.Second {
					t.Errorf("apply returned %s after it started, for a %s timeout", elapsed, timeout)
				}
				if tree == nil {
					t.Fatal("never saw tofu, the local provider, and the provisioner running together")
				}
				for pid, st := range tree {
					t.Logf("pid %d %s: Uid %s, Groups %q, CapEff %s", pid, st["Name"], st["Uid"], st["Groups"], st["CapEff"])
					if st["CapEff"] != "0000000000000000" {
						t.Errorf("pid %d %s ran with capabilities %s", pid, st["Name"], st["CapEff"])
					}
					if u.user == nil {
						continue
					}
					want := strconv.FormatUint(uint64(u.user.UID), 10)
					if st["Uid"] != strings.Repeat(want+"\t", 3)+want || st["Groups"] != "" {
						t.Errorf("pid %d %s ran with Uid %q, Groups %q", pid, st["Name"], st["Uid"], st["Groups"])
					}
				}
				if left := running(); len(left) > 0 {
					t.Fatalf("processes outlived the timeout: %v", names(left))
				}
			})
		}
	})

	t.Run("a run reads no file of the host", func(t *testing.T) {
		// The marker file is readable by every user, as most of a host's
		// files are, so only the run's view of the filesystem keeps it out.
		host := sharedDir(t, 0o755)
		marker := filepath.Join(host, "marker.txt")
		if err := os.WriteFile(marker, []byte(hostMarker), 0o644); err != nil {
			t.Fatal(err)
		}
		module := filepath.Join(host, "module")
		if err := os.Mkdir(module, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(module, "main.tf"), []byte(fmt.Sprintf("output \"v\" {\n  value = %q\n}\n", hostMarker)), 0o644); err != nil {
			t.Fatal(err)
		}
		// A module host that answers as OpenTofu's http getter asks,
		// with X-Terraform-Get naming a directory of the host.
		xtg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Terraform-Get", "file://"+module)
			w.WriteHeader(http.StatusNoContent)
		}))
		t.Cleanup(xtg.Close)
		// Git modules, cloned over https as a module host's would be,
		// whose links name the marker file: by its absolute path, and by
		// enough ../ to climb from anywhere in the run to the root.
		gitURL, gitRoot := newGitServer(t, tools["git"])
		linkTF := map[string]string{
			"abs-link": "output \"v\" {\n  value = file(\"${path.module}/link\")\n}\n",
			"rel-link": "data \"local_file\" \"l\" {\n  filename = \"${path.module}/link\"\n}\n\noutput \"v\" {\n  value = data.local_file.l.content\n}\n",
		}
		linkTarget := map[string]string{"abs-link": marker, "rel-link": strings.Repeat("../", 64) + strings.TrimPrefix(marker, "/")}
		for name, tf := range linkTF {
			gitRepo(t, tools["git"], gitRoot, name, map[string]string{"main.tf": tf}, nil, map[string]string{"link": linkTarget[name]})
		}
		gitModule := func(name string) string {
			return fmt.Sprintf("module \"x\" {\n  source = %q\n}\n", "git::"+gitURL+"/"+name+".git?ref=main")
		}
		output := "\noutput \"v\" {\n  value = module.x.v\n}\n"
		for _, c := range []struct {
			name, tf string
			// fetched is whether init must succeed, so that the plan
			// that follows reads the module's link.
			fetched bool
		}{
			{name: "data local_file of an absolute path", tf: fmt.Sprintf("data \"local_file\" \"m\" {\n  filename = %q\n}\n\noutput \"v\" {\n  value = data.local_file.m.content\n}\n", marker)},
			{name: "file() of an absolute path", tf: fmt.Sprintf("output \"v\" {\n  value = file(%q)\n}\n", marker)},
			{name: "a module host that answers X-Terraform-Get with file://", tf: fmt.Sprintf("module \"x\" {\n  source = %q\n}\n", xtg.URL+"/module") + output},
			{name: "file() of a link in a Git module to an absolute path", tf: gitModule("abs-link") + output, fetched: true},
			{name: "data local_file of a link in a Git module that climbs out with ../", tf: gitModule("rel-link") + output, fetched: true},
		} {
			t.Run(c.name, func(t *testing.T) {
				for _, u := range users {
					t.Run(u.name, func(t *testing.T) {
						s := stack(t, providersTF+c.tf)
						s.User = u.user
						p, err := r.Plan(ctx, s)
						se, ok := errors.AsType[*StepError](err)
						if ok {
							t.Logf("plan: %v: %s %q", err, diagnostics(se.Stdout), bytes.Join(bytes.Fields(se.Stderr), []byte(" ")))
						} else {
							t.Logf("plan: %v", err)
						}
						requireNoMarker(t, p, err)
						if c.fetched && ok && se.Step == "init" {
							t.Errorf("init failed, so the module and its link never reached the run: %v", stepText(err))
						}
					})
				}
			})
		}
	})

	t.Run("a git module over https clones in the run's view", func(t *testing.T) {
		url, _ := newGitServer(t, tools["git"])
		s := stack(t, providersTF+gitModuleTF(url))
		t.Run("with git", func(t *testing.T) {
			for _, u := range users {
				t.Run(u.name, func(t *testing.T) {
					s.User = u.user
					p, err := r.Plan(ctx, s)
					if err != nil {
						t.Fatalf("plan: %v", stepText(err))
					}
					// The value comes from the module's submodule, which
					// tofu fetches after the clone.
					if got := plannedOutput(t, p.JSON, "v"); got != "gate-submodule" {
						t.Fatalf("output v: %q, want %q", got, "gate-submodule")
					}
				})
			}
		})
		t.Run("without git the failure names the setting", func(t *testing.T) {
			noGit := newRunner(t, bin, cache, scratch, 5*time.Minute, 64<<20)
			s.User = runUser
			_, err := noGit.Plan(ctx, s)
			se, ok := errors.AsType[*StepError](err)
			if !ok || se.Step != "init" || !strings.Contains(se.Error(), "tofu block sets git") {
				t.Fatalf("plan without git: %v, want init's failure to name the tofu block's git", stepText(err))
			}
		})
	})

	t.Run("git refuses a file source of a repository the view holds", func(t *testing.T) {
		// Every run's view holds the plugin cache, and the run's uid may
		// read this repository there, so only GIT_ALLOW_PROTOCOL keeps git
		// from cloning it, over the config the stack's environment sets.
		repo := gitRepo(t, tools["git"], cache, "gate-file-repo", map[string]string{"main.tf": fmt.Sprintf("output \"v\" {\n  value = %q\n}\n", hostMarker)}, nil, nil)
		t.Cleanup(func() { os.RemoveAll(repo) })
		for _, u := range users {
			t.Run(u.name, func(t *testing.T) {
				s := stack(t, providersTF+fmt.Sprintf("module \"x\" {\n  source = %q\n}\n\noutput \"v\" {\n  value = module.x.v\n}\n", "git::file://"+repo),
					"GIT_CONFIG_COUNT=3", "GIT_CONFIG_KEY_0=safe.directory", "GIT_CONFIG_VALUE_0=*",
					"GIT_CONFIG_KEY_1=protocol.file.allow", "GIT_CONFIG_VALUE_1=always",
					"GIT_CONFIG_KEY_2=protocol.allow", "GIT_CONFIG_VALUE_2=always")
				s.User = u.user
				p, err := r.Plan(ctx, s)
				requireNoMarker(t, p, err)
				se, ok := errors.AsType[*StepError](err)
				if !ok || !strings.Contains(strings.Join(strings.Fields(string(se.Stderr)), " "), "transport 'file' not allowed") {
					t.Fatalf("plan: %v, want git to refuse the file transport", stepText(err))
				}
			})
		}
	})

	t.Run("another run's uid cannot move or enter a run's directory", func(t *testing.T) {
		other := User{UID: runUser.UID + 1, GID: runUser.GID + 1}
		owned := sharedDir(t, 0o755)
		foreign := filepath.Join(sharedDir(t, 0o755), "foreign")
		shared := sharedDir(t, os.ModeSticky|0o777)
		entry := filepath.Join(shared, "entry")
		for _, d := range []string{entry, filepath.Join(entry, "root"), foreign, filepath.Join(foreign, "root")} {
			if err := os.Mkdir(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		for _, d := range []string{owned, entry, foreign} {
			if err := os.Chown(d, int(other.UID), int(other.GID)); err != nil {
				t.Fatal(err)
			}
		}
		for root, why := range map[string]string{
			owned:                        "a root that belongs to the other uid",
			filepath.Join(entry, "root"): "a root under the other uid's entry in a sticky directory",
			// Only its owner may write it, and its owner may rename the
			// root in it and put a symbolic link in its place.
			filepath.Join(foreign, "root"): "a root in a directory of mode 0755 that belongs to the other uid",
		} {
			_, err := New(ctx, Options{Binary: bin, PluginCache: cache, ScratchRoot: root, Timeout: time.Minute, ApplyTimeout: time.Minute, MaxOutput: 1 << 20})
			if err == nil || !strings.Contains(err.Error(), "scratch root") {
				t.Errorf("New with %s: %v, want a refusal", why, err)
			}
		}

		// The http backend holds the plan's read of the state, and so the
		// run, open until the test releases it.
		state := newStateServer(t)
		release := state.hold()
		s := stack(t, mainTF, state.env())
		s.User = runUser
		short := newRunner(t, bin, cache, scratch, 2*time.Minute, 64<<20)
		done := make(chan error, 1)
		go func() {
			_, err := short.Plan(ctx, s)
			done <- err
		}()
		finished := false
		defer func() {
			if !finished {
				release()
				<-done
			}
		}()
		stop := make(chan struct{})
		timer := time.AfterFunc(time.Minute, func() { close(stop) })
		defer timer.Stop()
		if !state.waitHeld(stop) {
			t.Fatal("the plan never read the state")
		}
		ents, err := os.ReadDir(scratch)
		if err != nil || len(ents) != 1 {
			t.Fatalf("the scratch root holds %v, %v; want one run's directory", ents, err)
		}
		run := filepath.Join(scratch, ents[0].Name())
		st, err := os.Lstat(run)
		if err != nil {
			t.Fatal(err)
		}
		if uid := st.Sys().(*syscall.Stat_t).Uid; uid != runUser.UID || st.Mode().Perm() != 0o700 {
			t.Errorf("the run's directory belongs to uid %d with mode %v, want uid %d and mode 0700", uid, st.Mode().Perm(), runUser.UID)
		}
		moved := run + "-moved"
		if err := runas.AsUser(other, func() error { return os.Rename(run, moved) }); err == nil {
			_ = os.Rename(moved, run)
			t.Errorf("%s moved the run's directory of %s", other, runUser)
		}
		if err := runas.AsUser(other, func() error { _, err := os.ReadDir(run); return err }); err == nil {
			t.Errorf("%s listed the run's directory of %s", other, runUser)
		}
		release()
		finished = true
		if err := <-done; err != nil {
			t.Fatalf("the plan after its release: %v", stepText(err))
		}
	})

	t.Run("output over the limit kills the run", func(t *testing.T) {
		small := newRunner(t, bin, cache, scratch, time.Minute, 256)
		_, err := small.Plan(ctx, stack(t, mainTF, newStateServer(t).env()))
		if !errors.Is(err, ErrOutputLimit) {
			t.Fatalf("plan: %v, want ErrOutputLimit", stepText(err))
		}
		if left := running(); len(left) > 0 {
			t.Fatalf("processes outlived the run: %v", names(left))
		}
	})

	t.Run("a run as root or as writ is refused and ends nothing", func(t *testing.T) {
		// writ is root of the namespace, so uid 0 is writ's uid too.
		other := exec.Command(tools["sleep"], "300")
		if err := other.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() {
			_ = other.Process.Kill()
			_ = other.Wait()
		}()
		s := stack(t, providersTF)
		s.User = &User{UID: 0, GID: 0}
		_, err := r.Plan(ctx, s)
		if err == nil || !strings.Contains(err.Error(), "refuse to run as uid 0") {
			t.Errorf("plan as uid 0: %v, want a refusal", stepText(err))
		}
		if st, err := runas.ProcStatus(other.Process.Pid); err != nil || strings.HasPrefix(st["State"], "Z") {
			t.Fatalf("the run as uid 0 killed another process of uid 0: %v %s", err, st["State"])
		}
	})

	t.Run("a process that leaves the group with setsid dies with the run", func(t *testing.T) {
		s := holdStack(t, hold, `resource "terraform_data" "escape" {
  provisioner "local-exec" {
    command     = "escape"
    interpreter = ["${path.module}/hold"]
  }
}
`)
		s.User = runUser
		p, err := r.Plan(ctx, s)
		if err != nil {
			t.Fatalf("plan: %v", stepText(err))
		}
		a, err := r.Apply(ctx, s, p.File)
		if err != nil {
			t.Fatalf("apply: %v", stepText(err))
		}
		if !bytes.Contains(a.Log, []byte("escaped")) {
			t.Fatalf("the provisioner never started its escaped copy:\n%s", a.Log)
		}
		if left := running(); len(left) > 0 {
			t.Fatalf("processes outlived the run: %v", names(left))
		}
	})
}

// gitModuleTF calls the module that newGitServer serves.
func gitModuleTF(url string) string {
	return fmt.Sprintf("module \"m\" {\n  source = %q\n}\n\noutput \"v\" {\n  value = module.m.v\n}\n", "git::"+url+"/mod.git?ref=main")
}

// newGitServer serves two repositories over https on loopback with git
// http-backend, and returns its URL and the directory it serves, where a
// caller may add more: mod.git, a module whose output v is the output s
// of its submodule inner, and sub.git, that submodule. For the rest of the
// test, the CA bundle that the run's view holds is the server's
// certificate, bound over the host's in this process's own mount
// namespace.
func newGitServer(t *testing.T, git string) (string, string) {
	t.Helper()
	root := t.TempDir()
	sub := gitRepo(t, git, root, "sub", map[string]string{"main.tf": "output \"s\" {\n  value = \"gate-submodule\"\n}\n"}, nil, nil)
	head, err := exec.Command(git, "--git-dir", sub, "rev-parse", "main").Output()
	if err != nil {
		t.Fatal(err)
	}
	gitRepo(t, git, root, "mod", map[string]string{
		"main.tf":     "module \"inner\" {\n  source = \"./inner\"\n}\n\noutput \"v\" {\n  value = module.inner.s\n}\n",
		".gitmodules": "[submodule \"inner\"]\n\tpath = inner\n\turl = ../sub.git\n",
	}, map[string]string{"inner": strings.TrimSpace(string(head))}, nil)
	srv := httptest.NewTLSServer(&cgi.Handler{Path: git, Args: []string{"http-backend"},
		Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}})
	t.Cleanup(srv.Close)

	ca := ""
	for _, p := range caPaths {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			ca = real
			break
		}
	}
	if ca == "" {
		t.Fatal("the host has no CA bundle to bind the server's certificate over")
	}
	cert := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mount(cert, ca, "", syscall.MS_BIND, ""); err != nil {
		t.Fatalf("bind the server's certificate over %s: %v", ca, err)
	}
	t.Cleanup(func() { syscall.Unmount(ca, 0) })
	return srv.URL, root
}

// gitRepo makes the bare repository NAME.git in dir, with one commit on
// main that holds files, for each path of gitlinks a submodule at that
// commit, and for each path of links a symbolic link to that target. It
// returns the repository's path.
func gitRepo(t *testing.T, git, dir, name string, files, gitlinks, links map[string]string) string {
	t.Helper()
	repo := filepath.Join(dir, name+".git")
	work := t.TempDir()
	for f, text := range files {
		if err := os.WriteFile(filepath.Join(work, f), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for f, target := range links {
		if err := os.Symlink(target, filepath.Join(work, f)); err != nil {
			t.Fatal(err)
		}
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(git, append([]string{"--git-dir", repo, "--work-tree", work, "-c", "user.name=gate", "-c", "user.email=gate@example.invalid"}, args...)...)
		cmd.Env = []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if out, err := exec.Command(git, "init", "-q", "--bare", "-b", "main", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init %s: %v %s", repo, err, out)
	}
	run("add", ".")
	for p, commit := range gitlinks {
		run("update-index", "--add", "--cacheinfo", "160000,"+commit+","+p)
	}
	run("commit", "-q", "-m", "gate")
	return repo
}

// plannedOutput is the value of a root module output in `tofu show -json`
// of a saved plan.
func plannedOutput(t *testing.T, js []byte, name string) string {
	t.Helper()
	var doc struct {
		PlannedValues struct {
			Outputs map[string]struct {
				Value any `json:"value"`
			} `json:"outputs"`
		} `json:"planned_values"`
	}
	if err := json.Unmarshal(js, &doc); err != nil {
		t.Fatalf("the plan's JSON: %v\n%s", err, js)
	}
	return fmt.Sprint(doc.PlannedValues.Outputs[name].Value)
}

// hostMarker is the content of the host's file that a run must not read.
const hostMarker = "host-file-marker-8c41e07b"

// requireNoMarker fails t if hostMarker is anywhere in what a plan
// returned: its saved plan, its JSON, its event stream, a failed step's
// output, or the error.
func requireNoMarker(t *testing.T, p Plan, err error) {
	t.Helper()
	where := map[string][]byte{"the saved plan": p.File, "the plan's JSON": p.JSON, "the plan's events": p.Log, "the error": []byte(fmt.Sprint(err))}
	if se, ok := errors.AsType[*StepError](err); ok {
		where["the step's stdout"], where["the step's stderr"] = se.Stdout, se.Stderr
	}
	for name, b := range where {
		if bytes.Contains(b, []byte(hostMarker)) {
			t.Errorf("%s holds the host's file:\n%s", name, b)
		}
	}
}

// stateServer stands in for the http backend that writ serve serves. It
// keeps one state, and holds every read of it while it is held.
type stateServer struct {
	srv   *httptest.Server
	mu    sync.Mutex
	state []byte
	held  chan struct{}
	reads chan struct{}
}

func newStateServer(t *testing.T) *stateServer {
	t.Helper()
	s := &stateServer{reads: make(chan struct{}, 16)}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			s.mu.Lock()
			held := s.held
			s.mu.Unlock()
			if held != nil {
				s.reads <- struct{}{}
				<-held
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.state == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Write(s.state)
		case http.MethodPost:
			b, err := io.ReadAll(r.Body)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			s.mu.Lock()
			s.state = b
			s.mu.Unlock()
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *stateServer) env() string { return "TF_HTTP_ADDRESS=" + s.srv.URL + "/state" }

func (s *stateServer) get() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.state)
}

// hold makes every read wait until the returned func is called.
func (s *stateServer) hold() func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.held = make(chan struct{})
	var once sync.Once
	return func() { once.Do(func() { close(s.held) }) }
}

// waitHeld reports whether a read began waiting before stop closed.
func (s *stateServer) waitHeld(stop <-chan struct{}) bool {
	select {
	case <-s.reads:
		return true
	case <-stop:
		return false
	}
}

const providersTF = `terraform {
  required_providers {
    random = { source = "hashicorp/random", version = "3.9.1" }
    local  = { source = "hashicorp/local", version = "2.9.1" }
    tls    = { source = "hashicorp/tls", version = "4.4.2" }
  }
}
`

// mainTF is a stack on the http backend, whose state a stateServer keeps
// between runs.
const mainTF = `terraform {
  backend "http" {}
  required_providers {
    random = { source = "hashicorp/random", version = "3.9.1" }
    local  = { source = "hashicorp/local", version = "2.9.1" }
    tls    = { source = "hashicorp/tls", version = "4.4.2" }
  }
}

resource "random_pet" "p" {}

resource "tls_private_key" "k" {
  algorithm = "ED25519"
}

resource "local_file" "f" {
  filename = "${path.module}/out.txt"
  content  = random_pet.p.id
}
`

// stateResources is the addresses of the managed resources in the root
// module of `tofu show -json` of a state, sorted.
func stateResources(t *testing.T, js []byte) []string {
	t.Helper()
	var st struct {
		Values struct {
			RootModule struct {
				Resources []struct {
					Address, Mode string
				} `json:"resources"`
			} `json:"root_module"`
		} `json:"values"`
	}
	if err := json.Unmarshal(js, &st); err != nil {
		t.Fatalf("read show -json of a state: %v", err)
	}
	var out []string
	for _, r := range st.Values.RootModule.Resources {
		if r.Mode == "managed" {
			out = append(out, r.Address)
		}
	}
	slices.Sort(out)
	return out
}

func stack(t *testing.T, tf string, env ...string) Stack {
	t.Helper()
	lock, err := os.ReadFile("testdata/lock.hcl")
	if err != nil {
		t.Fatal(err)
	}
	return Stack{Files: fstest.MapFS{
		"main.tf":             {Data: []byte(tf)},
		".terraform.lock.hcl": {Data: lock},
	}, Dir: ".", Env: env}
}

// holdStack is a stack of providersTF and tf, with testdata/hold beside
// its main.tf.
func holdStack(t *testing.T, hold []byte, tf string) Stack {
	t.Helper()
	s := stack(t, providersTF+tf)
	s.Files.(fstest.MapFS)["hold"] = &fstest.MapFile{Data: hold, Mode: 0o755}
	return s
}

// lookTools finds each tool on PATH as a path that holds no symbolic
// link to a directory outside the store, which enterOwnRoot leaves out.
// The file itself stays as named, because coreutils reads its name.
func lookTools(t *testing.T, names ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, name := range names {
		p, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("the gate needs %s: %v", name, err)
		}
		dir, err := filepath.EvalSymlinks(filepath.Dir(p))
		if err != nil {
			t.Fatal(err)
		}
		out[name] = filepath.Join(dir, filepath.Base(p))
	}
	return out
}

// enterOwnRoot makes a tmpfs at dir the child's root directory, with the
// system's directories, /dev, /proc, the plugin cache, and this package's
// directory bound in. In the user namespace every directory of the host,
// / among them, belongs to the overflow uid, which stands for every host
// uid the namespace does not map, so New refuses a scratch root under any
// of them. The tmpfs belongs to root of the namespace, which is writ. It
// becomes the root by pivot_root, not chroot, because the kernel lets no
// chrooted process make the user namespace of a run's view.
func enterOwnRoot(t *testing.T, dir, cache string) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mount("tmpfs", dir, "tmpfs", 0, "mode=0755"); err != nil {
		t.Fatalf("mount a tmpfs at %s: %v", dir, err)
	}
	for _, p := range []string{"/nix", "/bin", "/usr", "/lib", "/lib64", "/etc", "/dev", "/proc", wd, cache} {
		if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
			continue
		}
		at := filepath.Join(dir, p)
		if err := os.MkdirAll(at, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mount(p, at, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
			t.Fatalf("bind %s: %v", p, err)
		}
	}
	tmp := filepath.Join(dir, "tmp")
	// A checkout under /tmp, bound above, made tmp already.
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tmp, os.ModeSticky|0o777); err != nil {
		t.Fatal(err)
	}
	pivotRoot(t, dir)
	if err := os.Chdir(wd); err != nil {
		t.Fatal(err)
	}
	os.Setenv("TMPDIR", "/tmp")
}

// pivotRoot makes dir, a mount point, the root directory and drops the
// old root.
func pivotRoot(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := syscall.PivotRoot(".", "."); err != nil {
		t.Fatalf("pivot_root to %s: %v", dir, err)
	}
	if err := syscall.Unmount(".", syscall.MNT_DETACH); err != nil {
		t.Fatalf("drop the old root: %v", err)
	}
	if err := os.Chdir("/"); err != nil {
		t.Fatal(err)
	}
}

// sharedDir is a directory that the run's uid can reach.
func sharedDir(t *testing.T, mode os.FileMode) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "writ-tofu-gate-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := os.Chmod(dir, mode); err != nil {
		t.Fatal(err)
	}
	return dir
}

// actions maps each resource change of `tofu show -json` to its actions.
func actions(t *testing.T, js []byte) map[string]string {
	t.Helper()
	var doc struct {
		FormatVersion   string `json:"format_version"`
		ResourceChanges []struct {
			Address string `json:"address"`
			Change  struct {
				Actions []string `json:"actions"`
			} `json:"change"`
		} `json:"resource_changes"`
	}
	if err := json.Unmarshal(js, &doc); err != nil || doc.FormatVersion == "" {
		t.Fatalf("the plan's JSON: %v\n%s", err, js)
	}
	out := map[string]string{}
	for _, c := range doc.ResourceChanges {
		out[c.Address] = strings.Join(c.Change.Actions, ",")
	}
	return out
}

// diagnostics joins the summary and detail of each diagnostic in a -json
// event stream.
func diagnostics(log []byte) string {
	var out []string
	for line := range bytes.Lines(log) {
		var ev struct {
			Type       string `json:"type"`
			Diagnostic struct {
				Summary string `json:"summary"`
				Detail  string `json:"detail"`
			} `json:"diagnostic"`
		}
		if json.Unmarshal(line, &ev) == nil && ev.Type == "diagnostic" {
			out = append(out, ev.Diagnostic.Summary+": "+ev.Diagnostic.Detail)
		}
	}
	return strings.Join(out, "; ")
}

// stepText adds a failed step's output to its error, for a test's log.
func stepText(err error) string {
	var se *StepError
	if errors.As(err, &se) {
		return fmt.Sprintf("%v\nstdout:\n%s\nstderr:\n%s", err, se.Stdout, se.Stderr)
	}
	return fmt.Sprint(err)
}

// running returns the status of every live process but this one. In the
// child's pid namespace that is everything a run started and left.
func running() map[int]map[string]string {
	out := map[int]map[string]string{}
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		st, err := runas.ProcStatus(pid)
		if err != nil || strings.HasPrefix(st["State"], "Z") {
			continue
		}
		out[pid] = st
	}
	return out
}

// watchTree returns the first set of running processes that holds one of
// each kind named: "tofu", "local" for the local provider, or "hold". It
// returns nil if stop closes first.
func watchTree(stop <-chan struct{}, kinds ...string) map[int]map[string]string {
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return nil
		case <-tick.C:
		}
		procs := running()
		seen := map[string]bool{}
		for _, st := range procs {
			switch {
			// The kernel cuts a process's name to 15 bytes.
			case st["Name"] == "terraform-provi" && strings.Contains(cmdline(st), "local"):
				seen["local"] = true
			default:
				seen[st["Name"]] = true
			}
		}
		if !slices.ContainsFunc(kinds, func(k string) bool { return !seen[k] }) {
			return procs
		}
	}
}

func names(procs map[int]map[string]string) []string {
	var out []string
	for pid, st := range procs {
		out = append(out, fmt.Sprintf("%d %s", pid, st["Name"]))
	}
	slices.Sort(out)
	return out
}

func cmdline(st map[string]string) string {
	b, _ := os.ReadFile("/proc/" + st["Pid"] + "/cmdline")
	return string(b)
}
