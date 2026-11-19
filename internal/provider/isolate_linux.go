package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"github.com/mr-karan/writ/internal/runas"
)

// Linux capability numbers and prctl options this file uses.
const (
	capKill   = 5
	capSetgid = 6
	capSetuid = 7

	linuxCapabilityVersion3 = 0x20080522
)

// capHeader and capData are the kernel's capget and capset arguments.
type capHeader struct {
	version uint32
	pid     int32
}

type capData struct {
	effective, permitted, inheritable uint32
}

// setProcAttr makes the kernel kill the provider when Writ dies, so a
// killed writ never leaks provider processes, and runs it as u when u is
// set, with no supplementary groups.
func setProcAttr(cmd *exec.Cmd, u *User) {
	cmd.SysProcAttr = runas.SysProcAttr(u)
}

// mkdirAs makes a fresh directory under root that belongs to u, mode 0700.
func mkdirAs(u User, root string) (string, error) {
	var dir string
	err := runas.AsUser(u, func() error {
		var err error
		dir, err = os.MkdirTemp(root, "writ-")
		return err
	})
	return dir, err
}

func removeAs(u User, dir string) {
	_ = runas.AsUser(u, func() error { return os.RemoveAll(dir) })
}

// dialAs connects to the unix socket at addr as u. The socket belongs to
// u, and its mode comes from the provider's umask, so only u may connect.
func dialAs(ctx context.Context, u User, addr string) (net.Conn, error) {
	var conn net.Conn
	err := runas.AsUser(u, func() error {
		var err error
		conn, err = (&net.Dialer{}).DialContext(ctx, "unix", addr)
		return err
	})
	return conn, err
}

// selfCaps returns this process's effective, inheritable, and ambient
// capability sets.
func selfCaps() (eff, inh, amb uint64, err error) {
	st, err := runas.ProcStatus(os.Getpid())
	if err != nil {
		return 0, 0, 0, err
	}
	eff, err1 := strconv.ParseUint(st["CapEff"], 16, 64)
	inh, err2 := strconv.ParseUint(st["CapInh"], 16, 64)
	amb, err3 := strconv.ParseUint(st["CapAmb"], 16, 64)
	return eff, inh, amb, errors.Join(err1, err2, err3)
}

// PrepareIsolation readies this process to start providers as other
// users. It needs CAP_SETUID and CAP_SETGID to switch users and CAP_KILL
// to stop a provider that will not exit. It clears the inheritable and
// ambient capabilities, which systemd's AmbientCapabilities sets: a
// process keeps ambient ones across exec, so a provider would otherwise
// start with CAP_SETUID. Every thread holds its own sets, so it changes
// them on all of them.
func PrepareIsolation() error {
	eff, inh, amb, err := selfCaps()
	if err != nil {
		return fmt.Errorf("read this process's capabilities: %w", err)
	}
	var missing []string
	for _, c := range []struct {
		bit  uint
		name string
	}{{capSetuid, "CAP_SETUID"}, {capSetgid, "CAP_SETGID"}, {capKill, "CAP_KILL"}} {
		if eff&(1<<c.bit) == 0 {
			missing = append(missing, c.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("running providers as other users needs %s, which this process lacks", strings.Join(missing, ", "))
	}
	if inh == 0 && amb == 0 {
		return nil
	}
	hdr := capHeader{version: linuxCapabilityVersion3}
	var data [2]capData
	if _, _, e := syscall.RawSyscall(syscall.SYS_CAPGET, uintptr(unsafe.Pointer(&hdr)), uintptr(unsafe.Pointer(&data[0])), 0); e != 0 {
		return fmt.Errorf("read the capabilities: %w", e)
	}
	data[0].inheritable, data[1].inheritable = 0, 0
	// Dropping a capability from the inheritable set drops it from the
	// ambient set too. Under cgo, as in a race build, this fails with
	// ENOTSUP; writ ships without cgo.
	if _, _, e := syscall.AllThreadsSyscall(syscall.SYS_CAPSET, uintptr(unsafe.Pointer(&hdr)), uintptr(unsafe.Pointer(&data[0])), 0); e != 0 {
		return fmt.Errorf("clear the inheritable capabilities: %w", e)
	}
	if _, inh, amb, err = selfCaps(); err != nil || inh != 0 || amb != 0 {
		return fmt.Errorf("the inheritable or ambient capabilities are still set: %x %x %v", inh, amb, err)
	}
	return nil
}
