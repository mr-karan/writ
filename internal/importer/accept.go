package importer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/mr-karan/writ/internal/addr"
	"github.com/mr-karan/writ/internal/mask"
)

// ImportsFormat is the format of the imports file that writ import bulk
// writes beside the code in an OpenTofu stack.
const ImportsFormat = "writ.import-selected/v0"

// Import is one object that writ import bulk selected in an OpenTofu
// stack: the resource instance in the root module that its import block
// adopts it as, and its import id.
type Import struct {
	Type string `json:"type"`
	Name string `json:"name"`
	// Key is the for_each key, nil for a resource without for_each.
	Key *string `json:"key"`
	ID  string  `json:"id"`
}

// Addr is the import's resource address as OpenTofu writes it.
func (i Import) Addr() string { return i.instance().address() }

// instance names a resource instance of the root module by its parts, so
// no quoting of a key decides whether two addresses are equal.
type instance struct {
	typ, name string
	keyed     bool
	key       string
}

// address is the instance's address as OpenTofu writes it in a plan.
func (in instance) address() string {
	a := in.typ + "." + in.name
	if in.keyed {
		a += "[" + tofuQuote(in.key) + "]"
	}
	return a
}

// tofuQuote is s quoted as OpenTofu 1.13.9 quotes a string key in an
// address, in toHCLQuotedString of internal/addrs/instance_key.go.
// It is tfcode.Quoted without the move to NFC, as OpenTofu prints a key.
func tofuQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i, r := range s {
		switch r {
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '$', '%':
			b.WriteRune(r)
			if strings.HasPrefix(s[i+1:], "{") {
				b.WriteRune(r)
			}
		default:
			switch {
			case unicode.IsPrint(r):
				b.WriteRune(r)
			case r < 0x10000:
				fmt.Fprintf(&b, `\u%04x`, r)
			default:
				fmt.Fprintf(&b, `\U%08x`, r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

func (i Import) instance() instance {
	in := instance{typ: i.Type, name: i.Name}
	if i.Key != nil {
		in.keyed, in.key = true, *i.Key
	}
	return in
}

// changeInstance is the instance a resource change is about, false when
// no import can name it: a data source, a module's resource, or a count
// index.
func changeInstance(c mask.ResourceChange) (instance, bool) {
	if c.Mode != "managed" || c.ModuleAddress != "" {
		return instance{}, false
	}
	in := instance{typ: c.Type, name: c.Name}
	switch k := c.Index.(type) {
	case nil:
	case string:
		in.keyed, in.key = true, k
	default:
		return instance{}, false
	}
	return in, true
}

type importsFile struct {
	Format  string   `json:"format"`
	Imports []Import `json:"imports"`
}

// EncodeImports is the imports file of imps.
func EncodeImports(imps []Import) ([]byte, error) {
	if err := checkImports(imps); err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(importsFile{Format: ImportsFormat, Imports: imps}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// ParseImports reads an imports file.
func ParseImports(raw []byte) ([]Import, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var f importsFile
	if err := dec.Decode(&f); err != nil {
		return nil, errors.New("it is not the JSON imports file writ import bulk writes beside the code")
	}
	if dec.More() {
		return nil, errors.New("more than one JSON value")
	}
	if f.Format != ImportsFormat {
		return nil, fmt.Errorf("its format is not %s, the file writ import bulk writes beside the code", ImportsFormat)
	}
	if err := checkImports(f.Imports); err != nil {
		return nil, err
	}
	return f.Imports, nil
}

// checkImports refuses no import, an import without a type, name, or id,
// and two imports of one resource instance. Its errors quote nothing of
// the file: they are printed before any plan's mask has read the secrets
// a key or id may hold.
func checkImports(imps []Import) error {
	if len(imps) == 0 {
		return errors.New("it names no import")
	}
	seen := map[instance]bool{}
	for i, imp := range imps {
		switch {
		case !addr.IsIdent(imp.Type) || !addr.IsIdent(imp.Name):
			return fmt.Errorf("imports[%d]: its type and name must be identifiers", i)
		case imp.ID == "":
			return fmt.Errorf("imports[%d] has no id", i)
		case seen[imp.instance()]:
			return fmt.Errorf("imports[%d] names the resource instance of an earlier import again", i)
		}
		seen[imp.instance()] = true
	}
	return nil
}

// Refusal is one reason a plan after an import is not accepted. Address is
// the resource change it is about, "" for the plan as a whole.
type Refusal struct {
	Address string
	Reason  string
}

// Acceptance is the judgement of a plan after an import.
type Acceptance struct {
	// Refusals are the reasons the plan is not accepted, none when it is.
	Refusals []Refusal
	// Imports is the number of selected objects the plan imports.
	Imports int
	// Settings are the provider-only settings the plan sets in place on
	// the objects it imports.
	Settings []Setting
}

// Quoting says whether the refusals of a judgement may quote import ids.
type Quoting bool

const (
	// QuoteIDs quotes the ids that differ, for the person who holds the
	// imports file.
	QuoteIDs Quoting = true
	// QuoteNoIDs names only the address and what differs, and sweeps every
	// id of the imports file out of the result, for a result that leaves
	// the person's machine, such as an agent's tool result. The plan's mask
	// hides an id that is a secret, and the imports file still holds it.
	QuoteNoIDs Quoting = false
)

// AcceptPlan judges p, the masked plan document of a stack after an import
// that selected want, as AcceptTofuPlan does. A masked document no longer
// holds the values that show which attributes an update changes, so an
// import that plans an update is refused.
func AcceptPlan(p *mask.Plan, want []Import, q Quoting) (Acceptance, error) {
	return accept(p, nil, want, mask.New(nil), q)
}

// AcceptTofuJSON judges raw, output of `tofu show -json` that the caller
// was handed, as AcceptPlan judges its masked document: nothing proves
// that raw is what tofu wrote for the plan that will be applied, so an
// import that plans an update is refused.
func AcceptTofuJSON(raw []byte, want []Import, q Quoting) (Acceptance, error) {
	m := mask.New(nil)
	p, err := m.Plan(raw)
	if err != nil {
		return Acceptance{}, err
	}
	return accept(p, nil, want, m, q)
}

// AcceptTofuPlan judges raw, the output of `tofu show -json` of a saved
// plan file that the caller ran itself, after an import that selected
// want. The plan is accepted when it did not error, imports each of want
// exactly once by its id, imports nothing else, and changes nothing else:
// OpenTofu then only adopts the selected objects. Each import is a no-op,
// or an update in place that changes only settings on the named list of
// providerOnly. A no-op that imports nothing is a resource the state
// already has. What an update changes is read from the values in raw.
// Every text and value of the result has passed the mask of raw.
func AcceptTofuPlan(raw []byte, want []Import) (Acceptance, error) {
	m := mask.New(nil)
	p, err := m.Plan(raw)
	if err != nil {
		return Acceptance{}, err
	}
	values, err := rawChanges(raw, p)
	if err != nil {
		return Acceptance{}, err
	}
	return accept(p, values, want, m, QuoteIDs)
}

// accept judges p. values are the changes of p before the mask, in its
// order, nil when only the masked document is at hand. m is the mask of
// p, which every text of the result passes.
func accept(p *mask.Plan, values []rawChange, want []Import, m *mask.Mask, q Quoting) (Acceptance, error) {
	var out []Refusal
	var settings []Setting
	if p.Errored {
		out = append(out, Refusal{Reason: "the plan errored, so OpenTofu did not finish it"})
	}
	wanted := make(map[instance]Import, len(want))
	for _, w := range want {
		wanted[w.instance()] = w
	}
	seen := map[instance]bool{}
	imports := 0
	for i, c := range p.ResourceChanges {
		noop := slices.Equal(c.Change.Actions, []string{"no-op"})
		if c.Change.Importing == nil {
			if !noop {
				out = append(out, Refusal{Address: c.Address, Reason: fmt.Sprintf("plans %s, and a plan after an import may only import", strings.Join(c.Change.Actions, " and "))})
			}
			continue
		}
		in, ok := changeInstance(c)
		// The address is what tofu apply acts on, so fields that name a
		// selected instance must not stand beside an address that names
		// another.
		if ok && c.Address != in.address() {
			out = append(out, Refusal{Address: c.Address, Reason: fmt.Sprintf("its address is not %s, which its module, type, name, and index make, so tofu show -json did not write this plan", in.address())})
			continue
		}
		w, selected := wanted[in]
		selected = selected && ok
		switch {
		case !selected:
			out = append(out, Refusal{Address: c.Address, Reason: "imports an object, and the import did not select this address"})
		case seen[in]:
			out = append(out, Refusal{Address: c.Address, Reason: "imports an object at an address that another change of the plan imports too"})
		case c.Change.Importing.ID != w.ID && q == QuoteNoIDs:
			out = append(out, Refusal{Address: c.Address, Reason: "its import id differs from the one the imports file selected"})
		case c.Change.Importing.ID != w.ID:
			out = append(out, Refusal{Address: c.Address, Reason: fmt.Sprintf("imports the object with id %q, and the import selected id %q", c.Change.Importing.ID, w.ID)})
		case noop:
			imports++
		default:
			var raw *rawChange
			if values != nil {
				raw = &values[i]
			}
			set, reason, err := importUpdate(c, raw, m)
			if err != nil {
				return Acceptance{}, err
			}
			if reason != "" {
				out = append(out, Refusal{Address: c.Address, Reason: reason})
				break
			}
			settings = append(settings, set...)
			imports++
		}
		if selected {
			seen[in] = true
		}
	}
	var ids []string
	for _, w := range want {
		ids = append(ids, w.ID)
		switch {
		case seen[w.instance()]:
		case q == QuoteNoIDs:
			out = append(out, Refusal{Address: w.Addr(), Reason: "the imports file selected an object at this address, and the plan does not import it"})
		default:
			out = append(out, Refusal{Address: w.Addr(), Reason: fmt.Sprintf("the import selected the object with id %q, and the plan does not import it", w.ID)})
		}
	}
	acc := Acceptance{Refusals: out, Imports: imports, Settings: settings}
	if err := m.Sweep(&acc); err != nil {
		return Acceptance{}, err
	}
	if q == QuoteNoIDs {
		if err := mask.New(ids).Sweep(&acc); err != nil {
			return Acceptance{}, err
		}
	}
	return acc, nil
}

// importUpdate returns the settings that c, the change of an object it
// imports, sets when it is an update in place of provider-only settings
// alone, and otherwise why it is refused. raw is c before the mask, nil
// when only a JSON document is at hand. Each attribute name in a reason has
// passed m, the mask of the plan, since a name can hold a secret.
func importUpdate(c mask.ResourceChange, raw *rawChange, m *mask.Mask) ([]Setting, string, error) {
	plans := strings.Join(c.Change.Actions, " and ")
	if !slices.Equal(c.Change.Actions, []string{"update"}) || len(c.Change.ReplacePaths) > 0 {
		return nil, fmt.Sprintf("imports the object and then plans %s, so the code does not describe it yet", plans), nil
	}
	if raw == nil {
		return nil, "imports the object and then plans update, and a plan given as JSON does not prove what the update changes; pass writ import accept the plan file that tofu plan -out saved, which it reads with tofu show -json", nil
	}
	settings, others, hidden, ok := settingsOnly(c, *raw)
	if !ok {
		return nil, "imports the object and then plans update, and the plan does not hold its values before and after as objects", nil
	}
	if err := m.Sweep(&others); err != nil {
		return nil, "", err
	}
	if err := m.Sweep(&hidden); err != nil {
		return nil, "", err
	}
	switch {
	case len(others) > 0:
		return nil, fmt.Sprintf("imports the object and then plans update of %s, which the named list of provider-only settings does not hold for %s, so the code does not describe it yet", strings.Join(quoteAll(others), ", "), c.Type), nil
	case len(hidden) > 0:
		return nil, fmt.Sprintf("imports the object and then plans update of %s, a provider-only setting whose value after the update is unknown until apply or sensitive, so the plan does not show what it sets", strings.Join(quoteAll(hidden), ", ")), nil
	case len(settings) == 0:
		return nil, "imports the object and then plans update, and the plan shows no attribute it changes", nil
	}
	return settings, "", nil
}

func quoteAll(attrs []string) []string {
	out := make([]string, len(attrs))
	for i, a := range attrs {
		out[i] = strconv.Quote(a)
	}
	return out
}
