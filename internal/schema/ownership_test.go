package schema

import (
	"fmt"
	"slices"
	"testing"

	"github.com/mr-karan/writ/internal/types"
)

func TestOwnershipAnnotations(t *testing.T) {
	aws := Source{Namespace: "hashicorp", Type: "aws"}
	desired := Attribute{Name: "desired_capacity", Type: types.TFloat, Optional: true, Computed: true}
	tests := []struct {
		name   string
		wire   string
		attrs  []Attribute
		scaled []string
	}{
		{"autoscaling group capacity is scaled", "aws_autoscaling_group", []Attribute{desired}, []string{"desired_capacity"}},
		{"a computed-only attribute is never scaled", "aws_autoscaling_group",
			[]Attribute{{Name: "desired_capacity", Type: types.TFloat, Computed: true}}, nil},
		{"a version without the attribute has none", "aws_autoscaling_group", nil, nil},
		{"other types own nothing", "aws_launch_template", []Attribute{desired}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := NewProvider(aws, "1.0.0", Block{}, []RawResource{{Wire: tt.wire, Block: Block{Attributes: tt.attrs}}})
			if err != nil {
				t.Fatal(err)
			}
			r, err := p.ResourceByWire(tt.wire)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(r.Scaled, tt.scaled) {
				t.Errorf("Scaled = %v, want %v", r.Scaled, tt.scaled)
			}
		})
	}
}

func TestImportOwnerRequiresComputedAndDeprecated(t *testing.T) {
	for _, computed := range []bool{false, true} {
		for _, deprecated := range []bool{false, true} {
			t.Run(fmt.Sprintf("computed=%t/deprecated=%t", computed, deprecated), func(t *testing.T) {
				a := annotator{ann: annotation{ImportOwners: map[string]InlineOwner{"inline": {Wire: "test_owner", Computed: computed}}}}
				b, err := a.block(Block{Blocks: []NestedBlock{{Name: "inline", Block: Block{Deprecated: deprecated}}}}, "")
				if err != nil {
					t.Fatal(err)
				}
				if (b.Blocks[0].ImportOwner != nil) != (computed && deprecated) {
					t.Fatal("owner annotation accepted without computed and deprecated metadata")
				}
			})
		}
	}
}
