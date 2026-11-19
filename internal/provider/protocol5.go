package provider

import (
	"context"

	"google.golang.org/grpc"

	"github.com/mr-karan/writ/internal/tfplugin5"
)

// v5 speaks plugin protocol 5. Its schema messages put write_only and
// deprecation_message at field numbers protocol 6 uses for other fields, so
// protocol 5 responses must never be decoded with protocol 6 types.
type v5 struct {
	c tfplugin5.ProviderClient
}

func newV5(conn *grpc.ClientConn) v5 { return v5{c: tfplugin5.NewProviderClient(conn)} }

func (a v5) getSchema(ctx context.Context) (schemaReply, error) {
	resp, err := a.c.GetSchema(ctx, &tfplugin5.GetProviderSchema_Request{})
	if err != nil {
		return schemaReply{}, err
	}
	return schemaReply{
		schema: &cachedSchema{
			Protocol:          5,
			GetSchemaOptional: resp.GetServerCapabilities().GetGetProviderSchemaOptional(),
			Provider:          v5Block(resp.GetProvider().GetBlock()),
			Resources:         v5Schemas(resp.GetResourceSchemas()),
			DataSources:       v5Schemas(resp.GetDataSourceSchemas()),
		},
		lists: v5Schemas(resp.GetListResourceSchemas()),
		diags: v5Diags(resp.GetDiagnostics()),
	}, nil
}

func v5Schemas(m map[string]*tfplugin5.Schema) map[string]rawResource {
	out := make(map[string]rawResource, len(m))
	for name, s := range m {
		out[name] = rawResource{Version: s.GetVersion(), Block: v5Block(s.GetBlock())}
	}
	return out
}

func v5Block(b *tfplugin5.Schema_Block) rawBlock {
	rb := rawBlock{Deprecated: b.GetDeprecated(), DeprecationMessage: b.GetDeprecationMessage()}
	for _, a := range b.GetAttributes() {
		rb.Attributes = append(rb.Attributes, rawAttribute{
			Name:               a.GetName(),
			Type:               a.GetType(),
			Required:           a.GetRequired(),
			Optional:           a.GetOptional(),
			Computed:           a.GetComputed(),
			Sensitive:          a.GetSensitive(),
			WriteOnly:          a.GetWriteOnly(),
			Deprecated:         a.GetDeprecated(),
			DeprecationMessage: a.GetDeprecationMessage(),
		})
	}
	for _, nb := range b.GetBlockTypes() {
		rb.Blocks = append(rb.Blocks, rawNestedBlock{
			Name:     nb.GetTypeName(),
			Nesting:  nesting(nb.GetNesting().String()),
			MinItems: nb.GetMinItems(),
			MaxItems: nb.GetMaxItems(),
			Block:    v5Block(nb.GetBlock()),
		})
	}
	return rb
}

func (a v5) configure(ctx context.Context, config []byte) ([]wireDiag, error) {
	resp, err := a.c.Configure(ctx, &tfplugin5.Configure_Request{
		TerraformVersion: terraformVersion,
		Config:           &tfplugin5.DynamicValue{Msgpack: config},
	})
	if err != nil {
		return nil, err
	}
	return v5Diags(resp.GetDiagnostics()), nil
}

func (a v5) validate(ctx context.Context, typ string, config []byte) ([]wireDiag, error) {
	resp, err := a.c.ValidateResourceTypeConfig(ctx, &tfplugin5.ValidateResourceTypeConfig_Request{
		TypeName: typ,
		Config:   &tfplugin5.DynamicValue{Msgpack: config},
	})
	if err != nil {
		return nil, err
	}
	return v5Diags(resp.GetDiagnostics()), nil
}

func (a v5) read(ctx context.Context, req readCall) (stateResult, error) {
	resp, err := a.c.ReadResource(ctx, &tfplugin5.ReadResource_Request{
		TypeName:        req.typ,
		CurrentState:    &tfplugin5.DynamicValue{Msgpack: req.current},
		Private:         req.private,
		CurrentIdentity: v5Identity(req.identity),
	})
	if err != nil {
		return stateResult{}, err
	}
	return stateResult{state: v5Dynamic(resp.GetNewState()), private: resp.GetPrivate(), identity: v5Dynamic(resp.GetNewIdentity().GetIdentityData()), diags: v5Diags(resp.GetDiagnostics())}, nil
}

func (a v5) importState(ctx context.Context, typ, id string) (importResult, error) {
	resp, err := a.c.ImportResourceState(ctx, &tfplugin5.ImportResourceState_Request{TypeName: typ, Id: id})
	if err != nil {
		return importResult{}, err
	}
	out := importResult{diags: v5Diags(resp.GetDiagnostics())}
	for _, r := range resp.GetImportedResources() {
		out.objects = append(out.objects, importedObject{typ: r.GetTypeName(), state: v5Dynamic(r.GetState()), private: r.GetPrivate(), identity: v5Dynamic(r.GetIdentity().GetIdentityData())})
	}
	return out, nil
}

func (a v5) plan(ctx context.Context, req planCall) (planResult, error) {
	resp, err := a.c.PlanResourceChange(ctx, &tfplugin5.PlanResourceChange_Request{
		TypeName:         req.typ,
		PriorState:       &tfplugin5.DynamicValue{Msgpack: req.prior},
		ProposedNewState: &tfplugin5.DynamicValue{Msgpack: req.proposed},
		Config:           &tfplugin5.DynamicValue{Msgpack: req.config},
		PriorPrivate:     req.priorPrivate,
		PriorIdentity:    v5Identity(req.priorIdentity),
	})
	if err != nil {
		return planResult{}, err
	}
	out := planResult{planned: v5Dynamic(resp.GetPlannedState()), private: resp.GetPlannedPrivate(), identity: v5Dynamic(resp.GetPlannedIdentity().GetIdentityData()), diags: v5Diags(resp.GetDiagnostics())}
	for _, p := range resp.GetRequiresReplace() {
		out.requiresReplace = append(out.requiresReplace, v5Path(p))
	}
	return out, nil
}

func (a v5) identitySchema(ctx context.Context, typ string) ([]identityAttribute, []wireDiag, error) {
	resp, err := a.c.GetResourceIdentitySchemas(ctx, &tfplugin5.GetResourceIdentitySchemas_Request{})
	if err != nil {
		return nil, nil, err
	}
	var out []identityAttribute
	for _, at := range resp.GetIdentitySchemas()[typ].GetIdentityAttributes() {
		out = append(out, identityAttribute{name: at.GetName(), typ: at.GetType(), required: at.GetRequiredForImport()})
	}
	return out, v5Diags(resp.GetDiagnostics()), nil
}

func (a v5) validateList(ctx context.Context, typ string, config, includeObject, limit []byte) ([]wireDiag, error) {
	resp, err := a.c.ValidateListResourceConfig(ctx, &tfplugin5.ValidateListResourceConfig_Request{
		TypeName:              typ,
		Config:                &tfplugin5.DynamicValue{Msgpack: config},
		IncludeResourceObject: &tfplugin5.DynamicValue{Msgpack: includeObject},
		Limit:                 &tfplugin5.DynamicValue{Msgpack: limit},
	})
	if err != nil {
		return nil, err
	}
	return v5Diags(resp.GetDiagnostics()), nil
}

func (a v5) list(ctx context.Context, typ string, config []byte, limit int64) (func() (listEvent, error), error) {
	stream, err := a.c.ListResource(ctx, &tfplugin5.ListResource_Request{TypeName: typ, Config: &tfplugin5.DynamicValue{Msgpack: config}, Limit: limit})
	if err != nil {
		return nil, err
	}
	return func() (listEvent, error) {
		ev, err := stream.Recv()
		if err != nil {
			return listEvent{}, err
		}
		return listEvent{identity: v5Dynamic(ev.GetIdentity().GetIdentityData()), diags: v5Diags(ev.GetDiagnostic())}, nil
	}, nil
}

func v5Dynamic(d *tfplugin5.DynamicValue) dynamic {
	return dynamic{msgpack: d.GetMsgpack(), json: d.GetJson()}
}

// v5Identity is nil for no identity, so the provider sees none.
func v5Identity(d dynamic) *tfplugin5.ResourceIdentityData {
	if d.none() {
		return nil
	}
	return &tfplugin5.ResourceIdentityData{IdentityData: &tfplugin5.DynamicValue{Msgpack: d.msgpack, Json: d.json}}
}

func v5Diags(ds []*tfplugin5.Diagnostic) []wireDiag {
	out := make([]wireDiag, 0, len(ds))
	for _, d := range ds {
		out = append(out, wireDiag{
			error:   d.GetSeverity() != tfplugin5.Diagnostic_WARNING,
			summary: d.GetSummary(),
			detail:  d.GetDetail(),
			path:    v5Path(d.GetAttribute()),
		})
	}
	return out
}

func v5Path(p *tfplugin5.AttributePath) []wireStep {
	var steps []wireStep
	for _, s := range p.GetSteps() {
		switch sel := s.GetSelector().(type) {
		case *tfplugin5.AttributePath_Step_AttributeName:
			steps = append(steps, wireStep{kind: stepAttr, name: sel.AttributeName})
		case *tfplugin5.AttributePath_Step_ElementKeyString:
			steps = append(steps, wireStep{kind: stepKey, name: sel.ElementKeyString})
		case *tfplugin5.AttributePath_Step_ElementKeyInt:
			steps = append(steps, wireStep{kind: stepIndex, index: sel.ElementKeyInt})
		}
	}
	return steps
}
