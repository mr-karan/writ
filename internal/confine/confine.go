// Package confine reads and writes the files of a stack or a repository,
// either anywhere on the file system or only beneath one directory. Beneath
// a directory, every call goes through an os.Root, which refuses a path that
// leaves it through .. or a symbolic link, so a caller cannot forget a check.
package confine

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// Files reads and writes files by their OS paths. The zero Files reaches
// any path. A Files from Open reaches only paths beneath its directory.
// Each read refuses a file that is not a regular file, such as a named pipe
// or a device, without blocking on it.
//
// Beneath a directory, a path is never cleaned as text. The kernel walks a
// .. from wherever a symbolic link before it led, so cleaning a .. away
// can name a different file. Files turns a path into one spelled from the
// directory once, in rel, and the os.Root walks the rest as the kernel does.
type Files struct {
	root *os.Root
	// real is the directory's absolute path with no symbolic link on it.
	real string
	// name is the spelling Resolve returns: filepath.Abs of the directory
	// Open was given when that names the same directory, else real.
	name string
	// spellings are the absolute paths, without . or .., that named the
	// directory when Open ran: real, name, and the directory Open was given
	// spelled from the physical working directory. rel accepts a path that
	// reaches one of them.
	spellings [][]string
}

var (
	// ErrOutside means a path leaves the directory a Files is confined to.
	ErrOutside = errors.New("outside the directory writ may reach")
	// ErrNotRegular means a read found something other than a regular file.
	ErrNotRegular = errors.New("not a regular file")
	// ErrTooLarge means a read found a file over the size its caller allows.
	ErrTooLarge = errors.New("too large")
	// ErrAbsoluteLink means a path beneath a directory goes through a
	// symbolic link with an absolute target. os.Root follows only relative
	// links, and resolving absolute ones against the directory would mean
	// a second resolver beside it, so writ asks for a relative link.
	ErrAbsoluteLink = errors.New("a symbolic link with an absolute target, which writ does not follow beneath --dir; replace it with a relative link")
)

// Open returns Files confined to dir, which must be a directory. dir is
// walked as the kernel walks it. Close releases it.
func Open(dir string) (Files, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return Files{}, err
	}
	f, err := named(root, dir)
	if err != nil {
		root.Close()
		return Files{}, err
	}
	return f, nil
}

// named finds the absolute spellings of the directory root was opened on.
// Each must name the same directory as root, which a check by identity
// proves, so neither depends on how dir was cleaned.
func named(root *os.Root, dir string) (Files, error) {
	info, err := root.Stat(".")
	if err != nil {
		return Files{}, err
	}
	abs := dir
	if !filepath.IsAbs(dir) {
		wd, err := physicalWd()
		if err != nil {
			return Files{}, err
		}
		abs = wd + string(filepath.Separator) + dir
	}
	// EvalSymlinks walks .. from where each link led, as the kernel does.
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return Files{}, err
	}
	if ri, err := os.Stat(real); err != nil || !os.SameFile(ri, info) {
		return Files{}, fmt.Errorf("open %s: the directory changed while writ opened it", dir)
	}
	f := Files{root: root, real: real, name: real, spellings: [][]string{split(real)}}
	same := func(p string) bool {
		pi, err := os.Stat(p)
		return err == nil && os.SameFile(pi, info)
	}
	if abs, err := filepath.Abs(dir); err == nil && same(abs) {
		f.name = abs
	}
	for _, p := range []string{f.name, filepath.Clean(abs)} {
		names := split(p)
		if same(p) && !slices.ContainsFunc(f.spellings, func(s []string) bool { return slices.Equal(s, names) }) {
			f.spellings = append(f.spellings, names)
		}
	}
	return f, nil
}

// Sub returns Files confined to the directory at path. Beneath a directory,
// it opens path through f's own root, so a link on path swapped after a
// check cannot lead the new root outside f, and it refuses a path whose
// spelling no longer names the directory it opened. The zero Files opens
// path as Open does.
func (f Files) Sub(path string) (Files, error) {
	if f.root == nil {
		return Open(path)
	}
	rel, err := f.rel(path)
	if err != nil {
		return Files{}, err
	}
	root, err := f.root.OpenRoot(rel)
	if err != nil {
		return Files{}, f.explain(path, err)
	}
	sub, err := named(root, path)
	if err != nil {
		root.Close()
		return Files{}, err
	}
	return sub, nil
}

// physicalWd returns the working directory as the kernel knows it, with no
// symbolic link on it. os.Getwd may return $PWD, which can hold links.
func physicalWd() (string, error) {
	return syscall.Getwd()
}

// Dir returns the absolute path of the directory of Files from Open, spelled
// as Resolve spells paths, or "" for the zero Files.
func (f Files) Dir() string { return f.name }

// Close releases the directory of Files from Open.
func (f Files) Close() error {
	if f.root == nil {
		return nil
	}
	return f.root.Close()
}

// rel spells path from the root for the root to walk, or returns
// ErrOutside. Every part of path after the root keeps its .. steps.
//
// The walk starts where the kernel starts it: at / for an absolute path, at
// the physical working directory for a relative one. Until the walk reaches
// one of the root's spellings, rel follows no link. Each name must lead
// toward one of the spellings, and a .. is taken as the parent only on a
// path with no link on it: a prefix of the start or of the root's real path.
// There the parent is the lexical one. A path that reaches the root in any
// other way is refused. rel only chooses a start inside the root. The
// os.Root then refuses any walk that leaves it.
func (f Files) rel(path string) (string, error) {
	parts := strings.Split(path, string(filepath.Separator))
	var start []string
	if !filepath.IsAbs(path) {
		wd, err := physicalWd()
		if err != nil {
			return "", err
		}
		start = split(wd)
	}
	real := split(f.real)
	outside := &fs.PathError{Op: "open", Path: path, Err: ErrOutside}
	cur := start
	for i := 0; ; i++ {
		if j := slices.IndexFunc(f.spellings, func(anchor []string) bool { return hasPrefix(cur, anchor) }); j >= 0 {
			rest := strings.Join(append(slices.Clone(cur[len(f.spellings[j]):]), parts[i:]...), string(filepath.Separator))
			if strings.Trim(rest, string(filepath.Separator)) == "" {
				return ".", nil
			}
			return strings.TrimLeft(rest, string(filepath.Separator)), nil
		}
		if i == len(parts) {
			return "", outside
		}
		switch parts[i] {
		case "", ".":
		case "..":
			if !hasPrefix(start, cur) && !hasPrefix(real, cur) {
				return "", outside
			}
			if len(cur) > 0 {
				cur = cur[:len(cur)-1]
			}
		default:
			next := append(slices.Clone(cur), parts[i])
			if !slices.ContainsFunc(f.spellings, func(anchor []string) bool { return hasPrefix(anchor, next) }) {
				return "", outside
			}
			cur = next
		}
	}
}

// hasPrefix reports whether the names of p begin with the names of prefix.
func hasPrefix(p, prefix []string) bool {
	return len(p) >= len(prefix) && slices.Equal(p[:len(prefix)], prefix)
}

// split returns the names in the clean absolute path p.
func split(p string) []string {
	return strings.FieldsFunc(p, func(r rune) bool { return os.IsPathSeparator(uint8(r)) })
}

// escaped turns the error os.Root returns for a path that leaves it, which
// the os package does not export, into ErrOutside.
func escaped(err error) error {
	if pe, ok := errors.AsType[*fs.PathError](err); ok && pe.Err.Error() == "path escapes from parent" {
		return &fs.PathError{Op: pe.Op, Path: pe.Path, Err: ErrOutside}
	}
	return err
}

// explain turns the error of a call on path through the root into
// ErrOutside, or into ErrAbsoluteLink when an absolute link on path is why
// the root refused it.
func (f Files) explain(path string, err error) error {
	err = escaped(err)
	if !errors.Is(err, ErrOutside) {
		return err
	}
	_, rerr := f.Resolve(path)
	if pe, ok := errors.AsType[*fs.PathError](rerr); ok && errors.Is(rerr, ErrAbsoluteLink) {
		op := "open"
		if orig, ok := errors.AsType[*fs.PathError](err); ok {
			op = orig.Op
		}
		return &fs.PathError{Op: op, Path: path, Err: pe.Err}
	}
	return err
}

// maxLinks is how many symbolic links Resolve follows, as the kernel bounds
// a lookup.
const maxLinks = 40

// Resolve returns where path leads, with every symbolic link on it
// followed and no . or .. left, spelled as an absolute path. It walks path
// one name at a time, with Lstat and Readlink, as the kernel walks it: a
// .. goes up from where the links before it led, and a name after a file
// that is not a directory is ENOTDIR. The end of path that does not exist
// yet stays as spelled, and a .. there is ENOENT, as the kernel gives.
// Beneath a directory, every Lstat and Readlink goes through the root, a
// walk that leaves it is ErrOutside, and a link with an absolute target
// inside is ErrAbsoluteLink. Resolve only reads links: a call on its result
// still goes through the root, which refuses a link swapped in since.
func (f Files) Resolve(path string) (string, error) {
	fail := func(err error) (string, error) {
		return "", &fs.PathError{Op: "resolve", Path: path, Err: err}
	}
	var cur []string
	lstat, readlink := os.Lstat, os.Readlink
	spell := func(names []string) string {
		return string(filepath.Separator) + filepath.Join(names...)
	}
	if f.root == nil {
		if !filepath.IsAbs(path) {
			wd, err := physicalWd()
			if err != nil {
				return "", err
			}
			cur = split(wd)
		}
	} else {
		rest, err := f.rel(path)
		if err != nil {
			return fail(ErrOutside)
		}
		path = rest
		spell = func(names []string) string { return filepath.Join(append([]string{f.name}, names...)...) }
		lstat = func(p string) (fs.FileInfo, error) {
			info, err := f.root.Lstat(p)
			return info, escaped(err)
		}
		readlink = func(p string) (string, error) {
			target, err := f.root.Readlink(p)
			return target, escaped(err)
		}
	}
	// at spells names for lstat and readlink: from / for the zero Files,
	// from the root beneath a directory.
	at := func(names []string) string {
		if f.root == nil {
			return spell(names)
		}
		return filepath.Join(names...)
	}
	parts := strings.Split(path, string(filepath.Separator))
	links, absolute := 0, ""
	for len(parts) > 0 {
		part := parts[0]
		parts = parts[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			if len(cur) == 0 {
				if f.root != nil {
					return fail(ErrOutside)
				}
				continue
			}
			cur = cur[:len(cur)-1]
			continue
		}
		next := append(slices.Clone(cur), part)
		info, err := lstat(at(next))
		if errors.Is(err, fs.ErrNotExist) {
			for _, p := range parts {
				if p == ".." {
					return fail(syscall.ENOENT)
				}
				if p != "" && p != "." {
					next = append(next, p)
				}
			}
			cur = next
			break
		}
		if err != nil {
			return "", err
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			if links++; links > maxLinks {
				return fail(syscall.ELOOP)
			}
			target, err := readlink(at(next))
			if err != nil {
				return "", err
			}
			if filepath.IsAbs(target) {
				if f.root != nil {
					rest, err := f.rel(target)
					if err != nil {
						return fail(ErrOutside)
					}
					if absolute == "" {
						absolute = spell(next)
					}
					target = rest
				}
				cur = nil
			}
			// The target's .. steps are walked one at a time, never cleaned
			// away, because a link before one moves where it leads.
			parts = append(strings.Split(target, string(filepath.Separator)), parts...)
		case info.IsDir():
			cur = next
		case len(parts) > 0:
			return fail(syscall.ENOTDIR)
		default:
			cur = next
		}
	}
	if absolute != "" {
		return fail(fmt.Errorf("%s is %w", absolute, ErrAbsoluteLink))
	}
	return spell(cur), nil
}

// ReadFile returns the bytes of the regular file at path.
func (f Files) ReadFile(path string) ([]byte, error) {
	return f.readFile(path, -1)
}

// ReadFileMax is ReadFile for a file of at most max bytes. It reads at most
// max+1 bytes, and refuses a larger file with ErrTooLarge.
func (f Files) ReadFileMax(path string, max int64) ([]byte, error) {
	return f.readFile(path, max)
}

// readFile reads the regular file at path, with no bound when max < 0.
func (f Files) readFile(path string, max int64) ([]byte, error) {
	// A stat first keeps a device from being opened at all. O_NONBLOCK keeps
	// a named pipe swapped in after the stat from blocking the open, and the
	// stat of the open file decides.
	if err := f.regular(path); err != nil {
		return nil, err
	}
	file, err := f.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, notRegular(path, info)
	}
	if max < 0 {
		return io.ReadAll(file)
	}
	b, err := io.ReadAll(io.LimitReader(file, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, &fs.PathError{Op: "read", Path: path, Err: fmt.Errorf("over %d bytes: %w", max, ErrTooLarge)}
	}
	return b, nil
}

// regular returns an error unless path is a regular file or is missing.
func (f Files) regular(path string) error {
	info, err := f.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if !info.Mode().IsRegular() {
		return notRegular(path, info)
	}
	return nil
}

func notRegular(path string, info fs.FileInfo) error {
	what := "something else"
	switch {
	case info.IsDir():
		what = "a directory"
	case info.Mode()&fs.ModeNamedPipe != 0:
		what = "a named pipe"
	case info.Mode()&fs.ModeDevice != 0:
		what = "a device"
	case info.Mode()&fs.ModeSocket != 0:
		what = "a socket"
	}
	return &fs.PathError{Op: "read", Path: path, Err: fmt.Errorf("is %s: %w", what, ErrNotRegular)}
}

// ReadDir returns the entries of the directory at path, sorted by name.
// Anything else at path, such as a named pipe swapped in for the directory
// after a caller checked it, is refused without blocking.
func (f Files) ReadDir(path string) ([]fs.DirEntry, error) {
	dir, err := f.OpenFile(path, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return entries, err
}

// Stat returns the file at path, following symbolic links.
func (f Files) Stat(path string) (fs.FileInfo, error) {
	if f.root == nil {
		return os.Stat(path)
	}
	rel, err := f.rel(path)
	if err != nil {
		return nil, err
	}
	info, err := f.root.Stat(rel)
	return info, f.explain(path, err)
}

// Lstat returns the file at path, not following a final symbolic link.
func (f Files) Lstat(path string) (fs.FileInfo, error) {
	if f.root == nil {
		return os.Lstat(path)
	}
	rel, err := f.rel(path)
	if err != nil {
		return nil, err
	}
	info, err := f.root.Lstat(rel)
	return info, f.explain(path, err)
}

// Readlink returns the target of the symbolic link at path.
func (f Files) Readlink(path string) (string, error) {
	if f.root == nil {
		return os.Readlink(path)
	}
	rel, err := f.rel(path)
	if err != nil {
		return "", err
	}
	target, err := f.root.Readlink(rel)
	return target, f.explain(path, err)
}

// OpenFile opens the file at path as os.OpenFile does.
func (f Files) OpenFile(path string, flag int, perm fs.FileMode) (*os.File, error) {
	if f.root == nil {
		return os.OpenFile(path, flag, perm)
	}
	rel, err := f.rel(path)
	if err != nil {
		return nil, err
	}
	file, err := f.root.OpenFile(rel, flag, perm)
	return file, f.explain(path, err)
}

// MkdirAll creates the directory at path and its missing parents. Beneath
// a directory it takes the steps os.MkdirAll takes, each through the root:
// it cuts path as text, keeping every .., so each parent is the directory
// the kernel finds. os.Root's own MkdirAll would also create the target of
// a link that leads nowhere, which os.MkdirAll refuses.
func (f Files) MkdirAll(path string, perm fs.FileMode) error {
	if f.root == nil {
		return os.MkdirAll(path, perm)
	}
	info, err := f.Stat(path)
	if err == nil {
		if info.IsDir() {
			return nil
		}
		return &fs.PathError{Op: "mkdir", Path: path, Err: syscall.ENOTDIR}
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if dir, _ := cut(strings.TrimRight(path, string(filepath.Separator))); len(dir) > 1 && dir != "."+string(filepath.Separator) {
		if err := f.MkdirAll(dir[:len(dir)-1], perm); err != nil {
			return err
		}
	}
	rel, err := f.rel(path)
	if err != nil {
		return err
	}
	if err := f.root.Mkdir(rel, perm); err != nil {
		if info, lerr := f.Lstat(path); lerr == nil && info.IsDir() {
			return nil
		}
		return f.explain(path, err)
	}
	return nil
}

// Symlink creates a symbolic link at path whose target is target. Beneath a
// directory, target must be relative, and Resolve must find it beneath the
// directory when it walks target from the directory that holds path, as the
// kernel will walk the link.
func (f Files) Symlink(target, path string) error {
	if f.root == nil {
		return os.Symlink(target, path)
	}
	if filepath.IsAbs(target) {
		return &fs.PathError{Op: "symlink", Path: path, Err: ErrOutside}
	}
	rel, err := f.rel(path)
	if err != nil {
		return err
	}
	dir, _ := cut(path)
	if _, err := f.Resolve(dir + target); err != nil {
		if errors.Is(err, ErrOutside) {
			return &fs.PathError{Op: "symlink", Path: path, Err: ErrOutside}
		}
		return err
	}
	return f.explain(path, f.root.Symlink(target, rel))
}

// cut splits path after its last separator, as spelled: dir keeps that
// separator, or is ./ when path has none. Unlike filepath.Dir it keeps
// every .., so dir is still the directory the kernel finds.
func cut(path string) (dir, name string) {
	i := strings.LastIndexFunc(path, func(r rune) bool { return os.IsPathSeparator(uint8(r)) })
	if i < 0 {
		return "." + string(filepath.Separator), path
	}
	return path[:i+1], path[i+1:]
}

func (f Files) rename(from, to string) error {
	if f.root == nil {
		return os.Rename(from, to)
	}
	relFrom, err := f.rel(from)
	if err != nil {
		return err
	}
	relTo, err := f.rel(to)
	if err != nil {
		return err
	}
	return f.explain(to, f.root.Rename(relFrom, relTo))
}

// Remove removes the file, empty directory, or symbolic link at path.
func (f Files) Remove(path string) error {
	if f.root == nil {
		return os.Remove(path)
	}
	rel, err := f.rel(path)
	if err != nil {
		return err
	}
	return f.explain(path, f.root.Remove(rel))
}

// WriteFileAtomic writes data through a new file in the same directory and
// renames it over path, so a crash never leaves a half-written file. A
// symbolic link at path is replaced, not followed. A zero perm keeps the
// mode of the file at path, or else is 0o644.
func (f Files) WriteFileAtomic(path string, data []byte, perm fs.FileMode) error {
	if perm == 0 {
		perm = 0o644
		if info, err := f.Stat(path); err == nil {
			perm = info.Mode().Perm()
		}
	}
	// The temporary file is spelled next to path's last name, so its
	// directory is the one the kernel finds for path.
	dir, name := cut(path)
	if name == "" || name == "." || name == ".." {
		return &fs.PathError{Op: "write", Path: path, Err: syscall.EISDIR}
	}
	var tmp string
	var file *os.File
	for range 10 {
		var nonce [8]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return err
		}
		tmp = dir + "." + name + ".tmp" + hex.EncodeToString(nonce[:])
		var err error
		file, err = f.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
	}
	if file == nil {
		return fmt.Errorf("create a temporary file next to %s: every name was taken", path)
	}
	defer f.Remove(tmp)
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Chmod(perm); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return f.rename(tmp, path)
}
