// Package authz decides what a principal may do in writ serve. A team is a
// rule over the claims of one issuer's tokens, and a grant gives teams
// operations and risk classes on stacks in environments. Grants only add:
// there are no deny rules and no rule order.
package authz

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
)

// Op is an operation a grant gives.
type Op string

const (
	Read     Op = "read"     // plans, state, the journal, runs
	Plan     Op = "plan"     // remote plans
	Apply    Op = "apply"    // runs of approved plans
	Approve  Op = "approve"  // approvals of plans
	Override Op = "override" // applies that skip the approval rules
	Adopt    Op = "adopt"    // secret sets for states no apply sealed one with
	Admin    Op = "admin"    // sessions
)

// Ops are the operations, in the order tables print them.
var Ops = []Op{Read, Plan, Apply, Approve, Override, Adopt, Admin}

// The risk classes of a plan, by the names a contract and a grant use.
const (
	RiskCreate   = "create"
	RiskUpdate   = "update"
	RiskReplace  = "replace"
	RiskDelete   = "delete"
	RiskDataLoss = "data-loss"
)

// Risks are the risk classes, in the order tables print them.
var Risks = []string{RiskCreate, RiskUpdate, RiskReplace, RiskDelete, RiskDataLoss}

// ParseOp reads an operation from config.
func ParseOp(s string) (Op, error) {
	if slices.Contains(Ops, Op(s)) {
		return Op(s), nil
	}
	return "", fmt.Errorf("operation %q is not one of %s", s, joinOps(Ops))
}

func joinOps(ops []Op) string {
	s := make([]string, len(ops))
	for i, o := range ops {
		s[i] = string(o)
	}
	return strings.Join(s, ", ")
}

// Claims are a verified token's claims as text; oidc.Claims has this
// method.
type Claims interface {
	Values(name string) ([]string, bool)
}

// Rule holds when the claim's value, or any element of a list claim, equals
// one of Values. A value that ends in /* matches one more path segment, and
// one that ends in /** matches any number. A missing claim never holds.
type Rule struct {
	Claim  string
	Values []string
}

// Team is a named rule over one issuer's tokens. Every rule must hold.
type Team struct {
	Name   string
	Issuer string
	Match  []Rule
}

// Grant gives Teams the operations Can, and for apply the risk classes
// Risks, on every stack that matches Stacks (see ParsePattern) in every
// environment in Envs ("*" for all).
type Grant struct {
	Teams  []string
	Stacks []string
	Envs   []string
	Can    []Op
	Risks  []string
}

// ParsePattern checks a stack pattern of a grant or an approval rule. The
// pattern's parts, split at /, match the parts of a stack name. A part is a
// path.Match glob, whose * and ? stop at a slash, or ** alone, which
// matches any number of parts, including none. At most one part is **, so
// a match takes one pass over the name.
func ParsePattern(p string) error {
	if p == "" {
		return errors.New("is empty")
	}
	double := false
	for _, part := range strings.Split(p, "/") {
		switch {
		case part == "**" && double:
			return errors.New("holds ** more than once; one ** already matches any number of parts")
		case part == "**":
			double = true
		case strings.Contains(part, "**"):
			return fmt.Errorf("has ** inside the part %q; ** must be a whole part, as in data/**, and * matches within one part", part)
		default:
			if _, err := path.Match(part, ""); err != nil {
				return fmt.Errorf("is not a glob: the part %q is malformed", part)
			}
		}
	}
	return nil
}

// MatchStack reports whether stack matches pattern. A pattern that
// ParsePattern refuses matches nothing.
func MatchStack(pattern, stack string) bool {
	if ParsePattern(pattern) != nil {
		return false
	}
	pat, name := strings.Split(pattern, "/"), strings.Split(stack, "/")
	i := slices.Index(pat, "**")
	if i < 0 {
		return len(pat) == len(name) && matchParts(pat, name)
	}
	head, tail := pat[:i], pat[i+1:]
	return len(name) >= len(head)+len(tail) &&
		matchParts(head, name[:len(head)]) && matchParts(tail, name[len(name)-len(tail):])
}

// matchParts matches parts of equal length one by one.
func matchParts(pat, name []string) bool {
	for i := range pat {
		if ok, _ := path.Match(pat[i], name[i]); !ok {
			return false
		}
	}
	return true
}

// Policy is the teams, grants, and approval rules of one config.
type Policy struct {
	Teams     []Team
	Grants    []Grant
	Approvals []ApprovalRule
}

// ApprovalRule says how many approvals a plan needs: Need, when the plan
// changes a stack that matches Stacks (see ParsePattern) in an environment
// in Envs ("*" for all) with any risk class in Risks.
type ApprovalRule struct {
	Stacks []string
	Envs   []string
	Risks  []string
	Need   int
}

// Need returns how many approvals a plan of stack in env with risks needs:
// the largest Need among the rules that match, with the indexes into
// Approvals of those rules. A plan with no changes needs none.
func (p *Policy) Need(stack, env string, risks []string, changes int) (int, []int) {
	if changes == 0 {
		return 0, nil
	}
	need := 0
	var rules []int
	for i, r := range p.Approvals {
		if !envMatches(r.Envs, env) ||
			!slices.ContainsFunc(r.Stacks, func(pat string) bool { return MatchStack(pat, stack) }) ||
			!slices.ContainsFunc(r.Risks, func(x string) bool { return slices.Contains(risks, x) }) {
			continue
		}
		rules = append(rules, i)
		need = max(need, r.Need)
	}
	return need, rules
}

// TeamsWith returns the teams that some grant gives op on stack@env, in
// config order, for the teams a plan's page lists as those that may
// approve or apply it. For apply, risks must be granted too.
func (p *Policy) TeamsWith(op Op, stack, env string, risks []string) []string {
	out := []string{}
	for _, t := range p.Teams {
		if p.Decide([]string{t.Name}, op, []string{stack}, env, risks).Allowed {
			out = append(out, t.Name)
		}
	}
	return out
}

// TeamsOf returns the names of every team whose rules the claims of a token
// from issuer match, sorted.
func (p *Policy) TeamsOf(issuer string, c Claims) []string {
	var out []string
	for _, t := range p.Teams {
		if t.Issuer == issuer && t.matches(c) {
			out = append(out, t.Name)
		}
	}
	slices.Sort(out)
	return out
}

func (t Team) matches(c Claims) bool {
	if len(t.Match) == 0 {
		return false
	}
	for _, r := range t.Match {
		got, ok := c.Values(r.Claim)
		if !ok || !slices.ContainsFunc(got, func(v string) bool {
			return slices.ContainsFunc(r.Values, func(want string) bool { return valueMatch(want, v) })
		}) {
			return false
		}
	}
	return true
}

// valueMatch matches one claim value against one listed value.
func valueMatch(want, got string) bool {
	switch {
	case strings.HasSuffix(want, "/**"):
		base := strings.TrimSuffix(want, "/**")
		return got == base || strings.HasPrefix(got, base+"/")
	case strings.HasSuffix(want, "/*"):
		base := strings.TrimSuffix(want, "/*")
		rest, ok := strings.CutPrefix(got, base+"/")
		return ok && rest != "" && !strings.Contains(rest, "/")
	}
	return want == got
}

// Decision is the answer to one question, with the grants behind it.
type Decision struct {
	Allowed bool
	// Grants are the indexes into Policy.Grants that gave the operation,
	// in order. Empty when the decision is a refusal.
	Grants []int
	// Reason says what is missing when the decision is a refusal.
	Reason string
}

// Decide says whether a principal in teams may do op on every stack in
// stacks in env. For apply, every risk class in risks must be granted on
// every stack too, from one grant or several.
func (p *Policy) Decide(teams []string, op Op, stacks []string, env string, risks []string) Decision {
	used := map[int]bool{}
	for _, st := range stacks {
		idx := p.covering(teams, op, st, env)
		if len(idx) == 0 {
			return Decision{Reason: fmt.Sprintf("no grant gives %s %s on %s@%s", teamWords(teams), op, st, env)}
		}
		for _, i := range idx {
			used[i] = true
		}
		if op != Apply {
			continue
		}
		for _, r := range risks {
			j := slices.IndexFunc(idx, func(i int) bool { return slices.Contains(p.Grants[i].Risks, r) })
			if j < 0 {
				return Decision{Reason: fmt.Sprintf("no grant gives %s the risk class %s on %s@%s", teamWords(teams), r, st, env)}
			}
		}
	}
	return p.allowed(used)
}

// allowed is the decision that allows, with the grants in used in order.
func (p *Policy) allowed(used map[int]bool) Decision {
	d := Decision{Allowed: true}
	for i := range p.Grants {
		if used[i] {
			d.Grants = append(d.Grants, i)
		}
	}
	return d
}

// InEnv reports whether some grant gives teams op on any stack in env, for
// what belongs to an environment and to no stack, such as a change made
// outside Writ to an object no stack manages.
func (p *Policy) InEnv(teams []string, op Op, env string) bool {
	return slices.ContainsFunc(p.Grants, func(g Grant) bool { return g.gives(teams, op) && envMatches(g.Envs, env) })
}

// covering returns the grants that give teams op on stack@env.
func (p *Policy) covering(teams []string, op Op, stack, env string) []int {
	var out []int
	for i, g := range p.Grants {
		if g.covers(teams, op, stack, env) {
			out = append(out, i)
		}
	}
	return out
}

func (g Grant) covers(teams []string, op Op, stack, env string) bool {
	return g.gives(teams, op) && envMatches(g.Envs, env) &&
		slices.ContainsFunc(g.Stacks, func(pat string) bool { return MatchStack(pat, stack) })
}

// envMatches reports whether envs, a grant's or a rule's, holds env or "*".
func envMatches(envs []string, env string) bool {
	return slices.ContainsFunc(envs, func(e string) bool { return e == "*" || e == env })
}

func (g Grant) gives(teams []string, op Op) bool {
	return slices.ContainsFunc(g.Teams, func(t string) bool { return slices.Contains(teams, t) }) && slices.Contains(g.Can, op)
}

// DecideEvery says whether a principal in teams may do op on every stack
// name of up to maxParts parts, in every environment. Only grants whose
// envs hold "*" count. A depth of names is covered by a pattern whose
// parts are each * or **: with no **, it covers names of as many parts as
// it has, and with **, names of at least as many parts as its * parts.
// Any other pattern counts as covering nothing here, so the answer may
// refuse what the patterns allow, never the reverse.
func (p *Policy) DecideEvery(teams []string, op Op, maxParts int) Decision {
	used := map[int]bool{}
	for parts := 1; parts <= maxParts; parts++ {
		i := slices.IndexFunc(p.Grants, func(g Grant) bool {
			return g.gives(teams, op) && slices.Contains(g.Envs, "*") &&
				slices.ContainsFunc(g.Stacks, func(pat string) bool { return everyName(pat, parts) })
		})
		if i < 0 {
			n := fmt.Sprintf("%d parts", parts)
			if parts == 1 {
				n = "one part"
			}
			return Decision{Reason: fmt.Sprintf("no grant gives %s %s on every stack in every environment: none covers the stack names of %s", teamWords(teams), op, n)}
		}
		used[i] = true
	}
	return p.allowed(used)
}

// everyName reports whether pat matches every stack name of parts parts.
func everyName(pat string, parts int) bool {
	if ParsePattern(pat) != nil {
		return false
	}
	segs := strings.Split(pat, "/")
	stars := 0
	for _, s := range segs {
		if s != "*" && s != "**" {
			return false
		}
		if s == "*" {
			stars++
		}
	}
	if slices.Contains(segs, "**") {
		return stars <= parts
	}
	return stars == parts
}

func teamWords(teams []string) string {
	if len(teams) == 0 {
		return "a principal in no team"
	}
	return "the teams " + strings.Join(teams, ", ")
}

// Row is one grant that applies to a principal, for writ whoami.
type Row struct {
	Grant int
	Envs  []string
	Stack []string
	Can   []Op
	Risks []string
}

// Table returns the grants that name any of teams, in config order.
func (p *Policy) Table(teams []string) []Row {
	var out []Row
	for i, g := range p.Grants {
		if slices.ContainsFunc(g.Teams, func(t string) bool { return slices.Contains(teams, t) }) {
			out = append(out, Row{Grant: i, Envs: g.Envs, Stack: g.Stacks, Can: g.Can, Risks: g.Risks})
		}
	}
	return out
}
