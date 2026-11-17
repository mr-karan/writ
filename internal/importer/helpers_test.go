package importer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"testing"

	"github.com/mr-karan/writ/internal/addr"
	"github.com/mr-karan/writ/internal/diag"
	"github.com/mr-karan/writ/internal/schema"
	"github.com/mr-karan/writ/internal/types"
	"github.com/mr-karan/writ/internal/value"
)

// testObject is one object a test imports through declare.
type testObject struct {
	Schema         *schema.Resource
	State          value.Value
	ProviderConfig value.Value
	// Plan is nil when the reader cannot plan.
	Plan Planner
}

type oneReader struct {
	o   testObject
	ask Validator
}

func (r oneReader) Read(context.Context, types.Name, addr.Addr, string) (Found, diag.List, error) {
	return Found{Schema: r.o.Schema, State: r.o.State, ProviderConfig: r.o.ProviderConfig}, nil, nil
}

func (r oneReader) Validator(types.Name, addr.Addr) Validator { return r.ask }

func (r oneReader) Planner(types.Name, addr.Addr, Found) Planner { return r.o.Plan }

var hclAligned = regexp.MustCompile(`(?m)^(\s*[^\s=]+)\s+= `)

// declare imports o alone through Bulk, asking ask whether the provider
// accepts a config. It returns the OpenTofu code, each attribute written
// `name = value` with one space, and the reason of each problem.
func declare(t *testing.T, o testObject, ask Validator) (string, []string) {
	t.Helper()
	res, err := Bulk(context.Background(), BulkOptions{
		Selections: []Selection{{Type: o.Schema.Name, IDs: []string{"edge"}}},
		Reader:     oneReader{o: o, ask: ask},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed() {
		t.Fatalf("bulk refused the object: %v", res.Problems)
	}
	var notes []string
	for _, p := range res.Problems {
		notes = append(notes, p.Reason)
	}
	return hclAligned.ReplaceAllString(string(res.Code), "$1 = "), notes
}

var publicSchema = &schema.Resource{Block: schema.Block{Attributes: []schema.Attribute{{Name: "note", Type: types.TString, Optional: true}}}}

var errLeak = errors.New("the object would write a known secret where it is not secret")

// check runs the secret checks Bulk runs on an object a provider read by
// id: errLeak when the id or a value outside a secret exposes a secret
// another owner keeps, and a *LimitError when k and read hold more secret
// bytes than an import compares values against. A value of k.Secrets that
// is not marked secret stands for a public value of another object of the
// run, whose certificates vouch for public material as the run's do.
func check(k Known, res *schema.Resource, id string, read value.Value) error {
	var known Known
	s := newSecretSet()
	for _, v := range k.Secrets {
		if v.IsSecret() {
			known.Secrets = append(known.Secrets, v)
			continue
		}
		s.addState(&node{}, publicSchema.Block, value.ObjectVal(map[string]value.Value{"note": v}))
	}
	for _, v := range known.Secrets {
		s.addValue(nil, v)
	}
	self := &node{}
	s.addState(self, res.Block, read)
	if err := s.ready(); err != nil {
		return err
	}
	if s.idLeaks(self, id) || s.inClear(res.Block, read, self) {
		return errLeak
	}
	return nil
}

// decodeJSON reads JSON with numbers kept as text, so an integer never passes
// through a float64.
func decodeJSON(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// decodeState converts a resource's attributes, as a state file of the
// current schema version holds them, into the value the provider package
// decodes for the same object: every schema attribute present, null when
// absent; record blocks as records; whole numbers as Int; and attributes the
// schema marks sensitive marked secret.
func decodeState(b schema.Block, raw json.RawMessage) (value.Value, error) {
	tree, err := decodeJSON(raw)
	if err != nil {
		return value.Value{}, err
	}
	obj, ok := tree.(map[string]any)
	if !ok {
		return value.Value{}, fmt.Errorf("attributes are %T, want an object", tree)
	}
	return decodeBlock(b, obj, nil)
}

func decodeBlock(b schema.Block, obj map[string]any, p value.Path) (value.Value, error) {
	fields := make(map[string]value.Value, len(b.Attributes)+len(b.Blocks))
	for _, a := range b.Attributes {
		v, err := decodeAttr(obj[a.Name], a.Type, a.Sensitive, p.Attr(a.Name))
		if err != nil {
			return value.Value{}, err
		}
		fields[a.Name] = v
	}
	for _, nb := range b.Blocks {
		v, err := decodeNested(nb, obj[nb.Name], p.Attr(nb.Name))
		if err != nil {
			return value.Value{}, err
		}
		fields[nb.Name] = v
	}
	return value.ObjectVal(fields), nil
}

func decodeNested(nb schema.NestedBlock, x any, p value.Path) (value.Value, error) {
	if x == nil {
		return value.NullVal(), nil
	}
	if nb.Nesting == schema.NestSingle {
		obj, ok := x.(map[string]any)
		if !ok {
			return value.Value{}, fmt.Errorf("%s: block is %T, want an object", p, x)
		}
		return decodeBlock(nb.Block, obj, p)
	}
	if nb.Nesting == schema.NestMap {
		obj, ok := x.(map[string]any)
		if !ok {
			return value.Value{}, fmt.Errorf("%s: map block is %T, want an object", p, x)
		}
		fields := make(map[string]value.Value, len(obj))
		for k, e := range obj {
			eo, ok := e.(map[string]any)
			if !ok {
				return value.Value{}, fmt.Errorf("%s: block is %T, want an object", p.Key(k), e)
			}
			v, err := decodeBlock(nb.Block, eo, p.Key(k))
			if err != nil {
				return value.Value{}, err
			}
			fields[k] = v
		}
		return value.ObjectVal(fields), nil
	}
	list, ok := x.([]any)
	if !ok {
		return value.Value{}, fmt.Errorf("%s: block list is %T, want an array", p, x)
	}
	if nb.IsRecord() && len(list) > 1 {
		return value.Value{}, fmt.Errorf("%s: state has %d blocks where writ expects at most one", p, len(list))
	}
	elems := make([]value.Value, 0, len(list))
	for i, e := range list {
		eo, ok := e.(map[string]any)
		if !ok {
			return value.Value{}, fmt.Errorf("%s: block is %T, want an object", p.Index(int64(i)), e)
		}
		v, err := decodeBlock(nb.Block, eo, p.Index(int64(i)))
		if err != nil {
			return value.Value{}, err
		}
		elems = append(elems, v)
	}
	if nb.IsRecord() {
		if len(elems) == 0 {
			return value.NullVal(), nil
		}
		return elems[0], nil
	}
	return value.ListVal(elems), nil
}

// decodeAttr converts one attribute value by its Writ type. A node whose type
// is a secret is marked, and the mark covers everything inside it.
func decodeAttr(x any, t types.Type, secret bool, p value.Path) (value.Value, error) {
	secret = secret || t.IsSecret()
	var v value.Value
	switch x := x.(type) {
	case nil:
		v = value.NullVal()
	case string:
		v = value.StringVal(x)
	case bool:
		v = value.BoolVal(x)
	case json.Number:
		n, err := number(x)
		if err != nil {
			return value.Value{}, fmt.Errorf("%s: %w", p, err)
		}
		v = n
	case []any:
		elems := make([]value.Value, len(x))
		for i, e := range x {
			ev, err := decodeAttr(e, t.Elem(), false, p.Index(int64(i)))
			if err != nil {
				return value.Value{}, err
			}
			elems[i] = ev
		}
		v = value.ListVal(elems)
	case map[string]any:
		fields := make(map[string]value.Value, len(x))
		switch t.Kind() {
		case types.Record:
			for _, f := range t.Fields() {
				fv, err := decodeAttr(x[f.Name], f.Type, false, p.Attr(f.Name))
				if err != nil {
					return value.Value{}, err
				}
				fields[f.Name] = fv
			}
		default:
			for k, e := range x {
				ev, err := decodeAttr(e, t.Elem(), false, p.Key(k))
				if err != nil {
					return value.Value{}, err
				}
				fields[k] = ev
			}
		}
		v = value.ObjectVal(fields)
	default:
		return value.Value{}, fmt.Errorf("%s: unexpected JSON %T", p, x)
	}
	if secret {
		v = v.Secret()
	}
	return v, nil
}

// number reads a JSON number as Int when it is whole and fits an int64, and
// as Float otherwise, as the provider package decodes numbers.
func number(n json.Number) (value.Value, error) {
	if i, err := strconv.ParseInt(n.String(), 10, 64); err == nil {
		return value.IntVal(i), nil
	}
	f, err := strconv.ParseFloat(n.String(), 64)
	if err != nil {
		return value.Value{}, fmt.Errorf("number %s: %w", n, err)
	}
	if f == math.Trunc(f) && f >= math.MinInt64 && f < math.MaxInt64 {
		return value.IntVal(int64(f)), nil
	}
	return value.FloatVal(f), nil
}
