//go:build linux && (amd64 || arm64)

package tofu

import (
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

// seccompDenied are the system calls that no process of a run may make
// once the helper has made its view: loading kernel code, mounting,
// reaching the kernel's keyrings, BPF, perf events, userfaultfd, and
// io_uring, each a large part of the kernel that a run never needs, and
// ptrace and the calls that, like it, reach into another process:
// process_vm_readv and process_vm_writev, which read and write its
// memory, and pidfd_getfd, which copies its descriptors. unshare and
// setns are denied too, and clone with a namespace flag, so no process of
// a run makes a user namespace, in which it would hold every capability
// again.
var seccompDenied = []uint32{
	unix.SYS_KEXEC_LOAD, unix.SYS_KEXEC_FILE_LOAD,
	unix.SYS_INIT_MODULE, unix.SYS_FINIT_MODULE, unix.SYS_DELETE_MODULE,
	unix.SYS_MOUNT, unix.SYS_UMOUNT2, unix.SYS_PIVOT_ROOT, unix.SYS_MOUNT_SETATTR,
	unix.SYS_OPEN_TREE, unix.SYS_MOVE_MOUNT, unix.SYS_FSOPEN, unix.SYS_FSCONFIG, unix.SYS_FSMOUNT, unix.SYS_FSPICK,
	unix.SYS_OPEN_BY_HANDLE_AT,
	unix.SYS_UNSHARE, unix.SYS_SETNS,
	unix.SYS_PTRACE, unix.SYS_PROCESS_VM_READV, unix.SYS_PROCESS_VM_WRITEV, unix.SYS_PIDFD_GETFD,
	unix.SYS_KEYCTL, unix.SYS_ADD_KEY, unix.SYS_REQUEST_KEY,
	unix.SYS_BPF, unix.SYS_PERF_EVENT_OPEN, unix.SYS_USERFAULTFD, unix.SYS_IO_URING_SETUP,
}

// cloneNamespaces are the flags of clone that make a namespace.
const cloneNamespaces = unix.CLONE_NEWNS | unix.CLONE_NEWCGROUP | unix.CLONE_NEWUTS | unix.CLONE_NEWIPC |
	unix.CLONE_NEWUSER | unix.CLONE_NEWPID | unix.CLONE_NEWNET

// auditArch is the architecture seccomp reports for a system call of this
// binary's own.
var auditArch = map[string]uint32{"amd64": unix.AUDIT_ARCH_X86_64, "arm64": unix.AUDIT_ARCH_AARCH64}[runtime.GOARCH]

// x32Bit marks a system call of the x32 ABI, which an amd64 kernel takes
// under AUDIT_ARCH_X86_64 too.
const x32Bit = 0x40000000

// seccompFilter is the classic BPF program of writ's filter. A system
// call of another architecture kills the process, such as one of i386
// through int 0x80, whose numbers are not the ones the filter knows; one
// of the x32 ABI and clone3, whose flags the filter cannot read, fail
// with ENOSYS, after which glibc falls back to clone; and the denied
// calls fail with EPERM.
func seccompFilter() []unix.SockFilter {
	const (
		ld   = unix.BPF_LD | unix.BPF_W | unix.BPF_ABS
		jeq  = unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K
		jge  = unix.BPF_JMP | unix.BPF_JGE | unix.BPF_K
		jset = unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K
		ret  = unix.BPF_RET | unix.BPF_K
		// Offsets in struct seccomp_data: nr, arch, and the low half of
		// args[0] on a little-endian machine.
		nr, arch, arg0 = 0, 4, 16
	)
	var (
		allow  = uint32(unix.SECCOMP_RET_ALLOW)
		kill   = uint32(unix.SECCOMP_RET_KILL_PROCESS)
		eperm  = uint32(unix.SECCOMP_RET_ERRNO) | uint32(unix.EPERM)
		enosys = uint32(unix.SECCOMP_RET_ERRNO) | uint32(unix.ENOSYS)
	)
	f := []unix.SockFilter{
		{Code: ld, K: arch},
		{Code: jeq, K: auditArch, Jt: 1},
		{Code: ret, K: kill},
		{Code: ld, K: nr},
	}
	if runtime.GOARCH == "amd64" {
		f = append(f, unix.SockFilter{Code: jge, K: x32Bit, Jf: 1}, unix.SockFilter{Code: ret, K: enosys})
	}
	f = append(f,
		unix.SockFilter{Code: jeq, K: unix.SYS_CLONE3, Jf: 1}, unix.SockFilter{Code: ret, K: enosys},
		unix.SockFilter{Code: jeq, K: unix.SYS_CLONE, Jf: 4},
		unix.SockFilter{Code: ld, K: arg0},
		unix.SockFilter{Code: jset, K: cloneNamespaces, Jf: 1},
		unix.SockFilter{Code: ret, K: eperm},
		unix.SockFilter{Code: ret, K: allow},
	)
	for _, call := range seccompDenied {
		f = append(f, unix.SockFilter{Code: jeq, K: call, Jf: 1}, unix.SockFilter{Code: ret, K: eperm})
	}
	return append(f, unix.SockFilter{Code: ret, K: allow})
}

// installSeccomp installs writ's filter on every thread of the helper,
// which has no_new_privs, so tofu and every process it starts run under
// it from exec on.
func installSeccomp() error {
	prog := seccompFilter()
	fprog := unix.SockFprog{Len: uint16(len(prog)), Filter: &prog[0]}
	r, _, e := unix.RawSyscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&fprog)))
	switch {
	case e != 0:
		return e
	case r != 0:
		return fmt.Errorf("thread %d could not take the filter", r)
	}
	return nil
}

// seccompReady reports why the kernel cannot run writ's filter, or nil:
// it needs seccomp's filter mode and SECCOMP_RET_KILL_PROCESS, of Linux
// 4.14, which a seccomp filter around writ itself may also refuse.
var seccompReady = sync.OnceValue(func() error {
	if _, err := unix.PrctlRetInt(unix.PR_GET_SECCOMP, 0, 0, 0, 0); err != nil {
		return fmt.Errorf("the kernel has no seccomp: %w", err)
	}
	action := uint32(unix.SECCOMP_RET_KILL_PROCESS)
	if _, _, e := unix.RawSyscall(unix.SYS_SECCOMP, unix.SECCOMP_GET_ACTION_AVAIL, 0, uintptr(unsafe.Pointer(&action))); e != 0 {
		return fmt.Errorf("the kernel's seccomp refuses SECCOMP_RET_KILL_PROCESS, which needs Linux 4.14: %w", e)
	}
	return nil
})
