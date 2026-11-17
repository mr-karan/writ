//go:build unix

package cli

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// sessionsAt is an App whose session file is at the returned path, which
// does not exist yet.
func sessionsAt(t *testing.T) (*App, string) {
	t.Helper()
	a := precedenceApp(t, t.TempDir(), "", nil)
	path, err := a.sessionsPath()
	if err != nil {
		t.Fatal(err)
	}
	return a, path
}

func TestSessionFileHardening(t *testing.T) {
	ctx := context.Background()
	body := []byte(`{"format":"writ.sessions-file/v0","sessions":{"https://writ.example.com":{"token":"writs_x","ends_at":"2026-11-10T13:00:00Z"}}}`)

	t.Run("a symbolic link is refused, and not written through", func(t *testing.T) {
		a, path := sessionsAt(t)
		target := filepath.Join(t.TempDir(), "elsewhere.json")
		if err := os.WriteFile(target, body, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		if _, _, err := a.readSessions(); err == nil || !strings.Contains(err.Error(), "symbolic link") {
			t.Fatalf("read through a link: %v", err)
		}
		if err := a.Logout(ctx, LogoutOptions{JSON: true, URL: "https://writ.example.com"}); err == nil || !strings.Contains(err.Error(), "symbolic link") {
			t.Fatalf("logout through a link: %v", err)
		}
		if b, _ := os.ReadFile(target); string(b) != string(body) {
			t.Fatalf("the link's target changed: %s", b)
		}
	})

	t.Run("a FIFO is refused without waiting for a writer", func(t *testing.T) {
		a, path := sessionsAt(t)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { _, _, err := a.readSessions(); done <- err }()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "not a regular file") {
				t.Fatalf("read a FIFO: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("reading the session file waits on a FIFO")
		}
	})

	t.Run("a directory is refused", func(t *testing.T) {
		a, path := sessionsAt(t)
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, _, err := a.readSessions(); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("read a directory: %v", err)
		}
	})

	t.Run("a file others may read is refused", func(t *testing.T) {
		a, path := sessionsAt(t)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := a.readSessions(); err == nil || !strings.Contains(err.Error(), "others may read it") {
			t.Fatalf("read a file of mode 0644: %v", err)
		}
	})

	t.Run("a file of another user is refused", func(t *testing.T) {
		other := fakeInfo{uid: uint32(os.Geteuid() + 1)}
		if ownedByUser(other) {
			t.Fatal("a file of another uid counts as the user's")
		}
		if !ownedByUser(fakeInfo{uid: uint32(os.Geteuid())}) {
			t.Fatal("the user's own file does not count as the user's")
		}
	})

	t.Run("a loose directory is made 0700 when writ writes", func(t *testing.T) {
		a, path := sessionsAt(t)
		dir := filepath.Dir(path)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		keep(t, a, map[string]savedSession{"https://writ.example.com": live("writs_y")})
		info, err := os.Stat(dir)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("directory %v %v, want mode 0700", info.Mode(), err)
		}
		if f, err := os.Stat(path); err != nil || f.Mode().Perm() != 0o600 {
			t.Fatalf("file %v %v, want mode 0600", f.Mode(), err)
		}
	})
}

// fakeInfo is a file's info with only an owner.
type fakeInfo struct{ uid uint32 }

func (f fakeInfo) Name() string       { return "sessions.json" }
func (f fakeInfo) Size() int64        { return 0 }
func (f fakeInfo) Mode() fs.FileMode  { return 0o600 }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return false }
func (f fakeInfo) Sys() any           { return &syscall.Stat_t{Uid: f.uid} }
