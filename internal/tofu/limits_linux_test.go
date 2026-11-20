package tofu

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
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
// unit, with the unit properties props. It logs the output, or skips t
// where systemd-run --user cannot make the scope.
func inDelegatedScope(t *testing.T, test string, props []string, env ...string) {
	t.Helper()
	self := systemdRunSelf(t)
	args := []string{"--user", "--scope", "--quiet", "--collect", "-p", "Delegate=yes"}
	for _, p := range props {
		args = append(args, "-p", p)
	}
	cmd := exec.Command("systemd-run", append(args, "--", self, "-test.run=^"+test+"$", "-test.v", "-test.count=1")...)
	cmd.Env = append(os.Environ(), env...)
	runInUnit(t, test, cmd)
}

// inDelegatedService runs this test binary again as test, as
// inDelegatedScope does, but as a transient service, which systemd starts
// itself and can confine with properties such as RestrictNamespaces, as it
// does writ serve's unit. The service gets env and this process's TMPDIR,
// and no other environment.
func inDelegatedService(t *testing.T, test string, props []string, env ...string) {
	t.Helper()
	self := systemdRunSelf(t)
	args := []string{"--user", "--wait", "--pipe", "--quiet", "--collect", "-p", "Delegate=yes", "--setenv=TMPDIR=" + os.TempDir()}
	for _, p := range props {
		args = append(args, "-p", p)
	}
	for _, e := range env {
		args = append(args, "--setenv="+e)
	}
	runInUnit(t, test, exec.Command("systemd-run", append(args, "--", self, "-test.run=^"+test+"$", "-test.v", "-test.count=1")...))
}

// systemdRunSelf returns this test binary's path, or skips t where
// systemd-run --user cannot make a delegated scope.
func systemdRunSelf(t *testing.T) string {
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
	return self
}

func runInUnit(t *testing.T, test string, cmd *exec.Cmd) {
	t.Helper()
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
	inDelegatedScope(t, "TestRunLimits", nil, limitsStageEnv+"=scope")
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
	t.Run("scratch, in a removed file a process holds", func(t *testing.T) {
		for _, fake := range []string{"scratch-unlinked", "scratch-mapped", "scratch-zombie"} {
			l := testLimits
			l.Memory = 1 << 30
			r := fakeRunnerLimits(t, fake, l)
			r.opts.Timeout = 15 * time.Second
			start := time.Now()
			requireLimit(t, r, "limits.run_scratch")
			if d := time.Since(start); d > 10*time.Second {
				t.Fatalf("the step that filled a removed file ran %v, so writ's measure did not stop it", d)
			}
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
		// systemd runs a delegated unit's ExecStartPre there. e.Cgroup is
		// runsCgroup, in the unit's cgroup.
		unit := filepath.Dir(e.Cgroup)
		control := filepath.Join(unit, ".control")
		if err := os.Mkdir(control, 0o755); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(control)
		if err := writeCgroup(control, "cgroup.procs", "0"); err != nil {
			t.Fatal(err)
		}
		defer writeCgroup(filepath.Join(unit, serveCgroup), "cgroup.procs", "0")
		if got := CheckLimits(testLimits); got.Cgroup != e.Cgroup {
			t.Fatalf("--check reports the cgroup %q (%s), not the unit's %s", got.Cgroup, got.CgroupOff, e.Cgroup)
		}
		if got := CheckLimits(testLimits); got.StepsPids != e.StepsPids || got.StepsMemory != e.StepsMemory {
			t.Fatalf("--check reports the steps' limits %d and %d, not writ serve's %d and %d", got.StepsPids, got.StepsMemory, e.StepsPids, e.StepsMemory)
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
	inDelegatedScope(t, "TestRunLimitsMountsCgroupFS", nil, limitsStageEnv+"=nomad-scope")
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

// TestRunLimitsUnderTheShippedUnit plans in a delegated service confined
// as the NixOS module confines writ serve: RestrictNamespaces="user mnt
// pid ipc", under which the kernel answers clone3 with ENOSYS, and
// NoNewPrivileges. --check must report what a step then gets, and a step
// must start in its cgroup.
func TestRunLimitsUnderTheShippedUnit(t *testing.T) {
	if os.Getenv(limitsStageEnv) == "restricted" {
		e := CheckLimits(testLimits)
		if e.Cgroup == "" {
			t.Fatalf("--check reports limits off: %s", e.CgroupOff)
		}
		r := fakeRunnerLimits(t, "cgroup", testLimits)
		if got := r.Enforcement(); got.Cgroup != e.Cgroup {
			t.Fatalf("writ serve makes each step's cgroup under %q (%s), but --check said %s", got.Cgroup, got.CgroupOff, e.Cgroup)
		}
		p, err := r.Plan(context.Background(), fakeStack())
		if err != nil {
			t.Fatal(err)
		}
		if cg := strings.TrimSpace(string(p.JSON)); !strings.HasPrefix(filepath.Base(cg), "run-") {
			t.Fatalf("the step ran in the cgroup %q, not one of its own", cg)
		}
		return
	}
	inDelegatedService(t, "TestRunLimitsUnderTheShippedUnit", []string{"RestrictNamespaces=user mnt pid ipc", "NoNewPrivileges=yes"}, limitsStageEnv+"=restricted")
}

// TestRunLimitsLeaveRoomForServe runs four steps at once that each start
// processes until the kernel refuses one, in a delegated scope whose unit
// allows 200 processes and threads, fewer than the four steps' run_pids
// together. writ serve shares the unit's limits with every step, so the
// steps together must leave it room to start processes and threads. Each
// step names the limit it reached, its own run_pids or the unit's
// TasksMax, which the steps share.
func TestRunLimitsLeaveRoomForServe(t *testing.T) {
	if os.Getenv(limitsStageEnv) != "tasks" {
		inDelegatedScope(t, "TestRunLimitsLeaveRoomForServe", []string{"TasksMax=200"}, limitsStageEnv+"=tasks")
		return
	}
	l := testLimits
	l.Memory = 1 << 30
	r := fakeRunnerLimits(t, "hold", l)
	e := r.Enforcement()
	if e.Cgroup == "" {
		t.Fatalf("in a delegated cgroup, limits are off: %s", e.CgroupOff)
	}
	// The unit's 200, less the 128 that writ serve keeps.
	if text, err := os.ReadFile(filepath.Join(e.Cgroup, "pids.max")); err != nil || strings.TrimSpace(string(text)) != "72" || e.StepsPids != 72 {
		t.Errorf("the steps share pids.max %q (%v), and Enforcement says %d, not 72", text, err, e.StepsPids)
	}
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Go(func() { _, errs[i] = r.Plan(context.Background(), fakeStack()) })
	}
	defer func() {
		wg.Wait()
		shared := 0
		for _, err := range errs {
			se, ok := errors.AsType[*StepError](err)
			if !ok || se.Limit == "" {
				t.Errorf("a step returned %v, which names no limit", err)
				continue
			}
			if strings.Contains(se.Limit, "TasksMax") {
				shared++
			}
			t.Log(se.Limit)
		}
		if shared == 0 {
			t.Error("no step named the limit the steps share")
		}
	}()
	// The fakes hold their processes for six seconds.
	time.Sleep(3 * time.Second)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(self, "-test.run=^$").CombinedOutput(); err != nil {
		t.Errorf("writ could not start a process while the steps held theirs: %v %s", err, out)
	}
	// Threads of writ's own, each blocked in a system call, as a busy
	// writ serve has; Go dies when it can't start one.
	var threads sync.WaitGroup
	for range 32 {
		threads.Go(func() {
			runtime.LockOSThread()
			time.Sleep(100 * time.Millisecond)
			_ = syscall.Nanosleep(&syscall.Timespec{Nsec: 100e6}, nil)
		})
	}
	threads.Wait()
}

// TestRunLimitsNameTheSharedMemory runs a step that allocates 256MiB,
// within its run_memory of 1GiB, in a delegated scope whose unit allows
// 640MiB: 128MiB once writ serve keeps its 512MiB. The kernel kills the
// step for the memory every step shares, and the step names that limit,
// not its own.
func TestRunLimitsNameTheSharedMemory(t *testing.T) {
	if os.Getenv(limitsStageEnv) != "memory" {
		inDelegatedScope(t, "TestRunLimitsNameTheSharedMemory", []string{"MemoryMax=640M", "MemorySwapMax=0"}, limitsStageEnv+"=memory")
		return
	}
	l := testLimits
	l.Memory = 1 << 30
	r := fakeRunnerLimits(t, "alloc", l)
	if e := r.Enforcement(); e.StepsMemory != 128<<20 {
		t.Fatalf("the steps share %d bytes of memory (%s), not 128MiB", e.StepsMemory, e.CgroupOff)
	}
	requireLimit(t, r, "MemoryMax")
}

// TestRunLimitsLeaveNoStaleSharedLimits starts writ serve's cgroups where
// an earlier start left runs with pids.max and memory.max that the unit no
// longer sets, in a delegated scope whose unit sets neither. The steps
// must not stay bound by limits of a unit that is gone. writ serve writes
// none of them as max: a writ serve that starts again in its cgroup finds
// the controllers off for its children, as a cgroup whose children have
// them may hold no process, and the kernel gives runs fresh limits when
// writ serve enables them again.
func TestRunLimitsLeaveNoStaleSharedLimits(t *testing.T) {
	if os.Getenv(limitsStageEnv) != "stale" {
		inDelegatedScope(t, "TestRunLimitsLeaveNoStaleSharedLimits", []string{"TasksMax=infinity", "MemoryMax=infinity"}, limitsStageEnv+"=stale")
		return
	}
	own, err := ownCgroup()
	if err != nil {
		t.Fatal(err)
	}
	// As a start before did: writ serve in its leaf, and runs with limits.
	for _, d := range []string{serveCgroup, runsCgroup} {
		if err := os.Mkdir(filepath.Join(own, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeCgroup(filepath.Join(own, serveCgroup), "cgroup.procs", "0"); err != nil {
		t.Fatal(err)
	}
	if err := writeCgroup(own, "cgroup.subtree_control", "+memory +pids"); err != nil {
		t.Fatal(err)
	}
	runs := filepath.Join(own, runsCgroup)
	for file, n := range map[string]string{"pids.max": "50", "memory.max": "104857600"} {
		if err := writeCgroup(runs, file, n); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeCgroup(own, "cgroup.subtree_control", "-memory -pids"); err != nil {
		t.Fatal(err)
	}
	if err := writeCgroup(own, "cgroup.procs", "0"); err != nil {
		t.Fatal(err)
	}
	set, err := cgroupBase(true)
	if err != nil {
		t.Fatal(err)
	}
	if set.memory != 0 {
		t.Skipf("a cgroup above the scope sets memory.max, so the steps share %d bytes", set.memory)
	}
	for file, n := range map[string]int64{"pids.max": set.pids, "memory.max": set.memory} {
		text, err := os.ReadFile(filepath.Join(runs, file))
		if err != nil {
			t.Fatal(err)
		}
		want := "max"
		if n > 0 {
			want = strconv.FormatInt(n, 10)
		}
		if got := strings.TrimSpace(string(text)); got != want {
			t.Errorf("runs has %s %s, not %s", file, got, want)
		}
	}
}

// writ serve --check runs the checks that writ serve's start does: that
// a cgroup has cgroup.kill, and whether the kernel holds a step to no
// swap. A directory stands in for a kernel's cgroup that lacks the file.
func TestCgroupChecks(t *testing.T) {
	bare := t.TempDir()
	if err := checkKill(bare); err == nil || !strings.Contains(err.Error(), "cgroup.kill") {
		t.Errorf("a cgroup without cgroup.kill passed: %v", err)
	}
	if _, err := os.Stat("/proc/swaps"); err != nil {
		t.Skip("this kernel has no swap")
	}
	if why := swapOff(bare); !strings.Contains(why, "memory.swap.max") {
		t.Errorf("a cgroup without memory.swap.max holds a step to no swap: %q", why)
	}
	held := t.TempDir()
	if err := os.WriteFile(filepath.Join(held, "memory.swap.max"), []byte("max\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if why := swapOff(held); why != "" {
		t.Errorf("a cgroup with memory.swap.max: %q", why)
	}
}

// cgroupStandIn returns a directory whose cgroup.procs and cgroup.threads
// list this process and its threads, each times times, as a step's
// cgroup lists its processes: a measure of held files reads only those.
func cgroupStandIn(t *testing.T, times int) *stepCgroup {
	t.Helper()
	tasks, err := os.ReadDir("/proc/self/task")
	if err != nil {
		t.Fatal(err)
	}
	var procs, threads strings.Builder
	for range times {
		procs.WriteString(strconv.Itoa(os.Getpid()) + "\n")
		for _, task := range tasks {
			threads.WriteString(task.Name() + "\n")
		}
	}
	dir := t.TempDir()
	for file, text := range map[string]string{"cgroup.procs": procs.String(), "cgroup.threads": threads.String()} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &stepCgroup{dir: dir}
}

// scratchStandIn returns a run's directory to measure, empty.
func scratchStandIn(t *testing.T) *scratch {
	t.Helper()
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return &scratch{run: root}
}

// A removed file that a step's process holds counts on whatever
// filesystem it lies: /proc/PID/maps names a file of a btrfs subvolume,
// which a run may make in its own directory, by the device of the
// filesystem, not the subvolume's device that stat gives, so a measure
// that keeps only the files of the run directory's device misses it. A
// file on /dev/shm stands in for one. A file both open and mapped counts
// once.
func TestScratchCountsHeldFilesOnAnyDevice(t *testing.T) {
	shm, err := os.MkdirTemp("/dev/shm", "writ-held-")
	if err != nil {
		t.Skipf("needs /dev/shm: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(shm) })
	sc := scratchStandIn(t)
	var a, b syscall.Stat_t
	if err := syscall.Stat(shm, &a); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Stat(os.TempDir(), &b); err != nil {
		t.Fatal(err)
	}
	if a.Dev == b.Dev {
		t.Skip("/dev/shm and TMPDIR are one filesystem")
	}
	const size = 4 << 20
	for _, how := range []string{"open", "mapped", "open and mapped"} {
		t.Run(how, func(t *testing.T) {
			f, err := os.CreateTemp(shm, "held-")
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if err := os.Remove(f.Name()); err != nil {
				t.Fatal(err)
			}
			if _, err := f.Write(make([]byte, size)); err != nil {
				t.Fatal(err)
			}
			if how != "open" {
				mem, err := syscall.Mmap(int(f.Fd()), 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
				if err != nil {
					t.Fatal(err)
				}
				defer syscall.Munmap(mem)
			}
			if how == "mapped" {
				if err := f.Close(); err != nil {
					t.Fatal(err)
				}
			}
			n, err := sc.usage(1<<40, cgroupStandIn(t, 1), time.Now().Add(scratchWithin))
			if err != nil {
				t.Fatal(err)
			}
			if n < size || n >= 2*size {
				t.Fatalf("the measure counted %d bytes, not the %d of the removed file this process holds, once", n, size)
			}
		})
	}
}

// A measure of the removed files a step's processes hold is bounded as a
// walk is: one past its deadline is as one over the limit, so a run that
// makes itself slow to read in /proc doesn't escape limits.run_scratch.
// A cgroup that lists this process many times stands in for one slow to
// read.
func TestScratchHeldMeasureIsBounded(t *testing.T) {
	sc := scratchStandIn(t)
	cg := cgroupStandIn(t, 20000)
	start := time.Now()
	_, err := sc.usage(1<<40, cg, time.Now().Add(100*time.Millisecond))
	if !errors.Is(err, errScratchSlow) {
		t.Fatalf("a measure past its deadline returned %v, not errScratchSlow", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("a measure with 100ms to run took %v", d)
	}
}

// A file with several links in the run's directory costs its blocks once
// in all, and at least scratchInode: the measure charges each link a
// share, with no record of the files it met.
func TestScratchChargesLinksAShare(t *testing.T) {
	for _, c := range []struct {
		name        string
		size, links int
		want        int64
	}{
		{"a file of 1MiB with 4 links", 1 << 20, 4, 1 << 20},
		{"an empty file with 3 links", 0, 3, scratchInode},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			first := filepath.Join(dir, "0")
			if err := os.WriteFile(first, make([]byte, c.size), 0o600); err != nil {
				t.Fatal(err)
			}
			for i := 1; i < c.links; i++ {
				if err := os.Link(first, filepath.Join(dir, strconv.Itoa(i))); err != nil {
					t.Fatal(err)
				}
			}
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			var st syscall.Stat_t
			if err := syscall.Stat(dir, &st); err != nil {
				t.Fatal(err)
			}
			dirCost := max(st.Blocks*512, scratchInode)
			n, err := (&scratch{run: root}).usage(1<<40, nil, time.Now().Add(scratchWithin))
			if err != nil {
				t.Fatal(err)
			}
			// Each share rounds up by less than a byte per link.
			if got := n - dirCost; got < c.want || got > c.want+int64(c.links) {
				t.Fatalf("the file counted %d bytes, not %d", got, c.want)
			}
		})
	}
}
