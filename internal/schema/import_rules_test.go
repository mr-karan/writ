package schema

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestImportRulesMatchAWSSchema(t *testing.T) {
	data, err := os.ReadFile("../provider/testdata/aws-6.70.0-schema.json")
	if err != nil {
		t.Fatal(err)
	}
	type attr struct {
		Name                         string
		Required, Optional, Computed bool
		Type                         json.RawMessage
	}
	type block struct {
		Deprecated         bool
		DeprecationMessage string `json:"deprecation_message"`
		Attributes         []attr
		Blocks             []json.RawMessage
	}
	var fs struct {
		Resources map[string]struct{ Block block }
	}
	if err := json.Unmarshal(data, &fs); err != nil {
		t.Fatal(err)
	}
	// Each block's off state, from the provider docs: mutual TLS is off at
	// mode "off", and stickiness is off at enabled = false.
	wantDisabled := map[string]DisabledWhen{
		"aws_lb_listener.mutual_authentication":             {Field: "mode", Value: "off"},
		"aws_lb_listener.default_action.forward.stickiness": {Field: "enabled", Value: "false"},
		"aws_lb_listener_rule.action.forward.stickiness":    {Field: "enabled", Value: "false"},
	}
	for wire, ann := range annotations()[Source{Namespace: "hashicorp", Type: "aws"}].Types {
		for path, rule := range ann.ImportDisabled {
			b := fs.Resources[wire].Block
			steps := strings.Split(path, ".")
			for i, name := range steps {
				found := false
				for _, raw := range b.Blocks {
					var nb struct {
						Name     string
						MinItems int `json:"min_items"`
						Block    block
					}
					if err := json.Unmarshal(raw, &nb); err != nil {
						t.Fatal(err)
					}
					if nb.Name == name {
						if i == len(steps)-1 && nb.MinItems != 0 {
							t.Errorf("%s.%s is required", wire, path)
						}
						b = nb.Block
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("missing block %s.%s", wire, path)
				}
			}
			if w, ok := wantDisabled[wire+"."+path]; !ok || rule != w {
				t.Errorf("%s.%s rule %+v, want %+v", wire, path, rule, w)
			}
			delete(wantDisabled, wire+"."+path)
			found := false
			for _, a := range b.Attributes {
				if a.Name == rule.Field {
					want := `"string"`
					if rule.Value == "false" {
						want = `"bool"`
					}
					if string(a.Type) != want {
						t.Errorf("%s.%s switch type %s, want %s", wire, path, a.Type, want)
					}
					found = true
				}
			}
			if !found {
				t.Errorf("missing switch %s.%s.%s", wire, path, rule.Field)
			}
		}
		for name, owner := range ann.ImportOwners {
			if !owner.Computed {
				t.Errorf("owner block is not computed: %s.%s", wire, name)
			}
			target, ok := fs.Resources[owner.Wire]
			if !ok {
				t.Errorf("missing owner %s", owner.Wire)
			}
			hasAttr := func(b block, name string) bool {
				for _, a := range b.Attributes {
					if a.Name == name {
						return true
					}
				}
				return false
			}
			if !hasAttr(target.Block, owner.OwnerField) || !hasAttr(fs.Resources[wire].Block, owner.ParentField) {
				t.Errorf("invalid owner fields for %s.%s", wire, name)
			}
			if owner.OwnerKey != "" && !hasAttr(target.Block, owner.OwnerKey) {
				t.Errorf("missing owner key %s", owner.OwnerKey)
			}
			found := false
			for _, raw := range fs.Resources[wire].Block.Blocks {
				var nb struct {
					Name  string
					Block block
				}
				if err := json.Unmarshal(raw, &nb); err != nil {
					t.Fatal(err)
				}
				if nb.Name == name {
					if !nb.Block.Deprecated || !strings.Contains(nb.Block.DeprecationMessage, owner.Wire+" resource") {
						t.Errorf("unverified owner %s.%s -> %s", wire, name, owner.Wire)
					}
					if owner.InlineKey != "" && !hasAttr(nb.Block, owner.InlineKey) {
						t.Errorf("missing inline key %s", owner.InlineKey)
					}
					found = true
				}
			}
			if !found {
				t.Errorf("missing deprecated inline block %s.%s", wire, name)
			}
		}
	}
	for path := range wantDisabled {
		t.Errorf("missing disabled rule %s", path)
	}
}
