package server

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/mr-karan/writ/internal/control"
)

// repoParts reads forge and repo blocks, poll_every, and
// poll_every_with_hook. Under --dev a repository may also be a local
// directory.
func (d *decoder) repoParts(top map[string]field, many map[string][]*hclsyntax.Block, cfg *Config, dev bool) {
	d.forges(many["forge"], cfg)
	if f, ok := top["poll_every"]; ok {
		cfg.PollEvery = d.duration(f.value, "poll_every")
		if cfg.PollEvery != 0 && cfg.PollEvery < time.Second {
			d.errorf(f.value, "poll_every must be at least 1s")
		}
	}
	cfg.PollEveryWithHook = max(DefaultPollEveryWithHook, cfg.PollEvery)
	if f, ok := top["poll_every_with_hook"]; ok {
		cfg.PollEveryWithHook = d.duration(f.value, "poll_every_with_hook")
		if cfg.PollEveryWithHook < cfg.PollEvery {
			d.errorf(f.value, "poll_every_with_hook must be at least poll_every, %s: a webhook only makes the poll rarer", cfg.PollEvery)
		}
	}
	if len(many["repo"]) == 0 {
		return
	}
	cfg.PinRepos = true
	seen := map[string]bool{}
	for i, re := range many["repo"] {
		r := d.attrs(re, fmt.Sprintf("repo %d", i+1), "name", "forge", "project", "path", "prefix", "webhook_secret_file")
		var spec RepoSpec
		for _, k := range []string{"name", "forge", "project"} {
			if _, ok := r[k]; !ok {
				d.errorf(re, "repo %d needs %s", i+1, k)
			}
		}
		get := func(k string) string {
			if x, ok := r[k]; ok {
				return d.str(x.value, "repo "+k)
			}
			return ""
		}
		spec.Name, spec.Forge, spec.Project, spec.Path, spec.Prefix = get("name"), get("forge"), get("project"), get("path"), get("prefix")
		if x, ok := r["webhook_secret_file"]; ok {
			spec.WebhookSecretFile = d.path(x.value, "webhook_secret_file")
			if spec.Forge == string(control.Dir) {
				d.errorf(x.key, "repo %s: a local directory has no webhook; webhook_secret_file is for a repository at a forge", spec.Name)
			}
		}
		if spec.Name != "" && seen[spec.Name] {
			d.errorf(r["name"].value, "repos names %s twice", spec.Name)
		}
		seen[spec.Name] = true
		if err := checkRepoSpec(spec, cfg.Forges, dev); err != nil && spec.Name != "" {
			d.errorf(re, "repo %s: %v", spec.Name, err)
		}
		cfg.Repos = append(cfg.Repos, spec)
	}
}

func (d *decoder) forges(bs []*hclsyntax.Block, cfg *Config) {
	for _, blk := range bs {
		name, key := blk.Labels[0], blk.LabelRanges[0]
		if !nameRE.MatchString(name) || name == string(control.Dir) {
			d.errorf(key, "forge name %q must be lowercase letters, digits, _ and -, and not dir", name)
		}
		r := d.attrs(blk, "forge "+name, "kind", "url", "read_token_file", "ca_file", "app_id", "key_file", "write_token_file")
		fg := Forge{Name: name}
		if k, ok := r["kind"]; ok {
			switch kind := d.str(k.value, "kind"); kind {
			case "gitlab":
				fg.Kind = control.GitLab
			case "github":
				d.errorf(k.value, "forge %s: kind github is not built yet; the GitHub App waits for stage 9 of the client-server design", name)
			default:
				d.errorf(k.value, "forge %s: kind %q is not gitlab", name, kind)
			}
		} else {
			d.errorf(blk, "forge %s needs kind", name)
		}
		if u, ok := r["url"]; ok {
			fg.URL = strings.TrimSuffix(d.str(u.value, "url"), "/")
			pu, err := url.Parse(fg.URL)
			ok := err == nil && pu.Host != "" && pu.User == nil && pu.RawQuery == "" && pu.Fragment == "" && (pu.Path == "" || pu.Path == "/") &&
				(pu.Scheme == "https" || pu.Scheme == "http" && (loopback(pu.Host) || loopback(pu.Host+":80")))
			if !ok {
				d.errorf(u.value, "forge %s: url must be https://HOST, or http:// on a loopback address, got %q", name, fg.URL)
			}
		} else {
			d.errorf(blk, "forge %s needs url", name)
		}
		if t, ok := r["read_token_file"]; ok {
			fg.ReadTokenFile = d.path(t.value, "read_token_file")
		} else {
			d.errorf(blk, "forge %s needs read_token_file, the file that holds a read-only token with read_api", name)
		}
		if c, ok := r["ca_file"]; ok {
			fg.CAFile = d.path(c.value, "ca_file")
		}
		if w, ok := r["write_token_file"]; ok {
			d.path(w.value, "write_token_file")
			d.warnf(w.key, "forge %s: write_token_file has no effect yet: merge request notes are not built", name)
		}
		for _, k := range []string{"app_id", "key_file"} {
			if x, ok := r[k]; ok {
				d.errorf(x.key, "forge %s: %s is for kind github, which is not built yet", name, k)
			}
		}
		cfg.Forges = append(cfg.Forges, fg)
	}
	slices.SortFunc(cfg.Forges, func(a, b Forge) int { return strings.Compare(a.Name, b.Name) })
}
