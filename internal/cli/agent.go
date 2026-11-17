package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/mr-karan/writ/internal/agent"
	"github.com/mr-karan/writ/internal/confine"
	"github.com/mr-karan/writ/internal/diag"
)

const agentInitFormat = "writ.agent-init/v0"

// What writ agent init did with each file.
const (
	agentCreated   = "created"   // the file was not there
	agentAppended  = "appended"  // AGENTS.md had no Writ section, and got one at its end
	agentUpdated   = "updated"   // Writ's section, or with --force a skill file, was rewritten
	agentUnchanged = "unchanged" // the file already held what this writ writes
	agentKept      = "kept"      // the file differs and is not Writ's to rewrite
	agentRemoved   = "removed"   // an older writ agent init wrote it, and this one does not
)

// AgentInitOptions are the flags of `writ agent init`.
type AgentInitOptions struct {
	JSON bool
	// Print writes nothing and prints what init would write.
	Print bool
	// Force rewrites skill files that differ from this writ's.
	Force bool
}

type agentInitReport struct {
	Format      string          `json:"format"`
	OK          bool            `json:"ok"`
	Files       []jsonAgentFile `json:"files"`
	Diagnostics []jsonDiag      `json:"diagnostics"`
}

type jsonAgentFile struct {
	Path   string `json:"path"`
	Action string `json:"action"`
}

// AgentInit puts the writ skill, in .agents/skills/writ and a copy in
// .claude/skills/writ, and a section of AGENTS.md that points to it into
// the repository at --dir. It writes regular files only, since writ serve
// refuses a commit whose tree holds a symbolic link, and replaces the link
// an older writ agent init made at .claude/skills/writ. It never writes
// through a symbolic link at a skill directory or below it: without --force
// such a link is refused, and with --force the link itself is removed and
// its target left as it is. It never changes AGENTS.md outside its own
// section, and never rewrites a skill file that differs without --force.
// Run twice, the second run changes nothing. Every file it reads or writes goes
// through an os.Root on --dir, so a symbolic link or .. that leads outside
// is refused, and so is a file that is not a regular file, such as a named
// pipe, which it never opens for reading.
func (a *App) AgentInit(ctx context.Context, o AgentInitOptions) (err error) {
	defer escapeError(&err, o.JSON)
	if o.Print {
		if o.JSON || o.Force {
			return usagef("--print writes nothing, so it takes no --json or --force")
		}
		return a.printAgentFiles()
	}
	rep := agentInitReport{Format: agentInitFormat, Files: []jsonAgentFile{}}
	var diags diag.List
	record := func(path, action string, err error) error {
		if err != nil {
			return err
		}
		rep.Files = append(rep.Files, jsonAgentFile{Path: path, Action: action})
		if action == agentKept {
			diags = append(diags, diag.Diagnostic{
				Severity: diag.Warning, Class: diag.Usage,
				Message: "kept " + path + ": it differs from what this writ writes", Fix: &diag.Fix{Message: "pass --force to replace it with this writ's"},
			})
		}
		return nil
	}
	files, err := confine.Open(a.opts.Dir)
	if err != nil {
		return err
	}
	defer files.Close()
	action, err := a.mergeAgentsFile(files)
	if errors.Is(err, agent.ErrBrokenSection) {
		diags.Errorf(diag.Usage, diag.Span{}, "%v; remove the markers, or fix them so one opening line comes before one closing line", err)
		rep.Diagnostics = toDiags(diags)
		return a.failed(jsonOnly(o.JSON, rep), diags)
	}
	if err := record(agent.AgentsFile, action, err); err != nil {
		return err
	}
	writeSkill := func(dir string) error {
		for _, f := range agent.SkillFiles(dir) {
			link, err := unlinkSkillPath(files, dir, f.Path, o.Force)
			if link != "" {
				rep.Files = append(rep.Files, jsonAgentFile{Path: link, Action: agentRemoved})
			}
			if err != nil {
				return err
			}
			action, err := a.writeSkillFile(files, f, o.Force)
			if err := record(f.Path, action, err); err != nil {
				return err
			}
		}
		return nil
	}
	removeStale := func(p string) error {
		removed, err := a.removeStale(files, p)
		if removed {
			rep.Files = append(rep.Files, jsonAgentFile{Path: p, Action: agentRemoved})
		}
		return err
	}
	if err := writeSkill(agent.SkillDir); err != nil {
		return err
	}
	if err := removeStale(path.Join(agent.SkillDir, "spec.md")); err != nil {
		return err
	}
	// The old link goes before the copy is written, which would otherwise
	// write through it.
	if err := removeStale(agent.ClaudeSkillDir); err != nil {
		return err
	}
	if err := writeSkill(agent.ClaudeSkillDir); err != nil {
		return err
	}
	rep.OK, rep.Diagnostics = true, toDiags(diags)
	if o.JSON {
		return writeJSON(a.opts.Stdout, rep)
	}
	out := a.humanOutput()
	for _, f := range rep.Files {
		fmt.Fprintf(out, "%s %s\n", f.Action, escapeHuman(f.Path, false))
	}
	printDiags(out, diags)
	return nil
}

// printAgentFiles prints Writ's section of AGENTS.md and each skill file,
// each after a line that names it.
func (a *App) printAgentFiles() error {
	out := a.opts.Stdout
	fmt.Fprintf(out, "==> %s (Writ's section) <==\n%s", agent.AgentsFile, agent.Section())
	for _, f := range agent.SkillFiles(agent.SkillDir) {
		fmt.Fprintf(out, "\n==> %s <==\n%s", f.Path, f.Text)
	}
	fmt.Fprintf(out, "\n==> %s <==\na copy of %s\n", agent.ClaudeSkillDir, agent.SkillDir)
	return nil
}

func (a *App) mergeAgentsFile(files confine.Files) (string, error) {
	path, err := writePath(files, filepath.Join(files.Dir(), agent.AgentsFile))
	if err != nil {
		return "", agentRefusal(agent.AgentsFile, err)
	}
	old, err := files.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return agentCreated, agentRefusal(agent.AgentsFile, files.WriteFileAtomic(path, agent.Section(), 0o644))
	}
	if err != nil {
		return "", agentRefusal(agent.AgentsFile, err)
	}
	merged, change, err := agent.Merge(old)
	switch {
	case err != nil:
		return "", err
	case change == agent.Unchanged:
		return agentUnchanged, nil
	case change == agent.Replaced:
		return agentUpdated, agentRefusal(agent.AgentsFile, files.WriteFileAtomic(path, merged, 0))
	}
	return agentAppended, agentRefusal(agent.AgentsFile, files.WriteFileAtomic(path, merged, 0))
}

func (a *App) writeSkillFile(files confine.Files, f agent.File, force bool) (string, error) {
	path, err := writePath(files, filepath.Join(files.Dir(), filepath.FromSlash(f.Path)))
	if err != nil {
		return "", agentRefusal(f.Path, err)
	}
	old, err := files.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := files.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return "", agentRefusal(f.Path, err)
		}
		return agentCreated, agentRefusal(f.Path, files.WriteFileAtomic(path, f.Text, 0o644))
	case err != nil:
		return "", agentRefusal(f.Path, err)
	case bytes.Equal(old, f.Text):
		return agentUnchanged, nil
	case !force:
		return agentKept, nil
	}
	return agentUpdated, agentRefusal(f.Path, files.WriteFileAtomic(path, f.Text, 0))
}

// unlinkSkillPath looks for a symbolic link at dir, a skill directory, and
// at each path from it down to file, which a write of file would follow.
// Without force a link is refused. With force the link is removed, and
// unlinkSkillPath returns its path; what it led to stays as it was.
func unlinkSkillPath(files confine.Files, dir, file string, force bool) (string, error) {
	paths := []string{dir}
	for name := range strings.SplitSeq(strings.TrimPrefix(file, dir+"/"), "/") {
		paths = append(paths, paths[len(paths)-1]+"/"+name)
	}
	for _, p := range paths {
		full := filepath.Join(files.Dir(), filepath.FromSlash(p))
		fi, err := files.Lstat(full)
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		if err != nil {
			return "", agentRefusal(file, err)
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			continue
		}
		if !force {
			return "", usagef("%s is a symbolic link, and writ agent init never writes the skill through one; remove the link, or pass --force to replace the link, not what it leads to, with this writ's skill", p)
		}
		if err := files.Remove(full); err != nil {
			return "", agentRefusal(p, err)
		}
		return p, nil
	}
	return "", nil
}

// maxLinks is how many symbolic links writePath follows, as the kernel
// bounds a lookup.
const maxLinks = 40

// writePath is the file a write to path changes: the target of a symbolic
// link, such as an AGENTS.md linked to a file elsewhere in the repository,
// so the write keeps the link. It is resolved, with no link or .. left, so
// a later filepath.Dir of it is the directory the kernel finds. A target
// outside the directory files reach is ErrOutside.
func writePath(files confine.Files, path string) (string, error) {
	for range maxLinks {
		fi, err := files.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) || err == nil && fi.Mode()&fs.ModeSymlink == 0 {
			return files.Resolve(path)
		}
		if err != nil {
			return "", err
		}
		target, err := files.Readlink(path)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(target) {
			// Joined as spelled: filepath.Join would clean a .. away
			// before the root follows the links ahead of it.
			target = path[:strings.LastIndexByte(path, filepath.Separator)+1] + target
		}
		path = target
	}
	return "", &fs.PathError{Op: "write", Path: path, Err: errors.New("too many levels of symbolic links")}
}

// agentRefusal turns a path that leads outside the repository, or a file
// that is not a regular file, into a usage error that names path, the
// file writ agent init means to write. Other errors pass through.
func agentRefusal(path string, err error) error {
	switch {
	case errors.Is(err, confine.ErrOutside):
		return usagef("%s leads outside --dir through a symbolic link: writ agent init writes only inside the repository; replace the link, or run writ agent init in the directory it leads to", path)
	case errors.Is(err, confine.ErrAbsoluteLink):
		return usagef("%s: %v", path, err)
	case errors.Is(err, confine.ErrNotRegular):
		return usagef("%s is not a regular file, so writ agent init does not read it: %v", path, err)
	}
	return err
}

// ownClaudeSkillLink matches each target an older writ agent init gave
// .claude/skills/writ: up to the repository, then down to the skill.
var ownClaudeSkillLink = regexp.MustCompile(`^(\.\./)+` + regexp.QuoteMeta(agent.SkillDir) + `$`)

// removeStale removes what an older writ agent init wrote at p, a slash
// path from --dir, and this one does not: spec.md, the language's
// reference, which only that version wrote, and the symbolic link it made
// at .claude/skills/writ. A link there that is not its own is the user's,
// and stays.
func (a *App) removeStale(files confine.Files, p string) (bool, error) {
	full := filepath.Join(files.Dir(), filepath.FromSlash(p))
	fi, err := files.Lstat(full)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, agentRefusal(p, err)
	case p == agent.ClaudeSkillDir && fi.Mode()&fs.ModeSymlink == 0:
		return false, nil
	case p == agent.ClaudeSkillDir:
		target, err := files.Readlink(full)
		if err != nil {
			return false, agentRefusal(p, err)
		}
		if !ownClaudeSkillLink.MatchString(filepath.ToSlash(target)) {
			return false, nil
		}
	case !fi.Mode().IsRegular():
		return false, nil
	}
	return true, agentRefusal(p, files.Remove(full))
}
