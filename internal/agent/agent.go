// Package agent holds what Writ puts into an OpenTofu repository for
// coding agents: the writ skill, and the section of AGENTS.md that points
// to it.
// It also holds the rule for writing that section into an AGENTS.md the
// user wrote, without changing a byte outside the section.
package agent

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
)

//go:embed skill
var skill embed.FS

// Where the files go, as slash paths from the repository root.
const (
	// AgentsFile is the file most coding agents read on start.
	AgentsFile = "AGENTS.md"
	// SkillDir holds the skill.
	SkillDir = ".agents/skills/writ"
	// ClaudeSkillDir is where Claude Code loads a project skill from. It
	// holds a copy of the skill, not a symbolic link to SkillDir, because
	// writ serve refuses a commit whose tree holds a link.
	ClaudeSkillDir = ".claude/skills/writ"
)

// The lines that open and close Writ's section of AGENTS.md. Everything
// between them, both included, belongs to writ agent init.
const (
	beginMarker = "<!-- BEGIN writ agent init: writ agent init rewrites this section -->"
	endMarker   = "<!-- END writ agent init -->"
)

const sectionBody = `## Writ

The infrastructure code here is OpenTofu, and Writ's control plane plans and applies it.

- Before you change a ` + "`.tf`" + ` file or ship an infrastructure change, load the ` + "`writ`" + ` skill, or read ` + "`.agents/skills/writ/SKILL.md`" + `.
- Commit a change, then plan the commit with the ` + "`plan`" + ` tool of the ` + "`writ`" + ` MCP server, or ` + "`writ plan --remote --json`" + ` in the stack directory. Tell the user the plan's id.
- Do not run ` + "`tofu`" + ` or ` + "`terraform`" + ` yourself, and never apply or approve a plan: a person does, and a plan's author cannot approve it.
`

// File is one file of the skill.
type File struct {
	// Path is the slash path from the repository root.
	Path string
	Text []byte
}

// SkillFiles returns the files of the skill as they go in dir, in lexical
// order of path.
func SkillFiles(dir string) []File {
	var files []File
	err := fs.WalkDir(skill, "skill", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		text, err := skill.ReadFile(p)
		files = append(files, File{Path: path.Join(dir, path.Base(p)), Text: text})
		return err
	})
	if err != nil {
		panic(fmt.Sprintf("read the embedded skill: %v", err))
	}
	return files
}

// Section returns Writ's section of AGENTS.md, markers included, ending in
// a newline.
func Section() []byte {
	return []byte(beginMarker + "\n" + sectionBody + endMarker + "\n")
}

// ErrBrokenSection means AGENTS.md has Writ's markers, but not as one
// opening line followed by one closing line. Merge does not guess which
// text is the user's.
var ErrBrokenSection = errors.New("AGENTS.md has a writ agent init marker without its pair, or more than one section")

// Change says what Merge did to AGENTS.md.
type Change int

const (
	// Unchanged: the file already held this section.
	Unchanged Change = iota
	// Appended: the file had no section, and got one at its end.
	Appended
	// Replaced: the file's section was rewritten in place.
	Replaced
)

// Merge returns the AGENTS.md that holds Writ's section: the section alone
// when existing is empty, existing with the section appended when it has
// none, and existing with its section replaced when it has one. Text
// outside the section keeps every byte, whitespace included. Merging its
// own result returns it unchanged.
func Merge(existing []byte) ([]byte, Change, error) {
	if len(existing) == 0 {
		return Section(), Appended, nil
	}
	begin, end, found, err := findSection(existing)
	if err != nil {
		return nil, Unchanged, err
	}
	if !found {
		out := slices.Clone(existing)
		switch {
		case bytes.HasSuffix(out, []byte("\n\n")):
		case bytes.HasSuffix(out, []byte("\n")):
			out = append(out, '\n')
		default:
			out = append(out, '\n', '\n')
		}
		return append(out, Section()...), Appended, nil
	}
	if bytes.Equal(existing[begin:end], Section()) {
		return existing, Unchanged, nil
	}
	out := slices.Clone(existing[:begin])
	out = append(out, Section()...)
	return append(out, existing[end:]...), Replaced, nil
}

// findSection returns the byte range of Writ's section: from the start of
// the opening line to past the newline that ends the closing line, or the
// end of the file.
func findSection(text []byte) (begin, end int, found bool, err error) {
	var opens, closes []int
	for at := 0; at < len(text); {
		line := text[at:]
		next := len(text)
		if i := bytes.IndexByte(line, '\n'); i >= 0 {
			line, next = line[:i], at+i+1
		}
		switch string(bytes.TrimRight(line, " \t\r")) {
		case beginMarker:
			opens = append(opens, at)
		case endMarker:
			closes = append(closes, next)
		}
		at = next
	}
	switch {
	case len(opens) == 0 && len(closes) == 0:
		return 0, 0, false, nil
	case len(opens) != 1 || len(closes) != 1 || closes[0] <= opens[0]:
		return 0, 0, false, ErrBrokenSection
	}
	return opens[0], closes[0], true, nil
}
