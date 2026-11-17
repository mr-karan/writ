package provider

import (
	"encoding/json"
	"os"
	"slices"
	"testing"

	"github.com/mr-karan/writ/internal/schema"
	"github.com/mr-karan/writ/internal/types"
)

// TestImportSourcesAreFilled checks that every attribute writ import may
// write as a reference reads as non-null when the body leaves it unset. Such
// an attribute is a read-only typed reference to its own resource type that
// some typed slot accepts, such as an AWS VPC's id or a tls certificate's
// cert_pem. If the annotation layer did not list it as Filled, the code
// writ import generates would not check. The fixtures are schemas the
// pinned providers returned. No slot accepts a random or local reference.
func TestImportSourcesAreFilled(t *testing.T) {
	fixtures := []struct {
		src           schema.Source
		version, file string
	}{
		{awsSrc, "6.70.0", "testdata/aws-6.70.0-schema.json"},
		{tlsSrc, "4.4.2", "testdata/tls-4.4.2-schema.json"},
	}
	kinds := []types.RefKind{types.IdRef, types.PemRef, types.ArnRef, types.NameRef}
	for _, fx := range fixtures {
		t.Run(fx.src.String(), func(t *testing.T) {
			data, err := os.ReadFile(fx.file)
			if err != nil {
				t.Fatal(err)
			}
			var cs cachedSchema
			if err := json.Unmarshal(data, &cs); err != nil {
				t.Fatal(err)
			}
			p, _, err := buildSchema(fx.src, fx.version, &cs)
			if err != nil {
				t.Fatal(err)
			}
			var resources []*schema.Resource
			var slots []types.Type
			for wire := range cs.Resources {
				r, err := p.ResourceByWire(wire)
				if err != nil {
					t.Fatal(err)
				}
				resources = append(resources, r)
				slots = append(slots, configSlots(r.Block)...)
			}
			sources := 0
			for _, r := range resources {
				for _, a := range r.Block.Attributes {
					if !readOnly(a) || a.Type.Kind() != types.Ref || !slices.ContainsFunc(kinds, func(k types.RefKind) bool {
						_, ok := types.Assign(a.Type, types.RefTo(k, r.Name))
						return ok
					}) || !slices.ContainsFunc(slots, func(slot types.Type) bool {
						_, ok := types.Assign(slot, a.Type)
						return ok
					}) {
						continue
					}
					sources++
					if !a.Filled {
						t.Errorf("%s.%s is a reference writ import may write, but it is not Filled", r.Wire, a.Name)
					}
				}
			}
			if sources == 0 {
				t.Fatal("writ import can write no reference here, so the test checks nothing")
			}
		})
	}
}

// readOnly reports whether a resource body may not set a.
func readOnly(a schema.Attribute) bool { return a.Computed && !a.Optional && !a.Required }

// configSlots returns the typed reference slots a resource body may set in
// b: those of its settable attributes and of its nested blocks, at any depth.
func configSlots(b schema.Block) []types.Type {
	var out []types.Type
	for _, a := range b.Attributes {
		if !readOnly(a) {
			out = append(out, refSlots(a.Type)...)
		}
	}
	for _, nb := range b.Blocks {
		out = append(out, configSlots(nb.Block)...)
	}
	return out
}

// refSlots returns the typed reference slots in t: t itself when it is a
// reference, and the elements of its lists, maps, and records, at any depth.
func refSlots(t types.Type) []types.Type {
	switch t.Kind() {
	case types.Ref:
		return []types.Type{t}
	case types.List, types.Map:
		return refSlots(t.Elem())
	case types.Record:
		var out []types.Type
		for _, f := range t.Fields() {
			out = append(out, refSlots(f.Type)...)
		}
		return out
	}
	return nil
}
