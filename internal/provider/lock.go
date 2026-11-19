// Package provider owns everything about Terraform provider binaries: finding
// them in the registry, pinning them in writ.lock, caching their schemas, and
// running them over the plugin protocol, version 5 or 6.
//
// It is the only package that imports internal/tfplugin5, internal/tfplugin6,
// google.golang.org/grpc, and github.com/zclconf/go-cty. Callers see
// schema.Provider, value.Value, value.Path, and Diag. The archtest package
// enforces this.
package provider

import (
	"cmp"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"

	"github.com/mr-karan/writ/internal/schema"
)

// LockFile is the lock file name in a stack directory.
const LockFile = "writ.lock"

// Lock pins each provider version, per platform, to the SHA-256 of its
// registry archive and of the extracted binary. The binary hash keys the
// schema cache, so a read finds a schema without hashing a 30 MB binary
// every time.
type Lock struct {
	entries []LockEntry
}

// LockEntry pins one provider version on one platform.
type LockEntry struct {
	Source   schema.Source
	Version  string
	Platform string // linux_amd64
	Archive  [32]byte
	Binary   [32]byte
}

// Requirement is one provider a stack requires: a source and an exact
// version.
type Requirement struct {
	StackDir string // owning stack, for shared-lock diagnostics
	Name     string // local name, for messages
	Source   schema.Source
	Version  string
	// Archives, when not nil, are the only archive hashes Install accepts,
	// such as the zh: hashes of an OpenTofu lock. Install refuses any other
	// archive before it downloads it or starts its binary.
	Archives [][32]byte
}

// ErrUnlistedArchive is the refusal of an archive whose hash is not one of
// a requirement's Archives.
var ErrUnlistedArchive = errors.New("the lock lists no hash for the archive")

// admit refuses an archive whose hash the requirement does not accept.
func (r Requirement) admit(platform string, archive [32]byte) error {
	if r.Archives == nil || slices.Contains(r.Archives, archive) {
		return nil
	}
	return fmt.Errorf("%s %s %s: %w, zh:%x, so writ does not start it", r.Source, r.Version, platform, ErrUnlistedArchive, archive)
}

// CurrentPlatform is the platform Writ runs on, such as linux_amd64.
func CurrentPlatform() string { return runtime.GOOS + "_" + runtime.GOARCH }

func parseHash(field, key string) ([32]byte, error) {
	var h [32]byte
	hexed, ok := strings.CutPrefix(field, key+"=sha256:")
	if !ok {
		return h, fmt.Errorf("want %s=sha256:<hex>, got %q", key, field)
	}
	b, err := hex.DecodeString(hexed)
	if err != nil || len(b) != len(h) {
		return h, fmt.Errorf("%s hash %q is not 64 hex digits", key, hexed)
	}
	copy(h[:], b)
	return h, nil
}

// Find returns the entry for src at version on the current platform.
func (l *Lock) Find(src schema.Source, version string) (LockEntry, bool) {
	return l.find(src, version, CurrentPlatform())
}

func (l *Lock) find(src schema.Source, version, platform string) (LockEntry, bool) {
	for _, e := range l.entries {
		if e.Source == src && e.Version == version && e.Platform == platform {
			return e, true
		}
	}
	return LockEntry{}, false
}

// with returns a copy of l holding e in place of any entry for the same
// source, version, and platform.
func (l *Lock) with(e LockEntry) *Lock {
	out := &Lock{entries: slices.DeleteFunc(slices.Clone(l.entries), func(x LockEntry) bool {
		return x.Source == e.Source && x.Version == e.Version && x.Platform == e.Platform
	})}
	out.entries = append(out.entries, e)
	out.sort()
	return out
}

func (l *Lock) sort() {
	slices.SortFunc(l.entries, func(a, b LockEntry) int {
		return cmp.Or(
			strings.Compare(a.Source.String(), b.Source.String()),
			strings.Compare(a.Version, b.Version),
			strings.Compare(a.Platform, b.Platform),
		)
	})
}

// Entries returns every entry, sorted, for writ serve, which checks each
// against the providers it allows.
func (l *Lock) Entries() []LockEntry { return slices.Clone(l.entries) }
