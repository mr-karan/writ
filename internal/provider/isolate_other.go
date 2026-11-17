//go:build !linux

package provider

import (
	"context"
	"net"
	"os/exec"
)

// setProcAttr does nothing where the kernel has no parent-death signal.
// start refuses a User before it gets here.
func setProcAttr(*exec.Cmd, *User) {}

// Command returns a command that fails to start: providers run as other
// users only on Linux.
func (u User) Command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Err = ErrIsolationUnsupported
	return cmd
}

func mkdirAs(User, string) (string, error) { return "", ErrIsolationUnsupported }

func removeAs(User, string) {}

func dialAs(context.Context, User, string) (net.Conn, error) {
	return nil, ErrIsolationUnsupported
}

// PrepareIsolation fails: providers run as other users only on Linux.
func PrepareIsolation() error { return ErrIsolationUnsupported }
