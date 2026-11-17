// Package cli implements the writ commands. cmd/writ parses flags into the
// option structs below and calls one App method; everything a command does,
// including rendering human and JSON output, happens here.
package cli

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/mr-karan/writ/internal/addr"
	"github.com/mr-karan/writ/internal/confine"
	"github.com/mr-karan/writ/internal/diag"
	"github.com/mr-karan/writ/internal/provider"
)

// ErrFailed means the command ran and reported its failure on stdout
// (diagnostics, a contract violation, a failing goal, a failed apply). Exit 1.
var ErrFailed = errors.New("command failed")

// Options configures an App. main fills it once.
type Options struct {
	Dir string // stack directory, from --dir; default "."
	// Files reads the stack's files and writes what writ import and writ
	// agent init write. writ mcp confines them to the directory it serves.
	// The zero value reaches any path.
	Files confine.Files
	// CacheDir holds provider binaries and schemas. Empty means
	// WRIT_CACHE_DIR, else os.UserCacheDir()/writ.
	CacheDir string
	// Stdin is what writ mcp reads requests from.
	Stdin  io.Reader
	Stdout io.Writer
	// Stderr takes what a command says about its output when stdout is the
	// output itself, as for the code writ import --type prints.
	Stderr    io.Writer
	Log       *slog.Logger // text handler on stderr
	Now       func() time.Time
	LookupEnv func(string) (string, bool)
	// Drain closes on the first interrupt, when writ apply --remote stops
	// following its run.
	Drain <-chan struct{}
	// Env is the environment to run in, from --env; empty means WRIT_ENV,
	// then none.
	Env string
	// Version is writ's own version.
	Version string
	// ProviderLaunch is how the importer's and the schema tool's providers
	// start; the zero value is a laptop's.
	ProviderLaunch provider.Launch
	// Server is the control plane the remote commands use, from --server:
	// a URL, or "dev". Empty means WRIT_SERVER, then the dev control plane.
	Server string
	// Color colors human output; main sets it from ColorFor(os.Stdout).
	Color bool
}

// App runs commands for one stack directory.
type App struct {
	opts Options
}

// New returns an App, filling unset options with the process defaults.
func New(opts Options) *App {
	if opts.Dir == "" {
		opts.Dir = "."
	}
	if opts.Stdin == nil {
		opts.Stdin = os.Stdin
	}
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	if opts.Stderr == nil {
		opts.Stderr = os.Stderr
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.LookupEnv == nil {
		opts.LookupEnv = os.LookupEnv
	}
	if env, ok := opts.LookupEnv("WRIT_ENV"); ok && opts.Env == "" {
		opts.Env = env
	}
	return &App{opts: opts}
}

func (a *App) dirs() (provider.Dirs, error) {
	if a.opts.CacheDir == "" {
		return provider.DefaultDirs()
	}
	abs, err := filepath.Abs(a.opts.CacheDir)
	return provider.Dirs{Cache: abs}, err
}

func (a *App) path(rel string) string { return filepath.Join(a.opts.Dir, rel) }

// DefaultRunsLimit is how many runs `writ runs` lists without --limit.
const DefaultRunsLimit = 50

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func parseAddr(s string) (addr.Addr, error) {
	a, err := addr.Parse(s)
	if err != nil {
		return addr.Addr{}, usagef("address %q: %v", s, err)
	}
	return a, nil
}

// failed prints report as JSON when it is not nil, else diags, and
// returns ErrFailed.
func (a *App) failed(report any, diags diag.List) error {
	if report != nil {
		if err := writeJSON(a.opts.Stdout, report); err != nil {
			return err
		}
		return ErrFailed
	}
	printDiags(a.humanOutput(), diags)
	return ErrFailed
}

func jsonOnly(asJSON bool, report any) any {
	if asJSON {
		return report
	}
	return nil
}
