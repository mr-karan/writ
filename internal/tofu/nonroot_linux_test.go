package tofu

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/provider"
	"github.com/mr-karan/writ/internal/runas"
)

// TestPlanAsRunUserWithoutRoot plans as a run's uid the way a team's writ
// serve does under systemd: as a user other than root, with only
// CAP_SETUID, CAP_SETGID, and CAP_KILL, ambient as AmbientCapabilities
// makes them and under no_new_privs as NoNewPrivileges sets, and with the
// ambient set cleared as writ serve clears it at start. Each run's
// directory is made with the run's filesystem ids before tofu starts,
// which makes the kernel mark writ not dumpable. The parent runs this test
// again in namespaces, where it makes a tmpfs its root directory, as the
// gate does, and becomes uid 1000 with setpriv. Where systemd-run --user
// can make a delegated scope, the test runs in one, and uid 1000 gets a
// cgroup of its own in it, delegated as Delegate=yes delegates a unit's
// cgroup to its user, in which a plan as the run's uid stops at its
// memory limit.
const (
	nonRootStageEnv  = "WRIT_TOFU_NONROOT_STAGE"
	nonRootRootEnv   = "WRIT_TOFU_NONROOT_ROOT"
	nonRootBinsEnv   = "WRIT_TOFU_NONROOT_BINS"
	nonRootCgroupEnv = "WRIT_TOFU_NONROOT_CGROUP"
)

func TestPlanAsRunUserWithoutRoot(t *testing.T) {
	switch os.Getenv(nonRootStageEnv) {
	case "scope":
		delegateToWrit(t)
		return
	case "root":
		becomeWrit(t)
		return
	case "writ":
		planAsRunUserWithoutRoot(t)
		return
	}
	for _, bin := range []string{"unshare", "setpriv"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("needs %s: run just test-serve", bin)
		}
	}
	if out, err := exec.Command("unshare", append(slices.Clone(namespaceArgs), "true")...).CombinedOutput(); err != nil {
		t.Skipf("needs a user namespace with subordinate uids: %v %s", err, out)
	}
	// Uid 1000 of the namespace is a subordinate uid on the host, which
	// can read nothing of this user's, so the binary goes where it may.
	bins := t.TempDir()
	if err := os.Chmod(bins, 0o755); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bins, "tofu.test"), b, 0o755); err != nil {
		t.Fatal(err)
	}
	env := []string{nonRootRootEnv + "=" + t.TempDir(), nonRootBinsEnv + "=" + bins}
	if out, err := exec.Command("systemd-run", "--user", "--scope", "--quiet", "--collect", "-p", "Delegate=yes", "true").CombinedOutput(); err != nil {
		t.Logf("no delegated cgroup, so no limits: systemd-run --user: %v %s", err, out)
		inNamespaces(t, env)
		return
	}
	inDelegatedScope(t, "TestPlanAsRunUserWithoutRoot", nil, append(env, nonRootStageEnv+"=scope")...)
}

// inNamespaces runs this test again as root of new namespaces.
func inNamespaces(t *testing.T, env []string) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("unshare", append(slices.Clone(namespaceArgs), self, "-test.run=^TestPlanAsRunUserWithoutRoot$", "-test.v", "-test.count=1")...)
	cmd.Env = append(append(os.Environ(), env...), nonRootStageEnv+"=root")
	out, err := cmd.CombinedOutput()
	t.Logf("in the namespaces:\n%s", out)
	switch {
	case strings.Contains(string(out), "--- SKIP: TestPlanAsRunUserWithoutRoot"):
		t.Skip("the test skipped in the namespaces")
	case err != nil || !strings.Contains(string(out), "--- PASS: TestPlanAsRunUserWithoutRoot"):
		t.Fatalf("the test failed in the namespaces: %v", err)
	}
}

// delegateToWrit runs alone in a delegated scope's cgroup. It moves into
// a leaf of it, gives its children cgroupControllers, makes the child
// that writ will have, and runs the test again in namespaces.
func delegateToWrit(t *testing.T) {
	own, err := ownCgroup()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"harness", "writ"} {
		if err := os.Mkdir(filepath.Join(own, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeCgroup(filepath.Join(own, "harness"), "cgroup.procs", "0"); err != nil {
		t.Fatal(err)
	}
	if err := writeCgroup(own, "cgroup.subtree_control", "+memory +pids +cpu"); err != nil {
		t.Fatal(err)
	}
	inNamespaces(t, []string{nonRootCgroupEnv + "=" + filepath.Join(own, "writ")})
}

// becomeWrit runs as root of the namespaces. It makes a tmpfs the root
// directory and runs the test again as uid 1000 with writ serve's
// capabilities.
func becomeWrit(t *testing.T) {
	// The new root holds the store but not a profile's links into it.
	setpriv, err := exec.LookPath("setpriv")
	if err == nil {
		setpriv, err = filepath.EvalSymlinks(setpriv)
	}
	if err != nil {
		t.Fatal(err)
	}
	bins := os.Getenv(nonRootBinsEnv)
	extra := []string{bins}
	if cg := os.Getenv(nonRootCgroupEnv); cg != "" {
		// Into the cgroup that systemd would give writ serve's unit,
		// delegated to its user, uid 1000, as Delegate=yes delegates it.
		if err := writeCgroup(cg, "cgroup.procs", "0"); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{"", "cgroup.procs", "cgroup.subtree_control", "cgroup.threads"} {
			if err := os.Chown(filepath.Join(cg, p), 1000, 1000); err != nil {
				t.Fatal(err)
			}
		}
		extra = append(extra, cgroupMount)
	}
	enterOwnRoot(t, os.Getenv(nonRootRootEnv), extra...)
	os.Setenv(nonRootStageEnv, "writ")
	err = syscall.Exec(setpriv, []string{"setpriv", "--reuid=1000", "--regid=1000", "--clear-groups",
		"--inh-caps=+setuid,+setgid,+kill", "--ambient-caps=+setuid,+setgid,+kill", "--no-new-privs",
		filepath.Join(bins, "tofu.test"), "-test.run=^TestPlanAsRunUserWithoutRoot$", "-test.v", "-test.count=1"}, os.Environ())
	t.Fatalf("exec setpriv: %v", err)
}

func planAsRunUserWithoutRoot(t *testing.T) {
	if os.Getuid() != 1000 {
		t.Fatalf("the child runs as uid %d, not 1000", os.Getuid())
	}
	if err := provider.PrepareIsolation(); errors.Is(err, syscall.ENOTSUP) {
		t.Skip("clearing the ambient capabilities needs a build without cgo, as writ ships: run just test-serve")
	} else if err != nil {
		t.Fatal(err)
	}
	// The run's uid makes its directory in the scratch root and reads the
	// plugin cache.
	scratch, cache := t.TempDir(), t.TempDir()
	for d, mode := range map[string]os.FileMode{filepath.Dir(scratch): 0o755, scratch: os.ModeSticky | 0o777, cache: 0o755} {
		if err := os.Chmod(d, mode); err != nil {
			t.Fatal(err)
		}
	}
	r := newRunnerOpts(t, Options{Binary: fakeTofu(t, "marks"), PluginCache: cache, ScratchRoot: scratch, Timeout: time.Minute, ApplyTimeout: time.Minute, MaxOutput: 1 << 20})
	ctx := context.Background()
	plan := func(uid uint32) error {
		s := fakeStack()
		s.User = &User{UID: uid, GID: uid}
		p, err := r.Plan(ctx, s)
		if se, ok := errors.AsType[*StepError](err); ok {
			return errors.Join(err, errors.New(string(se.Stderr)))
		} else if err != nil {
			return err
		}
		if f := strings.Fields(string(p.Log)); len(f) != 3 || f[2] != "<nil>" {
			return errors.New("the plan reported " + string(p.Log))
		}
		return nil
	}

	t.Run("a plan as the run's uid", func(t *testing.T) {
		if err := plan(61000); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a plan as the run's uid stops at its memory limit", func(t *testing.T) {
		if os.Getenv(nonRootCgroupEnv) == "" {
			t.Skip("no delegated cgroup: systemd-run --user could not make one")
		}
		r := newRunnerOpts(t, Options{Binary: fakeTofu(t, "alloc"), PluginCache: cache, ScratchRoot: scratch, Timeout: time.Minute, ApplyTimeout: time.Minute, MaxOutput: 1 << 20, Limits: testLimits})
		if e := r.Enforcement(); e.Cgroup == "" {
			t.Fatalf("in a cgroup delegated to writ, limits are off: %s", e.CgroupOff)
		}
		s := fakeStack()
		s.User = &User{UID: 61030, GID: 61030}
		_, err := r.Plan(ctx, s)
		if se, ok := errors.AsType[*StepError](err); !ok || !strings.Contains(se.Limit, "limits.run_memory") {
			t.Fatalf("the plan returned %v, not a stop at limits.run_memory", err)
		}
		t.Log(err)
	})

	// writ reads the step's /proc/PID/fd, which belongs to the run's uid,
	// at each measure of the run's directory.
	limited := func(t *testing.T, behaviour string, l Limits) (*Runner, Stack) {
		t.Helper()
		if os.Getenv(nonRootCgroupEnv) == "" {
			t.Skip("no delegated cgroup: systemd-run --user could not make one")
		}
		r := newRunnerOpts(t, Options{Binary: fakeTofu(t, behaviour), PluginCache: cache, ScratchRoot: scratch, Timeout: time.Minute, ApplyTimeout: time.Minute, MaxOutput: 1 << 20, Limits: l})
		if e := r.Enforcement(); e.Cgroup == "" {
			t.Fatalf("in a cgroup delegated to writ, limits are off: %s", e.CgroupOff)
		}
		s := fakeStack()
		s.User = &User{UID: 61040, GID: 61040}
		return r, s
	}

	t.Run("a step longer than a measure passes as the run's uid", func(t *testing.T) {
		r, s := limited(t, "nap", testLimits)
		if _, err := r.Plan(ctx, s); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a removed file the run's uid holds stops at limits.run_scratch", func(t *testing.T) {
		// Room in memory for the page cache of what the fake writes.
		l := testLimits
		l.Memory = 1 << 30
		for _, fake := range []string{"scratch-unlinked", "scratch-undumpable"} {
			r, s := limited(t, fake, l)
			_, err := r.Plan(ctx, s)
			if se, ok := errors.AsType[*StepError](err); !ok || !strings.Contains(se.Limit, "held more than limits.run_scratch") {
				t.Fatalf("%s: the plan returned %v, not a stop at limits.run_scratch", fake, err)
			}
			t.Logf("%s: %v", fake, err)
		}
	})

	t.Run("plans while other runs act as their uids", func(t *testing.T) {
		stop := make(chan struct{})
		var wg sync.WaitGroup
		for i := range 4 {
			wg.Go(func() {
				u := runas.User{UID: 61010 + uint32(i), GID: 61010 + uint32(i)}
				for {
					select {
					case <-stop:
						return
					default:
						_ = runas.AsUser(u, func() error { return nil })
					}
				}
			})
		}
		var errs []error
		for i := range 20 {
			errs = append(errs, plan(61020+uint32(i%4)))
		}
		close(stop)
		wg.Wait()
		if err := errors.Join(errs...); err != nil {
			t.Fatal(err)
		}
	})
}
