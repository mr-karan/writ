package provider

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/mr-karan/writ/internal/schema"
)

// schemaFormat versions the cache layout. It is part of the cache path, so
// a cache written in another format is a miss, which installing the provider again repairs.
const schemaFormat = 5

// cachedSchema is a provider's whole schema in Writ's protocol-neutral form,
// as GetProviderSchema returns it. Attribute types stay cty JSON type bytes,
// which only wire.go interprets, so reading the cache needs no protobuf.
type cachedSchema struct {
	Protocol int `json:"protocol"`
	// GetSchemaOptional is the server capability get_provider_schema_optional.
	// A provider without it refuses other calls until it has served its schema.
	GetSchemaOptional bool                   `json:"get_schema_optional"`
	Provider          rawBlock               `json:"provider"`
	Resources         map[string]rawResource `json:"resources"`
	DataSources       map[string]rawResource `json:"data_sources"`
}

type rawResource struct {
	Version int64    `json:"version"`
	Block   rawBlock `json:"block"`
}

type rawBlock struct {
	Deprecated         bool             `json:"deprecated,omitempty"`
	DeprecationMessage string           `json:"deprecation_message,omitempty"`
	Attributes         []rawAttribute   `json:"attributes,omitempty"`
	Blocks             []rawNestedBlock `json:"blocks,omitempty"`
}

type rawAttribute struct {
	Name string `json:"name"`
	// Type is the cty type in cty's JSON type encoding. It is empty when
	// Nested is set.
	Type               json.RawMessage `json:"type,omitempty"`
	Nested             *rawObject      `json:"nested,omitempty"`
	Required           bool            `json:"required,omitempty"`
	Optional           bool            `json:"optional,omitempty"`
	Computed           bool            `json:"computed,omitempty"`
	Sensitive          bool            `json:"sensitive,omitempty"`
	WriteOnly          bool            `json:"write_only,omitempty"`
	Deprecated         bool            `json:"deprecated,omitempty"`
	DeprecationMessage string          `json:"deprecation_message,omitempty"`
}

// rawObject is a protocol 6 nested attribute type.
type rawObject struct {
	Nesting    rawNesting     `json:"nesting"`
	Attributes []rawAttribute `json:"attributes"`
}

type rawNestedBlock struct {
	Name     string     `json:"name"`
	Nesting  rawNesting `json:"nesting"`
	MinItems int64      `json:"min_items,omitempty"`
	MaxItems int64      `json:"max_items,omitempty"`
	Block    rawBlock   `json:"block"`
}

// rawNesting is the protocol's nesting mode, by name, so the cache does not
// depend on either protocol's enum numbering.
type rawNesting string

const (
	nestSingle rawNesting = "single"
	nestGroup  rawNesting = "group"
	nestList   rawNesting = "list"
	nestSet    rawNesting = "set"
	nestMap    rawNesting = "map"
)

// SchemaDir is where this Writ caches the schema of the provider binary with
// the given SHA-256. The format version is part of the path, so a cache
// written by another Writ version is never read as this one.
//
// The cache holds one directory per provider binary. index.json holds
// everything but the resource types and data sources; each is a file of its
// own, named by its position in the index, so reading a schema costs only the
// types a stack uses. A provider like hashicorp/aws has over 1,700 types and
// 12 MB of schema.
//
//	<Cache>/schemas/v5/<binary sha256 hex>/index.json
//	<Cache>/schemas/v5/<binary sha256 hex>/<n>.json   resource type n
//	<Cache>/schemas/v5/<binary sha256 hex>/d<n>.json  data source n
func (d Dirs) SchemaDir(binary [32]byte) string {
	return filepath.Join(d.Cache, "schemas", fmt.Sprintf("v%d", schemaFormat), hex.EncodeToString(binary[:]))
}

type schemaIndex struct {
	Format            int      `json:"format"`
	Protocol          int      `json:"protocol"`
	GetSchemaOptional bool     `json:"get_schema_optional"`
	Provider          rawBlock `json:"provider"`
	// Resources lists the wire type names, sorted. The schema of Resources[n]
	// is in n.json.
	Resources []string `json:"resources"`
	// DataSources lists the wire data source names, sorted. The schema of
	// DataSources[n] is in d<n>.json.
	DataSources []string `json:"data_sources"`
}

// schemaCache is one provider's cached schema, read type by type.
type schemaCache struct {
	dir     string
	index   schemaIndex
	pos     map[string]int
	dataPos map[string]int
}

// errNoSchema means the cache holds no usable schema for a binary.
var errNoSchema = errors.New("no cached schema")

func openSchemaCache(d Dirs, binary [32]byte) (*schemaCache, error) {
	dir := d.SchemaDir(binary)
	data, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errNoSchema
	}
	if err != nil {
		return nil, err
	}
	c := &schemaCache{dir: dir}
	if err := json.Unmarshal(data, &c.index); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", errNoSchema, dir, err)
	}
	if c.index.Format != schemaFormat {
		return nil, fmt.Errorf("%w: %s has format %d, want %d", errNoSchema, dir, c.index.Format, schemaFormat)
	}
	c.pos = make(map[string]int, len(c.index.Resources))
	for i, w := range c.index.Resources {
		c.pos[w] = i
	}
	c.dataPos = make(map[string]int, len(c.index.DataSources))
	for i, w := range c.index.DataSources {
		c.dataPos[w] = i
	}
	return c, nil
}

func (c *schemaCache) resource(wire string) (rawResource, error) {
	i, ok := c.pos[wire]
	if !ok {
		return rawResource{}, fmt.Errorf("schema cache %s has no resource type %s", c.dir, wire)
	}
	return c.read(strconv.Itoa(i)+".json", wire)
}

func (c *schemaCache) dataSource(wire string) (rawResource, error) {
	i, ok := c.dataPos[wire]
	if !ok {
		return rawResource{}, fmt.Errorf("schema cache %s has no data source %s", c.dir, wire)
	}
	return c.read("d"+strconv.Itoa(i)+".json", wire)
}

func (c *schemaCache) read(name, wire string) (rawResource, error) {
	data, err := os.ReadFile(filepath.Join(c.dir, name))
	if err != nil {
		return rawResource{}, fmt.Errorf("schema cache: %w; install the provider again", err)
	}
	var r rawResource
	if err := json.Unmarshal(data, &r); err != nil {
		return rawResource{}, fmt.Errorf("schema cache %s: %s: %w; install the provider again", c.dir, wire, err)
	}
	if wire == "nomad_job" {
		for i := range r.Block.Attributes {
			if r.Block.Attributes[i].Name == "plan_response" {
				r.Block.Attributes[i].Sensitive = true
			}
		}
	}
	return r, nil
}

// build returns the annotated schema and wire types, reading each resource
// type from disk when it is first used.
func (c *schemaCache) build(src schema.Source, version string) (*schema.Provider, *wireSchema, error) {
	return buildLazySchema(src, version, c.index.Provider, lazyTypes{c.index.Resources, c.resource}, lazyTypes{c.index.DataSources, c.dataSource})
}

// writeSchemaCache writes the schema into a fresh directory and renames it
// into place, so a reader never sees a partial schema. A schema already in
// place is replaced.
func writeSchemaCache(d Dirs, binary [32]byte, cs *cachedSchema) error {
	final := d.SchemaDir(binary)
	parent := filepath.Dir(final)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, ".tmp-"+filepath.Base(final)+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	index := schemaIndex{
		Format: schemaFormat, Protocol: cs.Protocol, GetSchemaOptional: cs.GetSchemaOptional,
		Provider: cs.Provider, Resources: slices.Sorted(maps.Keys(cs.Resources)),
		DataSources: slices.Sorted(maps.Keys(cs.DataSources)),
	}
	for i, w := range index.Resources {
		if err := writeJSON(filepath.Join(tmp, strconv.Itoa(i)+".json"), cs.Resources[w]); err != nil {
			return err
		}
	}
	for i, w := range index.DataSources {
		if err := writeJSON(filepath.Join(tmp, "d"+strconv.Itoa(i)+".json"), cs.DataSources[w]); err != nil {
			return err
		}
	}
	if err := writeJSON(filepath.Join(tmp, "index.json"), index); err != nil {
		return err
	}
	if err := os.Rename(tmp, final); err == nil {
		return nil
	}
	old := tmp + "-old"
	if err := os.Rename(final, old); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	defer os.RemoveAll(old)
	return os.Rename(tmp, final)
}

func writeJSON(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// writeFileAtomic writes data next to path and renames it into place, so a
// reader never sees a partial file.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-"+filepath.Base(path)+"-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// MissingError reports requirements with no lock entry, no binary, or no
// cached schema. Every case has the same fix: install the providers.
type MissingError struct {
	Missing []Requirement
}

func (e *MissingError) Error() string {
	names := make([]string, len(e.Missing))
	for i, r := range e.Missing {
		names[i] = r.Source.String() + " " + r.Version
	}
	return "providers not installed: " + strings.Join(names, ", ") + "; install them online"
}

// Schemas reads the cached schema of every requirement from disk. It never
// starts a provider, so reading schemas needs no provider process and no
// credentials. Each resource type is read when it is first looked up; a
// type that fails to read is reported by the provider schema's Err.
func Schemas(dirs Dirs, lock *Lock, reqs []Requirement) (map[schema.Source]*schema.Provider, error) {
	out, _, err := readSchemas(dirs, lock, reqs, false)
	return out, err
}

func readSchemas(dirs Dirs, lock *Lock, reqs []Requirement, fromCache bool) (map[schema.Source]*schema.Provider, []LockEntry, error) {
	out := make(map[schema.Source]*schema.Provider, len(reqs))
	var unpinned []LockEntry
	missing := &MissingError{}
	for _, req := range reqs {
		entry, ok := lock.Find(req.Source, req.Version)
		pinned := ok
		if !ok && fromCache {
			entry, ok = cachedEntry(dirs, req)
		}
		if !ok {
			missing.Missing = append(missing.Missing, req)
			continue
		}
		c, err := openSchemaCache(dirs, entry.Binary)
		if errors.Is(err, errNoSchema) {
			missing.Missing = append(missing.Missing, req)
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		p, _, err := c.build(req.Source, req.Version)
		if err != nil {
			return nil, nil, err
		}
		out[req.Source] = p
		if !pinned {
			unpinned = append(unpinned, entry)
		}
	}
	if len(missing.Missing) > 0 {
		return nil, nil, missing
	}
	return out, unpinned, nil
}

// cachedEntry is the pin of req's cached binary on this platform, from the
// hashes recorded when it was extracted, or false when there is no such
// binary or it has changed since.
func cachedEntry(dirs Dirs, req Requirement) (LockEntry, bool) {
	if checkPathPart("version", req.Version) != nil {
		return LockEntry{}, false
	}
	platform := CurrentPlatform()
	archive, binary, err := cachedBinary(dirs.BinaryPath(req.Source, req.Version, platform), slog.New(slog.DiscardHandler))
	if err != nil {
		return LockEntry{}, false
	}
	return LockEntry{Source: req.Source, Version: req.Version, Platform: platform, Archive: archive, Binary: binary}, true
}
