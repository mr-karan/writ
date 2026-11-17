//go:build !unix

package sqldb

import "io/fs"

func makeOwnedSidecarPrivate(string, fs.FileInfo) error { return nil }
