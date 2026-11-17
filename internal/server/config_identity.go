package server

import (
	"fmt"
	"maps"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/mr-karan/writ/internal/authz"
	"github.com/mr-karan/writ/internal/oidc"
	"golang.org/x/net/idna"
)

func (d *decoder) issuers(top *hclsyntax.Body, bs []*hclsyntax.Block, cfg *Config) {
	for _, blk := range bs {
		name, key := blk.Labels[0], blk.LabelRanges[0]
		if !nameRE.MatchString(name) {
			d.errorf(key, "issuer name %q must be lowercase letters, digits, _ and -", name)
		}
		r := d.attrs(blk, "issuer "+name, "kind", "url", "identity", "ca_file", "client_id", "client_secret_file", "groups")
		is := Issuer{Name: name}
		if k, ok := r["kind"]; ok {
			raw := d.str(k.value, "kind")
			kind, ok := oidc.ParseKind(raw)
			if !ok {
				d.errorf(k.value, "issuer %s: issuer kind %q is not one of gitlab-ci, github-actions, people", name, raw)
			}
			is.Kind = kind
		} else {
			d.errorf(blk, "issuer %s needs kind", name)
		}
		if u, ok := r["url"]; ok {
			is.URL = d.str(u.value, "url")
			if pu, err := url.Parse(is.URL); err != nil || pu.Scheme != "https" || pu.Host == "" {
				d.errorf(u.value, "issuer %s: url must be https://", name)
			}
		} else {
			d.errorf(blk, "issuer %s needs url", name)
		}
		if id, ok := r["identity"]; ok {
			is.Identity = d.str(id.value, "identity")
			if !nameRE.MatchString(is.Identity) || is.Identity == devPersons {
				d.errorf(id.value, "issuer %s: identity %q must be lowercase letters, digits, _ and -, and not %s", name, is.Identity, devPersons)
			}
		} else {
			d.errorf(blk, "issuer %s needs identity, the name of the forge or identity provider whose user ids its tokens carry, such as a forge's name; issuers of one GitLab share it", name)
		}
		if c, ok := r["ca_file"]; ok {
			is.CAFile = d.path(c.value, "ca_file")
		}
		if is.Kind == oidc.People {
			d.people(blk, r, &is)
		} else {
			for _, k := range []string{"client_id", "client_secret_file", "groups"} {
				if x, ok := r[k]; ok {
					d.errorf(x.key, "issuer %s: %s is for kind people", name, k)
				}
			}
		}
		cfg.Issuers = append(cfg.Issuers, is)
	}
	slices.SortFunc(cfg.Issuers, func(a, b Issuer) int { return strings.Compare(a.Name, b.Name) })
	if len(cfg.Issuers) == 0 {
		d.errorf(top, "the config names no issuer, so no token can pass: add an issuer \"NAME\" { ... } block")
	}
}

// people reads the fields of a people issuer: the OAuth client writ serve
// is at the identity provider, and where a person's groups come from.
func (d *decoder) people(e hclsyntax.Node, r map[string]field, is *Issuer) {
	if c, ok := r["client_id"]; ok {
		is.ClientID = d.str(c.value, "client_id")
	} else {
		d.errorf(e, "issuer %s of kind people needs client_id", is.Name)
	}
	if c, ok := r["client_secret_file"]; ok {
		is.ClientSecretFile = d.path(c.value, "client_secret_file")
	} else {
		d.errorf(e, "issuer %s of kind people needs client_secret_file, the file that holds the client secret", is.Name)
	}
	if g, ok := r["groups"]; ok {
		switch v := d.str(g.value, "groups"); v {
		case "userinfo":
			is.GroupsFromUserinfo = true
		case "id_token":
		default:
			d.errorf(g.value, "issuer %s: groups is %q, not \"userinfo\" or \"id_token\"", is.Name, v)
		}
	}
}

func (d *decoder) teams(bs []*hclsyntax.Block, cfg *Config) {
	for _, blk := range bs {
		name, key := blk.Labels[0], blk.LabelRanges[0]
		if !nameRE.MatchString(name) {
			d.errorf(key, "team name %q must be lowercase letters, digits, _ and -", name)
		}
		r := d.attrs(blk, "team "+name, "issuer", "match")
		t := authz.Team{Name: name}
		if is, ok := r["issuer"]; ok {
			t.Issuer = d.str(is.value, "issuer")
			if !slices.ContainsFunc(cfg.Issuers, func(i Issuer) bool { return i.Name == t.Issuer }) {
				d.errorf(is.value, "team %s names issuer %s, which no issuer block defines", name, t.Issuer)
			}
		} else {
			d.errorf(blk, "team %s needs issuer", name)
		}
		m, ok := r["match"]
		if !ok {
			d.errorf(blk, "team %s needs match", name)
			continue
		}
		for claim, rf := range d.object(m.value, "match of team "+name) {
			vals := d.strs(rf.value, "match."+claim)
			if len(vals) == 0 {
				d.errorf(rf.value, "match.%s of team %s lists no value, so it never holds", claim, name)
			}
			t.Match = append(t.Match, authz.Rule{Claim: claim, Values: vals})
		}
		slices.SortFunc(t.Match, func(a, b authz.Rule) int { return strings.Compare(a.Claim, b.Claim) })
		if len(t.Match) == 0 {
			d.errorf(m.value, "team %s matches no claim, so it has no member", name)
		}
		cfg.Policy.Teams = append(cfg.Policy.Teams, t)
	}
	slices.SortFunc(cfg.Policy.Teams, func(a, b authz.Team) int { return strings.Compare(a.Name, b.Name) })
}

func (d *decoder) grants(bs []*hclsyntax.Block, cfg *Config) {
	for i, ge := range bs {
		r := d.attrs(ge, fmt.Sprintf("grant %d", i+1), "teams", "stacks", "envs", "can", "risks")
		var g authz.Grant
		get := func(k string) []string {
			f, ok := r[k]
			if !ok {
				if k == "risks" {
					return nil
				}
				d.errorf(ge, "grant %d needs %s", i+1, k)
				return nil
			}
			return d.strs(f.value, k)
		}
		g.Teams, g.Stacks, g.Envs, g.Risks = get("teams"), get("stacks"), get("envs"), get("risks")
		for _, t := range g.Teams {
			if !slices.ContainsFunc(cfg.Policy.Teams, func(x authz.Team) bool { return x.Name == t }) {
				d.errorf(r["teams"].value, "grant %d names team %s, which no team block defines", i+1, t)
			}
		}
		for _, s := range g.Stacks {
			if err := authz.ParsePattern(s); err != nil {
				d.errorf(r["stacks"].value, "grant %d: stack pattern %q %v", i+1, s, err)
			}
		}
		for _, en := range g.Envs {
			if _, ok := cfg.Envs[en]; !ok && en != "*" {
				d.errorf(r["envs"].value, "grant %d names env %s, which no env block defines", i+1, en)
			}
		}
		for _, c := range get("can") {
			op, err := authz.ParseOp(c)
			if err != nil {
				d.errorf(r["can"].value, "grant %d: %v", i+1, err)
			}
			g.Can = append(g.Can, op)
		}
		for _, risk := range g.Risks {
			if !slices.Contains(authz.Risks, risk) {
				d.errorf(r["risks"].value, "grant %d: risk class %q is not one of %s", i+1, risk, strings.Join(authz.Risks, ", "))
			}
		}
		cfg.Policy.Grants = append(cfg.Policy.Grants, g)
	}
}

// approvals reads the approval rules: each names stacks, envs, risk
// classes, and how many approvals a plan they match needs.
func (d *decoder) approvals(bs []*hclsyntax.Block, cfg *Config) {
	for i, ae := range bs {
		r := d.attrs(ae, fmt.Sprintf("approval rule %d", i+1), "stacks", "envs", "risks", "need")
		var rule authz.ApprovalRule
		for _, k := range []string{"stacks", "envs", "risks", "need"} {
			if _, ok := r[k]; !ok {
				d.errorf(ae, "approval rule %d needs %s", i+1, k)
			}
		}
		if f, ok := r["stacks"]; ok {
			rule.Stacks = d.strs(f.value, "stacks")
			for _, s := range rule.Stacks {
				if err := authz.ParsePattern(s); err != nil {
					d.errorf(f.value, "approval rule %d: stack pattern %q %v", i+1, s, err)
				}
			}
		}
		if f, ok := r["envs"]; ok {
			rule.Envs = d.strs(f.value, "envs")
			for _, en := range rule.Envs {
				if _, ok := cfg.Envs[en]; !ok && en != "*" {
					d.errorf(f.value, "approval rule %d names env %s, which no env block defines", i+1, en)
				}
			}
		}
		if f, ok := r["risks"]; ok {
			rule.Risks = d.strs(f.value, "risks")
			for _, risk := range rule.Risks {
				if !slices.Contains(authz.Risks, risk) {
					d.errorf(f.value, "approval rule %d: risk class %q is not one of %s", i+1, risk, strings.Join(authz.Risks, ", "))
				}
			}
		}
		if f, ok := r["need"]; ok {
			n, isInt := d.wholeNumber(f.value)
			if !isInt || n < 1 || n > maxNeed {
				d.errorf(f.value, "approval rule %d: need must be a whole number from 1 to %d", i+1, maxNeed)
			} else {
				rule.Need = int(n)
			}
		}
		cfg.Policy.Approvals = append(cfg.Policy.Approvals, rule)
	}
}

// maxNeed bounds how many approvals a rule may ask for.
const maxNeed = 10

// userClaims are the claims of a CI token that name the person who ran
// the job, rather than the job.
var userClaims = []string{"user_access_level", "user_id", "user_login", "github_team", "actor", "actor_id"}

// checkPeopleOps refuses the grants that would let a job, not a person,
// approve, override, or adopt: approve needs a team that matches a
// person's claim, and override and adopt a team of people.
func (d *decoder) checkPeopleOps(bs []*hclsyntax.Block, cfg *Config) {
	kinds := map[string]oidc.Kind{}
	for _, is := range cfg.Issuers {
		kinds[is.Name] = is.Kind
	}
	for i, g := range cfg.Policy.Grants {
		for _, name := range g.Teams {
			ti := slices.IndexFunc(cfg.Policy.Teams, func(t authz.Team) bool { return t.Name == name })
			if ti < 0 {
				continue
			}
			t := cfg.Policy.Teams[ti]
			if kinds[t.Issuer] == oidc.People {
				continue
			}
			if slices.Contains(g.Can, authz.Override) {
				d.errorf(bs[i], "grant %d gives override to team %s, whose issuer %s is a CI issuer: an override needs a person's reason, so give it to a team of people", i+1, t.Name, t.Issuer)
			}
			if slices.Contains(g.Can, authz.Adopt) {
				d.errorf(bs[i], "grant %d gives adopt to team %s, whose issuer %s is a CI issuer: an adoption needs a person's reason, so give it to a team of people", i+1, t.Name, t.Issuer)
			}
			person := slices.ContainsFunc(t.Match, func(r authz.Rule) bool { return slices.Contains(userClaims, r.Claim) })
			if slices.Contains(g.Can, authz.Approve) && !person {
				d.errorf(bs[i], "grant %d gives approve to team %s, which matches only claims of the job: match a claim of the person who runs it, such as user_access_level, user_id, or user_login, so an approval is a person's", i+1, t.Name)
			}
		}
	}
}

// lintIdentities warns when issuers of two identities, or a forge of the
// same name as one of them, have URLs that look like one host. One forge
// under two identities gives each person two ids, and an author could
// approve their own plan with the second. The comparison is best effort:
// an identity is what the config says, never what a URL spells.
func (d *decoder) lintIdentities(cfg *Config) {
	type use struct{ identity, what string }
	byHost := map[string][]use{}
	add := func(raw, identity, what string) {
		if h := hostKey(raw); h != "" {
			byHost[h] = append(byHost[h], use{identity, what})
		}
	}
	identities := map[string]bool{}
	for _, is := range cfg.Issuers {
		if is.Identity != "" {
			identities[is.Identity] = true
			add(is.URL, is.Identity, "issuer "+is.Name)
		}
	}
	for _, fg := range cfg.Forges {
		if identities[fg.Name] {
			add(fg.URL, fg.Name, "forge "+fg.Name)
		}
	}
	for _, h := range slices.Sorted(maps.Keys(byHost)) {
		us := byHost[h]
		for _, u := range us[1:] {
			if u.identity != us[0].identity {
				d.warnf(nil, "%s (identity %s) and %s (identity %s) look like one host, %s: if they are one forge, give them one identity, or each person there has two ids and can approve their own plans with the second",
					us[0].what, us[0].identity, u.what, u.identity, h)
				break
			}
		}
	}
}

// hostKey is a URL's host in one spelling, for lintIdentities: IDNA's
// ASCII form in lowercase without a trailing dot, an IP address in its
// shortest form, and the port as a number, without the scheme's default.
func hostKey(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	host := strings.TrimSuffix(u.Hostname(), ".")
	if a, err := netip.ParseAddr(host); err == nil {
		host = a.Unmap().String()
	} else if h, err := idna.Lookup.ToASCII(host); err == nil {
		host = h
	}
	host = strings.ToLower(host)
	port := u.Port()
	if n, err := strconv.ParseUint(port, 10, 16); err == nil {
		port = strconv.FormatUint(n, 10)
	}
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	if port == "" {
		return host
	}
	return net.JoinHostPort(host, port)
}
