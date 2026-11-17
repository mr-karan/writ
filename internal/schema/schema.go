// Package schema is Writ's own view of a provider's schema: resource types
// with Writ types on every attribute, refined by the annotation layer. The
// provider package builds it from the wire schema; the checker, the planner,
// and `writ schema` read it. Nothing here knows about protobuf or cty.
package schema

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/mr-karan/writ/internal/types"
)

// Source identifies a provider in a registry: registry.opentofu.org/hashicorp/tls.
type Source struct {
	Host      string // empty means registry.opentofu.org
	Namespace string
	Type      string // also the resource type prefix: tls_
}

// DefaultHost is the registry used when a `use` names no host.
const DefaultHost = "registry.opentofu.org"

// ParseSource reads "hashicorp/tls" or "example.com/acme/tls". A source that
// names the default host is stored without it, so both spellings compare equal.
func ParseSource(s string) (Source, error) {
	parts := strings.Split(s, "/")
	var src Source
	switch len(parts) {
	case 2:
		src = Source{Namespace: parts[0], Type: parts[1]}
	case 3:
		src = Source{Host: strings.ToLower(parts[0]), Namespace: parts[1], Type: parts[2]}
		if src.Host == DefaultHost {
			src.Host = ""
		}
	default:
		return Source{}, fmt.Errorf("provider source %q must be namespace/type or host/namespace/type", s)
	}
	for _, part := range []string{src.Namespace, src.Type} {
		if !validSourcePart(part) {
			return Source{}, fmt.Errorf("provider source %q: %q may hold only lowercase letters, digits, and hyphens", s, part)
		}
	}
	if src.Host != "" && strings.ContainsAny(src.Host, " \t") {
		return Source{}, fmt.Errorf("provider source %q has an invalid host", s)
	}
	return src, nil
}

func validSourcePart(s string) bool {
	if s == "" || s[0] == '-' {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

// String renders the short form when Host is the default.
func (s Source) String() string {
	if s.Host == "" {
		return s.Namespace + "/" + s.Type
	}
	return s.Host + "/" + s.Namespace + "/" + s.Type
}

// Provider is one provider version's schema, already annotated. A resource
// type or data source is read and annotated the first time it is looked up,
// so a stack pays only for the types it uses. Lookups are safe for
// concurrent use.
type Provider struct {
	Source  Source
	Version string
	Config  Block
	byName  map[string]*entry // by Writ type name: PrivateKey
	byWire  map[string]*entry // by wire name: tls_private_key
	refs    refIndex
	anns    map[string]annotation
	load    LoadFunc
	// Data sources have their own namespace: aws.Vpc names both a resource
	// type and a data source.
	dataByName map[string]*dataEntry
	dataByWire map[string]*dataEntry
	dataAnns   map[string]annotation
	loadData   LoadFunc

	mu      sync.Mutex
	loadErr error
}

// entry is one resource type, loaded at most once.
type entry struct {
	name types.Name
	wire string
	once sync.Once
	res  *Resource
	err  error
}

// Resource is one managed resource type.
type Resource struct {
	// Name is the Writ name, tls.PrivateKey. Wire is the provider's name,
	// tls_private_key. NewProvider derives Name from Wire, never the reverse.
	Name types.Name
	Wire string
	// Version is the schema version the provider reports for the type.
	Version int64
	Block   Block
	// Scaled lists the attribute paths a runtime such as an autoscaler
	// usually changes.
	Scaled []string
	// LegacySDK marks a type served by the legacy provider SDK (SDKv2),
	// which adds an optional, computed id to every resource type that the
	// plugin framework does not. It stores "" or 0 for an optional
	// attribute the configuration left unset, and plans null against that
	// value as no change.
	LegacySDK bool
	// ReadDefaults supplies missing provider-only settings after ReadResource.
	// Values are JSON, decoded against the provider's attribute types.
	ReadDefaults map[string]string
	Settings     *SettingsRule
	SettlingRead bool
	// Label names an object of a type with no name attribute, when
	// import bulk writes its code.
	Label *Label
}

// Label builds a readable name for an object whose import id is not one,
// such as an ARN.
type Label struct {
	// Parent is the attribute that holds the id of the object this one
	// belongs to. That object's name, when it is imported in the same run,
	// comes first.
	Parent string
	// Format builds the object's own part from its attributes, as an id
	// template does: {protocol}_{port}. Its Name tag replaces it.
	Format string
}

// SettingsRule identifies a provider-only switch whose consequence is already
// satisfied by the refreshed cloud state.
type SettingsRule struct {
	Attribute, Consequence, Matches string
}

// DataSource is one data source type, which a lookup reads.
type DataSource struct {
	// Name is the Writ name, local.File. Wire is the provider's name,
	// local_file.
	Name  types.Name
	Wire  string
	Block Block
}

// dataEntry is one data source, loaded at most once.
type dataEntry struct {
	name types.Name
	wire string
	once sync.Once
	ds   *DataSource
	err  error
}

// Block is a configuration block: attributes plus nested blocks.
type Block struct {
	Deprecated         bool
	DeprecationMessage string
	Attributes         []Attribute
	Blocks             []NestedBlock
}

// Attribute is one schema attribute with its Writ type. Type already includes
// the annotation refinement and, when Sensitive, the secret taint.
type Attribute struct {
	Name      string
	Type      types.Type
	Required  bool
	Optional  bool
	Computed  bool
	Sensitive bool
	// WriteOnly attributes are never stored: the provider reads them from
	// the config, and state always holds null for them.
	WriteOnly bool
	// Deprecated attributes still work, and the provider warns when the
	// config sets one.
	Deprecated bool
	// Filled is set by the annotation table for a computed attribute that
	// the provider sets to a non-null value when config leaves it null.
	Filled bool
}

// Nesting is how a nested block repeats.
type Nesting uint8

const (
	NestSingle Nesting = iota + 1 // a record
	NestList                      // a list of records
	NestSet                       // a list of records, order ignored
	NestMap                       // a map of records
)

// DisabledWhen identifies a block value that disables the feature.
type DisabledWhen struct {
	Field string
	Value string
}

// InlineOwner names the separate resource that replaces a deprecated inline block.
type InlineOwner struct {
	// Computed records the SDKv2 flag that the protocol omits for blocks.
	Computed    bool
	Wire        string
	ParentField string
	OwnerField  string
	InlineKey   string
	OwnerKey    string
}

// NestedBlock is a block type inside a block.
type NestedBlock struct {
	ImportDisabled *DisabledWhen
	ImportOwner    *InlineOwner
	Name           string
	Nesting        Nesting
	Block          Block
	MinItems       int64
	MaxItems       int64
	// single is set by the annotation table for list blocks that the provider
	// limits to one item in a validator rather than in MaxItems.
	single bool
}

// IsRecord reports whether Writ shows this block as one record. That holds for
// single blocks, and for list and set blocks limited to one item by MaxItems or
// by the annotation table. Such a list block is still a one-element list on the
// wire.
func (nb NestedBlock) IsRecord() bool {
	switch nb.Nesting {
	case NestSingle:
		return true
	case NestList, NestSet:
		return nb.MaxItems == 1 || nb.single
	}
	return false
}

// RawResource is the unannotated schema of one resource type or data source,
// filled in by the provider package from the wire schema.
type RawResource struct {
	Wire    string
	Version int64
	Block   Block
}

// LoadFunc returns the unannotated schema of one resource type or data
// source by its wire name. A Provider calls it at most once per type.
type LoadFunc func(wire string) (RawResource, error)

// Types names every resource type, or every data source, of a provider and
// reads each one's unannotated schema on first use.
type Types struct {
	Wires []string
	Load  LoadFunc
}

// NewProvider builds an annotated provider schema from every resource type at
// once, and fails if any of them cannot be annotated. Annotations that name
// an attribute this provider version lacks are skipped, because annotations
// cover several versions of a provider.
func NewProvider(src Source, version string, config Block, raw []RawResource) (*Provider, error) {
	byWire := make(map[string]RawResource, len(raw))
	wires := make([]string, 0, len(raw))
	for _, r := range raw {
		if _, dup := byWire[r.Wire]; dup {
			return nil, fmt.Errorf("provider %s: resource type %s appears twice", src, r.Wire)
		}
		byWire[r.Wire] = r
		wires = append(wires, r.Wire)
	}
	load := func(wire string) (RawResource, error) { return byWire[wire], nil }
	p, err := NewLazy(src, version, config, Types{Wires: wires, Load: load}, Types{})
	if err != nil {
		return nil, err
	}
	for _, w := range wires {
		if _, err := p.byWire[w].get(p); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// NewLazy builds an annotated provider schema that reads each resource type
// and data source the first time it is looked up. resources names every
// resource type the provider has, which the annotations need to infer typed
// references. NewProvider and NewLazy are the only constructors, so no
// schema exists without its annotations applied.
func NewLazy(src Source, version string, config Block, resources, data Types) (*Provider, error) {
	anns := annotations()[Source{Namespace: src.Namespace, Type: src.Type}]
	p := &Provider{
		Source: src, Version: version, Config: config, load: resources.Load, anns: anns.Types,
		byName:     make(map[string]*entry, len(resources.Wires)),
		byWire:     make(map[string]*entry, len(resources.Wires)),
		dataByName: make(map[string]*dataEntry, len(data.Wires)),
		dataByWire: make(map[string]*dataEntry, len(data.Wires)),
		dataAnns:   anns.Data,
		loadData:   data.Load,
	}
	for _, w := range resources.Wires {
		name, err := p.typeName(w, "resource type")
		if err != nil {
			return nil, err
		}
		if _, dup := p.byName[name.Type]; dup {
			return nil, fmt.Errorf("provider %s: two resource types map to %s", src, name)
		}
		e := &entry{name: name, wire: w}
		p.byName[name.Type], p.byWire[w] = e, e
	}
	for _, w := range data.Wires {
		name, err := p.typeName(w, "data source")
		if err != nil {
			return nil, err
		}
		if _, dup := p.dataByName[name.Type]; dup {
			return nil, fmt.Errorf("provider %s: two data sources map to %s", src, name)
		}
		e := &dataEntry{name: name, wire: w}
		p.dataByName[name.Type], p.dataByWire[w] = e, e
	}
	p.refs = newRefIndex(anns, p.byWire)
	return p, nil
}

// typeName derives the Writ name of a wire type: tls_self_signed_cert is
// tls.SelfSignedCert.
func (p *Provider) typeName(wire, what string) (types.Name, error) {
	prefix := p.Source.Type + "_"
	suffix, ok := strings.CutPrefix(wire, prefix)
	if !ok || suffix == "" {
		return types.Name{}, fmt.Errorf("provider %s: %s %q does not start with %q", p.Source, what, wire, prefix)
	}
	return types.Name{Provider: p.Source.Type, Type: pascal(suffix)}, nil
}

// get loads and annotates the entry's resource type once.
func (e *entry) get(p *Provider) (*Resource, error) {
	e.once.Do(func() {
		e.res, e.err = p.build(e)
		if e.err != nil {
			e.err = fmt.Errorf("provider %s %s: %s: %w", p.Source, p.Version, e.wire, e.err)
			p.mu.Lock()
			if p.loadErr == nil {
				p.loadErr = e.err
			}
			p.mu.Unlock()
		}
	})
	return e.res, e.err
}

// get loads and annotates the data source once.
func (e *dataEntry) get(p *Provider) (*DataSource, error) {
	e.once.Do(func() {
		e.ds, e.err = p.buildData(e)
		if e.err != nil {
			e.err = fmt.Errorf("provider %s %s: data source %s: %w", p.Source, p.Version, e.wire, e.err)
			p.mu.Lock()
			if p.loadErr == nil {
				p.loadErr = e.err
			}
			p.mu.Unlock()
		}
	})
	return e.ds, e.err
}

func (p *Provider) buildData(e *dataEntry) (*DataSource, error) {
	raw, err := p.loadData(e.wire)
	if err != nil {
		return nil, err
	}
	a := annotator{refs: &p.refs, owner: e.wire, ann: p.dataAnns[e.wire], data: true}
	block, err := a.block(raw.Block, "")
	if err != nil {
		return nil, err
	}
	return &DataSource{Name: e.name, Wire: e.wire, Block: block}, nil
}

func (p *Provider) build(e *entry) (*Resource, error) {
	raw, err := p.load(e.wire)
	if err != nil {
		return nil, err
	}
	ann := p.anns[e.wire]
	a := annotator{refs: &p.refs, owner: e.wire, ann: ann}
	block, err := a.block(raw.Block, "")
	if err != nil {
		return nil, err
	}
	legacy := slices.ContainsFunc(raw.Block.Attributes, func(a Attribute) bool { return a.Name == "id" && a.Optional && a.Computed })
	r := &Resource{Name: e.name, Wire: e.wire, Version: raw.Version, Block: block, LegacySDK: legacy}
	// Defaults are verified against this provider version. A new version
	// must prove the same behavior before it can normalize imported state.
	if p.Version == ann.ReadDefaultsVersion {
		r.ReadDefaults = maps.Clone(ann.ReadDefaults)
	}
	r.SettlingRead = ann.SettlingReadVersion != "" && p.Version == ann.SettlingReadVersion
	r.Label = ann.Label
	if p.Version == ann.SettingsVersion {
		r.Settings = ann.Settings
	}
	applyOwnership(p.Source, r)
	return r, nil
}

// pascal turns self_signed_cert into SelfSignedCert.
func pascal(snake string) string {
	var b strings.Builder
	for _, part := range strings.Split(snake, "_") {
		if part == "" {
			continue
		}
		b.WriteString(strings.ToUpper(part[:1]))
		b.WriteString(part[1:])
	}
	return b.String()
}

// Resource looks a resource type up by its Writ type name, PrivateKey. It
// reports false for a type the provider lacks and for one whose schema could
// not be read; Err tells the two apart.
func (p *Provider) Resource(writName string) (*Resource, bool) {
	e, ok := p.byName[writName]
	if !ok {
		return nil, false
	}
	r, err := e.get(p)
	return r, err == nil
}

// ResourceByWire looks a resource type up by its wire name, tls_private_key.
// The error says whether the provider lacks the type or its schema could not
// be read.
func (p *Provider) ResourceByWire(wire string) (*Resource, error) {
	e, ok := p.byWire[wire]
	if !ok {
		return nil, fmt.Errorf("provider %s has no resource type %s", p.Source, wire)
	}
	return e.get(p)
}

// TypeNames returns the Writ names of every resource type, sorted, without
// loading any of them.
func (p *Provider) TypeNames() []string {
	return slices.Sorted(maps.Keys(p.byName))
}

// DataSource looks a data source up by its Writ name, Ami. It reports false
// for a data source the provider lacks and for one whose schema could not be
// read; Err tells the two apart.
func (p *Provider) DataSource(writName string) (*DataSource, bool) {
	e, ok := p.dataByName[writName]
	if !ok {
		return nil, false
	}
	ds, err := e.get(p)
	return ds, err == nil
}

// DataSourceByWire looks a data source up by its wire name, aws_ami.
func (p *Provider) DataSourceByWire(wire string) (*DataSource, error) {
	e, ok := p.dataByWire[wire]
	if !ok {
		return nil, fmt.Errorf("provider %s has no data source %s", p.Source, wire)
	}
	return e.get(p)
}

// DataSourceNames returns the Writ names of every data source, sorted,
// without loading any of them.
func (p *Provider) DataSourceNames() []string {
	return slices.Sorted(maps.Keys(p.dataByName))
}

// Err returns the first error met while loading a resource type or data
// source, or nil. A type that failed to load is missing from Resource and
// DataSource, so a caller that sees a missing type checks Err before blaming
// the stack.
func (p *Provider) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.loadErr
}

// Lookup finds the attribute at a path such as subject.common_name, crossing
// nested blocks. The planner uses it to decide computed-ness when building the
// proposed new state.
func (b Block) Lookup(path []string) (Attribute, bool) {
	if len(path) == 0 {
		return Attribute{}, false
	}
	if len(path) == 1 {
		for _, a := range b.Attributes {
			if a.Name == path[0] {
				return a, true
			}
		}
		return Attribute{}, false
	}
	for _, nb := range b.Blocks {
		if nb.Name == path[0] {
			return nb.Block.Lookup(path[1:])
		}
	}
	return Attribute{}, false
}
