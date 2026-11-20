//go:build !linux

package tofu

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"

	"github.com/mr-karan/writ/internal/runas"
)

// startConfined starts cmd as it is: only Linux gives tofu a private view
// of the filesystem. writ serve --dev runs tofu so elsewhere, and a team's
// writ serve, which runs tofu only as a run's own user, cannot. Nor does
// a run get a network of its own.
func startConfined(cmd *exec.Cmd, c command) (func(), error) {
	if c.net != nil {
		return nil, errors.New("tofu: a run's own network namespace needs Linux")
	}
	if c.view.run != nil {
		cmd.Dir = filepath.Join(c.view.dir, c.view.work)
	}
	cmd.SysProcAttr = runas.SysProcAttr(c.user)
	cmd.SysProcAttr.Setpgid = true
	return func() {}, runas.Start(cmd)
}

// maxSocketPath is the size of sun_path on macOS and the BSDs, which holds
// a Unix socket's path and its closing NUL.
const maxSocketPath = 104

// pluginSocket is the longest name go-plugin gives a provider's socket in
// TMPDIR: os.CreateTemp's "plugin" and a decimal uint32.
const pluginSocket = "/plugin4294967295"

// runTmp is the run's TMPDIR, its home, where each provider makes its
// socket. Without a filesystem view to bind it at a short path, a home
// whose path leaves the socket no room fails here, not as a provider that
// never starts.
func runTmp(home string) (string, error) {
	if n := len(home) + len(pluginSocket); n >= maxSocketPath {
		return "", fmt.Errorf("the run's directory %s is too long: a provider's socket in it would take %d bytes of a Unix socket's limit of %d; set tofu.scratch_dir, or writ serve --dev --data, to a shorter path", home, n, maxSocketPath-1)
	}
	return home, nil
}
