package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mr-karan/writ/internal/server"
)

// The start banner of writ serve --dev says what PATH a run gets. With
// tofu.git set, the runner puts git's directory first, so the banner must
// too.
func TestDevRunPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	for _, tc := range []struct {
		name, git, want string
	}{
		{"no git", "", "/usr/local/bin:/usr/bin:/bin"},
		{"git by path", "/opt/git/bin/git", "/opt/git/bin:/usr/local/bin:/usr/bin:/bin"},
		{"git by name on PATH", "git", dir + ":/usr/local/bin:/usr/bin:/bin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := devRunPath(&server.Tofu{Binary: "tofu", Git: tc.git}); got != tc.want {
				t.Fatalf("devRunPath(git %q) = %q, want %q", tc.git, got, tc.want)
			}
		})
	}
}
