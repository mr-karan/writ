package provider_test

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mr-karan/writ/internal/addr"
	"github.com/mr-karan/writ/internal/diag"
	"github.com/mr-karan/writ/internal/importer"
	"github.com/mr-karan/writ/internal/provider"
	"github.com/mr-karan/writ/internal/schema"
	"github.com/mr-karan/writ/internal/types"
	"github.com/mr-karan/writ/internal/value"
)

func TestOfflineAWSImportPlan(t *testing.T) {
	c, _ := provider.OfflineAWS(t)
	reader := &offlineBulkReader{client: c, objects: map[string]importer.Found{}, wire: map[types.Name]string{}}
	var selections []importer.Selection
	ctx := context.Background()
	cases := []struct{ name, wire, state string }{
		{"off", "aws_lb_listener", `{"arn":"arn:aws:elasticloadbalancing:us-east-1:123456789012:listener/app/example/123/456","load_balancer_arn":"arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/app/example/123","port":443,"protocol":"HTTPS","certificate_arn":"arn:aws:acm:us-east-1:123456789012:certificate/12345678-1234-1234-1234-123456789012","default_action":[{"type":"fixed-response","order":1,"fixed_response":[{"content_type":"text/plain","status_code":"200"}]}],"mutual_authentication":[{"mode":"%s","ignore_client_certificate_expiry":false,"trust_store_arn":"%s","advertise_trust_store_ca_names":"off"}]}`},
	}
	template := cases[0].state
	cases[0].state = fmt.Sprintf(template, "off", "")
	for _, mode := range []string{"passthrough", "verify"} {
		trust := ""
		if mode == "verify" {
			trust = "arn:aws:elasticloadbalancing:us-east-1:123456789012:truststore/example/1234567890123456"
		}
		cases = append(cases, struct{ name, wire, state string }{mode, "aws_lb_listener", fmt.Sprintf(template, mode, trust)})
	}
	for _, d := range []int{0, 1, 60, 3600} {
		cases = append(cases, struct{ name, wire, state string }{fmt.Sprintf("duration%d", d), "aws_lb_listener_rule", fmt.Sprintf(`{"arn":"arn:aws:elasticloadbalancing:us-east-1:123456789012:listener-rule/app/example/123/456/789","listener_arn":"arn:aws:elasticloadbalancing:us-east-1:123456789012:listener/app/example/123/456","priority":10,"condition":[{"path_pattern":[{"values":["/*"]}]}],"action":[{"type":"forward","order":1,"forward":[{"target_group":[{"arn":"arn:aws:elasticloadbalancing:us-east-1:123456789012:targetgroup/example/1234567890123456","weight":1}],"stickiness":[{"enabled":false,"duration":%d}]}]}]}`, d)})
	}
	cases = append(cases, struct{ name, wire, state string }{"deprecated bucket", "aws_s3_bucket", `{"id":"example","bucket":"example","grant":[{"type":"CanonicalUser","id":"0123456789012345678901234567890123456789012345678901234567890123","permissions":["READ"]}],"versioning":[{"enabled":true,"mfa_delete":false}],"lifecycle_rule":[{"id":"expire","enabled":true,"prefix":"","expiration":[{"days":30}]}],"server_side_encryption_configuration":[{"rule":[{"apply_server_side_encryption_by_default":[{"sse_algorithm":"AES256"}]}]}]}`}, struct{ name, wire, state string }{"deprecated role", "aws_iam_role", `{"id":"app","name":"app","assume_role_policy":"{\"Version\":\"2012-10-17\",\"Statement\":[]}","inline_policy":[{"name":"read","policy":"{\"Version\":\"2012-10-17\",\"Statement\":[]}"}]}`})
	expanded := append([]struct{ name, wire, state string }{}, cases[:3]...)
	for _, tc := range cases[:3] {
		tc.name += " second"
		expanded = append(expanded, tc)
	}
	for _, tc := range cases[3:7] {
		expanded = append(expanded, tc)
		tc.name += " second"
		expanded = append(expanded, tc)
	}
	enabled := cases[5]
	enabled.name = "enabled"
	enabled.state = strings.ReplaceAll(enabled.state, `"enabled":false`, `"enabled":true`)
	expanded = append(expanded, enabled)
	plain := cases[5]
	plain.name = "plain forward"
	plain.state = strings.ReplaceAll(plain.state, `,"stickiness":[{"enabled":false,"duration":60}]`, ``)
	expanded = append(expanded, plain)
	for _, tc := range cases[7:] {
		expanded = append(expanded, tc)
		tc.name += " renamed"
		expanded = append(expanded, tc)
	}
	cases = expanded
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if strings.HasPrefix(tc.name, "passthrough") {
				tc.state = strings.ReplaceAll(tc.state, `,"trust_store_arn":""`, ``)
			}
			r, prior := provider.ImportFixture(t, tc.wire, tc.state)
			if v := prior.Get("arn"); v.Kind() == value.String && strings.Contains(tc.wire, "listener") {
				prior = testField(prior, "arn", value.StringVal(v.AsString()+fmt.Sprintf("%02x", len(selections))))
			}
			if strings.HasSuffix(tc.name, "renamed") {
				if tc.wire == "aws_s3_bucket" {
					prior = testField(prior, "bucket", value.StringVal("owned"))
					prior = testField(prior, "id", value.StringVal("owned"))
				} else {
					prior = testField(prior, "name", value.StringVal("owned"))
					prior = testField(prior, "id", value.StringVal("owned"))
				}
			}
			// Fill defaults from a valid plan, then restore the imported edge case.
			for _, a := range r.Block.Attributes {
				if a.Computed && prior.Get(a.Name).Kind() == value.Null {
					prior = testField(prior, a.Name, importZero(a.Type))
				}
			}
			if tc.wire == "aws_lb_listener" || tc.wire == "aws_lb_listener_rule" {
				prior = testField(prior, "id", prior.Get("arn"))
			}
			seed := prior
			safe := prior
			mutual := prior.Get("mutual_authentication")
			if mutual.Kind() != value.Null {
				fields := map[string]value.Value{"mode": mutual.Get("mode")}
				if strings.HasPrefix(tc.name, "verify") {
					fields["trust_store_arn"] = mutual.Get("trust_store_arn")
					fields["ignore_client_certificate_expiry"] = value.BoolVal(false)
					fields["advertise_trust_store_ca_names"] = value.StringVal("off")
				}
				safe = testField(safe, "mutual_authentication", value.ObjectVal(fields))
			} else if tc.name == "duration0" {
				action := safe.Get("action").Index(0)
				action = testField(action, "forward", testField(action.Get("forward"), "stickiness", value.NullVal()))
				safe = testField(safe, "action", value.ListVal([]value.Value{action}))
			}
			for _, a := range r.Block.Attributes {
				if a.Computed && !a.Optional {
					safe = testField(safe, a.Name, value.NullVal())
				}
			}
			safe = testField(safe, "id", value.NullVal())
			if tc.wire == "aws_lb_listener" {
				safe = testField(safe, "tcp_idle_timeout_seconds", value.NullVal())
			}
			initial, err := c.Plan(ctx, provider.PlanRequest{Type: tc.wire, Prior: seed, Config: safe, Proposed: importProposed(r.Block, seed, safe)})
			if err != nil {
				t.Fatal(err)
			}
			prior = initial.Planned
			if mutual.Kind() != value.Null {
				prior = testField(prior, "mutual_authentication", mutual)
			} else if tc.wire == "aws_lb_listener_rule" {
				prior = testField(prior, "action", seed.Get("action"))
			}

			id := fmt.Sprintf("fixture-%02d", len(selections))
			reader.objects[id] = importer.Found{Schema: r, Source: schema.Source{Namespace: "hashicorp", Type: "aws"}, State: prior}
			reader.wire[r.Name] = r.Wire
			selections = append(selections, importer.Selection{Type: r.Name, IDs: []string{id}})
			var accepted value.Value
			planner := func(ctx context.Context, cfg value.Value) (importer.Plan, error) {

				p, err := c.Plan(ctx, provider.PlanRequest{Type: tc.wire, Prior: prior, Config: cfg, Proposed: importProposed(r.Block, prior, cfg)})

				if err == nil && value.Equal(p.Planned, prior) && len(p.RequiresReplace) == 0 {
					accepted = cfg
				}
				return importer.Plan{Value: p.Planned, RequiresReplace: p.RequiresReplace, Private: p.Private}, err
			}
			code, notes := importOne(t, r, prior, planner, c.Validate)
			if strings.HasPrefix(tc.name, "passthrough") {
				count := 0
				for _, note := range notes {
					if strings.Contains(note, "a provider diagnostic names its complete path") {
						count++
					}
				}
				if count != 2 {
					t.Fatalf("want exactly two attributed omissions: %v", notes)
				}
				for _, field := range []string{"advertise_trust_store_ca_names", "ignore_client_certificate_expiry"} {
					if sets(code, field) {
						t.Fatalf("kept conflict %s: %v", field, notes)
					}
					if !strings.Contains(strings.Join(notes, " "), "mutual_authentication."+field) {
						t.Fatalf("missing full omission path %s: %v", field, notes)
					}
				}
			}

			if accepted.Kind() != value.Object {
				t.Fatalf("no no-change plan: %v", notes)
			}
			for _, note := range notes {
				if strings.Contains(note, "could not prove") || strings.Contains(note, "rejects the generated") {
					t.Fatal(note)
				}
			}
			if strings.HasPrefix(tc.name, "verify") {
				for _, field := range []string{"ignore_client_certificate_expiry", "trust_store_arn", "advertise_trust_store_ca_names"} {
					if !sets(code, field) {
						t.Fatalf("lost %s", field)
					}
				}
			}
			if strings.HasPrefix(tc.name, "duration60") || strings.HasPrefix(tc.name, "duration3600") {
				if !sets(code, "duration") {
					t.Fatal("lost stored duration")
				}
			}

			t.Log(code, notes)
		})
	}
	result, err := importer.Bulk(ctx, importer.BulkOptions{Selections: selections, Reader: reader})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("20-object bulk: objects=%d plan_calls=%d validate_calls=%d total=%d", len(result.Objects), reader.plans.Load(), reader.validates.Load(), reader.plans.Load()+reader.validates.Load())
	if len(result.Objects) != 20 || result.Failed() {
		t.Fatalf("bulk objects=%d problems=%v", len(result.Objects), result.Problems)
	}
	for _, p := range result.Problems {
		if strings.Contains(p.Reason, "hand fix") || strings.Contains(p.Reason, "could not prove") || strings.Contains(p.Reason, "rejects the generated") {
			t.Fatal(p.Reason)
		}
	}

}

func importProposed(b schema.Block, prior, cfg value.Value) value.Value {
	if cfg.Kind() == value.Null {
		return cfg
	}
	out := map[string]value.Value{}
	for _, a := range b.Attributes {
		v := cfg.Get(a.Name)
		if v.Kind() == value.Null && a.Computed {
			v = prior.Get(a.Name)
		}
		out[a.Name] = v
	}
	for _, nb := range b.Blocks {
		v, p := cfg.Get(nb.Name), prior.Get(nb.Name)
		if nb.IsRecord() {
			out[nb.Name] = importProposed(nb.Block, p, v)
		} else if v.Kind() == value.List {
			var elems []value.Value
			for i := range v.Len() {
				pv := value.NullVal()
				if p.Kind() == value.List && i < p.Len() {
					pv = p.Index(i)
				}
				elems = append(elems, importProposed(nb.Block, pv, v.Index(i)))
			}
			out[nb.Name] = value.ListVal(elems)
		} else {
			out[nb.Name] = v
		}
	}
	return value.ObjectVal(out)
}

func testField(v value.Value, k string, n value.Value) value.Value {
	fields := map[string]value.Value{}
	for _, key := range v.Keys() {
		fields[key] = v.Get(key)
	}
	fields[k] = n
	return value.ObjectVal(fields)
}

func importZero(ty types.Type) value.Value {
	switch ty.Kind() {
	case types.String, types.Ref:
		return value.StringVal("")
	case types.Bool:
		return value.BoolVal(false)
	case types.Int:
		return value.IntVal(0)
	case types.Float:
		return value.FloatVal(0)
	case types.List:
		return value.ListVal(nil)
	case types.Map:
		return value.ObjectVal(nil)
	}
	return value.NullVal()
}

type offlineBulkReader struct {
	client           *provider.Client
	objects          map[string]importer.Found
	wire             map[types.Name]string
	plans, validates atomic.Int64
}

func (r *offlineBulkReader) Read(_ context.Context, _ types.Name, _ addr.Addr, id string) (importer.Found, diag.List, error) {
	return r.objects[id], nil, nil
}
func (r *offlineBulkReader) Validator(types.Name, addr.Addr) importer.Validator {
	return func(ctx context.Context, wire string, c value.Value) ([]provider.Diag, error) {
		r.validates.Add(1)
		return r.client.Validate(ctx, wire, c)
	}
}
func (r *offlineBulkReader) Planner(typ types.Name, _ addr.Addr, prior importer.Found) importer.Planner {
	var b schema.Block
	for _, o := range r.objects {
		if o.Schema.Name == typ {
			b = o.Schema.Block
			break
		}
	}
	return func(ctx context.Context, c value.Value) (importer.Plan, error) {
		r.plans.Add(1)
		p, err := r.client.Plan(ctx, provider.PlanRequest{Type: r.wire[typ], Prior: prior.State, PriorIdentity: prior.Identity, Config: c, Proposed: importProposed(b, prior.State, c)})
		return importer.Plan{Value: p.Planned, RequiresReplace: p.RequiresReplace, Private: p.Private}, err
	}
}
