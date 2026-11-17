package provider

import (
	"encoding/json"
	"fmt"
)

// CachedSchema is one provider's cached schema, read type by type, in the
// shape `tofu providers schema -json` gives a type. It starts no provider.
type CachedSchema struct {
	c *schemaCache
}

// OpenCachedSchema opens the cached schema of the binary lock pins for req
// on this platform. Install with req makes both the pin and the cache.
func OpenCachedSchema(dirs Dirs, lock *Lock, req Requirement) (*CachedSchema, error) {
	entry, ok := lock.Find(req.Source, req.Version)
	if !ok {
		return nil, &MissingError{Missing: []Requirement{req}}
	}
	c, err := openSchemaCache(dirs, entry.Binary)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", req.Source, req.Version, err)
	}
	return &CachedSchema{c: c}, nil
}

// Resources lists the provider's resource types, sorted.
func (s *CachedSchema) Resources() []string { return s.c.index.Resources }

// DataSources lists the provider's data sources, sorted.
func (s *CachedSchema) DataSources() []string { return s.c.index.DataSources }

// HasResource reports whether the provider has the resource type wire.
func (s *CachedSchema) HasResource(wire string) bool { _, ok := s.c.pos[wire]; return ok }

// HasDataSource reports whether the provider has the data source wire.
func (s *CachedSchema) HasDataSource(wire string) bool { _, ok := s.c.dataPos[wire]; return ok }

// Resource reads one resource type.
func (s *CachedSchema) Resource(wire string) (*TypeSchema, error) {
	r, err := s.c.resource(wire)
	if err != nil {
		return nil, err
	}
	return &TypeSchema{Version: r.Version, Block: tofuBlock(r.Block)}, nil
}

// DataSource reads one data source.
func (s *CachedSchema) DataSource(wire string) (*TypeSchema, error) {
	r, err := s.c.dataSource(wire)
	if err != nil {
		return nil, err
	}
	return &TypeSchema{Version: r.Version, Block: tofuBlock(r.Block)}, nil
}

// TypeSchema is one type as `tofu providers schema -json` gives it, less
// the descriptions, which the cache does not keep.
type TypeSchema struct {
	Version int64       `json:"version"`
	Block   SchemaBlock `json:"block"`
}

// SchemaBlock is a block of a TypeSchema.
type SchemaBlock struct {
	Attributes map[string]SchemaAttribute `json:"attributes,omitempty"`
	BlockTypes map[string]SchemaBlockType `json:"block_types,omitempty"`
	Deprecated bool                       `json:"deprecated,omitempty"`
}

// SchemaAttribute is an attribute of a SchemaBlock. Type is cty's JSON
// type encoding, as OpenTofu prints it, such as "string" or
// ["list","string"]; a protocol 6 nested attribute has NestedType instead.
type SchemaAttribute struct {
	Type       json.RawMessage   `json:"type,omitempty"`
	NestedType *SchemaNestedType `json:"nested_type,omitempty"`
	Required   bool              `json:"required,omitempty"`
	Optional   bool              `json:"optional,omitempty"`
	Computed   bool              `json:"computed,omitempty"`
	Sensitive  bool              `json:"sensitive,omitempty"`
	WriteOnly  bool              `json:"write_only,omitempty"`
	Deprecated bool              `json:"deprecated,omitempty"`
}

// SchemaNestedType is a protocol 6 nested attribute's type.
type SchemaNestedType struct {
	Attributes  map[string]SchemaAttribute `json:"attributes"`
	NestingMode string                     `json:"nesting_mode"`
}

// SchemaBlockType is a nested block of a SchemaBlock.
type SchemaBlockType struct {
	NestingMode string      `json:"nesting_mode"`
	Block       SchemaBlock `json:"block"`
	MinItems    int64       `json:"min_items,omitempty"`
	MaxItems    int64       `json:"max_items,omitempty"`
}

func tofuBlock(b rawBlock) SchemaBlock {
	out := SchemaBlock{Deprecated: b.Deprecated}
	if len(b.Attributes) > 0 {
		out.Attributes = tofuAttributes(b.Attributes)
	}
	if len(b.Blocks) > 0 {
		out.BlockTypes = make(map[string]SchemaBlockType, len(b.Blocks))
		for _, nb := range b.Blocks {
			out.BlockTypes[nb.Name] = SchemaBlockType{NestingMode: string(nb.Nesting), Block: tofuBlock(nb.Block), MinItems: nb.MinItems, MaxItems: nb.MaxItems}
		}
	}
	return out
}

func tofuAttributes(attrs []rawAttribute) map[string]SchemaAttribute {
	out := make(map[string]SchemaAttribute, len(attrs))
	for _, a := range attrs {
		sa := SchemaAttribute{Type: a.Type, Required: a.Required, Optional: a.Optional, Computed: a.Computed, Sensitive: a.Sensitive, WriteOnly: a.WriteOnly, Deprecated: a.Deprecated}
		if a.Nested != nil {
			sa.NestedType = &SchemaNestedType{Attributes: tofuAttributes(a.Nested.Attributes), NestingMode: string(a.Nested.Nesting)}
		}
		out[a.Name] = sa
	}
	return out
}
