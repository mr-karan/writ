package policy

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"strconv"
	"syscall"
)

// limitMemory caps the process's address space at what it maps now plus
// headroom bytes. An idle Go process maps about 2 GiB of address space it
// has not touched, so a fixed cap would end the runtime instead of the
// policy.
func limitMemory(headroom uint64) error {
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return err
	}
	sc := bufio.NewScanner(bytes.NewReader(status))
	for sc.Scan() {
		f := bytes.Fields(sc.Bytes())
		if len(f) == 3 && string(f[0]) == "VmSize:" && string(f[2]) == "kB" {
			kb, err := strconv.ParseUint(string(f[1]), 10, 64)
			if err != nil {
				return err
			}
			n := kb<<10 + headroom
			return syscall.Setrlimit(syscall.RLIMIT_AS, &syscall.Rlimit{Cur: n, Max: n})
		}
	}
	return fmt.Errorf("no VmSize in /proc/self/status")
}

// closeInherited closes every descriptor above stderr that the process
// inherited. A descriptor that survived exec has no FD_CLOEXEC, and every
// descriptor the Go runtime opens, such as its cgroup file, has it, so the
// flag tells them apart. The list comes from raw system calls, so reading
// it opens nothing in the runtime's poller.
func closeInherited() error {
	dir, err := syscall.Open("/proc/self/fd", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	var fds []int
	buf := make([]byte, 8192)
	for {
		n, err := syscall.ReadDirent(dir, buf)
		if err != nil {
			syscall.Close(dir)
			return err
		}
		if n <= 0 {
			break
		}
		var names []string
		_, _, names = syscall.ParseDirent(buf[:n], -1, names)
		for _, name := range names {
			fd, err := strconv.Atoi(name)
			if err == nil && fd > 2 && fd != dir {
				fds = append(fds, fd)
			}
		}
	}
	syscall.Close(dir)
	for _, fd := range fds {
		flags, _, errno := syscall.RawSyscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFD, 0)
		if errno == 0 && flags&syscall.FD_CLOEXEC == 0 {
			if err := syscall.Close(fd); err != nil {
				return fmt.Errorf("close the inherited descriptor %d: %w", fd, err)
			}
		}
	}
	return nil
}

// self is the writ binary that evaluates policies.
func self() (string, error) { return "/proc/self/exe", nil }
