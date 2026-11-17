package provider_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/mr-karan/writ/internal/importer"
	"github.com/mr-karan/writ/internal/provider"
	"github.com/mr-karan/writ/internal/schema"
	"github.com/mr-karan/writ/internal/types"
	"github.com/mr-karan/writ/internal/value"
)

// A listener read with stickiness disabled and no DurationSeconds holds
// duration 0, which hashicorp/aws 6.70.0 rejects, and its legacy SDK sends
// that error at a path whose last step varies by call. Bulk must leave the
// disabled block out and write the same code every run.
func TestOfflineAWSListenerStickiness(t *testing.T) {
	c, _ := provider.OfflineAWS(t)
	ctx := context.Background()
	const wire = "aws_lb_listener"
	r, seed := provider.ImportFixture(t, wire, `{"arn":"arn:aws:elasticloadbalancing:us-east-1:123456789012:listener/app/app/1234567890123456/1234567890123456","load_balancer_arn":"arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/app/app/1234567890123456","port":80,"protocol":"HTTP","default_action":[{"type":"forward","order":1,"forward":[{"target_group":[{"arn":"arn:aws:elasticloadbalancing:us-east-1:123456789012:targetgroup/app/1234567890123456","weight":1}],"stickiness":[{"enabled":false,"duration":0}]}]}]}`)
	for _, a := range r.Block.Attributes {
		if a.Computed && seed.Get(a.Name).Kind() == value.Null {
			seed = testField(seed, a.Name, importZero(a.Type))
		}
	}
	seed = testField(seed, "id", seed.Get("arn"))
	// Fill defaults from a valid plan, then restore the imported stickiness.
	safe := seed
	action := safe.Get("default_action").Index(0)
	action = testField(action, "forward", testField(action.Get("forward"), "stickiness", value.NullVal()))
	safe = testField(safe, "default_action", value.ListVal([]value.Value{action}))
	for _, a := range r.Block.Attributes {
		if a.Computed && !a.Optional {
			safe = testField(safe, a.Name, value.NullVal())
		}
	}
	safe = testField(safe, "id", value.NullVal())
	safe = testField(safe, "tcp_idle_timeout_seconds", value.NullVal())
	initial, err := c.Plan(ctx, provider.PlanRequest{Type: wire, Prior: seed, Config: safe, Proposed: importProposed(r.Block, seed, safe)})
	if err != nil {
		t.Fatal(err)
	}
	prior := testField(initial.Planned, "default_action", seed.Get("default_action"))

	reader := &offlineBulkReader{client: c, objects: map[string]importer.Found{"listener": {Schema: r, Source: schema.Source{Namespace: "hashicorp", Type: "aws"}, State: prior}}, wire: map[types.Name]string{r.Name: wire}}
	var first string
	for i := range 50 {
		res, err := importer.Bulk(ctx, importer.BulkOptions{Selections: []importer.Selection{{Type: r.Name, IDs: []string{"listener"}}}, Reader: reader})
		if err != nil {
			t.Fatal(err)
		}
		out := fmt.Sprintf("%s\n%v", res.Code, res.Problems)
		if i == 0 {
			first = out
			if strings.Contains(string(res.Code), "stickiness") || strings.Contains(out, "rejects the generated") || !strings.Contains(out, "leaves out default_action[0].forward.stickiness, because the provider proves its omission plans no change") {
				t.Fatalf("bulk kept the disabled stickiness block:\n%s", out)
			}
			continue
		}
		if out != first {
			t.Fatalf("run %d differs from run 0:\n%s\nrun 0:\n%s", i, out, first)
		}
	}
}
