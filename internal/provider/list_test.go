package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zclconf/go-cty/cty"
	ctymsgpack "github.com/zclconf/go-cty/cty/msgpack"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/mr-karan/writ/internal/schema"
	"github.com/mr-karan/writ/internal/tfplugin5"
	"github.com/mr-karan/writ/internal/tfplugin6"
	"github.com/mr-karan/writ/internal/value"
)

// These tests run Client.List against a stand-in provider that serves the
// real plugin protocol over gRPC on loopback, in version 5 and version 6.
// hashicorp/random, tls, and local serve no list resources.

// listQueryType is the cty type of the stand-in's list config for
// fake_thing: an attribute and a list block, as hashicorp/aws gives
// aws_subnet a region and a filter block.
var listQueryType = cty.Object(map[string]cty.Type{
	"tag":    cty.String,
	"filter": cty.List(cty.Object(map[string]cty.Type{"name": cty.String, "values": cty.List(cty.String)})),
})

var thingIdentityType = cty.Object(map[string]cty.Type{"id": cty.String, "region": cty.String})

// listStandIn is the behavior both protocol servers share.
type listStandIn struct {
	ids []string
	// overrun sends every id, ignoring the limit, as a provider that
	// does not honor it would.
	overrun bool
	// failAt sends an error diagnostic in place of the event at this
	// index, when it is at least 0.
	failAt int
	// nullAt sends a null identity at this index, when it is at least 1.
	nullAt int
	// warnings sends this many events that hold only a warning of about
	// 1 KiB, before the first id.
	warnings int
	// bigID replaces the first id with one of 1 MiB.
	bigID bool
	// warnKey, when set, puts each warning under the path tag[warnKey].
	warnKey string
	// hold keeps the stream open after the last event until the client
	// cancels it, and then closes released.
	hold     bool
	released chan struct{}

	mu        sync.Mutex
	config    cty.Value
	limit     int64
	objects   bool
	calls     int
	validates int
}

// standInEvent is one ListResource event, before either protocol encodes
// it: an identity, a warning, or an error.
type standInEvent struct {
	identity []byte
	warning  string
	warnKey  string
	fail     bool
}

// validate answers ValidateListResourceConfig: each filter needs a name,
// as the schema requires. It returns the index of the first filter with
// none, or -1.
func (s *listStandIn) validate(typ string, config, objects, limit []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.validates++
	if typ != "fake_thing" {
		return -1, fmt.Errorf("unexpected list type %s", typ)
	}
	if o, err := ctymsgpack.Unmarshal(objects, cty.Bool); err != nil || o.True() {
		return -1, fmt.Errorf("include_resource_object = %#v, %v; want false", o, err)
	}
	if l, err := ctymsgpack.Unmarshal(limit, cty.Number); err != nil || l.IsNull() {
		return -1, fmt.Errorf("limit = %#v, %v; want a number", l, err)
	}
	v, err := ctymsgpack.Unmarshal(config, listQueryType)
	if err != nil {
		return -1, err
	}
	for i, f := range v.GetAttr("filter").AsValueSlice() {
		if f.GetAttr("name").IsNull() {
			return i, nil
		}
	}
	return -1, nil
}

func (s *listStandIn) record(typ string, config []byte, limit int64, objects bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if typ != "fake_thing" {
		return fmt.Errorf("unexpected list type %s", typ)
	}
	v, err := ctymsgpack.Unmarshal(config, listQueryType)
	if err != nil {
		return err
	}
	s.config, s.limit, s.objects = v, limit, objects
	return nil
}

// events returns the events to send.
func (s *listStandIn) events(limit int64) ([]standInEvent, error) {
	var out []standInEvent
	for range s.warnings {
		out = append(out, standInEvent{warning: strings.Repeat("w", 1024), warnKey: s.warnKey})
	}
	ids := s.ids
	if !s.overrun && int64(len(ids)) > limit {
		ids = ids[:limit]
	}
	for i, id := range ids {
		if i == 0 && s.bigID {
			id = strings.Repeat("x", 1<<20)
		}
		v := cty.ObjectVal(map[string]cty.Value{"id": cty.StringVal(id), "region": cty.StringVal("test-region-1")})
		if s.nullAt > 0 && i == s.nullAt {
			v = cty.NullVal(thingIdentityType)
		}
		b, err := ctymsgpack.Marshal(v, thingIdentityType)
		if err != nil {
			return nil, err
		}
		out = append(out, standInEvent{identity: b, fail: i == s.failAt})
	}
	return out, nil
}

func ctyTypeJSON(t cty.Type) []byte {
	b, err := t.MarshalJSON()
	if err != nil {
		panic(err)
	}
	return b
}

type listServer5 struct {
	tfplugin5.UnimplementedProviderServer
	s *listStandIn
}

func (l listServer5) GetSchema(context.Context, *tfplugin5.GetProviderSchema_Request) (*tfplugin5.GetProviderSchema_Response, error) {
	str := ctyTypeJSON(cty.String)
	block := func(attrs ...string) *tfplugin5.Schema {
		b := &tfplugin5.Schema_Block{}
		for _, a := range attrs {
			b.Attributes = append(b.Attributes, &tfplugin5.Schema_Attribute{Name: a, Type: str, Optional: true})
		}
		return &tfplugin5.Schema{Block: b}
	}
	thing := block("tag")
	thing.Block.BlockTypes = []*tfplugin5.Schema_NestedBlock{{
		TypeName: "filter", Nesting: tfplugin5.Schema_NestedBlock_LIST,
		Block: &tfplugin5.Schema_Block{Attributes: []*tfplugin5.Schema_Attribute{
			{Name: "name", Type: str, Required: true},
			{Name: "values", Type: ctyTypeJSON(cty.List(cty.String)), Required: true},
		}},
	}}
	return &tfplugin5.GetProviderSchema_Response{
		Provider:            block(),
		ResourceSchemas:     map[string]*tfplugin5.Schema{"fake_thing": block("id"), "fake_pair": block("id"), "fake_plain": block("id")},
		ListResourceSchemas: map[string]*tfplugin5.Schema{"fake_thing": thing, "fake_pair": block()},
	}, nil
}

func (l listServer5) GetResourceIdentitySchemas(context.Context, *tfplugin5.GetResourceIdentitySchemas_Request) (*tfplugin5.GetResourceIdentitySchemas_Response, error) {
	str := ctyTypeJSON(cty.String)
	return &tfplugin5.GetResourceIdentitySchemas_Response{IdentitySchemas: map[string]*tfplugin5.ResourceIdentitySchema{
		"fake_thing": {IdentityAttributes: []*tfplugin5.ResourceIdentitySchema_IdentityAttribute{
			{Name: "id", Type: str, RequiredForImport: true},
			{Name: "region", Type: str, OptionalForImport: true},
		}},
		"fake_pair": {IdentityAttributes: []*tfplugin5.ResourceIdentitySchema_IdentityAttribute{
			{Name: "left", Type: str, RequiredForImport: true},
			{Name: "right", Type: str, RequiredForImport: true},
		}},
	}}, nil
}

func (l listServer5) ValidateListResourceConfig(_ context.Context, req *tfplugin5.ValidateListResourceConfig_Request) (*tfplugin5.ValidateListResourceConfig_Response, error) {
	i, err := l.s.validate(req.GetTypeName(), req.GetConfig().GetMsgpack(), req.GetIncludeResourceObject().GetMsgpack(), req.GetLimit().GetMsgpack())
	if err != nil || i < 0 {
		return &tfplugin5.ValidateListResourceConfig_Response{}, err
	}
	path := &tfplugin5.AttributePath{Steps: []*tfplugin5.AttributePath_Step{
		{Selector: &tfplugin5.AttributePath_Step_AttributeName{AttributeName: "filter"}},
		{Selector: &tfplugin5.AttributePath_Step_ElementKeyInt{ElementKeyInt: int64(i)}},
		{Selector: &tfplugin5.AttributePath_Step_AttributeName{AttributeName: "name"}},
	}}
	return &tfplugin5.ValidateListResourceConfig_Response{Diagnostics: []*tfplugin5.Diagnostic{
		{Severity: tfplugin5.Diagnostic_ERROR, Summary: "Missing required argument", Detail: "a filter needs a name", Attribute: path},
	}}, nil
}

func (l listServer5) ListResource(req *tfplugin5.ListResource_Request, stream grpc.ServerStreamingServer[tfplugin5.ListResource_Event]) error {
	if err := l.s.record(req.GetTypeName(), req.GetConfig().GetMsgpack(), req.GetLimit(), req.GetIncludeResourceObject()); err != nil {
		return err
	}
	evs, err := l.s.events(req.GetLimit())
	if err != nil {
		return err
	}
	for _, ev := range evs {
		e := &tfplugin5.ListResource_Event{Identity: &tfplugin5.ResourceIdentityData{IdentityData: &tfplugin5.DynamicValue{Msgpack: ev.identity}}, DisplayName: "thing"}
		switch {
		case ev.fail:
			e = &tfplugin5.ListResource_Event{Diagnostic: []*tfplugin5.Diagnostic{{Severity: tfplugin5.Diagnostic_ERROR, Summary: "listing failed", Detail: "the stand-in refused page 2"}}}
		case ev.warning != "":
			d := &tfplugin5.Diagnostic{Severity: tfplugin5.Diagnostic_WARNING, Summary: "slow page", Detail: ev.warning}
			if ev.warnKey != "" {
				d.Attribute = &tfplugin5.AttributePath{Steps: []*tfplugin5.AttributePath_Step{
					{Selector: &tfplugin5.AttributePath_Step_AttributeName{AttributeName: "tag"}},
					{Selector: &tfplugin5.AttributePath_Step_ElementKeyString{ElementKeyString: ev.warnKey}},
				}}
			}
			e = &tfplugin5.ListResource_Event{Diagnostic: []*tfplugin5.Diagnostic{d}}
		}
		if err := stream.Send(e); err != nil {
			return err
		}
	}
	if l.s.hold {
		<-stream.Context().Done()
		close(l.s.released)
	}
	return nil
}

type listServer6 struct {
	tfplugin6.UnimplementedProviderServer
	s *listStandIn
}

func (l listServer6) GetProviderSchema(context.Context, *tfplugin6.GetProviderSchema_Request) (*tfplugin6.GetProviderSchema_Response, error) {
	str := ctyTypeJSON(cty.String)
	block := func(attrs ...string) *tfplugin6.Schema {
		b := &tfplugin6.Schema_Block{}
		for _, a := range attrs {
			b.Attributes = append(b.Attributes, &tfplugin6.Schema_Attribute{Name: a, Type: str, Optional: true})
		}
		return &tfplugin6.Schema{Block: b}
	}
	thing := block("tag")
	thing.Block.BlockTypes = []*tfplugin6.Schema_NestedBlock{{
		TypeName: "filter", Nesting: tfplugin6.Schema_NestedBlock_LIST,
		Block: &tfplugin6.Schema_Block{Attributes: []*tfplugin6.Schema_Attribute{
			{Name: "name", Type: str, Required: true},
			{Name: "values", Type: ctyTypeJSON(cty.List(cty.String)), Required: true},
		}},
	}}
	return &tfplugin6.GetProviderSchema_Response{
		Provider:            block(),
		ResourceSchemas:     map[string]*tfplugin6.Schema{"fake_thing": block("id"), "fake_pair": block("id"), "fake_plain": block("id")},
		ListResourceSchemas: map[string]*tfplugin6.Schema{"fake_thing": thing, "fake_pair": block()},
	}, nil
}

func (l listServer6) GetResourceIdentitySchemas(context.Context, *tfplugin6.GetResourceIdentitySchemas_Request) (*tfplugin6.GetResourceIdentitySchemas_Response, error) {
	str := ctyTypeJSON(cty.String)
	return &tfplugin6.GetResourceIdentitySchemas_Response{IdentitySchemas: map[string]*tfplugin6.ResourceIdentitySchema{
		"fake_thing": {IdentityAttributes: []*tfplugin6.ResourceIdentitySchema_IdentityAttribute{
			{Name: "id", Type: str, RequiredForImport: true},
			{Name: "region", Type: str, OptionalForImport: true},
		}},
		"fake_pair": {IdentityAttributes: []*tfplugin6.ResourceIdentitySchema_IdentityAttribute{
			{Name: "left", Type: str, RequiredForImport: true},
			{Name: "right", Type: str, RequiredForImport: true},
		}},
	}}, nil
}

func (l listServer6) ValidateListResourceConfig(_ context.Context, req *tfplugin6.ValidateListResourceConfig_Request) (*tfplugin6.ValidateListResourceConfig_Response, error) {
	i, err := l.s.validate(req.GetTypeName(), req.GetConfig().GetMsgpack(), req.GetIncludeResourceObject().GetMsgpack(), req.GetLimit().GetMsgpack())
	if err != nil || i < 0 {
		return &tfplugin6.ValidateListResourceConfig_Response{}, err
	}
	path := &tfplugin6.AttributePath{Steps: []*tfplugin6.AttributePath_Step{
		{Selector: &tfplugin6.AttributePath_Step_AttributeName{AttributeName: "filter"}},
		{Selector: &tfplugin6.AttributePath_Step_ElementKeyInt{ElementKeyInt: int64(i)}},
		{Selector: &tfplugin6.AttributePath_Step_AttributeName{AttributeName: "name"}},
	}}
	return &tfplugin6.ValidateListResourceConfig_Response{Diagnostics: []*tfplugin6.Diagnostic{
		{Severity: tfplugin6.Diagnostic_ERROR, Summary: "Missing required argument", Detail: "a filter needs a name", Attribute: path},
	}}, nil
}

func (l listServer6) ListResource(req *tfplugin6.ListResource_Request, stream grpc.ServerStreamingServer[tfplugin6.ListResource_Event]) error {
	if err := l.s.record(req.GetTypeName(), req.GetConfig().GetMsgpack(), req.GetLimit(), req.GetIncludeResourceObject()); err != nil {
		return err
	}
	evs, err := l.s.events(req.GetLimit())
	if err != nil {
		return err
	}
	for _, ev := range evs {
		e := &tfplugin6.ListResource_Event{Identity: &tfplugin6.ResourceIdentityData{IdentityData: &tfplugin6.DynamicValue{Msgpack: ev.identity}}, DisplayName: "thing"}
		switch {
		case ev.fail:
			e = &tfplugin6.ListResource_Event{Diagnostic: []*tfplugin6.Diagnostic{{Severity: tfplugin6.Diagnostic_ERROR, Summary: "listing failed", Detail: "the stand-in refused page 2"}}}
		case ev.warning != "":
			d := &tfplugin6.Diagnostic{Severity: tfplugin6.Diagnostic_WARNING, Summary: "slow page", Detail: ev.warning}
			if ev.warnKey != "" {
				d.Attribute = &tfplugin6.AttributePath{Steps: []*tfplugin6.AttributePath_Step{
					{Selector: &tfplugin6.AttributePath_Step_AttributeName{AttributeName: "tag"}},
					{Selector: &tfplugin6.AttributePath_Step_ElementKeyString{ElementKeyString: ev.warnKey}},
				}}
			}
			e = &tfplugin6.ListResource_Event{Diagnostic: []*tfplugin6.Diagnostic{d}}
		}
		if err := stream.Send(e); err != nil {
			return err
		}
	}
	if l.s.hold {
		<-stream.Context().Done()
		close(l.s.released)
	}
	return nil
}

// serveList starts the stand-in on loopback in the given protocol version
// and returns a Client connected to it.
func serveList(t *testing.T, version int, s *listStandIn) *Client {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	if version == 5 {
		tfplugin5.RegisterProviderServer(srv, listServer5{s: s})
	} else {
		tfplugin6.RegisterProviderServer(srv, listServer6{s: s})
	}
	go srv.Serve(lis) //nolint:errcheck // Serve returns when the test stops it
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///"+lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	proc := &process{protocol: version, conn: conn, stderr: &tailBuffer{max: 1024}}
	return &Client{src: schema.Source{Namespace: "example", Type: "fake"}, proc: proc, proto: proc.client()}
}

func thingIDs(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("thing-%04d", i+1)
	}
	return out
}

func TestClientList(t *testing.T) {
	query := value.ObjectVal(map[string]value.Value{
		"filter": value.ListVal([]value.Value{value.ObjectVal(map[string]value.Value{
			"name": value.StringVal("vpc-id"), "values": value.ListVal([]value.Value{value.StringVal("vpc-00000000000000001")}),
		})}),
	})
	for _, version := range []int{5, 6} {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			t.Run("lists the import ids", func(t *testing.T) {
				s := &listStandIn{ids: thingIDs(3), failAt: -1}
				c := serveList(t, version, s)
				ids, _, err := c.List(context.Background(), "fake_thing", query, 10)
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(ids, thingIDs(3)) {
					t.Errorf("ids = %v, want %v", ids, thingIDs(3))
				}
				if s.limit != int64(listRequestLimit(10)) || s.objects {
					t.Errorf("limit %d, include_resource_object %v; want %d and false", s.limit, s.objects, listRequestLimit(10))
				}
				f := s.config.GetAttr("filter").Index(cty.NumberIntVal(0))
				if f.GetAttr("name").AsString() != "vpc-id" || !s.config.GetAttr("tag").IsNull() {
					t.Errorf("config = %#v, want the query's filter and a null tag", s.config)
				}
			})
			t.Run("a null query is an empty config", func(t *testing.T) {
				s := &listStandIn{ids: thingIDs(1), failAt: -1}
				c := serveList(t, version, s)
				if _, _, err := c.List(context.Background(), "fake_thing", value.NullVal(), 10); err != nil {
					t.Fatal(err)
				}
				if s.config.IsNull() || s.config.GetAttr("filter").LengthInt() != 0 {
					t.Errorf("config = %#v, want an object with no filter", s.config)
				}
			})
			t.Run("stops at the limit when the provider sends more", func(t *testing.T) {
				s := &listStandIn{ids: thingIDs(50), overrun: true, failAt: -1}
				c := serveList(t, version, s)
				ids, _, err := c.List(context.Background(), "fake_thing", query, 4)
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(ids, thingIDs(4)) {
					t.Errorf("ids = %v, want the first 4", ids)
				}
			})
			t.Run("a type with no list resource", func(t *testing.T) {
				s := &listStandIn{failAt: -1}
				c := serveList(t, version, s)
				_, _, err := c.List(context.Background(), "fake_plain", query, 10)
				nl, ok := errors.AsType[*NoListError](err)
				if !ok || !strings.Contains(nl.Error(), "fake_plain") || s.calls != 0 {
					t.Fatalf("err = %v, calls %d; want a NoListError that names fake_plain and no ListResource call", err, s.calls)
				}
			})
			t.Run("an identity of two attributes", func(t *testing.T) {
				s := &listStandIn{failAt: -1}
				c := serveList(t, version, s)
				_, _, err := c.List(context.Background(), "fake_pair", value.NullVal(), 10)
				nl, ok := errors.AsType[*NoListError](err)
				if !ok || !strings.Contains(nl.Error(), "left, right") || s.calls != 0 {
					t.Fatalf("err = %v, calls %d; want a NoListError that names both identity attributes", err, s.calls)
				}
			})
			t.Run("a query attribute the list config does not have", func(t *testing.T) {
				s := &listStandIn{failAt: -1}
				c := serveList(t, version, s)
				bad := value.ObjectVal(map[string]value.Value{"vpc_id": value.StringVal("vpc-00000000000000001")})
				_, _, err := c.List(context.Background(), "fake_thing", bad, 10)
				if err == nil || !strings.Contains(err.Error(), `"vpc_id"`) || s.calls != 0 {
					t.Fatalf("err = %v, calls %d; want a refusal that names vpc_id before any call", err, s.calls)
				}
			})
			t.Run("the provider validates the query first", func(t *testing.T) {
				s := &listStandIn{ids: thingIDs(3), failAt: -1}
				c := serveList(t, version, s)
				bad := value.ObjectVal(map[string]value.Value{"filter": value.ListVal([]value.Value{value.ObjectVal(map[string]value.Value{
					"values": value.ListVal([]value.Value{value.StringVal("web")}),
				})})})
				ids, _, err := c.List(context.Background(), "fake_thing", bad, 10)
				pe, ok := errors.AsType[*Error](err)
				if !ok || pe.Op != "ValidateListResourceConfig" || ids != nil || s.validates != 1 || s.calls != 0 {
					t.Fatalf("ids %v, err = %v, validates %d, calls %d; want a validation error and no ListResource call", ids, err, s.validates, s.calls)
				}
				if !strings.Contains(err.Error(), "filter[0].name") || !strings.Contains(err.Error(), "a filter needs a name") {
					t.Errorf("err = %v, want the path and the provider's text", err)
				}
			})
			t.Run("a valid query is validated once", func(t *testing.T) {
				s := &listStandIn{ids: thingIDs(1), failAt: -1}
				c := serveList(t, version, s)
				if _, _, err := c.List(context.Background(), "fake_thing", query, 10); err != nil || s.validates != 1 || s.calls != 1 {
					t.Fatalf("err = %v, validates %d, calls %d; want one of each", err, s.validates, s.calls)
				}
			})
			t.Run("warnings are bounded and counted", func(t *testing.T) {
				s := &listStandIn{ids: thingIDs(1), warnings: 10000, failAt: -1}
				c := serveList(t, version, s)
				ids, warnings, err := c.List(context.Background(), "fake_thing", query, 1)
				if err != nil || len(ids) != 1 {
					t.Fatalf("ids %v, err %v; want one id", ids, err)
				}
				if len(warnings) > maxListWarnings+1 {
					t.Fatalf("kept %d warnings, want at most %d and a count", len(warnings), maxListWarnings)
				}
				kept := 0
				for _, w := range warnings[:len(warnings)-1] {
					kept += len(w.Summary) + len(w.Detail)
				}
				last := warnings[len(warnings)-1]
				want := fmt.Sprintf("%d more warnings", 10000-(len(warnings)-1))
				if kept > maxListWarningBytes || !strings.Contains(last.Summary, want) {
					t.Errorf("kept %d bytes, last warning %q; want at most %d bytes and a count that holds %q", kept, last.Summary, maxListWarningBytes, want)
				}
			})
			t.Run("a repeated id does not hide the next", func(t *testing.T) {
				a, b := thingIDs(2)[0], thingIDs(2)[1]
				s := &listStandIn{ids: []string{a, a, b}, failAt: -1}
				c := serveList(t, version, s)
				ids, _, err := c.List(context.Background(), "fake_thing", query, 2)
				if err != nil || !slices.Equal(ids, []string{a, b}) {
					t.Fatalf("ids %v, err %v; want [%s %s]", ids, err, a, b)
				}
			})
			t.Run("repeats that end the stream are a whole list", func(t *testing.T) {
				a := thingIDs(1)[0]
				s := &listStandIn{ids: []string{a, a, a}, failAt: -1}
				c := serveList(t, version, s)
				ids, _, err := c.List(context.Background(), "fake_thing", query, 2)
				if err != nil || !slices.Equal(ids, []string{a}) {
					t.Fatalf("ids %v, err %v; want [%s]", ids, err, a)
				}
			})
			t.Run("repeats up to the event cap fail the listing", func(t *testing.T) {
				a := thingIDs(1)[0]
				s := &listStandIn{ids: slices.Repeat([]string{a}, 100), failAt: -1}
				c := serveList(t, version, s)
				ids, _, err := c.List(context.Background(), "fake_thing", query, 2)
				if err == nil || ids != nil || !strings.Contains(err.Error(), "repeat") {
					t.Fatalf("ids %v, err = %v; want a refusal of a listing of repeats", ids, err)
				}
			})
			t.Run("the request budget stops a stream left open", func(t *testing.T) {
				const limit = 500
				a := thingIDs(1)[0]
				s := &listStandIn{ids: slices.Repeat([]string{a}, listRequestLimit(limit)), hold: true, released: make(chan struct{}), failAt: -1}
				c := serveList(t, version, s)
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				start := time.Now()
				ids, _, err := c.List(ctx, "fake_thing", query, limit)
				if took := time.Since(start); took > 10*time.Second {
					t.Fatalf("List took %s, want it to stop at the request budget", took)
				}
				if err == nil || ids != nil || !strings.Contains(err.Error(), "repeats") {
					t.Fatalf("ids %d, err = %v; want a refusal of a listing of repeats", len(ids), err)
				}
				select {
				case <-s.released:
				case <-time.After(10 * time.Second):
					t.Fatal("the stream was not cancelled after the client stopped reading")
				}
			})
			t.Run("warning paths count against the budget", func(t *testing.T) {
				s := &listStandIn{ids: thingIDs(1), warnings: 20, warnKey: strings.Repeat("k", 1<<20), failAt: -1}
				c := serveList(t, version, s)
				_, warnings, err := c.List(context.Background(), "fake_thing", query, 1)
				if err != nil {
					t.Fatal(err)
				}
				total := 0
				for _, w := range warnings {
					p := len(w.Path.String())
					if p > maxListWarningPathBytes {
						t.Fatalf("a kept path is %d bytes, want at most %d", p, maxListWarningPathBytes)
					}
					total += len(w.Summary) + len(w.Detail) + p
				}
				if total > maxListWarningBytes {
					t.Errorf("kept %d bytes of warnings, want at most %d", total, maxListWarningBytes)
				}
			})
			t.Run("an identity past the limit fails the listing", func(t *testing.T) {
				s := &listStandIn{ids: thingIDs(2), bigID: true, failAt: -1}
				c := serveList(t, version, s)
				ids, _, err := c.List(context.Background(), "fake_thing", query, 10)
				if err == nil || ids != nil || !strings.Contains(err.Error(), "limit") {
					t.Fatalf("ids %d, err = %v; want a refusal that names the limit", len(ids), err)
				}
			})
			t.Run("a null identity fails the listing", func(t *testing.T) {
				s := &listStandIn{ids: thingIDs(3), nullAt: 1, failAt: -1}
				c := serveList(t, version, s)
				ids, _, err := c.List(context.Background(), "fake_thing", query, 10)
				if err == nil || ids != nil || !strings.Contains(err.Error(), "null") {
					t.Fatalf("ids %v, err = %v; want a refusal of the null identity", ids, err)
				}
			})
			t.Run("an error event fails the whole listing", func(t *testing.T) {
				s := &listStandIn{ids: thingIDs(3), failAt: 1}
				c := serveList(t, version, s)
				ids, _, err := c.List(context.Background(), "fake_thing", query, 10)
				pe, ok := errors.AsType[*Error](err)
				if !ok || ids != nil || !strings.Contains(pe.Error(), "the stand-in refused page 2") {
					t.Fatalf("ids %v, err = %v; want a provider error and no ids", ids, err)
				}
			})
		})
	}
}

func TestImportKey(t *testing.T) {
	str, num := ctyTypeJSON(cty.String), ctyTypeJSON(cty.Number)
	tests := []struct {
		name  string
		attrs []identityAttribute
		want  string
		err   string
	}{
		{"one required string", []identityAttribute{{name: "account_id", typ: str}, {name: "id", typ: str, required: true}, {name: "region", typ: str}}, "id", ""},
		{"no identity", nil, "", "no resource identity"},
		{"none required", []identityAttribute{{name: "region", typ: str}}, "", "none of their identity attributes"},
		{"two required", []identityAttribute{{name: "cluster", typ: str, required: true}, {name: "name", typ: str, required: true}}, "", "identity of cluster, name"},
		{"a number", []identityAttribute{{name: "revision", typ: num, required: true}}, "", "not a string"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := importKey(tc.attrs)
			if tc.err == "" {
				if err != nil || got != tc.want {
					t.Fatalf("importKey = %q, %v; want %q", got, err, tc.want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("err = %v, want one that holds %q", err, tc.err)
			}
		})
	}
}

// cutPaths cuts n paths tags[<1 MiB key>] to 1 KiB, and keeps only the
// cut paths, so the keys are garbage unless a cut path still holds them.
func cutPaths(n int) []value.Path {
	out := make([]value.Path, n)
	for i := range out {
		p := value.Path{}.Attr("tags").Key(strings.Repeat(string(rune('a'+i)), 1<<20))
		out[i] = cutPath(p, maxListWarningPathBytes)
	}
	return out
}

func TestCutPathFreesTheRest(t *testing.T) {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	kept := cutPaths(20)
	runtime.GC()
	runtime.ReadMemStats(&after)
	for _, p := range kept {
		if p.String() != "tags" {
			t.Fatalf("cut path = %s, want tags", p)
		}
	}
	retained := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	runtime.KeepAlive(kept)
	if retained > 1<<20 {
		t.Errorf("20 cut paths retain %d bytes, want well under 1 MiB", retained)
	}
}
