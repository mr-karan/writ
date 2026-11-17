package provider_test

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/mr-karan/writ/internal/addr"
	"github.com/mr-karan/writ/internal/diag"
	"github.com/mr-karan/writ/internal/importer"
	"github.com/mr-karan/writ/internal/provider"
	"github.com/mr-karan/writ/internal/schema"
	"github.com/mr-karan/writ/internal/types"
	"github.com/mr-karan/writ/internal/value"
)

// oneObject reads one object for Bulk, and plans and validates it with
// the given stand-ins.
type oneObject struct {
	found    importer.Found
	plan     importer.Planner
	validate importer.Validator
}

func (o oneObject) Read(context.Context, types.Name, addr.Addr, string) (importer.Found, diag.List, error) {
	return o.found, nil, nil
}

func (o oneObject) Validator(types.Name, addr.Addr) importer.Validator { return o.validate }

func (o oneObject) Planner(types.Name, addr.Addr, importer.Found) importer.Planner {
	return o.plan
}

var aligned = regexp.MustCompile(`(?m)^(\s*[^\s=]+)\s+= `)

// importOne imports the object st of type r alone through Bulk. It returns
// the OpenTofu code, each attribute written `name = value` with one space,
// and the reason of each problem.
func importOne(t *testing.T, r *schema.Resource, st value.Value, plan importer.Planner, validate importer.Validator) (string, []string) {
	t.Helper()
	res, err := importer.Bulk(context.Background(), importer.BulkOptions{
		Selections: []importer.Selection{{Type: r.Name, IDs: []string{"example"}}},
		Reader:     oneObject{found: importer.Found{Schema: r, Source: schema.Source{Namespace: "hashicorp", Type: "aws"}, State: st}, plan: plan, validate: validate},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed() {
		t.Fatalf("bulk refused the object: %v", res.Problems)
	}
	var notes []string
	for _, p := range res.Problems {
		notes = append(notes, p.Reason)
	}
	return aligned.ReplaceAllString(string(res.Code), "$1 = "), notes
}

// sets reports whether code sets the attribute or block name.
func sets(code, name string) bool {
	return regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(name) + `( =| \{)`).MatchString(code)
}

func TestImportDisabledAndDeprecatedBlocks(t *testing.T) {
	cases := []struct {
		name, wire, state, omit, keep string
		notes                         int
	}{
		{"listener off", "aws_lb_listener", `{"mutual_authentication":[{"mode":"off","ignore_client_certificate_expiry":false,"trust_store_arn":""}]}`, "ignore_client_certificate_expiry", "mode =", 1},
		{"listener on", "aws_lb_listener", `{"mutual_authentication":[{"mode":"verify","ignore_client_certificate_expiry":true,"trust_store_arn":"trust"}]}`, "", "trust_store_arn", 0},
		{"stickiness off", "aws_lb_listener_rule", `{"action":[{"type":"forward","forward":[{"stickiness":[{"enabled":false,"duration":0}]}]}]}`, "", "duration = 0", 1},
		{"stickiness on", "aws_lb_listener_rule", `{"action":[{"type":"forward","forward":[{"stickiness":[{"enabled":true,"duration":60}]}]}]}`, "", "duration = 60", 0},
		{"bucket", "aws_s3_bucket", `{"bucket":"example","grant":[{"type":"CanonicalUser","permissions":["READ"]}],"lifecycle_rule":[{"enabled":true}],"versioning":[{"enabled":true}],"server_side_encryption_configuration":[{"rule":[{"apply_server_side_encryption_by_default":[{"sse_algorithm":"AES256"}]}]}]}`, "lifecycle_rule", "bucket", 4},
		{"role", "aws_iam_role", `{"name":"app","inline_policy":[{"name":"read","policy":"{}"}]}`, "inline_policy", "name", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, st := provider.ImportFixture(t, tc.wire, tc.state)
			code, notes := importOne(t, r, st, nil, validateImportStandIn)
			if tc.omit != "" && sets(code, tc.omit) {
				t.Errorf("kept %s:\n%s", tc.omit, code)
			}
			if tc.keep != "" && !strings.Contains(code, tc.keep) {
				t.Errorf("lost %s:\n%s", tc.keep, code)
			}
			if len(notes) != tc.notes {
				t.Errorf("notes=%v, want %d", notes, tc.notes)
			}
			if tc.name == "bucket" {
				for _, f := range []string{"grant", "versioning", "server_side_encryption_configuration"} {
					if sets(code, f) {
						t.Errorf("kept %s", f)
					}
				}
			}
			if tc.name == "listener on" {
				for _, f := range []string{"mode =", "ignore_client_certificate_expiry = true"} {
					if !strings.Contains(code, f) {
						t.Errorf("lost %s", f)
					}
				}
			}
		})
	}
}

// hashicorp/aws 6.70.0 reads a listener's disabled stickiness with no
// DurationSeconds as duration 0 and rejects it, and its legacy SDK may
// send that error at the enabled sibling's path, differing from call to
// call. Bulk must still write the same code every run.
func TestImportListenerStickinessSameEveryRun(t *testing.T) {
	r, st := provider.ImportFixture(t, "aws_lb_listener", `{"port":80,"protocol":"HTTP","default_action":[{"type":"forward","order":1,"forward":[{"target_group":[{"arn":"arn:aws:elasticloadbalancing:us-east-1:123456789012:targetgroup/app/1234567890123456","weight":1}],"stickiness":[{"enabled":false,"duration":0}]}]}]}`)
	calls := 0
	validate := func(_ context.Context, _ string, config value.Value) ([]provider.Diag, error) {
		calls++
		forward := config.Get("default_action").Index(0).Get("forward")
		stick := forward.Get("stickiness")
		if stick.Kind() != value.Object || stick.Get("duration").Kind() != value.Int || stick.Get("duration").AsInt() != 0 {
			return nil, nil
		}
		at := "duration"
		if calls%3 != 0 {
			at = "enabled"
		}
		return []provider.Diag{{Error: true, Path: value.Path{}.Attr("default_action").Index(0).Attr("forward").Attr("stickiness").Attr(at), Summary: "expected default_action.0.forward.0.stickiness.0.duration to be in the range (1 - 604800), got 0"}}, nil
	}
	plan := func(context.Context, value.Value) (importer.Plan, error) { return importer.Plan{Value: st}, nil }
	var first string
	for i := range 50 {
		calls = i
		code, notes := importOne(t, r, st, plan, validate)
		out := code + "\n" + strings.Join(notes, "\n")
		if i == 0 {
			first = out
			if strings.Contains(code, "stickiness") || strings.Contains(out, "rejects the generated") {
				t.Fatalf("kept the disabled stickiness block:\n%s", out)
			}
			continue
		}
		if out != first {
			t.Fatalf("run %d differs from run 0:\n%s\nrun 0:\n%s", i, out, first)
		}
	}
}

func validateImportStandIn(_ context.Context, _ string, config value.Value) ([]provider.Diag, error) {
	var ds []provider.Diag
	var walk func(value.Value, value.Path)
	walk = func(v value.Value, p value.Path) {
		switch v.Kind() {
		case value.Object:
			if mode := v.Get("mode"); mode.Kind() == value.String && mode.AsString() == "off" && v.Get("ignore_client_certificate_expiry").Kind() != value.Null {
				ds = append(ds, provider.Diag{Error: true, Path: p, Summary: "expiry applies only in verify mode"})
			}
			if duration := v.Get("duration"); duration.Kind() == value.Int && duration.AsInt() == 0 {
				ds = append(ds, provider.Diag{Error: true, Path: p.Attr("duration"), Summary: "duration must be between 1 and 604800"})
			}
			for _, k := range v.Keys() {
				walk(v.Get(k), p.Attr(k))
			}
		case value.List:
			for i := range v.Len() {
				walk(v.Index(i), p.Index(int64(i)))
			}
		}
	}
	walk(config, nil)
	return ds, nil
}
