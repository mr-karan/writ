package provider

import (
	"context"

	"google.golang.org/grpc"

	"github.com/mr-karan/writ/internal/tfplugin6"
)

// v6 speaks plugin protocol 6, which adds nested attribute types.
type v6 struct {
	c tfplugin6.ProviderClient
}

func newV6(conn *grpc.ClientConn) v6 { return v6{c: tfplugin6.NewProviderClient(conn)} }

func (a v6) getSchema(ctx context.Context) (*cachedSchema, []wireDiag, error) {
	resp, err := a.c.GetProviderSchema(ctx, &tfplugin6.GetProviderSchema_Request{})
	if err != nil {
		return nil, nil, err
	}
	cs := &cachedSchema{
		Protocol:          6,
		GetSchemaOptional: resp.GetServerCapabilities().GetGetProviderSchemaOptional(),
		Provider:          v6Block(resp.GetProvider().GetBlock()),
		Resources:         make(map[string]rawResource, len(resp.GetResourceSchemas())),
		DataSources:       make(map[string]rawResource, len(resp.GetDataSourceSchemas())),
	}
	for name, s := range resp.GetResourceSchemas() {
		cs.Resources[name] = rawResource{Version: s.GetVersion(), Block: v6Block(s.GetBlock())}
	}
	for name, s := range resp.GetDataSourceSchemas() {
		cs.DataSources[name] = rawResource{Version: s.GetVersion(), Block: v6Block(s.GetBlock())}
	}
	return cs, v6Diags(resp.GetDiagnostics()), nil
}

func v6Block(b *tfplugin6.Schema_Block) rawBlock {
	rb := rawBlock{Deprecated: b.GetDeprecated(), DeprecationMessage: b.GetDeprecationMessage()}
	for _, a := range b.GetAttributes() {
		rb.Attributes = append(rb.Attributes, v6Attribute(a))
	}
	for _, nb := range b.GetBlockTypes() {
		rb.Blocks = append(rb.Blocks, rawNestedBlock{
			Name:     nb.GetTypeName(),
			Nesting:  v6Nesting(nb.GetNesting()),
			MinItems: nb.GetMinItems(),
			MaxItems: nb.GetMaxItems(),
			Block:    v6Block(nb.GetBlock()),
		})
	}
	return rb
}

func v6Attribute(a *tfplugin6.Schema_Attribute) rawAttribute {
	ra := rawAttribute{
		Name:               a.GetName(),
		Type:               a.GetType(),
		Required:           a.GetRequired(),
		Optional:           a.GetOptional(),
		Computed:           a.GetComputed(),
		Sensitive:          a.GetSensitive(),
		WriteOnly:          a.GetWriteOnly(),
		Deprecated:         a.GetDeprecated(),
		DeprecationMessage: a.GetDeprecationMessage(),
	}
	if nt := a.GetNestedType(); nt != nil {
		ra.Type = nil
		ra.Nested = &rawObject{Nesting: v6ObjectNesting(nt.GetNesting())}
		for _, na := range nt.GetAttributes() {
			ra.Nested.Attributes = append(ra.Nested.Attributes, v6Attribute(na))
		}
	}
	return ra
}

func v6Nesting(n tfplugin6.Schema_NestedBlock_NestingMode) rawNesting {
	switch n {
	case tfplugin6.Schema_NestedBlock_SINGLE:
		return nestSingle
	case tfplugin6.Schema_NestedBlock_GROUP:
		return nestGroup
	case tfplugin6.Schema_NestedBlock_LIST:
		return nestList
	case tfplugin6.Schema_NestedBlock_SET:
		return nestSet
	case tfplugin6.Schema_NestedBlock_MAP:
		return nestMap
	}
	return rawNesting(n.String())
}

func v6ObjectNesting(n tfplugin6.Schema_Object_NestingMode) rawNesting {
	switch n {
	case tfplugin6.Schema_Object_SINGLE:
		return nestSingle
	case tfplugin6.Schema_Object_LIST:
		return nestList
	case tfplugin6.Schema_Object_SET:
		return nestSet
	case tfplugin6.Schema_Object_MAP:
		return nestMap
	}
	return rawNesting(n.String())
}

func (a v6) configure(ctx context.Context, config []byte) ([]wireDiag, error) {
	resp, err := a.c.ConfigureProvider(ctx, &tfplugin6.ConfigureProvider_Request{
		TerraformVersion: terraformVersion,
		Config:           &tfplugin6.DynamicValue{Msgpack: config},
	})
	if err != nil {
		return nil, err
	}
	return v6Diags(resp.GetDiagnostics()), nil
}

func (a v6) validate(ctx context.Context, typ string, config []byte) ([]wireDiag, error) {
	resp, err := a.c.ValidateResourceConfig(ctx, &tfplugin6.ValidateResourceConfig_Request{
		TypeName: typ,
		Config:   &tfplugin6.DynamicValue{Msgpack: config},
	})
	if err != nil {
		return nil, err
	}
	return v6Diags(resp.GetDiagnostics()), nil
}

func (a v6) read(ctx context.Context, req readCall) (stateResult, error) {
	resp, err := a.c.ReadResource(ctx, &tfplugin6.ReadResource_Request{
		TypeName:        req.typ,
		CurrentState:    &tfplugin6.DynamicValue{Msgpack: req.current},
		Private:         req.private,
		CurrentIdentity: v6Identity(req.identity),
	})
	if err != nil {
		return stateResult{}, err
	}
	return stateResult{state: v6Dynamic(resp.GetNewState()), private: resp.GetPrivate(), identity: v6Dynamic(resp.GetNewIdentity().GetIdentityData()), diags: v6Diags(resp.GetDiagnostics())}, nil
}

func (a v6) importState(ctx context.Context, typ, id string) (importResult, error) {
	resp, err := a.c.ImportResourceState(ctx, &tfplugin6.ImportResourceState_Request{TypeName: typ, Id: id})
	if err != nil {
		return importResult{}, err
	}
	out := importResult{diags: v6Diags(resp.GetDiagnostics())}
	for _, r := range resp.GetImportedResources() {
		out.objects = append(out.objects, importedObject{typ: r.GetTypeName(), state: v6Dynamic(r.GetState()), private: r.GetPrivate(), identity: v6Dynamic(r.GetIdentity().GetIdentityData())})
	}
	return out, nil
}

func (a v6) plan(ctx context.Context, req planCall) (planResult, error) {
	resp, err := a.c.PlanResourceChange(ctx, &tfplugin6.PlanResourceChange_Request{
		TypeName:         req.typ,
		PriorState:       &tfplugin6.DynamicValue{Msgpack: req.prior},
		ProposedNewState: &tfplugin6.DynamicValue{Msgpack: req.proposed},
		Config:           &tfplugin6.DynamicValue{Msgpack: req.config},
		PriorPrivate:     req.priorPrivate,
		PriorIdentity:    v6Identity(req.priorIdentity),
	})
	if err != nil {
		return planResult{}, err
	}
	out := planResult{planned: v6Dynamic(resp.GetPlannedState()), private: resp.GetPlannedPrivate(), identity: v6Dynamic(resp.GetPlannedIdentity().GetIdentityData()), diags: v6Diags(resp.GetDiagnostics())}
	for _, p := range resp.GetRequiresReplace() {
		out.requiresReplace = append(out.requiresReplace, v6Path(p))
	}
	return out, nil
}

func (a v6) listInfo(ctx context.Context, typ string) (listInfo, []wireDiag, error) {
	resp, err := a.c.GetProviderSchema(ctx, &tfplugin6.GetProviderSchema_Request{})
	if err != nil {
		return listInfo{}, nil, err
	}
	var out listInfo
	if s, ok := resp.GetListResourceSchemas()[typ]; ok {
		b := v6Block(s.GetBlock())
		out.config = &b
	}
	diags := v6Diags(resp.GetDiagnostics())
	ids, err := a.c.GetResourceIdentitySchemas(ctx, &tfplugin6.GetResourceIdentitySchemas_Request{})
	if err != nil {
		return listInfo{}, nil, err
	}
	for _, at := range ids.GetIdentitySchemas()[typ].GetIdentityAttributes() {
		out.identity = append(out.identity, identityAttribute{name: at.GetName(), typ: at.GetType(), required: at.GetRequiredForImport()})
	}
	return out, append(diags, v6Diags(ids.GetDiagnostics())...), nil
}

func (a v6) validateList(ctx context.Context, typ string, config, includeObject, limit []byte) ([]wireDiag, error) {
	resp, err := a.c.ValidateListResourceConfig(ctx, &tfplugin6.ValidateListResourceConfig_Request{
		TypeName:              typ,
		Config:                &tfplugin6.DynamicValue{Msgpack: config},
		IncludeResourceObject: &tfplugin6.DynamicValue{Msgpack: includeObject},
		Limit:                 &tfplugin6.DynamicValue{Msgpack: limit},
	})
	if err != nil {
		return nil, err
	}
	return v6Diags(resp.GetDiagnostics()), nil
}

func (a v6) list(ctx context.Context, typ string, config []byte, limit int64) (func() (listEvent, error), error) {
	stream, err := a.c.ListResource(ctx, &tfplugin6.ListResource_Request{TypeName: typ, Config: &tfplugin6.DynamicValue{Msgpack: config}, Limit: limit})
	if err != nil {
		return nil, err
	}
	return func() (listEvent, error) {
		ev, err := stream.Recv()
		if err != nil {
			return listEvent{}, err
		}
		return listEvent{identity: v6Dynamic(ev.GetIdentity().GetIdentityData()), diags: v6Diags(ev.GetDiagnostic())}, nil
	}, nil
}

func v6Dynamic(d *tfplugin6.DynamicValue) dynamic {
	return dynamic{msgpack: d.GetMsgpack(), json: d.GetJson()}
}

// v6Identity is nil for no identity, so the provider sees none.
func v6Identity(d dynamic) *tfplugin6.ResourceIdentityData {
	if len(d.msgpack) == 0 && len(d.json) == 0 {
		return nil
	}
	return &tfplugin6.ResourceIdentityData{IdentityData: &tfplugin6.DynamicValue{Msgpack: d.msgpack, Json: d.json}}
}

func v6Diags(ds []*tfplugin6.Diagnostic) []wireDiag {
	out := make([]wireDiag, 0, len(ds))
	for _, d := range ds {
		out = append(out, wireDiag{
			error:   d.GetSeverity() != tfplugin6.Diagnostic_WARNING,
			summary: d.GetSummary(),
			detail:  d.GetDetail(),
			path:    v6Path(d.GetAttribute()),
		})
	}
	return out
}

func v6Path(p *tfplugin6.AttributePath) []wireStep {
	var steps []wireStep
	for _, s := range p.GetSteps() {
		switch sel := s.GetSelector().(type) {
		case *tfplugin6.AttributePath_Step_AttributeName:
			steps = append(steps, wireStep{kind: stepAttr, name: sel.AttributeName})
		case *tfplugin6.AttributePath_Step_ElementKeyString:
			steps = append(steps, wireStep{kind: stepKey, name: sel.ElementKeyString})
		case *tfplugin6.AttributePath_Step_ElementKeyInt:
			steps = append(steps, wireStep{kind: stepIndex, index: sel.ElementKeyInt})
		}
	}
	return steps
}
