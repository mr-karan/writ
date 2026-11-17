//go:build !linux

package policy

import "os"

// limitMemory sets no hard cap outside Linux: macOS does not enforce
// RLIMIT_AS. The soft limit and the parent's deadline are the bounds there.
func limitMemory(uint64) error { return nil }

// closeInherited does nothing outside Linux, which has no /proc/self/fd.
// writ serve, which decides policies for others, runs on Linux.
func closeInherited() error { return nil }

// self is the writ binary that evaluates policies.
func self() (string, error) { return os.Executable() }
