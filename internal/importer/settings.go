package importer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"

	"github.com/mr-karan/writ/internal/mask"
)

// providerOnly is the named list of settings that a provider keeps in state
// and the cloud does not return, so the first plan after an import sets
// them in place. Each entry was seen in that plan on the provider and
// version it names; a type or provider that was not measured has none.
//
// The six nomad_job settings: hashicorp/nomad 2.6.1 gives each a schema
// default and its importer sets none of them, so the plan sets each from
// null to its default whether or not the code names the default. Measured
// by TestImportHCL/nomad in e2e/import_hcl_test.go.
var providerOnly = []providerSettings{{
	Provider: "registry.opentofu.org/hashicorp/nomad",
	Version:  "2.6.1",
	Type:     "nomad_job",
	Attributes: []string{
		"deregister_on_destroy",
		"deregister_on_id_change",
		"detach",
		"preserve_counts",
		"read_allocation_ids",
		"rerun_if_dead",
	},
}}

type providerSettings struct {
	// Provider is the provider's address as OpenTofu writes it in a plan.
	Provider   string
	Version    string
	Type       string
	Attributes []string
}

// Setting is a provider-only setting that an accepted plan sets in place
// on an object it imports.
type Setting struct {
	Address   string
	Attribute string
	// Value is the value the plan sets, as the masked plan holds it.
	Value any
	// Provider and Version name the provider that the setting was
	// measured on.
	Provider string
	Version  string
}

// rawChange is a resource change of `tofu show -json` with its values as
// OpenTofu wrote them, before any mask. The mask may replace two different
// values with one placeholder, so which attributes a change sets is read
// from these and never from the masked plan.
type rawChange struct {
	Change struct {
		Before          any `json:"before"`
		After           any `json:"after"`
		AfterUnknown    any `json:"after_unknown"`
		BeforeSensitive any `json:"before_sensitive"`
		AfterSensitive  any `json:"after_sensitive"`
	} `json:"change"`
}

// rawChanges reads the resource changes of raw, the output of `tofu show
// -json` that p masks, in the order p holds them.
func rawChanges(raw []byte, p *mask.Plan) ([]rawChange, error) {
	var doc struct {
		ResourceChanges []rawChange `json:"resource_changes"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&doc); err != nil {
		return nil, errors.New("read the plan: it does not have the shape of tofu show -json output")
	}
	if len(doc.ResourceChanges) != len(p.ResourceChanges) {
		return nil, fmt.Errorf("the plan has %d resource changes and its masked document %d", len(doc.ResourceChanges), len(p.ResourceChanges))
	}
	return doc.ResourceChanges, nil
}

// settingsOnly returns the settings an imported change c sets when every
// attribute that raw, its values before the mask, changes is on the named
// list for its provider and type, and its value after is known and not
// sensitive. It also returns the attributes it changes that are not on the
// list, and those on the list whose value after is unknown or sensitive
// anywhere in it. Each setting's value is the one c, masked, holds. It
// returns false when the change's values are not objects.
func settingsOnly(c mask.ResourceChange, raw rawChange) (settings []Setting, others, hidden []string, ok bool) {
	var listed *providerSettings
	for i, s := range providerOnly {
		if s.Provider == c.ProviderName && s.Type == c.Type {
			listed = &providerOnly[i]
		}
	}
	changed, ok := changedAttributes(raw)
	if !ok {
		return nil, nil, nil, false
	}
	// changedAttributes refuses trees that are not objects, so these are.
	unknown, _ := attributeTree(raw.Change.AfterUnknown)
	sensitive, _ := attributeTree(raw.Change.AfterSensitive)
	after, _ := c.Change.After.(map[string]any)
	for _, a := range changed {
		switch {
		case listed == nil || !slices.Contains(listed.Attributes, a):
			others = append(others, a)
		case holdsTrue(unknown[a]) || holdsTrue(sensitive[a]):
			hidden = append(hidden, a)
		default:
			settings = append(settings, Setting{Address: c.Address, Attribute: a, Value: after[a], Provider: listed.Provider, Version: listed.Version})
		}
	}
	return settings, others, hidden, true
}

// changedAttributes is the sorted top-level attributes of c that are in
// only one of before and after or differ between them, that are unknown
// after, or whose sensitive marks are in only one of before_sensitive and
// after_sensitive or differ, false when before or after is not an object or
// after_unknown or a sensitive tree marks the whole value.
func changedAttributes(c rawChange) ([]string, bool) {
	before, ok1 := c.Change.Before.(map[string]any)
	after, ok2 := c.Change.After.(map[string]any)
	if !ok1 || !ok2 {
		return nil, false
	}
	changed := map[string]bool{}
	differing(before, after, changed)
	unknown, ok := attributeTree(c.Change.AfterUnknown)
	if !ok {
		return nil, false
	}
	for k, v := range unknown {
		if holdsTrue(v) {
			changed[k] = true
		}
	}
	bs, ok1 := attributeTree(c.Change.BeforeSensitive)
	as, ok2 := attributeTree(c.Change.AfterSensitive)
	if !ok1 || !ok2 {
		return nil, false
	}
	differing(bs, as, changed)
	out := make([]string, 0, len(changed))
	for k := range changed {
		out = append(out, k)
	}
	slices.Sort(out)
	return out, true
}

// differing marks each key that is in only one of a and b, or whose values
// differ. A key absent and a key whose value is null are different.
func differing(a, b map[string]any, changed map[string]bool) {
	for k, v := range a {
		w, ok := b[k]
		if !ok || !reflect.DeepEqual(v, w) {
			changed[k] = true
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			changed[k] = true
		}
	}
}

// attributeTree is t, an after_unknown or sensitive tree of an object, by
// attribute: nil when t is absent or marks nothing, false when it marks the
// whole object or is not an object.
func attributeTree(t any) (map[string]any, bool) {
	switch t := t.(type) {
	case nil:
		return nil, true
	case bool:
		return nil, !t
	case map[string]any:
		return t, true
	}
	return nil, false
}

// holdsTrue reports whether v, a value of an after_unknown or sensitive
// tree, marks any value.
func holdsTrue(v any) bool {
	switch v := v.(type) {
	case bool:
		return v
	case map[string]any:
		for _, e := range v {
			if holdsTrue(e) {
				return true
			}
		}
	case []any:
		for _, e := range v {
			if holdsTrue(e) {
				return true
			}
		}
	}
	return false
}
