package provider

import (
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	"github.com/mr-karan/writ/internal/schema"
	"github.com/mr-karan/writ/internal/types"
)

// wireSchema holds what wire.go needs to convert values for one provider:
// the annotated Writ block and the cty object type of the provider config and
// of each resource type, built when the type is first used.
type wireSchema struct {
	config   wireBlock
	provider *schema.Provider
	types    *typeLoader
}

// wireBlock pairs an annotated Writ block with the cty type its values have
// on the wire. Both come from the same cached schema, so their attribute and
// block names match.
type wireBlock struct {
	block schema.Block
	ty    cty.Type
}

type wireResource struct {
	wireBlock
	readDefaults map[string]string
}

func (w *wireSchema) resource(typ string) (wireResource, error) {
	r, err := w.provider.ResourceByWire(typ)
	if err != nil {
		return wireResource{}, err
	}
	return wireResource{wireBlock: wireBlock{block: r.Block, ty: w.types.get(typ).ty}, readDefaults: r.ReadDefaults}, nil
}

// typeLoader reads and converts each resource type or data source at most
// once. The schema
// and the wire types of a resource type come from one conversion.
type typeLoader struct {
	read  func(wire string) (rawResource, error)
	types map[string]*loadedType // every wire name, fixed at construction
}

type loadedType struct {
	once sync.Once
	raw  schema.RawResource
	ty   cty.Type
	err  error
}

func (tl *typeLoader) get(wire string) *loadedType {
	lt := tl.types[wire]
	lt.once.Do(func() {
		r, err := tl.read(wire)
		if err != nil {
			lt.err = err
			return
		}
		b, ty, err := convertBlock(r.Block)
		if err != nil {
			lt.err = err
			return
		}
		lt.raw, lt.ty = schema.RawResource{Wire: wire, Version: r.Version, Block: b}, ty
	})
	return lt
}

func (tl *typeLoader) load(wire string) (schema.RawResource, error) {
	lt := tl.get(wire)
	return lt.raw, lt.err
}

// buildSchema turns a whole schema into Writ's annotated schema and the wire
// types used to encode values for it, converting every resource type, so it
// fails on any type Writ cannot represent.
//
// A cty number becomes a Writ Float: Float holds every number a provider
// accepts, and Int flows into it, so `length: 2` checks. Decoding narrows each
// integral value to an Int (see decodeValue).
func buildSchema(src schema.Source, version string, cs *cachedSchema) (*schema.Provider, *wireSchema, error) {
	from := func(m map[string]rawResource) lazyTypes {
		return lazyTypes{slices.Sorted(maps.Keys(m)), func(wire string) (rawResource, error) { return m[wire], nil }}
	}
	p, w, err := buildLazySchema(src, version, cs.Provider, from(cs.Resources), from(cs.DataSources))
	if err != nil {
		return nil, nil, err
	}
	if err := loadAll(p); err != nil {
		return nil, nil, err
	}
	return p, w, nil
}

// loadAll reads every resource type and data source and returns the first
// error.
func loadAll(p *schema.Provider) error {
	for _, name := range p.TypeNames() {
		if _, ok := p.Resource(name); !ok {
			return p.Err()
		}
	}
	for _, name := range p.DataSourceNames() {
		if _, ok := p.DataSource(name); !ok {
			return p.Err()
		}
	}
	return nil
}

// lazyTypes names every resource type, or every data source, and reads one
// from the cache.
type lazyTypes struct {
	wires []string
	read  func(string) (rawResource, error)
}

func (lt lazyTypes) loader() *typeLoader {
	tl := &typeLoader{read: lt.read, types: make(map[string]*loadedType, len(lt.wires))}
	for _, w := range lt.wires {
		tl.types[w] = &loadedType{}
	}
	return tl
}

// buildLazySchema is buildSchema for a schema read type by type: a resource
// type or data source is read and converted the first time it is looked up.
func buildLazySchema(src schema.Source, version string, provider rawBlock, resources, data lazyTypes) (*schema.Provider, *wireSchema, error) {
	cfg, cfgTy, err := convertBlock(provider)
	if err != nil {
		return nil, nil, fmt.Errorf("provider %s %s: provider config: %w", src, version, err)
	}
	rl, dl := resources.loader(), data.loader()
	// NewLazy annotates resources and data sources only; a sensitive provider
	// config attribute must accept a secret, so it carries the taint too.
	p, err := schema.NewLazy(src, version, taintSensitive(cfg),
		schema.Types{Wires: resources.wires, Load: rl.load},
		schema.Types{Wires: data.wires, Load: dl.load})
	if err != nil {
		return nil, nil, err
	}
	return p, &wireSchema{config: wireBlock{block: p.Config, ty: cfgTy}, provider: p, types: rl}, nil
}

func taintSensitive(b schema.Block) schema.Block {
	out := b
	out.Attributes = slices.Clone(b.Attributes)
	out.Blocks = slices.Clone(b.Blocks)
	for i, a := range out.Attributes {
		if a.Sensitive {
			out.Attributes[i].Type = types.Secret(a.Type)
		}
	}
	for i, nb := range out.Blocks {
		out.Blocks[i].Block = taintSensitive(nb.Block)
	}
	return out
}

// convertBlock returns a block's Writ form, with plain cty-derived types, and
// its implied cty object type, built the way OpenTofu's
// configschema.Block.ImpliedType builds it.
func convertBlock(rb rawBlock) (schema.Block, cty.Type, error) {
	b := schema.Block{Deprecated: rb.Deprecated, DeprecationMessage: rb.DeprecationMessage}
	atys := make(map[string]cty.Type, len(rb.Attributes)+len(rb.Blocks))
	for _, ra := range rb.Attributes {
		ty, wt, err := attributeType(ra)
		if err != nil {
			return schema.Block{}, cty.NilType, fmt.Errorf("attribute %s: %w", ra.Name, err)
		}
		atys[ra.Name] = ty
		b.Attributes = append(b.Attributes, schema.Attribute{
			Name:       ra.Name,
			Type:       wt,
			Required:   ra.Required,
			Optional:   ra.Optional,
			Computed:   ra.Computed,
			Sensitive:  ra.Sensitive,
			WriteOnly:  ra.WriteOnly,
			Deprecated: ra.Deprecated,
		})
	}
	for _, rn := range rb.Blocks {
		inner, ity, err := convertBlock(rn.Block)
		if err != nil {
			return schema.Block{}, cty.NilType, fmt.Errorf("block %s: %w", rn.Name, err)
		}
		nb := schema.NestedBlock{Name: rn.Name, Block: inner, MinItems: rn.MinItems, MaxItems: rn.MaxItems}
		switch rn.Nesting {
		// Writ has no group nesting. A group block is a single block that is
		// never null; Writ shows it as a record like any single block.
		case nestSingle, nestGroup:
			nb.Nesting, atys[rn.Name] = schema.NestSingle, ity
		case nestList:
			nb.Nesting, atys[rn.Name] = schema.NestList, cty.List(ity)
		case nestSet:
			nb.Nesting, atys[rn.Name] = schema.NestSet, cty.Set(ity)
		case nestMap:
			nb.Nesting, atys[rn.Name] = schema.NestMap, cty.Map(ity)
		default:
			return schema.Block{}, cty.NilType, fmt.Errorf("block %s: unknown nesting %q", rn.Name, rn.Nesting)
		}
		b.Blocks = append(b.Blocks, nb)
	}
	return b, cty.Object(atys), nil
}

func attributeType(ra rawAttribute) (cty.Type, types.Type, error) {
	if ra.Nested != nil {
		return nestedType(*ra.Nested)
	}
	ty, err := ctyjson.UnmarshalType(ra.Type)
	if err != nil {
		return cty.NilType, types.Type{}, err
	}
	wt, err := writType(ty)
	if err != nil {
		return cty.NilType, types.Type{}, err
	}
	// Optional object attributes matter to the checker, not to the wire,
	// where values carry every attribute.
	return ty.WithoutOptionalAttributesDeep(), wt, nil
}

// nestedType converts a protocol 6 nested attribute type. The record's
// fields keep their own flags, and sensitive fields carry the taint here
// because NewProvider annotates only top-level attributes.
func nestedType(o rawObject) (cty.Type, types.Type, error) {
	atys := make(map[string]cty.Type, len(o.Attributes))
	fields := make([]types.Field, 0, len(o.Attributes))
	for _, ra := range o.Attributes {
		ty, wt, err := attributeType(ra)
		if err != nil {
			return cty.NilType, types.Type{}, fmt.Errorf("%s: %w", ra.Name, err)
		}
		if ra.Sensitive {
			wt = types.Secret(wt)
		}
		atys[ra.Name] = ty
		fields = append(fields, types.Field{
			Name:     ra.Name,
			Type:     wt,
			Optional: !ra.Required,
			ReadOnly: ra.Computed && !ra.Optional && !ra.Required,
			Computed: ra.Computed,
		})
	}
	obj, rec := cty.Object(atys), types.RecordOf(fields...)
	switch o.Nesting {
	case nestSingle, nestGroup:
		return obj, rec, nil
	case nestList:
		return cty.List(obj), types.ListOf(rec), nil
	case nestSet:
		return cty.Set(obj), types.ListOf(rec), nil
	case nestMap:
		return cty.Map(obj), types.MapOf(rec), nil
	}
	return cty.NilType, types.Type{}, fmt.Errorf("unknown nesting %q", o.Nesting)
}

// writType maps a cty type to the Writ type a value of it has: sets become
// lists, and objects records. A field is optional only when the object type
// says so: OpenTofu refuses an object literal that leaves out any other
// field, such as a description of aws_security_group's ingress.
func writType(ty cty.Type) (types.Type, error) {
	switch {
	case ty == cty.String:
		return types.TString, nil
	case ty == cty.Number:
		return types.TFloat, nil
	case ty == cty.Bool:
		return types.TBool, nil
	case ty.IsListType(), ty.IsSetType():
		e, err := writType(ty.ElementType())
		return types.ListOf(e), err
	case ty.IsMapType():
		e, err := writType(ty.ElementType())
		return types.MapOf(e), err
	case ty.IsObjectType():
		fields := make([]types.Field, 0, len(ty.AttributeTypes()))
		for _, name := range slices.Sorted(maps.Keys(ty.AttributeTypes())) {
			ft, err := writType(ty.AttributeType(name))
			if err != nil {
				return types.Type{}, fmt.Errorf("%s: %w", name, err)
			}
			fields = append(fields, types.Field{Name: name, Type: ft, Optional: ty.AttributeOptional(name)})
		}
		return types.RecordOf(fields...), nil
	}
	return types.Type{}, fmt.Errorf("type %s is not supported in version 0", ty.FriendlyName())
}
