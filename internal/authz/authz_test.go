package authz_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/mr-karan/writ/internal/authz"
	"github.com/mr-karan/writ/internal/oidc"
)

func claims(t *testing.T, js string) oidc.Claims {
	t.Helper()
	var c oidc.Claims
	if err := json.Unmarshal([]byte(js), &c); err != nil {
		t.Fatal(err)
	}
	return c
}

var policy = &authz.Policy{
	Teams: []authz.Team{
		{Name: "infra_mr", Issuer: "gitlab", Match: []authz.Rule{{"project_id", []string{"4242"}}, {"pipeline_source", []string{"merge_request_event"}}}},
		{Name: "infra_main", Issuer: "gitlab", Match: []authz.Rule{{"project_id", []string{"4242"}}, {"ref_path", []string{"refs/heads/main"}}, {"ref_protected", []string{"true"}}}},
		{Name: "reviewers", Issuer: "gitlab", Match: []authz.Rule{{"project_id", []string{"4242"}}, {"user_access_level", []string{"maintainer", "owner"}}}},
		{Name: "platform", Issuer: "sso", Match: []authz.Rule{{"groups", []string{"acme/platform/**"}}}},
		{Name: "teams_one_level", Issuer: "sso", Match: []authz.Rule{{"groups", []string{"acme/*"}}}},
		{Name: "flag", Issuer: "sso", Match: []authz.Rule{{"admin", []string{"true"}}}},
		{Name: "empty", Issuer: "gitlab"},
	},
	Grants: []authz.Grant{
		{Teams: []string{"infra_mr"}, Stacks: []string{"*"}, Envs: []string{"staging", "prod"}, Can: []authz.Op{authz.Read, authz.Plan}},
		{Teams: []string{"infra_main"}, Stacks: []string{"*"}, Envs: []string{"prod"}, Can: []authz.Op{authz.Read, authz.Apply}, Risks: []string{"create", "update"}},
		{Teams: []string{"infra_main"}, Stacks: []string{"database"}, Envs: []string{"prod"}, Can: []authz.Op{authz.Apply}, Risks: []string{"delete"}},
		{Teams: []string{"platform"}, Stacks: []string{"data/*"}, Envs: []string{"*"}, Can: []authz.Op{authz.Plan}},
		{Teams: []string{"admins"}, Stacks: []string{"**"}, Envs: []string{"*"}, Can: []authz.Op{authz.Admin}},
	},
}

func TestTeams(t *testing.T) {
	tests := []struct {
		name   string
		issuer string
		claims string
		want   []string
	}{
		{"merge request job", "gitlab", `{"project_id":"4242","pipeline_source":"merge_request_event"}`, []string{"infra_mr"}},
		{"another project", "gitlab", `{"project_id":"4243","pipeline_source":"merge_request_event"}`, nil},
		{"right claims, other issuer", "actions", `{"project_id":"4242","pipeline_source":"merge_request_event"}`, nil},
		{"main needs every rule", "gitlab", `{"project_id":"4242","ref_path":"refs/heads/main","ref_protected":"false"}`, nil},
		{"main, protected", "gitlab", `{"project_id":"4242","ref_path":"refs/heads/main","ref_protected":"true"}`, []string{"infra_main"}},
		{"a number compares as its text", "gitlab", `{"project_id":4242,"pipeline_source":"merge_request_event"}`, []string{"infra_mr"}},
		{"a boolean compares as its text", "sso", `{"admin":true}`, []string{"flag"}},
		{"one of several values", "gitlab", `{"project_id":"4242","user_access_level":"owner"}`, []string{"reviewers"}},
		{"missing claim never holds", "gitlab", `{"project_id":"4242"}`, nil},
		{"list claim, any element", "sso", `{"groups":["other","acme/platform"]}`, []string{"platform", "teams_one_level"}},
		{"** covers subgroups", "sso", `{"groups":["acme/platform/dba/oncall"]}`, []string{"platform"}},
		{"* is one segment, ** is not a prefix", "sso", `{"groups":["acme/platformx"]}`, []string{"teams_one_level"}},
		{"null claim", "sso", `{"groups":null}`, nil},
		{"team with no rules matches nobody", "gitlab", `{}`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := policy.TeamsOf(tt.issuer, claims(t, tt.claims))
			if !slices.Equal(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDecide(t *testing.T) {
	tests := []struct {
		name   string
		teams  []string
		op     authz.Op
		stacks []string
		env    string
		risks  []string
		grants []int
		reason string
	}{
		{"plan prod", []string{"infra_mr"}, authz.Plan, []string{"network"}, "prod", nil, []int{0}, ""},
		{"plan an env no grant names", []string{"infra_mr"}, authz.Plan, []string{"network"}, "dev", nil, nil, "plan on network@dev"},
		{"mr job may not apply", []string{"infra_mr"}, authz.Apply, []string{"network"}, "prod", []string{"create"}, nil, "apply on network@prod"},
		{"apply within risks", []string{"infra_main"}, authz.Apply, []string{"network"}, "prod", []string{"create", "update"}, []int{1}, ""},
		{"apply a risk no grant gives", []string{"infra_main"}, authz.Apply, []string{"network"}, "prod", []string{"delete"}, nil, "risk class delete on network@prod"},
		{"two grants together", []string{"infra_main"}, authz.Apply, []string{"database"}, "prod", []string{"create", "delete"}, []int{1, 2}, ""},
		{"every stack must be covered", []string{"infra_main"}, authz.Apply, []string{"database", "network"}, "prod", []string{"delete"}, nil, "risk class delete on network@prod"},
		{"glob over a stack path", []string{"platform"}, authz.Plan, []string{"data/pg"}, "staging", nil, []int{3}, ""},
		{"glob is one segment", []string{"platform"}, authz.Plan, []string{"data/pg/replica"}, "staging", nil, nil, "plan on data/pg/replica@staging"},
		{"no teams", nil, authz.Read, []string{"network"}, "prod", nil, nil, "a principal in no team"},
		{"plan ignores risks", []string{"infra_mr"}, authz.Plan, []string{"network"}, "prod", []string{"data-loss"}, []int{0}, ""},
		{"** covers every depth", []string{"admins"}, authz.Admin, []string{"data/pg/replica", "network"}, "prod", nil, []int{4}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := policy.Decide(tt.teams, tt.op, tt.stacks, tt.env, tt.risks)
			if d.Allowed != (tt.reason == "") || !slices.Equal(d.Grants, tt.grants) || !strings.Contains(d.Reason, tt.reason) {
				t.Fatalf("got %+v, want grants %v reason %q", d, tt.grants, tt.reason)
			}
		})
	}
}

func TestTable(t *testing.T) {
	rows := policy.Table([]string{"infra_main"})
	if len(rows) != 2 || rows[0].Grant != 1 || rows[1].Grant != 2 {
		t.Fatalf("got %+v", rows)
	}
}

// TestStackPatterns checks grant patterns: * matches within one part of a
// stack name, and a part ** matches any number of parts, including none.
func TestStackPatterns(t *testing.T) {
	for _, tc := range []struct {
		pattern, stack string
		want           bool
	}{
		{"dir/**", "dir", true},
		{"dir/**", "dir/a", true},
		{"dir/**", "dir/a/b", true},
		{"dir/**", "dir/a/b/c/d/e/f", true},
		{"dir/**", "dirt/a", false},
		{"dir/**", "other/dir/a", false},
		{"**", "network", true},
		{"**", "a/b/c", true},
		{"**/pg", "pg", true},
		{"**/pg", "data/pg", true},
		{"**/pg", "eu/data/pg", true},
		{"**/pg", "data/pg/replica", false},
		{"**/pg", "data/pgx", false},
		{"data/**/pg", "data/pg", true},
		{"data/**/pg", "data/eu/pg", true},
		{"data/**/pg", "data/eu/west/pg", true},
		{"data/**/pg", "data/eu/west/mysql", false},
		{"data/**/pg", "eu/data/pg", false},
		{"a/**/b/c", "a/b", false},
		{"a/**/a", "a", false},
		{"a/**/a", "a/a", true},
		{"*/**", "x", true},
		{"*/**", "x/y/z", true},
		{"d*/**/p?", "data/eu/pg", true},
		{"*", "network", true},
		{"*", "apps/api", false},
		{"apps/*", "apps/api", true},
		{"apps/*", "apps/api/v2", false},
		{"apps/*", "apps", false},
		{"*/*", "apps/api", true},
		{"data", "data", true},
		{"data", "data/pg", false},
	} {
		t.Run(tc.pattern+" "+tc.stack, func(t *testing.T) {
			p := authz.Policy{
				Grants:    []authz.Grant{{Teams: []string{"team"}, Stacks: []string{tc.pattern}, Envs: []string{"prod"}, Can: []authz.Op{authz.Plan}}},
				Approvals: []authz.ApprovalRule{{Stacks: []string{tc.pattern}, Envs: []string{"prod"}, Risks: []string{"create"}, Need: 1}},
			}
			if got := p.Decide([]string{"team"}, authz.Plan, []string{tc.stack}, "prod", nil).Allowed; got != tc.want {
				t.Errorf("grant %q on %s: allowed %v, want %v", tc.pattern, tc.stack, got, tc.want)
			}
			if need, _ := p.Need(tc.stack, "prod", []string{"create"}, 1); (need == 1) != tc.want {
				t.Errorf("approval rule %q on %s: need %d, want a match %v", tc.pattern, tc.stack, need, tc.want)
			}
		})
	}
}

func TestParsePattern(t *testing.T) {
	for _, tc := range []struct{ pattern, err string }{
		{"network", ""},
		{"*", ""},
		{"**", ""},
		{"dir/**", ""},
		{"**/pg", ""},
		{"data/**/p?", ""},
		{"[a-c]*/**", ""},
		{"", "is empty"},
		{"[", "is not a glob"},
		{"data/[", "is not a glob"},
		{"a**", `has ** inside the part "a**"`},
		{"data/**x/pg", `has ** inside the part "**x"`},
		{"***", `has ** inside the part "***"`},
		{"**/**", "holds ** more than once"},
		{"a/**/b/**", "holds ** more than once"},
	} {
		t.Run(tc.pattern, func(t *testing.T) {
			err := authz.ParsePattern(tc.pattern)
			if tc.err == "" && err != nil || tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)) {
				t.Fatalf("ParsePattern(%q) = %v, want %q", tc.pattern, err, tc.err)
			}
			if err != nil && authz.MatchStack(tc.pattern, "a/b") {
				t.Fatalf("refused pattern %q matches", tc.pattern)
			}
		})
	}
}

// TestMatchStackLinear matches a deep name against patterns that would
// backtrack in a matcher that tried every split, and must not.
func TestMatchStackLinear(t *testing.T) {
	name := strings.Repeat("a/", 5000) + "b"
	for _, pat := range []string{"**/a", "a/**/c", strings.Repeat("a/", 100) + "**/" + strings.Repeat("a/", 100) + "c"} {
		if authz.MatchStack(pat, name) {
			t.Fatalf("%.30s... matches", pat)
		}
	}
	if !authz.MatchStack(strings.Repeat("*/", 100)+"**/*/b", name) {
		t.Fatal("deep name does not match")
	}
}

func TestDecideEvery(t *testing.T) {
	depths := func(n int) []string {
		var out []string
		for i := range n {
			out = append(out, strings.Repeat("*/", i)+"*")
		}
		return out
	}
	grant := func(stacks []string, envs ...string) authz.Grant {
		return authz.Grant{Teams: []string{"admins"}, Stacks: stacks, Envs: envs, Can: []authz.Op{authz.Admin}}
	}
	tests := []struct {
		name   string
		grants []authz.Grant
		want   string
	}{
		{"* is one depth", []authz.Grant{grant([]string{"*"}, "*")}, "none covers the stack names of 2 parts"},
		{"** is every depth", []authz.Grant{grant([]string{"**"}, "*")}, ""},
		{"*/** is every depth from one", []authz.Grant{grant([]string{"*/**"}, "*")}, ""},
		{"*/*/** misses one part", []authz.Grant{grant([]string{"*/*/**"}, "*")}, "of one part"},
		{"*/*/** with *", []authz.Grant{grant([]string{"*", "*/*/**"}, "*")}, ""},
		{"* and */**/* from two grants", []authz.Grant{grant([]string{"*"}, "*"), grant([]string{"*/**/*"}, "*")}, ""},
		{"every depth", []authz.Grant{grant(depths(4), "*")}, ""},
		{"every depth from two grants", []authz.Grant{grant(depths(2), "*"), grant([]string{"*/*/*", "*/*/*/*"}, "*")}, ""},
		{"a mixed ** is no pattern", []authz.Grant{grant([]string{"*", "*/*", "*/*/*", "***/*/*/*"}, "*")}, "of 4 parts"},
		{"a prefix is not every name", []authz.Grant{grant([]string{"data/**"}, "*")}, "of one part"},
		{"one depth short", []authz.Grant{grant(depths(3), "*")}, "of 4 parts"},
		{"a pattern that names a part", []authz.Grant{grant([]string{"*", "*/x", "*/*/*", "*/*/*/*"}, "*")}, "of 2 parts"},
		{"a pattern with a wildcard part", []authz.Grant{grant([]string{"*", "?*/*", "*/*/*", "*/*/*/*"}, "*")}, "of 2 parts"},
		{"one environment", []authz.Grant{grant(depths(4), "prod")}, "of one part"},
		{"another operation", []authz.Grant{{Teams: []string{"admins"}, Stacks: []string{"**"}, Envs: []string{"*"}, Can: []authz.Op{authz.Read}}}, "of one part"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := authz.Policy{Grants: tt.grants}
			d := p.DecideEvery([]string{"admins"}, authz.Admin, 4)
			if tt.want == "" {
				if !d.Allowed || len(d.Grants) == 0 {
					t.Fatalf("refused: %s", d.Reason)
				}
				return
			}
			if d.Allowed || !strings.Contains(d.Reason, tt.want) {
				t.Fatalf("got %+v, want a refusal with %q", d, tt.want)
			}
		})
	}
}

// The design's three example rules: any change to prod needs one, any
// delete or data loss anywhere needs one, and a destructive change to the
// prod database needs two.
func TestNeed(t *testing.T) {
	p := &authz.Policy{Approvals: []authz.ApprovalRule{
		{Stacks: []string{"*"}, Envs: []string{"prod"}, Risks: authz.Risks, Need: 1},
		{Stacks: []string{"*"}, Envs: []string{"*"}, Risks: []string{"delete", "data-loss"}, Need: 1},
		{Stacks: []string{"database"}, Envs: []string{"prod"}, Risks: []string{"replace", "delete", "data-loss"}, Need: 2},
	}}
	for _, tc := range []struct {
		name, stack, env string
		risks            []string
		changes          int
		need             int
		rules            []int
	}{
		{"staging create", "network", "staging", []string{"create"}, 1, 0, nil},
		{"staging delete", "network", "staging", []string{"delete"}, 1, 1, []int{1}},
		{"prod create", "network", "prod", []string{"create"}, 1, 1, []int{0}},
		{"prod database update", "database", "prod", []string{"update"}, 1, 1, []int{0}},
		{"prod database replace", "database", "prod", []string{"replace"}, 1, 2, []int{0, 2}},
		{"prod database data loss", "database", "prod", []string{"delete", "data-loss"}, 2, 2, []int{0, 1, 2}},
		{"no changes", "database", "prod", nil, 0, 0, nil},
		{"a stack under a slash", "data/pg", "prod", []string{"create"}, 1, 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			need, rules := p.Need(tc.stack, tc.env, tc.risks, tc.changes)
			if need != tc.need || !slices.Equal(rules, tc.rules) {
				t.Fatalf("Need = %d %v, want %d %v", need, rules, tc.need, tc.rules)
			}
		})
	}
}
