package importer

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/mr-karan/writ/internal/types"
	"github.com/mr-karan/writ/internal/value"
)

// gen holds what rendering the code declares besides resources: a variable
// for each secret value, and the Scaled fields it leaves to a runtime.
type gen struct {
	// names are the names taken in the file, for naming variables.
	names     map[string]bool
	params    []Param
	paramVals map[string]string
	// initials are the Scaled fields put in lifecycle.ignore_changes, in
	// the order the code was generated.
	initials []initialField
}

type initialField struct {
	n    *node
	attr string
}

type source struct {
	n    *node
	attr string
	typ  types.Type
}

// refKinds are the kinds of typed reference. Assigning a single-target
// reference of each kind tells whether an attribute's type names a resource,
// without reading the type's targets.
var refKinds = []types.RefKind{types.IdRef, types.PemRef, types.ArnRef, types.NameRef}

// sources indexes, by value, every attribute that names the resource it
// belongs to: a typed reference whose targets include the resource's own
// type, such as tls_self_signed_cert.cert_pem or an AWS id.
type sources map[string][]source

func indexSources(nodes []*node) sources {
	out := sources{}
	for _, n := range nodes {
		for _, a := range n.schema.Block.Attributes {
			if a.Type.Kind() != types.Ref || !names(a.Type, n.schema.Name) {
				continue
			}
			v := n.state.Get(a.Name)
			if v.Kind() != value.String || v.AsString() == "" {
				continue
			}
			s := v.AsString()
			out[s] = append(out[s], source{n: n, attr: a.Name, typ: a.Type})
		}
	}
	return out
}

// candidates returns the sources a literal s in a slot of type slot on
// resource from may refer to: other resources' attributes whose type the
// slot accepts. A plain String slot has none, so a coincidence never
// becomes an edge.
func (ss sources) candidates(from *node, slot types.Type, s string) []source {
	if slot.Kind() != types.Ref {
		return nil
	}
	var out []source
	for _, src := range ss[s] {
		if src.n == from {
			continue
		}
		if _, ok := types.Assign(slot, src.typ); ok {
			out = append(out, src)
		}
	}
	return out
}

func newGen(names map[string]bool) *gen {
	return &gen{names: names, paramVals: map[string]string{}}
}

// names reports whether a typed reference of type t accepts a reference to
// a resource of type name: Pem<tls.SelfSignedCert> names tls.SelfSignedCert.
func names(t types.Type, name types.Name) bool {
	for _, k := range refKinds {
		if _, ok := types.Assign(t, types.RefTo(k, name)); ok {
			return true
		}
	}
	return false
}

// secretContainer reports a list or map that is secret as a whole, which
// becomes one param: its keys are as secret as its elements.
func secretContainer(v value.Value) bool {
	return v.IsSecret() && (v.Kind() == value.List || v.Kind() == value.Object)
}

// param declares a variable for a secret scalar, or for a secret list or
// map of type t, and returns its name, so the value stays out of the code.
// A list or map variable's value is JSON.
func (g *gen) param(n *node, v value.Value, t types.Type, slotSecret bool, p value.Path) string {
	var pt types.Type
	var raw string
	switch v.Kind() {
	case value.List, value.Object:
		pt, raw = t, jsonText(v)
	case value.String:
		pt, raw = types.TString, v.AsString()
	case value.Int:
		pt, raw = types.TInt, strconv.FormatInt(v.AsInt(), 10)
	case value.Float:
		pt, raw = types.TFloat, strconv.FormatFloat(v.AsFloat(), 'g', -1, 64)
	case value.Bool:
		pt, raw = types.TBool, strconv.FormatBool(v.AsBool())
	}
	if slotSecret {
		pt = types.Secret(pt)
	}
	typ := hclType(pt)
	base := ident(strings.NewReplacer(".", "_", "[", "_", "]", "", `"`, "").Replace(n.addr.String() + "_" + p.String()))
	name := base
	for i := 2; g.names[name] || !validLet(name); i++ {
		name = base + "_" + strconv.Itoa(i)
	}
	g.names[name] = true
	g.params = append(g.params, Param{Name: name, Type: typ, From: n.tf, Attr: p.String()})
	g.paramVals[name] = raw
	return name
}

// jsonText writes a value as JSON, maps with their keys sorted.
func jsonText(v value.Value) string {
	b, err := json.Marshal(jsonOf(v))
	if err != nil {
		panic("importer: a value does not encode as JSON: " + err.Error())
	}
	return string(b)
}

func jsonOf(v value.Value) any {
	switch v.Kind() {
	case value.String:
		return v.AsString()
	case value.Int:
		return v.AsInt()
	case value.Float:
		return v.AsFloat()
	case value.Bool:
		return v.AsBool()
	case value.List:
		out := make([]any, v.Len())
		for i := range out {
			out[i] = jsonOf(v.Index(i))
		}
		return out
	case value.Object:
		out := make(map[string]any, v.Len())
		for _, k := range v.Keys() {
			out[k] = jsonOf(v.Get(k))
		}
		return out
	}
	return nil
}
