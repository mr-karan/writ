package importer_test

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/mr-karan/writ/internal/importer"
	"github.com/mr-karan/writ/internal/types"
	"github.com/mr-karan/writ/internal/value"
)

// wireType reads a type by its provider's name, as an OpenTofu stack's
// Type does, without loading a provider schema.
func wireType(wire string) (types.Name, error) {
	local, rest, ok := strings.Cut(wire, "_")
	if !ok {
		return types.Name{}, fmt.Errorf("type %q: want a resource type, such as random_uuid", wire)
	}
	return types.Name{Provider: local, Type: rest}, nil
}

func TestParseSelections(t *testing.T) {
	ok := `{"objects": [
		{"type": "random_uuid", "ids": ["a"]},
		{"type": "random_id", "values": [{"b": "AQ", "n": 2, "on": true}], "id": "{b},{n},{on}"},
		{"type": "aws_route", "from": "random_uuid", "over": "route", "id": "{parent.id}_{item.cidr_block}"},
		{"type": "aws_instance", "provider": "regional[\"eu-west-1\"]", "list": {"filter": [{"name": "tag:team"}]}}
	]}`
	sels, err := importer.ParseSelectionsOf(strings.NewReader(ok), wireType)
	if err != nil {
		t.Fatal(err)
	}
	if len(sels) != 4 || sels[3].Provider.String() != `regional["eu-west-1"]` || sels[2].Over.String() != "route" {
		t.Fatalf("selections = %+v", sels)
	}
	for _, tc := range []struct {
		name, in, want string
	}{
		{"not json", `{`, "not valid JSON"},
		{"unknown field", `{"objects": [{"type": "random_uuid", "ids": ["a"], "idz": 1}]}`, "unknown field"},
		{"two values", `{"objects": [{"type": "random_uuid", "ids": ["a"]}]} {}`, "more than one JSON value"},
		{"no objects", `{"objects": []}`, "no objects"},
		{"bad type", `{"objects": [{"type": "uuid", "ids": ["a"]}]}`, "want a resource type"},
		{"two ways", `{"objects": [{"type": "random_uuid", "ids": ["a"], "list": {}}]}`, "exactly one of"},
		{"no way", `{"objects": [{"type": "random_uuid"}]}`, "exactly one of"},
		{"id without values", `{"objects": [{"type": "random_uuid", "ids": ["a"], "id": "{x}"}]}`, "id goes with values or from"},
		{"values without id", `{"objects": [{"type": "random_uuid", "values": [{"x": "a"}]}]}`, "id goes with values or from"},
		{"over without from", `{"objects": [{"type": "random_uuid", "values": [{"x": "a"}], "id": "{x}", "over": "r"}]}`, "over goes with from"},
		{"from a later type", `{"objects": [{"type": "random_id", "from": "random_uuid", "id": "{parent.id}"}, {"type": "random_uuid", "ids": ["a"]}]}`, "no earlier entry"},
		{"empty id", `{"objects": [{"type": "random_uuid", "ids": [""]}]}`, "empty"},
		{"long id", `{"objects": [{"type": "random_uuid", "ids": ["` + strings.Repeat("x", importer.MaxIDBytes+1) + `"]}]}`, "more than the limit"},
		{"path in values", `{"objects": [{"type": "random_uuid", "values": [{"x": "a"}], "id": "{x.y}"}]}`, "fields, not paths"},
		{"wrong root", `{"objects": [{"type": "random_uuid", "ids": ["a"]}, {"type": "random_id", "from": "random_uuid", "id": "{item.id}"}]}`, "want {parent.ATTR}"},
		{"nested value", `{"objects": [{"type": "random_uuid", "values": [{"x": ["a"]}], "id": "{x}"}]}`, "want a string"},
		{"unclosed", `{"objects": [{"type": "random_uuid", "values": [{"x": "a"}], "id": "{x"}]}`, "with no }"},
		{"stray brace", `{"objects": [{"type": "random_uuid", "values": [{"x": "a"}], "id": "x}"}]}`, "with no {"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := importer.ParseSelectionsOf(strings.NewReader(tc.in), wireType)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one that says %q", err, tc.want)
			}
		})
	}
	t.Run("too large", func(t *testing.T) {
		big := `{"objects": [{"type": "random_uuid", "ids": ["` + strings.Repeat("x", importer.MaxSelectionBytes) + `"]}]}`
		_, err := importer.ParseSelectionsOf(strings.NewReader(big), wireType)
		if err == nil || !strings.Contains(err.Error(), "larger than") {
			t.Fatalf("err = %v, want a size refusal", err)
		}
	})
}

func TestTemplate(t *testing.T) {
	vars := value.ObjectVal(map[string]value.Value{
		"parent": value.ObjectVal(map[string]value.Value{"id": value.StringVal("rtb-1"), "n": value.IntVal(3), "f": value.FloatVal(1.5), "none": value.NullVal()}),
		"item":   value.ObjectVal(map[string]value.Value{"cidr": value.StringVal("10.0.0.0/16"), "key": value.StringVal("hunter2").Secret()}),
	})
	for _, tc := range []struct {
		tmpl, want string
		skip       bool
		secret     bool
	}{
		{tmpl: "{parent.id}_{item.cidr}", want: "rtb-1_10.0.0.0/16"},
		{tmpl: "{{literal}} {parent.n}/{parent.f}", want: "{literal} 3/1.5"},
		{tmpl: "{parent.none}", skip: true},
		{tmpl: "{parent.missing.deeper}", skip: true},
		{tmpl: "{item.key}", secret: true},
	} {
		t.Run(tc.tmpl, func(t *testing.T) {
			tmpl, err := importer.ParseTemplate(tc.tmpl)
			if err != nil {
				t.Fatal(err)
			}
			got, err := tmpl.Expand(vars)
			switch {
			case tc.skip:
				if err == nil || !strings.Contains(err.Error(), "has no value") {
					t.Fatalf("got %q, %v; want a skip", got, err)
				}
			case tc.secret:
				if err == nil || strings.Contains(err.Error(), "hunter2") || !strings.Contains(err.Error(), "secret") {
					t.Fatalf("got %q, %v; want a secret refusal that hides the value", got, err)
				}
			case err != nil || got != tc.want:
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	if _, err := importer.ParseTemplate(""); err == nil || errors.Is(err, nil) {
		t.Fatal("an empty template parses")
	}
}

// TestTemplateBounds requires that a template longer than MaxTemplateBytes
// is refused, and that an expansion stops at MaxIDBytes without building a
// longer string.
func TestTemplateBounds(t *testing.T) {
	if _, err := importer.ParseTemplate(strings.Repeat("a", importer.MaxTemplateBytes+1)); err == nil {
		t.Fatal("a template longer than the limit parses")
	}
	tmpl, err := importer.ParseTemplate(strings.Repeat("{x}", importer.MaxTemplateBytes/3))
	if err != nil {
		t.Fatal(err)
	}
	long := value.ObjectVal(map[string]value.Value{"x": value.StringVal(strings.Repeat("x", 4096))})
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	id, err := tmpl.Expand(long)
	runtime.ReadMemStats(&after)
	if err == nil || id != "" || !strings.Contains(err.Error(), "longer than the limit of 2048 bytes") {
		t.Fatalf("got a %d-byte id, %v; want the id limit", len(id), err)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 1<<20 {
		t.Fatalf("the expansion allocated %d bytes, want at most 1 MiB", grew)
	}
	one, err := importer.ParseTemplate("{x}")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{importer.MaxIDBytes, importer.MaxIDBytes + 1} {
		id, err := one.Expand(value.ObjectVal(map[string]value.Value{"x": value.StringVal(strings.Repeat("y", n))}))
		if ok := err == nil && len(id) == n; ok != (n <= importer.MaxIDBytes) {
			t.Errorf("a %d-byte value expands to %d bytes, %v", n, len(id), err)
		}
	}
}
