package runas

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestReap and TestStartMapped run this package's test binary again
// inside user and pid namespaces, as uid 1000 with the capabilities writ
// serve has. There Reap can kill only what the test started.
const (
	childEnv = "WRIT_TEST_RUNAS_CHILD"
	fsuidEnv = "WRIT_TEST_FSUID"
	sleepEnv = "WRIT_TEST_SLEEP"
)

const (
	writUID = 1000
	runUID  = 61000
)

var namespaceArgs = []string{"--user", "--map-root-user", "--map-users=auto", "--map-groups=auto", "--pid", "--fork", "--mount-proc"}

const dropToWrit = `exec setpriv --reuid=1000 --regid=1000 --clear-groups --inh-caps=+setuid,+setgid,+kill --ambient-caps=+setuid,+setgid,+kill "$@"`

func init() {
	if os.Getenv(sleepEnv) != "" {
		os.Stdout.WriteString("ready\n")
		time.Sleep(time.Hour)
		os.Exit(0)
	}
	v := os.Getenv(fsuidEnv)
	if v == "" {
		return
	}
	// init runs on the main thread, whose ids /proc/PID/status shows, as
	// writ's thread does while it acts as a run's user.
	uid, err := strconv.ParseUint(v, 10, 32)
	if err != nil {
		os.Exit(2)
	}
	u := User{UID: uint32(uid), GID: uint32(uid)}
	if setFS(u); fsIDs() != u {
		os.Exit(3)
	}
	os.Stdout.WriteString("ready\n")
	time.Sleep(time.Hour)
	os.Exit(0)
}

func TestReap(t *testing.T) {
	if os.Getenv(childEnv) != "" {
		reapChild(t)
		return
	}
	inNamespaces(t, "TestReap")
}

// TestStartMapped starts a process in a user namespace that maps a run's
// uid, after AsUser, as uid 1000 with the capabilities writ serve has.
func TestStartMapped(t *testing.T) {
	if os.Getenv(childEnv) != "" {
		startMappedChild(t)
		return
	}
	inNamespaces(t, "TestStartMapped")
}

// inNamespaces runs the test name of this package's test binary again
// inside user and pid namespaces, as uid 1000 with the capabilities writ
// serve has.
func inNamespaces(t *testing.T, name string) {
	t.Helper()
	for _, bin := range []string{"unshare", "setpriv"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("needs %s", bin)
		}
	}
	if out, err := exec.Command("unshare", append(slices.Clone(namespaceArgs), "true")...).CombinedOutput(); err != nil {
		t.Skipf("needs a user namespace with subordinate uids: %v %s", err, out)
	}
	// Uid 1000 in the namespace is a subordinate uid on the host, so the
	// binary goes where any user may run it.
	dir, err := os.MkdirTemp("/tmp", "writ-runas-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o755); err != nil {
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
	bin := filepath.Join(dir, "runas.test")
	if err := os.WriteFile(bin, b, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("unshare", append(slices.Clone(namespaceArgs), "sh", "-c", dropToWrit, "sh", bin, "-test.run=^"+name+"$", "-test.v", "-test.count=1")...)
	cmd.Env = append(os.Environ(), childEnv+"=1")
	out, err := cmd.CombinedOutput()
	t.Logf("in the namespaces:\n%s", out)
	if err != nil || !strings.Contains(string(out), "--- PASS: "+name) {
		t.Fatalf("the test failed in the namespaces: %v", err)
	}
}

func startMappedChild(t *testing.T) {
	if os.Getuid() != writUID {
		t.Fatalf("the child runs as uid %d, not %d", os.Getuid(), writUID)
	}
	if err := AsUser(User{UID: runUID, GID: runUID}, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if d := dumpable(t); d != 0 {
		t.Fatalf("after AsUser writ is dumpable %d, want 0, which StartMapped must undo to start the process", d)
	}
	cmd := exec.Command("/proc/self/exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:                 syscall.CLONE_NEWUSER,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: writUID, Size: 1}, {ContainerID: runUID, HostID: runUID, Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: writUID, Size: 1}, {ContainerID: runUID, HostID: runUID, Size: 1}},
		GidMappingsEnableSetgroups: true,
	}
	cmd.Env = []string{sleepEnv + "=1"}
	if err := StartMapped(cmd); err != nil {
		t.Fatalf("start a process in a user namespace that maps uid %d: %v", runUID, err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	// Dumpable, writ would let any process of its uid read its memory.
	if d := dumpable(t); d != 0 {
		t.Errorf("after StartMapped writ is dumpable %d, want 0 as before it", d)
	}
}

func dumpable(t *testing.T) uintptr {
	t.Helper()
	d, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_GET_DUMPABLE, 0, 0)
	if e != 0 {
		t.Fatal(e)
	}
	return d
}

func reapChild(t *testing.T) {
	if os.Getuid() != writUID {
		t.Fatalf("the child runs as uid %d, not %d", os.Getuid(), writUID)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name string
		uid  uint32
	}{
		{"Reap refuses uid 0", 0},
		{"Reap refuses writ's own uid", writUID},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := start(t, self, &User{UID: c.uid, GID: c.uid}, sleepEnv+"=1")
			if err := Reap(c.uid); err == nil || !strings.Contains(err.Error(), "refuse") {
				t.Errorf("Reap(%d): %v, want a refusal", c.uid, err)
			}
			requireAlive(t, p)
		})
	}

	t.Run("Reap leaves a process that only acts as the uid on files", func(t *testing.T) {
		p := start(t, self, nil, fsuidEnv+"="+strconv.Itoa(runUID))
		st, err := ProcStatus(p.Process.Pid)
		if err != nil {
			t.Fatal(err)
		}
		if want := "1000\t1000\t1000\t61000"; st["Uid"] != want {
			t.Fatalf("the stand-in for writ acting as the run's user has uids %q, want %q", st["Uid"], want)
		}
		if err := Reap(runUID); err != nil {
			t.Fatal(err)
		}
		requireAlive(t, p)
	})

	t.Run("Reap kills a process of the uid", func(t *testing.T) {
		p := start(t, self, &User{UID: runUID, GID: runUID}, sleepEnv+"=1")
		if err := Reap(runUID); err != nil {
			t.Fatal(err)
		}
		if err := p.Wait(); err == nil || !strings.Contains(err.Error(), "killed") {
			t.Fatalf("the process of uid %d: %v, want killed", runUID, err)
		}
	})
}

// start runs this test binary as u, or as the test when u is nil, and
// waits until it is ready. It is killed when the test ends.
func start(t *testing.T, self string, u *User, env string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(self)
	cmd.Env = []string{env}
	cmd.SysProcAttr = SysProcAttr(u)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	if line, err := bufio.NewReader(out).ReadString('\n'); line != "ready\n" {
		t.Fatalf("the helper never became ready: %q %v", line, err)
	}
	return cmd
}

func requireAlive(t *testing.T, p *exec.Cmd) {
	t.Helper()
	st, err := ProcStatus(p.Process.Pid)
	if err != nil || strings.HasPrefix(st["State"], "Z") {
		t.Fatalf("process %d was killed: %v %s", p.Process.Pid, err, st["State"])
	}
}
