//go:build linux && !amd64 && !arm64

package tofu

import (
	"errors"
	"runtime"
)

// seccompReady refuses: writ's filter knows the system call numbers of
// amd64 and arm64 only.
func seccompReady() error {
	return errors.New("writ's seccomp filter knows no system call numbers of " + runtime.GOARCH)
}

func installSeccomp() error { return seccompReady() }
