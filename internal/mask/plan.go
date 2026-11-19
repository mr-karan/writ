package mask

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/mr-karan/writ/internal/value"
)

// Plan is the masked plan document. It keeps the key names of `tofu show
// -json`, so a policy reads input.resource_changes as it does under conftest.
//
// The sweep leaves whole what the configuration names and no value can
// set, such as a resource's type and its actions, so that a secret which
// spells one cannot hide it from a policy or change a risk class. A field
// tagged mask:"keep" is not swept, mask:"keys" keeps a map's keys and
// sweeps its values, mask:"address" sweeps only the index keys of an
// address, mask:"index" sweeps an index whole as its address's key, and
// mask:"paths" keeps the first step of each path, which is
// always an attribute name.
type Plan struct {
	FormatVersion      string              `json:"format_version" mask:"keep"`
	TerraformVersion   string              `json:"terraform_version" mask:"keep"`
	Timestamp          string              `json:"timestamp,omitempty"`
	Errored            bool                `json:"errored"`
	Variables          []string            `json:"variables" mask:"keep"`
	Configuration      Configuration       `json:"configuration"`
	ResourceChanges    []ResourceChange    `json:"resource_changes"`
	ResourceDrift      []ResourceChange    `json:"resource_drift,omitempty"`
	OutputChanges      map[string]Change   `json:"output_changes,omitempty" mask:"keys"`
	Checks             any                 `json:"checks,omitempty"`
	RelevantAttributes []RelevantAttribute `json:"relevant_attributes,omitempty"`
}

// Configuration is what the masked document keeps of the configuration: the
// providers and the modules, with no expression or variable.
type Configuration struct {
	Providers []Provider `json:"providers"`
	Modules   []Module   `json:"modules,omitempty"`
}

type Provider struct {
	Key               string `json:"key" mask:"keep"`
	Name              string `json:"name" mask:"keep"`
	FullName          string `json:"full_name" mask:"keep"`
	Alias             string `json:"alias,omitempty" mask:"keep"`
	VersionConstraint string `json:"version_constraint,omitempty"`
	ModuleAddress     string `json:"module_address,omitempty" mask:"keep"`
}

type Module struct {
	Address           string `json:"address" mask:"keep"`
	Source            string `json:"source"`
	VersionConstraint string `json:"version_constraint,omitempty"`
}

type ResourceChange struct {
	Address         string `json:"address" mask:"address"`
	PreviousAddress string `json:"previous_address,omitempty" mask:"address"`
	ModuleAddress   string `json:"module_address,omitempty" mask:"address"`
	Mode            string `json:"mode" mask:"keep"`
	Type            string `json:"type" mask:"keep"`
	Name            string `json:"name" mask:"keep"`
	Index           any    `json:"index,omitempty" mask:"index"`
	ProviderName    string `json:"provider_name" mask:"keep"`
	Deposed         string `json:"deposed,omitempty"`
	Change          Change `json:"change"`
	ActionReason    string `json:"action_reason,omitempty" mask:"keep"`
}

// Change is one change as OpenTofu writes it, with every value under a true
// mark replaced by Sensitive. ChangedSensitivePaths lists the paths under a
// mark whose value differs between before and after, so a reviewer still
// sees that a secret changed.
type Change struct {
	Actions               []string   `json:"actions" mask:"keep"`
	Before                any        `json:"before"`
	After                 any        `json:"after"`
	AfterUnknown          any        `json:"after_unknown,omitempty"`
	BeforeSensitive       any        `json:"before_sensitive,omitempty"`
	AfterSensitive        any        `json:"after_sensitive,omitempty"`
	ReplacePaths          [][]any    `json:"replace_paths,omitempty" mask:"paths"`
	Importing             *Importing `json:"importing,omitempty"`
	GeneratedConfig       string     `json:"generated_config,omitempty"`
	ChangedSensitivePaths [][]any    `json:"changed_sensitive_paths,omitempty"`
}

type Importing struct {
	ID string `json:"id"`
}

type RelevantAttribute struct {
	Resource  string `json:"resource" mask:"address"`
	Attribute []any  `json:"attribute"`
}

// rawPlan is the part of `tofu show -json PLAN` the mask reads. Decoding
// into it is the allow-list: a key it does not name never reaches the
// masked document.
type rawPlan struct {
	FormatVersion    string `json:"format_version"`
	TerraformVersion string `json:"terraform_version"`
	Timestamp        string `json:"timestamp"`
	Errored          bool   `json:"errored"`
	Variables        map[string]struct {
		Value any `json:"value"`
	} `json:"variables"`
	Configuration struct {
		ProviderConfig map[string]struct {
			Name              string `json:"name"`
			FullName          string `json:"full_name"`
			Alias             string `json:"alias"`
			VersionConstraint string `json:"version_constraint"`
			ModuleAddress     string `json:"module_address"`
		} `json:"provider_config"`
		RootModule rawModule `json:"root_module"`
	} `json:"configuration"`
	ResourceChanges    []ResourceChange    `json:"resource_changes"`
	ResourceDrift      []ResourceChange    `json:"resource_drift"`
	OutputChanges      map[string]Change   `json:"output_changes"`
	Checks             any                 `json:"checks"`
	RelevantAttributes []RelevantAttribute `json:"relevant_attributes"`
}

type rawModule struct {
	Variables map[string]struct {
		Sensitive bool `json:"sensitive"`
	} `json:"variables"`
	ModuleCalls map[string]struct {
		Source            string    `json:"source"`
		VersionConstraint string    `json:"version_constraint"`
		Module            rawModule `json:"module"`
	} `json:"module_calls"`
}

// Plan masks the output of `tofu show -json PLAN`. It learns the plan's
// secrets: the values under OpenTofu's marks, before and after, and the
// variables declared sensitive. It masks values under secret names before
// the sweep, which may rewrite a key.
func (m *Mask) Plan(raw []byte) (*Plan, error) {
	var in rawPlan
	if err := decode(raw, &in); err != nil {
		return nil, fmt.Errorf("read the plan: %w", err)
	}
	if err := checkFormat(in.FormatVersion); err != nil {
		return nil, err
	}
	for name, v := range in.Variables {
		if in.Configuration.RootModule.Variables[name].Sensitive {
			m.collect(v.Value)
		}
	}
	for _, list := range []struct {
		name    string
		changes []ResourceChange
	}{{"resource_changes", in.ResourceChanges}, {"resource_drift", in.ResourceDrift}} {
		for i := range list.changes {
			c := &list.changes[i].Change
			if err := m.applyMarks(c); err != nil {
				return nil, fmt.Errorf("%s[%d]: %w", list.name, i, err)
			}
			c.Before, c.After = byName(c.Before), byName(c.After)
		}
	}
	for name, c := range in.OutputChanges {
		if err := m.applyMarks(&c); err != nil {
			return nil, fmt.Errorf("output_changes: %w", err)
		}
		if value.DeniedName(name) {
			c.Before, c.After = named(c.Before), named(c.After)
		} else {
			c.Before, c.After = byName(c.Before), byName(c.After)
		}
		in.OutputChanges[name] = c
	}

	out := &Plan{
		FormatVersion:      in.FormatVersion,
		TerraformVersion:   in.TerraformVersion,
		Timestamp:          in.Timestamp,
		Errored:            in.Errored,
		Variables:          []string{},
		Configuration:      Configuration{Providers: []Provider{}},
		ResourceChanges:    in.ResourceChanges,
		ResourceDrift:      in.ResourceDrift,
		OutputChanges:      in.OutputChanges,
		Checks:             in.Checks,
		RelevantAttributes: in.RelevantAttributes,
	}
	if out.ResourceChanges == nil {
		out.ResourceChanges = []ResourceChange{}
	}
	for name := range in.Variables {
		out.Variables = append(out.Variables, name)
	}
	slices.Sort(out.Variables)
	for key, p := range in.Configuration.ProviderConfig {
		out.Configuration.Providers = append(out.Configuration.Providers, Provider{
			Key: key, Name: p.Name, FullName: p.FullName, Alias: p.Alias,
			VersionConstraint: p.VersionConstraint, ModuleAddress: p.ModuleAddress,
		})
	}
	slices.SortFunc(out.Configuration.Providers, func(a, b Provider) int { return strings.Compare(a.Key, b.Key) })
	out.Configuration.Modules = modules("", in.Configuration.RootModule, nil)

	if err := m.Sweep(out); err != nil {
		return nil, err
	}
	return out, nil
}

// applyMarks records which sensitive paths change, then replaces every
// value under a mark.
func (m *Mask) applyMarks(c *Change) error {
	c.ChangedSensitivePaths = nil
	changedPaths(c.Before, c.After, c.BeforeSensitive, c.AfterSensitive, nil, &c.ChangedSensitivePaths)
	var err error
	if c.Before, err = m.redact(c.Before, c.BeforeSensitive, nil); err != nil {
		return fmt.Errorf("before_sensitive: %w", err)
	}
	if c.After, err = m.redact(c.After, c.AfterSensitive, c.AfterUnknown); err != nil {
		return fmt.Errorf("after_sensitive: %w", err)
	}
	return nil
}

// changedPaths appends each path where before or after is marked sensitive
// and the two values differ.
func changedPaths(before, after, bm, am any, path []any, out *[][]any) {
	if bm == true || am == true {
		if !reflect.DeepEqual(before, after) {
			*out = append(*out, slices.Clone(path))
		}
		return
	}
	bo, _ := bm.(map[string]any)
	ao, _ := am.(map[string]any)
	if bo != nil || ao != nil {
		keys := slices.Sorted(maps.Keys(bo))
		for k := range ao {
			if _, ok := bo[k]; !ok {
				keys = append(keys, k)
			}
		}
		slices.Sort(keys)
		bv, _ := before.(map[string]any)
		av, _ := after.(map[string]any)
		for _, k := range keys {
			changedPaths(bv[k], av[k], bo[k], ao[k], append(path, k), out)
		}
		return
	}
	bl, _ := bm.([]any)
	al, _ := am.([]any)
	bv, _ := before.([]any)
	av, _ := after.([]any)
	for i := range max(len(bl), len(al)) {
		changedPaths(at(bv, i), at(av, i), at(bl, i), at(al, i), append(path, i), out)
	}
}

// byName replaces the value of every attribute or key whose name holds a
// word of value.AlwaysSecret, at any depth, whatever its marks say.
func byName(v any) any {
	switch v := v.(type) {
	case map[string]any:
		for k, x := range v {
			if value.DeniedName(k) {
				v[k] = named(x)
			} else {
				v[k] = byName(x)
			}
		}
	case []any:
		for i, x := range v {
			v[i] = byName(x)
		}
	}
	return v
}

func named(v any) any {
	if v == nil {
		return nil
	}
	return Sensitive
}

// modules lists every module call under mod, depth first, by address.
func modules(prefix string, mod rawModule, out []Module) []Module {
	for _, n := range slices.Sorted(maps.Keys(mod.ModuleCalls)) {
		call := mod.ModuleCalls[n]
		address := prefix + "module." + n
		out = append(out, Module{Address: address, Source: call.Source, VersionConstraint: call.VersionConstraint})
		out = modules(address+".", call.Module, out)
	}
	return out
}
