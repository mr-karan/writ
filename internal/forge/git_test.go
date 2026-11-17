package forge_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/mr-karan/writ/internal/forge"
)

// Git before 2.44 ignores GIT_NO_LAZY_FETCH and fetches a missing object.
// With the variable turned off, as such a git behaves, git tries the fetch,
// and the empty GIT_ALLOW_PROTOCOL refuses its transport before it runs a
// program, over the repository's protocol.ext.allow.
func TestGitCmdAllowsNoTransport(t *testing.T) {
	for _, variant := range lazyFetchVariants {
		t.Run(variant, func(t *testing.T) {
			repo, _, marker := partialClone(t, variant)
			c := forge.GitCmd{Dir: repo, Max: 1 << 20, Env: []string{"GIT_NO_LAZY_FETCH=0"}}
			_, err := c.Run(context.Background(), "cat-file", "blob", "HEAD:stack/main.writ")
			if _, serr := os.Stat(marker); serr == nil {
				t.Fatal("git ran a program the repository's config names")
			}
			ge, ok := errors.AsType[*forge.GitError](err)
			if !ok || !strings.Contains(ge.Stderr, "not allowed") {
				t.Fatalf("git did not try the fetch and refuse its transport: %v", err)
			}
		})
	}
}

// The hardened command reads no config outside the repository and takes
// no GIT_ variable from writ's environment.
func TestGitCmdIgnoresInheritedGitVariables(t *testing.T) {
	repo, _, _ := partialClone(t, "uploadpack")
	t.Setenv("GIT_CONFIG_PARAMETERS", "'remote.leak.url'='https://env.example.com/x.git'")
	t.Setenv("GIT_ALLOW_PROTOCOL", "file:ssh:ext")
	out, err := forge.GitCmd{Dir: repo, Max: 1 << 20}.Run(context.Background(), "config", "--get-regexp", `^remote\.`)
	if err != nil || strings.Contains(string(out), "env.example.com") {
		t.Fatalf("config %q, %v", out, err)
	}
	inherited, err := forge.GitCmd{Dir: repo, Max: 1 << 20, Inherit: true}.Run(context.Background(), "config", "--get-regexp", `^remote\.`)
	if err != nil || !strings.Contains(string(inherited), "env.example.com") {
		t.Fatalf("with Inherit, git does not see the variable, so this test shows nothing: %q, %v", inherited, err)
	}
}

// Output over Max fails the command.
func TestGitCmdBoundsOutput(t *testing.T) {
	repo, _, _ := partialClone(t, "uploadpack")
	_, err := forge.GitCmd{Dir: repo, Max: 4}.Run(context.Background(), "rev-parse", "HEAD")
	if !errors.Is(err, forge.ErrGitOutputTooLong) {
		t.Fatalf("err %v, want ErrGitOutputTooLong", err)
	}
}
