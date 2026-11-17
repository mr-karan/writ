package value_test

import (
	"math"
	"slices"
	"testing"

	"github.com/mr-karan/writ/internal/value"
)

var (
	str  = value.StringVal
	num  = value.IntVal
	list = func(vs ...value.Value) value.Value { return value.ListVal(vs) }
	obj  = value.ObjectVal
	path = value.Path{}
)

func TestObjectAndListAccessors(t *testing.T) {
	o := obj(map[string]value.Value{"b": num(2), "a": num(1), "c": num(3)})
	if got := o.Keys(); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Fatalf("Keys() = %v, want sorted", got)
	}
	if o.Len() != 3 || o.Get("b").AsInt() != 2 {
		t.Fatalf("Len or Get wrong on %s", o)
	}
	if got := o.Get("missing"); got.Kind() != value.Null {
		t.Fatalf("Get of a missing key = %s, want null", got)
	}

	elems := []value.Value{str("x"), str("y")}
	l := value.ListVal(elems)
	elems[0] = str("changed")
	if l.Index(0).AsString() != "x" || l.Len() != 2 {
		t.Fatalf("ListVal must copy its input, got %s", l)
	}
}

func TestReadingFromASecretContainerIsSecret(t *testing.T) {
	l := list(str("a")).Secret()
	o := obj(map[string]value.Value{"p": str("x")}).Secret()
	tests := []struct {
		name string
		got  value.Value
	}{
		{"Index", l.Index(0)},
		{"Get", o.Get("p")},
		{"Get of a missing key", o.Get("missing")},
		{"At", o.At(path.Attr("p"))},
		{"At through a list", list(o).At(path.Index(0).Attr("p"))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !tt.got.IsSecret() {
				t.Fatalf("%s = %s, want the secret mark", tt.name, tt.got)
			}
		})
	}
}

func TestAccessorPanicsOnWrongKind(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("AsInt on a string must panic")
		}
	}()
	str("1").AsInt()
}

func TestHasSecret(t *testing.T) {
	tests := []struct {
		name string
		v    value.Value
		want bool
	}{
		{"plain", str("x"), false},
		{"marked", str("x").Secret(), true},
		{"inside a list", list(str("a"), str("b").Secret()), true},
		{"inside an object", obj(map[string]value.Value{"a": num(1), "p": str("x").Secret()}), true},
		{"marked container", list().Secret(), true},
		{"secret unknown", value.UnknownVal(true), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.v.HasSecret(); got != tt.want {
				t.Fatalf("HasSecret(%s) = %v, want %v", tt.v, got, tt.want)
			}
		})
	}
}

func TestIsWhollyKnown(t *testing.T) {
	tests := []struct {
		name string
		v    value.Value
		want bool
	}{
		{"scalar", num(1), true},
		{"null", value.NullVal(), true},
		{"nested known", obj(map[string]value.Value{"a": list(str("x"))}), true},
		{"unknown", value.UnknownVal(false), false},
		{"unknown in a list", list(num(1), value.UnknownVal(false)), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.v.IsWhollyKnown(); got != tt.want {
				t.Fatalf("IsWhollyKnown(%s) = %v, want %v", tt.v, got, tt.want)
			}
		})
	}
}

func TestAt(t *testing.T) {
	cert := obj(map[string]value.Value{
		"subject":   list(obj(map[string]value.Value{"common_name": str("example.com")})),
		"dns_names": list(str("a.example.com"), str("b.example.com")),
		"keepers":   obj(map[string]value.Value{"my key": str("v")}),
		"later":     value.UnknownVal(true),
	})
	tests := []struct {
		name string
		p    value.Path
		want value.Value
	}{
		{"attribute", path.Attr("dns_names").Index(1), str("b.example.com")},
		{"nested record in list", path.Attr("subject").Index(0).Attr("common_name"), str("example.com")},
		{"map key", path.Attr("keepers").Key("my key"), str("v")},
		{"empty path", nil, cert},
		{"missing attribute", path.Attr("nope"), value.NullVal()},
		{"index out of range", path.Attr("dns_names").Index(2), value.NullVal()},
		{"negative index", path.Attr("dns_names").Index(-1), value.NullVal()},
		{"name step on a list", path.Attr("dns_names").Attr("x"), value.NullVal()},
		{"index step on an object", path.Attr("keepers").Index(0), value.NullVal()},
		{"step through a scalar", path.Attr("dns_names").Index(0).Attr("x"), value.NullVal()},
		{"step through unknown keeps the mark", path.Attr("later").Attr("x").Index(3), value.UnknownVal(true)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cert.At(tt.p); !value.Equal(got, tt.want) {
				t.Fatalf("At(%s) = %s, want %s", tt.p, got, tt.want)
			}
		})
	}
}

func TestEqual(t *testing.T) {
	tests := []struct {
		name string
		a, b value.Value
		want bool
	}{
		{"same string", str("a"), str("a"), true},
		{"different string", str("a"), str("b"), false},
		{"int is not float", num(1), value.FloatVal(1), false},
		{"string is not int", str("1"), num(1), false},
		{"null", value.NullVal(), value.NullVal(), true},
		{"null is not an empty list", value.NullVal(), list(), false},
		{"empty list is not an empty object", list(), obj(nil), false},
		{"lists", list(num(1), num(2)), list(num(1), num(2)), true},
		{"list order matters", list(num(1), num(2)), list(num(2), num(1)), false},
		{"list length", list(num(1)), list(num(1), num(1)), false},
		{"objects", obj(map[string]value.Value{"a": num(1)}), obj(map[string]value.Value{"a": num(1)}), true},
		{"object keys", obj(map[string]value.Value{"a": num(1)}), obj(map[string]value.Value{"b": num(1)}), false},
		{"secret mark differs", str("a"), str("a").Secret(), false},
		{"both secret", str("a").Secret(), str("a").Secret(), true},
		{"container mark covers leaves", list(str("a")).Secret(), list(str("a").Secret()), true},
		{"partly secret is not wholly secret", list(str("a"), str("b")).Secret(), list(str("a").Secret(), str("b")), false},
		{"empty secret list", list().Secret(), list(), false},
		{"unknown equals unknown", value.UnknownVal(false), value.UnknownVal(false), true},
		{"unknown is not known", value.UnknownVal(false), value.NullVal(), false},
		{"unknown keeps its mark", value.UnknownVal(true), value.UnknownVal(false), false},
		{"NaN equals NaN", value.FloatVal(math.NaN()), value.FloatVal(math.NaN()), true},
		{"negative zero is not zero", value.FloatVal(math.Copysign(0, -1)), value.FloatVal(0), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := value.Equal(tt.a, tt.b); got != tt.want {
				t.Fatalf("Equal(%s, %s) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
			if got := value.Equal(tt.b, tt.a); got != tt.want {
				t.Fatalf("Equal is not symmetric for %s and %s", tt.a, tt.b)
			}
		})
	}
}

func TestWalk(t *testing.T) {
	v := obj(map[string]value.Value{
		"tags":    list(str("x"), str("y")),
		"empty":   list(),
		"subject": obj(map[string]value.Value{"cn": str("example.com"), "my key": num(1)}),
		"pem":     obj(map[string]value.Value{"body": str("secret")}).Secret(),
		"later":   value.UnknownVal(false),
	})
	type leaf struct {
		path   string
		secret bool
		kind   value.Kind
	}
	var got []leaf
	value.Walk(v, func(p value.Path, l value.Value) {
		got = append(got, leaf{p.String(), l.IsSecret(), l.Kind()})
	})
	want := []leaf{
		{"later", false, value.Unknown},
		{"pem.body", true, value.String},
		{"subject.cn", false, value.String},
		{`subject["my key"]`, false, value.Int},
		{"tags[0]", false, value.String},
		{"tags[1]", false, value.String},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Walk visited\n%v\nwant\n%v", got, want)
	}
}

func TestStringRedactsSecrets(t *testing.T) {
	tests := []struct {
		v    value.Value
		want string
	}{
		{obj(map[string]value.Value{"user": str("admin"), "password": str("hunter2").Secret()}), `{ password: (secret), user: "admin" }`},
		{list(str("a"), str("b")).Secret(), "(secret)"},
		{value.UnknownVal(true), "(secret)"},
		{value.UnknownVal(false), "(known after apply)"},
		{obj(map[string]value.Value{"my key": num(1), "f": value.FloatVal(0.5), "b": value.BoolVal(true), "n": value.NullVal()}), `{ b: true, f: 0.5, "my key": 1, n: null }`},
		{obj(nil), "{}"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := tt.v.String(); got != tt.want {
				t.Fatalf("String() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestPathString(t *testing.T) {
	tests := []struct {
		p    value.Path
		want string
	}{
		{nil, ""},
		{path.Attr("subject").Attr("common_name"), "subject.common_name"},
		{path.Attr("keepers").Key("a"), "keepers.a"},
		{path.Attr("keepers").Key("my key"), `keepers["my key"]`},
		{path.Attr("keepers").Key("${x}"), `keepers["\${x}"]`},
		{path.Attr("allowed_uses").Index(0), "allowed_uses[0]"},
		{path.Index(2).Attr("name"), "[2].name"},
		{path.Key("a.b"), `["a.b"]`},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := tt.p.String(); got != tt.want {
				t.Fatalf("String() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestPathFieldAndKeyStepsAreTheSamePlace(t *testing.T) {
	fromProvider := value.Path{value.AttrStep("keepers"), value.KeyStep("a")}
	fromWalk := path.Attr("keepers").Attr("a")
	if !fromWalk.HasPrefix(fromProvider) || !fromProvider.HasPrefix(fromWalk) {
		t.Fatalf("%s and %s must be the same path", fromProvider, fromWalk)
	}
	if fromProvider.String() != fromWalk.String() {
		t.Fatalf("%s and %s must print the same", fromProvider, fromWalk)
	}
}

func TestPathHasPrefix(t *testing.T) {
	p := path.Attr("subject").Index(0).Attr("common_name")
	tests := []struct {
		q    value.Path
		want bool
	}{
		{nil, true},
		{path.Attr("subject"), true},
		{path.Attr("subject").Index(0), true},
		{p, true},
		{p.Attr("more"), false},
		{path.Attr("subject").Index(1), false},
		{path.Attr("subj"), false},
	}
	for _, tt := range tests {
		t.Run(tt.q.String(), func(t *testing.T) {
			if got := p.HasPrefix(tt.q); got != tt.want {
				t.Fatalf("%s.HasPrefix(%s) = %v, want %v", p, tt.q, got, tt.want)
			}
		})
	}
}

func TestPathExtendNeverAliases(t *testing.T) {
	base := append(make(value.Path, 0, 8), value.AttrStep("a"))
	b := base.Attr("b")
	c := base.Attr("c")
	if b.String() != "a.b" || c.String() != "a.c" || base.String() != "a" {
		t.Fatalf("extending aliased the base: base=%s b=%s c=%s", base, b, c)
	}
}

func TestFormatFloat(t *testing.T) {
	tests := []struct {
		name string
		in   float64
		want string
	}{
		{"a port", 8000, "8000"},
		{"a million has no exponent", 1e6, "1000000"},
		{"past int64 has no exponent", 1e21, "1000000000000000000000"},
		{"negative whole", -42, "-42"},
		{"zero", 0, "0"},
		{"a fraction", 0.5, "0.5"},
		{"a fraction keeps its digits", 2.25, "2.25"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := value.FormatFloat(tt.in); got != tt.want {
				t.Errorf("FormatFloat(%v) = %s, want %s", tt.in, got, tt.want)
			}
		})
	}
}
