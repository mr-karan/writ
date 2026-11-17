// Package pgtest gives tests a scratch Postgres: a socket-only server in a
// temporary directory, as just e2e-postgres starts one. A package's
// TestMain calls Main, and each test calls New for a database of its own.
// Tests skip when there is no server and initdb is not on PATH.
package pgtest

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Env names a Postgres server URL for tests. Main sets it when it starts a
// server; set it yourself to use another.
const Env = "WRIT_TEST_POSTGRES"

func run(name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(ctx, name, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %v\n%s", name, err, out)
	}
	return nil
}

// Start starts a server and returns its URL and a function that stops it.
func Start() (string, func(), error) {
	for _, bin := range []string{"initdb", "pg_ctl", "createdb"} {
		if _, err := exec.LookPath(bin); err != nil {
			return "", nil, err
		}
	}
	// The socket path must stay short, so its directory is in /tmp. The
	// data can be large, so it follows TMPDIR.
	sock, err := os.MkdirTemp("/tmp", "wpg.")
	if err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp("", "wpg-data.")
	if err != nil {
		os.RemoveAll(sock)
		return "", nil, err
	}
	remove := func() {
		os.RemoveAll(dir)
		os.RemoveAll(sock)
	}
	data := filepath.Join(dir, "data")
	stop := func() {
		_ = run("pg_ctl", "-D", data, "-m", "immediate", "-w", "stop")
		remove()
	}
	if err := run("initdb", "-D", data, "-A", "trust", "-U", "writ", "--no-locale", "--encoding=UTF8"); err != nil {
		remove()
		return "", nil, err
	}
	if err := run("pg_ctl", "-D", data, "-l", filepath.Join(dir, "server.log"), "-o", fmt.Sprintf("-k %s -c listen_addresses='' -c fsync=off", sock), "-w", "start"); err != nil {
		remove()
		return "", nil, err
	}
	return "postgres://writ@/postgres?host=" + url.QueryEscape(sock), stop, nil
}

// Main runs a package's tests with a server: the one Env names, else one it
// starts when it can, else none, and tests that need one skip.
func Main(m *testing.M) int {
	if os.Getenv(Env) == "" {
		if u, stop, err := Start(); err == nil {
			defer stop()
			os.Setenv(Env, u)
		}
	}
	return m.Run()
}

// New makes a new, empty database for the test and returns its URL.
func New(t testing.TB) string {
	t.Helper()
	base := os.Getenv(Env)
	if base == "" {
		t.Skipf("needs Postgres: run under nix shell nixpkgs#postgresql_18 -c, or set %s", Env)
	}
	return Database(t, base)
}

// Database makes a new, empty database on the server at base and returns
// its URL.
func Database(t testing.TB, base string) string {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal("the test Postgres URL is not a URL")
	}
	name := "test_" + strings.ToLower(rand.Text())
	if err := run("createdb", "--maintenance-db", base, name); err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}
