package runas

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// SysProcAttr makes the kernel kill the process when writ dies, and runs
// it as u when u is set, with no supplementary groups. Go sets the death
// signal after it switches the user, which would otherwise clear it.
func SysProcAttr(u *User) *syscall.SysProcAttr {
	a := &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if u != nil {
		a.Credential = &syscall.Credential{Uid: u.UID, Gid: u.GID, Groups: []uint32{}}
	}
	return a
}

// Command returns a command that runs as u with no supplementary groups,
// killed when writ dies.
func (u User) Command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = SysProcAttr(&u)
	return cmd
}

// AsUser runs fn on an OS thread whose filesystem uid and gid are u's, so
// the files fn creates belong to u and fn passes u's permission checks.
// Linux keeps these ids for each thread. The thread returns to the
// runtime only once its own ids are back. Otherwise it stays locked, and
// the runtime ends it with the goroutine, so it never runs another
// goroutine with u's ids.
func AsUser(u User, fn func() error) error {
	errc := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		own := User{UID: uint32(syscall.Geteuid()), GID: uint32(syscall.Getegid())}
		setFS(u)
		if fsIDs() != u {
			if setFS(own); fsIDs() == own {
				runtime.UnlockOSThread()
			}
			errc <- fmt.Errorf("act as %s: the kernel refused; writ needs CAP_SETUID and CAP_SETGID", u)
			return
		}
		err := fn()
		if setFS(own); fsIDs() == own {
			runtime.UnlockOSThread()
		}
		errc <- err
	}()
	return <-errc
}

// setFS sets this thread's filesystem uid and gid. setfsuid reports no
// error, so fsIDs reads them back. A change of either makes the kernel
// mark the whole process not dumpable, so it waits for StartMapped.
func setFS(u User) {
	dumpMu.Lock()
	defer dumpMu.Unlock()
	syscall.RawSyscall(syscall.SYS_SETFSGID, uintptr(u.GID), 0, 0)
	syscall.RawSyscall(syscall.SYS_SETFSUID, uintptr(u.UID), 0, 0)
}

// dumpMu keeps setFS and Start from marking the process not dumpable
// while StartMapped starts a process.
var dumpMu sync.Mutex

// StartMapped starts cmd, which makes a user namespace whose uid_map and
// gid_map this process writes. The kernel lets a process other than root
// open those files only while it is dumpable, and AsUser leaves it not
// dumpable. So the process is dumpable while cmd starts and is put back
// after, with no AsUser or Start in between. Not dumpable, a process lets
// nothing of its uid without CAP_SYS_PTRACE trace it or read its memory.
func StartMapped(cmd *exec.Cmd) (err error) {
	dumpMu.Lock()
	defer dumpMu.Unlock()
	was, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_GET_DUMPABLE, 0, 0)
	if e != 0 {
		return fmt.Errorf("read whether writ is dumpable: %w", e)
	}
	if was == 1 {
		return cmd.Start()
	}
	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_SET_DUMPABLE, 1, 0); e != 0 {
		return fmt.Errorf("make writ dumpable to write the new user namespace's id maps: %w", e)
	}
	// Deferred, so a panic in Start leaves writ not dumpable too. The
	// kernel's 2, which fs.suid_dumpable may set, is not one prctl takes;
	// 0 keeps everything 2 keeps from other users.
	defer func() {
		if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_SET_DUMPABLE, 0, 0); e != 0 {
			if err == nil && cmd.Process != nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
			err = errors.Join(err, fmt.Errorf("make writ not dumpable again: %w", e))
		}
	}()
	return cmd.Start()
}

// Start starts cmd, whose SysProcAttr may switch the user it runs as.
// Go starts a child outside a new user namespace with CLONE_VM, so the
// child's change of ids marks writ itself not dumpable, as setFS does,
// and it waits for StartMapped the same way.
func Start(cmd *exec.Cmd) error {
	dumpMu.Lock()
	defer dumpMu.Unlock()
	return cmd.Start()
}

// NotDumpable marks this process not dumpable: it writes no core dump,
// and no process of its uid without CAP_SYS_PTRACE may trace it or read
// its memory. StartMapped makes it dumpable only while it starts a
// process.
func NotDumpable() error {
	dumpMu.Lock()
	defer dumpMu.Unlock()
	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_SET_DUMPABLE, 0, 0); e != 0 {
		return fmt.Errorf("make writ not dumpable: %w", e)
	}
	return nil
}

// fsIDs returns this thread's filesystem uid and gid. An invalid id
// changes nothing and returns the current one.
func fsIDs() User {
	uid, _, _ := syscall.RawSyscall(syscall.SYS_SETFSUID, uintptr(^uint32(0)), 0, 0)
	gid, _, _ := syscall.RawSyscall(syscall.SYS_SETFSGID, uintptr(^uint32(0)), 0, 0)
	return User{UID: uint32(uid), GID: uint32(gid)}
}

// CheckIsolated checks that a started process runs as u in every uid and
// gid field, with no supplementary group and no capability. It guards
// against a capability writ holds reaching the process through exec.
func CheckIsolated(pid int, u User) error {
	st, err := ProcStatus(pid)
	if err != nil {
		return fmt.Errorf("read the status of process %d: %w", pid, err)
	}
	for _, f := range strings.Fields(st["Uid"]) {
		if f != strconv.FormatUint(uint64(u.UID), 10) {
			return fmt.Errorf("runs with uids %q, not %d", st["Uid"], u.UID)
		}
	}
	for _, f := range strings.Fields(st["Gid"]) {
		if f != strconv.FormatUint(uint64(u.GID), 10) {
			return fmt.Errorf("runs with gids %q, not %d", st["Gid"], u.GID)
		}
	}
	if g := st["Groups"]; g != "" {
		return fmt.Errorf("has supplementary groups %s", g)
	}
	for _, k := range []string{"CapInh", "CapPrm", "CapEff", "CapAmb"} {
		v, err := strconv.ParseUint(st[k], 16, 64)
		if err != nil || v != 0 {
			return fmt.Errorf("started with capabilities: %s %s", k, st[k])
		}
	}
	return nil
}

// ProcStatus reads the fields of /proc/PID/status.
func ProcStatus(pid int) (map[string]string, error) {
	f, err := os.Open(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if ok {
			out[k] = strings.TrimSpace(v)
		}
	}
	return out, sc.Err()
}

// CheckForeign refuses uid 0 and the real, effective, and saved uids of
// this process, as the uid of a run or of what Reap kills: a run as one
// of them would hold writ's files, and Reap would kill writ itself.
func CheckForeign(uid uint32) error {
	if uid == 0 {
		return errors.New("uid 0 is root")
	}
	st, err := ProcStatus(os.Getpid())
	if err != nil {
		return fmt.Errorf("read writ's own uids: %w", err)
	}
	if slices.Contains(ownUIDs(st), strconv.FormatUint(uint64(uid), 10)) {
		return fmt.Errorf("uid %d is writ's own", uid)
	}
	return nil
}

// ownUIDs returns the real, effective, and saved uids of a status. The
// fourth, the filesystem uid, is left out: writ holds a run's uid there
// for a moment while it acts as the run's user.
func ownUIDs(st map[string]string) []string {
	f := strings.Fields(st["Uid"])
	return f[:min(len(f), 3)]
}

// Reap kills every process whose real, effective, or saved uid is uid, so
// nothing a run left behind outlives it and meets the next run that gets
// the same uid, even a process that left its group with setsid. It needs
// CAP_KILL, and refuses what CheckForeign refuses. Zombies hold nothing
// and are skipped.
func Reap(uid uint32) error {
	if err := CheckForeign(uid); err != nil {
		return fmt.Errorf("refuse to end the processes of uid %d: %w", uid, err)
	}
	want := strconv.FormatUint(uint64(uid), 10)
	deadline := time.Now().Add(5 * time.Second)
	for {
		ents, err := os.ReadDir("/proc")
		if err != nil {
			return err
		}
		var left []int
		for _, e := range ents {
			pid, err := strconv.Atoi(e.Name())
			if err != nil {
				continue
			}
			st, err := ProcStatus(pid)
			if err != nil || strings.HasPrefix(st["State"], "Z") {
				continue
			}
			if slices.Contains(ownUIDs(st), want) {
				left = append(left, pid)
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		if len(left) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("processes %v of uid %d did not exit", left, uid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
