package server

import (
	"maps"
	"net"
	"net/netip"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/mr-karan/writ/internal/authz"
	"github.com/mr-karan/writ/internal/awsrole"
	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/diag"
	"github.com/mr-karan/writ/internal/egress"
	"github.com/mr-karan/writ/internal/oidc"
	"github.com/mr-karan/writ/internal/policy"
)

var (
	nameRE   = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)
	envVarRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	regionRE = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]$`)
)

// ParseConfig reads a config file, written in HCL. Relative paths in it
// are relative to the file's directory. Warnings come back with the
// config; errors mean it is nil.
func ParseConfig(file string, src []byte) (*Config, diag.List) {
	b, diags := parseHCL(file, src)
	if diags.HasErrors() {
		return nil, diags
	}
	d := &decoder{dir: filepath.Dir(file), src: src, diags: diags}
	cfg := d.config(b)
	if d.diags.HasErrors() {
		return nil, d.diags
	}
	cfg.Text = string(src)
	return cfg, d.diags
}

func (d *decoder) config(b *hclsyntax.Body) *Config {
	cfg := &Config{
		PlanTTL: DefaultPlanTTL, SessionTTL: DefaultSessionTTL, PollEvery: DefaultPollEvery,
		Limits: Limits{RequestsPerSecond: DefaultRequestsPerSecond, RequestsPerAddress: DefaultRequestsPerAddress, PlansPerPrincipal: DefaultPlansPerPrincipal, Plans: DefaultPlans, Applies: DefaultApplies, Body: DefaultBody,
			PlanTimeout: DefaultPlanTimeout, ApplyStartTimeout: DefaultApplyStartTimeout, ApplyTimeout: DefaultApplyTimeout},
		Envs: map[string]Env{},
	}
	s := shape{
		attrs:  []string{"url", "listen", "database", "data", "provider_uids", "plan_ttl", "session_ttl", "poll_every", "poll_every_with_hook", "trusted_proxies", "egress_mode", "provider_registries"},
		blocks: []string{"tls", "limits", "retention", "ui", "aws", "tofu", "drift", "policy"},
		lists:  []string{"grant", "approval", "repo"},
		named:  []string{"issuer", "team", "env", "forge"},
		removed: map[string]string{
			"providers_dir": "OpenTofu installs the providers each stack's .terraform.lock.hcl pins into tofu.plugin_cache",
			"provider":      "a stack runs the providers its .terraform.lock.hcl pins, and a policy can restrict them",
		},
	}
	top, many := d.body(b, "the config", s)
	need := func(name string) (field, bool) {
		f, ok := top[name]
		if !ok {
			d.errorf(b, "the config needs %s", name)
		}
		return f, ok
	}
	if f, ok := need("url"); ok {
		cfg.URL = d.serverURL(f.value)
	}
	if f, ok := need("listen"); ok {
		cfg.Listen = d.str(f.value, "listen")
		if _, _, err := net.SplitHostPort(cfg.Listen); err != nil {
			d.errorf(f.value, "listen must be host:port, such as \"0.0.0.0:443\"")
		}
	}
	if f, ok := top["tls"]; ok {
		r := d.attrs(f.block(), "tls", "cert", "key")
		if c, ok := r["cert"]; ok {
			cfg.TLS = &TLSFiles{Cert: d.path(c.value, "tls.cert")}
		}
		if k, ok := r["key"]; ok && cfg.TLS != nil {
			cfg.TLS.Key = d.path(k.value, "tls.key")
		}
		if cfg.TLS == nil || cfg.TLS.Key == "" {
			d.errorf(f.value, "tls needs cert and key")
		}
	} else if f, ok := top["listen"]; ok && !loopback(cfg.Listen) {
		d.errorf(f.value, "listen %s is not a loopback address, so the config needs tls; only a loopback address may serve plain HTTP, behind a proxy that ends TLS", cfg.Listen)
	}
	if f, ok := need("database"); ok {
		cfg.Database = d.str(f.value, "database")
		if err := control.CheckURL(cfg.Database); err != nil {
			d.errorf(f.value, "database: %v", err)
		}
	}
	cfg.Data = "/var/lib/writ"
	if f, ok := top["data"]; ok {
		cfg.Data = d.path(f.value, "data")
	}
	if f, ok := top["plan_ttl"]; ok {
		cfg.PlanTTL = d.duration(f.value, "plan_ttl")
	}
	if f, ok := top["session_ttl"]; ok {
		cfg.SessionTTL = d.duration(f.value, "session_ttl")
	}
	if f, ok := top["limits"]; ok {
		r, _ := d.body(f.block().Body, "limits", shape{
			attrs:   []string{"requests_per_second", "requests_per_address", "plans_per_principal", "plans", "applies", "body", "plan_timeout", "apply_start_timeout", "apply_timeout"},
			removed: map[string]string{"files": "it bounded the source files of a bundle of the Writ language, which writ serve no longer reads"},
		})
		for name, lf := range r {
			switch name {
			case "requests_per_second":
				cfg.Limits.RequestsPerSecond = d.integer(lf.value, "limits."+name)
			case "requests_per_address":
				cfg.Limits.RequestsPerAddress = d.integer(lf.value, "limits."+name)
			case "plans_per_principal":
				cfg.Limits.PlansPerPrincipal = d.integer(lf.value, "limits."+name)
			case "plans":
				cfg.Limits.Plans = d.integer(lf.value, "limits."+name)
			case "applies":
				cfg.Limits.Applies = d.integer(lf.value, "limits."+name)
			case "body":
				cfg.Limits.Body = d.size(lf.value, "limits.body")
			case "plan_timeout":
				cfg.Limits.PlanTimeout = d.duration(lf.value, "limits."+name)
			case "apply_start_timeout":
				cfg.Limits.ApplyStartTimeout = d.duration(lf.value, "limits."+name)
			case "apply_timeout":
				cfg.Limits.ApplyTimeout = d.duration(lf.value, "limits."+name)
			}
		}
	}
	if f, ok := top["trusted_proxies"]; ok {
		cfg.TrustedProxies = d.prefixes(f.value, "trusted_proxies")
	}
	if f, ok := top["retention"]; ok {
		r := d.attrs(f.block(), "retention", "audit", "plans")
		if a, ok := r["audit"]; ok {
			cfg.Retention.Audit = d.duration(a.value, "retention.audit")
		}
		if p, ok := r["plans"]; ok {
			cfg.Retention.Plans = d.duration(p.value, "retention.plans")
		}
	}
	d.issuers(b, many["issuer"], cfg)
	d.ui(top, cfg)
	if len(many["team"]) == 0 {
		d.errorf(b, "the config needs a team \"NAME\" { ... } block")
	}
	d.teams(many["team"], cfg)
	d.envs(b, many["env"], cfg)
	if len(many["grant"]) == 0 {
		d.errorf(b, "the config needs a grant { ... } block")
	}
	d.grants(many["grant"], cfg)
	d.approvals(many["approval"], cfg)
	if f, ok := top["aws"]; ok {
		cfg.AWS = d.aws(f.block())
	}
	if f, ok := top["provider_uids"]; ok {
		cfg.ProviderUIDs = d.uidRange(f.value)
		if r := cfg.ProviderUIDs; r != nil && r.Count() < cfg.Limits.Plans {
			d.errorf(f.value, "provider_uids holds %d uids, fewer than limits.plans, %d: each running plan needs its own", r.Count(), cfg.Limits.Plans)
		}
	}
	if f, ok := top["policy"]; ok {
		cfg.Policies = d.policies(f.block())
	}
	if f, ok := top["tofu"]; ok {
		cfg.Tofu = d.tofu(f.block())
		if _, ok := top["provider_uids"]; !ok {
			d.errorf(f.key, "tofu needs provider_uids: a stack runs the code of any commit writ serve plans, and OpenTofu and its providers running as writ's own user could read the state keys, the database, and the tokens")
		}
		if t := cfg.Tofu; t != nil && cfg.ProviderUIDs != nil {
			for _, what := range []string{"tofu.plugin_cache", "tofu.scratch_dir"} {
				dir := t.PluginCache
				if what == "tofu.scratch_dir" {
					dir = t.ScratchDir
				}
				if rel, err := filepath.Rel(cfg.Data, dir); err == nil && !strings.HasPrefix(rel, "..") {
					d.errorf(f.value, "%s %s is inside data %s, which only writ may read; with provider_uids, OpenTofu runs as other users, so set it outside data", what, dir, cfg.Data)
				}
			}
		}
	}
	if f, ok := top["drift"]; ok {
		r := d.attrs(f.block(), "drift", "every")
		e, ok := r["every"]
		switch {
		case !ok:
			d.errorf(f.key, "drift needs every, how often each stack is checked, such as \"6h\"")
		case isEmptyString(e.value):
			d.errorf(e.value, "drift.every is empty: set how often each stack is checked, such as \"6h\", or remove the drift block for no drift checks")
		default:
			if cfg.DriftEvery = d.duration(e.value, "drift.every"); cfg.DriftEvery != 0 && cfg.DriftEvery < minDriftEvery {
				d.errorf(e.value, "drift.every must be at least 1m: each check reads every resource of a stack through its providers")
			}
		}
		if _, ok := top["tofu"]; !ok {
			d.errorf(f.key, "drift checks plan OpenTofu stacks, and this config has no tofu block to run OpenTofu with")
		}
		if cfg.Limits.Plans < 2 {
			d.errorf(f.key, "drift checks need limits.plans of at least 2, and it is %d: a drift check never takes the last free plan slot, which stays for people's plans", cfg.Limits.Plans)
		}
	}
	d.egress(top, cfg)
	d.roles(b, top, cfg)
	d.checkPeopleOps(many["grant"], cfg)
	d.repoParts(top, many, cfg, false)
	d.lint(cfg)
	return cfg
}

// prefixes reads a list of CIDRs, such as "10.0.0.0/8".
func (d *decoder) prefixes(e hclsyntax.Node, what string) []netip.Prefix {
	var out []netip.Prefix
	for _, x := range d.list(e, what) {
		s := d.str(x, what+" element")
		p, err := netip.ParsePrefix(s)
		switch {
		case s == "":
		case err != nil:
			d.errorf(x, "%s element must be a CIDR such as \"10.0.0.0/8\" or \"127.0.0.1/32\", got %q", what, s)
		case p.Addr().Is4In6():
			d.errorf(x, "%s element %s is an IPv4 range in IPv6 form: write it as IPv4", what, s)
		case p != p.Masked():
			d.errorf(x, "%s element %s sets bits past its prefix length: write %s", what, s, p.Masked())
		default:
			out = append(out, p)
		}
	}
	return out
}

func loopback(hostPort string) bool {
	host, _, err := net.SplitHostPort(hostPort)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// serverURL reads url: HTTPS, or HTTP on a loopback host for a server
// behind a local proxy or in a test.
func (d *decoder) serverURL(e hclsyntax.Node) string {
	s := d.str(e, "url")
	u, err := url.Parse(s)
	ok := err == nil && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && (u.Path == "" || u.Path == "/")
	if ok && u.Scheme == "http" {
		ok = loopback(u.Host) || loopback(u.Host+":80")
	} else if ok {
		ok = u.Scheme == "https"
	}
	if !ok {
		d.errorf(e, "url must be https://HOST with no path, or http:// on a loopback address, got %q", s)
	}
	return strings.TrimSuffix(s, "/")
}

// ui reads the ui block. The control UI is on when a people issuer exists,
// unless ui.enabled is false.
func (d *decoder) ui(top map[string]field, cfg *Config) {
	people := slices.ContainsFunc(cfg.Issuers, func(i Issuer) bool { return i.Kind == oidc.People })
	cfg.UI = people
	f, ok := top["ui"]
	if !ok {
		return
	}
	r := d.attrs(f.block(), "ui", "enabled")
	if en, ok := r["enabled"]; ok {
		b := d.literal(en.value)
		if b != "true" && b != "false" {
			d.errorf(en.value, "ui.enabled must be true or false")
			return
		}
		cfg.UI = b == "true"
		if cfg.UI && !people {
			d.errorf(en.value, "ui.enabled needs an issuer of kind people, which people sign in with")
		}
	}
}

func (d *decoder) envs(top *hclsyntax.Body, bs []*hclsyntax.Block, cfg *Config) {
	for _, blk := range bs {
		name, key := blk.Labels[0], blk.LabelRanges[0]
		if !nameRE.MatchString(name) {
			d.errorf(key, "environment name %q must be lowercase letters, digits, _ and -", name)
		}
		r, _ := d.body(blk.Body, "env "+name, shape{
			attrs: []string{"key", "plan_role", "apply_role", "s3_endpoints", "provider_env", "egress"},
			removed: map[string]string{
				"secret_env": "it gave values to secret_env() of the Writ language; give a provider a secret with provider_env",
				"state":      "a stack's backend block says where its state is",
			},
		})
		env := Env{Name: name}
		if k, ok := r["key"]; ok {
			env.Key = d.path(k.value, "key")
		} else {
			d.errorf(blk, "env %s needs key, the state key file", name)
		}
		role := func(key string) string {
			x, ok := r[key]
			if !ok {
				return ""
			}
			arn := d.str(x.value, key)
			if err := awsrole.CheckRoleARN(arn); err != nil {
				d.errorf(x.value, "env %s: %s: %v", name, key, err)
			}
			return arn
		}
		env.PlanRole, env.ApplyRole = role("plan_role"), role("apply_role")
		if x, ok := r["s3_endpoints"]; ok {
			for _, e := range d.strs(x.value, "s3_endpoints") {
				if !endpointURL(e) {
					d.errorf(x.value, "env %s: s3_endpoints lists %q, which is not https://HOST, or http:// on a loopback address", name, e)
				}
				env.S3Endpoints = append(env.S3Endpoints, e)
			}
		}
		if s, ok := r["provider_env"]; ok {
			env.ProviderEnv = map[string]string{}
			for v, vf := range d.object(s.value, "provider_env of env "+name) {
				if !envVarRE.MatchString(v) {
					d.errorf(vf.key, "provider_env name %q is not an environment variable name", v)
				} else if why := reservedProviderEnv(v); why != "" {
					d.errorf(vf.key, "provider_env name %s %s", v, why)
				}
				env.ProviderEnv[v] = d.path(vf.value, "provider_env."+v)
			}
		}
		if x, ok := r["egress"]; ok {
			env.Egress = d.strs(x.value, "egress of env "+name)
			var err error
			if env.egress, err = egress.ParseList(env.Egress); err != nil {
				d.errorf(x.value, "env %s: %v", name, err)
			}
		}
		cfg.Envs[name] = env
	}
	if len(cfg.Envs) == 0 {
		d.errorf(top, "the config names no env, so nothing can be planned: add an env \"NAME\" { ... } block")
	}
}

// registryRE is a registry's host name as a lock file's provider address
// may start with it.
var registryRE = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*$`)

// egress reads egress_mode, off by default, which only a config that runs
// OpenTofu may turn on, and provider_registries, and checks that each
// env's runs can reach their state from their own network namespace.
func (d *decoder) egress(top map[string]field, cfg *Config) {
	cfg.EgressMode = egress.Off
	cfg.ProviderRegistries = defaultProviderRegistries
	if r, ok := top["provider_registries"]; ok {
		cfg.ProviderRegistries = d.strs(r.value, "provider_registries")
		for _, h := range cfg.ProviderRegistries {
			if !registryRE.MatchString(h) {
				d.errorf(r.value, "provider_registries item %q: it is not a registry's host name, such as registry.opentofu.org, as a lock file's provider address starts with", h)
			}
		}
	}
	f, ok := top["egress_mode"]
	if ok {
		m, err := egress.ParseMode(d.str(f.value, "egress_mode"))
		if err != nil {
			d.errorf(f.value, "%v", err)
			return
		}
		cfg.EgressMode = m
	}
	if !cfg.EgressMode.On() {
		if r, ok := top["provider_registries"]; ok {
			d.warnf(r.value, "provider_registries is set, and egress_mode is off, so a provider install asks any registry a lock file names; set egress_mode to report or enforce")
		}
		return
	}
	if _, ok := top["tofu"]; !ok {
		d.errorf(f.value, "egress_mode applies to the OpenTofu runs, and the config has no tofu block")
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Envs)) {
		for _, e := range cfg.Envs[name].S3Endpoints {
			if u, err := url.Parse(e); err == nil && u.Scheme == "http" {
				d.errorf(f.value, "env %s: s3_endpoints lists %s, a loopback address, which a run cannot reach from its own network namespace under egress_mode %s; list an https endpoint the env's egress allows", name, e, cfg.EgressMode)
			}
		}
	}
}

// reservedProviderEnv says why writ serve keeps a variable name from
// provider_env, or "".
func reservedProviderEnv(name string) string {
	switch {
	case strings.HasPrefix(name, "TF_VAR_"):
		// It sets a variable of an OpenTofu stack.
		return ""
	case strings.HasPrefix(name, "AWS_"):
		return "starts with AWS_: writ serve gives providers AWS credentials from the environment's role"
	case strings.HasPrefix(name, "WRIT_"):
		return "starts with WRIT_, which names writ's own settings"
	case strings.HasPrefix(name, "PLUGIN_"), strings.HasPrefix(name, "TF_"):
		return "is a plugin setting, which writ sets"
	case name == "HOME" || name == "TMPDIR":
		return "names the provider's own directory, which writ sets"
	}
	return ""
}

// endpointURL reports whether s is an AWS endpoint writ serve may send
// a role's signed requests to: https://HOST, or http:// on loopback.
func endpointURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Host != "" && u.User == nil && u.RawQuery == "" && (u.Scheme == "https" || u.Scheme == "http" && (loopback(u.Host) || loopback(u.Host+":80")))
}

// aws reads the aws block: region, sts_endpoint, and session.
func (d *decoder) aws(e *hclsyntax.Block) *AWS {
	a := &AWS{Session: DefaultSession}
	r := d.attrs(e, "aws", "region", "sts_endpoint", "session")
	if f, ok := r["region"]; ok {
		a.Region = d.str(f.value, "aws.region")
		if !regionRE.MatchString(a.Region) {
			d.errorf(f.value, "aws.region %q is not a region such as eu-west-1", a.Region)
		}
	} else {
		d.errorf(e, "aws needs region, the region of STS")
	}
	if f, ok := r["sts_endpoint"]; ok {
		a.STSEndpoint = d.str(f.value, "aws.sts_endpoint")
		if !endpointURL(a.STSEndpoint) {
			d.errorf(f.value, "aws.sts_endpoint must be https://HOST, or http:// on a loopback address, got %q", a.STSEndpoint)
		}
	}
	if f, ok := r["session"]; ok {
		a.Session = d.duration(f.value, "aws.session")
		if a.Session != 0 && (a.Session < awsrole.MinDuration || a.Session > awsrole.MaxDuration) {
			d.errorf(f.value, "aws.session must be from 15m to 12h, got %s", a.Session)
		}
	}
	return a
}

// policies reads the policy block: its bounds, and each set with the
// .rego files of its directory.
func (d *decoder) policies(blk *hclsyntax.Block) *Policies {
	one, many := d.body(blk.Body, "policy", shape{attrs: []string{"timeout", "memory"}, named: []string{"set"}})
	p := &Policies{Timeout: policy.DefaultTimeout, Memory: policy.DefaultMemory}
	if f, ok := one["timeout"]; ok {
		p.Timeout = d.duration(f.value, "policy.timeout")
	}
	if f, ok := one["memory"]; ok {
		if p.Memory = d.size(f.value, "policy.memory"); p.Memory > 0 && p.Memory < 64<<20 {
			d.errorf(f.value, "policy.memory must be at least 64MiB, which the evaluation process needs for itself")
		}
	}
	for _, b := range many["set"] {
		name := b.Labels[0]
		what := "policy set " + name
		if !nameRE.MatchString(name) {
			d.errorf(b.LabelRanges[0], "%s: a set's name is lower case letters, digits, - and _, starting with a letter", what)
		}
		r := d.attrs(b, what, "dir", "level")
		s := PolicySet{Name: name, Level: policy.Mandatory}
		if f, ok := r["level"]; ok {
			s.Level = policy.Level(d.str(f.value, what+" level"))
			if s.Level != policy.Mandatory && s.Level != policy.Advisory {
				d.errorf(f.value, "%s has level %q; use \"mandatory\", whose deny refuses, or \"advisory\", whose deny only warns", what, s.Level)
			}
		}
		f, ok := r["dir"]
		if !ok {
			d.errorf(b, "%s needs dir, the directory of its .rego files", what)
			continue
		}
		if s.Dir = d.path(f.value, what+" dir"); s.Dir == "" {
			continue
		}
		files, err := policy.LoadDir(s.Dir)
		if err != nil {
			d.errorf(f.value, "%s: %v", what, err)
			continue
		}
		s.Files = files
		p.Sets = append(p.Sets, s)
	}
	if len(many["set"]) == 0 {
		d.errorf(blk, "policy needs a set \"NAME\" { dir = \"DIR\" }, the directory of the set's .rego files")
	}
	return p
}

// tofu reads the tofu block. Every attribute but git is required.
func (d *decoder) tofu(e *hclsyntax.Block) *Tofu {
	r := d.attrs(e, "tofu", "binary", "plugin_cache", "scratch_dir", "git")
	t := &Tofu{}
	for _, a := range []struct {
		name string
		to   *string
	}{{"binary", &t.Binary}, {"plugin_cache", &t.PluginCache}, {"scratch_dir", &t.ScratchDir}} {
		name, to := a.name, a.to
		f, ok := r[name]
		if !ok {
			d.errorf(e, "tofu needs %s", name)
			continue
		}
		if name == "binary" && !strings.Contains(d.str(f.value, "tofu.binary"), "/") {
			*to = d.str(f.value, "tofu.binary")
			continue
		}
		*to = d.path(f.value, "tofu."+name)
	}
	if f, ok := r["git"]; ok {
		if t.Git = d.str(f.value, "tofu.git"); strings.Contains(t.Git, "/") {
			t.Git = d.path(f.value, "tofu.git")
		}
	}
	if t.PluginCache != "" && t.PluginCache == t.ScratchDir {
		d.errorf(e, "tofu.plugin_cache and tofu.scratch_dir must be two directories")
	}
	return t
}

// uidRange reads provider_uids, such as "61000-61063".
func (d *decoder) uidRange(e hclsyntax.Node) *UIDRange {
	s := d.str(e, "provider_uids")
	a, b, ok := strings.Cut(s, "-")
	first, err1 := strconv.ParseUint(a, 10, 32)
	last, err2 := strconv.ParseUint(b, 10, 32)
	if !ok || err1 != nil || err2 != nil || first < 1000 || last < first || last >= 1<<31 || last-first >= 65536 {
		d.errorf(e, "provider_uids must be a range such as \"61000-61063\" of at most 65536 uids from 1000 up, got %q", s)
		return nil
	}
	return &UIDRange{First: uint32(first), Last: uint32(last)}
}

// roles checks what an environment's role needs: aws, to reach STS, and
// provider_uids, so providers cannot read the state keys or reach the
// host's own identity.
func (d *decoder) roles(e hclsyntax.Node, top map[string]field, cfg *Config) {
	var withRole []string
	for _, env := range cfg.Envs {
		if env.PlanRole != "" || env.ApplyRole != "" {
			withRole = append(withRole, env.Name)
		}
	}
	slices.Sort(withRole)
	if len(withRole) == 0 {
		if f, ok := top["aws"]; ok {
			d.warnf(f.key, "aws has no effect: no environment names a role")
		}
		return
	}
	if cfg.AWS == nil {
		d.errorf(e, "env %s names a role, so the config needs aws { region = \"...\" }", withRole[0])
	}
	if _, ok := top["provider_uids"]; !ok {
		d.errorf(e, "env %s names a role, so the config needs provider_uids: providers that run as writ's own user can read the state keys and reach the host's own AWS identity", withRole[0])
	}
}

// lint warns about configs that are valid and risky, as writ serve --check
// prints them.
func (d *decoder) lint(cfg *Config) {
	var span any
	ids := map[string]string{"project_path": "project_id", "namespace_path": "namespace_id", "repository": "repository_id", "repository_owner": "repository_owner_id"}
	refKeys := []string{"ref", "ref_path", "pipeline_source", "event_name", "environment", "workflow_ref", "ci_config_ref_uri"}
	for _, t := range cfg.Policy.Teams {
		claims := map[string]bool{}
		for _, r := range t.Match {
			claims[r.Claim] = true
		}
		for p, id := range ids {
			if claims[p] && !claims[id] {
				d.warnf(span, "team %s matches %s without %s; a renamed project's path can be taken by another, so prefer the id", t.Name, p, id)
			}
		}
		writes := slices.ContainsFunc(cfg.Policy.Grants, func(g authz.Grant) bool {
			return slices.Contains(g.Teams, t.Name) && (slices.Contains(g.Can, authz.Apply) || slices.Contains(g.Can, authz.Override))
		})
		if writes && !slices.ContainsFunc(refKeys, func(k string) bool { return claims[k] }) {
			d.warnf(span, "team %s may apply but matches no ref, pipeline source, or environment, so every branch's pipeline has that power", t.Name)
		}
	}
	d.lintIdentities(cfg)
	if cfg.TLS == nil && len(cfg.TrustedProxies) == 0 {
		d.warnf(span, "writ serve serves plain HTTP behind a proxy, and trusted_proxies is not set, so every client counts as the proxy's address against limits.requests_per_address; list the proxy in trusted_proxies")
	}
	if cfg.ProviderUIDs == nil {
		d.warnf(span, "providers run as writ's own user, so they can read the state keys and reach the host's own identity; set provider_uids")
	}
	if !cfg.EgressMode.On() {
		for _, name := range slices.Sorted(maps.Keys(cfg.Envs)) {
			if cfg.Envs[name].Egress != nil {
				d.warnf(span, "env %s sets egress, and egress_mode is off, so its runs keep the host's network; set egress_mode to report or enforce", name)
			}
		}
	}
}
