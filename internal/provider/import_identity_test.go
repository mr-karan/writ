package provider_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mr-karan/writ/internal/addr"
	"github.com/mr-karan/writ/internal/importer"
	"github.com/mr-karan/writ/internal/provider"
	"github.com/mr-karan/writ/internal/schema"
	"github.com/mr-karan/writ/internal/types"
	"github.com/mr-karan/writ/internal/value"
)

// TestOfflineAWSImportIdentity imports a security group and a target
// group with the resource identity an import's read gives each, whose
// plans also return one. hashicorp/aws 6.70.0 plans the identity it gets
// as the prior one, so bulk proves their code plans no change. When the
// object changed after the read so that the code would replace it, bulk
// must warn.
func TestOfflineAWSImportIdentity(t *testing.T) {
	c, _ := provider.OfflineAWS(t)
	ctx := context.Background()
	const tg = "arn:aws:elasticloadbalancing:us-east-1:123456789012:targetgroup/app/0123456789abcdef"
	objects := []struct{ wire, id, identity, state string }{
		{"aws_security_group", "sg-0123456789abcdef0", `{"account_id":"123456789012","id":"sg-0123456789abcdef0","region":"us-east-1"}`, `{"id":"sg-0123456789abcdef0","arn":"arn:aws:ec2:us-east-1:123456789012:security-group/sg-0123456789abcdef0","name":"web","description":"web","vpc_id":"vpc-0123456789abcdef0","owner_id":"123456789012","revoke_rules_on_delete":false,"ingress":[{"from_port":80,"to_port":80,"protocol":"tcp","cidr_blocks":["0.0.0.0/0"],"ipv6_cidr_blocks":[],"prefix_list_ids":[],"security_groups":[],"self":false,"description":""}],"egress":[{"from_port":0,"to_port":0,"protocol":"-1","cidr_blocks":["0.0.0.0/0"],"ipv6_cidr_blocks":[],"prefix_list_ids":[],"security_groups":[],"self":false,"description":""}],"tags":{"team":"web"},"tags_all":{"team":"web"}}`},
		{"aws_lb_target_group", tg, `{"arn":"` + tg + `"}`, `{"id":"` + tg + `","arn":"` + tg + `","name":"app","port":80,"protocol":"HTTP","vpc_id":"vpc-0123456789abcdef0","target_type":"instance","tags":{"team":"web"},"tags_all":{"team":"web"}}`},
	}
	for _, o := range objects {
		t.Run(o.wire, func(t *testing.T) {
			r, seed := provider.ImportFixture(t, o.wire, o.state)
			for _, a := range r.Block.Attributes {
				if a.Computed && seed.Get(a.Name).Kind() == value.Null {
					seed = testField(seed, a.Name, importZero(a.Type))
				}
			}
			// The state an import reads is what the provider plans for the
			// object's own settings.
			cfg := seed
			for _, a := range r.Block.Attributes {
				if a.Computed && !a.Optional {
					cfg = testField(cfg, a.Name, value.NullVal())
				}
			}
			cfg = testField(cfg, "id", value.NullVal())
			identity := provider.IdentityOf(t, o.identity)
			first, err := c.Plan(ctx, provider.PlanRequest{Type: o.wire, Prior: seed, PriorIdentity: identity, Config: cfg, Proposed: importProposed(r.Block, seed, cfg)})
			if err != nil {
				t.Fatal(err)
			}
			if !provider.IdentityEqual(first.Identity, identity) {
				t.Fatalf("planned identity %v, want the prior %v", first.Identity, identity)
			}
			read := importer.Found{Schema: r, Source: schema.Source{Namespace: "hashicorp", Type: "aws"}, State: first.Planned, Identity: identity}
			for _, tc := range []struct {
				name  string
				prior importer.Found
				warns bool
			}{
				{name: "as read", prior: read},
				{name: "renamed", prior: importer.Found{State: testField(read.State, "name", value.StringVal("other")), Identity: identity}, warns: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					reader := &identityReader{offlineBulkReader: offlineBulkReader{client: c, objects: map[string]importer.Found{o.id: read}, wire: map[types.Name]string{r.Name: o.wire}}, prior: tc.prior}
					res, err := importer.Bulk(ctx, importer.BulkOptions{Selections: []importer.Selection{{Type: r.Name, IDs: []string{o.id}}}, Reader: reader})
					if err != nil {
						t.Fatal(err)
					}
					var warnings []string
					for _, p := range res.Problems {
						if strings.Contains(p.Reason, "could not prove") || strings.Contains(p.Reason, "hand fix") {
							warnings = append(warnings, p.Reason)
						}
					}
					if tc.warns != (len(warnings) > 0) {
						t.Fatalf("warnings = %q, want a warning: %v", warnings, tc.warns)
					}
				})
			}
		})
	}
}

// identityReader plans every config against prior, as if the object had
// changed after the read.
type identityReader struct {
	offlineBulkReader
	prior importer.Found
}

func (r *identityReader) Planner(typ types.Name, via addr.Addr, _ importer.Found) importer.Planner {
	return r.offlineBulkReader.Planner(typ, via, r.prior)
}
