package provider_test

import (
	"context"
	"fmt"
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

// TestBulkDoubtsAMovedIdentity imports one object from a provider that
// checks no identity. A plan that keeps the state but moves the identity
// must not pass as a no-change plan, and a provider with no identity must
// not make bulk doubt one.
func TestBulkDoubtsAMovedIdentity(t *testing.T) {
	ctx := context.Background()
	for _, version := range []int{5, 6} {
		for _, tc := range []struct {
			name        string
			none, moves bool
			warns       bool
		}{
			{name: "kept"},
			{name: "moved", moves: true, warns: true},
			{name: "no identity", none: true},
		} {
			t.Run(fmt.Sprintf("v%d/%s", version, tc.name), func(t *testing.T) {
				c, p := provider.ServeIdentityStandIn(t, version, tc.none, tc.moves)
				r, err := p.ResourceByWire("fake_thing")
				if err != nil {
					t.Fatal(err)
				}
				res, err := importer.Bulk(ctx, importer.BulkOptions{Selections: []importer.Selection{{Type: r.Name, IDs: []string{"thing-1"}}}, Reader: clientReader{c: c, r: r}})
				if err != nil {
					t.Fatal(err)
				}
				if len(res.Objects) != 1 {
					t.Fatalf("objects = %d, problems = %+v; want one object", len(res.Objects), res.Problems)
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
	}
}

// clientReader reads and plans through a provider client as TofuStack
// does, with one resource type.
type clientReader struct {
	c *provider.Client
	r *schema.Resource
}

func (cr clientReader) Read(ctx context.Context, _ types.Name, _ addr.Addr, id string) (importer.Found, diag.List, error) {
	obj, err := cr.c.Import(ctx, provider.ImportRequest{Type: cr.r.Wire, ID: id})
	if err != nil {
		return importer.Found{}, nil, err
	}
	return importer.Found{Schema: cr.r, Source: schema.Source{Namespace: "example", Type: "fake"}, State: obj.State, Private: obj.Private, Identity: obj.Identity}, nil, nil
}

func (cr clientReader) Validator(types.Name, addr.Addr) importer.Validator {
	return cr.c.Validate
}

func (cr clientReader) Planner(_ types.Name, _ addr.Addr, found importer.Found) importer.Planner {
	return func(ctx context.Context, cfg value.Value) (importer.Plan, error) {
		resp, err := cr.c.Plan(ctx, provider.PlanRequest{Type: cr.r.Wire, Prior: found.State, Proposed: provider.ProposeNew(cr.r.Block, found.State, cfg), Config: cfg, PriorPrivate: found.Private, PriorIdentity: found.Identity})
		return importer.Plan{Value: resp.Planned, RequiresReplace: resp.RequiresReplace, Private: resp.Private}, err
	}
}
