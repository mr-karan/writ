// Package servetest starts a dev control plane inside a test's process:
// the real server.Server, which runs no OpenTofu, on an httptest server,
// with its control database in SQLite in the test's temporary directory. A test points an App at it with Client,
// and the App finds it as it finds writ serve --dev, through client.json.
package servetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/mr-karan/writ/internal/cli"
	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/server"
)

// User is the dev user of every test server.
const User = "tester"

// Options configures Start.
type Options struct {
	// Log receives the server's log; nil discards it.
	Log *slog.Logger
}

// Server is a running test control plane.
type Server struct {
	URL   string
	Token string
	// Data is the dev data directory: control.db, state/, keys/, and
	// client.json.
	Data string
	// Home is the XDG_DATA_HOME whose writ/dev is Data.
	Home  string
	Store *control.Store
}

// Start starts a dev control plane and stops it when the test ends.
func Start(t testing.TB, o Options) *Server {
	t.Helper()
	ctx := context.Background()
	home := t.TempDir()
	data := filepath.Join(home, "writ", "dev")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	store, err := control.Open(ctx, control.Options{Location: filepath.Join(data, "control.db")})
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 32)
	rand.Read(b)
	token := "writd_" + hex.EncodeToString(b)
	hs := httptest.NewUnstartedServer(nil)
	url := "http://" + hs.Listener.Addr().String()
	cfg := server.DevConfig(server.DevOptions{URL: url, Listen: hs.Listener.Addr().String(), Database: filepath.Join(data, "control.db"), Data: data})
	srv, err := server.New(ctx, server.Options{Config: cfg, Store: store, DevToken: token, DevUser: User, Version: "test", Log: o.Log})
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	hs.Config.Handler = srv.Handler()
	hs.Start()
	t.Cleanup(func() {
		hs.Close()
		srv.Close()
		store.Close()
	})
	if err := cli.WriteDevClient(data, url, token, User); err != nil {
		t.Fatal(err)
	}
	return &Server{URL: url, Token: token, Data: data, Home: home, Store: store}
}

// Client points o at the server: the App finds it in client.json, as it
// finds writ serve --dev, and sees only the variables in env besides the
// data home and a config home of its own, so a session from writ login in
// the user's ~/.config/writ never picks another control plane.
func (s *Server) Client(o cli.Options, env map[string]string) cli.Options {
	o.LookupEnv = func(name string) (string, bool) {
		if name == "XDG_DATA_HOME" {
			return s.Home, true
		}
		if v, ok := env[name]; ok || name != "XDG_CONFIG_HOME" {
			return v, ok
		}
		return filepath.Join(s.Home, "config"), true
	}
	return o
}
