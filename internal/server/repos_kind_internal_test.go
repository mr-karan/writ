package server

import (
	"strings"
	"testing"

	"github.com/mr-karan/writ/internal/control"
)

// TestForgeKindChange checks that a repository read from a GitLab forge
// is not taken for one of a GitHub forge that reuses the forge's name and
// URL: its numeric id is GitLab's, so the config's pinned repository no
// longer matches it, and no client reads it.
func TestForgeKindChange(t *testing.T) {
	const url = "https://git.example.com"
	github := Forge{Name: "main", Kind: control.GitHub, URL: url}
	s := &Server{cfg: &Config{Forges: []Forge{github}}, forges: map[string]forgeClient{"main": {cfg: github}}}
	rp := control.Repo{Name: "infra", Forge: "main", Kind: control.GitLab, ForgeURL: url, Project: "acme/infra", ProjectID: "4242", Path: "infra"}
	spec := RepoSpec{Name: "infra", Forge: "main", Project: "acme/infra", Path: "infra"}
	if s.sameSpec(rp, spec) {
		t.Fatal("a GitLab repository matches the spec of a GitHub forge of the same name and URL")
	}
	if _, err := s.clientOf(rp); err == nil || !strings.Contains(err.Error(), "forge main is now of kind github, and repository infra was added at a forge of kind gitlab") {
		t.Fatalf("the client of a GitLab repository at a GitHub forge: %v", err)
	}
	rp.Kind = control.GitHub
	if !s.sameSpec(rp, spec) {
		t.Fatal("a GitHub repository does not match its own spec")
	}
	if _, err := s.clientOf(rp); err != nil {
		t.Fatalf("the client of a GitHub repository: %v", err)
	}
}
