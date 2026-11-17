package cli

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// gitBase is a temporary directory with no link in its path, the
// repository's parent.
func gitBase(t *testing.T) string {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return base
}

// markerScript writes a script that creates marker, and returns its path.
// Each program the repository names runs it, so a marker that exists
// shows that git ran a program the project's files name.
func markerScript(t *testing.T, dir, marker string) string {
	t.Helper()
	script := filepath.Join(dir, "planted.sh")
	write(t, script, "#!/bin/sh\ntouch "+marker+"\ncat\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// makeStale moves the stack file's time forward, so git that reads the
// work tree refreshes its index.
func makeStale(t *testing.T, repo string) {
	t.Helper()
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(repo, "stack", "main.tf"), future, future); err != nil {
		t.Fatal(err)
	}
}

func noMarker(t *testing.T, marker, what string) {
	t.Helper()
	if _, err := os.Stat(marker); err == nil {
		t.Errorf("%s: git ran a program the repository names", what)
		os.Remove(marker)
	}
}

// An assistant can write .git/config, .git/hooks, .gitattributes, and
// .git/info/attributes in the project. The plan tool must not make git run
// a program those name.
func TestMCPWorkTreeRunsNoPlantedProgram(t *testing.T) {
	cases := []struct {
		name  string
		plant func(t *testing.T, repo, script string)
	}{
		{"core.fsmonitor", func(t *testing.T, repo, script string) {
			gitRun(t, repo, "config", "core.fsmonitor", script+"; false")
		}},
		{"clean filter", func(t *testing.T, repo, script string) {
			gitRun(t, repo, "config", "filter.x.clean", script)
			write(t, filepath.Join(repo, ".gitattributes"), "*.tf filter=x\n")
			gitRun(t, repo, "add", ".gitattributes")
			gitRun(t, repo, "commit", "-qm", "attrs")
			makeStale(t, repo)
		}},
		{"process filter in info/attributes", func(t *testing.T, repo, script string) {
			gitRun(t, repo, "config", "filter.x.process", script)
			write(t, filepath.Join(repo, ".git", "info", "attributes"), "*.tf filter=x\n")
			makeStale(t, repo)
		}},
		{"post-index-change hook", func(t *testing.T, repo, script string) {
			hook := filepath.Join(repo, ".git", "hooks", "post-index-change")
			write(t, hook, "#!/bin/sh\nexec "+script+"\n")
			os.Chmod(hook, 0o755)
			makeStale(t, repo)
		}},
		{"core.hooksPath in project", func(t *testing.T, repo, script string) {
			hook := filepath.Join(repo, "h", "post-index-change")
			write(t, hook, "#!/bin/sh\nexec "+script+"\n")
			os.Chmod(hook, 0o755)
			gitRun(t, repo, "add", "h")
			gitRun(t, repo, "commit", "-qm", "h")
			gitRun(t, repo, "config", "core.hooksPath", "h")
			makeStale(t, repo)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base := gitBase(t)
			repo, marker := filepath.Join(base, "root"), filepath.Join(base, "MARKER")
			gitRepo(t, repo, "out x = 1\n")
			c.plant(t, repo, markerScript(t, base, marker))
			os.Remove(marker)
			app, dir := mcpApp(t, repo, "stack")
			wt := app.findWorkTree(context.Background(), reposRemote(t, repo), dir)
			if wt.source == nil {
				t.Fatalf("no commit to plan: %s", wt.why)
			}
			noMarker(t, marker, "findWorkTree")
		})
	}
}

// Each git command the plan tool runs starts no program that the
// repository's config, hooks, or attributes name, and reads no system or
// global config.
func TestMCPGitCommandsRunNoPlantedProgram(t *testing.T) {
	base := gitBase(t)
	repo, marker := filepath.Join(base, "root"), filepath.Join(base, "MARKER")
	head := gitRepo(t, repo, "out x = 1\n")
	gitRun(t, repo, "update-ref", "refs/remotes/origin/main", head)
	script := markerScript(t, base, marker)
	hook := "#!/bin/sh\nexec " + script + "\n"
	for _, name := range []string{"post-index-change", "reference-transaction", "post-checkout", "pre-auto-gc", "post-rewrite", "fsmonitor-watchman"} {
		for _, dir := range []string{filepath.Join(repo, ".git", "hooks"), filepath.Join(repo, "h")} {
			write(t, filepath.Join(dir, name), hook)
			os.Chmod(filepath.Join(dir, name), 0o755)
		}
	}
	for key, value := range map[string]string{
		"core.fsmonitor": script, "core.hooksPath": "h", "core.pager": script, "pager.branch": script,
		"filter.x.clean": script, "filter.x.smudge": script, "filter.x.process": script,
		"diff.external": script, "diff.x.textconv": script, "core.sshCommand": script,
		"core.alternateRefsCommand": script, "gpg.program": script, "credential.helper": "!" + script,
		"alias.branch": "!" + script, "remote.origin.url": "https://git.example.com/acme/infra.git",
	} {
		gitRun(t, repo, "config", key, value)
	}
	attrs := "* filter=x diff=x\n"
	write(t, filepath.Join(repo, ".gitattributes"), attrs)
	write(t, filepath.Join(repo, ".git", "info", "attributes"), attrs)
	makeStale(t, repo)
	os.Remove(marker)
	// An inherited environment that names programs or other config.
	t.Setenv("GIT_EXTERNAL_DIFF", script)
	t.Setenv("GIT_PAGER", script)
	t.Setenv("GIT_CONFIG_PARAMETERS", "'core.fsmonitor'='"+script+"'")

	app, dir := mcpApp(t, repo, "stack")
	git, why := app.openGitTree(context.Background(), dir)
	if why != "" {
		t.Fatal(why)
	}
	defer git.close()
	ctx := context.Background()
	for _, args := range [][]string{
		{"config", "--get-regexp", `^remote\..*\.url$`},
		{"rev-parse", "HEAD"},
		{"branch", "-r", "--contains", head},
		{"symbolic-ref", "-q", "HEAD"},
	} {
		if _, err := git.output(ctx, args...); err != nil {
			t.Errorf("git %s: %v", strings.Join(args, " "), err)
		}
		noMarker(t, marker, strings.Join(args, " "))
	}
	// The commands above start no program even with the repository's
	// settings; these overrides keep it so for a git that would.
	for key, want := range map[string]string{"core.fsmonitor": "false", "core.hooksPath": "/dev/null"} {
		if got, err := git.output(ctx, "config", key); err != nil || got != want {
			t.Errorf("git sees %s %q, %v; want %q over the repository's", key, got, err, want)
		}
	}
	wt := app.findWorkTree(ctx, reposRemote(t, repo), dir)
	if wt.source == nil {
		t.Errorf("no commit to plan: %s", wt.why)
	}
	noMarker(t, marker, "findWorkTree")
}

// git beneath a directory reads only the repository's config: a value in a
// global or system config, or in config the environment passes, is not
// seen.
func TestMCPGitReadsNoGlobalConfig(t *testing.T) {
	base := gitBase(t)
	repo := filepath.Join(base, "root")
	gitRepo(t, repo, "out x = 1\n")
	global := filepath.Join(base, "global")
	write(t, global, "[remote \"leak\"]\n\turl = https://global.example.com/x.git\n")
	system := filepath.Join(base, "system")
	write(t, system, "[remote \"leak2\"]\n\turl = https://system.example.com/x.git\n")
	write(t, filepath.Join(base, "home", ".gitconfig"), "[remote \"leak3\"]\n\turl = https://home.example.com/x.git\n")
	t.Setenv("HOME", filepath.Join(base, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "home"))
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_SYSTEM", system)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "remote.leak4.url")
	t.Setenv("GIT_CONFIG_VALUE_0", "https://env.example.com/x.git")
	app, dir := mcpApp(t, repo, "stack")
	git, why := app.openGitTree(context.Background(), dir)
	if why != "" {
		t.Fatal(why)
	}
	defer git.close()
	out, _ := git.output(context.Background(), "config", "--get-regexp", `^remote\..*\.url$`)
	if strings.Contains(out, "example.com") {
		t.Fatalf("git read config from outside the repository: %q", out)
	}
	if out, err := gitOutput(context.Background(), repo, "config", "--get-regexp", `^remote\..*\.url$`); err != nil || !strings.Contains(out, "global.example.com") {
		t.Fatalf("git outside --dir does not read the global config, so this test shows nothing: %q, %v", out, err)
	}
}

// .git/commondir and objects/info/alternates name other Git directories by
// path, as a .git file does, so the plan tool refuses them.
func TestMCPGitDirNamesNoOtherRepository(t *testing.T) {
	for _, name := range []string{"commondir", filepath.Join("objects", "info", "alternates")} {
		t.Run(name, func(t *testing.T) {
			base := gitBase(t)
			repo, outside := filepath.Join(base, "root"), filepath.Join(base, "outside")
			gitRepo(t, repo, "out x = 1\n")
			gitRepo(t, outside, "out x = 2\n")
			target := filepath.Join(outside, ".git")
			if name != "commondir" {
				target = filepath.Join(target, "objects")
			}
			write(t, filepath.Join(repo, ".git", name), target+"\n")
			app, dir := mcpApp(t, repo, "stack")
			git, why := app.openGitTree(context.Background(), dir)
			if git.tree != nil {
				git.close()
				t.Fatal("opened a work tree whose Git directory names another")
			}
			if want := "names another Git directory by path"; !strings.Contains(why, want) || strings.Contains(why, outside) {
				t.Fatalf("why %q, want %q and nothing outside", why, want)
			}
		})
	}
}

// An assistant can make the repository a partial clone, with a promisor
// remote whose fetch runs a program, and remove the commit's tree. git
// ls-tree then needs the tree, and must not fetch it.
func TestMCPPartialCloneRunsNoProgram(t *testing.T) {
	for _, variant := range []string{"uploadpack", "sshCommand", "ext"} {
		t.Run(variant, func(t *testing.T) {
			base := gitBase(t)
			repo, marker := filepath.Join(base, "root"), filepath.Join(base, "MARKER")
			gitRepo(t, repo, "out x = 1\n")
			script := markerScript(t, base, marker)
			tree := gitRun(t, repo, "rev-parse", "HEAD^{tree}")
			if err := os.Remove(filepath.Join(repo, ".git", "objects", tree[:2], tree[2:])); err != nil {
				t.Fatal(err)
			}
			gitRun(t, repo, "config", "core.repositoryformatversion", "1")
			gitRun(t, repo, "config", "extensions.partialClone", "origin")
			gitRun(t, repo, "config", "remote.origin.promisor", "true")
			switch variant {
			case "uploadpack":
				gitRun(t, repo, "config", "remote.origin.url", filepath.Join(base, "none"))
				gitRun(t, repo, "config", "remote.origin.uploadpack", script)
			case "sshCommand":
				gitRun(t, repo, "config", "remote.origin.url", "ssh://example.invalid/x.git")
				gitRun(t, repo, "config", "core.sshCommand", script)
			case "ext":
				gitRun(t, repo, "config", "protocol.ext.allow", "always")
				gitRun(t, repo, "config", "remote.origin.url", "ext::"+script)
			}
			app, dir := mcpApp(t, repo, "stack")
			git, why := app.openGitTree(context.Background(), dir)
			if why != "" {
				t.Fatal(why)
			}
			defer git.close()
			wt := app.findWorkTree(context.Background(), reposRemote(t, repo), dir)
			if wt.source != nil {
				t.Error("a commit plan of a commit whose tree is missing")
			}
			noMarker(t, marker, "findWorkTree")
		})
	}
}

// A process git starts can hold git's stdout open after git exits or is
// killed. git.output returns soon after git ends, not when that process
// does.
func TestGitReturnsWhileAChildHoldsStdout(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	write(t, filepath.Join(dir, "git"), "#!/bin/sh\nsleep 30 &\necho $! > "+pidFile+"\necho out\nexec sleep 30\n")
	if err := os.Chmod(filepath.Join(dir, "git"), 0o755); err != nil {
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
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	_, err := gitOutput(ctx, t.TempDir(), "rev-parse", "--show-toplevel")
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("git took %s", took)
	}
	if err == nil {
		t.Fatal("a git that was killed succeeded")
	}
}
