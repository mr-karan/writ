package tofu

import (
	"bytes"
	"encoding/json"
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
	// Kept means the code requires From as well, so the lock keeps it
	// and pins To beside it, at the same version and hashes.
	Kept bool
}

// FromTerraform makes a lock file that Terraform wrote fit OpenTofu's
// tofu init -lockfile=readonly, which refuses every provider of
// registry.terraform.io/hashicorp/ and needs one entry for each address
// the code requires. required reports whether the code names a full
// address in a source, after OpenTofu's resolution of a source with no
// host, such as "carlpett/sops", to registry.opentofu.org.
//
// Each provider of registry.terraform.io that the code does not require
// by that address moves to registry.opentofu.org at the same version: tofu
// resolves an unnamed provider, and one with no host, there. One the code
// requires by that address stays, and when the code also requires it in
// OpenTofu's registry, as a module that names it with no host does, the
// lock pins both. One of another namespace keeps its hashes: OpenTofu's
// registry serves the same release archives for it, and tofu refuses an
// archive that matches none. One of hashicorp/ loses them: OpenTofu's
// registry serves its own builds of those, signed by OpenTofu, and Fill
// records their hashes. A lock file with nothing to move is returned as it
// is. The code requiring registry.terraform.io/hashicorp/ is an error,
// which no lock file can fix.
func FromTerraform(lock []byte, required func(source string) bool) ([]byte, []Moved, error) {
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
		m := Moved{From: p.Source, To: opentofuRegistry + "/" + rest}
		if required(p.Source) {
			if hashicorp {
				return nil, nil, fmt.Errorf("the code names provider %s by Terraform's registry, which OpenTofu's tofu init refuses with the lock file read-only: name it %s, which OpenTofu installs from its own registry", p.Source, rest)
			}
			out = append(out, p)
			if !required(m.To) || pinned[m.To] {
				continue
			}
			m.Kept = true
			moved = append(moved, m)
			p.Source = m.To
			out = append(out, p)
			continue
		}
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

// stateProviderRE matches the provider address of a resource in a state
// file, as provider["registry.terraform.io/hashicorp/aws"] or
// module.m.provider["registry.terraform.io/hashicorp/aws"].alias.
var stateProviderRE = regexp.MustCompile(`provider\["` + regexp.QuoteMeta(terraformRegistry) + `/([^"]+)"\]`)

// StateForLock returns a state file whose resources name each provider by
// the address the lock file pins, so that tofu show, which loads the
// schema of every provider a state names, finds them installed. A state
// that Terraform wrote names its providers in registry.terraform.io, and
// FromTerraform moves those the code does not require there to
// registry.opentofu.org; a plan maps the addresses itself, show does not.
// A provider moves only when the lock pins it in OpenTofu's registry and
// not in Terraform's. Nothing but the resources' provider addresses
// changes, and a state with nothing to move is returned as it is.
func StateForLock(state, lock []byte) ([]byte, error) {
	if !bytes.Contains(state, []byte(terraformRegistry+"/")) {
		return state, nil
	}
	providers, err := LockedProviders(lock)
	if err != nil {
		return nil, err
	}
	pinned := map[string]bool{}
	for _, p := range providers {
		pinned[p.Source] = true
	}
	var st map[string]json.RawMessage
	if err := json.Unmarshal(state, &st); err != nil {
		return nil, fmt.Errorf("read the state: %w", err)
	}
	var resources []map[string]json.RawMessage
	if raw, ok := st["resources"]; ok {
		if err := json.Unmarshal(raw, &resources); err != nil {
			return nil, fmt.Errorf("read the state's resources: %w", err)
		}
	}
	moved := false
	for _, r := range resources {
		var addr string
		if err := json.Unmarshal(r["provider"], &addr); err != nil {
			continue
		}
		to := stateProviderRE.ReplaceAllStringFunc(addr, func(m string) string {
			rest := stateProviderRE.FindStringSubmatch(m)[1]
			if pinned[terraformRegistry+"/"+rest] || !pinned[opentofuRegistry+"/"+rest] {
				return m
			}
			return `provider["` + opentofuRegistry + "/" + rest + `"]`
		})
		if to == addr {
			continue
		}
		r["provider"], _ = json.Marshal(to)
		moved = true
	}
	if !moved {
		return state, nil
	}
	if st["resources"], err = json.Marshal(resources); err != nil {
		return nil, err
	}
	return json.Marshal(st)
}
