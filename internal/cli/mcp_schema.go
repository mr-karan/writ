package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"

	"github.com/mr-karan/writ/internal/diag"
	"github.com/mr-karan/writ/internal/importer"
	"github.com/mr-karan/writ/internal/mcp"
	"github.com/mr-karan/writ/internal/provider"
	"github.com/mr-karan/writ/internal/schema"
	"github.com/mr-karan/writ/internal/server"
	"github.com/mr-karan/writ/internal/tofu"
)

// providerSchemaFormat is the document of the schema tool.
const providerSchemaFormat = "writ.provider-schema/v0"

type providerSchemaReport struct {
	Format    string               `json:"format"`
	OK        bool                 `json:"ok"`
	Providers []schemaProviderInfo `json:"providers"`
	Types     []schemaTypeInfo     `json:"types"`
	Lists     []schemaTypeList     `json:"lists"`
	// Diagnostics are server.Diagnostic: the tool has no file positions.
	Diagnostics []server.Diagnostic `json:"diagnostics"`
}

type schemaProviderInfo struct {
	Source  string `json:"source"`
	Version string `json:"version"`
}

// schemaTypeInfo is one type the schema tool looked up.
type schemaTypeInfo struct {
	// Type is the resource type, or data.NAME for a data source.
	Type     string               `json:"type"`
	Kind     string               `json:"kind"`
	Provider string               `json:"provider"`
	Schema   *provider.TypeSchema `json:"schema"`
}

// schemaTypeList is the types of one provider, which a provider's name
// asks for.
type schemaTypeList struct {
	Provider    string   `json:"provider"`
	Resources   []string `json:"resources"`
	DataSources []string `json:"data_sources"`
}

// Schema looks up types in the cached schemas of the providers the stack's
// .terraform.lock.hcl pins. The lock is read through the os.Root.
func (t *mcpTools) Schema(ctx context.Context, in mcp.SchemaInput) (mcp.Result, error) {
	for _, f := range in.Fields {
		if _, err := path.Match(f, ""); err != nil {
			return mcp.Result{}, usagef("field pattern %q: %v", f, err)
		}
	}
	app, _, err := t.app(in.Dir, "")
	if err != nil {
		return mcp.Result{}, err
	}
	lockText, err := t.base.Files.ReadFile(app.path(tofu.LockFile))
	if errors.Is(err, fs.ErrNotExist) {
		return mcp.Result{}, usagef("%s has no %s: run tofu init in the stack, which writes it, and commit it", app.path("."), tofu.LockFile)
	}
	if err != nil {
		return mcp.Result{}, err
	}
	locked, err := tofu.LockedProviders(lockText)
	if err != nil {
		return mcp.Result{}, usagef("%v", err)
	}
	reqs, err := importer.TofuRequirements(app.opts.Dir, locked)
	if err != nil {
		return mcp.Result{}, usagef("%v", err)
	}
	dirs, err := app.dirs()
	if err != nil {
		return mcp.Result{}, err
	}
	lock, err := provider.Install(ctx, provider.InstallOptions{Dirs: dirs, Log: app.opts.Log, Offline: !t.allowProviders}, nil, reqs)
	if err != nil {
		if !t.allowProviders {
			return mcp.Result{}, usagef("%v; the schema tool reads only Writ's provider cache at %s, checked against %s: ask the person who runs writ mcp to fill it, or to add --allow-providers, which downloads a missing provider", err, dirs.Cache, tofu.LockFile)
		}
		return mcp.Result{}, err
	}
	rep := providerSchemaReport{Format: providerSchemaFormat, OK: true, Providers: []schemaProviderInfo{}, Types: []schemaTypeInfo{}, Lists: []schemaTypeList{}, Diagnostics: []server.Diagnostic{}}
	schemas := make([]*provider.CachedSchema, len(reqs))
	for i, req := range reqs {
		if schemas[i], err = provider.OpenCachedSchema(dirs, lock, req); err != nil {
			return mcp.Result{}, err
		}
		rep.Providers = append(rep.Providers, schemaProviderInfo{Source: fullSource(req.Source), Version: req.Version})
	}
	for _, name := range in.Types {
		found, err := lookupType(&rep, name, reqs, schemas, in)
		if err != nil {
			return mcp.Result{}, err
		}
		if !found {
			rep.OK = false
			rep.Diagnostics = append(rep.Diagnostics, server.Diagnostic{Severity: "error", Class: string(diag.Usage),
				Message: fmt.Sprintf("no provider that %s pins has %s; the providers are %s", tofu.LockFile, schemaWhat(name), providerNames(reqs))})
		}
	}
	res, err := reencode(rep)
	res.Failed = !rep.OK
	return res, err
}

// lookupType adds name to rep from the first provider that has it: a
// provider's list of types for its name, else the resource type, or the
// data source of data.NAME.
func lookupType(rep *providerSchemaReport, name string, reqs []provider.Requirement, schemas []*provider.CachedSchema, in mcp.SchemaInput) (bool, error) {
	wire, data := strings.CutPrefix(name, "data.")
	for i, s := range schemas {
		src := fullSource(reqs[i].Source)
		var ts *provider.TypeSchema
		var err error
		kind := "resource"
		switch {
		case !data && wire == reqs[i].Source.Type:
			rep.Lists = append(rep.Lists, schemaTypeList{Provider: src, Resources: s.Resources(), DataSources: s.DataSources()})
			return true, nil
		case !data && s.HasResource(wire):
			ts, err = s.Resource(wire)
		case data && s.HasDataSource(wire):
			ts, err = s.DataSource(wire)
			kind = "data_source"
		default:
			continue
		}
		if err != nil {
			return false, err
		}
		keepFields(&ts.Block, in.Fields, in.RequiredOnly)
		rep.Types = append(rep.Types, schemaTypeInfo{Type: name, Kind: kind, Provider: src, Schema: ts})
		return true, nil
	}
	return false, nil
}

// fullSource is a provider's address as .terraform.lock.hcl writes it,
// with its registry's host.
func fullSource(src schema.Source) string {
	if src.Host == "" {
		src.Host = schema.DefaultHost
	}
	return src.String()
}

func schemaWhat(name string) string {
	if wire, ok := strings.CutPrefix(name, "data."); ok {
		return "the data source " + wire
	}
	return "the resource type " + name
}

func providerNames(reqs []provider.Requirement) string {
	names := make([]string, len(reqs))
	for i, r := range reqs {
		names[i] = r.Source.Type + " (" + fullSource(r.Source) + " " + r.Version + ")"
	}
	return strings.Join(names, ", ")
}

// keepFields keeps, of a type's top-level attributes and nested blocks,
// those whose names match a pattern, all of them when there is none, and
// with requiredOnly only the required attributes and the blocks that need
// an item.
func keepFields(b *provider.SchemaBlock, patterns []string, requiredOnly bool) {
	match := func(name string) bool {
		if len(patterns) == 0 {
			return true
		}
		return slices.ContainsFunc(patterns, func(p string) bool { ok, _ := path.Match(p, name); return ok })
	}
	for name, a := range b.Attributes {
		if !match(name) || requiredOnly && !a.Required {
			delete(b.Attributes, name)
		}
	}
	for name, bt := range b.BlockTypes {
		if !match(name) || requiredOnly && bt.MinItems == 0 {
			delete(b.BlockTypes, name)
		}
	}
}
