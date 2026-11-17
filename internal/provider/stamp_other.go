//go:build !linux

package provider

import "io/fs"

// stampOf reports false where Writ does not read ctime, so every start hashes
// the binary.
func stampOf(fs.FileInfo) (fileStamp, bool) { return fileStamp{}, false }
