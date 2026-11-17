package cli

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/agent"
)

func TestAgentInit(t *testing.T) {
	section := string(agent.Section())
	skill := string(agent.SkillFiles(agent.SkillDir)[0].Text)
	tests := []struct {
		name  string
		setup func(t *testing.T, dir string)
		force bool
		// want maps paths to their actions. agentsMD is the AGENTS.md the
		// run must leave, and keep lists files it must not change.
		want     map[string]string
		agentsMD string
		keep     []string
		failed   bool
		// noClaude: .claude/skills/writ does not hold this writ's skill
		// after.
		noClaude bool
	}{
		{
			name:     "an empty repository",
			want:     map[string]string{agent.AgentsFile: agentCreated, agent.SkillDir + "/SKILL.md": agentCreated, agent.ClaudeSkillDir + "/SKILL.md": agentCreated},
			agentsMD: section,
		},
		{
			name: "an AGENTS.md the user wrote",
			setup: func(t *testing.T, dir string) {
				write(t, filepath.Join(dir, "AGENTS.md"), "# Rules\n\nUse tabs.\n")
			},
			want:     map[string]string{agent.AgentsFile: agentAppended},
			agentsMD: "# Rules\n\nUse tabs.\n\n" + section,
		},
		{
			name: "an AGENTS.md with an old section",
			setup: func(t *testing.T, dir string) {
				old := strings.Replace(section, "## Writ", "## Writ, version 0", 1)
				write(t, filepath.Join(dir, "AGENTS.md"), "# Rules\n"+old+"Tail.\n")
			},
			want:     map[string]string{agent.AgentsFile: agentUpdated},
			agentsMD: "# Rules\n" + section + "Tail.\n",
		},
		{
			name: "an AGENTS.md that links to another file",
			setup: func(t *testing.T, dir string) {
				write(t, filepath.Join(dir, "docs", "rules.md"), "# Rules\n")
				if err := os.Symlink(filepath.Join("docs", "rules.md"), filepath.Join(dir, "AGENTS.md")); err != nil {
					t.Fatal(err)
				}
			},
			want:     map[string]string{agent.AgentsFile: agentAppended},
			agentsMD: "# Rules\n\n" + section,
		},
		{
			name: "an AGENTS.md that links to another file by an absolute path in the repository",
			setup: func(t *testing.T, dir string) {
				write(t, filepath.Join(dir, "docs", "rules.md"), "# Rules\n")
				if err := os.Symlink(filepath.Join(dir, "docs", "rules.md"), filepath.Join(dir, "AGENTS.md")); err != nil {
					t.Fatal(err)
				}
			},
			want:     map[string]string{agent.AgentsFile: agentAppended},
			agentsMD: "# Rules\n\n" + section,
		},
		{
			name: "an AGENTS.md that links through another link and then ..",
			setup: func(t *testing.T, dir string) {
				write(t, filepath.Join(dir, "rules.md"), "# Top\n")
				write(t, filepath.Join(dir, "nested", "rules.md"), "# Rules\n")
				if err := os.MkdirAll(filepath.Join(dir, "nested", "deep"), 0o755); err != nil {
					t.Fatal(err)
				}
				symlink(t, filepath.Join("nested", "deep"), filepath.Join(dir, "alias"))
				symlink(t, "alias/../rules.md", filepath.Join(dir, "AGENTS.md"))
			},
			want:     map[string]string{agent.AgentsFile: agentAppended},
			agentsMD: "# Rules\n\n" + section,
			keep:     []string{"rules.md"},
		},
		{
			name: "an AGENTS.md of whitespace only keeps it",
			setup: func(t *testing.T, dir string) {
				write(t, filepath.Join(dir, "AGENTS.md"), " \n\t\n")
			},
			want:     map[string]string{agent.AgentsFile: agentAppended},
			agentsMD: " \n\t\n\n" + section,
		},
		{
			name: "a broken section",
			setup: func(t *testing.T, dir string) {
				write(t, filepath.Join(dir, "AGENTS.md"), "# Rules\n"+strings.SplitAfter(section, "\n")[0])
			},
			agentsMD: "# Rules\n" + strings.SplitAfter(section, "\n")[0],
			failed:   true,
			noClaude: true,
		},
		{
			name: "a skill file the user changed",
			setup: func(t *testing.T, dir string) {
				write(t, filepath.Join(dir, agent.SkillDir, "SKILL.md"), "my own\n")
			},
			want:     map[string]string{agent.SkillDir + "/SKILL.md": agentKept, agent.SkillDir + "/reference.md": agentCreated, agent.ClaudeSkillDir + "/SKILL.md": agentCreated},
			agentsMD: section,
			keep:     []string{agent.SkillDir + "/SKILL.md"},
		},
		{
			name: "a skill file the user changed, with --force",
			setup: func(t *testing.T, dir string) {
				write(t, filepath.Join(dir, agent.SkillDir, "SKILL.md"), "my own\n")
			},
			force:    true,
			want:     map[string]string{agent.SkillDir + "/SKILL.md": agentUpdated},
			agentsMD: section,
		},
		{
			name: "a Claude skill of the user's own named writ",
			setup: func(t *testing.T, dir string) {
				write(t, filepath.Join(dir, agent.ClaudeSkillDir, "SKILL.md"), "mine\n")
			},
			want:     map[string]string{agent.ClaudeSkillDir + "/SKILL.md": agentKept},
			agentsMD: section,
			keep:     []string{agent.ClaudeSkillDir + "/SKILL.md"},
			noClaude: true,
		},
		{
			name: ".claude links to a directory in the repository",
			setup: func(t *testing.T, dir string) {
				if err := os.MkdirAll(filepath.Join(dir, "stored", "claude"), 0o755); err != nil {
					t.Fatal(err)
				}
				symlink(t, filepath.Join("stored", "claude"), filepath.Join(dir, ".claude"))
			},
			want:     map[string]string{agent.ClaudeSkillDir + "/SKILL.md": agentCreated},
			agentsMD: section,
		},
		{
			name: ".claude links through another link and then ..",
			setup: func(t *testing.T, dir string) {
				for _, d := range []string{"claude", filepath.Join("nested", "deep"), filepath.Join("nested", "claude")} {
					if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
						t.Fatal(err)
					}
				}
				symlink(t, filepath.Join("nested", "deep"), filepath.Join(dir, "alias"))
				symlink(t, "alias/../claude", filepath.Join(dir, ".claude"))
			},
			want:     map[string]string{agent.ClaudeSkillDir + "/SKILL.md": agentCreated},
			agentsMD: section,
		},
		{
			name: "the skill of an older writ agent init",
			setup: func(t *testing.T, dir string) {
				write(t, filepath.Join(dir, agent.SkillDir, "SKILL.md"), "the language skill\n")
				write(t, filepath.Join(dir, agent.SkillDir, "spec.md"), "the language\n")
				symlink(t, "../../.agents/skills/writ", filepath.Join(dir, agent.ClaudeSkillDir))
			},
			force: true,
			want: map[string]string{
				agent.SkillDir + "/SKILL.md": agentUpdated, agent.SkillDir + "/spec.md": agentRemoved,
				agent.ClaudeSkillDir: agentRemoved, agent.ClaudeSkillDir + "/SKILL.md": agentCreated,
			},
			agentsMD: section,
		},
		{
			name: "a link of an older writ agent init through a linked .claude",
			setup: func(t *testing.T, dir string) {
				symlink(t, filepath.Join("stored", "claude"), filepath.Join(dir, ".claude"))
				symlink(t, "../../../.agents/skills/writ", filepath.Join(dir, "stored", "claude", "skills", "writ"))
			},
			want:     map[string]string{agent.ClaudeSkillDir: agentRemoved, agent.ClaudeSkillDir + "/SKILL.md": agentCreated},
			agentsMD: section,
		},
		{
			name: ".claude/skills links to .agents/skills",
			setup: func(t *testing.T, dir string) {
				if err := os.MkdirAll(filepath.Join(dir, ".agents", "skills"), 0o755); err != nil {
					t.Fatal(err)
				}
				symlink(t, filepath.Join("..", ".agents", "skills"), filepath.Join(dir, ".claude", "skills"))
			},
			want:     map[string]string{agent.ClaudeSkillDir + "/SKILL.md": agentUnchanged},
			agentsMD: section,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.setup != nil {
				tt.setup(t, dir)
			}
			before := map[string]string{}
			for _, p := range tt.keep {
				b, err := os.ReadFile(filepath.Join(dir, p))
				if err != nil {
					t.Fatal(err)
				}
				before[p] = string(b)
			}
			rep, err := agentInit(t, dir, tt.force)
			if tt.failed {
				if !errors.Is(err, ErrFailed) || rep.OK || len(rep.Diagnostics) != 1 || rep.Diagnostics[0].Class != "usage" {
					t.Fatalf("err %v, report %+v", err, rep)
				}
			} else if err != nil || !rep.OK {
				t.Fatalf("err %v, report %+v", err, rep)
			}
			got := map[string]string{}
			for _, f := range rep.Files {
				got[f.Path] = f.Action
			}
			for path, action := range tt.want {
				if got[path] != action {
					t.Errorf("%s: %q, want %q (all: %v)", path, got[path], action, got)
				}
			}
			if b, _ := os.ReadFile(filepath.Join(dir, "AGENTS.md")); string(b) != tt.agentsMD {
				t.Errorf("AGENTS.md\n%q\nwant\n%q", b, tt.agentsMD)
			}
			if strings.HasPrefix(tt.name, "an AGENTS.md that links") {
				if fi, err := os.Lstat(filepath.Join(dir, "AGENTS.md")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
					t.Errorf("AGENTS.md is no longer a link: %v", err)
				}
			}
			for p, text := range before {
				if b, _ := os.ReadFile(filepath.Join(dir, p)); string(b) != text {
					t.Errorf("%s changed to %q", p, b)
				}
			}
			if tt.failed {
				return
			}
			b, err := os.ReadFile(filepath.Join(dir, agent.ClaudeSkillDir, "SKILL.md"))
			if loads := err == nil && string(b) == skill; loads == tt.noClaude {
				t.Errorf("Claude Code loads this writ's skill from %s: %v, want %v (%v)", agent.ClaudeSkillDir, loads, !tt.noClaude, err)
			}
			if fi, err := os.Lstat(filepath.Join(dir, agent.ClaudeSkillDir)); err != nil || fi.Mode()&fs.ModeSymlink != 0 {
				t.Errorf("%s is a symbolic link or missing: %v", agent.ClaudeSkillDir, err)
			}
			if _, err := os.Lstat(filepath.Join(dir, agent.SkillDir, "spec.md")); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("%s/spec.md is left: %v", agent.SkillDir, err)
			}
			again, err := agentInit(t, dir, false)
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range again.Files {
				if f.Action != agentUnchanged && f.Action != agentKept {
					t.Errorf("a second run did %s to %s", f.Action, f.Path)
				}
			}
		})
	}
}

// writ agent init never writes through a symbolic link of the user's at a
// skill directory or below it. Without --force it refuses the link; with
// --force it replaces the link with this writ's skill. Either way, what the
// link leads to is not changed.
func TestAgentInitWritesThroughNoSkillLink(t *testing.T) {
	tests := []struct {
		name, link, target string
	}{
		{"a link of the user's own at .claude/skills/writ", agent.ClaudeSkillDir, "../../mine"},
		{"a link of the user's own at .agents/skills/writ", agent.SkillDir, "../../mine"},
		{"a skill file that links to a file in the repository", agent.SkillDir + "/reference.md", "../../../mine/reference.md"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, filepath.Join(dir, "mine", "SKILL.md"), "my skill\n")
			write(t, filepath.Join(dir, "mine", "reference.md"), "my reference\n")
			symlink(t, tt.target, filepath.Join(dir, filepath.FromSlash(tt.link)))
			mine := tree(t, filepath.Join(dir, "mine"))

			var out bytes.Buffer
			err := New(Options{Dir: dir, Stdout: &out}).AgentInit(context.Background(), AgentInitOptions{})
			if _, ok := errors.AsType[*UsageError](err); !ok || !strings.Contains(err.Error(), tt.link+" is a symbolic link") {
				t.Fatalf("err %v, want a usage error that names %s", err, tt.link)
			}
			if after := tree(t, filepath.Join(dir, "mine")); !maps.Equal(after, mine) {
				t.Fatalf("writ agent init wrote through the link:\n%v\nwant\n%v", after, mine)
			}
			if target, err := os.Readlink(filepath.Join(dir, filepath.FromSlash(tt.link))); err != nil || target != tt.target {
				t.Fatalf("the link now leads to %q, %v", target, err)
			}

			rep, err := agentInit(t, dir, true)
			if err != nil || !rep.OK || !slices.Contains(rep.Files, jsonAgentFile{Path: tt.link, Action: agentRemoved}) {
				t.Fatalf("err %v, report %+v", err, rep)
			}
			if after := tree(t, filepath.Join(dir, "mine")); !maps.Equal(after, mine) {
				t.Fatalf("writ agent init --force wrote through the link:\n%v\nwant\n%v", after, mine)
			}
			for _, skillDir := range []string{agent.SkillDir, agent.ClaudeSkillDir} {
				for _, f := range agent.SkillFiles(skillDir) {
					p := filepath.Join(dir, filepath.FromSlash(f.Path))
					fi, err := os.Lstat(p)
					if b, _ := os.ReadFile(p); err != nil || !fi.Mode().IsRegular() || !bytes.Equal(b, f.Text) {
						t.Errorf("%s is not this writ's skill file: %v", f.Path, err)
					}
				}
			}
		})
	}
}

// writ agent init writes only inside --dir. A symbolic link that leads
// outside, on AGENTS.md or on a directory it writes into, is refused, and
// nothing outside changes. A named pipe at AGENTS.md is refused without
// blocking on it.
func TestAgentInitStaysInTheRepository(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, dir, outside string)
		want  string
	}{
		{
			name: "AGENTS.md links outside",
			setup: func(t *testing.T, dir, outside string) {
				write(t, filepath.Join(outside, "rules"), "# Outside rules\n")
				symlink(t, filepath.Join("..", filepath.Base(outside), "rules"), filepath.Join(dir, "AGENTS.md"))
			},
			want: "AGENTS.md leads outside --dir through a symbolic link",
		},
		{
			name: "AGENTS.md links outside by an absolute path",
			setup: func(t *testing.T, dir, outside string) {
				write(t, filepath.Join(outside, "rules"), "# Outside rules\n")
				symlink(t, filepath.Join(outside, "rules"), filepath.Join(dir, "AGENTS.md"))
			},
			want: "AGENTS.md leads outside --dir through a symbolic link",
		},
		{
			name: "AGENTS.md links to a link that leads outside",
			setup: func(t *testing.T, dir, outside string) {
				write(t, filepath.Join(outside, "rules"), "# Outside rules\n")
				symlink(t, filepath.Join(outside, "rules"), filepath.Join(dir, "docs", "rules.md"))
				symlink(t, filepath.Join("docs", "rules.md"), filepath.Join(dir, "AGENTS.md"))
			},
			want: "AGENTS.md leads outside --dir through a symbolic link",
		},
		{
			name: ".agents links outside",
			setup: func(t *testing.T, dir, outside string) {
				symlink(t, outside, filepath.Join(dir, ".agents"))
			},
			want: agent.SkillDir + "/SKILL.md leads outside --dir through a symbolic link",
		},
		{
			name: ".claude links outside",
			setup: func(t *testing.T, dir, outside string) {
				symlink(t, outside, filepath.Join(dir, ".claude"))
			},
			want: agent.ClaudeSkillDir + " leads outside --dir through a symbolic link",
		},
		{
			name: ".claude links inside by an absolute path",
			setup: func(t *testing.T, dir, outside string) {
				if err := os.MkdirAll(filepath.Join(dir, "stored", "claude"), 0o755); err != nil {
					t.Fatal(err)
				}
				symlink(t, filepath.Join(dir, "stored", "claude"), filepath.Join(dir, ".claude"))
			},
			want: "is a symbolic link with an absolute target, which writ does not follow beneath --dir; replace it with a relative link",
		},
		{
			name: "AGENTS.md is a named pipe",
			setup: func(t *testing.T, dir, outside string) {
				if err := syscall.Mkfifo(filepath.Join(dir, "AGENTS.md"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: "AGENTS.md is not a regular file, so writ agent init does not read it",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := t.TempDir()
			dir, outside := filepath.Join(base, "repo"), filepath.Join(base, "outside")
			for _, d := range []string{dir, outside} {
				if err := os.Mkdir(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			tt.setup(t, dir, outside)
			before := tree(t, outside)
			done := make(chan error, 1)
			go func() {
				var out bytes.Buffer
				done <- New(Options{Dir: dir, Stdout: &out}).AgentInit(context.Background(), AgentInitOptions{})
			}()
			var err error
			select {
			case err = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("writ agent init blocked")
			}
			if _, ok := errors.AsType[*UsageError](err); !ok || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err %v, want a usage error with %q", err, tt.want)
			}
			if after := tree(t, outside); !maps.Equal(after, before) {
				t.Fatalf("the directory outside changed:\n%v\nwant\n%v", after, before)
			}
		})
	}
}

// tree maps each path under dir to its text, or to the target of a link.
func tree(t *testing.T, dir string) map[string]string {
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

func symlink(t *testing.T, target, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

func agentInit(t *testing.T, dir string, force bool) (agentInitReport, error) {
	t.Helper()
	var out bytes.Buffer
	err := New(Options{Dir: dir, Stdout: &out}).AgentInit(context.Background(), AgentInitOptions{JSON: true, Force: force})
	var rep agentInitReport
	if jerr := json.Unmarshal(out.Bytes(), &rep); jerr != nil {
		t.Fatalf("%v: %s", jerr, out.String())
	}
	return rep, err
}

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writ agent init writes where the kernel resolves each path, in layouts
// where a link is followed by .. and cleaning the path names another place.
// alias links to nested/deep, chain links to alias, and dest exists only as
// nested/dest, so alias/../dest is nested/dest.
func TestAgentInitAfterLinkThenDotDot(t *testing.T) {
	section := string(agent.Section())
	skill := string(agent.SkillFiles(agent.SkillDir)[0].Text)
	tests := []struct {
		name  string
		links map[string]string
		// sub is the --dir, spelled from the temporary directory.
		sub string
		// agentsMD and skillDir are where AGENTS.md and the skill must be,
		// spelled from the temporary directory.
		agentsMD, skillDir string
	}{
		{name: "AGENTS.md links through alias and then ..", links: map[string]string{"AGENTS.md": "alias/../dest/rules.md"}, agentsMD: "nested/dest/rules.md"},
		{name: "AGENTS.md links through ./alias// and then ..", links: map[string]string{"AGENTS.md": "./alias//../dest/rules.md"}, agentsMD: "nested/dest/rules.md"},
		{name: ".agents links through alias and then ..", links: map[string]string{".agents": "alias/../dest"}, skillDir: "nested/dest/skills/writ"},
		{name: ".agents links through alias/./..//", links: map[string]string{".agents": "alias/./..//dest"}, skillDir: "nested/dest/skills/writ"},
		{name: ".claude links through alias and then ..", links: map[string]string{".claude": "alias/../claude"}},
		{name: ".claude links through chain and then ..", links: map[string]string{".claude": "chain/./../claude"}},
		{name: ".claude links through alias and then ../..", links: map[string]string{".claude": "alias/../../claude"}},
		{name: ".claude links through nested/.. and alias/..", links: map[string]string{".claude": "./nested/../alias/../claude"}},
		{name: ".claude links to nested/deep/..", links: map[string]string{".claude": "nested/deep/../claude"}},
		{name: "--dir is alias/..", sub: "alias/..", agentsMD: "nested/AGENTS.md", skillDir: "nested/.agents/skills/writ"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := t.TempDir()
			for _, d := range []string{"nested/deep", "nested/dest", "nested/claude", "claude"} {
				if err := os.MkdirAll(filepath.Join(base, d), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			write(t, filepath.Join(base, "nested", "dest", "rules.md"), "# Rules\n")
			symlink(t, filepath.Join("nested", "deep"), filepath.Join(base, "alias"))
			symlink(t, "alias", filepath.Join(base, "chain"))
			for name, target := range tt.links {
				symlink(t, target, filepath.Join(base, name))
			}
			dir := base
			if tt.sub != "" {
				dir = base + "/" + tt.sub
			}
			agentsMD, skillDir := cmp.Or(tt.agentsMD, "AGENTS.md"), cmp.Or(tt.skillDir, ".agents/skills/writ")
			for run := range 2 {
				rep, err := agentInit(t, dir, false)
				if err != nil || !rep.OK {
					t.Fatalf("run %d: err %v, report %+v", run, err, rep)
				}
				for _, f := range rep.Files {
					if run == 1 && f.Action != agentUnchanged {
						t.Errorf("a second run did %s to %s", f.Action, f.Path)
					}
				}
			}
			if b, err := os.ReadFile(filepath.Join(base, agentsMD)); err != nil || !strings.HasSuffix(string(b), section) {
				t.Errorf("%s holds %q, %v", agentsMD, b, err)
			}
			if b, err := os.ReadFile(filepath.Join(base, skillDir, "SKILL.md")); err != nil || string(b) != skill {
				t.Errorf("%s/SKILL.md: %v", skillDir, err)
			}
			if _, err := os.Lstat(filepath.Join(base, "dest")); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("made a top-level dest: %v", err)
			}
			if b, err := os.ReadFile(dir + "/" + agent.ClaudeSkillDir + "/SKILL.md"); err != nil || string(b) != skill {
				t.Errorf("%s/SKILL.md: %v", agent.ClaudeSkillDir, err)
			}
		})
	}
}
