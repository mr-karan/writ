package value

import (
	"testing"

	"github.com/mr-karan/writ/internal/schema"
	"github.com/mr-karan/writ/internal/types"
)

func TestPathPrinter(t *testing.T) {
	const key = "synthetic-secret-key"
	p := Path{}.Attr("credentials").Key(key).Index(3).Attr("token")
	obj := func(v Value) Value { return ObjectVal(map[string]Value{"credentials": v}) }
	for _, tt := range []struct {
		name     string
		printer  PathPrinter
		want     string
		collapse string
	}{
		{"public", PathPrinter{}, `credentials["synthetic-secret-key"][3].token`, `credentials["synthetic-secret-key"][3].token`},
		{"sensitive schema", PathPrinter{Schema: schema.Block{Attributes: []schema.Attribute{{Name: "credentials", Type: types.MapOf(types.TString), Sensitive: true}}}}, "credentials[(secret)][(secret)][(secret)]", "credentials"},
		{"secret type", PathPrinter{Schema: schema.Block{Attributes: []schema.Attribute{{Name: "credentials", Type: types.Secret(types.MapOf(types.TString))}}}}, "credentials[(secret)][(secret)][(secret)]", "credentials"},
		{"type holding a secret", PathPrinter{Schema: schema.Block{Attributes: []schema.Attribute{{Name: "credentials", Type: types.MapOf(types.Secret(types.TString))}}}}, "credentials[(secret)][(secret)][(secret)]", "credentials"},
		{"marked prior", PathPrinter{Values: []Value{obj(ObjectVal(nil).Secret()), obj(ObjectVal(nil))}}, "credentials[(secret)][(secret)][(secret)]", "credentials"},
		{"marked config", PathPrinter{Values: []Value{obj(ObjectVal(nil)), obj(ObjectVal(nil).Secret())}}, "credentials[(secret)][(secret)][(secret)]", "credentials"},
		{"map holding marked leaves", PathPrinter{Values: []Value{obj(ObjectVal(map[string]Value{key: StringVal("token").Secret()}))}}, "credentials[(secret)][(secret)][(secret)]", "credentials"},
		{"list holding marked leaves", PathPrinter{Values: []Value{obj(ListVal([]Value{StringVal("token").Secret()}))}}, "credentials[(secret)][(secret)][(secret)]", "credentials"},
		{"root mark", PathPrinter{Values: []Value{ObjectVal(nil).Secret()}}, "[(secret)][(secret)][(secret)][(secret)]", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.printer.String(p); got != tt.want {
				t.Errorf("path = %q, want %q", got, tt.want)
			}
			if got := tt.printer.Collapse(p).String(); got != tt.collapse {
				t.Errorf("collapsed = %q, want %q", got, tt.collapse)
			}
			if p.String() != `credentials["synthetic-secret-key"][3].token` {
				t.Error("printer changed the raw path")
			}
		})
	}
}

func TestPathPrinterNestedBlocks(t *testing.T) {
	b := schema.Block{Blocks: []schema.NestedBlock{{Name: "items", Nesting: schema.NestList, Block: schema.Block{Attributes: []schema.Attribute{{Name: "credentials", Sensitive: true}}}}}}
	p := Path{}.Attr("items").Index(0).Attr("credentials").Key("synthetic-secret-key")
	if got := (PathPrinter{Schema: b}).String(p); got != "items[0].credentials[(secret)]" {
		t.Errorf("path = %q", got)
	}
}
