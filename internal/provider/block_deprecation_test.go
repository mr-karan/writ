package provider

import (
	"testing"

	"github.com/mr-karan/writ/internal/tfplugin5"
	"github.com/mr-karan/writ/internal/tfplugin6"
)

func TestBlockDeprecationSurvivesProtocols(t *testing.T) {
	for _, tc := range []struct {
		name  string
		block rawBlock
	}{
		{"v5", v5Block(&tfplugin5.Schema_Block{Deprecated: true, DeprecationMessage: "use replacement"})},
		{"v6", v6Block(&tfplugin6.Schema_Block{Deprecated: true, DeprecationMessage: "use replacement"})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, _, err := convertBlock(tc.block)
			if err != nil {
				t.Fatal(err)
			}
			if !b.Deprecated || b.DeprecationMessage != "use replacement" {
				t.Fatalf("lost deprecation: %+v", b)
			}
			if !taintSensitive(b).Deprecated {
				t.Fatal("taint lost deprecation")
			}
		})
	}
}
