//go:build unix

package sqldb

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

type otherOwner struct{ fs.FileInfo }

func (i otherOwner) Sys() any { return &syscall.Stat_t{Uid: uint32(os.Geteuid() + 1)} }

func TestSidecarRepairRequiresOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sidecar")
	if err := os.WriteFile(path, nil, 0o444); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := makeOwnedSidecarPrivate(path, otherOwner{info}); err != nil {
		t.Fatal(err)
	}
	info, err = os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o444 {
		t.Fatalf("changed another owner's sidecar to %o", info.Mode().Perm())
	}
}

func TestSidecarRepairDoesNotFollowReplacementSymlink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sidecar")
	target := filepath.Join(dir, "unrelated")
	if err := os.WriteFile(path, nil, 0o444); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	_ = makeOwnedSidecarPrivate(path, info)
	info, err = os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("changed symlink target to %o", info.Mode().Perm())
	}
}
