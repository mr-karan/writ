//go:build !linux

package runas

import (
	"context"
	"os/exec"
	"syscall"
)

// SysProcAttr runs the process as u when u is set, with no supplementary
// groups. Only Linux has a parent-death signal.
func SysProcAttr(u *User) *syscall.SysProcAttr {
	a := &syscall.SysProcAttr{}
	if u != nil {
		a.Credential = &syscall.Credential{Uid: u.UID, Gid: u.GID, Groups: []uint32{}}
	}
	return a
}

// Command returns a command that fails to start: only Linux runs a process
// as another user.
func (u User) Command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Err = ErrUnsupported
	return cmd
}

func AsUser(User, func() error) error { return ErrUnsupported }

func CheckIsolated(int, User) error { return ErrUnsupported }

func CheckForeign(uint32) error { return ErrUnsupported }

// Reap fails: only Linux runs a process as another user.
func Reap(uint32) error { return ErrUnsupported }
