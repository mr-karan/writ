package provider

import (
	"slices"
	"testing"

	"github.com/mr-karan/writ/internal/schema"
	"github.com/mr-karan/writ/internal/types"
	"github.com/mr-karan/writ/internal/value"
)

func TestProposeNew(t *testing.T) {
	inner := schema.Block{Attributes: []schema.Attribute{proposeAttr("name", "optional"), proposeAttr("uid", "computed")}}
	block := schema.Block{
		Attributes: []schema.Attribute{
			proposeAttr("id", "computed"),
			proposeAttr("algorithm", "required"),
			proposeAttr("curve", "optional", "computed"),
			proposeAttr("comment", "optional"),
			proposeAttrOf("tags", types.ListOf(types.TString), "optional"),
		},
		Blocks: []schema.NestedBlock{
			{Name: "subject", Nesting: schema.NestSingle, Block: inner},
			{Name: "rule", Nesting: schema.NestList, Block: inner},
			{Name: "member", Nesting: schema.NestSet, Block: inner},
			{Name: "named", Nesting: schema.NestMap, Block: inner},
		},
	}
	list := func(vs ...value.Value) value.Value { return value.ListVal(vs) }
	el := func(name, uid any) value.Value { return proposeObj("name", name, "uid", uid) }
	config := proposeObj(
		"id", nil, "algorithm", "ECDSA", "curve", nil, "comment", nil, "tags", list(value.StringVal("t")),
		"subject", el("s", nil),
		"rule", list(el("r0", nil), el("r1", nil)),
		"member", list(el("m", nil)),
		"named", proposeObj("k", el("n", nil)),
	)
	prior := proposeObj(
		"id", "id-1", "algorithm", "RSA", "curve", "P224", "comment", "old", "tags", nil,
		"subject", el("old", "su"),
		"rule", list(el("old", "u0")),
		"member", list(el("m", "mu")),
		"named", proposeObj("k", el("old", "nu"), "gone", el("x", "gu")),
	)
	tests := []struct {
		name       string
		prior, cfg value.Value
		want       value.Value
	}{
		{
			name:  "a create takes config and leaves computed attributes null",
			prior: value.NullVal(), cfg: config,
			want: proposeObj(
				"id", nil, "algorithm", "ECDSA", "curve", nil, "comment", nil, "tags", list(value.StringVal("t")),
				"subject", el("s", nil),
				"rule", list(el("r0", nil), el("r1", nil)),
				"member", list(el("m", nil)),
				"named", proposeObj("k", el("n", nil)),
			),
		},
		{
			name:  "an update keeps computed values, drops optional ones config removed, and matches blocks by index or key",
			prior: prior, cfg: config,
			want: proposeObj(
				"id", "id-1", "algorithm", "ECDSA", "curve", "P224", "comment", nil, "tags", list(value.StringVal("t")),
				"subject", el("s", "su"),
				"rule", list(el("r0", "u0"), el("r1", nil)),
				"member", list(el("m", nil)),
				"named", proposeObj("k", el("n", "nu")),
			),
		},
		{
			name:  "a block config leaves unset is null",
			prior: prior,
			cfg:   proposeObj("id", nil, "algorithm", "RSA", "curve", "P256", "comment", nil, "tags", nil, "subject", nil, "rule", nil, "member", nil, "named", nil),
			want:  proposeObj("id", "id-1", "algorithm", "RSA", "curve", "P256", "comment", nil, "tags", nil, "subject", nil, "rule", nil, "member", nil, "named", nil),
		},
		{
			name:  "unknown config values pass through",
			prior: prior,
			cfg:   proposeObj("id", nil, "algorithm", value.UnknownVal(false), "curve", nil, "comment", nil, "tags", nil, "subject", nil, "rule", value.UnknownVal(false), "member", nil, "named", nil),
			want:  proposeObj("id", "id-1", "algorithm", value.UnknownVal(false), "curve", "P224", "comment", nil, "tags", nil, "subject", nil, "rule", value.UnknownVal(false), "member", nil, "named", nil),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ProposeNew(block, tt.prior, tt.cfg); !value.Equal(got, tt.want) {
				t.Errorf("ProposeNew =\n %s\nwant\n %s", got, tt.want)
			}
		})
	}
}

func proposeAttr(name string, flags ...string) schema.Attribute {
	return proposeAttrOf(name, types.TString, flags...)
}

func proposeAttrOf(name string, t types.Type, flags ...string) schema.Attribute {
	return schema.Attribute{Name: name, Type: t, Required: slices.Contains(flags, "required"), Optional: slices.Contains(flags, "optional"), Computed: slices.Contains(flags, "computed")}
}

// proposeObj is an object of name and value pairs; a string, an int, or
// nil stands for its value.
func proposeObj(kv ...any) value.Value {
	m := map[string]value.Value{}
	for i := 0; i < len(kv); i += 2 {
		switch x := kv[i+1].(type) {
		case value.Value:
			m[kv[i].(string)] = x
		case string:
			m[kv[i].(string)] = value.StringVal(x)
		case int:
			m[kv[i].(string)] = value.IntVal(int64(x))
		case nil:
			m[kv[i].(string)] = value.NullVal()
		}
	}
	return value.ObjectVal(m)
}
