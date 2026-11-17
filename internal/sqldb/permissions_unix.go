//go:build unix

package sqldb

import (
	"io/fs"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func makeOwnedSidecarPrivate(path string, info fs.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Geteuid() {
		return nil
	}
	m := info.Mode().Perm()
	want := (m | 0o600) &^ 0o077
	if m == want {
		return nil
	}
	// No descriptor may be opened and closed here: that would release this
	// process's SQLite fcntl locks on the inode.
	return unix.Fchmodat(unix.AT_FDCWD, path, uint32(want), unix.AT_SYMLINK_NOFOLLOW)
}
