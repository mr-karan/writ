package provider

import (
	"github.com/mr-karan/writ/internal/schema"
	"github.com/mr-karan/writ/internal/value"
)

// ProposeNew computes the proposed new state Terraform's protocol expects:
// config values where config sets them, prior values for computed attributes
// that config leaves null, null otherwise. Plan, apply, and import call it.
//
// It follows Terraform's objchange.ProposedNew. For a create, a computed
// attribute the config leaves null is null, not unknown: the provider marks
// what it will compute. Set blocks are proposed from config alone, because
// matching set elements to prior elements needs the provider's own rules.
func ProposeNew(b schema.Block, prior, config value.Value) value.Value {
	if config.Kind() == value.Null && prior.Kind() == value.Null {
		return value.NullVal()
	}
	return proposeBlock(b, prior, config)
}

func proposeBlock(b schema.Block, prior, config value.Value) value.Value {
	out := make(map[string]value.Value, len(b.Attributes)+len(b.Blocks))
	for _, a := range b.Attributes {
		cv := field(config, a.Name)
		switch {
		case cv.Kind() != value.Null:
			out[a.Name] = cv
		case a.Computed:
			out[a.Name] = field(prior, a.Name)
		default:
			out[a.Name] = value.NullVal()
		}
	}
	for _, nb := range b.Blocks {
		out[nb.Name] = proposeNested(nb, field(prior, nb.Name), field(config, nb.Name))
	}
	return value.ObjectVal(out)
}

func proposeNested(nb schema.NestedBlock, prior, config value.Value) value.Value {
	if config.Kind() == value.Unknown {
		return config
	}
	if nb.Nesting == schema.NestSingle || nb.IsRecord() {
		if config.Kind() == value.Null {
			return config
		}
		return proposeBlock(nb.Block, prior, config)
	}
	switch nb.Nesting {
	case schema.NestMap:
		if config.Kind() != value.Object {
			return config
		}
		out := make(map[string]value.Value, config.Len())
		for _, k := range config.Keys() {
			out[k] = proposeBlock(nb.Block, field(prior, k), config.Get(k))
		}
		return value.ObjectVal(out)
	case schema.NestList:
		if config.Kind() != value.List {
			return config
		}
		out := make([]value.Value, config.Len())
		for i := range out {
			var p value.Value
			if prior.Kind() == value.List && i < prior.Len() {
				p = prior.Index(i)
			}
			out[i] = proposeBlock(nb.Block, p, config.Index(i))
		}
		return value.ListVal(out)
	default:
		if config.Kind() != value.List {
			return config
		}
		out := make([]value.Value, config.Len())
		for i := range out {
			out[i] = proposeBlock(nb.Block, value.NullVal(), config.Index(i))
		}
		return value.ListVal(out)
	}
}

// field reads name from an object, and null from anything else, so a null
// prior proposes like an object with every attribute null.
func field(v value.Value, name string) value.Value {
	if v.Kind() != value.Object {
		return value.NullVal()
	}
	return v.Get(name)
}
