package ui

import (
	"fmt"
	"slices"
	"testing"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/server"
)

// TestResourceRules holds the rule table to its longest prefix: a type
// that starts with two prefixes takes the longer one's group.
func TestResourceRules(t *testing.T) {
	for _, c := range []struct{ typ, cat, group string }{
		{"aws_route53_zone", "dns", ""},
		{"aws_route_table", "net", "VPC"},
		{"aws_iam_role_policy_attachment", "iam", "Policies"},
		{"aws_iam_role", "iam", "Roles"},
		{"aws_vpc_security_group_ingress_rule", "net", "Security groups"},
		{"aws_sns_topic_subscription", "mon", ""},
		{"aws_sns_topic", "msg", ""},
		{"nomad_job", "nomad", ""},
	} {
		r, ok := ruleOf(c.typ)
		if !ok || r.Category != c.cat || r.Group != c.group {
			t.Errorf("%s is %s/%q (%v), want %s/%q", c.typ, r.Category, r.Group, ok, c.cat, c.group)
		}
	}
	for _, typ := range []string{"kubernetes_namespace", "aws", "awsx_thing", ""} {
		if r, ok := ruleOf(typ); ok {
			t.Errorf("%s matched %+v, want Other", typ, r)
		}
	}
	v := groupResources([]server.IndexResource{{Address: "kubernetes_namespace.a", Type: "kubernetes_namespace"}}, nil, "", "service")
	if len(v.Groups) != 1 || v.Groups[0].Key != "other" || v.Groups[0].Name != "Other" {
		t.Errorf("an unmatched type is in %+v, want Other", v.Groups)
	}
}

func TestResourceModuleAndLabel(t *testing.T) {
	for _, c := range []struct{ addr, module string }{
		{"aws_lb.app", "root"},
		{"aws_lb.app[0]", "root"},
		{"module.vpc.aws_subnet.private[\"a\"]", "module.vpc"},
		{"module.a.module.b.aws_s3_bucket.logs", "module.a.module.b"},
		{"module.cluster[\"eu.west\"].aws_instance.x", "module.cluster[\"eu.west\"]"},
		{"module.c[\"a]b\"].module.d[1].nomad_job.web", "module.c[\"a]b\"].module.d[1]"},
		{"data.aws_ami.base", "root"},
	} {
		if got := moduleOf(c.addr); got != c.module {
			t.Errorf("moduleOf(%s) = %s, want %s", c.addr, got, c.module)
		}
	}
	for typ, want := range map[string]string{"aws_lb_listener": "lb listener", "random_pet": "pet", "terraform_data": "data", "thing": "thing"} {
		if got := labelOf(typ); got != want {
			t.Errorf("labelOf(%s) = %q, want %q", typ, got, want)
		}
	}
}

// manyResources is a state of 64 resources across five categories, the
// Other among them, in the root and in two modules.
func manyResources() []server.IndexResource {
	var rs []server.IndexResource
	add := func(addr, typ, id string) {
		rs = append(rs, server.IndexResource{Address: addr, Type: typ, Provider: "registry.opentofu.org/hashicorp/aws", ID: id})
	}
	for i := range 16 {
		add(fmt.Sprintf("module.vpc.aws_subnet.private[%d]", i), "aws_subnet", fmt.Sprintf("subnet-%04x", i))
		add(fmt.Sprintf("aws_iam_role.svc[%d]", i), "aws_iam_role", fmt.Sprintf("svc-%d", i))
		add(fmt.Sprintf("module.jobs.nomad_job.app[%d]", i), "nomad_job", fmt.Sprintf("app-%d", i))
	}
	for i := range 15 {
		add(fmt.Sprintf("aws_s3_bucket.logs[%d]", i), "aws_s3_bucket", fmt.Sprintf("acme-logs-%d", i))
	}
	add("kubernetes_namespace.ops", "kubernetes_namespace", "ops")
	return rs
}

// rowsIn are the addresses of every row of v's groups.
func rowsIn(v resourcesView) []string {
	var out []string
	for _, g := range v.Groups {
		for _, s := range g.Subs {
			for _, r := range s.Rows {
				out = append(out, r.Address)
			}
		}
	}
	return out
}

// TestResourceFilter holds the filter to the address, the type, and the
// ID: a word only the provider holds matches nothing.
func TestResourceFilter(t *testing.T) {
	rs := []server.IndexResource{
		{Address: "aws_lb.app", Type: "aws_lb", Provider: "registry.opentofu.org/hashicorp/aws", ID: "app-alb", Region: "eu-west-1"},
		{Address: "aws_s3_bucket.logs", Type: "aws_s3_bucket", Provider: "registry.opentofu.org/hashicorp/aws", ID: "acme-logs", ARN: "arn:aws:s3:::acme-logs"},
		{Address: "nomad_job.web", Type: "nomad_job", Provider: "registry.opentofu.org/hashicorp/nomad", ID: "web"},
	}
	for q, want := range map[string][]string{
		"APP":         {"aws_lb.app"},
		"s3_bucket":   {"aws_s3_bucket.logs"},
		"WEB":         {"nomad_job.web"},
		"acme-logs":   {"aws_s3_bucket.logs"},
		"hashicorp":   nil,
		"eu-west-1":   nil,
		"arn:aws:s3":  nil,
		"  nomad  ":   {"nomad_job.web"},
		"no-such-row": nil,
	} {
		v := groupResources(rs, nil, q, "flat")
		if got := rowsIn(v); !slices.Equal(got, want) {
			t.Errorf("filter %q lists %q, want %q", q, got, want)
		}
		if v.Shown.N != len(want) || v.Total.N != len(rs) || !allOpen(v) {
			t.Errorf("filter %q: %d of %d shown, groups open %v", q, v.Shown.N, v.Total.N, allOpen(v))
		}
	}
}

// allOpen reports whether every group of v is open.
func allOpen(v resourcesView) bool {
	for _, g := range v.Groups {
		if !g.Open {
			return false
		}
	}
	return true
}

// TestResourceGroupings holds each grouping to every row once, and the
// strip's counts to the rows shown.
func TestResourceGroupings(t *testing.T) {
	rs := manyResources()
	var all []string
	for _, r := range rs {
		all = append(all, r.Address)
	}
	slices.Sort(all)
	for _, by := range []string{"service", "module", "flat"} {
		v := groupResources(rs, nil, "", by)
		got := rowsIn(v)
		slices.Sort(got)
		if !slices.Equal(got, all) {
			t.Errorf("group=%s lists %d rows, want each of %d once", by, len(got), len(all))
		}
		sum := 0
		for _, s := range v.Strip {
			sum += s.Count.N
			if len(s.Subs) != 0 {
				t.Errorf("group=%s: the strip's %s has rows", by, s.Key)
			}
		}
		if sum != v.Total.N || v.Total.N != len(rs) {
			t.Errorf("group=%s: the strip counts %d of %d", by, sum, v.Total.N)
		}
		for _, g := range v.Groups {
			n := 0
			for _, s := range g.Subs {
				n += len(s.Rows)
			}
			if g.Count.N != n {
				t.Errorf("group=%s: %s counts %d and holds %d", by, g.Name, g.Count.N, n)
			}
			if g.Open != (by == "flat") {
				t.Errorf("group=%s: %s of a stack of %d resources is open %v", by, g.Name, len(rs), g.Open)
			}
		}
	}
	if v := groupResources(rs, nil, "", "flat"); len(v.Groups) != 1 || v.Groups[0].Name != "" {
		t.Errorf("group=flat makes %d groups", len(v.Groups))
	}
	mods := groupResources(rs, nil, "", "module")
	var names []string
	for _, g := range mods.Groups {
		names = append(names, g.Name)
	}
	if want := []string{"root", "module.jobs", "module.vpc"}; !slices.Equal(names, want) {
		t.Errorf("group=module makes %q, want %q", names, want)
	}
	svc := groupResources(rs, nil, "", "service")
	var keys []string
	for _, g := range svc.Groups {
		keys = append(keys, g.Key)
	}
	if want := []string{"net", "iam", "nomad", "sto", "other"}; !slices.Equal(keys, want) {
		t.Errorf("group=service makes %q, want %q, by count and Other last", keys, want)
	}
	if small := groupResources(rs[:openUpTo], nil, "", "service"); !allOpen(small) {
		t.Errorf("a stack of %d resources shows its groups folded", openUpTo)
	}
	if filtered := groupResources(rs, nil, "subnet", "service"); !allOpen(filtered) || filtered.Shown.N != 16 || len(filtered.Strip) != 1 || filtered.Strip[0].Count.N != 16 {
		t.Errorf("a filter of 16 rows: %+v", filtered.Strip)
	}
}

// TestResourceSubGroups names a sub-group only in a category of more
// than one, in rule order.
func TestResourceSubGroups(t *testing.T) {
	rs := []server.IndexResource{
		{Address: "aws_subnet.a", Type: "aws_subnet"},
		{Address: "aws_lb.app", Type: "aws_lb"},
		{Address: "aws_security_group.web", Type: "aws_security_group"},
		{Address: "aws_iam_role.r", Type: "aws_iam_role"},
	}
	v := groupResources(rs, nil, "", "service")
	var subs []string
	for _, s := range v.Groups[0].Subs {
		subs = append(subs, s.Name)
	}
	if v.Groups[0].Key != "net" || !slices.Equal(subs, []string{"Load balancers", "Security groups", "VPC"}) {
		t.Errorf("Networking's sub-groups: %q", subs)
	}
	if iam := v.Groups[1]; iam.Key != "iam" || len(iam.Subs) != 1 || iam.Subs[0].Name != "" {
		t.Errorf("IAM of one sub-group: %+v", iam.Subs)
	}
}

// TestResourceDriftMarks marks a row exactly when the last drift check
// names its address, outside over code, and counts the marks.
func TestResourceDriftMarks(t *testing.T) {
	rs := manyResources()
	changes := []control.DriftChange{
		{Address: "aws_s3_bucket.logs[3]", Kind: control.DriftCode},
		{Address: "aws_s3_bucket.logs[3]", Kind: control.DriftOutside},
		{Address: "module.vpc.aws_subnet.private[1]", Kind: control.DriftOutside},
		{Address: "module.vpc.aws_subnet.private[2]", Kind: control.DriftCode},
		{Address: "module.vpc.aws_subnet.private[2]x", Kind: control.DriftCode},
		{Address: "aws_s3_bucket.gone", Kind: control.DriftCode},
	}
	want := map[string]string{"aws_s3_bucket.logs[3]": "outside", "module.vpc.aws_subnet.private[1]": "outside", "module.vpc.aws_subnet.private[2]": "code"}
	for _, by := range []string{"service", "module", "flat"} {
		v := groupResources(rs, &server.DriftCheckInfo{Changes: changes}, "", by)
		v.linkDrift("/drift")
		if v.Marked.N != len(want) {
			t.Errorf("group=%s marks %d, want %d", by, v.Marked.N, len(want))
		}
		for _, g := range v.Groups {
			for _, s := range g.Subs {
				for _, r := range s.Rows {
					if r.Drift != want[r.Address] || (r.Drift != "") != (r.DriftHref == "/drift") {
						t.Errorf("group=%s: %s is marked %q (%s), want %q", by, r.Address, r.Drift, r.DriftHref, want[r.Address])
					}
				}
			}
		}
	}
	if v := groupResources(rs, &server.DriftCheckInfo{Changes: changes}, "acme-logs", "flat"); v.Marked.N != len(want) {
		t.Errorf("a filter changes the marks to %d", v.Marked.N)
	}
	if v := groupResources(rs, nil, "", "service"); !v.Marked.None() {
		t.Error("a stack with no drift check has marks")
	}
}
