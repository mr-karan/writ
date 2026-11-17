package forge

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// Local reads a Git repository on this machine with the git binary, for
// writ serve --dev: a person's own work tree, or a bare repository. It
// reads commits, never the work tree, so uncommitted edits are not part of
// what it reads.
type Local struct{}

// ErrNoGit is the error of a git command when no git binary is on PATH.
// The release binary and install.sh bring no git, so it says how to get one.
var ErrNoGit = errors.New("git is not installed: writ reads local Git repositories with the git binary, and none is on PATH; install git, such as apt install git, dnf install git, brew install git, or nix profile install nixpkgs#gitMinimal")

// git runs git in dir, hardened as GitCmd runs it: the dev control plane
// reads a repository that an assistant in the project can write, so its
// config must not make git start a program.
func (Local) git(ctx context.Context, dir string, max int64, args ...string) ([]byte, error) {
	out, err := GitCmd{Dir: dir, Max: int(max)}.Run(ctx, args...)
	if ge, ok := errors.AsType[*GitError](err); ok {
		msg := ge.Stderr
		if i := strings.IndexByte(msg, '\n'); i > 0 {
			msg = msg[:i]
		}
		if msg == "" {
			msg = ge.Err.Error()
		}
		return nil, fmt.Errorf("git %s in %s: %s: %w", args[0], dir, trunc(msg), ErrNotFound)
	}
	if errors.Is(err, ErrGitOutputTooLong) {
		return nil, &TooLargeError{What: "git " + args[0] + ": the output", Max: max}
	}
	return out, err
}

func (l Local) line(ctx context.Context, dir string, args ...string) (string, error) {
	b, err := l.git(ctx, dir, 64<<10, args...)
	return strings.TrimSpace(string(b)), err
}

// Project finds the Git repository that holds the absolute directory ref.
// Its id and path are the work tree's absolute path, or the repository's
// own for a bare one, and its default branch is the branch HEAD names.
func (l Local) Project(ctx context.Context, ref string) (Project, error) {
	if !filepath.IsAbs(ref) {
		return Project{}, fmt.Errorf("directory %q is not absolute", trunc(ref))
	}
	bare, err := l.line(ctx, ref, "rev-parse", "--is-bare-repository")
	if errors.Is(err, ErrNoGit) {
		return Project{}, err
	}
	if err != nil {
		return Project{}, fmt.Errorf("%s is not in a Git repository: %w", ref, err)
	}
	what := "--show-toplevel"
	if bare == "true" {
		what = "--absolute-git-dir"
	}
	top, err := l.line(ctx, ref, "rev-parse", what)
	if err != nil {
		return Project{}, err
	}
	if resolved, err := filepath.EvalSymlinks(top); err == nil {
		top = resolved
	}
	branch, err := l.line(ctx, top, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return Project{}, fmt.Errorf("%s has no branch checked out: %w", top, err)
	}
	return Project{ID: top, Path: top, DefaultBranch: branch}, nil
}

// Head reads the commit a branch points to.
func (l Local) Head(ctx context.Context, projectID, branch string) (string, error) {
	return l.verify(ctx, projectID, "refs/heads/"+branch)
}

// Commit checks that a commit exists and returns its full id.
func (l Local) Commit(ctx context.Context, projectID, sha string) (string, error) {
	if !IsSHA(sha) {
		return "", fmt.Errorf("commit %q is not a full SHA-1", trunc(sha))
	}
	return l.verify(ctx, projectID, sha)
}

// OnRef reports whether sha is the commit ref names or an ancestor of it.
func (l Local) OnRef(ctx context.Context, projectID, sha, ref string) (bool, error) {
	if !IsSHA(sha) {
		return false, fmt.Errorf("commit %q is not a full SHA-1", trunc(sha))
	}
	head, err := l.verify(ctx, projectID, ref)
	if err != nil {
		return false, err
	}
	// Two commits with no common history have no merge base, and git
	// fails: the commit is not on the ref.
	base, err := l.line(ctx, projectID, "merge-base", "--end-of-options", sha, head)
	return err == nil && base == sha, nil
}

func (l Local) verify(ctx context.Context, dir, rev string) (string, error) {
	sha, err := l.line(ctx, dir, "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{commit}")
	if errors.Is(err, ErrNoGit) {
		return "", err
	}
	if err != nil {
		return "", fmt.Errorf("read %s: no such commit: %w", rev, ErrNotFound)
	}
	if !IsSHA(sha) {
		return "", fmt.Errorf("read %s: git named no commit", rev)
	}
	return sha, nil
}

// Tree lists a commit's files under dir with git ls-tree.
func (l Local) Tree(ctx context.Context, projectID, sha, dir string) ([]File, error) {
	if !IsSHA(sha) {
		return nil, fmt.Errorf("commit %q is not a full SHA-1", trunc(sha))
	}
	args := []string{"ls-tree", "-r", "-z", "--full-tree", sha}
	if dir != "" {
		args = append(args, "--", dir)
	}
	out, err := l.git(ctx, projectID, 64<<20, args...)
	if err != nil {
		return nil, err
	}
	what := "read the tree of " + Short(sha)
	var files []File
	for rec := range strings.SplitSeq(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		if rec == "" {
			continue
		}
		meta, name, ok := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 3 {
			return nil, fmt.Errorf("%s: git printed %q", what, trunc(rec))
		}
		mode, typ, id := f[0], f[1], f[2]
		if typ != "blob" {
			continue
		}
		rel, err := under(dir, name)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", what, err)
		}
		if mode == "120000" {
			return nil, fmt.Errorf("%s: %s is a symbolic link, which writ serve does not follow", what, name)
		}
		if len(files) == MaxEntries {
			return nil, fmt.Errorf("%s: it lists over %d entries", what, MaxEntries)
		}
		files = append(files, File{Path: rel, Blob: id})
	}
	return files, nil
}

// Blob reads a file's content by its blob id.
func (l Local) Blob(ctx context.Context, projectID, blob string, max int64) ([]byte, error) {
	if !IsSHA(blob) {
		return nil, fmt.Errorf("blob id %q is not a SHA-1", trunc(blob))
	}
	return l.git(ctx, projectID, max, "cat-file", "blob", blob)
}
