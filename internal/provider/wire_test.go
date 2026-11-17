package provider

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"
	ctymsgpack "github.com/zclconf/go-cty/cty/msgpack"

	"github.com/mr-karan/writ/internal/schema"
	"github.com/mr-karan/writ/internal/types"
	"github.com/mr-karan/writ/internal/value"
)

// kindsSchema has one attribute of every cty kind and one block of every
// nesting, under a source the annotation table does not know.
const kindsSchema = `{
  "format": 1, "protocol": 5, "get_schema_optional": true,
  "provider": {"attributes": [
    {"name": "token", "type": "string", "optional": true, "sensitive": true},
    {"name": "region", "type": "string", "optional": true}
  ]},
  "resources": {"kinds_thing": {"version": 2, "block": {
    "attributes": [
      {"name": "id", "type": "string", "computed": true},
      {"name": "s", "type": "string", "optional": true},
      {"name": "n", "type": "number", "optional": true},
      {"name": "b", "type": "bool", "optional": true},
      {"name": "l", "type": ["list", "string"], "optional": true},
      {"name": "st", "type": ["set", "string"], "optional": true},
      {"name": "m", "type": ["map", "number"], "optional": true},
      {"name": "o", "type": ["object", {"a": "string", "c": "number"}], "optional": true},
      {"name": "secret", "type": "string", "optional": true, "sensitive": true},
      {"name": "secrets", "type": ["list", "string"], "optional": true, "sensitive": true}
    ],
    "blocks": [
      {"name": "single", "nesting": "single", "block": {"attributes": [{"name": "x", "type": "string", "optional": true}]}},
      {"name": "one", "nesting": "list", "max_items": 1, "block": {"attributes": [{"name": "x", "type": "string", "optional": true}]}},
      {"name": "many", "nesting": "list", "block": {"attributes": [{"name": "x", "type": "string", "optional": true}]}},
      {"name": "tags", "nesting": "set", "block": {"attributes": [{"name": "x", "type": "string", "optional": true}]}},
      {"name": "named", "nesting": "map", "block": {"attributes": [{"name": "x", "type": "string", "optional": true}]}}
    ]
  }}}
}`

// tlsSchema is the part of the tls provider's schema that the annotation
// table refines: tls_cert_request's subject is a LIST block with max_items 0
// that Writ shows as a record.
const tlsSchema = `{
  "format": 1, "protocol": 5,
  "provider": {},
  "resources": {"tls_cert_request": {"block": {
    "attributes": [
      {"name": "id", "type": "string", "computed": true},
      {"name": "private_key_pem", "type": "string", "optional": true, "sensitive": true},
      {"name": "cert_request_pem", "type": "string", "computed": true}
    ],
    "blocks": [
      {"name": "subject", "nesting": "list", "block": {"attributes": [
        {"name": "common_name", "type": "string", "optional": true},
        {"name": "street_address", "type": ["list", "string"], "optional": true}
      ]}}
    ]
  }}}
}`

func mustSchema(t *testing.T, src schema.Source, raw string) (*schema.Provider, *wireSchema) {
	t.Helper()
	var cs cachedSchema
	if err := json.Unmarshal([]byte(raw), &cs); err != nil {
		t.Fatal(err)
	}
	p, w, err := buildSchema(src, "1.0.0", &cs)
	if err != nil {
		t.Fatal(err)
	}
	return p, w
}

func kindsWire(t *testing.T) wireResource {
	t.Helper()
	_, w := mustSchema(t, schema.Source{Namespace: "example", Type: "kinds"}, kindsSchema)
	r, err := w.resource("kinds_thing")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func obj(fields map[string]value.Value) value.Value { return value.ObjectVal(fields) }
func str(s string) value.Value                      { return value.StringVal(s) }
func list(vs ...value.Value) value.Value            { return value.ListVal(vs) }

func roundTrip(t *testing.T, w wireBlock, v value.Value) value.Value {
	t.Helper()
	raw, err := w.encode(v)
	if err != nil {
		t.Fatalf("encode %s: %v", v, err)
	}
	got, err := w.decode(dynamic{msgpack: raw})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

// unset is the decoded form of a thing whose config sets nothing: null
// attributes and null single and record blocks, but empty list, set, and map
// blocks, because an absent collection block is an empty collection on the
// wire.
func unset() map[string]value.Value {
	return map[string]value.Value{
		"id": value.NullVal(), "s": value.NullVal(), "n": value.NullVal(), "b": value.NullVal(),
		"l": value.NullVal(), "st": value.NullVal(), "m": value.NullVal(), "o": value.NullVal(),
		"secret": value.NullVal().Secret(), "secrets": value.NullVal().Secret(),
		"single": value.NullVal(), "one": value.NullVal(),
		"many": list(), "tags": list(), "named": obj(nil),
	}
}

func with(base map[string]value.Value, set map[string]value.Value) value.Value {
	m := make(map[string]value.Value, len(base))
	for k, v := range base {
		m[k] = v
	}
	for k, v := range set {
		m[k] = v
	}
	return obj(m)
}

func TestWireRoundTrip(t *testing.T) {
	w := kindsWire(t).wireBlock
	x := func(s string) value.Value { return obj(map[string]value.Value{"x": str(s)}) }
	tests := []struct {
		name string
		in   map[string]value.Value
		want map[string]value.Value // nil means the same as in
	}{
		{name: "nothing set", in: nil},
		{name: "string", in: map[string]value.Value{"s": str("hello")}},
		{name: "bool", in: map[string]value.Value{"b": value.BoolVal(true)}},
		{name: "int", in: map[string]value.Value{"n": value.IntVal(42)}},
		{name: "int beyond float precision", in: map[string]value.Value{"n": value.IntVal(1<<53 + 1)}},
		{name: "negative int", in: map[string]value.Value{"n": value.IntVal(-7)}},
		{name: "fractional float", in: map[string]value.Value{"n": value.FloatVal(2.5)}},
		{
			name: "integral float narrows to int",
			in:   map[string]value.Value{"n": value.FloatVal(3)},
			want: map[string]value.Value{"n": value.IntVal(3)},
		},
		{name: "list", in: map[string]value.Value{"l": list(str("a"), str("b"))}},
		{name: "empty list", in: map[string]value.Value{"l": list()}},
		{name: "map", in: map[string]value.Value{"m": obj(map[string]value.Value{"one": value.IntVal(1), "half": value.FloatVal(0.5)})}},
		{name: "empty map", in: map[string]value.Value{"m": obj(nil)}},
		{
			name: "object fills missing attributes with null",
			in:   map[string]value.Value{"o": obj(map[string]value.Value{"a": str("x")})},
			want: map[string]value.Value{"o": obj(map[string]value.Value{"a": str("x"), "c": value.NullVal()})},
		},
		{name: "single block", in: map[string]value.Value{"single": x("s")}},
		{name: "max_items 1 block as a record", in: map[string]value.Value{"one": x("o")}},
		{name: "list block", in: map[string]value.Value{"many": list(x("a"), x("b"))}},
		{name: "map block", in: map[string]value.Value{"named": obj(map[string]value.Value{"k": x("v")})}},
		{name: "set block", in: map[string]value.Value{"tags": list(x("t"))}},
		{
			name: "sensitive attribute is marked",
			in:   map[string]value.Value{"secret": str("hunter2"), "secrets": list(str("a"))},
			want: map[string]value.Value{"secret": str("hunter2").Secret(), "secrets": list(str("a")).Secret()},
		},
		{
			name: "unknowns",
			in: map[string]value.Value{
				"id": value.UnknownVal(false), "l": value.UnknownVal(false), "m": value.UnknownVal(false),
				"single": value.UnknownVal(false), "one": value.UnknownVal(false), "many": value.UnknownVal(false),
			},
		},
		{name: "unknown inside a list", in: map[string]value.Value{"l": list(value.UnknownVal(false), str("x"))}},
		{name: "unknown inside a block", in: map[string]value.Value{"one": obj(map[string]value.Value{"x": value.UnknownVal(false)})}},
		{
			name: "unknown sensitive attribute stays secret",
			in:   map[string]value.Value{"secret": value.UnknownVal(false)},
			want: map[string]value.Value{"secret": value.UnknownVal(true)},
		},
		{
			name: "null collection blocks become empty",
			in:   map[string]value.Value{"many": value.NullVal(), "tags": value.NullVal(), "named": value.NullVal()},
			want: map[string]value.Value{"many": list(), "tags": list(), "named": obj(nil)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := tt.want
			if want == nil {
				want = tt.in
			}
			got := roundTrip(t, w, obj(tt.in))
			if exp := with(unset(), want); !value.Equal(got, exp) {
				t.Errorf("round trip\n got %s\nwant %s", got, exp)
			}
		})
	}
}

func TestWireNullObject(t *testing.T) {
	w := kindsWire(t).wireBlock
	if got := roundTrip(t, w, value.NullVal()); got.Kind() != value.Null {
		t.Errorf("null state decoded as %s", got)
	}
	got, err := w.decode(dynamic{})
	if err != nil || got.Kind() != value.Null {
		t.Errorf("missing DynamicValue decoded as %s, %v; want null", got, err)
	}
}

func TestWireSetsAreStableLists(t *testing.T) {
	w := kindsWire(t).wireBlock
	ab := roundTrip(t, w, obj(map[string]value.Value{"st": list(str("a"), str("b"))})).Get("st")
	ba := roundTrip(t, w, obj(map[string]value.Value{"st": list(str("b"), str("a"), str("b"))})).Get("st")
	if ab.Kind() != value.List || ab.Len() != 2 {
		t.Fatalf("set decoded as %s, want a two-element list", ab)
	}
	if !value.Equal(ab, ba) {
		t.Errorf("equal sets decoded in different orders: %s and %s", ab, ba)
	}
}

// wireValue decodes encoded bytes with cty, to inspect the wire shape.
func wireValue(t *testing.T, w wireBlock, v value.Value) cty.Value {
	t.Helper()
	raw, err := w.encode(v)
	if err != nil {
		t.Fatal(err)
	}
	cv, err := ctymsgpack.Unmarshal(raw, w.ty)
	if err != nil {
		t.Fatal(err)
	}
	return cv
}

func TestWireRecordBlocksAreOneElementLists(t *testing.T) {
	_, ws := mustSchema(t, schema.Source{Namespace: "hashicorp", Type: "tls"}, tlsSchema)
	w, err := ws.resource("tls_cert_request")
	if err != nil {
		t.Fatal(err)
	}
	subject := obj(map[string]value.Value{"common_name": str("example.com")})

	cv := wireValue(t, w.wireBlock, obj(map[string]value.Value{"subject": subject}))
	sub := cv.GetAttr("subject")
	if !sub.Type().IsListType() || sub.LengthInt() != 1 {
		t.Fatalf("subject on the wire is %#v, want a one-element list", sub)
	}
	if cn := sub.Index(cty.NumberIntVal(0)).GetAttr("common_name"); !cn.RawEquals(cty.StringVal("example.com")) {
		t.Errorf("subject[0].common_name = %#v", cn)
	}

	cv = wireValue(t, w.wireBlock, obj(nil))
	if sub := cv.GetAttr("subject"); sub.IsNull() || sub.LengthInt() != 0 {
		t.Errorf("absent subject on the wire is %#v, want an empty list", sub)
	}

	got := roundTrip(t, w.wireBlock, obj(map[string]value.Value{"subject": subject}))
	wantSubject := obj(map[string]value.Value{"common_name": str("example.com"), "street_address": value.NullVal()})
	if !value.Equal(got.Get("subject"), wantSubject) {
		t.Errorf("subject decoded as %s, want %s", got.Get("subject"), wantSubject)
	}
}

func TestWireEncodeErrors(t *testing.T) {
	w := kindsWire(t).wireBlock
	tests := []struct {
		name string
		in   value.Value
		want string
	}{
		{"wrong kind", obj(map[string]value.Value{"n": str("2")}), "n: want number, got string"},
		{"wrong element kind", obj(map[string]value.Value{"l": list(value.IntVal(1))}), "l[0]: want string, got int"},
		{"unknown attribute", obj(map[string]value.Value{"nope": str("x")}), `no attribute or block "nope"`},
		{"list where a record block goes", obj(map[string]value.Value{"one": list()}), "one: want an object, got list"},
		{"top level not an object", str("x"), "want an object, got string"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := w.encode(tt.in)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("encode error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestWirePath(t *testing.T) {
	_, ws := mustSchema(t, schema.Source{Namespace: "hashicorp", Type: "tls"}, tlsSchema)
	tlsReq, _ := ws.resource("tls_cert_request")
	kinds := kindsWire(t)
	attr := func(n string) wireStep { return wireStep{kind: stepAttr, name: n} }
	key := func(k string) wireStep { return wireStep{kind: stepKey, name: k} }
	idx := func(i int64) wireStep { return wireStep{kind: stepIndex, index: i} }
	tests := []struct {
		name  string
		block wireBlock
		steps []wireStep
		want  value.Path
	}{
		{"record block drops its index", tlsReq.wireBlock, []wireStep{attr("subject"), idx(0), attr("common_name")},
			value.Path{}.Attr("subject").Attr("common_name")},
		{"whole record block", tlsReq.wireBlock, []wireStep{attr("subject")}, value.Path{}.Attr("subject")},
		{"list in a record block", tlsReq.wireBlock, []wireStep{attr("subject"), idx(0), attr("street_address"), idx(1)},
			value.Path{}.Attr("subject").Attr("street_address").Index(1)},
		{"list block keeps its index", kinds.wireBlock, []wireStep{attr("many"), idx(2), attr("x")},
			value.Path{}.Attr("many").Index(2).Attr("x")},
		{"map block keeps its key", kinds.wireBlock, []wireStep{attr("named"), key("k"), attr("x")},
			value.Path{}.Attr("named").Key("k").Attr("x")},
		{"single block", kinds.wireBlock, []wireStep{attr("single"), attr("x")}, value.Path{}.Attr("single").Attr("x")},
		{"attribute element", kinds.wireBlock, []wireStep{attr("l"), idx(1)}, value.Path{}.Attr("l").Index(1)},
		{"map attribute key", kinds.wireBlock, []wireStep{attr("m"), key("one")}, value.Path{}.Attr("m").Key("one")},
		{"no schema", wireBlock{}, []wireStep{attr("a"), idx(0)}, value.Path{}.Attr("a").Index(0)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.block.path(tt.steps); !slices.Equal(got, tt.want) {
				t.Errorf("path = %s, want %s", got, tt.want)
			}
		})
	}
}

// An object type's fields are optional only when the type says so, since
// OpenTofu refuses an object literal that leaves out any other field.
func TestWritTypeObjectFields(t *testing.T) {
	for _, tc := range []struct {
		ty   cty.Type
		want string
	}{
		{cty.Object(map[string]cty.Type{"a": cty.String, "c": cty.Number}), "{ a: String, c: Float }"},
		{cty.ObjectWithOptionalAttrs(map[string]cty.Type{"a": cty.String, "c": cty.Number}, []string{"c"}), "{ a: String, c?: Float }"},
	} {
		got, err := writType(tc.ty)
		if err != nil {
			t.Fatal(err)
		}
		if got.String() != tc.want {
			t.Errorf("writType(%s) = %s, want %s", tc.ty.FriendlyName(), got, tc.want)
		}
	}
}

func TestBuildSchemaTypes(t *testing.T) {
	p, _ := mustSchema(t, schema.Source{Namespace: "example", Type: "kinds"}, kindsSchema)
	r, ok := p.Resource("Thing")
	if !ok {
		t.Fatal("no kinds.Thing")
	}
	if r.Version != 2 {
		t.Errorf("schema version = %d, want 2", r.Version)
	}
	want := map[string]string{
		"s":       "String",
		"n":       "Float",
		"b":       "Bool",
		"l":       "[String]",
		"st":      "[String]",
		"m":       "{String: Float}",
		"o":       "{ a: String, c: Float }",
		"secret":  "Secret<String>",
		"secrets": "Secret<[String]>",
		"id":      "Id<kinds.Thing>",
	}
	for name, typ := range want {
		a, ok := r.Block.Lookup([]string{name})
		if !ok {
			t.Errorf("no attribute %s", name)
			continue
		}
		if got := a.Type.String(); got != typ {
			t.Errorf("%s: type %s, want %s", name, got, typ)
		}
	}
	token, _ := p.Config.Lookup([]string{"token"})
	if !token.Type.IsSecret() {
		t.Errorf("sensitive provider config attribute token has type %s, want a secret", token.Type)
	}
	region, _ := p.Config.Lookup([]string{"region"})
	if region.Type.IsSecret() {
		t.Errorf("region has type %s, want a plain String", region.Type)
	}
}

func TestBuildSchemaNestedAttributes(t *testing.T) {
	const raw = `{
  "format": 1, "protocol": 6, "provider": {},
  "resources": {"six_thing": {"block": {"attributes": [
    {"name": "rules", "optional": true, "nested": {"nesting": "list", "attributes": [
      {"name": "port", "type": "number", "required": true},
      {"name": "note", "type": "string", "optional": true},
      {"name": "token", "type": "string", "optional": true, "sensitive": true}
    ]}}
  ]}}}
}`
	p, w := mustSchema(t, schema.Source{Namespace: "example", Type: "six"}, raw)
	r, _ := p.Resource("Thing")
	rules, _ := r.Block.Lookup([]string{"rules"})
	if got, want := rules.Type.String(), "[{ note?: String, port: Float, token?: Secret<String> }]"; got != want {
		t.Errorf("rules type = %s, want %s", got, want)
	}
	thing, _ := w.resource("six_thing")
	in := obj(map[string]value.Value{"rules": list(obj(map[string]value.Value{"port": value.IntVal(22), "token": str("t")}))})
	got := roundTrip(t, thing.wireBlock, in).Get("rules").Index(0)
	want := obj(map[string]value.Value{"port": value.IntVal(22), "note": value.NullVal(), "token": str("t").Secret()})
	if !value.Equal(got, want) {
		t.Errorf("nested attribute round trip = %s, want %s", got, want)
	}
}

func TestBuildSchemaRejectsDynamicTypes(t *testing.T) {
	const raw = `{"format": 1, "provider": {}, "resources": {"dyn_thing": {"block": {"attributes": [
    {"name": "anything", "type": "dynamic", "optional": true}
  ]}}}}`
	var cs cachedSchema
	if err := json.Unmarshal([]byte(raw), &cs); err != nil {
		t.Fatal(err)
	}
	_, _, err := buildSchema(schema.Source{Namespace: "example", Type: "dyn"}, "1.0.0", &cs)
	if err == nil || !strings.Contains(err.Error(), "dyn_thing: attribute anything") {
		t.Errorf("buildSchema error = %v, want one naming dyn_thing.anything", err)
	}
}

func TestDecodeMarksFollowWritTypes(t *testing.T) {
	// A secret taint deep inside a type marks only that part of the value.
	rec := types.RecordOf(
		types.Field{Name: "open", Type: types.TString},
		types.Field{Name: "hidden", Type: types.Secret(types.TString)},
	)
	cv := cty.ObjectVal(map[string]cty.Value{"open": cty.StringVal("a"), "hidden": cty.StringVal("b")})
	got := decodeValue(cv, rec, false)
	if got.IsSecret() || got.Get("open").IsSecret() || !got.Get("hidden").IsSecret() {
		t.Errorf("decoded %s: want only hidden marked secret", got)
	}
}

func TestFillReadDefaults(t *testing.T) {
	_, ws := mustSchema(t, schema.Source{Namespace: "test", Type: "kinds"}, kindsSchema)
	w, err := ws.resource("kinds_thing")
	if err != nil {
		t.Fatal(err)
	}
	w.readDefaults = map[string]string{"b": "false", "s": `"10m"`}
	for _, tc := range []struct {
		name     string
		in, want value.Value
	}{
		{"missing", value.ObjectVal(map[string]value.Value{"b": value.NullVal(), "s": value.NullVal(), "secret": value.StringVal("held").Secret()}), value.ObjectVal(map[string]value.Value{"b": value.BoolVal(false), "s": value.StringVal("10m"), "secret": value.StringVal("held").Secret()})},
		{"explicit", value.ObjectVal(map[string]value.Value{"b": value.BoolVal(true), "s": value.StringVal("0s")}), value.ObjectVal(map[string]value.Value{"b": value.BoolVal(true), "s": value.StringVal("0s")})},
		{"gone", value.NullVal(), value.NullVal()},
		{"unknown", value.ObjectVal(map[string]value.Value{"b": value.UnknownVal(false), "s": value.UnknownVal(false)}), value.ObjectVal(map[string]value.Value{"b": value.UnknownVal(false), "s": value.UnknownVal(false)})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := w.fillReadDefaults(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if !value.Equal(got, tc.want) || got.HasSecret() != tc.want.HasSecret() {
				t.Fatalf("read defaults changed state or marks: %v", err)
			}
		})
	}
}

func TestAWSReadDefaultsVersion(t *testing.T) {
	raw, err := os.ReadFile("testdata/aws-6.70.0-schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var cs cachedSchema
	if err := json.Unmarshal(raw, &cs); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"6.70.0", "6.71.0"} {
		t.Run(version, func(t *testing.T) {
			_, ws, err := buildSchema(schema.Source{Namespace: "hashicorp", Type: "aws"}, version, &cs)
			if err != nil {
				t.Fatal(err)
			}
			for wire, count := range map[string]int{"aws_autoscaling_group": 1, "aws_launch_template": 0} {
				w, err := ws.resource(wire)
				if err != nil {
					t.Fatal(err)
				}
				if version != "6.70.0" {
					count = 0
				}
				if len(w.readDefaults) != count {
					t.Fatalf("%s has %d defaults, want %d", wire, len(w.readDefaults), count)
				}
				for name := range w.readDefaults {
					if !w.ty.HasAttribute(name) {
						t.Fatalf("default names absent attribute %s", name)
					}
				}
				if _, err := w.fillReadDefaults(value.ObjectVal(nil)); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
