package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"

	"github.com/mr-karan/writ/internal/server"
	"github.com/mr-karan/writ/internal/tofu"
)

// maxTofuOutput bounds each step's stdout and stderr, and the saved plan,
// of an OpenTofu run.
const maxTofuOutput = 64 << 20

// tofuProgram finds a program the tofu block names, its binary or git: a
// path as it is, and a name on PATH.
func tofuProgram(name string) (string, error) {
	if strings.Contains(name, "/") {
		return name, nil
	}
	return exec.LookPath(name)
}

// newTofuRunner starts the OpenTofu runner of the config's tofu block. It
// makes the plugin cache, which only writ may write, and the scratch
// directory, which a run as a provider uid makes its directory in, so it
// is shared with the sticky bit, as /tmp is.
func newTofuRunner(ctx context.Context, cfg *server.Config) (*tofu.Runner, error) {
	t := cfg.Tofu
	bin, err := tofuProgram(t.Binary)
	if err != nil {
		return nil, fmt.Errorf("tofu.binary: %w", err)
	}
	var git string
	if t.Git != "" {
		if git, err = tofuProgram(t.Git); err != nil {
			return nil, fmt.Errorf("tofu.git: %w", err)
		}
	}
	scratch := os.FileMode(0o700)
	if cfg.ProviderUIDs != nil {
		scratch = os.ModeSticky | 0o777
	}
	for _, d := range []struct {
		path string
		mode os.FileMode
	}{{t.PluginCache, 0o755}, {t.ScratchDir, scratch}} {
		if err := os.MkdirAll(d.path, 0o700); err != nil {
			return nil, err
		}
		if err := os.Chmod(d.path, d.mode); err != nil {
			return nil, err
		}
	}
	return tofu.New(ctx, tofu.Options{Binary: bin, Git: git, PluginCache: t.PluginCache, ScratchRoot: t.ScratchDir, Timeout: cfg.Limits.PlanTimeout, ApplyTimeout: cfg.Limits.ApplyTimeout, MaxOutput: maxTofuOutput, Limits: runLimits(cfg)})
}

// runLimits are the config's limits of each OpenTofu step. The dev
// control plane's config sets none.
func runLimits(cfg *server.Config) tofu.Limits {
	l := cfg.Limits
	return tofu.Limits{Memory: l.RunMemory, Pids: l.RunPids, CPUs: l.RunCPUs, Scratch: l.RunScratch}
}

// serveTofu is the OpenTofu runner writ serve passes to the server, and
// what closes it. A config without a tofu block has none. The dev control
// plane runs without one when PATH has no OpenTofu, and logs why.
func serveTofu(ctx context.Context, cfg *server.Config, log *slog.Logger) (server.TofuRunner, func(), error) {
	if cfg.Tofu == nil {
		return nil, func() {}, nil
	}
	r, err := newTofuRunner(ctx, cfg)
	if err != nil && cfg.Dev {
		log.Warn("stacks of .tf files are refused: writ serve --dev found no OpenTofu "+tofu.Version, "err", err)
		return nil, func() {}, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("tofu: %w", err)
	}
	e := r.Enforcement()
	if e.CgroupOff != "" {
		log.Warn("run limits: OpenTofu steps run without limits.run_memory, run_pids, and run_cpus", "why", e.CgroupOff)
	}
	if !e.Seccomp {
		log.Warn("run limits: OpenTofu processes run without writ's seccomp filter", "why", e.SeccompOff)
	}
	return r, func() { r.Close() }, nil
}
