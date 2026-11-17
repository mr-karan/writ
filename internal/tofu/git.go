package tofu

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// gitHelpers are the programs of git's exec path that tofu init's clone
// over https runs: git, which git starts again for fetch-pack, index-pack,
// and its other builtins, and the https transport.
var gitHelpers = []string{"git", "git-remote-http", "git-remote-https"}

// gitTools is what of git a run's view holds.
type gitTools struct {
	// bin is git as tofu finds it on PATH.
	bin string
	// files are bin and the helpers, each by the path the view holds it
	// at.
	files []string
}

// CheckGit checks that binary is git and that its exec path holds what a
// clone over https runs.
func CheckGit(ctx context.Context, binary string) error {
	_, err := findGit(ctx, binary)
	return err
}

func findGit(ctx context.Context, binary string) (*gitTools, error) {
	bin, err := filepath.Abs(binary)
	if err != nil {
		return nil, fmt.Errorf("tofu: git: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, _, err := command{binary: bin, args: []string{"--exec-path"}, max: 4 << 10}.run(ctx)
	if err != nil {
		return nil, fmt.Errorf("tofu: run %s --exec-path: %w", bin, err)
	}
	dir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("tofu: %s --exec-path printed no full path", bin)
	}
	g := &gitTools{bin: bin, files: []string{bin}}
	for _, h := range gitHelpers {
		p := filepath.Join(dir, h)
		if _, err := os.Stat(p); err != nil {
			return nil, fmt.Errorf("tofu: git %s: its exec path has no %s, which a clone over https runs: %w", bin, h, err)
		}
		g.files = append(g.files, p)
	}
	return g, nil
}

// path is the PATH of a step: git's own directory, then the stack's PATH.
func (g *gitTools) path(stackEnv []string) string {
	path := filepath.Dir(g.bin)
	for _, kv := range stackEnv {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok && v != "" {
			path = filepath.Dir(g.bin) + ":" + v
		}
	}
	return "PATH=" + path
}

// gitEnv keeps git, which tofu init runs for a module source that needs
// it, to https and ssh, which GIT_ALLOW_PROTOCOL enforces over any
// protocol.allow setting, and from the host's and the user's config. The
// alias stands in for git-submodule, a shell script that the view leaves
// out: tofu runs `git submodule update --init --recursive` after every
// clone, and git reads an alias only when it finds no such program.
var gitEnv = []string{
	"GIT_CONFIG_NOSYSTEM=1",
	"GIT_CONFIG_GLOBAL=" + os.DevNull,
	"GIT_ALLOW_PROTOCOL=https:ssh",
	"GIT_TERMINAL_PROMPT=0",
	"GIT_CONFIG_PARAMETERS='alias.submodule'='submodule--helper'",
}

// needsGit reports whether a failed init's stderr says that tofu found no
// git, in go-getter's words, which tofu wraps across lines.
func needsGit(stderr []byte) bool {
	return strings.Contains(strings.Join(strings.Fields(string(stderr)), " "), "git must be available and on the PATH")
}
