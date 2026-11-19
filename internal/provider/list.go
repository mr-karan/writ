package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
	ctymsgpack "github.com/zclconf/go-cty/cty/msgpack"

	"github.com/mr-karan/writ/internal/value"
)

// Bounds on what a listing keeps. The object limit bounds the ids, and
// these bound what a provider sends besides them.
const (
	// MaxImportIDBytes bounds one import id. An AWS ARN is at most 2,048
	// bytes.
	MaxImportIDBytes = 2048
	// maxIdentityBytes bounds the encoded identity of one listed object,
	// which may hold a few attributes besides its import id.
	maxIdentityBytes = 16 << 10
	// maxListWarnings and maxListWarningBytes bound the warnings a
	// listing keeps. The rest are counted in one more warning.
	maxListWarnings     = 20
	maxListWarningBytes = 16 << 10
	// maxListWarningPathBytes bounds the printed path of a kept warning.
	// A longer path is cut to the steps that fit.
	maxListWarningPathBytes = 1 << 10
)

// listRequestLimit is the number of objects Writ asks a provider for, to
// find limit distinct ids. A provider may list one object more than once,
// so the stream may hold repeats, and the request leaves room for them.
func listRequestLimit(limit int) int { return 4*limit + 16 }

// NoListError says a provider cannot list the objects of a type by import
// id: it has no list resource of the type, or the type's resource identity
// is not one string.
type NoListError struct {
	Provider string
	Type     string
	Reason   string
}

func (e *NoListError) Error() string {
	return fmt.Sprintf("provider %s %s", e.Provider, e.Reason)
}

// List calls ListResource for the resource type typ, with query as the
// list config, and returns the distinct import id of each object, at most
// limit. Fewer than limit means the provider listed every object. It asks
// the provider for listRequestLimit(limit) objects, skips an id it already
// has, and fails when the provider sends that many before limit distinct
// ids, since the list then may hold more. A null query is an empty config. The import id is the one identity
// attribute the provider requires for import, as `id` for aws_subnet and
// `name` for aws_iam_role. An error event fails the whole listing, so a
// caller never takes a partial list for all of it.
func (c *Client) List(ctx context.Context, typ string, query value.Value, limit int) ([]string, []Diag, error) {
	const op = "ListResource"
	sctx, cancel := context.WithTimeout(ctx, quickTimeout)
	info, wd, err := readListInfo(sctx, c.proto, typ)
	cancel()
	if err != nil {
		return nil, nil, c.callError("GetProviderSchema", "", err)
	}
	if diags := c.diags(wireBlock{}, wd); hasError(diags) {
		return nil, nil, &Error{Op: "GetProviderSchema", Diags: diags}
	}
	if info.config == nil {
		return nil, nil, &NoListError{Provider: c.src.String(), Type: typ, Reason: "does not list objects of " + typ}
	}
	key, idType, err := importKey(info.identity)
	if err != nil {
		return nil, nil, &NoListError{Provider: c.src.String(), Type: typ, Reason: fmt.Sprintf("lists objects of %s, but %s", typ, err)}
	}
	block, ty, err := convertBlock(*info.config)
	if err != nil {
		return nil, nil, decodeError("GetProviderSchema", typ, err)
	}
	w := wireBlock{block: block, ty: ty}
	if query.Kind() == value.Null {
		query = value.ObjectVal(nil)
	}
	config, err := w.encode(query)
	if err != nil {
		return nil, nil, fmt.Errorf("the list query for %s: %w", typ, err)
	}
	request := listRequestLimit(limit)
	c.hold(block, query)
	if err := c.validateList(ctx, w, typ, config, request); err != nil {
		return nil, nil, err
	}
	lctx, cancel := context.WithTimeout(ctx, resourceTimeout)
	// Cancelling ends the stream when the limit stops it early.
	defer cancel()
	next, err := c.proto.list(lctx, typ, config, int64(request))
	if err != nil {
		return nil, nil, c.callError(op, typ, err)
	}
	var ids []string
	seen := map[string]bool{}
	var warnings []Diag
	warnBytes, dropped, results := 0, 0, 0
	// The loop stops at the request without another receive, since a
	// provider may leave the stream open; the deferred cancel ends it.
	for len(ids) < limit && results < request {
		ev, err := next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, c.callError(op, typ, err)
		}
		diags := c.diags(w, ev.diags)
		if hasError(diags) {
			return nil, nil, &Error{Op: op, Type: typ, Diags: diags}
		}
		for _, d := range diags {
			d.Path = cutPath(d.Path, maxListWarningPathBytes)
			n := len(d.Summary) + len(d.Detail) + len(d.Path.String())
			if len(warnings) >= maxListWarnings || warnBytes+n > maxListWarningBytes {
				dropped++
				continue
			}
			warnings, warnBytes = append(warnings, d), warnBytes+n
		}
		size := len(ev.identity.msgpack) + len(ev.identity.json)
		if size == 0 {
			continue
		}
		if size > maxIdentityBytes {
			return nil, nil, fmt.Errorf("%s %s: an identity is %d bytes long, more than the limit of %d", op, typ, size, maxIdentityBytes)
		}
		results++
		id, err := identityID(ev.identity, idType, key)
		if err != nil {
			return nil, nil, decodeError(op, typ, err)
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) < limit && results >= request {
		return nil, nil, fmt.Errorf("%s %s: the provider sent %d objects with only %d distinct ids, so its repeats may hide more objects; narrow the query", op, typ, results, len(ids))
	}
	if dropped > 0 {
		warnings = append(warnings, Diag{Summary: fmt.Sprintf("the provider sent %d more warnings for this listing, which are not kept", dropped), Held: c.held.Load()})
	}
	return ids, warnings, nil
}

// validateList calls ValidateListResourceConfig with the encoded config,
// include_resource_object false, and the limit, and fails on any error it
// returns.
func (c *Client) validateList(ctx context.Context, w wireBlock, typ string, config []byte, limit int) error {
	const op = "ValidateListResourceConfig"
	objects, err := ctymsgpack.Marshal(cty.False, cty.Bool)
	if err != nil {
		return encodeError(op, typ, "include_resource_object", err)
	}
	lim, err := ctymsgpack.Marshal(cty.NumberIntVal(int64(limit)), cty.Number)
	if err != nil {
		return encodeError(op, typ, "limit", err)
	}
	vctx, cancel := context.WithTimeout(ctx, quickTimeout)
	defer cancel()
	wd, err := c.proto.validateList(vctx, typ, config, objects, lim)
	if err != nil {
		return c.callError(op, typ, err)
	}
	if diags := c.diags(w, wd); hasError(diags) {
		return &Error{Op: op, Type: typ, Diags: diags}
	}
	return nil
}

// importKey returns the one identity attribute required for import, and
// the identity's cty type. An identity of more attributes needs import by
// identity, which Writ does not do.
func importKey(attrs []identityAttribute) (string, cty.Type, error) {
	if len(attrs) == 0 {
		return "", cty.NilType, errors.New("gives them no resource identity, so they have no import id")
	}
	fields := make(map[string]cty.Type, len(attrs))
	var required []string
	for _, a := range attrs {
		ty, err := ctyjson.UnmarshalType(a.typ)
		if err != nil {
			return "", cty.NilType, fmt.Errorf("gives identity attribute %s a type Writ cannot read: %w", a.name, err)
		}
		fields[a.name] = ty
		if a.required {
			required = append(required, a.name)
		}
	}
	switch {
	case len(required) == 0:
		return "", cty.NilType, errors.New("names none of their identity attributes as required for import, so they have no import id")
	case len(required) > 1:
		return "", cty.NilType, fmt.Errorf("names each by an identity of %s, and Writ imports by one id", strings.Join(required, ", "))
	case fields[required[0]] != cty.String:
		return "", cty.NilType, fmt.Errorf("names each by %s, which is not a string", required[0])
	}
	return required[0], cty.Object(fields), nil
}

func identityID(d dynamic, ty cty.Type, key string) (string, error) {
	var v cty.Value
	var err error
	if len(d.msgpack) > 0 {
		v, err = ctymsgpack.Unmarshal(d.msgpack, ty)
	} else {
		v, err = ctyjson.Unmarshal(d.json, ty)
	}
	if err != nil {
		return "", fmt.Errorf("identity: %w", err)
	}
	if v.IsNull() || !v.IsKnown() {
		return "", errors.New("identity is null")
	}
	id := v.GetAttr(key)
	switch {
	case id.IsNull() || !id.IsKnown() || id.AsString() == "":
		return "", fmt.Errorf("identity has no %s", key)
	case len(id.AsString()) > MaxImportIDBytes:
		return "", fmt.Errorf("identity %s is %d bytes long, more than the limit of %d", key, len(id.AsString()), MaxImportIDBytes)
	}
	return id.AsString(), nil
}

// cutPath returns the longest prefix of p whose printed form has at most
// limit bytes. A cut prefix is a copy, so the steps it drops, and the long
// keys they hold, can be freed.
func cutPath(p value.Path, limit int) value.Path {
	for i := range p {
		if len(p[:i+1].String()) > limit {
			return slices.Clone(p[:i])
		}
	}
	return p
}
