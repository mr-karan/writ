package schema

import (
	"testing"

	"github.com/mr-karan/writ/internal/types"
)

func TestSettingsVersion(t *testing.T) {
	for _, version := range []string{"6.70.0", "6.69.0", "6.71.0"} {
		t.Run(version, func(t *testing.T) {
			p, err := NewProvider(Source{Namespace: "hashicorp", Type: "aws"}, version, Block{}, []RawResource{{Wire: "aws_launch_template", Block: Block{Attributes: []Attribute{
				{Name: "name", Type: types.TString, Optional: true},
				{Name: "update_default_version", Type: types.TBool, Optional: true},
				{Name: "default_version", Type: types.TInt, Optional: true, Computed: true},
				{Name: "latest_version", Type: types.TInt, Computed: true},
			}}}})
			if err != nil {
				t.Fatal(err)
			}
			r, err := p.ResourceByWire("aws_launch_template")
			if err != nil {
				t.Fatal(err)
			}
			if (r.Settings != nil) != (version == "6.70.0") {
				t.Fatalf("settings rule for %s = %v", version, r.Settings)
			}
		})
	}
}
