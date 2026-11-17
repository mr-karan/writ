package importer

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/mr-karan/writ/internal/provider"
	"github.com/mr-karan/writ/internal/schema"
	"github.com/mr-karan/writ/internal/types"
	"github.com/mr-karan/writ/internal/value"
)

func TestImportRejectsNestedOptionalZero(t *testing.T) {
	p := awsTypes(t)
	n := awsNode(t, p, "aws_default_network_acl", `{"default_network_acl_id":"acl-example","ingress":[{"action":"allow","protocol":"tcp","rule_no":100,"from_port":80,"to_port":80,"icmp_code":0}]}`)
	configure(n)
	ask := func(_ context.Context, _ string, c value.Value) ([]provider.Diag, error) {
		if !isNull(c.Get("ingress").Index(0).Get("icmp_code")) {
			return []provider.Diag{{Error: true, Path: value.Path{}.Attr("ingress").Index(0).Attr("icmp_code"), Summary: "invalid optional zero"}}, nil
		}
		return nil, nil
	}
	notes, err := validate(context.Background(), ask, n, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 || !isNull(n.config.Get("ingress").Index(0).Get("icmp_code")) {
		t.Fatalf("notes=%v config=%v", notes, n.config)
	}
	if v := n.config.Get("ingress").Index(0).Get("from_port"); v.Kind() != value.Int || v.AsInt() != 80 {
		t.Fatal("lost required value")
	}
}

func TestInlineOwnerMustMatchObjectAndProvider(t *testing.T) {
	for _, tc := range []struct {
		name, bucket, via, wire string
		want                    int
	}{
		{"same bucket", "example", "", "aws_s3_bucket_lifecycle_configuration", 0}, {"other bucket", "other", "", "aws_s3_bucket_lifecycle_configuration", 1}, {"other provider", "example", "west", "aws_s3_bucket_lifecycle_configuration", 1}, {"other type", "example", "", "aws_s3_bucket_policy", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := &node{schema: &schema.Resource{Wire: "aws_s3_bucket", Block: schema.Block{Attributes: []schema.Attribute{{Name: "bucket", Type: types.TString, Optional: true, Computed: true}}, Blocks: []schema.NestedBlock{{Name: "lifecycle_rule", Nesting: schema.NestList, ImportOwner: &schema.InlineOwner{Wire: "aws_s3_bucket_lifecycle_configuration", ParentField: "bucket", OwnerField: "bucket"}}}}}, state: value.ObjectVal(map[string]value.Value{"bucket": value.StringVal("example"), "lifecycle_rule": value.ListVal([]value.Value{value.ObjectVal(nil)})})}
			peer := &node{schema: &schema.Resource{Wire: tc.wire}, provider: tc.via, state: value.ObjectVal(map[string]value.Value{"bucket": value.StringVal(tc.bucket)})}
			notes, _ := configure(n, peer)
			if len(notes) != tc.want {
				t.Fatalf("notes=%v, want %d", notes, tc.want)
			}
			if n.config.Get("lifecycle_rule").Len() != 0 {
				t.Fatal("inline ownership survived")
			}
		})
	}
}

func TestImportValidationIsBounded(t *testing.T) {
	var attrs []schema.Attribute
	state := map[string]value.Value{}
	for i := range 130 {
		name := fmt.Sprintf("optional_%d", i)
		attrs = append(attrs, schema.Attribute{Name: name, Type: types.TInt, Optional: true})
		state[name] = value.IntVal(0)
	}
	n := &node{schema: &schema.Resource{Wire: "test_object", Block: schema.Block{Attributes: attrs}}, state: value.ObjectVal(state)}
	configure(n)
	calls := 0
	ask := func(_ context.Context, _ string, c value.Value) ([]provider.Diag, error) {
		calls++
		for _, a := range attrs {
			if !isNull(c.Get(a.Name)) {
				return []provider.Diag{{Error: true, Path: value.Path{}.Attr(a.Name), Summary: "invalid zero"}}, nil
			}
		}
		return nil, nil
	}
	notes, err := validate(context.Background(), ask, n, nil)
	if err != nil || calls != 128 || len(notes) == 0 || !strings.Contains(notes[len(notes)-1].Reason, "128 provider calls") {
		t.Fatalf("calls=%d err=%v, want bounded refusal", calls, err)
	}
}

func TestInlinePolicyOwnerMustMatchPolicyName(t *testing.T) {
	for _, tc := range []struct {
		name string
		want int
	}{{"read", 0}, {"other", 1}} {
		t.Run(tc.name, func(t *testing.T) {
			n := &node{schema: &schema.Resource{Block: schema.Block{Blocks: []schema.NestedBlock{{Name: "inline_policy", Nesting: schema.NestSet, ImportOwner: &schema.InlineOwner{Wire: "aws_iam_role_policy", ParentField: "name", OwnerField: "role", InlineKey: "name", OwnerKey: "name"}}}}}, state: value.ObjectVal(map[string]value.Value{"name": value.StringVal("app"), "inline_policy": value.ListVal([]value.Value{value.ObjectVal(map[string]value.Value{"name": value.StringVal("read")})})})}
			peer := &node{schema: &schema.Resource{Wire: "aws_iam_role_policy"}, state: value.ObjectVal(map[string]value.Value{"role": value.StringVal("app"), "name": value.StringVal(tc.name)})}
			notes, _ := configure(n, peer)
			if len(notes) != tc.want {
				t.Fatalf("notes=%v, want %d", notes, tc.want)
			}
		})
	}
}

func TestRequiredDisabledBlockStays(t *testing.T) {
	nb := schema.NestedBlock{MinItems: 1, ImportDisabled: &schema.DisabledWhen{Field: "enabled", Value: "false"}}
	if disabledBlock(nb, value.ObjectVal(map[string]value.Value{"enabled": value.BoolVal(false)})) {
		t.Fatal("required block removed")
	}
}

func TestPlanPruningRequiresPathsAndNoChange(t *testing.T) {
	for _, tc := range []struct {
		name     string
		computed bool
		path     bool
		noop     bool
	}{
		{"noncomputed a", false, true, true}, {"policy changes to null", true, true, false}, {"text alone", true, false, true}, {"computed no change", true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := &node{tf: "test.example", schema: &schema.Resource{Wire: "test_object", Block: schema.Block{Attributes: []schema.Attribute{{Name: "policy", Type: types.TInt, Optional: true, Computed: tc.computed}}}}}
			n.state = value.ObjectVal(map[string]value.Value{"policy": value.IntVal(1)})
			n.config = n.state
			n.plan = func(_ context.Context, c value.Value) (Plan, error) {
				if isNull(c.Get("policy")) {
					if tc.noop {
						return Plan{Value: n.state}, nil
					}
					return Plan{Value: c}, nil
				}
				var path value.Path
				if tc.path {
					path = path.Attr("policy")
				}
				return Plan{}, &provider.Error{Diags: []provider.Diag{{Error: true, Path: path, Summary: "policy: invalid principal"}}}
			}
			notes, err := planConfig(context.Background(), acceptPlanConfig, n)
			wantOmitted := tc.computed && tc.path && tc.noop
			if err != nil || isNull(n.config.Get("policy")) != wantOmitted || len(notes) == 0 || tc.path && !strings.Contains(notes[0].Reason, "policy") {
				t.Fatalf("config=%v notes=%v err=%v", n.config, notes, err)
			}
		})
	}
}

func TestPlanPruningKeepsUnrelatedNoncomputedValue(t *testing.T) {
	n := &node{tf: "test.example", schema: &schema.Resource{Wire: "test_object", Block: schema.Block{Attributes: []schema.Attribute{{Name: "a", Type: types.TInt, Optional: true}, {Name: "b", Type: types.TInt, Optional: true, Computed: true}}}}}
	n.state = value.ObjectVal(map[string]value.Value{"a": value.IntVal(1), "b": value.IntVal(2)})
	n.config = n.state
	n.plan = func(_ context.Context, c value.Value) (Plan, error) {
		if isNull(c.Get("b")) {
			return Plan{Value: c}, nil
		}
		ds := []provider.Diag{{Error: true, Path: value.Path{}.Attr("b"), Summary: "invalid b"}}
		if !isNull(c.Get("a")) {
			ds = append(ds, provider.Diag{Error: true, Path: value.Path{}.Attr("a"), Summary: "invalid a"})
		}
		return Plan{}, &provider.Error{Diags: ds}
	}
	notes, err := planConfig(context.Background(), acceptPlanConfig, n)
	if err != nil || !value.Equal(n.config.Get("a"), n.state.Get("a")) || len(notes) == 0 || !strings.Contains(notes[0].Reason, "a") || !strings.Contains(notes[0].Reason, "b") {
		t.Fatalf("config=%v notes=%v err=%v", n.config, notes, err)
	}
}

func TestDisabledOmissionsAreProvedAndBounded(t *testing.T) {
	for _, preserve := range []bool{false, true} {
		t.Run(fmt.Sprint(preserve), func(t *testing.T) {
			n := &node{tf: "test.large", schema: &schema.Resource{Wire: "test_object"}}
			fields := map[string]value.Value{}
			for i := range 130 {
				k := fmt.Sprintf("block_%03d", i)
				n.schema.Block.Blocks = append(n.schema.Block.Blocks, schema.NestedBlock{Name: k, Nesting: schema.NestList, ImportDisabled: &schema.DisabledWhen{Field: "enabled", Value: "false"}, Block: schema.Block{Attributes: []schema.Attribute{{Name: "enabled", Type: types.TBool, Optional: true}}}})
				fields[k] = value.ListVal([]value.Value{value.ObjectVal(map[string]value.Value{"enabled": value.BoolVal(false)})})
			}
			n.state = value.ObjectVal(fields)
			n.config = n.state
			calls := 0
			n.plan = func(_ context.Context, c value.Value) (Plan, error) {
				calls++
				if preserve {
					return Plan{Value: n.state}, nil
				}
				return Plan{Value: c}, nil
			}
			notes, err := omitDisabled(context.Background(), n)
			if err != nil || calls != 128 || !n.pruningStopped {
				t.Fatalf("calls=%d notes=%v err=%v", calls, notes, err)
			}
			for i, nb := range n.schema.Block.Blocks {
				want := 1
				if preserve && i >= 2 {
					want = 0
				}
				if n.config.Get(nb.Name).Len() != want {
					t.Fatalf("%s lost without proof or kept despite proof", nb.Name)
				}
			}
		})
	}
}

func TestValidationBoundStopsLaterPruningPasses(t *testing.T) {
	n := &node{tf: "test.large", schema: &schema.Resource{Wire: "test_object"}}
	fields := map[string]value.Value{}
	for i := range 130 {
		k := fmt.Sprintf("optional_%03d", i)
		n.schema.Block.Attributes = append(n.schema.Block.Attributes, schema.Attribute{Name: k, Type: types.TInt, Optional: true})
		fields[k] = value.IntVal(0)
	}
	n.state = value.ObjectVal(fields)
	n.config = n.state
	ask := func(_ context.Context, _ string, cfg value.Value) ([]provider.Diag, error) {
		for _, k := range cfg.Keys() {
			if !isNull(cfg.Get(k)) {
				return []provider.Diag{{Error: true, Path: value.Path{}.Attr(k), Summary: "invalid zero"}}, nil
			}
		}
		return nil, nil
	}
	if _, err := validate(context.Background(), ask, n, nil); err != nil {
		t.Fatal(err)
	}
	kept := setPath(n.config, value.Path{}.Attr("optional_000"), value.StringVal(""))
	plans := 0
	n.plan = func(context.Context, value.Value) (Plan, error) { plans++; return Plan{Value: n.state}, nil }
	n.emptied = []emptied{{path: value.Path{}.Attr("optional_000")}}
	if _, err := prove(context.Background(), ask, n, nil, false); err != nil {
		t.Fatal(err)
	}
	if _, err := planConfig(context.Background(), acceptPlanConfig, n); err != nil {
		t.Fatal(err)
	}
	if _, err := omitDisabled(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	if plans != 0 || !value.Equal(kept, n.config) {
		t.Fatalf("continued pruning after the bound: plans=%d", plans)
	}
}

func TestDiagnosticPathAttribution(t *testing.T) {
	b := schema.Block{Blocks: []schema.NestedBlock{{Name: "block", Nesting: schema.NestList}}}
	path := value.Path{}.Attr("block").Index(0).Attr("policy")
	for _, tc := range []struct {
		name, text string
		structured value.Path
		want       bool
	}{
		{"quoted path", `invalid "block[0].policy"`, nil, true},
		{"bare name", `policy: invalid principal`, nil, false},
		{"unquoted path", `invalid block[0].policy`, nil, false},
		{"other index", `invalid "block[1].policy"`, nil, false},
		{"structured wins", `invalid "block[0].policy"`, value.Path{}.Attr("other"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if diagnosticNames(b, provider.Diag{Error: true, Summary: tc.text, Path: tc.structured}, path) != tc.want {
				t.Fatal("incorrect diagnostic attribution")
			}
		})
	}
}

func TestValidationBoundRestoresUnprovedStrings(t *testing.T) {
	n := &node{tf: "test.large", schema: &schema.Resource{Wire: "test_object"}}
	fields := map[string]value.Value{"empty": value.StringVal("")}
	n.schema.Block.Attributes = append(n.schema.Block.Attributes, schema.Attribute{Name: "empty", Type: types.TString, Optional: true})
	for i := range 130 {
		k := fmt.Sprintf("optional_%03d", i)
		n.schema.Block.Attributes = append(n.schema.Block.Attributes, schema.Attribute{Name: k, Type: types.TInt, Optional: true})
		fields[k] = value.IntVal(0)
	}
	n.state = value.ObjectVal(fields)
	n.config = setPath(n.state, value.Path{}.Attr("empty"), value.NullVal())
	n.emptied = []emptied{{path: value.Path{}.Attr("empty")}}
	ask := func(_ context.Context, _ string, c value.Value) ([]provider.Diag, error) {
		for _, k := range c.Keys() {
			if k != "empty" && !isNull(c.Get(k)) {
				return []provider.Diag{{Error: true, Path: value.Path{}.Attr(k), Summary: "invalid zero"}}, nil
			}
		}
		return nil, nil
	}
	notes, err := validate(context.Background(), ask, n, nil)
	if err != nil || !n.pruningStopped || !value.Equal(n.config.Get("empty"), n.state.Get("empty")) || !strings.Contains(notes[len(notes)-1].Reason, "restores every unproved empty string") {
		t.Fatalf("config=%v notes=%v err=%v", n.config, notes, err)
	}
}
