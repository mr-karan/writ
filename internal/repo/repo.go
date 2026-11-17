// Package repo turns one commit of a repository into what the control
// plane plans. Read lists the commit's tree under the repository's path,
// refuses a commit that holds .writ files, the code of Writ's retired
// language, and reads every .terraform.lock.hcl. It fetches from the forge
// only the blobs the store lacks, and refuses a blob whose content does
// not hash to its id.
//
// An OpenTofu stack is a directory with a committed .terraform.lock.hcl
// and .tf or .tofu files. Discover finds a stack by its lock file and the
// tree, and TofuSources reads one stack's code and other files and those
// of the local modules it calls for a plan, under the limits on one
// stack, so a file that is too large, or OpenTofu code elsewhere in the
// repository, refuses only the stacks that hold it.
package repo

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/mr-karan/writ/internal/diag"
	"github.com/mr-karan/writ/internal/forge"
	"github.com/mr-karan/writ/internal/tofu"
)

// Store keeps blobs by id. *control.Store is one.
type Store interface {
	// BlobSizes returns the size of each of ids the store holds, without
	// its content.
	BlobSizes(ctx context.Context, ids []string) (map[string]int64, error)
	Blobs(ctx context.Context, ids []string) (map[string][]byte, error)
	PutBlob(ctx context.Context, id string, content []byte, now time.Time) error
}

// Limits bound the files of one commit that the control plane reads.
type Limits struct {
	// Files and Bytes bound the lock files read and their total size, and
	// Files also the files of one OpenTofu stack.
	Files int
	Bytes int64
	// File bounds one .tf, .tofu, .tfvars, or lock file of an OpenTofu
	// stack.
	File int64
	// StackFile bounds any other file of an OpenTofu stack, and StackBytes
	// all the files of one stack and of the local modules it calls.
	StackFile  int64
	StackBytes int64
}

// DefaultLimits are the bounds of the design: 2,000 files and 16 MiB, and
// 1 MiB for one file. A file that an OpenTofu stack's code
// reads, such as a function's archive, may hold 16 MiB, the size of a
// whole bundle, and one stack 64 MiB, which bounds what one plan holds in
// memory and writes to its work directory.
var DefaultLimits = Limits{Files: 2000, Bytes: 16 << 20, File: 1 << 20, StackFile: 16 << 20, StackBytes: 64 << 20}

// Snapshot is the files of one commit that the control plane read.
type Snapshot struct {
	Commit string
	// Tree lists every regular file under the repository's path.
	Tree []forge.File
	// Files holds the files read, by path from the repository's path.
	Files map[string][]byte
	// Fetched counts the blobs read from the forge for this snapshot; the
	// rest came from the store.
	Fetched int
	Bytes   int64
	// fresh holds the blobs read from the forge, by id, which Keep stores.
	fresh map[string][]byte
	// tooLarge holds the OpenTofu lock files that Read left out for their
	// size, with the limit each passed, so that TofuSources
	// refuses only the stacks that read one.
	tooLarge map[string]int64
}

// Keep stores the blobs the snapshot read from the forge. Read stores
// nothing, so a commit that fails a check or is refused leaves no blob
// behind: the caller keeps the blobs once it has accepted the commit.
// The blobs are a cache of content by its hash, so a commit stored
// without them only costs a later fetch. Keep checks ctx before each
// blob, so a cancelled caller waits for one blob at most.
func (s *Snapshot) Keep(ctx context.Context, st Store, now time.Time) error {
	for _, id := range slices.Sorted(maps.Keys(s.fresh)) {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("store the blobs in the control database: %w", err)
		}
		if err := st.PutBlob(ctx, id, s.fresh[id], now); err != nil {
			return fmt.Errorf("store a blob in the control database: %w", err)
		}
		// TofuSources reads more blobs after the commit's were kept.
		delete(s.fresh, id)
	}
	return nil
}

// writExt is the extension of the code of Writ's retired language.
const writExt = ".writ"

// maxNamed bounds the .writ files a refusal names.
const maxNamed = 5

// Read reads the lock files of commit sha under dir in a project. tree,
// when not nil, is the commit's tree that was read before, so the forge is
// not asked for it again. A commit that holds a .writ file is refused,
// with the files named: writ serve plans OpenTofu stacks only. Read writes
// nothing to the store; Keep does.
func Read(ctx context.Context, c forge.Client, projectID, sha, dir string, tree []forge.File, st Store, lim Limits) (*Snapshot, error) {
	if tree == nil {
		var err error
		if tree, err = c.Tree(ctx, projectID, sha, dir); err != nil {
			return nil, err
		}
	}
	snap := &Snapshot{Commit: sha, Tree: tree, Files: map[string][]byte{}, fresh: map[string][]byte{}, tooLarge: map[string]int64{}}
	var locks []forge.File
	var writ []string
	for _, f := range tree {
		switch {
		case path.Ext(f.Path) == writExt:
			writ = append(writ, f.Path)
		case path.Base(f.Path) == tofu.LockFile:
			locks = append(locks, f)
		}
	}
	if len(writ) > 0 {
		slices.Sort(writ)
		return nil, &WritFilesError{Commit: sha, Names: writ}
	}
	// A lock file over the limit on one is left out, so it refuses only its
	// stack. A stack's code is read by TofuSources.
	if err := snap.count(locks, lim); err != nil {
		return nil, err
	}
	err := snap.load(ctx, c, projectID, locks, st, func(forge.File) int64 { return lim.File }, func(f forge.File, max int64) error {
		snap.tooLarge[f.Path] = max
		return nil
	}, snap.countBytes(lim))
	if err != nil {
		return nil, err
	}
	return snap, nil
}

// WritFilesError refuses a commit that holds .writ files, the code of
// Writ's retired language: Names lists them, sorted.
type WritFilesError struct {
	Commit string
	Names  []string
}

func (e *WritFilesError) Error() string {
	listed, more := e.Names, ""
	if len(listed) > maxNamed {
		listed, more = listed[:maxNamed], fmt.Sprintf(" and %d more", len(e.Names)-maxNamed)
	}
	return fmt.Sprintf("commit %s holds .writ files, the code of Writ's retired language, which writ serve no longer plans: %s%s; move those stacks to OpenTofu, or out of the repository's path", short(e.Commit), strings.Join(listed, ", "), more)
}

// count refuses files that would take the snapshot over its limit on
// files.
func (s *Snapshot) count(files []forge.File, lim Limits) error {
	if len(files) > 0 && len(s.Files)+len(files) > lim.Files {
		return fmt.Errorf("commit %s has over %d lock files", short(s.Commit), lim.Files)
	}
	return nil
}

// countBytes adds one file to the snapshot's bytes and refuses a total
// over its limit.
func (s *Snapshot) countBytes(lim Limits) func(forge.File, int64) error {
	return func(_ forge.File, n int64) error {
		if s.Bytes += n; s.Bytes > lim.Bytes {
			return fmt.Errorf("the lock files of commit %s are over %d bytes", short(s.Commit), lim.Bytes)
		}
		return nil
	}
}

// load reads files into the snapshot. It checks the size of every blob the
// store holds before it loads any content, reads the rest from the forge,
// each checked against its id, and loads the held blobs last. A file over
// limit(f) bytes goes to over, whose error refuses the read and whose nil
// leaves the file out. count takes each file that is read, with its size,
// before its content is kept, and its error refuses the read.
func (s *Snapshot) load(ctx context.Context, c forge.Client, projectID string, files []forge.File, st Store, limit func(forge.File) int64, over, count func(forge.File, int64) error) error {
	if len(files) == 0 {
		return nil
	}
	ids := make([]string, len(files))
	for i, f := range files {
		ids[i] = f.Blob
	}
	sizes, err := st.BlobSizes(ctx, ids)
	if err != nil {
		return fmt.Errorf("read blob sizes from the control database: %w", err)
	}
	var held []forge.File
	for _, f := range files {
		n, ok := sizes[f.Blob]
		if !ok {
			continue
		}
		if n > limit(f) {
			if err := over(f, limit(f)); err != nil {
				return err
			}
			continue
		}
		if err := count(f, n); err != nil {
			return err
		}
		held = append(held, f)
	}
	for _, f := range files {
		if _, ok := sizes[f.Blob]; ok {
			continue
		}
		b, ok := s.fresh[f.Blob]
		if !ok {
			b, err = c.Blob(ctx, projectID, f.Blob, limit(f))
			if _, big := errors.AsType[*forge.TooLargeError](err); big {
				if err := over(f, limit(f)); err != nil {
					return err
				}
				continue
			}
			if err != nil {
				return fmt.Errorf("read %s of commit %s: %w", f.Path, short(s.Commit), err)
			}
			if got := forge.BlobID(b); got != f.Blob {
				return fmt.Errorf("%s of commit %s does not match its blob id: the tree names %s and the content hashes to %s, so writ serve refuses the commit", f.Path, short(s.Commit), f.Blob, got)
			}
			s.fresh[f.Blob] = b
			s.Fetched++
		}
		if int64(len(b)) > limit(f) {
			if err := over(f, limit(f)); err != nil {
				return err
			}
			continue
		}
		if err := count(f, int64(len(b))); err != nil {
			return err
		}
		s.Files[f.Path] = b
	}
	if len(held) == 0 {
		return nil
	}
	heldIDs := make([]string, len(held))
	for i, f := range held {
		heldIDs[i] = f.Blob
	}
	content, err := st.Blobs(ctx, heldIDs)
	if err != nil {
		return fmt.Errorf("read blobs from the control database: %w", err)
	}
	for _, f := range held {
		b, ok := content[f.Blob]
		if !ok || int64(len(b)) != sizes[f.Blob] {
			return fmt.Errorf("blob %s of %s changed in the control database while it was read", short(f.Blob), f.Path)
		}
		s.Files[f.Path] = b
	}
	return nil
}

// Stack is one stack discovery found.
type Stack struct {
	// Dir is the stack's directory from the repository's path; the stack's
	// name on the control plane is the repository's prefix and Dir.
	Dir string
	// Providers are what its .terraform.lock.hcl pins.
	Providers []Provider
}

// Provider is one provider version a lock pins.
type Provider struct {
	Source, Version string
}

// Discover finds the stacks of a snapshot: each directory that holds a
// .terraform.lock.hcl and OpenTofu code. A commit with none is an error.
func Discover(snap *Snapshot) ([]Stack, diag.List, error) {
	dirs := snap.tofuStackDirs()
	if len(dirs) == 0 {
		return nil, nil, fmt.Errorf("commit %s holds no OpenTofu stack, a directory with a committed %s and .tf or .tofu files, under the repository's path; run tofu providers lock in each stack and commit its lock file", short(snap.Commit), tofu.LockFile)
	}
	var diags diag.List
	out := []Stack{}
	for _, d := range dirs {
		st := Stack{Dir: d, Providers: []Provider{}}
		lock := path.Join(d, tofu.LockFile)
		if max, big := snap.tooLarge[lock]; big {
			diags.Errorf(diag.Provider, diag.Span{File: lock}, "the lock file is over %d bytes, so a plan of the stack is refused", max)
			out = append(out, st)
			continue
		}
		ps, err := tofu.LockedProviders(snap.Files[lock])
		if err != nil {
			diags.Errorf(diag.Provider, diag.Span{File: lock}, "%v", err)
		}
		for _, p := range ps {
			st.Providers = append(st.Providers, Provider{Source: p.Source, Version: p.Version})
		}
		out = append(out, st)
	}
	return out, diags, nil
}

// root names the directory the snapshot's names start from in errors.
func (snap *Snapshot) root() string { return "the repository's path" }

// what names the snapshot in errors.
func (snap *Snapshot) what() string { return "commit " + short(snap.Commit) }

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
