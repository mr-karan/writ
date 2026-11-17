package importer

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mr-karan/writ/internal/provider"
	"github.com/mr-karan/writ/internal/schema"
	"github.com/mr-karan/writ/internal/types"
	"github.com/mr-karan/writ/internal/value"
)

func TestDeclarePlanOmissionMustValidate(t *testing.T) {
	for _, mode := range []string{"accept", "reject", "transport"} {
		t.Run(mode, func(t *testing.T) {
			state := value.ObjectVal(map[string]value.Value{"blk": value.ListVal([]value.Value{value.ObjectVal(map[string]value.Value{"x": value.StringVal("explicit"), "y": value.StringVal("dependent")})})})
			obj := testObject{Schema: &schema.Resource{Name: types.Name{Provider: "test", Type: "Object"}, Wire: "test_object", Block: schema.Block{Blocks: []schema.NestedBlock{{Name: "blk", Nesting: schema.NestList, Block: schema.Block{Attributes: []schema.Attribute{{Name: "x", Type: types.TString, Optional: true, Computed: true}, {Name: "y", Type: types.TString, Optional: true}}}}}}}, State: state}
			obj.Plan = func(_ context.Context, cfg value.Value) (Plan, error) {
				if isNull(cfg.Get("blk").Index(0).Get("x")) {
					return Plan{Value: state}, nil
				}
				return Plan{}, &provider.Error{Diags: []provider.Diag{{Error: true, Summary: `invalid "blk[0].x"`}}}
			}
			ask := func(_ context.Context, _ string, cfg value.Value) ([]provider.Diag, error) {
				if isNull(cfg.Get("blk").Index(0).Get("x")) {
					switch mode {
					case "reject":
						return []provider.Diag{{Error: true, Summary: `"blk[0].y" requires "blk[0].x" remote-private-text`}}, nil
					case "transport":
						return nil, errors.New("remote-private-text")
					}
				}
				return nil, nil
			}
			code, notes := declare(t, obj, ask)
			if strings.Contains(code, "x =") != (mode != "accept") {
				t.Fatalf("invalid omission decision, code=%s", code)
			}
			text := strings.Join(notes, "\n")
			if !strings.Contains(text, "blk[0].x") || mode != "accept" && !strings.Contains(text, "validat") || strings.Contains(text, "remote-private-text") {
				t.Fatalf("unsafe or missing validation note: %s", text)
			}
		})
	}
}

func TestDeclareNotesUnprovenFirstPlan(t *testing.T) {
	for _, tc := range []struct {
		name string
		plan Plan
		err  error
		why  string
	}{
		{name: "transport", err: errors.New("remote-private-text"), why: "failed"},
		{name: "unknown", plan: Plan{Value: value.UnknownVal(false)}, why: "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := testObject{Schema: &schema.Resource{Name: types.Name{Provider: "test", Type: "Object"}, Wire: "test_object", Block: schema.Block{Attributes: []schema.Attribute{{Name: "x", Type: types.TString, Optional: true}}}}, State: value.ObjectVal(map[string]value.Value{"x": value.StringVal("explicit")}), Plan: func(context.Context, value.Value) (Plan, error) { return tc.plan, tc.err }}
			code, notes := declare(t, obj, func(context.Context, string, value.Value) ([]provider.Diag, error) { return nil, nil })
			text := strings.Join(notes, "\n")
			if !strings.Contains(code, `x = "explicit"`) || !strings.Contains(text, "no-change plan") || !strings.Contains(text, tc.why) || strings.Contains(text, "remote-private-text") {
				t.Fatalf("missing safe explanation: notes=%q code=%s", text, code)
			}
		})
	}
}

func acceptPlanConfig(context.Context, string, value.Value) ([]provider.Diag, error) {
	return nil, nil
}
