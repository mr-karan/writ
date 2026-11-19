package provider

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mr-karan/writ/internal/schema"
	"github.com/mr-karan/writ/internal/value"
)

// terraformVersion is sent in ConfigureProvider. Some providers gate behavior
// on it; 1.10.0 is a release whose protocol behavior Writ follows.
const terraformVersion = "1.10.0"

// Per-call deadlines.
const (
	quickTimeout    = 2 * time.Minute
	resourceTimeout = 30 * time.Minute
)

// Client is one running provider. Its methods are safe for concurrent use;
// providers serve concurrent RPCs.
type Client struct {
	nomadPatched bool
	src          schema.Source
	proc         *process
	proto        protocol
	// wire holds the cty types of the provider config and of every resource,
	// derived from the cached schema. Only wire.go reads it.
	wire *wireSchema
	// held is set once the process holds a secret; see hold.
	held atomic.Bool
}

// Diag is a provider diagnostic. Path is the attribute it is about, empty
// when it is about the whole resource.
type Diag struct {
	Error   bool
	Summary string
	Detail  string
	Path    value.Path
	// Held says the process that sent it held a secret then, so its text
	// may quote one; see Subject.Show.
	Held bool
}

// Error is returned when a provider reports error diagnostics. Diags holds
// every diagnostic of the call, warnings included.
type Error struct {
	Op    string // PlanResourceChange, ReadResource, ...
	Type  string // wire resource type; empty for ConfigureProvider
	Diags []Diag
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(e.Op)
	if e.Type != "" {
		b.WriteString(" " + e.Type)
	}
	b.WriteString(":")
	sep := " "
	for _, d := range e.Diags {
		if !d.Error {
			continue
		}
		b.WriteString(sep)
		sep = "; "
		if len(d.Path) > 0 {
			b.WriteString(d.Path.String() + ": ")
		}
		b.WriteString(d.Summary)
		if d.Detail != "" {
			b.WriteString(": " + d.Detail)
		}
	}
	return b.String()
}

// Identity is a resource identity as its provider encoded it; the zero
// value is none. Writ does not decode it. It goes back to the provider
// with the next call about the object, as OpenTofu sends it, and a plan
// must keep it.
type Identity struct{ data dynamic }

// identityOf treats an encoded null as no identity.
func identityOf(d dynamic) Identity {
	if bytes.Equal(d.msgpack, []byte{0xc0}) || string(bytes.TrimSpace(d.json)) == "null" {
		return Identity{}
	}
	return Identity{data: d}
}

// none reports whether the provider gave no identity.
func (i Identity) none() bool { return i.data.none() }

// equal compares encodings. A provider encodes one identity alike each
// time, so a difference in bytes counts as a change; at worst that makes
// a caller doubt a plan it could have trusted.
func (i Identity) equal(o Identity) bool {
	return bytes.Equal(i.data.msgpack, o.data.msgpack) && bytes.Equal(i.data.json, o.data.json)
}

// PlanRequest is the input to PlanResourceChange. Prior is null for a create.
type PlanRequest struct {
	Type         string // wire resource type: tls_private_key
	Prior        value.Value
	Proposed     value.Value
	Config       value.Value
	PriorPrivate []byte
	// PriorIdentity is the identity the last read returned with Prior.
	PriorIdentity Identity
}

// PlanResponse is the provider's answer.
type PlanResponse struct {
	Planned         value.Value // may contain Unknown; sensitive leaves marked secret
	RequiresReplace []value.Path
	Private         []byte
	Identity        Identity
	Warnings        []Diag
}

// Plan calls PlanResourceChange.
func (c *Client) Plan(ctx context.Context, req PlanRequest) (PlanResponse, error) {
	const op = "PlanResourceChange"
	w, err := c.wire.resource(req.Type)
	if err != nil {
		return PlanResponse{}, err
	}
	call := planCall{typ: req.Type, priorPrivate: req.PriorPrivate, priorIdentity: req.PriorIdentity.data}
	if call.prior, err = w.encode(c.priorState(req.Type, req.Prior)); err != nil {
		return PlanResponse{}, encodeError(op, req.Type, "prior state", err)
	}
	if call.proposed, err = w.encode(c.priorState(req.Type, req.Proposed)); err != nil {
		return PlanResponse{}, encodeError(op, req.Type, "proposed state", err)
	}
	if call.config, err = w.encode(req.Config); err != nil {
		return PlanResponse{}, encodeError(op, req.Type, "config", err)
	}
	ctx, cancel := context.WithTimeout(ctx, resourceTimeout)
	defer cancel()
	c.hold(w.block, req.Prior, req.Proposed, req.Config)
	resp, err := c.proto.plan(ctx, call)
	if err != nil {
		return PlanResponse{}, c.callError(op, req.Type, err)
	}
	planned, decErr := w.decode(resp.planned)
	c.hold(w.block, planned)
	diags := c.diags(w.wireBlock, resp.diags)
	if hasError(diags) {
		return PlanResponse{}, &Error{Op: op, Type: req.Type, Diags: diags}
	}
	if decErr != nil {
		return PlanResponse{}, decodeError(op, req.Type, decErr)
	}
	identity := identityOf(resp.identity)
	// SDKv2 refuses an identity change only when it receives a prior
	// identity, and another provider may not check at all. A plan that
	// changes the identity is never one that keeps the object.
	if !req.PriorIdentity.none() && !identity.none() && !identity.equal(req.PriorIdentity) {
		return PlanResponse{}, &Error{Op: op, Type: req.Type, Diags: append(diags, Diag{Error: true, Summary: "the provider planned another resource identity for the object", Detail: "an object's identity never changes, so this plan does not keep the object it read"})}
	}
	out := PlanResponse{Planned: planned, Private: resp.private, Identity: identity, Warnings: diags}
	for _, steps := range resp.requiresReplace {
		out.RequiresReplace = append(out.RequiresReplace, w.path(steps))
	}
	return out, nil
}

// ReadRequest is the input to ReadResource.
type ReadRequest struct {
	Type     string
	Current  value.Value
	Private  []byte
	Identity Identity
}

// ReadResponse holds the refreshed state; New is null when the object is
// gone. Identity is the one the provider returned, or else the one Read
// sent, so a provider that returns none keeps the object's identity.
type ReadResponse struct {
	New      value.Value
	Private  []byte
	Identity Identity
	Warnings []Diag
}

// Read calls ReadResource.
func (c *Client) Read(ctx context.Context, req ReadRequest) (ReadResponse, error) {
	const op = "ReadResource"
	w, err := c.wire.resource(req.Type)
	if err != nil {
		return ReadResponse{}, err
	}
	call := readCall{typ: req.Type, private: req.Private, identity: req.Identity.data}
	if call.current, err = w.encode(c.priorState(req.Type, req.Current)); err != nil {
		return ReadResponse{}, encodeError(op, req.Type, "current state", err)
	}
	ctx, cancel := context.WithTimeout(ctx, resourceTimeout)
	defer cancel()
	c.hold(w.block, req.Current)
	resp, err := c.proto.read(ctx, call)
	if err != nil {
		return ReadResponse{}, c.callError(op, req.Type, err)
	}
	newState, decErr := w.decode(resp.state)
	c.hold(w.block, newState)
	diags := c.diags(w.wireBlock, resp.diags)
	if hasError(diags) {
		return ReadResponse{}, &Error{Op: op, Type: req.Type, Diags: diags}
	}
	if decErr != nil {
		return ReadResponse{}, decodeError(op, req.Type, decErr)
	}
	newState, err = w.fillReadDefaults(newState)
	if err != nil {
		return ReadResponse{}, decodeError(op, req.Type, err)
	}
	identity := identityOf(resp.identity)
	if identity.none() {
		identity = req.Identity
	}
	return ReadResponse{New: newState, Private: resp.private, Identity: identity, Warnings: diags}, nil
}

// ImportRequest names an existing object by the id its provider imports it
// by, such as a security group id or a Nomad job name.
type ImportRequest struct {
	Type string
	ID   string
	// OneRead reads the object once after the import, as OpenTofu does,
	// also for a type whose second read settles it.
	OneRead bool
}

// ImportResponse is the object as the provider reads it now. State is null
// when the provider knows the id's form but finds no object.
type ImportResponse struct {
	State    value.Value
	Private  []byte
	Identity Identity
	Warnings []Diag
}

// Import calls ImportResourceState and then ReadResource, as Terraform does:
// most providers answer the first call with little more than the id, and the
// read fills in the rest. A provider may answer with objects of other types
// as well; Import keeps the one of the type asked for and warns about the
// others.
func (c *Client) Import(ctx context.Context, req ImportRequest) (ImportResponse, error) {
	resp, err := c.importObject(ctx, req)
	// The id is the provider's way to the object, and on a type with a
	// sensitive attribute it may be one of the object's secrets, as a
	// random_password's is. The import's own calls may quote it, so their
	// text is held. An id that is a secret is also in the object's
	// sensitive state, which the process then holds for the run.
	if w, werr := c.wire.resource(req.Type); werr == nil && hasSensitive(w.block) {
		for i := range resp.Warnings {
			resp.Warnings[i].Held = true
		}
		err = heldError(err)
	}
	return resp, err
}

// hasSensitive reports whether b has an attribute that is sensitive or
// whose type holds a secret, at any depth.
func hasSensitive(b schema.Block) bool {
	return slices.ContainsFunc(b.Attributes, func(a schema.Attribute) bool { return a.Sensitive || a.Type.HasSecret() }) ||
		slices.ContainsFunc(b.Blocks, func(nb schema.NestedBlock) bool { return hasSensitive(nb.Block) })
}

// importObject is Import, before it marks its text held.
func (c *Client) importObject(ctx context.Context, req ImportRequest) (ImportResponse, error) {
	const op = "ImportResourceState"
	w, err := c.wire.resource(req.Type)
	if err != nil {
		return ImportResponse{}, err
	}
	ictx, cancel := context.WithTimeout(ctx, resourceTimeout)
	defer cancel()
	resp, err := c.proto.importState(ictx, req.Type, req.ID)
	if err != nil {
		return ImportResponse{}, c.callError(op, req.Type, err)
	}
	for _, o := range resp.objects {
		if ow, err := c.wire.resource(o.typ); err == nil {
			v, _ := ow.decode(o.state)
			c.hold(ow.block, v)
		}
	}
	diags := c.diags(w.wireBlock, resp.diags)
	if hasError(diags) {
		return ImportResponse{}, &Error{Op: op, Type: req.Type, Diags: diags}
	}
	var found []importedObject
	for _, o := range resp.objects {
		if o.typ == req.Type {
			found = append(found, o)
			continue
		}
		diags = append(diags, Diag{Summary: fmt.Sprintf("the provider also returned a %s for this id, which is not imported", o.typ)})
	}
	switch len(found) {
	case 0:
		return ImportResponse{}, fmt.Errorf("%s %s: the provider returned no object of this type for the id", op, req.Type)
	case 1:
	default:
		return ImportResponse{}, fmt.Errorf("%s %s: the provider returned %d objects of this type for the id, and an address holds one", op, req.Type, len(found))
	}
	imported, err := w.decode(found[0].state)
	if err != nil {
		return ImportResponse{}, decodeError(op, req.Type, err)
	}
	read, err := c.Read(ctx, ReadRequest{Type: req.Type, Current: imported, Private: found[0].private, Identity: identityOf(found[0].identity)})
	if err != nil {
		// The imported state says what the read's error may quote.
		return ImportResponse{State: imported, Private: found[0].private}, err
	}
	resource, err := c.wire.provider.ResourceByWire(req.Type)
	if err != nil {
		return ImportResponse{State: read.New, Private: read.Private}, err
	}
	if read.New.Kind() != value.Null && resource.SettlingRead && !req.OneRead {
		diags = append(diags, read.Warnings...)
		first := read
		read, err = c.Read(ctx, ReadRequest{Type: req.Type, Current: first.New, Private: first.Private, Identity: first.Identity})
		if err != nil {
			return ImportResponse{State: first.New, Private: first.Private}, err
		}
	}
	return ImportResponse{State: read.New, Private: read.Private, Identity: read.Identity, Warnings: append(diags, read.Warnings...)}, nil
}

// heldError marks the provider text in err held: each diagnostic of an
// *Error, and a *ProcessError.
func heldError(err error) error {
	if pe, ok := errors.AsType[*Error](err); ok {
		pe.Diags = slices.Clone(pe.Diags)
		for i := range pe.Diags {
			pe.Diags[i].Held = true
		}
	}
	if xe, ok := errors.AsType[*ProcessError](err); ok {
		xe.Held = true
	}
	return err
}

// Validate calls ValidateResourceConfig. Error diagnostics are in the list;
// the error reports only a failure to ask the provider.
func (c *Client) Validate(ctx context.Context, typ string, config value.Value) ([]Diag, error) {
	const op = "ValidateResourceConfig"
	w, err := c.wire.resource(typ)
	if err != nil {
		return nil, err
	}
	raw, err := w.encode(config)
	if err != nil {
		return nil, encodeError(op, typ, "config", err)
	}
	ctx, cancel := context.WithTimeout(ctx, quickTimeout)
	defer cancel()
	c.hold(w.block, config)
	wd, err := c.proto.validate(ctx, typ, raw)
	if err != nil {
		return nil, c.callError(op, typ, err)
	}
	return c.diags(w.wireBlock, wd), nil
}

// ready prepares a started provider for calls. A provider without the
// get_provider_schema_optional capability refuses to serve until it has sent
// its schema once, even though Writ reads the schema from the cache.
func (c *Client) ready(ctx context.Context, getSchemaOptional bool) error {
	if !getSchemaOptional {
		sctx, cancel := context.WithTimeout(ctx, quickTimeout)
		s, err := c.proto.getSchema(sctx)
		cancel()
		if err != nil {
			return c.callError("GetProviderSchema", "", err)
		}
		if diags := c.diags(wireBlock{}, s.diags); hasError(diags) {
			return &Error{Op: "GetProviderSchema", Diags: diags}
		}
	}
	return nil
}

// configure calls ConfigureProvider. A null config is an empty provider
// block.
func (c *Client) configure(ctx context.Context, config value.Value) error {
	const op = "ConfigureProvider"
	if config.Kind() == value.Null {
		config = value.ObjectVal(nil)
	}
	raw, err := c.wire.config.encode(config)
	if err != nil {
		return encodeError(op, "", "provider config", err)
	}
	ctx, cancel := context.WithTimeout(ctx, quickTimeout)
	defer cancel()
	c.hold(c.wire.config.block, config)
	wd, err := c.proto.configure(ctx, raw)
	if err != nil {
		return c.callError(op, "", err)
	}
	if diags := c.diags(c.wire.config, wd); hasError(diags) {
		return &Error{Op: op, Diags: diags}
	}
	return nil
}

func (c *Client) diags(b wireBlock, wd []wireDiag) []Diag {
	if len(wd) == 0 {
		return nil
	}
	out := make([]Diag, len(wd))
	for i, d := range wd {
		out[i] = Diag{Error: d.error, Summary: d.summary, Detail: d.detail, Path: b.path(d.path), Held: c.held.Load()}
	}
	// The legacy SDK validates a block's attributes in Go map order, so its
	// diagnostics arrive in another order on each call.
	slices.SortStableFunc(out, func(x, y Diag) int {
		return cmp.Or(strings.Compare(x.Path.String(), y.Path.String()), strings.Compare(x.Summary, y.Summary), strings.Compare(x.Detail, y.Detail))
	})
	return out
}

func hasError(ds []Diag) bool {
	for _, d := range ds {
		if d.Error {
			return true
		}
	}
	return false
}

// callError reports a failed RPC. A provider that crashed explains why on
// stderr, so the captured tail goes into the message.
func (c *Client) callError(op, typ string, err error) error {
	what := op
	if typ != "" {
		what += " " + typ
	}
	pe := c.proc.failed(err)
	pe.What, pe.Held = fmt.Sprintf("provider %s: %s", c.src, what), c.held.Load()
	return pe
}

func encodeError(op, typ, what string, err error) error {
	return fmt.Errorf("%s %s: encode %s: %w", op, typ, what, err)
}

func decodeError(op, typ string, err error) error {
	return fmt.Errorf("%s %s: decode the provider's answer: %w", op, typ, err)
}

// PatchedNomad reports the patch contract of the cached job schema.
func (c *Client) PatchedNomad() bool { return c.nomadPatched }
