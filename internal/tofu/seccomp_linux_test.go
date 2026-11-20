package tofu

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// seccompProbe makes each system call writ's filter denies, in a way that
// changes nothing when the call is allowed, and one exec, and returns a
// line for each: its name and the error it returned, or ok. With
// PROBE_CLONE set it also calls clone with CLONE_NEWUSER, which, allowed,
// would start a second copy of the probe.
func seccompProbe() string {
	var b strings.Builder
	line := func(name string, e syscall.Errno) {
		if e == 0 {
			fmt.Fprintf(&b, "%s: ok\n", name)
			return
		}
		fmt.Fprintf(&b, "%s: %s\n", name, unix.ErrnoName(e))
	}
	call := func(name string, nr uintptr, args ...uintptr) {
		a := append(args, 0, 0, 0, 0, 0, 0)
		r, _, e := unix.Syscall6(nr, a[0], a[1], a[2], a[3], a[4], a[5])
		if e == 0 && name == "userfaultfd" {
			unix.Close(int(r))
		}
		line(name, e)
	}
	session := unix.KEY_SPEC_SESSION_KEYRING
	none := -1
	call("unshare", unix.SYS_UNSHARE, unix.CLONE_NEWUSER)
	call("clone3", unix.SYS_CLONE3)
	call("setns", unix.SYS_SETNS, uintptr(none))
	call("ptrace", unix.SYS_PTRACE, unix.PTRACE_PEEKUSR, 999999)
	call("keyctl", unix.SYS_KEYCTL, unix.KEYCTL_GET_KEYRING_ID, uintptr(session))
	call("perf_event_open", unix.SYS_PERF_EVENT_OPEN, 0, 0, uintptr(none), uintptr(none))
	call("userfaultfd", unix.SYS_USERFAULTFD, 1) // UFFD_USER_MODE_ONLY
	call("io_uring_setup", unix.SYS_IO_URING_SETUP, 0, 0)
	call("bpf", unix.SYS_BPF, unix.BPF_PROG_LOAD, 0, 0)
	call("mount", unix.SYS_MOUNT, 0, 0, 0)
	// No iovec, so an allowed call reads and writes nothing.
	call("process_vm_readv", unix.SYS_PROCESS_VM_READV, uintptr(os.Getpid()), 0, 0, 0, 0, 0)
	call("process_vm_writev", unix.SYS_PROCESS_VM_WRITEV, uintptr(os.Getpid()), 0, 0, 0, 0, 0)
	call("pidfd_getfd", unix.SYS_PIDFD_GETFD, uintptr(none), 0, 0)
	if os.Getenv("PROBE_CLONE") != "" {
		call("clone", unix.SYS_CLONE, unix.CLONE_NEWUSER|uintptr(unix.SIGCHLD))
	}
	cmd := exec.Command("/proc/self/exe", "version")
	cmd.Args[0] = filepath.Join(filepath.Dir(os.Args[0]), fakePrefix+"ok")
	if out, err := cmd.Output(); err != nil || !strings.Contains(string(out), "OpenTofu") {
		fmt.Fprintf(&b, "exec: %v %q\n", err, out)
	} else {
		b.WriteString("exec: ok\n")
	}
	return b.String()
}

// TestSeccompFilter runs the probe as tofu, where writ's filter denies
// each call, and outside a run, where the same calls return what the
// kernel makes of them, which shows that the filter, not the view's lack
// of capabilities, refused them. A process of the run still starts
// another.
func TestSeccompFilter(t *testing.T) {
	if err := seccompReady(); err != nil {
		t.Skipf("the kernel cannot run writ's filter: %v", err)
	}
	bin := fakeTofu(t, "seccomp")
	r := fakeRunner(t, bin, time.Minute, 1<<20)
	s := fakeStack()
	s.Env = []string{"PROBE_CLONE=1"}
	_, err := r.Plan(context.Background(), s)
	se, ok := errors.AsType[*StepError](err)
	if !ok {
		t.Fatalf("the probe returned %v, not its output", err)
	}
	got := probeLines(string(se.Stdout))
	t.Logf("under the filter:\n%s", se.Stdout)
	want := map[string]string{
		"unshare": "EPERM", "clone": "EPERM", "clone3": "ENOSYS", "setns": "EPERM", "ptrace": "EPERM",
		"keyctl": "EPERM", "perf_event_open": "EPERM", "userfaultfd": "EPERM", "io_uring_setup": "EPERM",
		"bpf": "EPERM", "mount": "EPERM", "process_vm_readv": "EPERM", "process_vm_writev": "EPERM",
		"pidfd_getfd": "EPERM", "exec": "ok",
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s under the filter: %q, want %s", name, got[name], w)
		}
	}
	out, _ := exec.Command(bin, "probe").Output()
	control := probeLines(string(out))
	t.Logf("outside a run:\n%s", out)
	for _, name := range []string{"unshare", "clone3", "setns", "ptrace", "process_vm_readv", "process_vm_writev", "pidfd_getfd"} {
		if control[name] == want[name] {
			t.Errorf("%s outside a run: %q, as under the filter, so the test proves nothing of it", name, control[name])
		}
	}
}

func probeLines(out string) map[string]string {
	m := map[string]string{}
	for line := range strings.Lines(out) {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), ": "); ok {
			m[k] = v
		}
	}
	return m
}
