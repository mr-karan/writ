package tofu

import (
	"bytes"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

const (
	terraformRegistry = "registry.terraform.io"
	opentofuRegistry  = "registry.opentofu.org"
)

// Moved is a provider of a lock file that Terraform wrote, which
// FromTerraform moved to OpenTofu's registry.
type Moved struct {
	// From is the address the lock file names, such as
	// registry.terraform.io/hashicorp/random, and To the one the run
	// uses, such as registry.opentofu.org/hashicorp/random.
	From, To string
	// Rehashed means the lock file's hashes were dropped, because
	// OpenTofu's registry serves its own builds of the hashicorp/
	// providers, and Fill records the hashes of those builds.
	Rehashed bool
}

// FromTerraform makes a lock file that Terraform wrote fit OpenTofu's
// tofu init -lockfile=readonly, which refuses every provider of
// registry.terraform.io/hashicorp/ and every one the code does not name by
// that address. Each provider of registry.terraform.io whose full address
// named reports false, as for a source with no host, such as
// "hashicorp/random", moves to registry.opentofu.org, where OpenTofu
// resolves such a source, at the same version. One of another namespace
// keeps its hashes:
// OpenTofu's registry serves the same release archives for it, and tofu
// refuses an archive that matches none. One of hashicorp/ loses them:
// OpenTofu's registry serves its own builds of those, signed by OpenTofu,
// and Fill records their hashes. A lock file with nothing to move is
// returned as it is. The code naming registry.terraform.io/hashicorp/ is
// an error, which no lock file can fix.
func FromTerraform(lock []byte, named func(source string) bool) ([]byte, []Moved, error) {
	providers, err := LockedProviders(lock)
	if err != nil {
		return nil, nil, err
	}
	pinned := map[string]bool{}
	for _, p := range providers {
		pinned[p.Source] = true
	}
	var out []Provider
	var moved []Moved
	for _, p := range providers {
		rest, ok := strings.CutPrefix(p.Source, terraformRegistry+"/")
		if !ok {
			out = append(out, p)
			continue
		}
		hashicorp := strings.HasPrefix(rest, "hashicorp/")
		if named(p.Source) {
			if hashicorp {
				return nil, nil, fmt.Errorf("the code names provider %s by Terraform's registry, which OpenTofu's tofu init refuses with the lock file read-only: name it %s, which OpenTofu installs from its own registry", p.Source, rest)
			}
			out = append(out, p)
			continue
		}
		m := Moved{From: p.Source, To: opentofuRegistry + "/" + rest}
		moved = append(moved, m)
		if pinned[m.To] {
			// The lock file pins the provider in OpenTofu's registry too,
			// so the Terraform entry is one that tofu would drop.
			continue
		}
		p.Source = m.To
		if hashicorp {
			p.Hashes = nil
			moved[len(moved)-1].Rehashed = true
		}
		out = append(out, p)
	}
	if len(moved) == 0 {
		return lock, nil, nil
	}
	slices.SortFunc(out, func(a, b Provider) int { return strings.Compare(a.Source, b.Source) })
	text, err := formatLock(out)
	return text, moved, err
}

var hashRE = regexp.MustCompile(`^[a-z0-9]+:[A-Za-z0-9+/=]+$`)

// formatLock writes a lock file that pins providers. It leaves out each
// provider's version constraints, which tofu reads from the code and
// does not compare with the lock file's.
func formatLock(providers []Provider) ([]byte, error) {
	var b bytes.Buffer
	for _, p := range providers {
		// LockedProviders checked the source and the version, so only a
		// hash could break out of its string.
		fmt.Fprintf(&b, "provider %q {\n  version = %q\n", p.Source, p.Version)
		if len(p.Hashes) > 0 {
			b.WriteString("  hashes = [\n")
			for _, h := range p.Hashes {
				if !hashRE.MatchString(h) {
					return nil, fmt.Errorf("%s: provider %s has a hash that is not a scheme and a base64 or hex digest, such as \"zh:<hex>\"", LockFile, p.Source)
				}
				fmt.Fprintf(&b, "    %q,\n", h)
			}
			b.WriteString("  ]\n")
		}
		b.WriteString("}\n\n")
	}
	return b.Bytes(), nil
}
