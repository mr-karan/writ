package server

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/authz"
	"github.com/mr-karan/writ/internal/diag"
	"github.com/mr-karan/writ/internal/egress"
)

func TestParseConfigSample(t *testing.T) {
	src, err := os.ReadFile("testdata/serve.hcl")
	if err != nil {
		t.Fatal(err)
	}
	cfg, diags := ParseConfig("testdata/serve.hcl", src)
	if diags.HasErrors() {
		t.Fatal(diags)
	}
	if len(diags) != 0 {
		t.Fatalf("warnings: %v", diags)
	}
	if cfg.URL != "https://writ.example.com" || cfg.TLS == nil || cfg.PlanTTL != 24*time.Hour || cfg.Retention.Audit != 400*24*time.Hour {
		t.Fatalf("got %+v", cfg)
	}
	if len(cfg.Policy.Teams) != 3 || cfg.Policy.Teams[1].Name != "infra_mr" || len(cfg.Policy.Grants) != 3 || len(cfg.Policy.Approvals) != 2 {
		t.Fatalf("policy %+v", cfg.Policy)
	}
	if g := cfg.Policy.Grants; !slices.Equal(g[0].Teams, []string{"infra_mr"}) || !slices.Equal(g[2].Teams, []string{"infra_main"}) {
		t.Fatalf("grants out of order: %+v", g)
	}
	if need, rules := cfg.Policy.Need("network", "staging", []string{"delete"}, 1); need != 1 || len(rules) != 1 || rules[0] != 1 {
		t.Fatalf("need of a staging delete: %d %v", need, rules)
	}
	if cfg.Envs["prod"].ApplyRole != "arn:aws:iam::222222222222:role/writ-apply" || cfg.Limits.Applies != 4 {
		t.Fatalf("apply role %q, applies %d", cfg.Envs["prod"].ApplyRole, cfg.Limits.Applies)
	}
	if d := cfg.Policy.Decide([]string{"infra_mr"}, authz.Plan, []string{"network"}, "prod", nil); !d.Allowed {
		t.Fatal(d.Reason)
	}
	if cfg.Limits.Body != 16<<20 {
		t.Fatalf("limits %+v", cfg.Limits)
	}
	if got := cfg.Envs["staging"].S3Endpoints; !slices.Equal(got, []string{"https://s3.storage.example.com", "http://127.0.0.1:7070"}) || cfg.Envs["prod"].S3Endpoints != nil {
		t.Fatalf("s3_endpoints of staging %q and of prod %q", got, cfg.Envs["prod"].S3Endpoints)
	}
	prod := cfg.Envs["prod"]
	if prod.PlanRole != "arn:aws:iam::222222222222:role/writ-plan" || prod.ProviderEnv["NOMAD_TOKEN"] != "/run/secrets/prod/nomad_token" {
		t.Fatalf("prod %+v", prod)
	}
	if *cfg.AWS != (AWS{Region: "eu-west-1", Session: time.Hour}) || *cfg.ProviderUIDs != (UIDRange{First: 61000, Last: 61063}) {
		t.Fatalf("aws %+v, provider_uids %+v", cfg.AWS, cfg.ProviderUIDs)
	}
}

const minimal = `url = "http://127.0.0.1:8080"
listen = "127.0.0.1:8080"
database = "postgres://writ@/writ?host=/tmp"

issuer "gitlab" {
  kind = "gitlab-ci"
  identity = "gitlab"
  url = "https://gitlab.com"
}

team "mr" {
  issuer = "gitlab"
  match = { project_id = ["1"] }
}

grant {
  teams = ["mr"]
  stacks = ["*"]
  envs = ["prod"]
  can = ["plan"]
}

env "prod" {
  key = "keys/prod.key"
}

`

// edited is minimal with old replaced by new once, or with new added at
// the end when old is "".
func edited(t *testing.T, old, new string) string {
	t.Helper()
	if old == "" {
		return minimal + new + "\n"
	}
	src := strings.Replace(minimal, old, new, 1)
	if src == minimal {
		t.Fatalf("the edit %q matched nothing", old)
	}
	return src
}

func TestParseConfigErrors(t *testing.T) {
	const key = `key = "keys/prod.key"`
	env := func(more string) string { return key + "\n  " + more }
	approval := func(more string) string {
		return "approval {\n  stacks = [\"*\"]\n  envs = [\"prod\"]\n  " + more + "\n}"
	}
	const people = "kind = \"people\"\n  client_id = \"c\"\n  client_secret_file = \"s\""
	const tofuBlock = "provider_uids = \"61000-61063\"\ntofu {\n  binary = \"tofu\"\n  plugin_cache = \"/var/lib/writ-tofu/plugins\"\n  scratch_dir = \"/var/lib/writ-tofu/runs\"\n}\n"
	tests := []struct {
		name, old, new, want string
	}{
		{"not a body", minimal, `[1]`, "argument or block definition required"},
		{"unknown egress_mode", "", `egress_mode = "on"`, `egress_mode must be "off", "report", or "enforce", not "on"`},
		{"egress_mode without tofu", "", `egress_mode = "report"`, "egress_mode applies to the OpenTofu runs, and the config has no tofu block"},
		{"egress not a list", key, env(`egress = "*.amazonaws.com"`), "egress of env prod must be a list [ ... ]"},
		{"egress wildcard too wide", key, env(`egress = ["*.com"]`), `env prod: egress item "*.com": a wildcard needs a domain of two labels or more`},
		{"egress loopback", key, env(`egress = ["127.0.0.1:5432"]`), `env prod: egress item "127.0.0.1:5432": the egress proxy never connects to a loopback address`},
		{"egress url", key, env(`egress = ["https://sts.amazonaws.com"]`), `env prod: egress item "https://sts.amazonaws.com": it is not HOST, *.DOMAIN, or an IP address`},
		{"egress with a loopback s3 endpoint", key, env("s3_endpoints = [\"http://127.0.0.1:7070\"]\n}\negress_mode = \"enforce\"\n" + tofuBlock + "env \"qa\" {\n  key = \"k\""), "env prod: s3_endpoints lists http://127.0.0.1:7070, a loopback address, which a run cannot reach from its own network namespace under egress_mode enforce"},
		{"unknown key", "", `lisen = "x"`, "the config has no field lisen"},
		{"unknown block", "", `listen_on { port = 1 }`, "the config has no field listen_on"},
		{"a block as an attribute", "", `tls = { cert = "c", key = "k" }`, "tls is a block in the config: write tls { ... }"},
		{"a named block as an attribute", "", `env = { qa = { key = "k" } }`, `env is a block in the config: write env "NAME" { ... }`},
		{"an attribute as a block", "", `url { host = "x" }`, "url is an attribute in the config: write url = ..."},
		{"a block with a label", "", `tls "main" { cert = "c" }`, "tls takes no label"},
		{"a named block without a name", "", `env { key = "k" }`, "env takes one label, its name"},
		{"a named block twice", "", `env "prod" { key = "k" }`, "the config names env prod twice"},
		{"a block twice", "", "limits { plans = 2 }\nlimits { plans = 3 }", "the config sets limits twice"},
		{"no tls off loopback", `listen = "127.0.0.1:8080"`, `listen = "0.0.0.0:8080"`, "needs tls"},
		{"http url off loopback", `url = "http://127.0.0.1:8080"`, `url = "http://writ.example.com"`, "url must be https"},
		{"url with a path", `url = "http://127.0.0.1:8080"`, `url = "https://writ.example.com/api"`, "url must be https"},
		{"missing url", "url = \"http://127.0.0.1:8080\"\n", ``, "the config needs url"},
		{"password in the database url", `postgres://writ@`, `postgres://writ:hunter2@`, "holds a password"},
		{"sqlite database", `postgres://writ@/writ?host=/tmp`, `/var/lib/writ/control.db`, "must be postgres"},
		{"unknown issuer kind", `kind = "gitlab-ci"`, `kind = "jenkins"`, "not one of"},
		{"people issuer without a client", `kind = "gitlab-ci"`, `kind = "people"`, "needs client_id"},
		{"people issuer without a secret", `kind = "gitlab-ci"`, "kind = \"people\"\n  client_id = \"c\"", "needs client_secret_file"},
		{"people issuer with other groups", `kind = "gitlab-ci"`, people + "\n  groups = \"ldap\"", "not \"userinfo\""},
		{"client of a CI issuer", `kind = "gitlab-ci"`, "kind = \"gitlab-ci\"\n  client_id = \"c\"", "client_id is for kind people"},
		{"CI issuer without an identity", "  identity = \"gitlab\"\n", ``, "issuer gitlab needs identity"},
		{"people issuer without an identity", "kind = \"gitlab-ci\"\n  identity = \"gitlab\"", people, "issuer gitlab needs identity"},
		{"identity of a host", `identity = "gitlab"`, `identity = "gitlab.com"`, "must be lowercase letters"},
		{"identity of the dev user", `identity = "gitlab"`, `identity = "dev"`, "and not dev"},
		{"issuer name of a host", `issuer "gitlab" {`, `issuer "gitlab.com" {`, `issuer name "gitlab.com" must be lowercase letters`},
		{"no issuer", "issuer \"gitlab\" {\n  kind = \"gitlab-ci\"\n  identity = \"gitlab\"\n  url = \"https://gitlab.com\"\n}\n", ``, "names no issuer, so no token can pass"},
		{"ui without a people issuer", "", `ui { enabled = true }`, "needs an issuer of kind people"},
		{"ui enabled not a bool", "", `ui { enabled = "yes" }`, "true or false"},
		{"ui enabled a number", "", `ui { enabled = 1 }`, "true or false"},
		{"ui with an unknown field", "", `ui { actions = [] }`, "ui has no field actions"},
		{"issuer over http", `url = "https://gitlab.com"`, `url = "http://gitlab.com"`, "must be https"},
		{"team of an unknown issuer", `issuer = "gitlab"`, `issuer = "github"`, "which no issuer block defines"},
		{"team with no rule", `match = { project_id = ["1"] }`, `match = {}`, "matches no claim"},
		{"team match not an object", `match = { project_id = ["1"] }`, `match = ["project_id"]`, "match of team mr must be an object"},
		{"team match with a computed key", `match = { project_id = ["1"] }`, `match = { (x) = ["1"] }`, "a key must be a name or a plain string"},
		{"team match with a key twice", `match = { project_id = ["1"] }`, `match = { project_id = ["1"], "project_id" = ["2"] }`, "sets project_id twice"},
		{"rule with no value", `project_id = ["1"]`, `project_id = []`, "lists no value"},
		{"no team", "team \"mr\" {\n  issuer = \"gitlab\"\n  match = { project_id = [\"1\"] }\n}\n", ``, "the config needs a team"},
		{"grant of an unknown team", `teams = ["mr"]`, `teams = ["mx"]`, "team mx"},
		{"grant of an unknown op", `can = ["plan"]`, `can = ["deploy"]`, "not one of read"},
		{"grant of an unknown env", `envs = ["prod"]`, `envs = ["qa"]`, "env qa"},
		{"grant of an unknown risk", `can = ["plan"]`, "can = [\"plan\"]\n  risks = [\"destroy\"]", "risk class"},
		{"grant without can", "  can = [\"plan\"]\n", ``, "grant 1 needs can"},
		{"no grant", "grant {\n  teams = [\"mr\"]\n  stacks = [\"*\"]\n  envs = [\"prod\"]\n  can = [\"plan\"]\n}\n", ``, "the config needs a grant"},
		{"bad stack glob", `stacks = ["*"]`, `stacks = ["["]`, "not a glob"},
		{"** inside a part", `stacks = ["*"]`, `stacks = ["apps**"]`, `grant 1: stack pattern "apps**" has ** inside the part "apps**"`},
		{"two **", `stacks = ["*"]`, `stacks = ["a/**/b/**"]`, `grant 1: stack pattern "a/**/b/**" holds ** more than once`},
		{"empty stack pattern", `stacks = ["*"]`, `stacks = [""]`, `grant 1: stack pattern "" is empty`},
		{"stacks not a list", `stacks = ["*"]`, `stacks = "*"`, "stacks must be a list"},
		{"env sets state", key, env(`state = "s3://b/p"`), "env prod: state was removed: a stack's backend block says where its state is; delete it"},
		{"secret_env", key, env(`secret_env = { DB_PASSWORD = "/k" }`), "env prod: secret_env was removed: it gave values to secret_env() of the Writ language; give a provider a secret with provider_env; delete it"},
		{"a provider list", "", "provider {\n  source = \"hashicorp/random\"\n  versions = [\"3.9.1\"]\n}", "the config: provider was removed: a stack runs the providers its .terraform.lock.hcl pins, and a policy can restrict them; delete it"},
		{"providers_dir", "", `providers_dir = "/var/lib/writ-providers"`, "the config: providers_dir was removed: OpenTofu installs the providers each stack's .terraform.lock.hcl pins into tofu.plugin_cache; delete it"},
		{"limits.files", "", `limits { files = 2000 }`, "limits: files was removed: it bounded the source files of a bundle of the Writ language, which writ serve no longer reads; delete it"},
		{"interpolated string", `url = "http://127.0.0.1:8080"`, `url = "http://${x}"`, "plain string"},
		{"a reference for a string", `url = "http://127.0.0.1:8080"`, `url = var.url`, "plain string"},
		{"missing envs", "env \"prod\" {\n  key = \"keys/prod.key\"\n}\n", ``, "names no env, so nothing can be planned"},
		{"duplicate key", `listen = "127.0.0.1:8080"`, "listen = \"127.0.0.1:8080\"\nlisten = \"127.0.0.1:8081\"", "attribute redefined"},
		{"bad duration", "", `plan_ttl = "a day"`, "must be a duration"},
		{"bad size", "", `limits { body = "16MB" }`, "must be a size"},
		{"bad role", key, env(`plan_role = "arn:aws:iam::1:role/p"`), "not an IAM role's ARN"},
		{"s3 endpoint over http", key, env(`s3_endpoints = ["http://s3.example.com"]`), `s3_endpoints lists "http://s3.example.com", which is not https://HOST, or http:// on a loopback address`},
		{"s3 endpoint with a query", key, env(`s3_endpoints = ["https://s3.example.com/?x=1"]`), "which is not https://HOST"},
		{"s3 endpoints not a list", key, env(`s3_endpoints = "https://s3.example.com"`), "s3_endpoints must be a list"},
		{"role without aws", key, env(`plan_role = "arn:aws:iam::111111111111:role/p"`), "needs aws"},
		{"role without provider_uids", key, env(`plan_role = "arn:aws:iam::111111111111:role/p"`), "needs provider_uids"},
		{"aws without region", "", `aws {}`, "aws needs region"},
		{"bad region", "", `aws { region = "Europe" }`, "not a region"},
		{"sts over http", "", "aws {\n  region = \"eu-west-1\"\n  sts_endpoint = \"http://sts.example.com\"\n}", "sts_endpoint must be https"},
		{"short session", "", "aws {\n  region = \"eu-west-1\"\n  session = \"5m\"\n}", "from 15m to 12h"},
		{"provider_env AWS name", key, env(`provider_env = { AWS_ACCESS_KEY_ID = "/k" }`), "starts with AWS_"},
		{"provider_env HOME", key, env(`provider_env = { HOME = "/k" }`), "own directory"},
		{"provider_env TMPDIR", key, env(`provider_env = { TMPDIR = "/k" }`), "own directory"},
		{"provider_env WRIT_ name", key, env(`provider_env = { WRIT_DATA = "/k" }`), "starts with WRIT_"},
		{"provider_env PLUGIN_ name", key, env(`provider_env = { PLUGIN_PROTOCOL_VERSIONS = "/k" }`), "is a plugin setting"},
		{"bad uid range", "", `provider_uids = "61000"`, "provider_uids must be a range"},
		{"system uids", "", `provider_uids = "0-63"`, "provider_uids must be a range"},
		{"too few uids", "", "provider_uids = \"61000-61003\"", "fewer than limits.plans"},
		{"more than 65536 uids", "", `provider_uids = "61000-126536"`, "provider_uids must be a range"},
		{"uids past 2^31", "", `provider_uids = "2147483000-2147483648"`, "provider_uids must be a range"},
		{"a watch block", key, env("watch {}"), "env prod has no field watch"},
		{"a notify block", "", `notify { url_file = "/u" }`, "has no field notify"},
		{"an approval rule with no need", "", approval(`risks = ["create"]`), "needs need"},
		{"an approval rule that needs none", "", approval("risks = [\"create\"]\n  need = 0"), "need must be a whole number from 1 to 10"},
		{"an approval rule that needs too many", "", approval("risks = [\"create\"]\n  need = 11"), "need must be a whole number from 1 to 10"},
		{"an approval rule that needs a fraction", "", approval("risks = [\"create\"]\n  need = 1.5"), "need must be a whole number from 1 to 10"},
		{"an approval rule that needs a bool", "", approval("risks = [\"create\"]\n  need = true"), "need must be a whole number from 1 to 10"},
		{"an approval rule of an unknown risk", "", approval("risks = [\"destroy\"]\n  need = 1"), "risk class \"destroy\""},
		{"an approval rule of an unknown env", "", "approval {\n  stacks = [\"*\"]\n  envs = [\"qa\"]\n  risks = [\"create\"]\n  need = 1\n}", "names env qa"},
		{"an approval rule with a bad glob", "", "approval {\n  stacks = [\"[\"]\n  envs = [\"prod\"]\n  risks = [\"create\"]\n  need = 1\n}", "is not a glob"},
		{"an approval rule with ** inside a part", "", "approval {\n  stacks = [\"data/pg**\"]\n  envs = [\"prod\"]\n  risks = [\"create\"]\n  need = 1\n}", `approval rule 1: stack pattern "data/pg**" has ** inside the part "pg**"`},
		{"override to a CI team", `can = ["plan"]`, `can = ["plan", "override"]`, "gives override to team mr"},
		{"adopt to a CI team", `can = ["plan"]`, `can = ["plan", "adopt"]`, "gives adopt to team mr, whose issuer gitlab is a CI issuer"},
		{"trusted proxy not a CIDR", "", `trusted_proxies = ["10.0.0.1"]`, "must be a CIDR"},
		{"trusted proxy with host bits", "", `trusted_proxies = ["10.0.0.1/8"]`, "write 10.0.0.0/8"},
		{"trusted proxy in IPv6 form", "", `trusted_proxies = ["::ffff:10.0.0.0/104"]`, "write it as IPv4"},
		{"address limit of zero", "", `limits { requests_per_address = 0 }`, "limits.requests_per_address must be a whole number"},
		{"address limit of a negative number", "", `limits { requests_per_address = -1 }`, "limits.requests_per_address must be a whole number"},
		{"plan timeout not a duration", "", `limits { plan_timeout = "soon" }`, "limits.plan_timeout must be a duration"},
		{"apply timeout not a duration", "", `limits { apply_timeout = 4 }`, "limits.apply_timeout must be a plain string"},
		{"approve to a team of job claims", `can = ["plan"]`, `can = ["plan", "approve"]`, "matches only claims of the job"},
		{"a repo with no forge", "", "repo {\n  name = \"infra\"\n  forge = \"gitlab\"\n  project = \"acme/infra\"\n}", "the config names no forge"},
		{"tofu without a binary", "", "tofu {\n  plugin_cache = \"/var/lib/writ-tofu/plugins\"\n  scratch_dir = \"/var/lib/writ-tofu/runs\"\n}", "tofu needs binary"},
		{"tofu without provider_uids", "", "tofu {\n  binary = \"tofu\"\n  plugin_cache = \"/var/lib/writ-tofu/plugins\"\n  scratch_dir = \"/var/lib/writ-tofu/runs\"\n}", "tofu needs provider_uids: a stack runs the code of any commit"},
		{"tofu with one directory for both", "", "tofu {\n  binary = \"tofu\"\n  plugin_cache = \"/t\"\n  scratch_dir = \"/t\"\n}", "must be two directories"},
		{"tofu plugin cache inside data", "", "provider_uids = \"61000-61063\"\ntofu {\n  binary = \"tofu\"\n  plugin_cache = \"/var/lib/writ/tofu\"\n  scratch_dir = \"/var/lib/writ-runs\"\n}", "tofu.plugin_cache /var/lib/writ/tofu is inside data"},
		{"drift without every", "", "provider_uids = \"61000-61063\"\ntofu {\n  binary = \"tofu\"\n  plugin_cache = \"/var/lib/writ-tofu/plugins\"\n  scratch_dir = \"/var/lib/writ-tofu/runs\"\n}\ndrift {}", "drift needs every"},
		{"drift every under a minute", "", "provider_uids = \"61000-61063\"\ntofu {\n  binary = \"tofu\"\n  plugin_cache = \"/var/lib/writ-tofu/plugins\"\n  scratch_dir = \"/var/lib/writ-tofu/runs\"\n}\ndrift { every = \"30s\" }", "drift.every must be at least 1m"},
		{"drift without tofu", "", "drift { every = \"6h\" }", "drift checks plan OpenTofu stacks, and this config has no tofu block"},
		{"drift every empty", "", tofuBlock + "drift { every = \"\" }", "drift.every is empty: set how often each stack is checked"},
		{"drift every zero", "", tofuBlock + "drift { every = \"0s\" }", "drift.every must be a duration"},
		{"drift with one plan slot", "", tofuBlock + "limits { plans = 1 }\ndrift { every = \"6h\" }", "drift checks need limits.plans of at least 2, and it is 1"},
		{"provider_env of another TF_ name", key, env(`provider_env = { TF_CLI_ARGS = "/k" }`), "is a plugin setting"},
		{"a forge of another kind", "", "forge \"corp\" {\n  kind = \"gitea\"\n  url = \"https://git.example.com\"\n  read_token_file = \"t\"\n}", `forge corp: kind "gitea" is not gitlab`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := edited(t, tt.old, tt.new)
			_, diags := ParseConfig("/etc/writ/serve", []byte(src))
			if !diags.HasErrors() || !strings.Contains(diagText(diags), tt.want) {
				t.Fatalf("got %v, want %q", diags, tt.want)
			}
			for _, d := range diags {
				if d.Severity == diag.Error && (d.Span.File != "/etc/writ/serve" || d.Span.Start.Line == 0) {
					t.Errorf("error without a file and line: %s at %s", d.Message, d.Span)
				}
			}
		})
	}
}

// An error points at the line and column of what is wrong, deep in the
// file, as writ serve --check prints it.
func TestParseConfigErrorPosition(t *testing.T) {
	tests := []struct {
		name, old, new, at, want string
	}{
		{"a value", `can = ["plan"]`, `can = ["deploy"]`, `["deploy"]`, "not one of read"},
		{"a block's name", `env "prod" {`, `env "Prod" {`, `"Prod"`, `environment name "Prod"`},
		{"a value in an object", `project_id = ["1"]`, "project_id = [\"1\"]\n    \"ref\" = []", `[]`, "match.ref of team mr lists no value"},
		{"a syntax error", `teams = ["mr"]`, `teams = ["mr"] @`, `@`, "invalid character"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := edited(t, tt.old, tt.new)
			_, diags := ParseConfig("/etc/writ/serve", []byte(src))
			i := slices.IndexFunc(diags, func(d diag.Diagnostic) bool { return strings.Contains(d.Message, tt.want) })
			if i < 0 {
				t.Fatalf("no error %q in %v", tt.want, diags)
			}
			off := strings.Index(src, tt.new)
			if tt.at != "" {
				off += strings.Index(tt.new, tt.at)
			}
			line := strings.Count(src[:off], "\n") + 1
			if got := diags[i].Span; got.File != "/etc/writ/serve" || got.Start.Line != line {
				t.Fatalf("%q at %s, want line %d", diags[i].Message, got, line)
			}
			if tt.at != "" {
				col := off - strings.LastIndex(src[:off], "\n")
				if got := diags[i].Span.Start.Col; got != col {
					t.Fatalf("%q at column %d, want %d", diags[i].Message, got, col)
				}
			}
		})
	}
}

func TestParseConfigWarnings(t *testing.T) {
	src := strings.Replace(minimal, `key = "keys/prod.key"`, "key = \"keys/prod.key\"\n  apply_role = \"arn:aws:iam::111111111111:role/writ-apply\"", 1)
	src += "aws { region = \"eu-west-1\" }\nprovider_uids = \"61000-61063\"\n"
	src = strings.Replace(src, `match = { project_id = ["1"] }`, `match = { project_path = ["acme/infra"] }`, 1)
	src = strings.Replace(src, `can = ["plan"]`, `can = ["plan", "apply"]`, 1)
	cfg, diags := ParseConfig("/etc/writ/serve", []byte(src))
	if diags.HasErrors() {
		t.Fatal(diags)
	}
	text := diagText(diags)
	for _, want := range []string{"without project_id", "matches no ref"} {
		if !strings.Contains(text, want) {
			t.Errorf("no warning %q in %s", want, text)
		}
	}
	if strings.Contains(text, "no effect yet: writ serve does not apply") || strings.Contains(text, "approvals has no effect") {
		t.Errorf("a config is warned that applies are not built: %s", text)
	}
	if strings.Contains(text, "set provider_uids") {
		t.Errorf("a config with provider_uids is warned to set them: %s", text)
	}
	_, diags = ParseConfig("/etc/writ/serve", []byte(minimal))
	if !strings.Contains(diagText(diags), "providers run as writ's own user") {
		t.Errorf("a config without provider_uids is not warned: %v", diags)
	}
	if !strings.Contains(diagText(diags), "trusted_proxies is not set") {
		t.Errorf("a config behind a proxy without trusted_proxies is not warned: %v", diags)
	}
	src = minimal + `trusted_proxies = ["127.0.0.1/32", "fd00::/8"]` + "\n"
	cfg2, diags := ParseConfig("/etc/writ/serve", []byte(src))
	if diags.HasErrors() || strings.Contains(diagText(diags), "trusted_proxies is not set") {
		t.Errorf("a config with trusted_proxies: %v", diags)
	}
	if fmt.Sprint(cfg2.TrustedProxies) != "[127.0.0.1/32 fd00::/8]" || cfg2.Limits.RequestsPerAddress != DefaultRequestsPerAddress {
		t.Errorf("trusted_proxies %v, requests_per_address %d", cfg2.TrustedProxies, cfg2.Limits.RequestsPerAddress)
	}
	if l := cfg2.Limits; l.PlanTimeout != 30*time.Minute || l.ApplyStartTimeout != 10*time.Minute || l.ApplyTimeout != 4*time.Hour {
		t.Errorf("time limits %s %s %s, want 30m 10m 4h by default", l.PlanTimeout, l.ApplyStartTimeout, l.ApplyTimeout)
	}
	if cfg.Envs["prod"].Key != "/etc/writ/keys/prod.key" {
		t.Errorf("a relative key path is %s, want it under the config's directory", cfg.Envs["prod"].Key)
	}
}

// HCL's other spellings read the same: comments of each kind, a quoted
// key, and a key set with a colon.
func TestParseConfigHCLForms(t *testing.T) {
	src := strings.Replace(minimal, `match = { project_id = ["1"] }`, `match = {
    # a comment
    "project_id" = ["1"] // another
    user_login: ["alice"] /* and a third */
  }`, 1)
	src = strings.Replace(src, `key = "keys/prod.key"`, "key = \"keys/prod.key\"\n  provider_env = { \"DB_PASSWORD\" = \"secrets/db\" }", 1)
	cfg, diags := ParseConfig("/etc/writ/serve", []byte(src))
	if diags.HasErrors() {
		t.Fatal(diags)
	}
	if m := cfg.Policy.Teams[0].Match; len(m) != 2 || m[0].Claim != "project_id" || m[1].Claim != "user_login" || m[1].Values[0] != "alice" {
		t.Fatalf("match %+v", m)
	}
	if e := cfg.Envs["prod"]; e.Key != "/etc/writ/keys/prod.key" || e.ProviderEnv["DB_PASSWORD"] != "/etc/writ/secrets/db" {
		t.Fatalf("env %+v", e)
	}
}

// Repositories parse, and are not warned about as not built.
func TestParseConfigRepos(t *testing.T) {
	src := minimal + `forge "gitlab" {
  kind = "gitlab"
  url = "https://gitlab.com"
  read_token_file = "secrets/gitlab_read"
}
repo {
  name = "infra"
  forge = "gitlab"
  project = "acme/infra"
  path = "infra"
}
repo {
  name = "apps"
  forge = "gitlab.com"
  project = "acme/apps"
}
poll_every = "2m"
`
	cfg, diags := ParseConfig("/etc/writ/serve", []byte(src))
	if diags.HasErrors() {
		t.Fatal(diags)
	}
	if text := diagText(diags); strings.Contains(text, "has no effect yet") || strings.Contains(text, "not built") {
		t.Fatalf("warned of a built stage: %s", text)
	}
	if len(cfg.Forges) != 1 || cfg.Forges[0].ReadTokenFile != "/etc/writ/secrets/gitlab_read" || len(cfg.Repos) != 2 || cfg.Repos[1].Name != "apps" || !cfg.PinRepos || cfg.PollEvery != 2*time.Minute {
		t.Fatalf("forges %+v, repos %+v, pinned %v, poll_every %s", cfg.Forges, cfg.Repos, cfg.PinRepos, cfg.PollEvery)
	}
	src += "repo {\n  name = \"infra\"\n  forge = \"gitlab\"\n  project = \"acme/other\"\n}\n"
	if _, diags := ParseConfig("/etc/writ/serve", []byte(src)); !strings.Contains(diagText(diags), "repos names infra twice") {
		t.Fatalf("a repository named twice: %v", diags)
	}
}

// The dev config takes forge and repo blocks, which may name a local
// directory, poll_every, and a tofu block, and nothing else.
func TestParseDevConfig(t *testing.T) {
	src := `forge "gitlab" {
  kind = "gitlab"
  url = "https://gitlab.com"
  read_token_file = "secrets/gitlab_read"
}
repo {
  name = "infra"
  forge = "gitlab"
  project = "acme/infra"
}
repo {
  name = "local"
  forge = "dir"
  project = "/srv/git/local"
}
poll_every = "5s"
tofu {
  binary = "/opt/tofu/bin/tofu"
  plugin_cache = "plugins"
  scratch_dir = "/var/tmp/writ-runs"
  git = "bin/git"
}
`
	cfg, diags := ParseDevConfig("/home/u/.config/writ/dev", []byte(src))
	if diags.HasErrors() {
		t.Fatal(diags)
	}
	if len(cfg.Forges) != 1 || cfg.Forges[0].ReadTokenFile != "/home/u/.config/writ/secrets/gitlab_read" || len(cfg.Repos) != 2 || cfg.Repos[1].Forge != "dir" || !cfg.PinRepos || cfg.PollEvery != 5*time.Second || cfg.Text != src {
		t.Fatalf("dev config %+v", cfg)
	}
	if want := (Tofu{Binary: "/opt/tofu/bin/tofu", PluginCache: "/home/u/.config/writ/plugins", ScratchDir: "/var/tmp/writ-runs", Git: "/home/u/.config/writ/bin/git"}); cfg.Tofu == nil || *cfg.Tofu != want {
		t.Fatalf("dev tofu %+v, want %+v", cfg.Tofu, want)
	}
	for _, tt := range []struct{ src, want string }{
		{`listen = "127.0.0.1:1"`, "the dev config has no field listen"},
		{`env "prod" { key = "k" }`, "the dev config has no field env"},
		{"repo {\n  name = \"x\"\n  forge = \"dir\"\n  project = \"rel/path\"\n}", "absolute path"},
		{`poll_every = "10ms"`, "at least 1s"},
	} {
		_, diags := ParseDevConfig("/home/u/.config/writ/dev", []byte(tt.src))
		if !diags.HasErrors() || !strings.Contains(diagText(diags), tt.want) {
			t.Errorf("%s: got %v, want %q", tt.src, diags, tt.want)
		}
	}
}

// The dev config declares the team its grant names, so a plan's page and
// GET /v1/config name the team that may approve and apply.
func TestDevConfigDeclaresItsTeam(t *testing.T) {
	cfg := DevConfig(DevOptions{URL: "http://127.0.0.1:1", Data: t.TempDir()})
	for _, op := range []authz.Op{authz.Approve, authz.Apply} {
		if got := cfg.Policy.TeamsWith(op, "net/core/vpc", "staging", authz.Risks); !slices.Equal(got, []string{DevTeam}) {
			t.Errorf("the teams that may %s every risk class: %v, want [%s]", op, got, DevTeam)
		}
	}
}

// writ serve --check warns when issuers of two identities, or a forge
// named as one of them, have URLs that look like one host, whatever the
// spelling, and stays quiet when they share an identity.
func TestParseConfigIdentityHosts(t *testing.T) {
	people := func(identity, url string) string {
		return fmt.Sprintf("issuer \"sso\" {\n  kind = \"people\"\n  identity = %q\n  url = %q\n  client_id = \"c\"\n  client_secret_file = \"s\"\n}\n", identity, url)
	}
	ci := func(name, url string) string {
		return fmt.Sprintf("issuer %q {\n  kind = \"gitlab-ci\"\n  identity = %q\n  url = %q\n}\n", name, name, url)
	}
	forge := func(url string) string {
		return fmt.Sprintf("forge \"corp\" {\n  kind = \"gitlab\"\n  url = %q\n  read_token_file = \"t\"\n}\n", url)
	}
	tests := []struct {
		name, issuer, top, want string
	}{
		{"case and port", people("corp", "https://GITLAB.com:0443"), "", "issuer gitlab (identity gitlab) and issuer sso (identity corp) look like one host, gitlab.com"},
		{"trailing dot", people("corp", "https://gitlab.com."), "", "look like one host, gitlab.com"},
		{"forge of the identity", people("corp", "https://sso.example.com"), forge("https://gitlab.com"),
			"issuer gitlab (identity gitlab) and forge corp (identity corp) look like one host, gitlab.com"},
		{"idn", ci("other", "https://bücher.example") + people("corp", "https://sso.example.com"),
			forge("https://xn--bcher-kva.example"), "issuer other (identity other) and forge corp (identity corp) look like one host, xn--bcher-kva.example"},
		{"distinct hosts", people("corp", "https://sso.example.com"), forge("https://code.example.com"), ""},
		{"ipv6", ci("v6", "https://[2001:db8::1]") + ci("w6", "https://[2001:0db8:0000:0000:0000:0000:0000:0001]:443"), "", "look like one host, 2001:db8::1"},
		{"one identity", people("gitlab", "https://GITLAB.com:443"), "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, diags := ParseConfig("/etc/writ/serve", []byte(minimal+tt.issuer+tt.top))
			if diags.HasErrors() {
				t.Fatal(diags)
			}
			text := diagText(diags)
			if tt.want == "" && strings.Contains(text, "look like one host") || tt.want != "" && !strings.Contains(text, tt.want) {
				t.Errorf("warnings:\n%s\nwant %q", text, tt.want)
			}
		})
	}
}

func diagText(l diag.List) string {
	var b strings.Builder
	for _, d := range l {
		b.WriteString(d.Message + "\n")
	}
	return b.String()
}

func TestCheckStackName(t *testing.T) {
	for _, bad := range []string{"", ".", "..", "a/..", "/a", "a//b", "-x/../y", strings.Repeat("a", 129)} {
		if CheckStackName(bad) == nil {
			t.Errorf("accepted stack name %q", bad)
		}
	}
	for _, good := range []string{"network", "data/pg", "e2e.serve_1"} {
		if err := CheckStackName(good); err != nil {
			t.Error(err)
		}
	}
}

func TestParseConfigPeople(t *testing.T) {
	src := minimal + `issuer "sso" {
  kind = "people"
  identity = "gitlab"
  url = "https://gitlab.com"
  client_id = "8f1c0b7e4d2a"
  client_secret_file = "secrets/oidc"
  groups = "userinfo"
}
session_ttl = "8h"
`
	cfg, diags := ParseConfig("/etc/writ/serve", []byte(src))
	if diags.HasErrors() {
		t.Fatal(diags)
	}
	sso := cfg.Issuers[1]
	if sso.Kind != "people" || sso.ClientID != "8f1c0b7e4d2a" || sso.ClientSecretFile != "/etc/writ/secrets/oidc" || !sso.GroupsFromUserinfo {
		t.Fatalf("issuer %+v", sso)
	}
	if !cfg.UI || cfg.SessionTTL != 8*time.Hour {
		t.Fatalf("ui %v, session_ttl %s", cfg.UI, cfg.SessionTTL)
	}
	off, diags := ParseConfig("/etc/writ/serve", []byte(strings.Replace(src, `session_ttl = "8h"`, `ui { enabled = false }`, 1)))
	if diags.HasErrors() || off.UI || off.SessionTTL != DefaultSessionTTL {
		t.Fatalf("ui %v, session_ttl %s, %v", off.UI, off.SessionTTL, diags)
	}
}

// The config sample in docs/config.md parses with no error and no warning.
func TestParseConfigTofu(t *testing.T) {
	src := edited(t, "", "provider_uids = \"61000-61063\"\ntofu {\n  binary = \"tofu\"\n  plugin_cache = \"/var/lib/writ-tofu/plugins\"\n  scratch_dir = \"runs\"\n  git = \"git\"\n}\ndrift {\n  every = \"6h\"\n}")
	src = strings.Replace(src, `key = "keys/prod.key"`, "key = \"keys/prod.key\"\n  provider_env = { TF_VAR_region = \"/run/writ/region\" }", 1)
	cfg, diags := ParseConfig("/etc/writ/serve", []byte(src))
	if diags.HasErrors() {
		t.Fatal(diags)
	}
	want := Tofu{Binary: "tofu", PluginCache: "/var/lib/writ-tofu/plugins", ScratchDir: "/etc/writ/runs", Git: "git"}
	if cfg.Tofu == nil || *cfg.Tofu != want {
		t.Fatalf("tofu %+v, want %+v", cfg.Tofu, want)
	}
	if cfg.DriftEvery != 6*time.Hour {
		t.Fatalf("drift.every = %s, want 6h", cfg.DriftEvery)
	}
	if got := cfg.Envs["prod"].ProviderEnv["TF_VAR_region"]; got != "/run/writ/region" {
		t.Fatalf("provider_env TF_VAR_region = %q: a variable of an OpenTofu stack comes from provider_env", got)
	}
}

// egress_mode is off unless the config sets it, and an env's egress list
// is kept as written and parsed.
func TestParseConfigEgress(t *testing.T) {
	const tofuBlock = "provider_uids = \"61000-61063\"\ntofu {\n  binary = \"tofu\"\n  plugin_cache = \"/var/lib/writ-tofu/plugins\"\n  scratch_dir = \"/var/lib/writ-tofu/runs\"\n}\n"
	cfg, diags := ParseConfig("/etc/writ/serve", []byte(edited(t, "", tofuBlock)))
	if diags.HasErrors() || cfg.EgressMode != egress.Off {
		t.Fatalf("egress_mode %q by default, %v; want off", cfg.EgressMode, diags)
	}
	src := edited(t, "", tofuBlock+"egress_mode = \"enforce\"")
	src = strings.Replace(src, `key = "keys/prod.key"`, "key = \"keys/prod.key\"\n  egress = [\"*.amazonaws.com\", \"nomad.example.internal:80\"]", 1)
	cfg, diags = ParseConfig("/etc/writ/serve", []byte(src))
	if diags.HasErrors() || len(diags) > 1 {
		t.Fatal(diags)
	}
	prod := cfg.Envs["prod"]
	if cfg.EgressMode != egress.Enforce || !slices.Equal(prod.Egress, []string{"*.amazonaws.com", "nomad.example.internal:80"}) {
		t.Fatalf("egress_mode %q, egress of prod %q", cfg.EgressMode, prod.Egress)
	}
	if _, err := egress.ParseList(prod.Egress); err != nil {
		t.Fatal(err)
	}

	// A list with egress_mode off has no effect, which --check says.
	src = strings.Replace(edited(t, "", tofuBlock), `key = "keys/prod.key"`, "key = \"keys/prod.key\"\n  egress = [\"*.amazonaws.com\"]", 1)
	_, diags = ParseConfig("/etc/writ/serve", []byte(src))
	if diags.HasErrors() || !strings.Contains(diagText(diags), "env prod sets egress, and egress_mode is off, so its runs keep the host's network") {
		t.Fatalf("a list under egress_mode off: %v", diags)
	}
}

func TestDocsConfigSample(t *testing.T) {
	doc, err := os.ReadFile("../../docs/config.md")
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(doc), "## Sample config\n")
	if !ok {
		t.Fatal("docs/config.md has no section Sample config")
	}
	_, rest, ok = strings.Cut(rest, "```hcl\n")
	sample, _, closed := strings.Cut(rest, "```\n")
	if !ok || !closed {
		t.Fatal("the section has no hcl block")
	}
	_, diags := ParseConfig("/etc/writ/serve", []byte(sample))
	if len(diags) != 0 {
		t.Fatal(diags)
	}
}
