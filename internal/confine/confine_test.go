package confine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// layout makes base/root and base/outside, each with a file, and links in
// root to both, and returns root, outside, and Files confined to root.
func layout(t *testing.T) (string, string, Files) {
	t.Helper()
	base := t.TempDir()
	root, outside := filepath.Join(base, "root"), filepath.Join(base, "outside")
	for _, d := range []string{filepath.Join(root, "sub"), outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, text := range map[string]string{filepath.Join(root, "in.txt"): "in", filepath.Join(outside, "out.txt"): "out"} {
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	links := map[string]string{
		"rel-in":      "in.txt",
		"rel-out":     "../outside/out.txt",
		"abs-out":     filepath.Join(outside, "out.txt"),
		"dir-out":     outside,
		"sub/up-in":   "../in.txt",
		"sub/up-out":  "../../outside/out.txt",
		"chain-to-in": "sub/up-in",
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	files, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { files.Close() })
	return root, outside, files
}

func TestReadFileStaysInside(t *testing.T) {
	root, _, files := layout(t)
	tests := []struct {
		path string
		want string
	}{
		{path: "in.txt", want: "in"},
		{path: "rel-in", want: "in"},
		{path: "sub/up-in", want: "in"},
		{path: "chain-to-in", want: "in"},
		{path: "rel-out"},
		{path: "abs-out"},
		{path: "dir-out/out.txt"},
		{path: "sub/up-out"},
		{path: "../outside/out.txt"},
		{path: "sub/../../outside/out.txt"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got, err := files.ReadFile(filepath.Join(root, tt.path))
			if tt.want != "" {
				if err != nil || string(got) != tt.want {
					t.Fatalf("%q, %v; want %q", got, err, tt.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("read %q from outside", got)
			}
			if !errors.Is(err, ErrOutside) {
				t.Fatalf("err %v, want ErrOutside", err)
			}
		})
	}
	t.Run("the zero Files reads anywhere", func(t *testing.T) {
		got, err := Files{}.ReadFile(filepath.Join(root, "rel-out"))
		if err != nil || string(got) != "out" {
			t.Fatalf("%q, %v", got, err)
		}
	})
}

// A named pipe is refused without blocking, with and without a root.
func TestReadFileRefusesANamedPipe(t *testing.T) {
	root, _, files := layout(t)
	pipe := filepath.Join(root, "pipe")
	if err := syscall.Mkfifo(pipe, 0o644); err != nil {
		t.Fatal(err)
	}
	for name, f := range map[string]Files{"confined": files, "zero": {}} {
		t.Run(name, func(t *testing.T) {
			done := make(chan error, 1)
			go func() {
				_, err := f.ReadFile(pipe)
				done <- err
			}()
			select {
			case err := <-done:
				if !errors.Is(err, ErrNotRegular) {
					t.Fatalf("err %v, want ErrNotRegular", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("ReadFile blocked on a named pipe")
			}
		})
	}
}

func TestWritesStayInside(t *testing.T) {
	root, outside, files := layout(t)
	if err := files.WriteFileAtomic(filepath.Join(root, "rel-out"), []byte("new"), 0); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "out.txt")); string(b) != "out" {
		t.Fatalf("a write through a link changed the file outside: %q", b)
	}
	if err := files.WriteFileAtomic(filepath.Join(root, "dir-out", "new.txt"), []byte("new"), 0o644); err == nil {
		t.Fatal("wrote through a linked directory")
	}
	if err := files.MkdirAll(filepath.Join(root, "dir-out", "made"), 0o755); err == nil {
		t.Fatal("made a directory through a linked directory")
	}
	if err := files.Symlink("../../outside", filepath.Join(root, "sub", "link")); !errors.Is(err, ErrOutside) {
		t.Fatalf("err %v, want ErrOutside for a link whose target is outside", err)
	}
	if err := files.Symlink("../in.txt", filepath.Join(root, "sub", "link")); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 1 {
		t.Fatalf("outside holds %v, %v", entries, err)
	}
}

// A named pipe where a directory was is refused without blocking, with and
// without a root, both when it is there from the start and when it is
// swapped in and out of the directory's place while ReadDir runs.
func TestReadDirRefusesANamedPipe(t *testing.T) {
	root, _, files := layout(t)
	pipe := filepath.Join(root, "pipe")
	if err := syscall.Mkfifo(pipe, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("sub", filepath.Join(root, "dir-in")); err != nil {
		t.Fatal(err)
	}
	flip, aside := filepath.Join(root, "flip"), filepath.Join(root, "aside")
	if err := os.Mkdir(flip, 0o755); err != nil {
		t.Fatal(err)
	}
	// swap renames the pipe into flip's place and back until stop closes.
	// Each step leaves flip a directory, missing, or the pipe.
	swap := func(stop <-chan struct{}) error {
		for {
			select {
			case <-stop:
				return nil
			default:
			}
			for _, step := range [][2]string{{flip, aside}, {pipe, flip}, {flip, pipe}, {aside, flip}} {
				if err := os.Rename(step[0], step[1]); err != nil {
					return err
				}
			}
		}
	}
	for name, f := range map[string]Files{"confined": files, "zero": {}} {
		t.Run(name, func(t *testing.T) {
			done := make(chan error, 1)
			go func() {
				if _, err := f.ReadDir(pipe); !errors.Is(err, syscall.ENOTDIR) {
					done <- fmt.Errorf("a pipe: err %v, want ENOTDIR", err)
					return
				}
				if entries, err := f.ReadDir(filepath.Join(root, "dir-in")); err != nil || len(entries) != 2 {
					done <- fmt.Errorf("a link to a directory: %v, %v", entries, err)
					return
				}
				stop, swapped := make(chan struct{}), make(chan error, 1)
				go func() { swapped <- swap(stop) }()
				pipes := 0
				for range 2000 {
					if _, err := f.ReadDir(flip); errors.Is(err, syscall.ENOTDIR) {
						pipes++
					}
				}
				close(stop)
				if err := <-swapped; err != nil {
					done <- err
					return
				}
				t.Logf("found the pipe in place of the directory %d times", pipes)
				done <- nil
			}()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("ReadDir blocked on a named pipe")
			}
		})
	}
}

// An absolute link to a file inside the directory is refused with a
// message that asks for a relative link; one to a file outside is
// ErrOutside, as any link outside is.
func TestAbsoluteLinks(t *testing.T) {
	root, _, files := layout(t)
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"abs-in": filepath.Join(root, "in.txt"), "abs-real-in": filepath.Join(real, "in.txt"), "abs-dir-in": filepath.Join(root, "sub")} {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"abs-in", "abs-real-in", "abs-dir-in/up-in"} {
		t.Run(path, func(t *testing.T) {
			_, err := files.ReadFile(filepath.Join(root, path))
			if !errors.Is(err, ErrAbsoluteLink) || errors.Is(err, ErrOutside) || !strings.Contains(err.Error(), "replace it with a relative link") {
				t.Fatalf("err %v, want ErrAbsoluteLink", err)
			}
		})
	}
	if _, err := files.Stat(filepath.Join(root, "abs-out")); !errors.Is(err, ErrOutside) {
		t.Fatalf("err %v, want ErrOutside for an absolute link outside", err)
	}
}

func TestResolve(t *testing.T) {
	root, outside, files := layout(t)
	for _, d := range []string{filepath.Join(root, "stored", "claude", "skills"), filepath.Join(root, ".agents", "skills", "writ")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("stored/claude", filepath.Join(root, ".claude")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "in.txt"), filepath.Join(root, "abs-in")); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{filepath.Join(root, "claude"), filepath.Join(root, "nested", "deep"), filepath.Join(root, "nested", "claude")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for link, target := range map[string]string{"alias": "nested/deep", "dotdot": "alias/../claude"} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		path string
		want string
		err  error
	}{
		{path: "in.txt", want: "in.txt"},
		{path: "chain-to-in", want: "in.txt"},
		{path: ".claude/skills/writ", want: "stored/claude/skills/writ"},
		{path: "dotdot", want: "nested/claude"},
		{path: "alias/../claude", want: "nested/claude"},
		{path: "rel-out", err: ErrOutside},
		{path: "dir-out/out.txt", err: ErrOutside},
		{path: "abs-in", err: ErrAbsoluteLink},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got, err := files.Resolve(root + string(filepath.Separator) + tt.path)
			if tt.err != nil {
				if !errors.Is(err, tt.err) {
					t.Fatalf("%q, %v; want %v", got, err, tt.err)
				}
				return
			}
			if err != nil || got != filepath.Join(root, tt.want) {
				t.Fatalf("%q, %v; want %q", got, err, tt.want)
			}
		})
	}
	t.Run("the zero Files follows links anywhere", func(t *testing.T) {
		got, err := Files{}.Resolve(filepath.Join(root, "rel-out"))
		want, _ := filepath.EvalSymlinks(filepath.Join(outside, "out.txt"))
		if err != nil || got != want {
			t.Fatalf("%q, %v; want %q", got, err, want)
		}
	})
	t.Run("a link's target is checked from where its directory resolves", func(t *testing.T) {
		link := filepath.Join(root, ".claude", "skills", "writ")
		if err := files.Symlink("../../../.agents/skills/writ", link); err != nil {
			t.Fatal(err)
		}
		if info, err := files.Stat(link); err != nil || !info.IsDir() {
			t.Fatalf("%v, %v", info, err)
		}
		if err := files.Symlink("../../../../outside", filepath.Join(root, ".claude", "skills", "out")); !errors.Is(err, ErrOutside) {
			t.Fatalf("err %v, want ErrOutside", err)
		}
	})
}

// A read through a path whose .. follows a link goes where the kernel goes.
func TestReadFileAfterLinkThenDotDot(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{filepath.Join(root, "nested", "deep")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "x.txt"), []byte("top"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "x.txt"), []byte("nested"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("nested", "deep"), filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	files, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	path := root + "/alias/../x.txt"
	want, err := os.ReadFile(path)
	if err != nil || string(want) != "nested" {
		t.Fatalf("the kernel reads %q, %v", want, err)
	}
	if got, err := files.ReadFile(path); err != nil || string(got) != string(want) {
		t.Fatalf("%q, %v; want %q", got, err, want)
	}
}

// mkfiles makes each directory and file under dir, and each link with its
// target, in that order.
func mkfiles(t *testing.T, dir string, dirs []string, files, links map[string]string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, text := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
}

func open(t *testing.T, dir string) Files {
	t.Helper()
	files, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { files.Close() })
	return files
}

// An atomic write whose directory follows a link and then .. writes where
// the kernel writes: the temporary file goes next to the file, not to a
// directory that cleaning the path names.
func TestWriteFileAtomicAfterLinkThenDotDot(t *testing.T) {
	root := t.TempDir()
	mkfiles(t, root, []string{"nested/deep", "nested/dest"}, nil, map[string]string{"alias": "nested/deep"})
	files := open(t, root)
	if err := files.WriteFileAtomic(root+"/alias/../dest/new", []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "nested", "dest", "new")); err != nil || string(b) != "data" {
		t.Fatalf("nested/dest/new holds %q, %v", b, err)
	}
	if _, err := os.Lstat(filepath.Join(root, "dest")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("made a top-level dest: %v", err)
	}
}

// A link target is checked by walking it from the link's directory, so a
// link in the target that leads up out of the directory is seen.
func TestSymlinkRefusesATargetThatLeavesThroughALink(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "a", "root")
	mkfiles(t, base, []string{"a/root", "outside"}, map[string]string{"outside/x": "outside"}, map[string]string{"a/root/up": ".."})
	files := open(t, root)
	link := filepath.Join(root, "link")
	if err := files.Symlink("up/../outside/x", link); !errors.Is(err, ErrOutside) {
		t.Fatalf("err %v, want ErrOutside", err)
	}
	if _, err := os.Lstat(link); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("made the link: %v", err)
	}
}

// Resolve does not walk on through a regular file, as the kernel does not.
func TestResolveRefusesAFileAsADirectory(t *testing.T) {
	root := t.TempDir()
	mkfiles(t, root, []string{"nested"}, map[string]string{"x": "top", "nested/x": "nested"}, nil)
	files := open(t, root)
	path := root + "/x/../nested/x"
	if _, err := os.Stat(path); !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("the kernel gives %v", err)
	}
	if got, err := files.Resolve(path); !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("%q, %v; want ENOTDIR", got, err)
	}
}

// A relative path is walked from the working directory, as the kernel walks
// it, even when the working directory is above the confined directory.
func TestRelativePathFromOutsideTheDirectory(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	mkfiles(t, base, []string{"root", "other"}, map[string]string{"root/x": "in"}, nil)
	files := open(t, root)
	for _, tt := range []struct{ cwd, path string }{
		{cwd: base, path: "./root/x"},
		{cwd: base, path: "root//x"},
		{cwd: filepath.Join(base, "other"), path: "../root/x"},
	} {
		t.Run(tt.path, func(t *testing.T) {
			t.Chdir(tt.cwd)
			if b, err := files.ReadFile(tt.path); err != nil || string(b) != "in" {
				t.Fatalf("%q, %v; want %q", b, err, "in")
			}
		})
	}
	t.Run("a walk that leaves is refused", func(t *testing.T) {
		t.Chdir(root)
		if _, err := files.ReadFile("../root/x"); !errors.Is(err, ErrOutside) {
			t.Fatalf("err %v, want ErrOutside", err)
		}
	})
}

// A directory opened through a link to it, by a relative path, takes paths
// that start with that spelling, relative and absolute, as the kernel does.
func TestRelativePathThroughALinkToTheDirectory(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mkfiles(t, base, []string{"root"}, map[string]string{"root/x": "in"}, map[string]string{"alias": "root"})
	t.Chdir(base)
	files := open(t, "alias")
	for _, path := range []string{"alias/x", "./alias//x", filepath.Join(base, "alias", "x"), "root/x"} {
		if b, err := files.ReadFile(path); err != nil || string(b) != "in" {
			t.Errorf("%s: %q, %v; want %q", path, b, err, "in")
		}
	}
}
