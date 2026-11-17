//go:build linux

package confine

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// The differential tests generate paths over one layout of directories,
// files, and links, and run each operation of Files and the plain kernel
// call on the same path. When the kernel's walk stays beneath the root, the
// two must agree. When it leaves, Files must return ErrOutside. When the
// kernel fails, Files must fail too.
//
// A walk that leaves the root cannot come back: no atom names a, root, or
// a link to root, so "stays beneath" is "ends beneath", which the kernel's
// own answer shows as an inode of the root's tree.

// atoms are the names a generated path is made of.
var atoms = []string{"alias", "chain", "up", "dup", "d", "e", "nested", "deep", "dest", "x", "fl", "out", "outside", "link", "missing", "..", ".", ""}

// tails returns every path of up to n atoms, the empty path first.
func tails(n int) []string {
	out := []string{""}
	level := []string{""}
	for range n {
		var next []string
		for _, p := range level {
			for _, a := range atoms {
				if p == "" {
					next = append(next, a+"/")
				} else {
					next = append(next, p+a+"/")
				}
			}
		}
		out = append(out, next...)
		level = next
	}
	for i, p := range out {
		out[i] = strings.TrimSuffix(p, "/")
	}
	return out
}

// spelling is where a generated path starts: in a working directory, given
// relative to the layout's base, or at the absolute path base+prefix when
// cwd is empty.
type spelling struct {
	cwd    string
	prefix string
}

var spellings = []spelling{
	{prefix: "/a/root/"},
	{cwd: "a/root", prefix: ""},
	{cwd: "a/root/nested", prefix: "../"},
	{cwd: "a", prefix: "root/"},
	{cwd: "a", prefix: "./root//"},
	{cwd: "outside", prefix: "../a/root/"},
}

// paths returns the path spelling s gives tail for Files, and the same
// path spelled from / for the kernel. The kernel spelling joins the working
// directory and the path as text, without cleaning: the base has no link
// on it, so the kernel walks both the same way.
func (s spelling) paths(base, tail string) (confined, kernel string) {
	if s.cwd == "" {
		p := base + s.prefix + tail
		return p, p
	}
	p := s.prefix + tail
	if p == "" || strings.HasPrefix(p, "/") {
		// An empty first atom would make the path absolute.
		p = "." + p
	}
	return p, base + "/" + s.cwd + "/" + p
}

// differentialLayout makes base/a/root, the confined directory, and
// base/outside, and returns base.
func differentialLayout(t *testing.T) string {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mkfiles(t, base,
		[]string{"a/root/d/e", "a/root/nested/deep", "a/root/nested/dest", "outside/d"},
		map[string]string{"a/root/x": "top", "a/root/nested/x": "nested", "a/root/nested/dest/x": "dest", "a/root/d/e/x": "e", "outside/x": "outside"},
		map[string]string{
			"a/root/alias":            "nested/deep",
			"a/root/chain":            "alias",
			"a/root/up":               "..",
			"a/root/dup":              "d/..",
			"a/root/fl":               "x",
			"a/root/out":              "../../outside",
			"a/root/nested/dest/link": "x",
			"a/root/missing":          "nowhere",
			"a/self":                  "root",
			"rootlink":                "a/root",
		})
	return base
}

type inode struct{ dev, ino uint64 }

func inodeOf(info fs.FileInfo) inode {
	st := info.Sys().(*syscall.Stat_t)
	return inode{dev: st.Dev, ino: st.Ino}
}

// inodes returns the inode of each entry in the tree at dir, links not
// followed, with dir itself.
func inodes(t *testing.T, dir string) map[inode]bool {
	t.Helper()
	out := map[inode]bool{}
	err := filepath.WalkDir(dir, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(p)
		if err == nil {
			out[inodeOf(info)] = true
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// agree checks one operation on one path. kernelErr is the kernel's error,
// and inside says whether the kernel's answer is beneath the root. same
// compares the two answers when both succeed.
func agree(op, path string, kernelErr error, inside bool, confinedErr error, same func() bool) error {
	switch {
	case kernelErr != nil:
		if confinedErr == nil {
			return fmt.Errorf("%s %s: the kernel fails with %v, Files does not", op, path, kernelErr)
		}
	case !inside:
		if !errors.Is(confinedErr, ErrOutside) {
			return fmt.Errorf("%s %s: the kernel leaves the root, Files gives %v", op, path, confinedErr)
		}
	case confinedErr != nil:
		return fmt.Errorf("%s %s: the kernel stays inside, Files fails with %v", op, path, confinedErr)
	case !same():
		return fmt.Errorf("%s %s: Files and the kernel disagree", op, path)
	}
	return nil
}

// TestDifferentialReads runs each read of Files beside the kernel's.
func TestDifferentialReads(t *testing.T) {
	base := differentialLayout(t)
	files := open(t, filepath.Join(base, "a", "root"))
	in := inodes(t, filepath.Join(base, "a", "root"))
	var failures []error
	checked := 0
	for i, s := range spellings {
		set := tails(2)
		if i == 0 {
			set = tails(3)
		}
		if s.cwd != "" {
			t.Chdir(filepath.Join(base, s.cwd))
		}
		for _, tail := range set {
			p, k := s.paths(base, tail)
			failures = append(failures, readsAgree(files, in, p, k)...)
			checked++
		}
	}
	for _, err := range failures[:min(len(failures), 20)] {
		t.Error(err)
	}
	t.Logf("%d paths, 6 operations each, %d disagreements", checked, len(failures))
}

// TestDifferentialRootAliases opens the root through a link to it, by an
// absolute and by a relative path, and runs each read beside the kernel's
// on paths that start with the spelling Open was given, from the working
// directory Open ran in and from others.
func TestDifferentialRootAliases(t *testing.T) {
	base := differentialLayout(t)
	in := inodes(t, filepath.Join(base, "a", "root"))
	for _, o := range []struct {
		cwd, dir string
		starts   []spelling
	}{
		{"", base + "/a/self", []spelling{{prefix: "/a/self/"}, {prefix: "/a/root/"}, {cwd: "a", prefix: "self/"}, {cwd: "outside", prefix: "../a/self/"}}},
		{"", base + "/rootlink", []spelling{{prefix: "/rootlink/"}, {prefix: "/a/root/"}, {cwd: ".", prefix: "./rootlink//"}, {cwd: "outside", prefix: "../rootlink/"}}},
		{"a", "self", []spelling{{cwd: "a", prefix: "self/"}, {cwd: "a", prefix: "./self//"}, {cwd: "a", prefix: "root/"}, {prefix: "/a/self/"}, {cwd: "outside", prefix: "../a/self/"}, {cwd: "a/root/nested", prefix: "../"}}},
		{".", "rootlink", []spelling{{cwd: ".", prefix: "rootlink/"}, {cwd: "outside", prefix: "../rootlink/"}, {prefix: "/rootlink/"}}},
		{"outside", "../a/self", []spelling{{cwd: "outside", prefix: "../a/self/"}, {cwd: "a", prefix: "self/"}, {prefix: "/a/self/"}}},
	} {
		t.Run(o.cwd+":"+strings.TrimPrefix(o.dir, base), func(t *testing.T) {
			if o.cwd != "" {
				t.Chdir(filepath.Join(base, o.cwd))
			}
			files, err := Open(o.dir)
			if err != nil {
				t.Fatal(err)
			}
			defer files.Close()
			var failures []error
			checked := 0
			for _, s := range o.starts {
				if s.cwd != "" {
					t.Chdir(filepath.Join(base, s.cwd))
				}
				for _, tail := range tails(2) {
					p, k := s.paths(base, tail)
					failures = append(failures, readsAgree(files, in, p, k)...)
					checked++
				}
			}
			for _, err := range failures[:min(len(failures), 20)] {
				t.Error(err)
			}
			t.Logf("%d paths, 6 operations each, %d disagreements", checked, len(failures))
		})
	}
}

// readsAgree runs each read of files on p and the kernel's on k, the same
// path spelled from /, and returns each disagreement. in holds the inodes
// of the root's tree.
func readsAgree(files Files, in map[inode]bool, p, k string) []error {
	insideStat := func(p string) bool {
		info, err := os.Stat(p)
		return err == nil && in[inodeOf(info)]
	}
	insideLstat := func(p string) bool {
		info, err := os.Lstat(p)
		return err == nil && in[inodeOf(info)]
	}
	var failures []error
	check := func(err error) {
		if err != nil {
			failures = append(failures, err)
		}
	}
	kInfo, kErr := os.Stat(k)
	cInfo, cErr := files.Stat(p)
	check(agree("stat", p, kErr, insideStat(k), cErr, func() bool { return os.SameFile(kInfo, cInfo) }))

	kInfo, kErr = os.Lstat(k)
	cInfo, cErr = files.Lstat(p)
	check(agree("lstat", p, kErr, insideLstat(k), cErr, func() bool { return os.SameFile(kInfo, cInfo) }))

	kBytes, kErr := os.ReadFile(k)
	cBytes, cErr := files.ReadFile(p)
	check(agree("read", p, kErr, insideStat(k), cErr, func() bool { return bytes.Equal(kBytes, cBytes) }))

	kEntries, kErr := os.ReadDir(k)
	cEntries, cErr := files.ReadDir(p)
	check(agree("readdir", p, kErr, insideStat(k), cErr, func() bool {
		return slices.EqualFunc(kEntries, cEntries, func(a, b fs.DirEntry) bool { return a.Name() == b.Name() })
	}))

	kTarget, kErr := os.Readlink(k)
	cTarget, cErr := files.Readlink(p)
	check(agree("readlink", p, kErr, insideLstat(k), cErr, func() bool { return kTarget == cTarget }))

	check(resolveAgrees(files, p, k, insideStat(k)))
	return failures
}

// resolveAgrees checks Resolve against stat. Where the kernel finds a file,
// Resolve must name the same one, spelled from the root's Dir with no link,
// . or .. after it. Where the kernel finds nothing at the last name,
// Resolve may give the path a write would make, which must be spelled
// beneath the root.
func resolveAgrees(files Files, p, k string, inside bool) error {
	kInfo, kErr := os.Stat(k)
	got, err := files.Resolve(p)
	if errors.Is(kErr, fs.ErrNotExist) {
		if err == nil && !strings.HasPrefix(got, files.Dir()+"/") {
			return fmt.Errorf("resolve %s: %s for a missing file is not beneath the root", p, got)
		}
		return nil
	}
	if err := agree("resolve", p, kErr, inside, err, func() bool {
		info, err := os.Stat(got)
		rest, under := strings.CutPrefix(got, files.Dir())
		real, rerr := filepath.EvalSymlinks(got)
		dir, derr := filepath.EvalSymlinks(files.Dir())
		return err == nil && os.SameFile(kInfo, info) && under && rerr == nil && derr == nil && real == dir+rest
	}); err != nil {
		return fmt.Errorf("%w (got %q)", err, got)
	}
	if kErr == nil {
		got, err := Files{}.Resolve(k)
		info, serr := os.Stat(got)
		if err != nil || serr != nil || !os.SameFile(kInfo, info) {
			return fmt.Errorf("resolve %s with the zero Files: %q, %v", k, got, err)
		}
	}
	return nil
}

// where returns the path the kernel gives the file at p, with no link on
// it, from the descriptor of p opened.
func where(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return os.Readlink(fmt.Sprintf("/proc/self/fd/%d", f.Fd()))
}

// TestDifferentialWrites runs each write of Files in one copy of the layout
// and the kernel's in another, beside it. At the end the two roots must hold
// the same tree, and Files' copy must be unchanged outside its root. A link
// Files refuses is removed from the kernel's root, so later paths agree.
func TestDifferentialWrites(t *testing.T) {
	kbase, cbase := differentialLayout(t), differentialLayout(t)
	files := open(t, filepath.Join(cbase, "a", "root"))
	before := snapshot(t, cbase)
	// inside says whether the kernel made p beneath the root.
	inside := func(p string) bool {
		got, err := where(p)
		return err == nil && (got == kbase+"/a/root" || strings.HasPrefix(got, kbase+"/a/root/"))
	}
	var failures []error
	check := func(err error) {
		if err != nil {
			failures = append(failures, err)
		}
	}
	n := 0
	for _, s := range spellings {
		if s.cwd != "" {
			t.Chdir(filepath.Join(cbase, s.cwd))
		}
		for _, tail := range tails(2) {
			n++
			dir, _ := s.paths(cbase, tail)
			_, kdir := s.paths(kbase, tail)
			sep := "/"
			if strings.HasSuffix(dir, "/") || dir == "" {
				sep = ""
			}
			name := fmt.Sprintf("w%d", n)
			kErr := os.WriteFile(kdir+sep+name, []byte(name), 0o644)
			kIn := inside(kdir + sep + name)
			cErr := files.WriteFileAtomic(dir+sep+name, []byte(name), 0o644)
			check(agree("write", dir+sep+name, kErr, kIn, cErr, func() bool { return true }))

			made := fmt.Sprintf("m%d", n)
			kErr = os.MkdirAll(kdir+sep+made+"/n", 0o755)
			kIn = inside(kdir + sep + made + "/n")
			cErr = files.MkdirAll(dir+sep+made+"/n", 0o755)
			check(agree("mkdirall", dir+sep+made+"/n", kErr, kIn, cErr, func() bool { return true }))
		}
	}
	// A link's target is walked from the link's directory. Each generated
	// target goes in a few directories, some spelled through a link and
	// then .., and each is checked against what the kernel's link reaches.
	for _, at := range []string{"", "alias/../", "nested/dest/", "chain/./../dest/"} {
		for _, tail := range tails(2) {
			for _, suffix := range []string{"", "/outside/x", "/x"} {
				target := tail + suffix
				if target == "" || strings.HasPrefix(target, "/") {
					continue
				}
				n++
				name := fmt.Sprintf("s%d", n)
				klink := kbase + "/a/root/" + at + name
				clink := cbase + "/a/root/" + at + name
				kErr := os.Symlink(target, klink)
				cErr := files.Symlink(target, clink)
				if kErr != nil {
					check(agree("symlink", clink, kErr, false, cErr, nil))
					continue
				}
				_, reach := os.Stat(klink)
				kIn := inside(klink)
				switch {
				case reach != nil:
					// A link the kernel cannot follow reaches nothing. Files
					// may refuse it, so the kernel's copy goes too.
					if cErr != nil {
						os.Remove(klink)
					}
				default:
					check(agree("symlink "+target, clink, nil, kIn, cErr, func() bool { return true }))
					if !kIn || cErr != nil {
						os.Remove(klink)
					}
				}
			}
		}
	}
	for _, err := range failures[:min(len(failures), 20)] {
		t.Error(err)
	}
	ktree, ctree := snapshot(t, kbase+"/a/root"), snapshot(t, cbase+"/a/root")
	differ := func(format string, args ...any) {
		failures = append(failures, fmt.Errorf(format, args...))
		if len(failures) < 40 {
			t.Errorf(format, args...)
		}
	}
	for p, v := range ktree {
		if got, ok := ctree[p]; !ok || got != v {
			differ("%s: the kernel's root holds %q, Files' %q", p, v, got)
		}
	}
	for p, v := range ctree {
		if _, ok := ktree[p]; !ok {
			differ("%s: only Files' root holds %q", p, v)
		}
	}
	after := snapshot(t, cbase)
	for p := range maps.Keys(before) {
		if v, ok := after[p]; !strings.HasPrefix(p, "a/root/") && (!ok || before[p] != v) {
			differ("%s: Files changed it outside the root to %q", p, v)
		}
	}
	for p, v := range after {
		if _, ok := before[p]; !ok && !strings.HasPrefix(p, "a/root/") {
			differ("%s: Files made it outside the root, holding %q", p, v)
		}
	}
	t.Logf("%d writes, %d entries in each root, %d disagreements", n, len(ktree), len(failures))
}

// snapshot maps each path under dir to its text, "dir", or the target of a
// link.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == dir {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			out[rel] = "-> " + target
			return err
		case d.IsDir():
			out[rel] = "dir"
		default:
			b, err := os.ReadFile(p)
			out[rel] = string(b)
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
