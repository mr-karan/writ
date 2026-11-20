package tofu

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// limitsStageEnv tells this test binary, run again, which stage of a test
// in a delegated cgroup it is.
const limitsStageEnv = "WRIT_TOFU_LIMITS_STAGE"

// inDelegatedScope runs this test binary again, with env, as test, in a
// transient systemd scope of the user's own whose cgroup is delegated to
// the user and holds only that process, as Delegate=yes gives writ serve's
// unit. It returns the output, or skips t where systemd-run --user cannot
// make the scope.
func inDelegatedScope(t *testing.T, test string, env ...string) {
	t.Helper()
	if _, err := exec.LookPath("systemd-run"); err != nil {
		t.Skip("needs systemd-run --user for a delegated cgroup: run just test-serve on a host with a systemd user session")
	}
	if out, err := exec.Command("systemd-run", "--user", "--scope", "--quiet", "--collect", "-p", "Delegate=yes", "true").CombinedOutput(); err != nil {
		t.Skipf("needs systemd-run --user for a delegated cgroup: %v %s", err, out)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("systemd-run", "--user", "--scope", "--quiet", "--collect", "-p", "Delegate=yes", "--",
		self, "-test.run=^"+test+"$", "-test.v", "-test.count=1")
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	t.Logf("in a delegated cgroup:\n%s", out)
	switch {
	case strings.Contains(string(out), "--- SKIP: "+test):
		t.Skip("the test skipped in the delegated cgroup")
	case err != nil || !strings.Contains(string(out), "--- PASS: "+test):
		t.Fatalf("the test failed in the delegated cgroup: %v", err)
	}
}

// testLimits are the limits TestRunLimits sets, small enough that the
// fakes pass each at once.
var testLimits = Limits{Memory: 64 << 20, Pids: 64, CPUs: 0.5, Scratch: 16 << 20}

// TestRunLimits proves Options.Limits on this machine. In the test's own
// cgroup, which holds other processes, a Runner cannot make cgroups: it
// says why, and runs each step without them, so a step may use more memory
// than Limits allows, while the scratch limit, which needs no cgroup,
// still holds. In a cgroup delegated to the test, as systemd's
// Delegate=yes gives writ serve's unit, each step runs in a cgroup of its
// own that is gone when the step ends: a step that allocates past Memory,
// starts processes past Pids, or fills its directory past Scratch is
// stopped and its error names the limit, and four busy threads get no more
// than CPUs of CPU time.
func TestRunLimits(t *testing.T) {
	if os.Getenv(limitsStageEnv) == "scope" {
		runLimits(t)
		return
	}
	t.Run("without a delegated cgroup, limits are off and say why", func(t *testing.T) {
		r := fakeRunnerLimits(t, "alloc", testLimits)
		e := r.Enforcement()
		if e.Cgroup != "" {
			t.Skipf("the test's own cgroup %s let writ make cgroups, so this host shows no degraded case", e.Cgroup)
		}
		if e.CgroupOff == "" {
			t.Fatal("limits are off and Enforcement gives no reason")
		}
		t.Logf("off: %s", e.CgroupOff)
		_, err := r.Plan(context.Background(), fakeStack())
		if se, ok := errors.AsType[*StepError](err); ok && se.Limit != "" {
			t.Fatalf("a step stopped at a limit writ said it does not enforce: %v", err)
		}
	})
	t.Run("the scratch limit holds without a cgroup", func(t *testing.T) {
		requireLimit(t, fakeRunnerLimits(t, "scratch-exit", testLimits), "limits.run_scratch")
	})
	inDelegatedScope(t, "TestRunLimits", limitsStageEnv+"=scope")
}

func runLimits(t *testing.T) {
	r := fakeRunnerLimits(t, "cgroup", testLimits)
	e := r.Enforcement()
	if e.Cgroup == "" {
		t.Fatalf("in a delegated cgroup, limits are off: %s", e.CgroupOff)
	}
	t.Run("each step runs in a cgroup of its own, which ends with it", func(t *testing.T) {
		s := fakeStack()
		p, err := r.Plan(context.Background(), s)
		if err != nil {
			t.Fatal(err)
		}
		for _, out := range [][]byte{p.Log, p.JSON} {
			cg := strings.TrimPrefix(strings.TrimSpace(string(out)), "0::")
			if !strings.HasPrefix(filepath.Base(cg), "run-") || !strings.HasSuffix(e.Cgroup, filepath.Dir(cg)) {
				t.Errorf("a step ran in the cgroup %q, not in one of its own under %s", cg, e.Cgroup)
			}
		}
		requireNoStepCgroups(t, e.Cgroup)
	})
	t.Run("memory", func(t *testing.T) {
		requireLimit(t, fakeRunnerLimits(t, "alloc", testLimits), "limits.run_memory")
	})
	t.Run("pids", func(t *testing.T) {
		// Room in memory for the fake's processes, which the race
		// detector makes several times larger.
		l := testLimits
		l.Memory = 1 << 30
		r := fakeRunnerLimits(t, "forkbomb", l)
		requireLimit(t, r, "limits.run_pids")
		requireNoneLeft(t, r.opts.Binary)
	})
	t.Run("scratch, while the step runs", func(t *testing.T) {
		// Room in memory for the page cache of what the fake writes, which
		// counts against memory.max.
		l := testLimits
		l.Memory = 1 << 30
		start := time.Now()
		requireLimit(t, fakeRunnerLimits(t, "scratch", l), "limits.run_scratch")
		if d := time.Since(start); d > 10*time.Second {
			t.Fatalf("the step that filled its directory ran %v, so writ's measure did not stop it", d)
		}
	})
	t.Run("a process left behind dies with the step's cgroup", func(t *testing.T) {
		r := fakeRunnerLimits(t, "leave", testLimits)
		if _, err := r.Plan(context.Background(), fakeStack()); err == nil {
			t.Fatal("the fake's plan passed")
		}
		requireNoneLeft(t, r.opts.Binary)
		requireNoStepCgroups(t, e.Cgroup)
	})
	t.Run("cpu", func(t *testing.T) {
		r := fakeRunnerLimits(t, "spin", testLimits)
		_, err := r.Plan(context.Background(), fakeStack())
		se, ok := errors.AsType[*StepError](err)
		if !ok {
			t.Fatalf("the fake returned %v", err)
		}
		f := strings.Fields(string(se.Stdout))
		if len(f) != 4 || f[0] != "cpu" {
			t.Fatalf("the fake printed %q", se.Stdout)
		}
		cpu, _ := strconv.ParseFloat(f[1], 64)
		wall, _ := strconv.ParseFloat(f[3], 64)
		// Unbounded, four busy threads take four CPUs. Load on the host
		// only lowers what they get.
		if bound := testLimits.CPUs * wall * 1.5; cpu > bound {
			t.Fatalf("four busy threads took %.2fs of CPU in %.2fs, past %v CPUs (%.2fs with slack)", cpu, wall, testLimits.CPUs, bound)
		}
		t.Logf("four busy threads took %.2fs of CPU in %.2fs under %v CPUs", cpu, wall, testLimits.CPUs)
	})
	t.Run("--check in systemd's .control reports the unit's cgroup", func(t *testing.T) {
		// systemd runs a delegated unit's ExecStartPre there.
		control := filepath.Join(e.Cgroup, ".control")
		if err := os.Mkdir(control, 0o755); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(control)
		if err := writeCgroup(control, "cgroup.procs", "0"); err != nil {
			t.Fatal(err)
		}
		defer writeCgroup(filepath.Join(e.Cgroup, serveCgroup), "cgroup.procs", "0")
		if got := CheckLimits(testLimits); got.Cgroup != e.Cgroup {
			t.Fatalf("--check reports the cgroup %q (%s), not the unit's %s", got.Cgroup, got.CgroupOff, e.Cgroup)
		}
	})
}

func fakeRunnerLimits(t *testing.T, behaviour string, l Limits) *Runner {
	t.Helper()
	return newRunnerOpts(t, Options{Binary: fakeTofu(t, behaviour), PluginCache: t.TempDir(), ScratchRoot: t.TempDir(),
		Timeout: time.Minute, ApplyTimeout: time.Minute, MaxOutput: 1 << 20, Limits: l})
}

// requireLimit plans with r's fake and requires the plan to stop at the
// limit named want.
func requireLimit(t *testing.T, r *Runner, want string) {
	t.Helper()
	_, err := r.Plan(context.Background(), fakeStack())
	se, ok := errors.AsType[*StepError](err)
	if !ok || !strings.Contains(se.Limit, want) {
		t.Fatalf("the plan returned %v, not a stop at %s", err, want)
	}
	if !strings.Contains(se.Error(), want) {
		t.Fatalf("the step's error %q does not name %s", se.Error(), want)
	}
	t.Log(se.Error())
}

// requireNoStepCgroups fails t if base holds a step's cgroup.
func requireNoStepCgroups(t *testing.T, base string) {
	t.Helper()
	ents, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if e.IsDir() && strings.HasPrefix(e.Name(), "run-") {
			t.Errorf("the step's cgroup %s outlived the step", e.Name())
		}
	}
}

// TestRunLimitsMountsCgroupFS runs writ as a task of Nomad's exec driver
// runs it: as root, alone in its cgroup, in a mount namespace whose /sys
// is read-only, with no cgroup filesystem mounted. writ mounts the
// cgroup v2 filesystem at /sys/fs/cgroup itself and gives each step a
// cgroup under its own. Root here is root of a user namespace that owns a
// cgroup namespace rooted at the test's cgroup, which lets it mount the
// filesystem as real root may anywhere.
func TestRunLimitsMountsCgroupFS(t *testing.T) {
	switch os.Getenv(limitsStageEnv) {
	case "nomad-scope":
		intoOwnCgroupNamespace(t)
		return
	case "nomad":
		limitsWithoutCgroupFS(t)
		return
	}
	if _, err := exec.LookPath("unshare"); err != nil {
		t.Skip("needs unshare: run just test-serve")
	}
	inDelegatedScope(t, "TestRunLimitsMountsCgroupFS", limitsStageEnv+"=nomad-scope")
}

// intoOwnCgroupNamespace runs alone in a delegated cgroup. It moves into a
// leaf of it, gives its children cgroupControllers, and runs the test
// again in another child, alone, as root of new user, mount, network, and
// cgroup namespaces.
func intoOwnCgroupNamespace(t *testing.T) {
	own, err := ownCgroup()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"harness", "task"} {
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
	task, err := os.Open(filepath.Join(own, "task"))
	if err != nil {
		t.Fatal(err)
	}
	defer task.Close()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// No pid namespace: unshare would stay behind in the task's cgroup to
	// wait for its child.
	cmd := exec.Command("unshare", "--user", "--map-root-user", "--mount", "--net", "--cgroup", self, "-test.run=^TestRunLimitsMountsCgroupFS$", "-test.v", "-test.count=1")
	cmd.Env = append(os.Environ(), limitsStageEnv+"=nomad", nonRootRootEnv+"="+t.TempDir())
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(task.Fd())}
	out, err := cmd.CombinedOutput()
	t.Logf("as root of a mount namespace with no cgroup filesystem:\n%s", out)
	if err != nil || !strings.Contains(string(out), "--- PASS: TestRunLimitsMountsCgroupFS") {
		t.Fatalf("the test failed: %v", err)
	}
}

func limitsWithoutCgroupFS(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	enterOwnRoot(t, os.Getenv(nonRootRootEnv), filepath.Dir(self))
	// A read-only /sys with an empty fs/cgroup, as the sysfs that the
	// exec driver mounts has. The kernel refuses a user namespace a sysfs
	// of its own where no sysfs is mounted, so a tmpfs stands in for it.
	if err := os.MkdirAll("/sys", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mount("tmpfs", "/sys", "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, "mode=0755"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cgroupMount, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mount("", "/sys", "", syscall.MS_REMOUNT|syscall.MS_RDONLY|syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, ""); err != nil {
		t.Fatal(err)
	}
	if mounts, err := cgroup2Mounts(); err != nil || len(mounts) > 0 {
		t.Fatalf("the cgroup filesystem is mounted already: %v %v", mounts, err)
	}
	r := fakeRunnerLimits(t, "alloc", testLimits)
	e := r.Enforcement()
	if e.Cgroup == "" {
		t.Fatalf("limits are off: %s", e.CgroupOff)
	}
	t.Logf("each step's cgroup is under %s", e.Cgroup)
	requireLimit(t, r, "limits.run_memory")
}

// A run that swaps a directory of its own for a FIFO while writ measures
// its directory must not block the measure: an open of a FIFO waits for a
// writer that never comes, and the step would never end.
func TestScratchMeasureMeetsAFIFO(t *testing.T) {
	r := fakeRunnerLimits(t, "fifo-swap", Limits{Scratch: 16 << 20})
	r.opts.Timeout = 3 * time.Second
	if err := planWithin(t, r, 30*time.Second); err == nil {
		t.Fatal("the fake's plan passed")
	}
}

// Each file costs the filesystem an inode, though it holds no data, so the
// measure charges it at least scratchInode: a run of empty files must
// still meet limits.run_scratch.
func TestScratchCountsEachInode(t *testing.T) {
	requireLimit(t, fakeRunnerLimits(t, "scratch-inodes", testLimits), "limits.run_scratch")
}
