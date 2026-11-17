package tofu

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// LockFile is the dependency lock file that marks a stack's directory and
// pins its providers.
const LockFile = ".terraform.lock.hcl"

// Provider is one provider a lock file pins.
type Provider struct {
	// Source is the provider's full address, such as
	// registry.opentofu.org/hashicorp/random.
	Source  string
	Version string
	// Hashes are the lock's hashes of the provider's packages, such as
	// zh:<hex> for the SHA-256 of a release archive.
	Hashes []string
}

var (
	sourceRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*/[a-z0-9][a-z0-9_-]*/[a-z0-9][a-z0-9_-]*$`)
	versionRE = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)
)

// LockedProviders reads the providers a lock file pins, sorted by source.
// A source or version of a shape tofu does not write is an error, so both
// may be written into HCL unquoted by anything but double quotes.
func LockedProviders(lock []byte) ([]Provider, error) {
	f, diags := hclsyntax.ParseConfig(lock, LockFile, hcl.InitialPos)
	if diags.HasErrors() {
		return nil, fmt.Errorf("read %s: %s", LockFile, diags.Error())
	}
	var out []Provider
	for _, b := range f.Body.(*hclsyntax.Body).Blocks {
		if b.Type != "provider" {
			continue
		}
		if len(b.Labels) != 1 || !sourceRE.MatchString(b.Labels[0]) {
			return nil, fmt.Errorf("%s: a provider block needs one label, the provider's full address, such as \"registry.opentofu.org/hashicorp/random\"", LockFile)
		}
		attr, ok := b.Body.Attributes["version"]
		if !ok {
			return nil, fmt.Errorf("%s: provider %s has no version", LockFile, b.Labels[0])
		}
		v, ok := literalString(attr.Expr)
		if !ok || !versionRE.MatchString(v) {
			return nil, fmt.Errorf("%s: provider %s has a version that is not a version such as \"3.7.2\"", LockFile, b.Labels[0])
		}
		p := Provider{Source: b.Labels[0], Version: v}
		if attr, ok := b.Body.Attributes["hashes"]; ok {
			list, ok := attr.Expr.(*hclsyntax.TupleConsExpr)
			if !ok {
				return nil, fmt.Errorf("%s: provider %s has hashes that are not a list", LockFile, b.Labels[0])
			}
			for _, e := range list.Exprs {
				h, ok := literalString(e)
				if !ok {
					return nil, fmt.Errorf("%s: provider %s has a hash that is not a string", LockFile, b.Labels[0])
				}
				p.Hashes = append(p.Hashes, h)
			}
		}
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b Provider) int { return strings.Compare(a.Source, b.Source) })
	return out, nil
}

// literalString is the value of a quoted string with no interpolation.
func literalString(e hclsyntax.Expression) (string, bool) {
	t, ok := e.(*hclsyntax.TemplateExpr)
	if !ok || !t.IsStringLiteral() {
		return "", false
	}
	v, diags := t.Value(nil)
	if diags.HasErrors() || !v.IsKnown() || v.IsNull() {
		return "", false
	}
	return v.AsString(), true
}

// Fill installs the providers lock pins into the plugin cache, as writ's
// own user, so that a run as another user finds them there and only reads
// the cache. It runs `tofu init` on a configuration it writes itself, which
// requires exactly those providers, so no code of a stack runs as writ,
// and tofu checks each provider against the lock's hashes. Then it takes
// group and other write access from everything in the cache, and gives
// them read access, which writ's umask may have withheld. One Fill runs at
// a time, and no run's init overlaps it.
func (r *Runner) Fill(ctx context.Context, lock []byte) error {
	providers, err := LockedProviders(lock)
	if err != nil {
		return err
	}
	var main strings.Builder
	main.WriteString("terraform {\n  required_providers {\n")
	for i, p := range providers {
		fmt.Fprintf(&main, "    p%d = { source = %q, version = %q }\n", i, p.Source, p.Version)
	}
	main.WriteString("  }\n}\n")
	ctx, cancel := context.WithTimeout(ctx, r.opts.Timeout)
	defer cancel()
	r.cache.Lock()
	defer r.cache.Unlock()
	sc, err := r.makeScratch([]string{"."}, []file{
		{name: "main.tf", data: []byte(main.String()), mode: 0o600},
		{name: LockFile, data: lock, mode: 0o600},
	}, ".", nil, nil)
	if err != nil {
		return err
	}
	defer sc.remove()
	sc.fill = true
	if _, err := r.step(ctx, sc, false, "init", "init", "-input=false", "-lockfile=readonly", "-backend=false", "-no-color"); err != nil {
		return err
	}
	return shareCache(r.opts.PluginCache)
}

// shareCache makes every directory and file of the plugin cache readable
// by every user and writable only by its owner. It follows no symbolic
// link.
func shareCache(dir string) error {
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		perm := info.Mode().Perm()
		want := perm &^ 0o022
		switch {
		case info.IsDir():
			want |= 0o555
		case info.Mode().IsRegular():
			want |= 0o444
			if perm&0o100 != 0 {
				want |= 0o111
			}
		default:
			return nil
		}
		if want == perm {
			return nil
		}
		return os.Chmod(p, want)
	})
	if err != nil {
		return fmt.Errorf("tofu: share the plugin cache with the runs: %w", err)
	}
	return nil
}
