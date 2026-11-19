package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/mr-karan/writ/internal/tfplugin5"
	"github.com/mr-karan/writ/internal/tfplugin6"
)

type schemaServer5 struct {
	tfplugin5.UnimplementedProviderServer
	resp *tfplugin5.GetProviderSchema_Response
}

func (s schemaServer5) GetSchema(context.Context, *tfplugin5.GetProviderSchema_Request) (*tfplugin5.GetProviderSchema_Response, error) {
	return s.resp, nil
}

type schemaServer6 struct {
	tfplugin6.UnimplementedProviderServer
	resp *tfplugin6.GetProviderSchema_Response
}

func (s schemaServer6) GetProviderSchema(context.Context, *tfplugin6.GetProviderSchema_Request) (*tfplugin6.GetProviderSchema_Response, error) {
	return s.resp, nil
}

// serveSchema serves one GetProviderSchema answer on loopback and returns
// the adapter of resp's protocol connected to it.
func serveSchema(t *testing.T, resp any) protocol {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	version := 5
	switch r := resp.(type) {
	case *tfplugin5.GetProviderSchema_Response:
		tfplugin5.RegisterProviderServer(srv, schemaServer5{resp: r})
	case *tfplugin6.GetProviderSchema_Response:
		tfplugin6.RegisterProviderServer(srv, schemaServer6{resp: r})
		version = 6
	}
	go srv.Serve(lis) //nolint:errcheck // Serve returns when the test stops it
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///"+lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return (&process{protocol: version, conn: conn}).client()
}

func readSchema(t *testing.T, p protocol) (*cachedSchema, []wireDiag) {
	t.Helper()
	reply, err := p.getSchema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return reply.schema, reply.diags
}

// cacheFiles writes cs as the schema cache does and returns each file's
// bytes by name.
func cacheFiles(t *testing.T, cs *cachedSchema) map[string]string {
	t.Helper()
	dirs := Dirs{Cache: t.TempDir()}
	if err := writeSchemaCache(dirs, hash(2), cs); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dirs.SchemaDir(hash(2)))
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dirs.SchemaDir(hash(2)), e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = string(data)
	}
	return out
}

func sameCache(t *testing.T, got, want *cachedSchema) {
	t.Helper()
	g, w := cacheFiles(t, got), cacheFiles(t, want)
	if len(g) != len(w) {
		t.Errorf("cache has %d files, want %d", len(g), len(w))
	}
	for name, data := range w {
		if g[name] != data {
			t.Errorf("cache file %s:\n got %.300s\nwant %.300s", name, g[name], data)
		}
	}
}

// TestSchemaThroughBothProtocols reads the real schemas of the test
// providers through each protocol: protocol 5 from the provider itself,
// protocol 6 from a stand-in that serves the same schema as protocol 6
// messages. Both must write the cache files of the checked-in fixture.
func TestSchemaThroughBothProtocols(t *testing.T) {
	dirs, lock := installed(t)
	for _, req := range testReqs {
		t.Run(req.Name, func(t *testing.T) {
			data, err := os.ReadFile(fmt.Sprintf("testdata/%s-%s-schema.json", req.Source.Type, req.Version))
			if err != nil {
				t.Fatal(err)
			}
			var want cachedSchema
			if err := json.Unmarshal(data, &want); err != nil {
				t.Fatal(err)
			}
			e, _ := lock.Find(req.Source, req.Version)
			ctx := context.Background()
			proc, err := start(ctx, dirs.BinaryPath(req.Source, req.Version, e.Platform), Launch{OwnEnv: true})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := proc.shutdown(ctx); err != nil {
					t.Error(err)
				}
			})
			raw, err := tfplugin5.NewProviderClient(proc.conn).GetSchema(ctx, &tfplugin5.GetProviderSchema_Request{})
			if err != nil {
				t.Fatal(err)
			}

			got5, diags := readSchema(t, proc.client())
			if len(diags) > 0 {
				t.Errorf("protocol 5 diagnostics: %+v", diags)
			}
			sameCache(t, got5, &want)

			got6, diags := readSchema(t, serveSchema(t, v6Response(raw)))
			if len(diags) > 0 {
				t.Errorf("protocol 6 diagnostics: %+v", diags)
			}
			want.Protocol = 6
			sameCache(t, got6, &want)
		})
	}
}

// v6Response is a protocol 5 schema answer as protocol 6 messages.
func v6Response(r *tfplugin5.GetProviderSchema_Response) *tfplugin6.GetProviderSchema_Response {
	schemas := func(m map[string]*tfplugin5.Schema) map[string]*tfplugin6.Schema {
		out := make(map[string]*tfplugin6.Schema, len(m))
		for name, s := range m {
			out[name] = v6Schema(s)
		}
		return out
	}
	return &tfplugin6.GetProviderSchema_Response{
		Provider:            v6Schema(r.GetProvider()),
		ResourceSchemas:     schemas(r.GetResourceSchemas()),
		DataSourceSchemas:   schemas(r.GetDataSourceSchemas()),
		ListResourceSchemas: schemas(r.GetListResourceSchemas()),
		ServerCapabilities:  &tfplugin6.ServerCapabilities{GetProviderSchemaOptional: r.GetServerCapabilities().GetGetProviderSchemaOptional()},
	}
}

func v6Schema(s *tfplugin5.Schema) *tfplugin6.Schema {
	if s == nil {
		return nil
	}
	return &tfplugin6.Schema{Version: s.GetVersion(), Block: v6SchemaBlock(s.GetBlock())}
}

func v6SchemaBlock(b *tfplugin5.Schema_Block) *tfplugin6.Schema_Block {
	if b == nil {
		return nil
	}
	out := &tfplugin6.Schema_Block{Deprecated: b.GetDeprecated(), DeprecationMessage: b.GetDeprecationMessage()}
	for _, a := range b.GetAttributes() {
		out.Attributes = append(out.Attributes, &tfplugin6.Schema_Attribute{
			Name: a.GetName(), Type: a.GetType(), Required: a.GetRequired(), Optional: a.GetOptional(), Computed: a.GetComputed(),
			Sensitive: a.GetSensitive(), WriteOnly: a.GetWriteOnly(), Deprecated: a.GetDeprecated(), DeprecationMessage: a.GetDeprecationMessage(),
		})
	}
	for _, nb := range b.GetBlockTypes() {
		out.BlockTypes = append(out.BlockTypes, &tfplugin6.Schema_NestedBlock{
			TypeName: nb.GetTypeName(), Nesting: tfplugin6.Schema_NestedBlock_NestingMode(nb.GetNesting()),
			MinItems: nb.GetMinItems(), MaxItems: nb.GetMaxItems(), Block: v6SchemaBlock(nb.GetBlock()),
		})
	}
	return out
}

// TestSchemaConversion reads a schema with every flag and nesting mode
// through both protocols. The rules the cache depends on: nesting modes by
// name, an unknown mode kept by its enum name, a nested attribute type
// without its cty type, and every severity but a warning an error.
func TestSchemaConversion(t *testing.T) {
	str := []byte(`"string"`)
	inner := rawBlock{Attributes: []rawAttribute{{Name: "x", Type: str, Optional: true}}}
	want := rawBlock{
		Deprecated: true, DeprecationMessage: "use another",
		Attributes: []rawAttribute{
			{Name: "a", Type: str, Required: true},
			{Name: "b", Type: str, Optional: true, Computed: true, Sensitive: true},
			{Name: "c", Type: str, Optional: true, WriteOnly: true, Deprecated: true, DeprecationMessage: "gone"},
		},
		Blocks: []rawNestedBlock{
			{Name: "single", Nesting: nestSingle, Block: inner},
			{Name: "group", Nesting: nestGroup, Block: inner},
			{Name: "list", Nesting: nestList, MinItems: 1, MaxItems: 3, Block: inner},
			{Name: "set", Nesting: nestSet, Block: inner},
			{Name: "map", Nesting: nestMap, Block: inner},
			{Name: "invalid", Nesting: "INVALID", Block: inner},
			{Name: "unnamed", Nesting: "9", Block: inner},
		},
	}
	modes := []int32{1, 5, 2, 3, 4, 0, 9}

	inner5 := &tfplugin5.Schema_Block{Attributes: []*tfplugin5.Schema_Attribute{{Name: "x", Type: str, Optional: true}}}
	block5 := &tfplugin5.Schema_Block{
		Deprecated: true, DeprecationMessage: "use another",
		Attributes: []*tfplugin5.Schema_Attribute{
			{Name: "a", Type: str, Required: true},
			{Name: "b", Type: str, Optional: true, Computed: true, Sensitive: true},
			{Name: "c", Type: str, Optional: true, WriteOnly: true, Deprecated: true, DeprecationMessage: "gone"},
		},
	}
	for i, nb := range want.Blocks {
		nested := &tfplugin5.Schema_NestedBlock{TypeName: nb.Name, Nesting: tfplugin5.Schema_NestedBlock_NestingMode(modes[i]), Block: inner5}
		if nb.Name == "list" {
			nested.MinItems, nested.MaxItems = 1, 3
		}
		block5.BlockTypes = append(block5.BlockTypes, nested)
	}
	path5 := &tfplugin5.AttributePath{Steps: []*tfplugin5.AttributePath_Step{
		{Selector: &tfplugin5.AttributePath_Step_AttributeName{AttributeName: "tags"}},
		{Selector: &tfplugin5.AttributePath_Step_ElementKeyString{ElementKeyString: "k"}},
		{Selector: &tfplugin5.AttributePath_Step_ElementKeyInt{ElementKeyInt: 2}},
	}}
	resp5 := &tfplugin5.GetProviderSchema_Response{
		Provider:           &tfplugin5.Schema{Block: inner5},
		ResourceSchemas:    map[string]*tfplugin5.Schema{"fake_thing": {Version: 3, Block: block5}},
		DataSourceSchemas:  map[string]*tfplugin5.Schema{"fake_info": {Block: inner5}},
		ServerCapabilities: &tfplugin5.ServerCapabilities{GetProviderSchemaOptional: true},
		Diagnostics: []*tfplugin5.Diagnostic{
			{Severity: tfplugin5.Diagnostic_ERROR, Summary: "e", Detail: "ed", Attribute: path5},
			{Severity: tfplugin5.Diagnostic_WARNING, Summary: "w"},
			{Severity: tfplugin5.Diagnostic_INVALID, Summary: "i"},
		},
	}
	wantDiags := []wireDiag{
		{error: true, summary: "e", detail: "ed", path: []wireStep{{kind: stepAttr, name: "tags"}, {kind: stepKey, name: "k"}, {kind: stepIndex, index: 2}}},
		{error: false, summary: "w"},
		{error: true, summary: "i"},
	}
	wantSchema := func(protocol int, resource rawBlock) *cachedSchema {
		return &cachedSchema{
			Protocol: protocol, GetSchemaOptional: true, Provider: inner,
			Resources:   map[string]rawResource{"fake_thing": {Version: 3, Block: resource}},
			DataSources: map[string]rawResource{"fake_info": {Block: inner}},
		}
	}

	t.Run("protocol 5", func(t *testing.T) {
		got, diags := readSchema(t, serveSchema(t, resp5))
		if want := wantSchema(5, want); !reflect.DeepEqual(got, want) {
			t.Errorf("schema:\n got %+v\nwant %+v", got, want)
		}
		if !reflect.DeepEqual(diags, wantDiags) {
			t.Errorf("diagnostics:\n got %+v\nwant %+v", diags, wantDiags)
		}
	})

	t.Run("protocol 6", func(t *testing.T) {
		resp6 := v6Response(resp5)
		resp6.Diagnostics = []*tfplugin6.Diagnostic{
			{Severity: tfplugin6.Diagnostic_ERROR, Summary: "e", Detail: "ed", Attribute: &tfplugin6.AttributePath{Steps: []*tfplugin6.AttributePath_Step{
				{Selector: &tfplugin6.AttributePath_Step_AttributeName{AttributeName: "tags"}},
				{Selector: &tfplugin6.AttributePath_Step_ElementKeyString{ElementKeyString: "k"}},
				{Selector: &tfplugin6.AttributePath_Step_ElementKeyInt{ElementKeyInt: 2}},
			}}},
			{Severity: tfplugin6.Diagnostic_WARNING, Summary: "w"},
			{Severity: tfplugin6.Diagnostic_INVALID, Summary: "i"},
		}
		x := &tfplugin6.Schema_Attribute{Name: "x", Type: str, Optional: true}
		object := func(mode int32, attrs ...*tfplugin6.Schema_Attribute) *tfplugin6.Schema_Object {
			return &tfplugin6.Schema_Object{Nesting: tfplugin6.Schema_Object_NestingMode(mode), Attributes: attrs}
		}
		resp6.ResourceSchemas["fake_thing"].Block.Attributes = append(resp6.ResourceSchemas["fake_thing"].Block.Attributes,
			&tfplugin6.Schema_Attribute{Name: "n_single", NestedType: object(1, x), Optional: true},
			&tfplugin6.Schema_Attribute{Name: "n_list", NestedType: object(2, x), Computed: true, Sensitive: true},
			&tfplugin6.Schema_Attribute{Name: "n_set", NestedType: object(3, x), Required: true},
			&tfplugin6.Schema_Attribute{Name: "n_map", NestedType: object(4, &tfplugin6.Schema_Attribute{Name: "deep", NestedType: object(2, x), Optional: true}), Optional: true},
			&tfplugin6.Schema_Attribute{Name: "n_invalid", NestedType: object(0, x), Optional: true},
			&tfplugin6.Schema_Attribute{Name: "n_typed", Type: str, NestedType: object(1, x), Optional: true},
		)
		xs := []rawAttribute{{Name: "x", Type: str, Optional: true}}
		resource := want
		resource.Attributes = append(append([]rawAttribute(nil), want.Attributes...),
			rawAttribute{Name: "n_single", Nested: &rawObject{Nesting: nestSingle, Attributes: xs}, Optional: true},
			rawAttribute{Name: "n_list", Nested: &rawObject{Nesting: nestList, Attributes: xs}, Computed: true, Sensitive: true},
			rawAttribute{Name: "n_set", Nested: &rawObject{Nesting: nestSet, Attributes: xs}, Required: true},
			rawAttribute{Name: "n_map", Nested: &rawObject{Nesting: nestMap, Attributes: []rawAttribute{{Name: "deep", Nested: &rawObject{Nesting: nestList, Attributes: xs}, Optional: true}}}, Optional: true},
			rawAttribute{Name: "n_invalid", Nested: &rawObject{Nesting: "INVALID", Attributes: xs}, Optional: true},
			rawAttribute{Name: "n_typed", Nested: &rawObject{Nesting: nestSingle, Attributes: xs}, Optional: true},
		)

		got, diags := readSchema(t, serveSchema(t, resp6))
		if want := wantSchema(6, resource); !reflect.DeepEqual(got, want) {
			t.Errorf("schema:\n got %+v\nwant %+v", got, want)
		}
		if !reflect.DeepEqual(diags, wantDiags) {
			t.Errorf("diagnostics:\n got %+v\nwant %+v", diags, wantDiags)
		}
	})
}
