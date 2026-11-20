//go:build linux

package tofu

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	"github.com/mr-karan/writ/internal/runas"
)

// These tests run this test binary in tofu's place, to make tofu misbehave
// in ways the real binary does only by chance. It runs inside the run's
// filesystem view, as tofu does, which holds no shell and no tool, so the
// fakes are Go. A fake is a symbolic link to the binary whose name,
// fake-tofu-BEHAVIOUR, says what it does. Every fake answers `tofu version`
// as OpenTofu Version unless its behaviour is about that answer.
const fakePrefix = "fake-tofu-"

func init() {
	behaviour, ok := strings.CutPrefix(filepath.Base(os.Args[0]), fakePrefix)
	if !ok {
		return
	}
	os.Exit(fakeTofuMain(behaviour, os.Args[1:]))
}

func fakeTofuMain(behaviour string, args []string) int {
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}
	if behaviour == "git" {
		// A git whose exec path is the fake's own directory.
		if cmd != "--exec-path" {
			return 2
		}
		fmt.Println(filepath.Dir(os.Args[0]))
		return 0
	}
	if cmd == "version" {
		switch behaviour {
		case "loud-version":
			fmt.Printf("%100000s", "x")
			return 0
		case "slow-version":
			time.Sleep(time.Hour)
		case "terraform":
			fmt.Println("Terraform v" + Version + " token-7f3a")
			return 0
		case "leave-version":
			if err := startSleeper(); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
		}
		fmt.Println("OpenTofu v" + Version)
		return 0
	}
	var err error
	switch behaviour {
	case "sleep":
		time.Sleep(time.Hour)
	case "proc":
		// How /proc is mounted, and what a write to a file of it that a
		// process may write to its own does.
		werr := os.WriteFile("/proc/self/comm", []byte("written\n"), 0)
		fmt.Println("write:", werr)
		var info []byte
		if info, err = os.ReadFile("/proc/self/mountinfo"); err == nil {
			for line := range strings.Lines(string(info)) {
				if f := strings.Fields(line); len(f) > 5 && f[4] == "/proc" {
					fmt.Println("mount:", f[5])
				}
			}
		}
	case "ok", "leave-version", "loud-version", "slow-version", "terraform":
	case "marks":
		// What `pwd -P` and $HOME are, which the test reads from the
		// plan's and the show's output.
		wd, werr := syscall.Getwd()
		fmt.Println(wd, os.Getenv("HOME"), werr)
		if cmd == "plan" {
			err = os.WriteFile(planFile, []byte("saved"), 0o600)
		}
	case "leave":
		// init leaves a process behind that holds none of tofu's pipes,
		// as a provider tofu fails to stop does.
		if cmd != "init" {
			return 1
		}
		err = startSleeper()
	case "loud":
		fmt.Printf("%4096s", "x")
	case "seccomp":
		fmt.Print(seccompProbe())
		return 1
	case "apply-event":
		// apply reports a change, and on SIGINT, as tofu does, it reports
		// the state it saved and exits.
		if cmd == "apply" {
			stop := make(chan os.Signal, 1)
			signal.Notify(stop, os.Interrupt)
			fmt.Println(applyEvent)
			select {
			case <-stop:
				fmt.Println(stoppedEvent)
				return 1
			case <-time.After(time.Hour):
			}
		}
	case "fifo-dir":
		if cmd == "init" {
			if err = os.Rename("../files", "../files.old"); err == nil {
				err = syscall.Mkfifo("../files", 0o600)
			}
		}
	case "fifo-plan":
		switch cmd {
		case "plan":
			if err = os.Remove(planFile); err == nil || errors.Is(err, os.ErrNotExist) {
				err = syscall.Mkfifo(planFile, 0o600)
			}
		case "show":
			fmt.Println("{}")
		}
	case "swap-plan":
		// show, as a provider it starts may, writes over the saved plan it
		// was given and over the one in the stack's directory. Each step
		// prints the saved plan it read.
		switch {
		case cmd == "plan":
			err = os.WriteFile(planFile, []byte("reviewed"), 0o600)
		case cmd == "show" && len(args) == 3, cmd == "apply":
			saved := args[len(args)-1]
			var data []byte
			if data, err = os.ReadFile(saved); err == nil {
				fmt.Printf("%s read %s\n", cmd, data)
				if cmd == "show" {
					_ = os.WriteFile(saved, []byte("swapped"), 0o600)
					_ = os.WriteFile(planFile, []byte("swapped"), 0o600)
				}
			}
		}
	case "fds":
		// What of the host the step's descriptors and network reach, for
		// TestRunNetworkHidesTheHost.
		fmt.Print(fdProbe())
		if cmd == "plan" {
			err = os.WriteFile(planFile, []byte("saved"), 0o600)
		}
	case "net":
		// What the step's network lets it reach, for TestRunNetwork. An
		// init with PROBE_INIT_FAIL set fails, so Fill returns what it saw.
		fmt.Print(netProbe())
		switch {
		case cmd == "plan":
			err = os.WriteFile(planFile, []byte("saved"), 0o600)
		case cmd == "init" && os.Getenv("PROBE_INIT_FAIL") != "":
			return 1
		}
	case "creds":
		// init fails if it sees the run's credential or lacks the
		// backend's address; the other steps print what they saw.
		cred, addr := os.Getenv("CRED"), os.Getenv("TF_HTTP_ADDRESS")
		if cmd == "init" {
			if cred != "" || addr == "" {
				fmt.Fprintf(os.Stderr, "init saw CRED=%q TF_HTTP_ADDRESS=%q\n", cred, addr)
				return 1
			}
			break
		}
		fmt.Println(cmd, cred, addr)
		if cmd == "plan" {
			err = os.WriteFile(planFile, []byte("saved"), 0o600)
		}
	case "fill":
		// The fake records the configuration it was asked to init, and
		// writes into the cache as a service with UMask=0077 would.
		if cmd != "init" {
			return 3
		}
		var main []byte
		if main, err = os.ReadFile("main.tf"); err == nil {
			syscall.Umask(0o077)
			seen := append(main, strings.Join(args, " ")+"\n"...)
			err = os.WriteFile(filepath.Join(os.Getenv("TF_PLUGIN_CACHE_DIR"), "seen"), seen, 0o666)
		}
	case "fill-record", "fill-bump":
		// The fake logs each init's flags, and an init that may write the
		// lock file records a hash of each provider that has none, as
		// tofu does. fill-bump writes another version too, which tofu
		// does not.
		if cmd != "init" {
			return 3
		}
		if err = appendFile(filepath.Join(os.Getenv("TF_PLUGIN_CACHE_DIR"), "seen"), strings.Join(args, " ")+"\n"); err != nil || slices.Contains(args, "-lockfile=readonly") {
			break
		}
		var lock []byte
		var ps []Provider
		if lock, err = os.ReadFile(LockFile); err == nil {
			ps, err = LockedProviders(lock)
		}
		for i := range ps {
			if len(ps[i].Hashes) == 0 {
				ps[i].Hashes = []string{"zh:" + strings.Repeat("a", 64)}
			}
			if behaviour == "fill-bump" {
				ps[i].Version = "9.9.9"
			}
		}
		if err == nil {
			if lock, err = formatLock(ps); err == nil {
				err = os.WriteFile(LockFile, lock, 0o600)
			}
		}
	default:
		fmt.Fprintln(os.Stderr, "no fake behaviour", behaviour)
		return 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func appendFile(name, text string) error {
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

const (
	applyEvent   = `{"type":"apply_start","hook":{"resource":{"addr":"random_pet.p"}}}`
	stoppedEvent = `{"type":"diagnostic","diagnostic":{"severity":"error","summary":"execution halted"}}`
)

// startSleeper starts a copy of this fake, named fake-tofu-sleep in the
// fake's directory, which sleeps.
func startSleeper() error {
	cmd := exec.Command("/proc/self/exe", "sleep")
	cmd.Args[0] = filepath.Join(filepath.Dir(os.Args[0]), fakePrefix+"sleep")
	return cmd.Start()
}

// fakeTofu returns the path of a fake with behaviour, in a directory of
// its own, so the processes it starts can be told apart by that path.
func fakeTofu(t *testing.T, behaviour string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), fakePrefix+behaviour)
	if err := os.Symlink(self, path); err != nil {
		t.Fatal(err)
	}
	return path
}

func fakeRunner(t *testing.T, bin string, timeout time.Duration, max int) *Runner {
	t.Helper()
	return newRunner(t, bin, t.TempDir(), t.TempDir(), timeout, max)
}

func newRunner(t *testing.T, bin, cache, scratch string, timeout time.Duration, max int) *Runner {
	t.Helper()
	return newRunnerOpts(t, Options{Binary: bin, PluginCache: cache, ScratchRoot: scratch, Timeout: timeout, ApplyTimeout: timeout, MaxOutput: max})
}

func newRunnerOpts(t *testing.T, o Options) *Runner {
	t.Helper()
	r, err := New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func fakeStack() Stack {
	return Stack{Files: fstest.MapFS{"main.tf": {Data: []byte("")}}, Dir: "."}
}

// requireNoneLeft fails t unless, within a few seconds, no live process
// was started from bin's directory, as every process a fake starts is. It
// kills any it finds either way.
func requireNoneLeft(t *testing.T, bin string) {
	t.Helper()
	dir := filepath.Dir(bin) + "/"
	deadline := time.Now().Add(5 * time.Second)
	for {
		var left []int
		ents, _ := os.ReadDir("/proc")
		for _, e := range ents {
			pid, err := strconv.Atoi(e.Name())
			if err != nil {
				continue
			}
			argv, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
			st, serr := runas.ProcStatus(pid)
			if err == nil && serr == nil && !strings.HasPrefix(st["State"], "Z") && bytes.HasPrefix(argv, []byte(dir)) {
				left = append(left, pid)
			}
		}
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			for _, pid := range left {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
			t.Fatalf("processes %v that the fake started outlived it", left)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestScratchRootMustBeSafe(t *testing.T) {
	bin := fakeTofu(t, "ok")
	for _, c := range []struct {
		name         string
		parent, root os.FileMode
		ok           bool
	}{
		{"writable by its owner only", 0o700, 0o700, true},
		{"writable by all with the sticky bit, as /tmp", 0o755, os.ModeSticky | 0o777, true},
		{"writable by all without the sticky bit", 0o755, 0o777, false},
		{"writable by its group", 0o755, 0o770, false},
		{"under a directory all may write without the sticky bit", 0o777, 0o700, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			parent := t.TempDir()
			root := filepath.Join(parent, "root")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(root, c.root); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(parent, c.parent); err != nil {
				t.Fatal(err)
			}
			r, err := New(context.Background(), Options{Binary: bin, PluginCache: t.TempDir(), ScratchRoot: root, Timeout: time.Minute, ApplyTimeout: time.Minute, MaxOutput: 1 << 20})
			if c.ok && err != nil {
				t.Fatalf("New refused a safe scratch root: %v", err)
			}
			if err == nil {
				r.Close()
			}
			if !c.ok && (err == nil || !strings.Contains(err.Error(), "scratch root")) {
				t.Fatalf("New with a scratch root other users can change: %v, want a refusal", err)
			}
		})
	}
}

func TestScratchRootMovedAfterNew(t *testing.T) {
	bin := fakeTofu(t, "marks")
	parent := t.TempDir()
	root, moved := filepath.Join(parent, "root"), filepath.Join(parent, "moved")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	r := newRunner(t, bin, t.TempDir(), root, time.Minute, 1<<20)
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	p, err := r.Plan(context.Background(), fakeStack())
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(moved)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{string(p.Log), string(p.JSON)} {
		f := strings.Fields(line)
		if len(f) != 3 || f[2] != "<nil>" {
			t.Fatalf("the fake reported %q, want its directory and HOME", line)
		}
		for _, p := range f[:2] {
			if !strings.HasPrefix(p, want+string(filepath.Separator)) {
				t.Errorf("tofu ran with directory %s, want one inside %s, the scratch root New opened before it moved", p, want)
			}
		}
	}
	if ents, err := os.ReadDir(root); err != nil || len(ents) > 0 {
		t.Errorf("the directory put in the scratch root's place holds %v, %v; want nothing", ents, err)
	}
}

func TestRunUserMayNotBeWrit(t *testing.T) {
	for _, c := range []struct {
		name string
		uid  uint32
	}{
		{"uid 0", 0},
		{"writ's uid", uint32(os.Getuid())},
	} {
		t.Run(c.name, func(t *testing.T) {
			scratch := t.TempDir()
			r := newRunner(t, fakeTofu(t, "ok"), t.TempDir(), scratch, time.Minute, 1<<20)
			s := fakeStack()
			s.User = &User{UID: c.uid, GID: uint32(os.Getgid())}
			_, err := r.Plan(context.Background(), s)
			if err == nil || !strings.Contains(err.Error(), "refuse to run as") {
				t.Errorf("plan as %s: %v, want a refusal", s.User, err)
			}
			if ents, err := os.ReadDir(scratch); err != nil || len(ents) > 0 {
				t.Errorf("the refused run made %v, %v in the scratch root", ents, err)
			}
		})
	}
}

func TestDescendantsDieWithTofu(t *testing.T) {
	bin := fakeTofu(t, "leave")
	r := fakeRunner(t, bin, time.Minute, 1<<20)
	if _, err := r.Plan(context.Background(), fakeStack()); err == nil {
		t.Fatal("plan: no error, want the plan step's exit code")
	}
	requireNoneLeft(t, bin)
}

func TestOutputOverTheLimitAlwaysFails(t *testing.T) {
	// Every step writes past the limit and exits 0 at once. Which the run
	// sees first varies from run to run.
	r := fakeRunner(t, fakeTofu(t, "loud"), time.Minute, 256)
	const runs = 200
	var missed []error
	for range runs {
		if _, err := r.Plan(context.Background(), fakeStack()); !errors.Is(err, ErrOutputLimit) {
			missed = append(missed, err)
		}
	}
	if len(missed) > 0 {
		t.Fatalf("%d of %d plans over the output limit returned another result; the first: %v", len(missed), runs, missed[0])
	}
}

func TestVersionProbeIsBounded(t *testing.T) {
	newWith := func(ctx context.Context, bin string) error {
		r, err := New(ctx, Options{Binary: bin, PluginCache: t.TempDir(), ScratchRoot: t.TempDir(), Timeout: time.Minute, ApplyTimeout: time.Minute, MaxOutput: 1 << 20})
		if err == nil {
			r.Close()
		}
		return err
	}

	t.Run("output over the limit", func(t *testing.T) {
		err := newWith(context.Background(), fakeTofu(t, "loud-version"))
		if !errors.Is(err, ErrOutputLimit) {
			t.Fatalf("New: %v, want ErrOutputLimit", err)
		}
	})

	t.Run("a timeout keeps the context's error", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		start := time.Now()
		err := newWith(ctx, fakeTofu(t, "slow-version"))
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("New: %v, want context.DeadlineExceeded", err)
		}
		if d := time.Since(start); d > 10*time.Second {
			t.Errorf("New returned %s after a 1s timeout", d)
		}
	})

	t.Run("a process it leaves is killed", func(t *testing.T) {
		bin := fakeTofu(t, "leave-version")
		if err := newWith(context.Background(), bin); err != nil {
			t.Fatal(err)
		}
		requireNoneLeft(t, bin)
	})

	t.Run("the error leaves out the binary's output", func(t *testing.T) {
		err := newWith(context.Background(), fakeTofu(t, "terraform"))
		if !errors.Is(err, ErrVersion) || strings.Contains(err.Error(), "token-7f3a") {
			t.Fatalf("New: %v, want ErrVersion without the binary's output", err)
		}
	})
}

// At its timeout an apply is interrupted, not killed, so tofu writes the
// state of the changes it made, and its events are kept.
func TestApplyTimeoutInterrupts(t *testing.T) {
	bin := fakeTofu(t, "apply-event")
	// The timeout must fall in apply, after init and show have started a
	// process each, which takes seconds under the race detector.
	r := newRunnerOpts(t, Options{Binary: bin, PluginCache: t.TempDir(), ScratchRoot: t.TempDir(), Timeout: time.Hour, ApplyTimeout: 10 * time.Second, MaxOutput: 1 << 20})
	res, err := r.Apply(context.Background(), fakeStack(), []byte("saved plan"))
	log := res.Log
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("apply: %v, want a timeout", err)
	}
	if !bytes.Contains(log, []byte(applyEvent)) || !bytes.Contains(log, []byte(stoppedEvent)) {
		t.Fatalf("the timed-out apply returned this event stream:\n%s\nwant the event tofu wrote before the timeout and the one it wrote on SIGINT", log)
	}
}

// The run's credentials reach plan, its show, and apply, and never init,
// which runs every module source and provider download of the commit,
// nor the show of the saved plan an apply runs, which needs none.
func TestInitGetsNoCredentials(t *testing.T) {
	r := fakeRunner(t, fakeTofu(t, "creds"), time.Minute, 1<<20)
	const addr = "http://127.0.0.1/v1/tfstate/app"
	s := fakeStack()
	s.Env = []string{"TF_HTTP_ADDRESS=" + addr}
	s.Creds = []string{"CRED=role-secret"}
	p, err := r.Plan(context.Background(), s)
	if se, ok := errors.AsType[*StepError](err); ok {
		t.Fatalf("%v: %s", err, se.Stderr)
	} else if err != nil {
		t.Fatal(err)
	}
	a, err := r.Apply(context.Background(), s, []byte("saved plan"))
	if se, ok := errors.AsType[*StepError](err); ok {
		t.Fatalf("%v: %s", err, se.Stderr)
	} else if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ got, want string }{
		{string(p.Log), "plan role-secret " + addr},
		{string(p.JSON), "show role-secret " + addr},
		{string(a.Plan), "show  " + addr},
		{string(a.Log), "apply role-secret " + addr},
	} {
		if c.got != c.want+"\n" {
			t.Errorf("a step saw %q, want %q", c.got, c.want+"\n")
		}
	}
}

// planWithin runs r.Plan and fails t if it does not return within d, which
// a blocked open of a FIFO would make it do.
func planWithin(t *testing.T, r *Runner, d time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := r.Plan(context.Background(), fakeStack())
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		t.Fatalf("Plan did not return within %s: an open of a FIFO the run left blocked it", d)
		return nil
	}
}

func TestFIFOInPlaceOfTheStackDirectory(t *testing.T) {
	bin := fakeTofu(t, "fifo-dir")
	r := fakeRunner(t, bin, 2*time.Second, 1<<20)
	if err := planWithin(t, r, 10*time.Second); err == nil {
		t.Fatal("Plan succeeded with a FIFO in place of the stack directory")
	}
}

func TestFIFOInPlaceOfTheSavedPlan(t *testing.T) {
	bin := fakeTofu(t, "fifo-plan")
	r := fakeRunner(t, bin, 2*time.Second, 1<<20)
	if err := planWithin(t, r, 10*time.Second); err == nil {
		t.Fatal("Plan succeeded with a FIFO for its saved plan")
	}
}

// show starts providers, which run as the run's uid, in the run's view,
// and may write what the run may. The saved plan that Plan returns, and
// the one that Apply's apply reads, must be the bytes show read, whatever
// such a process writes meanwhile.
func TestShowCannotSwapTheSavedPlan(t *testing.T) {
	r := fakeRunner(t, fakeTofu(t, "swap-plan"), 10*time.Second, 1<<20)
	p, err := r.Plan(context.Background(), fakeStack())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if string(p.JSON) != "show read reviewed\n" || string(p.File) != "reviewed" {
		t.Errorf("Plan returned the saved plan %q for a show that printed %q, want the plan show read", p.File, p.JSON)
	}
	a, err := r.Apply(context.Background(), fakeStack(), []byte("reviewed"))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if string(a.Plan) != "show read reviewed\n" || string(a.Log) != "apply read reviewed\n" {
		t.Errorf("Apply's show printed %q and its apply %q, want both to read the plan Apply was given", a.Plan, a.Log)
	}
}

// A git without the https transport, as a git built without curl is, is
// refused when the runner starts, not at a run's first git module.
func TestGitNeedsItsHTTPSTransport(t *testing.T) {
	git := fakeTofu(t, "git")
	if err := os.WriteFile(filepath.Join(filepath.Dir(git), "git"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := New(context.Background(), Options{Binary: fakeTofu(t, "ok"), Git: git, PluginCache: t.TempDir(), ScratchRoot: t.TempDir(), Timeout: time.Minute, ApplyTimeout: time.Minute, MaxOutput: 1 << 20})
	if err == nil || !strings.Contains(err.Error(), "has no git-remote-http,") {
		t.Fatalf("New with a git that has no git-remote-http: %v, want a refusal that names it", err)
	}
}

// tofu and its providers may read /proc, never write it: a write there
// changes the kernel's view of the run's processes.
func TestProcIsReadOnly(t *testing.T) {
	out, stderr, err := command{binary: fakeTofu(t, "proc"), args: []string{"plan"}, max: 1 << 20}.run(context.Background())
	if err != nil {
		t.Fatalf("run: %v\n%s", err, stderr)
	}
	if want := "write: open /proc/self/comm: read-only file system\n"; !strings.Contains(string(out), want) {
		t.Errorf("the run's write to /proc/self/comm reported\n%s\nwant %q", out, want)
	}
	mounts := regexp.MustCompile(`(?m)^mount: (\S+)$`).FindAllStringSubmatch(string(out), -1)
	if len(mounts) != 1 || !slices.Contains(strings.Split(mounts[0][1], ","), "ro") {
		t.Errorf("the run's /proc mounts are %q, want one, read-only", mounts)
	}
}
