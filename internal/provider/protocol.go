package provider

import "context"

// protocol is one provider connection, over plugin protocol 5 or 6. Each
// adapter converts its own protobuf messages into these protocol-neutral
// types and back, so nothing above this interface knows which protocol a
// provider speaks. Values cross as msgpack bytes; wire.go turns them into
// Writ values.
type protocol interface {
	getSchema(ctx context.Context) (*cachedSchema, []wireDiag, error)
	configure(ctx context.Context, config []byte) ([]wireDiag, error)
	validate(ctx context.Context, typ string, config []byte) ([]wireDiag, error)
	read(ctx context.Context, req readCall) (stateResult, error)
	importState(ctx context.Context, typ, id string) (importResult, error)
	plan(ctx context.Context, req planCall) (planResult, error)
	listInfo(ctx context.Context, typ string) (listInfo, []wireDiag, error)
	validateList(ctx context.Context, typ string, config, includeObject, limit []byte) ([]wireDiag, error)
	list(ctx context.Context, typ string, config []byte, limit int64) (func() (listEvent, error), error)
}

// listInfo is what a provider says about listing objects of one type: the
// schema of its list config, nil when it has no list resource of the type,
// and the attributes of the type's resource identity.
type listInfo struct {
	config   *rawBlock
	identity []identityAttribute
}

type identityAttribute struct {
	name     string
	typ      []byte // cty JSON type
	required bool   // required_for_import
}

// listEvent is one object a ListResource stream returned.
type listEvent struct {
	identity dynamic
	diags    []wireDiag
}

type readCall struct {
	typ      string
	current  []byte
	private  []byte
	identity dynamic
}

type planCall struct {
	typ                     string
	prior, proposed, config []byte
	priorPrivate            []byte
	priorIdentity           dynamic
}

// stateResult is the answer to ReadResource.
type stateResult struct {
	state    dynamic
	private  []byte
	identity dynamic
	diags    []wireDiag
}

// importResult is the answer to ImportResourceState: one state per object the
// id names, each with the wire type the provider gave it.
type importResult struct {
	objects []importedObject
	diags   []wireDiag
}

type importedObject struct {
	typ      string
	state    dynamic
	private  []byte
	identity dynamic
}

type planResult struct {
	planned         dynamic
	requiresReplace [][]wireStep
	private         []byte
	identity        dynamic
	diags           []wireDiag
}

// wireDiag is a provider diagnostic before its path is mapped to Writ's.
type wireDiag struct {
	error   bool
	summary string
	detail  string
	path    []wireStep
}
