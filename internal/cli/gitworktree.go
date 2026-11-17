package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/mr-karan/writ/internal/confine"
	"github.com/mr-karan/writ/internal/forge"
	"github.com/mr-karan/writ/internal/repo"
	"github.com/mr-karan/writ/internal/server"
)

// gitOutput runs git in dir and returns its output, trimmed. With no git
// on PATH it returns forge.ErrNoGit.
func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	return gitTree{top: dir}.output(ctx, args...)
}

// gitTree is a Git work tree that git runs in.
type gitTree struct {
	// top is the work tree's path, spelled as the stack directory is.
	top string
	// tree and gitDir, when set, hold the work tree and its .git directory
	// open, from a walk through a confined root. git then runs on the open
	// directories through /proc/self/fd, and never looks up top, so a
	// directory swapped for a link after the walk does not move it.
	tree, gitDir *os.File
}

// gitOutputMax bounds what one git command may print. ls-tree of a
// repository's path is the longest output.
const gitOutputMax = 64 << 20

// gitTimeoutTree bounds each git command on a work tree.
const gitTimeoutTree = 10 * time.Second

// output runs git in the work tree and returns its output, trimmed. With
// no git on PATH it returns forge.ErrNoGit. Beneath a directory, git runs
// hardened, as forge.GitCmd runs it.
func (g gitTree) output(ctx context.Context, args ...string) (string, error) {
	return g.outputEnv(ctx, nil, args...)
}

// outputEnv is output with env added to git's environment.
func (g gitTree) outputEnv(ctx context.Context, env []string, args ...string) (string, error) {
	c := forge.GitCmd{Dir: g.top, Max: gitOutputMax, Timeout: gitTimeoutTree, Inherit: g.tree == nil, Env: env}
	if g.tree != nil {
		// ExtraFiles become descriptors 3 and 4 in git. With GIT_DIR set,
		// git does no discovery, and the kernel resolves each /proc/self/fd
		// link to the directory open there, not to a path.
		c.Dir = "/proc/self/fd/3"
		c.ExtraFiles = []*os.File{g.tree, g.gitDir}
		c.Env = append([]string{"GIT_DIR=/proc/self/fd/4", "GIT_WORK_TREE=/proc/self/fd/3"}, env...)
	}
	out, err := c.Run(ctx, args...)
	if errors.Is(err, forge.ErrGitOutputTooLong) {
		return "", fmt.Errorf("git %s printed more than %d MiB", strings.Join(args, " "), gitOutputMax>>20)
	}
	return strings.TrimSpace(string(out)), err
}

// workTree is what writ plan --remote learns about the Git work tree that
// holds a stack: the repository of the control plane it is, the stack's
// name there, and either the commit to plan or why there is none.
type workTree struct {
	repo *server.RepoInfo
	// top is the work tree's directory; root is the repository's path in
	// it; name is the stack's name on the control plane.
	top, root, name string
	source          *server.SourceRequest
	// why says why no commit can be planned, when source is nil.
	why string
}

// openGitTree finds the Git work tree that holds the stack directory dir,
// or says why there is none. Git finds it when Files reach any path.
//
// Beneath a directory, writ finds it instead, and git never looks up a
// path: Lstat through the root looks for .git in dir and each parent up to
// the root, and stops at an error. The work tree and its .git directory
// are then opened through the root, and git runs on those descriptors. A
// .git file, of a linked work tree or a submodule, names a Git directory
// by path, so it is refused, and so are commondir and objects/info/alternates
// in the Git directory, which name other Git directories by path. git
// reads the rest of the Git directory by path, such as a file an include
// in its config names, and that is not confined.
func (a *App) openGitTree(ctx context.Context, dir string) (gitTree, string) {
	files := a.opts.Files
	if files.Dir() == "" {
		top, err := gitOutput(ctx, dir, "rev-parse", "--show-toplevel")
		if errors.Is(err, forge.ErrNoGit) {
			return gitTree{}, err.Error()
		}
		if err != nil {
			return gitTree{}, dir + " is not in a Git work tree"
		}
		return gitTree{top: canonical(top)}, ""
	}
	if runtime.GOOS != "linux" {
		return gitTree{}, "beneath --dir, writ runs git only on Linux, where it can run git on a directory it opened"
	}
	top := dir
	for {
		info, err := files.Lstat(filepath.Join(top, ".git"))
		if err == nil && !info.IsDir() {
			return gitTree{}, filepath.Join(top, ".git") + " is not a directory: a linked work tree or a submodule names its Git directory by path, which writ does not follow beneath --dir"
		}
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return gitTree{}, fmt.Sprintf("look for a Git work tree: %v", err)
		}
		if top == files.Dir() {
			return gitTree{}, dir + " is in no Git work tree beneath --dir"
		}
		top = filepath.Dir(top)
	}
	tree, err := files.OpenFile(top, os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return gitTree{}, fmt.Sprintf("open the Git work tree: %v", err)
	}
	gitDir, err := files.OpenFile(filepath.Join(top, ".git"), os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		tree.Close()
		return gitTree{}, fmt.Sprintf("open the Git directory: %v", err)
	}
	for _, name := range []string{"commondir", filepath.Join("objects", "info", "alternates")} {
		p := filepath.Join(top, ".git", name)
		_, err := files.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		tree.Close()
		gitDir.Close()
		if err != nil {
			return gitTree{}, fmt.Sprintf("look at %s: %v", p, err)
		}
		return gitTree{}, p + " names another Git directory by path, which writ does not follow beneath --dir"
	}
	return gitTree{top: top, tree: tree, gitDir: gitDir}, ""
}

// close releases the directories the work tree holds open.
func (g gitTree) close() {
	if g.tree != nil {
		g.tree.Close()
		g.gitDir.Close()
	}
}

// is reports whether the work tree is the directory projectID names: the
// same file as the open work tree, else the same canonical path.
func (g gitTree) is(projectID string) bool {
	if g.tree == nil {
		return canonical(projectID) == g.top
	}
	pi, err := os.Stat(projectID)
	ti, terr := g.tree.Stat()
	return err == nil && terr == nil && os.SameFile(pi, ti)
}

// findWorkTree looks at the Git work tree that holds the OpenTofu stack
// directory dir. The plan is of a commit when the work tree is a
// repository of the control plane, has no edit under the repository's
// path, and, for a forge, its HEAD is on a remote branch, so the forge has
// it. Beneath a directory, it asks git status with tofuStatus.
func (a *App) findWorkTree(ctx context.Context, r *remote, dir string) workTree {
	git, why := a.openGitTree(ctx, dir)
	if why != "" {
		return workTree{why: why}
	}
	defer git.close()
	top := git.top
	doc, err := r.get(ctx, "/v1/repos", remoteTimeout)
	var repos server.ReposDoc
	if err != nil || json.Unmarshal(doc, &repos) != nil {
		return workTree{why: "writ serve did not list its repositories"}
	}
	var remotes []string
	if out, err := git.output(ctx, "config", "--get-regexp", `^remote\..*\.url$`); err == nil {
		for line := range strings.SplitSeq(out, "\n") {
			if _, u, ok := strings.Cut(line, " "); ok {
				remotes = append(remotes, u)
			}
		}
	}
	var wt workTree
	for i := range repos.Repos {
		rp := &repos.Repos[i]
		root := filepath.Join(top, filepath.FromSlash(rp.Path))
		rel, err := filepath.Rel(root, dir)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			continue
		}
		if rp.Kind == "dir" && git.is(rp.ProjectID) || rp.Kind != "dir" && remoteIs(remotes, rp) {
			wt = workTree{repo: rp, top: top, root: root, name: rp.Prefix + filepath.ToSlash(rel)}
			break
		}
	}
	if wt.repo == nil {
		return workTree{why: "the work tree " + top + " is not a repository of this control plane; add it with writ repo add"}
	}
	head, err := git.output(ctx, "rev-parse", "HEAD")
	if err != nil {
		wt.why = "the work tree has no commit"
		return wt
	}
	if git.tree != nil {
		why = a.tofuStatus(ctx, git, wt, head, dir)
	} else {
		why = git.status(ctx, head, wt.repo.Path)
	}
	if why != "" {
		wt.why = why
		return wt
	}
	if wt.repo.Kind != "dir" {
		if on, _ := git.output(ctx, "branch", "-r", "--contains", head); on == "" {
			wt.why = "commit " + short(head) + " is on no remote branch; push it"
			return wt
		}
	}
	ref, _ := git.output(ctx, "symbolic-ref", "-q", "HEAD")
	wt.source = &server.SourceRequest{Repo: wt.repo.Name, Ref: ref, Commit: head}
	return wt
}

// status says how the work tree differs from commit head under the
// repository's path p, or "" when it does not.
func (g gitTree) status(ctx context.Context, head, p string) string {
	status, err := g.output(ctx, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--", dirOrDot(p))
	if err != nil {
		return err.Error()
	}
	if status == "" {
		return ""
	}
	// Each record is XY, a space, and the path; git trims the first
	// record's leading space with the output's.
	var files []string
	for rec := range strings.SplitSeq(status, "\x00") {
		if _, name, ok := strings.Cut(strings.TrimLeft(rec, " "), " "); ok {
			files = append(files, strings.TrimSpace(name))
		}
	}
	return differsWords(head, files)
}

// tofuStatus says how the work tree beneath a directory differs from
// commit head where a plan of the OpenTofu stack in dir reads, or "" when
// it does not. Such a plan sends no file: the control plane reads the
// stack's directory and the directory of each local module it calls, and
// all below them, from the commit. git status of those directories says
// whether the work tree holds the commit there, so any entry refuses the
// plan: an edit, a new file, a change of mode or of file type. The module
// walk reads the work tree's code. Where git status finds no entry, that
// code is the commit's, so the walk is the control plane's.
func (a *App) tofuStatus(ctx context.Context, git gitTree, wt workTree, head, dir string) string {
	rel, err := filepath.Rel(wt.root, dir)
	if err != nil {
		return err.Error()
	}
	var total int64
	dirs, err := repo.TofuModuleDirs(head, filepath.ToSlash(rel), func(d string) (map[string][]byte, error) {
		return readTofuCode(a.opts.Files, filepath.Join(wt.root, filepath.FromSlash(d)), &total)
	})
	if err != nil {
		return "writ cannot walk the local modules of the stack: " + err.Error()
	}
	blank, err := git.blankFilters(ctx)
	if err != nil {
		return err.Error()
	}
	var specs []string
	for _, d := range dirs {
		specs = append(specs, dirOrDot(path.Join(wt.repo.Path, d)))
	}
	// git status reports no edit to a file the index marks assume-unchanged
	// or skip-worktree, and an agent can set either mark.
	out, err := git.outputEnv(ctx, blank, append([]string{"--no-pager", "ls-files", "-v", "-z", "--"}, specs...)...)
	if err != nil {
		return err.Error()
	}
	var hidden []string
	for rec := range strings.SplitSeq(out, "\x00") {
		if tag, name, ok := strings.Cut(rec, " "); ok && len(tag) == 1 && (tag == "S" || unicode.IsLower(rune(tag[0]))) {
			hidden = append(hidden, name)
		}
	}
	if len(hidden) > 0 {
		return fmt.Sprintf("git status cannot see an edit to %s (%s), which the index marks assume-unchanged or skip-worktree; clear the marks with git update-index --no-assume-unchanged --no-skip-worktree", plural(len(hidden), "file"), firstThree(hidden))
	}
	out, err = git.outputEnv(ctx, blank, append([]string{"--no-pager", "status", "--porcelain=v2", "-z", "--untracked-files=all", "--ignored=no", "--ignore-submodules=dirty", "--no-renames", "--"}, specs...)...)
	if err != nil {
		return err.Error()
	}
	var files []string
	for rec := range strings.SplitSeq(out, "\x00") {
		if rec == "" {
			continue
		}
		// An ordinary change has eight fields before its path, an unmerged
		// one ten, and an untracked file one.
		fields := map[byte]int{'1': 8, 'u': 10, '?': 1}[rec[0]]
		if f := strings.SplitN(rec, " ", fields+1); fields > 0 && len(f) == fields+1 {
			rec = f[fields]
		}
		files = append(files, rec)
	}
	if len(files) == 0 {
		return ""
	}
	return differsWords(head, files) + "; commit it"
}

// readTofuCode returns the .tf and .tofu files directly in dir, by name,
// none when dir does not exist. Each file is bounded as the control plane
// bounds a source file, and total, all the files read, as it bounds a
// stack.
func readTofuCode(files confine.Files, dir string, total *int64) (map[string][]byte, error) {
	ents, err := files.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	code := map[string][]byte{}
	for _, e := range ents {
		if ext := filepath.Ext(e.Name()); !e.Type().IsRegular() || ext != ".tf" && ext != ".tofu" {
			continue
		}
		text, err := files.ReadFileMax(filepath.Join(dir, e.Name()), repo.DefaultLimits.File)
		if err != nil {
			return nil, err
		}
		if *total += int64(len(text)); *total > repo.DefaultLimits.StackBytes {
			return nil, fmt.Errorf("the code of the stack and its local modules is over %d bytes", repo.DefaultLimits.StackBytes)
		}
		code[e.Name()] = text
	}
	return code, nil
}

// blankFilters returns the environment that blanks each filter driver the
// repository's config names, as GIT_CONFIG_COUNT entries, which git takes
// over the repository's config. git status runs a driver's clean or
// process program on a file whose stat data changed, and an agent can
// write .git/config. An empty program is no program to git. A variable
// keeps a driver's name whole, where -c would split it at an =. A driver
// that is not required passes a file through when it has no program, and
// a required one fails, so each driver is also made not required.
func (g gitTree) blankFilters(ctx context.Context) ([]string, error) {
	out, err := g.output(ctx, "config", "--null", "--list")
	if err != nil {
		return nil, err
	}
	var keys, drivers []string
	for rec := range strings.SplitSeq(out, "\x00") {
		key, _, _ := strings.Cut(rec, "\n")
		dot := strings.LastIndexByte(key, '.')
		if !strings.HasPrefix(key, "filter.") || dot <= len("filter.") {
			continue
		}
		switch key[dot+1:] {
		case "clean", "smudge", "process", "required":
			if driver := key[:dot]; !slices.Contains(drivers, driver) {
				drivers = append(drivers, driver)
			}
		}
	}
	for _, driver := range drivers {
		keys = append(keys, driver+".clean", "", driver+".smudge", "", driver+".process", "", driver+".required", "false")
	}
	env := []string{fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(keys)/2)}
	for i := 0; i < len(keys); i += 2 {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i/2, keys[i]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i/2, keys[i+1]))
	}
	return env, nil
}

func differsWords(head string, files []string) string {
	return fmt.Sprintf("the work tree differs from commit %s in %s (%s)", short(head), plural(len(files), "file"), firstThree(files))
}

func firstThree(files []string) string {
	shown := strings.Join(files[:min(len(files), 3)], ", ")
	if len(files) > 3 {
		shown += ", ..."
	}
	return shown
}

func dirOrDot(p string) string {
	if p == "" {
		return "."
	}
	return p
}

// remoteIs reports whether one of a work tree's remote URLs is the
// repository's project at its forge: https://HOST/PROJECT(.git), or
// git@HOST:PROJECT(.git), or ssh://git@HOST/PROJECT(.git).
func remoteIs(remotes []string, rp *server.RepoInfo) bool {
	host := strings.ToLower(hostOf(rp.ForgeURL))
	if h, _, ok := strings.Cut(host, ":"); ok {
		host = h
	}
	for _, raw := range remotes {
		var h, p string
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			h, p = u.Hostname(), strings.TrimPrefix(u.Path, "/")
		} else if at, rest, ok := strings.Cut(raw, "@"); ok && !strings.Contains(at, "/") {
			h, p, _ = strings.Cut(rest, ":")
		}
		p = strings.TrimSuffix(strings.TrimSuffix(p, "/"), ".git")
		if strings.ToLower(h) == host && strings.EqualFold(p, rp.Project) {
			return true
		}
	}
	return false
}

// canonical is dir made absolute with its symbolic links resolved, or as
// near as can be when they do not resolve.
func canonical(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return filepath.Clean(dir)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}
