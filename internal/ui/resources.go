package ui

import (
	"cmp"
	"slices"
	"strconv"
	"strings"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/server"
)

// resourceCategory is a category of a stack's resources. Key names its
// color (cat-KEY) and its icon in the sprite (i-KEY).
type resourceCategory struct{ Key, Name string }

// resourceCategories are the categories in the order a tie of counts
// takes. Other, which no rule names, is always last.
var resourceCategories = []resourceCategory{
	{"net", "Networking"}, {"cmp", "Compute"}, {"sto", "Storage"}, {"db", "Database"},
	{"iam", "IAM"}, {"dns", "DNS"}, {"cert", "Certificates"}, {"nomad", "Nomad"},
	{"mon", "Monitoring"}, {"msg", "Messaging"}, {"cfg", "Config"}, {"other", "Other"},
}

// resourceRule sorts a resource type into a category and a sub-group by
// its longest matching prefix. Group is "" in a category of one group.
type resourceRule struct{ Prefix, Category, Group string }

// resourceRules are the prefixes of the types, in the order of the
// sub-groups of each category. The longest prefix that a type starts
// with wins: aws_route53_zone is DNS though aws_route is VPC.
var resourceRules = rules(
	"net", "Load balancers", "aws_lb aws_alb aws_elb",
	"net", "Security groups", "aws_security_group aws_vpc_security_group_",
	"net", "VPC", "aws_vpc aws_subnet aws_route aws_internet_gateway aws_nat_gateway aws_eip aws_network_ aws_ec2_transit_gateway",
	"net", "CDN", "aws_cloudfront_ cloudflare_",
	"cmp", "Instances", "aws_instance aws_launch_template aws_autoscaling_ aws_ami aws_key_pair aws_placement_group",
	"cmp", "Functions", "aws_lambda_",
	"cmp", "Containers", "aws_ecs_ aws_ecr_ aws_eks_",
	"sto", "Buckets", "aws_s3_",
	"sto", "Volumes", "aws_ebs_ aws_volume_attachment aws_efs_",
	"db", "", "aws_db_ aws_rds_ aws_dynamodb_ aws_elasticache_ postgresql_ mysql_",
	"iam", "Roles", "aws_iam_role aws_iam_instance_profile",
	"iam", "Policies", "aws_iam_policy aws_iam_role_policy aws_iam_group_policy aws_iam_user_policy",
	"iam", "Users and groups", "aws_iam_user aws_iam_group aws_iam_access_key",
	"iam", "Other", "aws_iam_ aws_kms_",
	"dns", "", "aws_route53_ dns_",
	"cert", "", "aws_acm_ tls_ acme_",
	"nomad", "", "nomad_ consul_ vault_",
	"mon", "", "aws_cloudwatch_ aws_sns_topic_subscription grafana_",
	"msg", "", "aws_sqs_ aws_sns_ aws_kinesis_ aws_msk_",
	"cfg", "", "aws_ssm_ aws_secretsmanager_ random_ local_ null_ time_ terraform_data",
)

// rules reads triples of category, group, and prefixes split by spaces.
func rules(spec ...string) []resourceRule {
	var out []resourceRule
	for i := 0; i+2 < len(spec); i += 3 {
		for _, p := range strings.Fields(spec[i+2]) {
			out = append(out, resourceRule{Prefix: p, Category: spec[i], Group: spec[i+1]})
		}
	}
	return out
}

// ruleOf is the rule of the longest prefix typ starts with, and false
// when none does.
func ruleOf(typ string) (resourceRule, bool) {
	var best resourceRule
	for _, r := range resourceRules {
		if strings.HasPrefix(typ, r.Prefix) && len(r.Prefix) > len(best.Prefix) {
			best = r
		}
	}
	return best, best.Prefix != ""
}

// resourceRow is one resource of the index as the resources view shows it.
type resourceRow struct {
	server.IndexResource
	// Module is "root", or the module path, such as "module.a.module.b".
	Module string
	// Label is the type without its provider prefix, "_" as " ".
	Label string
	// Drift is "", "outside", or "code": what the last drift check found
	// of the resource. outside wins over code. DriftHref links the drift
	// view when Drift is set.
	Drift, DriftHref string
}

type resourceSub struct {
	// Name is "" when the group has one sub-group.
	Name string
	Rows []resourceRow
}

// resourceGroup is a card of the view: a category, a module, or for the
// flat view the one card of every row, whose Name is "".
type resourceGroup struct {
	resourceCategory
	// ID is the card's anchor, which the strip links. Module reports a
	// group of one module, whose Name is the module path.
	ID     string
	Module bool
	Count  figure
	Subs   []resourceSub
	Open   bool
}

// resourcesView is the resources view of a stack: the strip of
// categories, and the groups of the rows that match the filter.
type resourcesView struct {
	// Total counts the resources of the index, Shown those that match the
	// filter, and Marked those the last drift check names.
	Total, Shown, Marked figure
	Query                string
	// GroupBy is service, module, or flat.
	GroupBy string
	// Strip is every category with a row shown, by count, with no Subs.
	Strip  []resourceGroup
	Groups []resourceGroup
}

// openUpTo is how many resources a stack lists with its groups open.
const openUpTo = 60

// groupResources sorts the resources rs of a stack into the groups of by,
// keeping those whose address, type, or ID holds query, case folded.
// changes are the changes of the stack's last drift check.
func groupResources(rs []server.IndexResource, changes []control.DriftChange, query, by string) resourcesView {
	drift := map[string]string{}
	for _, c := range changes {
		if drift[c.Address] != control.DriftOutside {
			drift[c.Address] = c.Kind
		}
	}
	query = strings.TrimSpace(query)
	v := resourcesView{Total: exact(len(rs)), Query: query, GroupBy: by}
	q := strings.ToLower(query)
	var rows []resourceRow
	marked := 0
	for _, r := range rs {
		row := resourceRow{IndexResource: r, Module: moduleOf(r.Address), Label: labelOf(r.Type), Drift: drift[r.Address]}
		if row.Drift != "" {
			marked++
		}
		if q == "" || strings.Contains(strings.ToLower(r.Address), q) || strings.Contains(strings.ToLower(r.Type), q) || strings.Contains(strings.ToLower(r.ID), q) {
			rows = append(rows, row)
		}
	}
	slices.SortStableFunc(rows, func(a, b resourceRow) int { return strings.Compare(a.Address, b.Address) })
	v.Shown, v.Marked = exact(len(rows)), exact(marked)
	open := len(rs) <= openUpTo || query != ""

	byCat := map[string][]resourceRow{}
	for _, r := range rows {
		rule, _ := ruleOf(r.Type)
		byCat[cmp.Or(rule.Category, "other")] = append(byCat[cmp.Or(rule.Category, "other")], r)
	}
	for _, c := range resourceCategories {
		if n := len(byCat[c.Key]); n > 0 {
			v.Strip = append(v.Strip, resourceGroup{resourceCategory: c, ID: "cat-" + c.Key, Count: exact(n)})
		}
	}
	slices.SortStableFunc(v.Strip, func(a, b resourceGroup) int {
		if (a.Key == "other") != (b.Key == "other") {
			if a.Key == "other" {
				return 1
			}
			return -1
		}
		return b.Count.N - a.Count.N
	})

	switch by {
	case "module":
		mods := map[string][]resourceRow{}
		var names []string
		for _, r := range rows {
			if mods[r.Module] == nil {
				names = append(names, r.Module)
			}
			mods[r.Module] = append(mods[r.Module], r)
		}
		slices.SortFunc(names, func(a, b string) int {
			if (a == "root") != (b == "root") {
				if a == "root" {
					return -1
				}
				return 1
			}
			return strings.Compare(a, b)
		})
		for i, m := range names {
			v.Groups = append(v.Groups, resourceGroup{resourceCategory: resourceCategory{Key: "other", Name: m}, ID: "mod-" + strconv.Itoa(i), Module: true, Count: exact(len(mods[m])), Subs: []resourceSub{{Rows: mods[m]}}, Open: open})
		}
	case "flat":
		if len(rows) > 0 {
			v.Groups = []resourceGroup{{Count: exact(len(rows)), Subs: []resourceSub{{Rows: rows}}, Open: true}}
		}
	default:
		for _, s := range v.Strip {
			g := s
			g.Open = open
			subs := map[string]int{}
			for _, rule := range resourceRules {
				if _, ok := subs[rule.Group]; !ok && rule.Category == g.Key {
					subs[rule.Group] = len(g.Subs)
					g.Subs = append(g.Subs, resourceSub{Name: rule.Group})
				}
			}
			if g.Key == "other" {
				g.Subs = []resourceSub{{}}
				subs[""] = 0
			}
			for _, r := range byCat[g.Key] {
				rule, _ := ruleOf(r.Type)
				g.Subs[subs[rule.Group]].Rows = append(g.Subs[subs[rule.Group]].Rows, r)
			}
			g.Subs = slices.DeleteFunc(g.Subs, func(s resourceSub) bool { return len(s.Rows) == 0 })
			if len(g.Subs) == 1 {
				g.Subs[0].Name = ""
			}
			v.Groups = append(v.Groups, g)
		}
	}
	return v
}

// moduleOf is the module of an address: the address up to its last
// module.NAME segment, an index such as ["a.b"] included, or "root".
func moduleOf(addr string) string {
	end := 0
	for rest := addr; strings.HasPrefix(rest, "module."); {
		i := len("module.")
		for i < len(rest) && rest[i] != '.' && rest[i] != '[' {
			i++
		}
		if i < len(rest) && rest[i] == '[' {
			i = indexEnd(rest, i)
		}
		if i >= len(rest) || rest[i] != '.' {
			break
		}
		end += i
		rest = rest[i+1:]
		end++
	}
	if end == 0 {
		return "root"
	}
	return addr[:end-1]
}

// indexEnd is the position after the index that opens at s[i], '[', or
// len(s) when it does not close. A key in quotes may hold ']' and '.'.
func indexEnd(s string, i int) int {
	quoted := false
	for i++; i < len(s); i++ {
		switch {
		case quoted && s[i] == '\\':
			i++
		case s[i] == '"':
			quoted = !quoted
		case !quoted && s[i] == ']':
			return i + 1
		}
	}
	return len(s)
}

// labelOf is a type's words without its provider: aws_lb_listener is
// "lb listener".
func labelOf(typ string) string {
	if _, rest, ok := strings.Cut(typ, "_"); ok && rest != "" {
		typ = rest
	}
	return strings.ReplaceAll(typ, "_", " ")
}

// linkDrift sets the link to the drift view on each marked row.
func (v *resourcesView) linkDrift(href string) {
	for _, g := range v.Groups {
		for _, s := range g.Subs {
			for i := range s.Rows {
				if s.Rows[i].Drift != "" {
					s.Rows[i].DriftHref = href
				}
			}
		}
	}
}
