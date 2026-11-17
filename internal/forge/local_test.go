package forge_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/forge"
)

// lazyFetchVariants are the ways a partial clone's config makes a lazy
// fetch run a program: the promisor remote's upload-pack, the ssh
// command, and an ext:: URL that protocol.ext.allow lets through.
var lazyFetchVariants = []string{"uploadpack", "sshCommand", "ext"}

// partialClone makes a repository whose config says it is a partial
// clone, and removes the blob of stack/main.writ, so reading that blob
// makes git fetch it from the promisor remote. Each program the config
// names creates marker.
func partialClone(t *testing.T, variant string) (repo, head, marker string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo, marker = filepath.Join(base, "root"), filepath.Join(base, "MARKER")
	if err := os.MkdirAll(filepath.Join(repo, "stack"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "stack", "main.writ"), []byte("out x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-q")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", "x")
	head = git(t, repo, "rev-parse", "HEAD")
	blob := git(t, repo, "rev-parse", "HEAD:stack/main.writ")
	if err := os.Remove(filepath.Join(repo, ".git", "objects", blob[:2], blob[2:])); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(base, "planted.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch "+marker+"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "config", "core.repositoryformatversion", "1")
	git(t, repo, "config", "extensions.partialClone", "origin")
	git(t, repo, "config", "remote.origin.promisor", "true")
	switch variant {
	case "uploadpack":
		git(t, repo, "config", "remote.origin.url", filepath.Join(base, "none"))
		git(t, repo, "config", "remote.origin.uploadpack", script)
	case "sshCommand":
		git(t, repo, "config", "remote.origin.url", "ssh://example.invalid/x.git")
		git(t, repo, "config", "core.sshCommand", script)
	case "ext":
		git(t, repo, "config", "protocol.ext.allow", "always")
		git(t, repo, "config", "remote.origin.url", "ext::"+script)
	}
	return repo, head, marker
}

// An assistant in the project can make the repository a partial clone
// and remove an object. writ serve --dev then reads the blob, and git must
// not fetch it with a program the config names.
func TestLocalPartialCloneRunsNoProgram(t *testing.T) {
	for _, variant := range lazyFetchVariants {
		t.Run(variant, func(t *testing.T) {
			repo, head, marker := partialClone(t, variant)
			ctx := context.Background()
			files, err := forge.Local{}.Tree(ctx, repo, head, "")
			if err != nil || len(files) != 1 {
				t.Fatalf("tree %v, %v", files, err)
			}
			if _, err := (forge.Local{}).Blob(ctx, repo, files[0].Blob, 1<<20); err == nil {
				t.Error("read a blob the repository does not hold")
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("git ran a program the repository's config names")
			}
		})
	}
}

// fakeGit puts a git on PATH that starts a process that holds stdout and
// stderr open, then runs script. Cleanup ends that process.
func fakeGit(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	text := "#!/bin/sh\nsleep 30 &\necho $! > " + pidFile + "\n" + script
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(text), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Cleanup(func() {
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
}

// A process that git starts can hold git's stdout open after git exits or
// is killed. The call returns soon after git ends, not when that process
// does.
func TestLocalReturnsWhileAChildHoldsStdout(t *testing.T) {
	for name, script := range map[string]string{
		"git exits":  "echo out\n",
		"git hangs":  "exec sleep 30\n",
		"git killed": "echo out\nexec sleep 30\n",
	} {
		t.Run(name, func(t *testing.T) {
			fakeGit(t, script)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			start := time.Now()
			_, err := forge.Local{}.Blob(ctx, t.TempDir(), strings.Repeat("a", 40), 1<<20)
			if took := time.Since(start); took > 10*time.Second {
				t.Fatalf("the call took %s", took)
			}
			if err == nil {
				t.Fatal("a call whose output stayed open succeeded")
			}
		})
	}
}

// An assistant in the project can write .git/refs/replace/<X> with a plain
// file write, so git would serve another commit's content under X's id.
// writ serve --dev must read X's own tree.
func TestLocalIgnoresReplaceRefs(t *testing.T) {
	repo := t.TempDir()
	write := func(text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, "main.writ"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("out x = 1\n")
	git(t, repo, "init", "-q")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", "x")
	reviewed := git(t, repo, "rev-parse", "HEAD")
	write("out x = 2\n")
	git(t, repo, "commit", "-qam", "y")
	other := git(t, repo, "rev-parse", "HEAD")
	git(t, repo, "reset", "-q", "--hard", reviewed)
	ref := filepath.Join(repo, ".git", "refs", "replace", reviewed)
	if err := os.MkdirAll(filepath.Dir(ref), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ref, []byte(other+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	files, err := forge.Local{}.Tree(ctx, repo, reviewed, "")
	if err != nil || len(files) != 1 {
		t.Fatalf("tree %v, %v", files, err)
	}
	got, err := forge.Local{}.Blob(ctx, repo, files[0].Blob, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "out x = 1\n" {
		t.Errorf("the tree of %s read %q, the content of a replacement", reviewed[:8], got)
	}
}
