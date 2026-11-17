//go:build !unix

package cli

import (
	"io/fs"
	"os"
)

// openSessionsFlags opens the session file. readSessions refuses a
// symbolic link with Lstat first on systems without O_NOFOLLOW.
const openSessionsFlags = os.O_RDONLY

// ownedByUser reports true: these systems keep owners in ACLs, which writ
// does not read.
func ownedByUser(fs.FileInfo) bool { return true }
