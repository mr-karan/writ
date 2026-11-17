package server

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/diag"
	"github.com/mr-karan/writ/internal/forge"
)

var projectRE = regexp.MustCompile(`^([A-Za-z0-9_][A-Za-z0-9_.-]*/)+[A-Za-z0-9_][A-Za-z0-9_.-]*$|^[1-9][0-9]{0,18}$`)

// checkRepoSpec checks a repository's fields as a request or the config
// names them.
func checkRepoSpec(spec RepoSpec, forges []Forge, dev bool) error {
	if !nameRE.MatchString(spec.Name) {
		return fmt.Errorf("repository name %q must be lowercase letters, digits, _ and -, starting with a letter", trunc(spec.Name))
	}
	if spec.Forge == string(control.Dir) {
		if !dev {
			return fmt.Errorf("forge dir names a local directory, which only writ serve --dev reads")
		}
		if !filepath.IsAbs(spec.Project) || filepath.Clean(spec.Project) != spec.Project {
			return fmt.Errorf("a dir repository's project is the absolute path of its directory, got %q", trunc(spec.Project))
		}
	} else {
		if _, err := findForge(forges, spec.Forge); err != nil {
			return err
		}
		if len(spec.Project) > 255 || !projectRE.MatchString(spec.Project) {
			return fmt.Errorf("project %q must be a project's path, such as acme/infra, or its numeric id", trunc(spec.Project))
		}
	}
	if err := forge.CheckDir(spec.Path); err != nil {
		return err
	}
	if spec.Prefix != "" {
		p, ok := strings.CutSuffix(spec.Prefix, "/")
		if !ok || CheckStackName(p) != nil {
			return fmt.Errorf("prefix %q must be a stack name and a slash, such as payments/", trunc(spec.Prefix))
		}
	}
	return nil
}

// findForge finds a forge by its name, or by the host of its URL when
// exactly one forge has that host.
func findForge(forges []Forge, ref string) (Forge, error) {
	var byHost []Forge
	for _, f := range forges {
		if f.Name == ref {
			return f, nil
		}
		if u, err := url.Parse(f.URL); err == nil && u.Host == ref {
			byHost = append(byHost, f)
		}
	}
	switch {
	case len(byHost) == 1:
		return byHost[0], nil
	case len(byHost) > 1:
		return Forge{}, fmt.Errorf("forges %s and %s both have the host %s; name one", byHost[0].Name, byHost[1].Name, ref)
	case len(forges) == 0:
		return Forge{}, fmt.Errorf("the config names no forge, so writ serve reads no repository at %q; add a forge block", trunc(ref))
	}
	names := make([]string, len(forges))
	for i, f := range forges {
		names[i] = f.Name + " (" + f.URL + ")"
	}
	return Forge{}, fmt.Errorf("no forge is named %q or has it as its host; the config has %s", trunc(ref), strings.Join(names, ", "))
}

// ParseDevConfig reads the HCL config writ serve --dev --config takes:
// forge and repo blocks, and poll_every, which add to the dev identity,
// and a tofu and a policy block. The rest of a team's config does not
// apply under --dev yet.
func ParseDevConfig(file string, src []byte) (*Config, diag.List) {
	b, diags := parseHCL(file, src)
	if diags.HasErrors() {
		return nil, diags
	}
	d := &decoder{dir: filepath.Dir(file), src: src, diags: diags}
	cfg := &Config{PollEvery: DefaultPollEvery}
	top, many := d.body(b, "the dev config", shape{attrs: []string{"poll_every"}, blocks: []string{"tofu", "policy"}, lists: []string{"repo"}, named: []string{"forge"}})
	d.repoParts(top, many, cfg, true)
	if f, ok := top["tofu"]; ok {
		cfg.Tofu = d.tofu(f.block())
	}
	if f, ok := top["policy"]; ok {
		cfg.Policies = d.policies(f.block())
	}
	if d.diags.HasErrors() {
		return nil, d.diags
	}
	cfg.Text = string(src)
	return cfg, d.diags
}
