//go:build unix

package control_test

import (
	"context"
	"io/fs"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/mr-karan/writ/internal/control"
)

// The data directory of writ serve --dev holds the control database and the
// state of every stack. All of it is private to the owner, and so is the
// directory.
func TestDataDirectoryIsPrivate(t *testing.T) {
	old := syscall.Umask(0)
	defer syscall.Umask(old)
	ctx := context.Background()
	data := filepath.Join(t.TempDir(), "data")
	s, err := control.Open(ctx, control.Options{Location: filepath.Join(data, "control.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	err = filepath.WalkDir(data, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		want := fs.FileMode(0o600)
		if d.IsDir() {
			want = 0o700
		}
		rel, _ := filepath.Rel(data, path)
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s has mode %o, want %o", rel, got, want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
