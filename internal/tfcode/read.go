package tfcode

import (
	"fmt"
	"maps"
	"math/big"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"

	"github.com/mr-karan/writ/internal/schema"
	"github.com/mr-karan/writ/internal/types"
	"github.com/mr-karan/writ/internal/value"
)

// Stack is what an import needs to know of a stack's code: the providers it
// requires and configures, and the names it declares.
type Stack struct {
	// Required maps the local name of each provider in required_providers
	// to its source.
	Required map[string]schema.Source
	// Providers holds each provider block by its reference.
	Providers map[ProviderRef]*Provider
	// Resources holds the names declared for each resource type:
	// Resources["random_uuid"]["main"].
	Resources map[string]map[string]bool
	// Variables are the names of the variable blocks.
	Variables map[string]bool
}

// ProviderRef names one provider configuration: the provider's local name
// and its alias, "" for the configuration without one.
type ProviderRef struct{ Name, Alias string }

// String is the reference as a resource's provider argument writes it.
func (r ProviderRef) String() string {
	if r.Alias == "" {
		return r.Name
	}
	return r.Name + "." + r.Alias
}

// Provider is one provider block.
type Provider struct {
	Ref  ProviderRef
	body *hclsyntax.Body
}

// Read reads the code of one stack: files maps the name of each .tf or .tofu
// file in the stack's directory to its text.
func Read(files map[string][]byte) (*Stack, error) {
	st := &Stack{Required: map[string]schema.Source{}, Providers: map[ProviderRef]*Provider{}, Resources: map[string]map[string]bool{}, Variables: map[string]bool{}}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		f, diags := hclsyntax.ParseConfig(files[name], name, hcl.InitialPos)
		if diags.HasErrors() {
			return nil, fmt.Errorf("read %s: %s", name, diags.Error())
		}
		for _, b := range f.Body.(*hclsyntax.Body).Blocks {
			if err := st.add(name, b); err != nil {
				return nil, err
			}
		}
	}
	return st, nil
}

func (st *Stack) add(file string, b *hclsyntax.Block) error {
	switch {
	case b.Type == "terraform":
		for _, inner := range b.Body.Blocks {
			if inner.Type == "required_providers" {
				if err := st.required(file, inner.Body); err != nil {
					return err
				}
			}
		}
	case b.Type == "provider" && len(b.Labels) == 1:
		ref := ProviderRef{Name: b.Labels[0]}
		if a, ok := b.Body.Attributes["alias"]; ok {
			v, diags := a.Expr.Value(nil)
			if diags.HasErrors() || v.Type() != cty.String || v.IsNull() {
				return fmt.Errorf("%s: provider %s: alias is not a literal string", file, ref.Name)
			}
			ref.Alias = v.AsString()
		}
		if _, dup := st.Providers[ref]; dup {
			return fmt.Errorf("%s: a second provider block for %s", file, ref)
		}
		st.Providers[ref] = &Provider{Ref: ref, body: b.Body}
	case b.Type == "resource" && len(b.Labels) == 2:
		if st.Resources[b.Labels[0]] == nil {
			st.Resources[b.Labels[0]] = map[string]bool{}
		}
		st.Resources[b.Labels[0]][b.Labels[1]] = true
	case b.Type == "variable" && len(b.Labels) == 1:
		st.Variables[b.Labels[0]] = true
	}
	return nil
}

// required reads required_providers: each attribute is a local name, whose
// value is an object with a source, or a bare version constraint.
func (st *Stack) required(file string, body *hclsyntax.Body) error {
	for name, a := range body.Attributes {
		v, diags := a.Expr.Value(nil)
		if diags.HasErrors() {
			return fmt.Errorf("%s: required_providers.%s is not a literal: %s", file, name, diags.Error())
		}
		src := "hashicorp/" + name
		if v.Type().IsObjectType() && v.Type().HasAttribute("source") {
			s := v.GetAttr("source")
			if s.Type() != cty.String || s.IsNull() {
				return fmt.Errorf("%s: required_providers.%s.source is not a string", file, name)
			}
			src = s.AsString()
		}
		parsed, err := schema.ParseSource(src)
		if err != nil {
			return fmt.Errorf("%s: required_providers.%s: %w", file, name, err)
		}
		st.Required[name] = parsed
	}
	return nil
}

// Source is the provider source of a local name: the one required_providers
// gives, or hashicorp/NAME, as OpenTofu implies.
func (st *Stack) Source(name string) schema.Source {
	if s, ok := st.Required[name]; ok {
		return s
	}
	return schema.Source{Namespace: "hashicorp", Type: name}
}

// Config is the provider block as a value of the provider's configuration
// block. It reads literal values only: an expression that refers to a
// variable, a local value, or anything else is an error that names the
// attribute. A sensitive attribute's value is marked secret.
func (p *Provider) Config(b schema.Block) (value.Value, error) {
	if p == nil {
		return value.ObjectVal(nil), nil
	}
	return blockValue(p.Ref.String(), p.body, b, true)
}

func blockValue(at string, body *hclsyntax.Body, b schema.Block, top bool) (value.Value, error) {
	out := map[string]value.Value{}
	for name, a := range body.Attributes {
		if top && (name == "alias" || name == "version") {
			continue
		}
		i := slices.IndexFunc(b.Attributes, func(sa schema.Attribute) bool { return sa.Name == name })
		if i < 0 {
			return value.Value{}, fmt.Errorf("provider %s: %s is not an argument of the provider", at, name)
		}
		v, diags := a.Expr.Value(nil)
		if diags.HasErrors() {
			return value.Value{}, fmt.Errorf("provider %s: %s is not a literal value, and writ import reads only literals in a provider block; set it in the environment the provider reads, or write the value", at, name)
		}
		sa := b.Attributes[i]
		conv, err := fromCty(v, sa.Type, at+"."+name)
		if err != nil {
			return value.Value{}, err
		}
		if sa.Sensitive {
			conv = conv.Secret()
		}
		out[name] = conv
	}
	byType := map[string][]*hclsyntax.Block{}
	for _, inner := range body.Blocks {
		byType[inner.Type] = append(byType[inner.Type], inner)
	}
	for typ, blocks := range byType {
		i := slices.IndexFunc(b.Blocks, func(nb schema.NestedBlock) bool { return nb.Name == typ })
		if i < 0 {
			return value.Value{}, fmt.Errorf("provider %s: %s is not a block of the provider", at, typ)
		}
		nb := b.Blocks[i]
		switch nb.Nesting {
		case schema.NestSingle:
			if len(blocks) > 1 {
				return value.Value{}, fmt.Errorf("provider %s: %s is written more than once", at, typ)
			}
			v, err := blockValue(at+"."+typ, blocks[0].Body, nb.Block, false)
			if err != nil {
				return value.Value{}, err
			}
			out[typ] = v
		case schema.NestMap:
			m := map[string]value.Value{}
			for _, inner := range blocks {
				if len(inner.Labels) != 1 {
					return value.Value{}, fmt.Errorf("provider %s: %s needs one label", at, typ)
				}
				v, err := blockValue(at+"."+typ, inner.Body, nb.Block, false)
				if err != nil {
					return value.Value{}, err
				}
				m[inner.Labels[0]] = v
			}
			out[typ] = value.ObjectVal(m)
		default:
			var elems []value.Value
			for _, inner := range blocks {
				v, err := blockValue(at+"."+typ, inner.Body, nb.Block, false)
				if err != nil {
					return value.Value{}, err
				}
				elems = append(elems, v)
			}
			out[typ] = value.ListVal(elems)
		}
	}
	return value.ObjectVal(out), nil
}

// fromCty converts a literal to a value of type t.
func fromCty(v cty.Value, t types.Type, at string) (value.Value, error) {
	if v.IsNull() {
		return value.NullVal(), nil
	}
	if !v.IsWhollyKnown() {
		return value.Value{}, fmt.Errorf("provider %s: the value is not known", at)
	}
	ty := v.Type()
	switch t.Kind() {
	case types.String, types.Enum, types.Ref:
		switch ty {
		case cty.String:
			return value.StringVal(v.AsString()), nil
		case cty.Number:
			return value.StringVal(v.AsBigFloat().Text('f', -1)), nil
		case cty.Bool:
			return value.StringVal(fmt.Sprint(v.True())), nil
		}
	case types.Bool:
		switch {
		case ty == cty.Bool:
			return value.BoolVal(v.True()), nil
		case ty == cty.String && (v.AsString() == "true" || v.AsString() == "false"):
			return value.BoolVal(v.AsString() == "true"), nil
		}
	case types.Int, types.Float:
		if ty != cty.Number {
			break
		}
		f := v.AsBigFloat()
		if i, acc := f.Int64(); acc == big.Exact && t.Kind() == types.Int {
			return value.IntVal(i), nil
		}
		if t.Kind() == types.Int {
			return value.Value{}, fmt.Errorf("provider %s: want a whole number", at)
		}
		x, _ := f.Float64()
		return value.FloatVal(x), nil
	case types.List:
		if !ty.IsListType() && !ty.IsTupleType() && !ty.IsSetType() {
			break
		}
		var elems []value.Value
		for it := v.ElementIterator(); it.Next(); {
			_, e := it.Element()
			ev, err := fromCty(e, t.Elem(), at)
			if err != nil {
				return value.Value{}, err
			}
			elems = append(elems, ev)
		}
		return value.ListVal(elems), nil
	case types.Map, types.Record:
		if !ty.IsMapType() && !ty.IsObjectType() {
			break
		}
		m := map[string]value.Value{}
		for it := v.ElementIterator(); it.Next(); {
			k, e := it.Element()
			ft := t.Elem()
			if t.Kind() == types.Record {
				f, ok := t.Field(k.AsString())
				if !ok {
					return value.Value{}, fmt.Errorf("provider %s: %s is not a field", at, k.AsString())
				}
				ft = f.Type
			}
			ev, err := fromCty(e, ft, at+"."+k.AsString())
			if err != nil {
				return value.Value{}, err
			}
			m[k.AsString()] = ev
		}
		return value.ObjectVal(m), nil
	}
	return value.Value{}, fmt.Errorf("provider %s: want %s, got %s", at, t, strings.ToLower(ty.FriendlyName()))
}
