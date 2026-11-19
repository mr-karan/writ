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

func mkdirAs(User, string) (string, error) { return "", ErrIsolationUnsupported }

func removeAs(User, string) {}

func dialAs(context.Context, User, string) (net.Conn, error) {
	return nil, ErrIsolationUnsupported
}

// PrepareIsolation fails: providers run as other users only on Linux.
func PrepareIsolation() error { return ErrIsolationUnsupported }
