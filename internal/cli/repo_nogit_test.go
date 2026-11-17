package cli_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/mr-karan/writ/internal/cli"
	"github.com/mr-karan/writ/internal/forge"
)

// TestRepoAddWithoutGit checks that writ repo add on a machine with no git
// binary says git is missing and how to install it, not that the directory
// is outside a Git work tree.
func TestRepoAddWithoutGit(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	dir := t.TempDir()
	var out bytes.Buffer
	app := cli.New(cli.Options{Dir: dir, Stdout: &out})
	err := app.RepoAdd(context.Background(), cli.RepoAddOptions{Location: dir})
	if !errors.Is(err, forge.ErrNoGit) {
		t.Fatalf("repo add without git: %v", err)
	}
}
