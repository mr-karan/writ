package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mr-karan/writ/internal/addr"
	"github.com/mr-karan/writ/internal/provider"
	"github.com/mr-karan/writ/internal/schema"
	"github.com/mr-karan/writ/internal/types"
	"github.com/mr-karan/writ/internal/value"
)

// The schemas below are the attributes of the hashicorp/aws 6.70.0 types
// that these tests read, so the tests start no provider.

func str(name string, required, optional, computed bool) schema.Attribute {
	return schema.Attribute{Name: name, Type: types.TString, Required: required, Optional: optional, Computed: computed}
}

var (
	tagsAttr    = schema.Attribute{Name: "tags", Type: types.MapOf(types.TString), Optional: true}
	tagsAllAttr = schema.Attribute{Name: "tags_all", Type: types.MapOf(types.TString), Optional: true, Computed: true}
	routeFields = []string{
		"carrier_gateway_id", "cidr_block", "core_network_arn", "destination_prefix_list_id", "egress_only_gateway_id", "gateway_id",
		"ipv6_cidr_block", "local_gateway_id", "nat_gateway_id", "network_interface_id", "odb_network_arn", "transit_gateway_id",
		"vpc_endpoint_id", "vpc_peering_connection_id",
	}
)

// awsTypes builds the aws_route_table, aws_default_network_acl, and
// aws_subnet schemas. route is an attribute that holds records, as SDKv2's
// ConfigModeAttr makes it, and ingress and egress are nested set blocks.
func awsTypes(t *testing.T) *schema.Provider {
	t.Helper()
	var route []types.Field
	for _, f := range routeFields {
		route = append(route, types.Field{Name: f, Type: types.TString, Optional: true})
	}
	num := func(name string, required bool) schema.Attribute {
		return schema.Attribute{Name: name, Type: types.TFloat, Required: required, Optional: !required}
	}
	rule := schema.Block{Attributes: []schema.Attribute{
		str("action", true, false, false), str("cidr_block", false, true, false), num("from_port", true), num("icmp_code", false),
		num("icmp_type", false), str("ipv6_cidr_block", false, true, false), str("protocol", true, false, false), num("rule_no", true), num("to_port", true),
	}}
	raw := []schema.RawResource{
		{Wire: "aws_route_table", Block: schema.Block{Attributes: []schema.Attribute{
			str("arn", false, false, true), str("id", false, true, true), str("owner_id", false, false, true),
			str("region", false, true, true),
			{Name: "route", Type: types.ListOf(types.RecordOf(route...)), Optional: true, Computed: true},
			tagsAttr, tagsAllAttr, str("vpc_id", true, false, false),
		}}},
		{Wire: "aws_default_network_acl", Block: schema.Block{
			Attributes: []schema.Attribute{
				str("arn", false, false, true), str("default_network_acl_id", true, false, false), str("id", false, true, true),
				str("region", false, true, true), tagsAttr, tagsAllAttr,
			},
			Blocks: []schema.NestedBlock{{Name: "egress", Nesting: schema.NestSet, Block: rule}, {Name: "ingress", Nesting: schema.NestSet, Block: rule}},
		}},
		{Wire: "aws_subnet", Block: schema.Block{Attributes: []schema.Attribute{
			str("arn", false, false, true), str("cidr_block", false, true, true), str("customer_owned_ipv4_pool", false, true, false),
			str("id", false, true, true), str("ipv6_cidr_block", false, true, true), str("outpost_arn", false, true, false), str("region", false, true, true),
			tagsAttr, tagsAllAttr, str("vpc_id", true, false, false),
		}}},
		// A plugin framework type: its id is computed only, and "" is a
		// value the configuration set.
		{Wire: "aws_vpc_security_group_ingress_rule", Block: schema.Block{Attributes: []schema.Attribute{
			str("arn", false, false, true), str("cidr_ipv4", false, true, false), str("description", false, true, false),
			str("id", false, false, true), str("region", false, true, true), str("security_group_id", true, false, false),
		}}},
	}
	p, err := schema.NewProvider(schema.Source{Namespace: "hashicorp", Type: "aws"}, "6.70.0", schema.Block{}, raw)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func awsNode(t *testing.T, p *schema.Provider, wire, attrs string) *node {
	t.Helper()
	r, err := p.ResourceByWire(wire)
	if err != nil {
		t.Fatal(err)
	}
	st, err := decodeState(r.Block, json.RawMessage(attrs))
	if err != nil {
		t.Fatal(err)
	}
	return &node{tf: wire + ".this", schema: r, state: st, addr: addr.Root("this"), plan: unchanged(st)}
}

// planValue turns a func that plans a value into a Planner whose plans
// replace nothing.
func planValue(f func(context.Context, value.Value) (value.Value, error)) Planner {
	return func(ctx context.Context, c value.Value) (Plan, error) {
		v, err := f(ctx, c)
		return Plan{Value: v}, err
	}
}

// unchanged plans no change to the object prior, as a provider does for a
// config that leaves out only values it reads as unset.
func unchanged(prior value.Value) Planner {
	return func(context.Context, value.Value) (Plan, error) { return Plan{Value: prior}, nil }
}

// emptyStrings stands in for the provider's validators: it rejects a
// nested block or an attribute of records at the path of the whole set
// when an element holds "" in a field the check names, as aws 6.70.0 does
// for a route or a NACL rule.
func emptyStrings(sets []string, fields ...string) Validator {
	return func(_ context.Context, _ string, config value.Value) ([]provider.Diag, error) {
		var ds []provider.Diag
		for _, set := range sets {
			v := config.Get(set)
			if v.Kind() != value.List {
				continue
			}
			for i := range v.Len() {
				for _, f := range fields {
					if fv := v.Index(i).Get(f); fv.Kind() == value.String && fv.AsString() == "" {
						ds = append(ds, provider.Diag{Error: true, Summary: `"" is not a valid CIDR block`, Path: value.Path{}.Attr(set)})
					}
				}
			}
		}
		return ds, nil
	}
}

func TestValidateClearsZerosInsideSets(t *testing.T) {
	aws := awsTypes(t)
	route := func(cidr, ipv6, gw, eigw string) string {
		fields := map[string]string{}
		for _, f := range routeFields {
			fields[f] = ""
		}
		fields["cidr_block"], fields["ipv6_cidr_block"], fields["gateway_id"], fields["egress_only_gateway_id"] = cidr, ipv6, gw, eigw
		b, _ := json.Marshal(fields)
		return string(b)
	}
	const table = `{"id": "rtb-00000000000000001", "vpc_id": "vpc-00000000000000001", "route": [%s, %s]}`
	const acl = `{"id": "acl-00000000000000001", "default_network_acl_id": "acl-00000000000000001",
		"egress": [{"action": "allow", "cidr_block": "0.0.0.0/0", "from_port": 0, "icmp_code": 0, "icmp_type": 0, "ipv6_cidr_block": "", "protocol": "-1", "rule_no": 100, "to_port": 0}],
		"ingress": [{"action": "allow", "cidr_block": "", "from_port": 0, "icmp_code": 0, "icmp_type": 0, "ipv6_cidr_block": "::/0", "protocol": "-1", "rule_no": 101, "to_port": 0}]}`
	cidrs := []string{"cidr_block", "ipv6_cidr_block"}
	for _, tc := range []struct {
		name, wire, state string
		ask               Validator
		set               string
		// want maps each element of set to each field's value, nil or
		// absent for null.
		want []map[string]any
		// notes are in the problems the import reports, in order.
		notes []string
	}{
		{
			// aws_route_table is a legacy SDK type, so configure leaves out
			// every "" in a route, and the provider accepts the rest.
			name: "route table", wire: "aws_route_table",
			state: strings.Replace(strings.Replace(table, "%s", route("0.0.0.0/0", "", "igw-00000000000000001", ""), 1), "%s", route("", "::/0", "", "eigw-00000000000000001"), 1),
			ask:   emptyStrings([]string{"route"}, cidrs...), set: "route",
			want: []map[string]any{
				{"cidr_block": "0.0.0.0/0", "gateway_id": "igw-00000000000000001"},
				{"ipv6_cidr_block": "::/0", "egress_only_gateway_id": "eigw-00000000000000001"},
			},
		},
		{
			name: "default network acl", wire: "aws_default_network_acl", state: acl,
			ask: emptyStrings([]string{"ingress", "egress"}, cidrs...), set: "ingress",
			// configure leaves out the empty strings, so the provider
			// accepts the rest, zero numbers included.
			want: []map[string]any{{"action": "allow", "ipv6_cidr_block": "::/0", "from_port": int64(0), "icmp_code": int64(0), "icmp_type": int64(0), "protocol": "-1", "rule_no": int64(101), "to_port": int64(0)}},
		},
		{
			// A provider that rejects a zero number inside a set block
			// element gets the optional zeros cleared, and required ones
			// kept.
			name: "zero numbers in a set block", wire: "aws_default_network_acl", state: acl,
			ask: func(_ context.Context, _ string, config value.Value) ([]provider.Diag, error) {
				if v := config.Get("ingress").Index(0).Get("icmp_code"); isNull(v) {
					return nil, nil
				}
				return []provider.Diag{{Error: true, Summary: "icmp_code needs icmp", Path: value.Path{}.Attr("ingress")}}, nil
			},
			set: "ingress",
			// icmp_type keeps its 0, because the provider accepts it.
			want:  []map[string]any{{"action": "allow", "ipv6_cidr_block": "::/0", "from_port": int64(0), "icmp_type": int64(0), "protocol": "-1", "rule_no": int64(101), "to_port": int64(0)}},
			notes: []string{"the code leaves out ingress[0].icmp_code, because the provider rejects each of these empty or zero values and accepts null (ingress: icmp_code needs icmp)"},
		},
		{
			// A computed attribute the provider still rejects with its zeros
			// cleared is left out whole, and the import says so.
			name: "route table rejected whole", wire: "aws_route_table",
			state: strings.Replace(strings.Replace(table, "%s", route("0.0.0.0/0", "", "igw-00000000000000001", ""), 1), "%s", route("", "::/0", "", "eigw-00000000000000001"), 1),
			ask: func(_ context.Context, _ string, config value.Value) ([]provider.Diag, error) {
				if isNull(config.Get("route")) {
					return nil, nil
				}
				return []provider.Diag{{Error: true, Summary: "conflicting route targets", Path: value.Path{}.Attr("route")}}, nil
			},
			set:   "route",
			notes: []string{"route is left out of the code, because the provider rejects its value (route: conflicting route targets)"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := awsNode(t, aws, tc.wire, tc.state)
			configure(n)
			probs, err := validate(context.Background(), tc.ask, n, sources{})
			if err != nil {
				t.Fatal(err)
			}
			if len(probs) != len(tc.notes) {
				t.Fatalf("problems = %+v, want notes %q", probs, tc.notes)
			}
			for i, note := range tc.notes {
				if !strings.Contains(probs[i].Reason, note) {
					t.Errorf("problem %d = %q, want %q", i, probs[i].Reason, note)
				}
			}
			got := n.config.Get(tc.set)
			if tc.want == nil {
				if !isNull(got) {
					t.Errorf("%s = %v, want null", tc.set, got)
				}
				return
			}
			if got.Kind() != value.List || got.Len() != len(tc.want) {
				t.Fatalf("%s = %v, want %d elements", tc.set, got, len(tc.want))
			}
			for i, want := range tc.want {
				checkFields(t, fmt.Sprintf("%s[%d]", tc.set, i), got.Index(i), want)
			}
		})
	}
}

// checkFields reports each field of the record e that differs from want,
// where a field want lacks or maps to nil must be null.
func checkFields(t *testing.T, at string, e value.Value, want map[string]any) {
	t.Helper()
	for _, k := range e.Keys() {
		fv, w := e.Get(k), want[k]
		switch w := w.(type) {
		case nil:
			if !isNull(fv) {
				t.Errorf("%s.%s = %v, want null", at, k, fv)
			}
		case string:
			if fv.Kind() != value.String || fv.AsString() != w {
				t.Errorf("%s.%s = %v, want %q", at, k, fv, w)
			}
		case int64:
			if fv.Kind() != value.Int || fv.AsInt() != w {
				t.Errorf("%s.%s = %v, want %d", at, k, fv, w)
			}
		case bool:
			if fv.Kind() != value.Bool || fv.AsBool() != w {
				t.Errorf("%s.%s = %v, want %v", at, k, fv, w)
			}
		}
	}
}

// A provider that rejects one "" inside a list block, at the path of the
// block, gets only that "" left out. The false and 0 next to it validate,
// so they stay, and the code still enforces them: for an optional, computed
// field a null config keeps the prior value, so a plan would not revert
// drift to enabled: true, port: 443. A rejected top-level zero value is
// left out with a note too, and the same holds inside an attribute of
// records. The type is a plugin framework type, whose id
// is computed only, so configure keeps every "".
func TestValidateLeavesOutOnlyRejectedZeros(t *testing.T) {
	p, err := schema.NewProvider(schema.Source{Namespace: "example", Type: "net"}, "1.0.0", schema.Block{}, []schema.RawResource{
		{Wire: "net_forward", Block: schema.Block{
			Attributes: []schema.Attribute{
				str("id", false, false, true), str("name", true, false, false), str("prefix", false, true, false),
				{Name: "hop", Type: types.ListOf(types.RecordOf(
					types.Field{Name: "via", Type: types.TString, Optional: true},
					types.Field{Name: "weight", Type: types.TFloat, Optional: true},
				)), Optional: true, Computed: true},
			},
			Blocks: []schema.NestedBlock{{Name: "rule", Nesting: schema.NestList, Block: schema.Block{Attributes: []schema.Attribute{
				str("destination", false, true, false),
				{Name: "enabled", Type: types.TBool, Optional: true, Computed: true},
				{Name: "port", Type: types.TFloat, Optional: true, Computed: true},
			}}}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.ResourceByWire("net_forward")
	if err != nil {
		t.Fatal(err)
	}
	if r.LegacySDK {
		t.Fatal("net_forward reads as a legacy SDK type")
	}
	st, err := decodeState(r.Block, json.RawMessage(`{"id": "f-1", "name": "edge", "prefix": "",
		"rule": [{"destination": "", "enabled": false, "port": 0}, {"destination": "10.0.0.1", "enabled": false, "port": 0}],
		"hop": [{"via": "", "weight": 0}, {"via": "10.0.0.9", "weight": 0}]}`))
	if err != nil {
		t.Fatal(err)
	}
	n := &node{tf: "net_forward.edge", schema: r, state: st, addr: addr.Root("edge")}
	configure(n)
	var asked int
	ask := func(_ context.Context, _ string, config value.Value) ([]provider.Diag, error) {
		asked++
		var ds []provider.Diag
		if v := config.Get("prefix"); v.Kind() == value.String && v.AsString() == "" {
			ds = append(ds, provider.Diag{Error: true, Summary: "prefix must not be empty", Path: value.Path{}.Attr("prefix")})
		}
		for set, f := range map[string]string{"rule": "destination", "hop": "via"} {
			elems := config.Get(set)
			for i := range elems.Len() {
				if v := elems.Index(i).Get(f); v.Kind() == value.String && v.AsString() == "" {
					ds = append(ds, provider.Diag{Error: true, Summary: `"" is not an address`, Path: value.Path{}.Attr(set)})
				}
			}
		}
		return ds, nil
	}
	probs, err := validate(context.Background(), ask, n, sources{})
	if err != nil {
		t.Fatal(err)
	}
	notes := []string{
		"the code leaves out prefix, because the provider rejects the empty or zero value",
		"the code leaves out hop[0].via, rule[0].destination, because the provider rejects each of these empty or zero values and accepts null",
	}
	if len(probs) != len(notes) {
		t.Fatalf("problems = %+v, want notes %q", probs, notes)
	}
	for i, note := range notes {
		if !strings.Contains(probs[i].Reason, note) {
			t.Errorf("problem %d = %q, want %q", i, probs[i].Reason, note)
		}
	}
	if !isNull(n.config.Get("prefix")) {
		t.Errorf("prefix = %v, want null", n.config.Get("prefix"))
	}
	rules := n.config.Get("rule")
	if rules.Len() != 2 {
		t.Fatalf("rule = %v, want 2 elements", rules)
	}
	checkFields(t, "rule[0]", rules.Index(0), map[string]any{"enabled": false, "port": int64(0)})
	checkFields(t, "rule[1]", rules.Index(1), map[string]any{"destination": "10.0.0.1", "enabled": false, "port": int64(0)})
	hops := n.config.Get("hop")
	checkFields(t, "hop[0]", hops.Index(0), map[string]any{"weight": int64(0)})
	checkFields(t, "hop[1]", hops.Index(1), map[string]any{"via": "10.0.0.9", "weight": int64(0)})
	// One ask per round, and one per zero tried, each bounded by the zero
	// values: prefix, then via and destination, then the final check.
	if asked > 10 {
		t.Errorf("asked the provider %d times, want at most 10", asked)
	}
}

func TestConfigureLeavesOutDerivedValues(t *testing.T) {
	aws := awsTypes(t)
	east := value.ObjectVal(map[string]value.Value{"region": value.StringVal("us-east-1"), "access_key": value.StringVal("mock").Secret()})
	west := value.ObjectVal(map[string]value.Value{"region": value.StringVal("us-west-2")})
	const subnet = `{"id": "subnet-00000000000000001", "arn": "arn:aws:ec2:us-east-1:123456789012:subnet/subnet-00000000000000001",
		"vpc_id": "vpc-00000000000000001", "cidr_block": "10.0.1.0/24", "region": "us-east-1",
		"outpost_arn": "", "customer_owned_ipv4_pool": "", "ipv6_cidr_block": "", %s}`
	const rule = `{"id": "sgr-00000000000000001", "security_group_id": "sg-00000000000000001", "cidr_ipv4": "10.0.0.0/8", "description": "", "region": "us-east-1"%s}`
	for _, tc := range []struct {
		name     string
		wire     string
		state    string
		tags     string
		defaults value.Value
		gone     []string
		kept     []string
	}{
		{
			name: "tags_all repeats tags", wire: "aws_subnet", state: subnet, tags: `"tags": {"Name": "a"}, "tags_all": {"Name": "a"}`, defaults: east,
			gone: []string{"outpost_arn", "customer_owned_ipv4_pool", "tags_all", "region"},
			// ipv6_cidr_block is optional and computed, so its "" stays:
			// a null config would hand the value to the provider.
			kept: []string{"cidr_block", "ipv6_cidr_block", "tags", "vpc_id"},
		},
		{
			name: "both empty", wire: "aws_subnet", state: subnet, tags: `"tags": null, "tags_all": {}`, defaults: east,
			gone: []string{"tags_all", "region"},
		},
		{
			// default_tags added Team, so tags_all holds more than tags.
			name: "tags_all holds default tags", wire: "aws_subnet", state: subnet, tags: `"tags": {"Name": "a"}, "tags_all": {"Name": "a", "Team": "net"}`, defaults: west,
			gone: []string{"outpost_arn"},
			kept: []string{"tags", "tags_all", "region"},
		},
		{
			name: "provider unknown", wire: "aws_subnet", state: subnet, tags: `"tags": {}, "tags_all": {}`, defaults: value.NullVal(),
			gone: []string{"tags_all"},
			kept: []string{"region", "tags"},
		},
		{
			name: "plugin framework keeps empty strings", wire: "aws_vpc_security_group_ingress_rule", state: rule, defaults: east,
			gone: []string{"region"},
			kept: []string{"description", "cidr_ipv4"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := awsNode(t, aws, tc.wire, strings.Replace(tc.state, "%s", tc.tags, 1))
			n.defaults = tc.defaults
			configure(n)
			for _, k := range tc.gone {
				if v := n.config.Get(k); !isNull(v) {
					t.Errorf("%s = %v, want it left out", k, v)
				}
			}
			for _, k := range tc.kept {
				if isNull(n.config.Get(k)) {
					t.Errorf("%s was left out", k)
				}
			}
		})
	}
}

// In a legacy SDK type, an optional, computed "" inside the records of an
// attribute stays in the code while the provider accepts it, because a
// null config would stop the code from enforcing it. When the provider
// rejects it, it is left out with a note. An optional "" that is not
// computed is left out with no note, because the SDK reads it as unset.
func TestDeclareKeepsComputedEmptyStrings(t *testing.T) {
	p, err := schema.NewProvider(schema.Source{Namespace: "example", Type: "fw"}, "1.0.0", schema.Block{}, []schema.RawResource{
		{Wire: "fw_policy", Block: schema.Block{Attributes: []schema.Attribute{
			str("id", false, true, true), str("name", true, false, false),
			{Name: "rules", Type: types.ListOf(types.RecordOf(
				types.Field{Name: "description", Type: types.TString, Optional: true, Computed: true},
				types.Field{Name: "label", Type: types.TString, Optional: true},
				types.Field{Name: "port", Type: types.TFloat, Optional: true},
			)), Optional: true},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.ResourceByWire("fw_policy")
	if err != nil {
		t.Fatal(err)
	}
	if !r.LegacySDK {
		t.Fatal("fw_policy does not read as a legacy SDK type")
	}
	st := value.ObjectVal(map[string]value.Value{
		"id": value.StringVal("p-1"), "name": value.StringVal("edge"),
		"rules": value.ListVal([]value.Value{value.ObjectVal(map[string]value.Value{
			"description": value.StringVal(""), "label": value.StringVal(""), "port": value.IntVal(0),
		})}),
	})
	rejects := func(_ context.Context, _ string, config value.Value) ([]provider.Diag, error) {
		if v := config.Get("rules").Index(0).Get("description"); v.Kind() == value.String && v.AsString() == "" {
			return []provider.Diag{{Error: true, Summary: "description must not be empty", Path: value.Path{}.Attr("rules")}}, nil
		}
		return nil, nil
	}
	for _, tc := range []struct {
		name  string
		ask   Validator
		code  []string
		gone  []string
		notes []string
	}{
		{
			name: "accepted", ask: func(context.Context, string, value.Value) ([]provider.Diag, error) { return nil, nil },
			code: []string{`description = ""`, "port = 0"},
			gone: []string{"label"},
		},
		{
			name: "rejected", ask: rejects,
			code:  []string{"port = 0"},
			gone:  []string{"label", "description"},
			notes: []string{"the code leaves out rules[0].description, because the provider rejects each of these empty or zero values and accepts null"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, notes := declare(t, testObject{Schema: r, State: st, Plan: unchanged(st)}, tc.ask)
			for _, c := range tc.code {
				if !strings.Contains(code, c) {
					t.Errorf("the code lacks %s:\n%s", c, code)
				}
			}
			for _, g := range tc.gone {
				if strings.Contains(code, g) {
					t.Errorf("the code still sets %s:\n%s", g, code)
				}
			}
			if len(notes) != len(tc.notes) {
				t.Fatalf("notes = %q, want %q", notes, tc.notes)
			}
			for i, note := range tc.notes {
				if !strings.Contains(notes[i], note) {
					t.Errorf("note %d = %q, want %q", i, notes[i], note)
				}
			}
		})
	}
}

// The schema does not say which attributes have a default. An SDKv2 field
// with Default "x" and a prior "" plans no change while the config holds
// "", and "" to "x" once the config leaves it out, because the SDK fills in
// the default only for a field the config lacks. So the "" values are left
// out only when a plan without them equals a plan with them. Here the
// plans differ at mode and rules, so the code keeps every "", with one
// note. A provider with no defaults plans the same both ways, and every ""
// goes with no note. With no planner every
// "" stays, and so it does when a plan fails.
func TestProveKeepsAnEmptyStringWithADefault(t *testing.T) {
	p, err := schema.NewProvider(schema.Source{Namespace: "example", Type: "fw"}, "1.0.0", schema.Block{}, []schema.RawResource{
		{Wire: "fw_rule", Block: schema.Block{Attributes: []schema.Attribute{
			str("id", false, true, true), str("name", true, false, false),
			str("mode", false, true, false), str("label", false, true, false),
			{Name: "rules", Type: types.ListOf(types.RecordOf(
				types.Field{Name: "proto", Type: types.TString, Optional: true},
				types.Field{Name: "note", Type: types.TString, Optional: true},
			)), Optional: true},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.ResourceByWire("fw_rule")
	if err != nil {
		t.Fatal(err)
	}
	if !r.LegacySDK {
		t.Fatal("fw_rule does not read as a legacy SDK type")
	}
	rule := func(proto, note string) value.Value {
		return value.ObjectVal(map[string]value.Value{"proto": value.StringVal(proto), "note": value.StringVal(note)})
	}
	st := value.ObjectVal(map[string]value.Value{
		"id": value.StringVal("r-1"), "name": value.StringVal("edge"),
		"mode": value.StringVal(""), "label": value.StringVal(""),
		"rules": value.ListVal([]value.Value{rule("", "")}),
	})
	// defaults plans the object as SDKv2 does for mode and proto, which
	// have Default "x" and "tcp": a field the config lacks gets the default.
	var plans atomic.Int32
	defaults := func(_ context.Context, config value.Value) (value.Value, error) {
		plans.Add(1)
		mode := config.Get("mode")
		if isNull(mode) {
			mode = value.StringVal("x")
		}
		var rules []value.Value
		cr := config.Get("rules")
		for i := range cr.Len() {
			proto, note := cr.Index(i).Get("proto"), cr.Index(i).Get("note")
			if isNull(proto) {
				proto = value.StringVal("tcp")
			}
			if isNull(note) {
				note = value.StringVal("")
			}
			rules = append(rules, value.ObjectVal(map[string]value.Value{"proto": proto, "note": note}))
		}
		label := config.Get("label")
		if isNull(label) {
			label = value.StringVal("")
		}
		return value.ObjectVal(map[string]value.Value{
			"id": st.Get("id"), "name": config.Get("name"), "mode": mode, "label": label, "rules": value.ListVal(rules),
		}), nil
	}
	// none plans as a provider with no defaults: an unset string is "".
	none := func(_ context.Context, config value.Value) (value.Value, error) {
		plans.Add(1)
		empty := func(v value.Value) value.Value {
			if isNull(v) {
				return value.StringVal("")
			}
			return v
		}
		var rules []value.Value
		cr := config.Get("rules")
		for i := range cr.Len() {
			rules = append(rules, value.ObjectVal(map[string]value.Value{"proto": empty(cr.Index(i).Get("proto")), "note": empty(cr.Index(i).Get("note"))}))
		}
		return value.ObjectVal(map[string]value.Value{
			"id": st.Get("id"), "name": config.Get("name"), "mode": empty(config.Get("mode")), "label": empty(config.Get("label")), "rules": value.ListVal(rules),
		}), nil
	}
	accepts := func(context.Context, string, value.Value) ([]provider.Diag, error) { return nil, nil }
	fails := func(context.Context, value.Value) (value.Value, error) {
		return value.Value{}, errors.New("plan failed")
	}
	for _, tc := range []struct {
		name  string
		plan  Planner
		code  []string
		gone  []string
		notes []string
		plans int
	}{
		{
			name: "default", plan: planValue(defaults),
			code:  []string{`mode = ""`, `label = ""`, `proto = ""`, `note = ""`},
			notes: []string{`the code keeps every "" this object holds, because a plan without them differs from a plan with them at mode, rules`},
			plans: 3,
		},
		{
			name: "no default", plan: planValue(none),
			gone:  []string{"mode", "label", "proto", "note"},
			plans: 3,
		},
		{name: "no planner", code: []string{`mode = ""`, `label = ""`, `proto = ""`, `note = ""`}},
		{name: "plan fails", plan: planValue(fails), code: []string{`mode = ""`, `label = ""`, `proto = ""`, `note = ""`}, notes: []string{"could not prove a no-change plan for the generated code, because the plan request failed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plans.Store(0)
			code, notes := declare(t, testObject{Schema: r, State: st, Plan: tc.plan}, accepts)
			for _, c := range tc.code {
				if !strings.Contains(code, c) {
					t.Errorf("the code lacks %s:\n%s", c, code)
				}
			}
			for _, g := range tc.gone {
				if strings.Contains(code, g) {
					t.Errorf("the code still sets %s:\n%s", g, code)
				}
			}
			if len(notes) != len(tc.notes) {
				t.Fatalf("notes = %q, want %q", notes, tc.notes)
			}
			for i, note := range tc.notes {
				if !strings.Contains(notes[i], note) {
					t.Errorf("note %d = %q, want %q", i, notes[i], note)
				}
			}
			if got := int(plans.Load()); got != tc.plans {
				t.Errorf("planned %d times, want %d", got, tc.plans)
			}
		})
	}
}

// Each planner below stands in for an SDKv2 resource whose plan of the code
// without its "" values differs from the plan with them in a way that a
// check of the omitted fields alone misses. Each time the code keeps every
// "", with a note.
func TestProveComparesWholePlans(t *testing.T) {
	p, err := schema.NewProvider(schema.Source{Namespace: "example", Type: "fw"}, "1.0.0", schema.Block{}, []schema.RawResource{
		{Wire: "fw_rule", Block: schema.Block{Attributes: []schema.Attribute{
			str("id", false, true, true), str("name", true, false, false),
			str("mode", false, true, false), str("label", false, true, false), str("prefix", false, true, false),
			str("source", false, false, true),
			{Name: "rules", Type: types.ListOf(types.RecordOf(
				types.Field{Name: "id", Type: types.TString, Optional: true},
				types.Field{Name: "label", Type: types.TString, Optional: true},
			)), Optional: true},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.ResourceByWire("fw_rule")
	if err != nil {
		t.Fatal(err)
	}
	rule := func(id, label string) value.Value {
		return value.ObjectVal(map[string]value.Value{"id": value.StringVal(id), "label": value.StringVal(label)})
	}
	st := value.ObjectVal(map[string]value.Value{
		"id": value.StringVal("r-1"), "name": value.StringVal("edge"),
		"mode": value.StringVal(""), "label": value.StringVal(""), "prefix": value.StringVal(""),
		"source": value.StringVal("explicit"),
		"rules":  value.ListVal([]value.Value{rule("a", ""), rule("b", "x")}),
	})
	// with returns the object with the attributes in set changed.
	with := func(set map[string]value.Value) value.Value {
		fields := map[string]value.Value{}
		for _, k := range st.Keys() {
			fields[k] = st.Get(k)
		}
		for k, v := range set {
			fields[k] = v
		}
		return value.ObjectVal(fields)
	}
	for _, tc := range []struct {
		name string
		plan func(config value.Value) value.Value
		at   string
	}{
		{
			// mode has Default "x" and label Default "y", and label's diff
			// is suppressed while mode is "x". Without the "", the plan
			// changes mode and not label; a proof that put back only mode
			// would then plan label to "y" and leave label out silently.
			name: "suppressed default",
			plan: func(c value.Value) value.Value {
				mode := "x"
				if !isNull(c.Get("mode")) {
					mode = c.Get("mode").AsString()
				}
				set := map[string]value.Value{"mode": value.StringVal(mode)}
				if isNull(c.Get("label")) && mode != "x" {
					set["label"] = value.StringVal("y")
				}
				return with(set)
			},
			at: "mode",
		},
		{
			// Leaving out prefix keeps prefix as it is, and changes a
			// computed sibling.
			name: "computed sibling",
			plan: func(c value.Value) value.Value {
				if isNull(c.Get("prefix")) {
					return with(map[string]value.Value{"source": value.StringVal("default")})
				}
				return st
			},
			at: "source",
		},
		{
			// The labels change places: each field holds the same values
			// across the elements, and each element holds another.
			name: "swapped elements",
			plan: func(c value.Value) value.Value {
				if isNull(c.Get("rules").Index(0).Get("label")) {
					return with(map[string]value.Value{"rules": value.ListVal([]value.Value{rule("a", "x"), rule("b", "")})})
				}
				return st
			},
			at: "rules",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := func(_ context.Context, c value.Value) (Plan, error) { return Plan{Value: tc.plan(c)}, nil }
			accepts := func(context.Context, string, value.Value) ([]provider.Diag, error) { return nil, nil }
			code, notes := declare(t, testObject{Schema: r, State: st, Plan: plan}, accepts)
			for _, c := range []string{`mode = ""`, `label = ""`, `prefix = ""`} {
				if !strings.Contains(code, c) {
					t.Errorf("the code lacks %s:\n%s", c, code)
				}
			}
			want := "because a plan without them differs from a plan with them at " + tc.at
			if len(notes) != 1 || !strings.Contains(notes[0], want) {
				t.Errorf("notes = %q, want one with %q", notes, want)
			}
		})
	}
}

// A plan proves nothing about a value it leaves unknown, nor about an
// object that only one of the two plans replaces, so each time the code
// keeps every "", with a note. Both reproduce SDKv2 v2.40.0 behaviour:
// CustomizeDiff marks a computed attribute unknown with SetNewComputed,
// and ForceNew on a field the config leaves out replaces the object.
func TestProveNeedsKnownPlansThatReplaceAlike(t *testing.T) {
	p, err := schema.NewProvider(schema.Source{Namespace: "example", Type: "fw"}, "1.0.0", schema.Block{}, []schema.RawResource{
		{Wire: "fw_rule", Block: schema.Block{Attributes: []schema.Attribute{
			str("id", false, true, true), str("name", true, false, false),
			str("prefix", false, true, false), str("result", false, false, true),
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.ResourceByWire("fw_rule")
	if err != nil {
		t.Fatal(err)
	}
	st := value.ObjectVal(map[string]value.Value{
		"id": value.StringVal("r-1"), "name": value.StringVal("edge"), "prefix": value.StringVal(""), "result": value.StringVal("explicit"),
	})
	unknown := value.ObjectVal(map[string]value.Value{
		"id": value.StringVal("r-1"), "name": value.StringVal("edge"), "prefix": value.StringVal(""), "result": value.UnknownVal(false),
	})
	for _, tc := range []struct {
		name string
		plan Planner
		note string
	}{
		{
			// Both plans leave result unknown, so they compare equal, and
			// the apply may still resolve it to "default" for one and
			// "explicit" for the other.
			name: "unknown",
			plan: func(context.Context, value.Value) (Plan, error) { return Plan{Value: unknown}, nil },
			note: `the code keeps every "" this object holds, because a plan leaves result unknown until the apply`,
		},
		{
			// Both plans hold the same value, and only the one without
			// prefix replaces the object.
			name: "replace",
			plan: func(_ context.Context, c value.Value) (Plan, error) {
				if isNull(c.Get("prefix")) {
					return Plan{Value: st, RequiresReplace: []value.Path{value.Path{}.Attr("prefix")}}, nil
				}
				return Plan{Value: st}, nil
			},
			note: `the code keeps every "" this object holds, because a plan without them replaces the object on other paths than a plan with them: prefix`,
		},
		{
			name: "private",
			plan: func(_ context.Context, c value.Value) (Plan, error) {
				if isNull(c.Get("prefix")) {
					return Plan{Value: st, Private: []byte("a")}, nil
				}
				return Plan{Value: st, Private: []byte("b")}, nil
			},
			note: "the provider keeps other private data",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accepts := func(context.Context, string, value.Value) ([]provider.Diag, error) { return nil, nil }
			code, notes := declare(t, testObject{Schema: r, State: st, Plan: tc.plan}, accepts)
			if !strings.Contains(code, `prefix = ""`) {
				t.Errorf("the code lacks prefix: \"\":\n%s", code)
			}
			wantNotes := 1
			if tc.name == "unknown" {
				wantNotes = 2
			}
			if len(notes) != wantNotes || !strings.Contains(notes[0], tc.note) {
				t.Fatalf("notes = %q, want %d with %q", notes, wantNotes, tc.note)
			}
			if wantNotes == 2 && (!strings.Contains(notes[1], "could not prove a no-change plan") || !strings.Contains(notes[1], tc.name)) {
				t.Errorf("missing final plan explanation: %q", notes[1])
			}
		})
	}
	// Replace paths in another order are the same set.
	same := func(_ context.Context, c value.Value) (Plan, error) {
		rr := []value.Path{value.Path{}.Attr("name"), value.Path{}.Attr("id")}
		if isNull(c.Get("prefix")) {
			rr = []value.Path{rr[1], rr[0]}
		}
		return Plan{Value: st, RequiresReplace: rr}, nil
	}
	accepts := func(context.Context, string, value.Value) ([]provider.Diag, error) { return nil, nil }
	code, notes := declare(t, testObject{Schema: r, State: st, Plan: same}, accepts)
	if strings.Contains(code, "prefix") || len(notes) != 1 || !strings.Contains(notes[0], "could not prove") {
		t.Errorf("replace paths in another order kept prefix, notes %q:\n%s", notes, code)
	}
}

// No import note prints a key of a secret map, whether the path comes from
// a plan's replace paths or from a provider diagnostic, which may also echo
// the key in its text.
func TestNotesRedactSecretPaths(t *testing.T) {
	const key = "s3cr3t-key-name"
	p, err := schema.NewProvider(schema.Source{Namespace: "example", Type: "fw"}, "1.0.0", schema.Block{}, []schema.RawResource{
		{Wire: "fw_rule", Block: schema.Block{Attributes: []schema.Attribute{
			str("id", false, true, true), str("name", true, false, false), str("prefix", false, true, false),
			{Name: "credentials", Type: types.MapOf(types.TString), Computed: true, Sensitive: true},
			{Name: "tokens", Type: types.MapOf(types.TString), Optional: true, Sensitive: true},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.ResourceByWire("fw_rule")
	if err != nil {
		t.Fatal(err)
	}
	secretMap := value.ObjectVal(map[string]value.Value{key: value.StringVal("token-value-1")}).Secret()
	st := value.ObjectVal(map[string]value.Value{
		"id": value.StringVal("r-1"), "name": value.StringVal("edge"), "prefix": value.StringVal(""),
		"credentials": secretMap, "tokens": secretMap,
	})
	at := func(attr string) value.Path { return value.Path{}.Attr(attr).Key(key) }
	accepts := func(context.Context, string, value.Value) ([]provider.Diag, error) { return nil, nil }
	for _, tc := range []struct {
		name string
		plan Planner
		ask  Validator
		want string
	}{
		{
			name: "replace path",
			plan: func(_ context.Context, c value.Value) (Plan, error) {
				if isNull(c.Get("prefix")) {
					return Plan{Value: st, RequiresReplace: []value.Path{at("credentials")}}, nil
				}
				return Plan{Value: st}, nil
			},
			ask:  accepts,
			want: "replaces the object on other paths than a plan with them: credentials[(secret)]",
		},
		{
			name: "diagnostic",
			plan: unchanged(st),
			ask: func(context.Context, string, value.Value) ([]provider.Diag, error) {
				return []provider.Diag{{Error: true, Summary: "bad token " + key, Path: at("tokens")}}, nil
			},
			want: "the provider rejects the generated code: tokens[(secret)]: the provider reported an error about a secret, and Writ withholds its text",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, notes := declare(t, testObject{Schema: r, State: st, Plan: tc.plan}, tc.ask)
			text := strings.Join(notes, "\n")
			if strings.Contains(text, key) || strings.Contains(code, key) {
				t.Errorf("the key is in the notes or the code:\n%s\n%s", text, code)
			}
			if !strings.Contains(text, tc.want) {
				t.Errorf("notes = %q, want one with %q", notes, tc.want)
			}
		})
	}
}
