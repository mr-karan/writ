package provider

import (
	"fmt"
	"math"
	"math/big"
	"slices"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
	ctymsgpack "github.com/zclconf/go-cty/cty/msgpack"

	"github.com/mr-karan/writ/internal/schema"
	"github.com/mr-karan/writ/internal/types"
	"github.com/mr-karan/writ/internal/value"
)

// This file is where Writ values meet cty. Every conversion is type-directed:
// it walks the annotated Writ block and the cty type together, so Writ needs
// no type information inside its values.
//
// The two sides differ in three ways:
//   - A nested block that IsRecord is a Writ record. On the wire a single
//     block is an object, and a list or set block is a one-element list, or
//     an empty one when the record is null.
//   - An absent list, set, or map block is null in Writ and an empty
//     collection on the wire, as Terraform sends it.
//   - Sets are Writ lists, in cty's set order, which is stable for equal sets.

// dynamic is a protocol DynamicValue. Providers answer in msgpack, but the
// protocol allows JSON, and a missing value means null.
type dynamic struct {
	msgpack []byte
	json    []byte
}

// encode converts a Writ value of this block to msgpack.
func (b wireBlock) encode(v value.Value) ([]byte, error) {
	cv, err := encodeBlock(b.block, b.ty, v, nil)
	if err != nil {
		return nil, err
	}
	return ctymsgpack.Marshal(cv, b.ty)
}

// decode converts a value of this block from the wire. Attributes the schema
// marks sensitive get the secret mark here, which is how provider-produced
// secrets enter Writ.
func (b wireBlock) decode(d dynamic) (value.Value, error) {
	var cv cty.Value
	var err error
	switch {
	case len(d.msgpack) > 0:
		cv, err = ctymsgpack.Unmarshal(d.msgpack, b.ty)
	case len(d.json) > 0:
		cv, err = ctyjson.Unmarshal(d.json, b.ty)
	default:
		return value.NullVal(), nil
	}
	if err != nil {
		return value.Value{}, err
	}
	return decodeBlock(b.block, cv, nil)
}

func encodeBlock(b schema.Block, ty cty.Type, v value.Value, p value.Path) (cty.Value, error) {
	switch v.Kind() {
	case value.Null:
		return cty.NullVal(ty), nil
	case value.Unknown:
		return cty.UnknownVal(ty), nil
	case value.Object:
	default:
		return cty.NilVal, kindError(p, "an object", v)
	}
	for _, k := range v.Keys() {
		if !ty.HasAttribute(k) {
			return cty.NilVal, fmt.Errorf("%s: the schema has no attribute or block %q", pathOrRoot(p), k)
		}
	}
	vals := make(map[string]cty.Value, len(b.Attributes)+len(b.Blocks))
	for _, a := range b.Attributes {
		cv, err := encodeValue(v.Get(a.Name), ty.AttributeType(a.Name), p.Attr(a.Name))
		if err != nil {
			return cty.NilVal, err
		}
		vals[a.Name] = cv
	}
	for _, nb := range b.Blocks {
		cv, err := encodeNested(nb, ty.AttributeType(nb.Name), v.Get(nb.Name), p.Attr(nb.Name))
		if err != nil {
			return cty.NilVal, err
		}
		vals[nb.Name] = cv
	}
	return cty.ObjectVal(vals), nil
}

func encodeNested(nb schema.NestedBlock, ty cty.Type, v value.Value, p value.Path) (cty.Value, error) {
	if nb.Nesting == schema.NestSingle {
		return encodeBlock(nb.Block, ty, v, p)
	}
	ety := ty.ElementType()
	switch v.Kind() {
	case value.Unknown:
		return cty.UnknownVal(ty), nil
	case value.Null:
		if nb.Nesting == schema.NestMap {
			return cty.MapValEmpty(ety), nil
		}
		return collection(ty, nil), nil
	}
	if nb.IsRecord() {
		elem, err := encodeBlock(nb.Block, ety, v, p)
		if err != nil {
			return cty.NilVal, err
		}
		return collection(ty, []cty.Value{elem}), nil
	}
	if nb.Nesting == schema.NestMap {
		if v.Kind() != value.Object {
			return cty.NilVal, kindError(p, "a map of blocks", v)
		}
		elems := make(map[string]cty.Value, v.Len())
		for _, k := range v.Keys() {
			elem, err := encodeBlock(nb.Block, ety, v.Get(k), p.Key(k))
			if err != nil {
				return cty.NilVal, err
			}
			elems[k] = elem
		}
		if len(elems) == 0 {
			return cty.MapValEmpty(ety), nil
		}
		return cty.MapVal(elems), nil
	}
	if v.Kind() != value.List {
		return cty.NilVal, kindError(p, "a list of blocks", v)
	}
	elems := make([]cty.Value, v.Len())
	for i := range elems {
		elem, err := encodeBlock(nb.Block, ety, v.Index(i), p.Index(int64(i)))
		if err != nil {
			return cty.NilVal, err
		}
		elems[i] = elem
	}
	return collection(ty, elems), nil
}

func encodeValue(v value.Value, ty cty.Type, p value.Path) (cty.Value, error) {
	switch v.Kind() {
	case value.Null:
		return cty.NullVal(ty), nil
	case value.Unknown:
		return cty.UnknownVal(ty), nil
	}
	switch {
	case ty == cty.String && v.Kind() == value.String:
		return cty.StringVal(v.AsString()), nil
	case ty == cty.Bool && v.Kind() == value.Bool:
		return cty.BoolVal(v.AsBool()), nil
	case ty == cty.Number && v.Kind() == value.Int:
		return cty.NumberIntVal(v.AsInt()), nil
	case ty == cty.Number && v.Kind() == value.Float:
		f := v.AsFloat()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return cty.NilVal, fmt.Errorf("%s: %v is not a number a provider accepts", pathOrRoot(p), f)
		}
		return cty.NumberFloatVal(f), nil
	case (ty.IsListType() || ty.IsSetType()) && v.Kind() == value.List:
		elems := make([]cty.Value, v.Len())
		for i := range elems {
			e, err := encodeValue(v.Index(i), ty.ElementType(), p.Index(int64(i)))
			if err != nil {
				return cty.NilVal, err
			}
			elems[i] = e
		}
		return collection(ty, elems), nil
	case ty.IsMapType() && v.Kind() == value.Object:
		elems := make(map[string]cty.Value, v.Len())
		for _, k := range v.Keys() {
			e, err := encodeValue(v.Get(k), ty.ElementType(), p.Key(k))
			if err != nil {
				return cty.NilVal, err
			}
			elems[k] = e
		}
		if len(elems) == 0 {
			return cty.MapValEmpty(ty.ElementType()), nil
		}
		return cty.MapVal(elems), nil
	case ty.IsObjectType() && v.Kind() == value.Object:
		for _, k := range v.Keys() {
			if !ty.HasAttribute(k) {
				return cty.NilVal, fmt.Errorf("%s: unexpected attribute %q", pathOrRoot(p), k)
			}
		}
		attrs := make(map[string]cty.Value, len(ty.AttributeTypes()))
		for name, aty := range ty.AttributeTypes() {
			e, err := encodeValue(v.Get(name), aty, p.Attr(name))
			if err != nil {
				return cty.NilVal, err
			}
			attrs[name] = e
		}
		return cty.ObjectVal(attrs), nil
	}
	return cty.NilVal, kindError(p, ty.FriendlyName(), v)
}

// collection builds a list or set value of type ty. cty needs the element
// type spelled out for an empty one.
func collection(ty cty.Type, elems []cty.Value) cty.Value {
	ety := ty.ElementType()
	switch {
	case ty.IsSetType() && len(elems) == 0:
		return cty.SetValEmpty(ety)
	case ty.IsSetType():
		return cty.SetVal(elems)
	case len(elems) == 0:
		return cty.ListValEmpty(ety)
	}
	return cty.ListVal(elems)
}

func kindError(p value.Path, want string, v value.Value) error {
	return fmt.Errorf("%s: want %s, got %s", pathOrRoot(p), want, v.Kind())
}

func pathOrRoot(p value.Path) string {
	if len(p) == 0 {
		return "resource"
	}
	return p.String()
}

func decodeBlock(b schema.Block, cv cty.Value, p value.Path) (value.Value, error) {
	if !cv.IsKnown() {
		return value.UnknownVal(false), nil
	}
	if cv.IsNull() {
		return value.NullVal(), nil
	}
	fields := make(map[string]value.Value, len(b.Attributes)+len(b.Blocks))
	for _, a := range b.Attributes {
		fields[a.Name] = decodeValue(cv.GetAttr(a.Name), a.Type, a.Sensitive || a.Type.IsSecret())
	}
	for _, nb := range b.Blocks {
		v, err := decodeNested(nb, cv.GetAttr(nb.Name), p.Attr(nb.Name))
		if err != nil {
			return value.Value{}, err
		}
		fields[nb.Name] = v
	}
	return value.ObjectVal(fields), nil
}

func decodeNested(nb schema.NestedBlock, cv cty.Value, p value.Path) (value.Value, error) {
	if nb.Nesting == schema.NestSingle {
		return decodeBlock(nb.Block, cv, p)
	}
	if !cv.IsKnown() {
		return value.UnknownVal(false), nil
	}
	if cv.IsNull() {
		return value.NullVal(), nil
	}
	if nb.IsRecord() {
		var elems []cty.Value
		for _, e := range cv.Elements() {
			elems = append(elems, e)
		}
		switch len(elems) {
		case 0:
			return value.NullVal(), nil
		case 1:
			return decodeBlock(nb.Block, elems[0], p)
		}
		return value.Value{}, fmt.Errorf("%s: provider sent %d blocks where writ expects at most one", p, len(elems))
	}
	if nb.Nesting == schema.NestMap {
		fields := make(map[string]value.Value, cv.LengthInt())
		for k, e := range cv.Elements() {
			v, err := decodeBlock(nb.Block, e, p.Key(k.AsString()))
			if err != nil {
				return value.Value{}, err
			}
			fields[k.AsString()] = v
		}
		return value.ObjectVal(fields), nil
	}
	var elems []value.Value
	for _, e := range cv.Elements() {
		v, err := decodeBlock(nb.Block, e, p.Index(int64(len(elems))))
		if err != nil {
			return value.Value{}, err
		}
		elems = append(elems, v)
	}
	return value.ListVal(elems), nil
}

// decodeValue converts an attribute value. A node whose Writ type is a secret
// is marked, and the mark covers everything inside it.
//
// A number becomes an Int when it is integral and fits in an int64, and a
// Float otherwise. The schema types every number Float, and Int flows into
// Float, so the narrowing is sound, and it makes decoding canonical: 3 and
// 3.0 from a provider are the same Writ value.
func decodeValue(cv cty.Value, t types.Type, secret bool) value.Value {
	secret = secret || t.IsSecret()
	if !cv.IsKnown() {
		return value.UnknownVal(secret)
	}
	v := value.NullVal()
	ty := cv.Type()
	switch {
	case cv.IsNull():
	case ty == cty.String:
		v = value.StringVal(cv.AsString())
	case ty == cty.Bool:
		v = value.BoolVal(cv.True())
	case ty == cty.Number:
		v = number(cv.AsBigFloat())
	case ty.IsListType(), ty.IsSetType(), ty.IsTupleType():
		var elems []value.Value
		for _, e := range cv.Elements() {
			elems = append(elems, decodeValue(e, t.Elem(), false))
		}
		v = value.ListVal(elems)
	case ty.IsMapType():
		fields := make(map[string]value.Value)
		for k, e := range cv.Elements() {
			fields[k.AsString()] = decodeValue(e, t.Elem(), false)
		}
		v = value.ObjectVal(fields)
	case ty.IsObjectType():
		fields := make(map[string]value.Value, len(ty.AttributeTypes()))
		for name := range ty.AttributeTypes() {
			f, _ := t.Field(name)
			fields[name] = decodeValue(cv.GetAttr(name), f.Type, false)
		}
		v = value.ObjectVal(fields)
	}
	if secret {
		v = v.Secret()
	}
	return v
}

func number(f *big.Float) value.Value {
	if f.IsInt() {
		if i, acc := f.Int64(); acc == big.Exact {
			return value.IntVal(i)
		}
	}
	x, _ := f.Float64()
	return value.FloatVal(x)
}

// wireStep is one step of a protocol AttributePath.
type wireStep struct {
	kind  stepKind
	name  string // attribute name or string key
	index int64
}

type stepKind uint8

const (
	stepAttr stepKind = iota + 1
	stepKey
	stepIndex
)

// path converts a protocol attribute path into the Writ path of the same
// location. The index step after a block that Writ shows as a record is
// dropped, so subject[0].common_name becomes subject.common_name.
func (b wireBlock) path(steps []wireStep) value.Path {
	var p value.Path
	cur, inBlock := b.block, true
	for i := 0; i < len(steps); i++ {
		s := steps[i]
		if inBlock && s.kind == stepAttr {
			if j := slices.IndexFunc(cur.Blocks, func(nb schema.NestedBlock) bool { return nb.Name == s.name }); j >= 0 {
				nb := cur.Blocks[j]
				p, cur = p.Attr(s.name), nb.Block
				if nb.Nesting != schema.NestSingle && i+1 < len(steps) && steps[i+1].kind != stepAttr {
					i++
					if !nb.IsRecord() {
						p = appendStep(p, steps[i])
					}
				}
				continue
			}
		}
		inBlock = false
		p = appendStep(p, s)
	}
	return p
}

func appendStep(p value.Path, s wireStep) value.Path {
	switch s.kind {
	case stepKey:
		return p.Key(s.name)
	case stepIndex:
		return p.Index(s.index)
	}
	return p.Attr(s.name)
}

func (w wireResource) fillReadDefaults(v value.Value) (value.Value, error) {
	if v.Kind() != value.Object || len(w.readDefaults) == 0 {
		return v, nil
	}
	fields := make(map[string]value.Value, len(v.Keys()))
	for _, k := range v.Keys() {
		fields[k] = v.Get(k)
	}
	for _, a := range w.block.Attributes {
		raw, ok := w.readDefaults[a.Name]
		if !ok || v.Get(a.Name).Kind() != value.Null {
			continue
		}
		if !a.Optional || a.Computed || a.Sensitive || a.WriteOnly {
			return value.Value{}, fmt.Errorf("invalid read default for %s", a.Name)
		}
		cv, err := ctyjson.Unmarshal([]byte(raw), w.ty.AttributeType(a.Name))
		if err != nil {
			return value.Value{}, fmt.Errorf("read default for %s: %w", a.Name, err)
		}
		fields[a.Name] = decodeValue(cv, a.Type, false)
	}
	return value.ObjectVal(fields), nil
}
