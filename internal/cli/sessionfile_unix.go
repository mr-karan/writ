//go:build unix

package cli

import (
	"io/fs"
	"os"
	"syscall"
)

// openSessionsFlags opens the session file without following a symbolic
// link, and without waiting on a FIFO that stands in its place, so the
// checks on the open file are checks on what is read.
const openSessionsFlags = os.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK

// ownedByUser reports whether this process's user owns info's file.
func ownedByUser(info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid()
}
