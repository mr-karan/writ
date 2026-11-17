package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"testing"

	"github.com/zclconf/go-cty/cty"
	ctymsgpack "github.com/zclconf/go-cty/cty/msgpack"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/mr-karan/writ/internal/schema"
	"github.com/mr-karan/writ/internal/tfplugin5"
	"github.com/mr-karan/writ/internal/tfplugin6"
	"github.com/mr-karan/writ/internal/value"
)

// These tests run Import and Plan against a stand-in provider that serves
// the real plugin protocol over gRPC on loopback, in version 5 and 6. It
// keeps no state of its own and checks no identity, as a provider that
// trusts its caller does.

var (
	identityStateType = cty.Object(map[string]cty.Type{"id": cty.String, "name": cty.String})
	identityType      = cty.Object(map[string]cty.Type{"id": cty.String})
)

type identityStandIn struct {
	// none returns no identity from any call.
	none bool
	// moves plans an identity of another object, and the state unchanged.
	moves bool

	mu sync.Mutex
	// read and planned are the identities ReadResource and
	// PlanResourceChange received, nil when none.
	read, planned [][]byte
}

func identityOfID(id string) []byte {
	b, err := ctymsgpack.Marshal(cty.ObjectVal(map[string]cty.Value{"id": cty.StringVal(id)}), identityType)
	if err != nil {
		panic(err)
	}
	return b
}

func (s *identityStandIn) imported(id string) (state, identity []byte) {
	state, err := ctymsgpack.Marshal(cty.ObjectVal(map[string]cty.Value{"id": cty.StringVal(id), "name": cty.StringVal("web")}), identityStateType)
	if err != nil {
		panic(err)
	}
	if s.none {
		return state, nil
	}
	return state, identityOfID(id)
}

// readIdentity returns the identity a read answers with: the one it got.
func (s *identityStandIn) readIdentity(current []byte) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.read = append(s.read, current)
	if s.none {
		return nil
	}
	return current
}

func (s *identityStandIn) planIdentity(prior []byte) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.planned = append(s.planned, prior)
	switch {
	case s.none:
		return nil
	case s.moves:
		return identityOfID("thing-other")
	}
	return prior
}

type identityServer5 struct {
	tfplugin5.UnimplementedProviderServer
	s *identityStandIn
}

func (i identityServer5) ImportResourceState(_ context.Context, req *tfplugin5.ImportResourceState_Request) (*tfplugin5.ImportResourceState_Response, error) {
	state, id := i.s.imported(req.GetId())
	r := &tfplugin5.ImportResourceState_ImportedResource{TypeName: req.GetTypeName(), State: &tfplugin5.DynamicValue{Msgpack: state}}
	if id != nil {
		r.Identity = &tfplugin5.ResourceIdentityData{IdentityData: &tfplugin5.DynamicValue{Msgpack: id}}
	}
	return &tfplugin5.ImportResourceState_Response{ImportedResources: []*tfplugin5.ImportResourceState_ImportedResource{r}}, nil
}

func (i identityServer5) ReadResource(_ context.Context, req *tfplugin5.ReadResource_Request) (*tfplugin5.ReadResource_Response, error) {
	resp := &tfplugin5.ReadResource_Response{NewState: req.GetCurrentState()}
	if id := i.s.readIdentity(req.GetCurrentIdentity().GetIdentityData().GetMsgpack()); id != nil {
		resp.NewIdentity = &tfplugin5.ResourceIdentityData{IdentityData: &tfplugin5.DynamicValue{Msgpack: id}}
	}
	return resp, nil
}

func (i identityServer5) PlanResourceChange(_ context.Context, req *tfplugin5.PlanResourceChange_Request) (*tfplugin5.PlanResourceChange_Response, error) {
	resp := &tfplugin5.PlanResourceChange_Response{PlannedState: req.GetProposedNewState()}
	if id := i.s.planIdentity(req.GetPriorIdentity().GetIdentityData().GetMsgpack()); id != nil {
		resp.PlannedIdentity = &tfplugin5.ResourceIdentityData{IdentityData: &tfplugin5.DynamicValue{Msgpack: id}}
	}
	return resp, nil
}

func (identityServer5) ValidateResourceTypeConfig(context.Context, *tfplugin5.ValidateResourceTypeConfig_Request) (*tfplugin5.ValidateResourceTypeConfig_Response, error) {
	return &tfplugin5.ValidateResourceTypeConfig_Response{}, nil
}

type identityServer6 struct {
	tfplugin6.UnimplementedProviderServer
	s *identityStandIn
}

func (i identityServer6) ImportResourceState(_ context.Context, req *tfplugin6.ImportResourceState_Request) (*tfplugin6.ImportResourceState_Response, error) {
	state, id := i.s.imported(req.GetId())
	r := &tfplugin6.ImportResourceState_ImportedResource{TypeName: req.GetTypeName(), State: &tfplugin6.DynamicValue{Msgpack: state}}
	if id != nil {
		r.Identity = &tfplugin6.ResourceIdentityData{IdentityData: &tfplugin6.DynamicValue{Msgpack: id}}
	}
	return &tfplugin6.ImportResourceState_Response{ImportedResources: []*tfplugin6.ImportResourceState_ImportedResource{r}}, nil
}

func (i identityServer6) ReadResource(_ context.Context, req *tfplugin6.ReadResource_Request) (*tfplugin6.ReadResource_Response, error) {
	resp := &tfplugin6.ReadResource_Response{NewState: req.GetCurrentState()}
	if id := i.s.readIdentity(req.GetCurrentIdentity().GetIdentityData().GetMsgpack()); id != nil {
		resp.NewIdentity = &tfplugin6.ResourceIdentityData{IdentityData: &tfplugin6.DynamicValue{Msgpack: id}}
	}
	return resp, nil
}

func (i identityServer6) PlanResourceChange(_ context.Context, req *tfplugin6.PlanResourceChange_Request) (*tfplugin6.PlanResourceChange_Response, error) {
	resp := &tfplugin6.PlanResourceChange_Response{PlannedState: req.GetProposedNewState()}
	if id := i.s.planIdentity(req.GetPriorIdentity().GetIdentityData().GetMsgpack()); id != nil {
		resp.PlannedIdentity = &tfplugin6.ResourceIdentityData{IdentityData: &tfplugin6.DynamicValue{Msgpack: id}}
	}
	return resp, nil
}

func (identityServer6) ValidateResourceConfig(context.Context, *tfplugin6.ValidateResourceConfig_Request) (*tfplugin6.ValidateResourceConfig_Response, error) {
	return &tfplugin6.ValidateResourceConfig_Response{}, nil
}

// serveIdentity starts the stand-in on loopback in the given protocol
// version and returns a Client connected to it, with the schema of
// fake_thing.
func serveIdentity(t *testing.T, version int, s *identityStandIn) (*Client, *schema.Provider) {
	t.Helper()
	str := json.RawMessage(`"string"`)
	cs := &cachedSchema{Protocol: version, Resources: map[string]rawResource{"fake_thing": {Block: rawBlock{Attributes: []rawAttribute{
		{Name: "id", Type: str, Computed: true},
		{Name: "name", Type: str, Optional: true},
	}}}}}
	src := schema.Source{Namespace: "example", Type: "fake"}
	p, ws, err := buildSchema(src, "1.0.0", cs)
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	if version == 5 {
		tfplugin5.RegisterProviderServer(srv, identityServer5{s: s})
	} else {
		tfplugin6.RegisterProviderServer(srv, identityServer6{s: s})
	}
	go srv.Serve(lis) //nolint:errcheck // Serve returns when the test stops it
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///"+lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	proc := &process{protocol: version, conn: conn, stderr: &tailBuffer{max: 1024}}
	return &Client{src: src, proc: proc, proto: proc.client(), wire: ws}, p
}

func TestClientCarriesIdentity(t *testing.T) {
	ctx := context.Background()
	for _, version := range []int{5, 6} {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			planOf := func(t *testing.T, c *Client, p *schema.Provider, got ImportResponse) error {
				r, err := p.ResourceByWire("fake_thing")
				if err != nil {
					t.Fatal(err)
				}
				cfg := value.ObjectVal(map[string]value.Value{"id": value.NullVal(), "name": value.StringVal("web")})
				_, err = c.Plan(ctx, PlanRequest{Type: "fake_thing", Prior: got.State, Proposed: ProposeNew(r.Block, got.State, cfg), Config: cfg, PriorIdentity: got.Identity})
				return err
			}
			t.Run("import, read, and plan pass the identity on", func(t *testing.T) {
				s := &identityStandIn{}
				c, p := serveIdentity(t, version, s)
				got, err := c.Import(ctx, ImportRequest{Type: "fake_thing", ID: "thing-1"})
				if err != nil {
					t.Fatal(err)
				}
				if err := planOf(t, c, p, got); err != nil {
					t.Fatal(err)
				}
				want := identityOfID("thing-1")
				if len(s.read) != 1 || string(s.read[0]) != string(want) || len(s.planned) != 1 || string(s.planned[0]) != string(want) {
					t.Errorf("read got identities %x, plan got %x; want %x for each", s.read, s.planned, want)
				}
			})
			t.Run("a plan that moves the identity fails", func(t *testing.T) {
				s := &identityStandIn{moves: true}
				c, p := serveIdentity(t, version, s)
				got, err := c.Import(ctx, ImportRequest{Type: "fake_thing", ID: "thing-1"})
				if err != nil {
					t.Fatal(err)
				}
				err = planOf(t, c, p, got)
				if pe, ok := err.(*Error); !ok || len(pe.Diags) != 1 || !pe.Diags[0].Error || pe.Diags[0].Summary != "the provider planned another resource identity for the object" {
					t.Fatalf("err = %v, want the identity change refused", err)
				}
			})
			t.Run("a provider with no identity gets none", func(t *testing.T) {
				s := &identityStandIn{none: true}
				c, p := serveIdentity(t, version, s)
				got, err := c.Import(ctx, ImportRequest{Type: "fake_thing", ID: "thing-1"})
				if err != nil {
					t.Fatal(err)
				}
				if err := planOf(t, c, p, got); err != nil {
					t.Fatal(err)
				}
				if len(s.read) != 1 || s.read[0] != nil || len(s.planned) != 1 || s.planned[0] != nil {
					t.Errorf("read got identities %x, plan got %x; want none", s.read, s.planned)
				}
			})
		})
	}
}
