//go:build e2e

// Package e2e drives the real writ binary and OpenTofu against the real
// hashicorp/random, hashicorp/tls, and hashicorp/local providers, which it
// downloads once into a plugin cache, so later runs work offline. Run it
// with `just e2e`; the justfile names the other suites and their tags.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

var (
	// writBin is the writ binary, built once.
	writBin string
	// cacheDir is the provider cache every case shares.
	cacheDir string
	runTmp   string // TMPDIR of every writ this suite runs
)

// commandTimeout bounds one writ invocation.
const commandTimeout = 5 * time.Minute

// TestMain builds writ once into a temp dir.
func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	tmp, err := os.MkdirTemp("", "writ-e2e-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(tmp)
	// Writ puts each provider's socket directory under TMPDIR, so pointing
	// it at this run's directory tells this run's providers apart from those
	// of another suite running at the same time.
	if err := os.Setenv("TMPDIR", tmp); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	runTmp = tmp
	// Database latency is fixture setup, not a timeout under test. The normal
	// binary ignores this variable.
	if err := os.Setenv("WRIT_TEST_DATABASE_TIMEOUT", "1m"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	// A test that runs this binary again in a namespace passes the writ it
	// built, so the child builds nothing.
	writBin = os.Getenv(childWritEnv)
	if writBin == "" {
		writBin = filepath.Join(tmp, "writ")
		build := exec.Command("go", "build", "-o", writBin, "github.com/mr-karan/writ/cmd/writ")
		build.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := build.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "build writ: %v\n%s", err, out)
			return 1
		}
	}
	cacheDir = os.Getenv("WRIT_CACHE_DIR")
	if cacheDir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		cacheDir = filepath.Join(base, "writ")
	}
	return m.Run()
}

// result is one finished writ invocation.
type result struct {
	args   []string
	stdout string
	stderr string
	code   int
	killed bool // ended by SIGKILL
}

func (r result) String() string {
	return fmt.Sprintf("writ %s: exit %d\nstdout:\n%s\nstderr:\n%s", strings.Join(r.args, " "), r.code, r.stdout, r.stderr)
}

// stackDir is one copy of testdata/certs.
type stackDir struct {
	t   *testing.T
	dir string
	env []string
}

func (s *stackDir) exec(extraEnv []string, args ...string) result {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, writBin, args...)
	cmd.Dir = s.dir
	cmd.Env = append(append(os.Environ(), s.env...), extraEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	s.captureOutput(stdout.Bytes(), stderr.Bytes())
	r := result{args: args, stdout: stdout.String(), stderr: stderr.String()}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		r.code = exitErr.ExitCode()
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() && ws.Signal() == syscall.SIGKILL {
			r.killed = true
		}
	default:
		s.t.Fatalf("run writ %v: %v", args, err)
	}
	return r
}

func (s *stackDir) captureOutput(stdout, stderr []byte) {
	s.t.Helper()

	if path := os.Getenv("WRIT_TEST_OUTPUT"); path != "" {
		f, openErr := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if openErr != nil {
			s.t.Fatal(openErr)
		}
		_, writeErr := f.Write(append(slices.Clone(stdout), stderr...))
		closeErr := f.Close()
		if writeErr != nil || closeErr != nil {
			s.t.Fatal("capture writ output failed")
		}
	}
}

// run runs writ and fails the test unless it exits with want.
func (s *stackDir) run(want int, args ...string) result {
	s.t.Helper()
	r := s.exec(nil, args...)
	if r.code != want || r.killed {
		s.t.Fatalf("want exit %d\n%s", want, r)
	}
	return r
}

// runJSON runs writ, checks the exit code, and decodes stdout into v.
func (s *stackDir) runJSON(want int, v any, args ...string) result {
	s.t.Helper()
	r := s.run(want, args...)
	if err := json.Unmarshal([]byte(r.stdout), v); err != nil {
		s.t.Fatalf("decode JSON: %v\n%s", err, r)
	}
	return r
}

func (s *stackDir) write(name, text string) {
	s.t.Helper()
	if err := os.WriteFile(filepath.Join(s.dir, name), []byte(text), 0o644); err != nil {
		s.t.Fatal(err)
	}
}
