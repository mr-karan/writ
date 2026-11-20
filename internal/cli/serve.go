package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mr-karan/writ/internal/awsrole"
	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/diag"
	"github.com/mr-karan/writ/internal/egress"
	"github.com/mr-karan/writ/internal/notify"
	"github.com/mr-karan/writ/internal/oidc"
	"github.com/mr-karan/writ/internal/provider"
	"github.com/mr-karan/writ/internal/server"
	"github.com/mr-karan/writ/internal/tofu"
	"github.com/mr-karan/writ/internal/ui"
)

// ServeOptions are the flags of `writ serve`.
type ServeOptions struct {
	Config string
	// Check reads the config, prints what it allows, and serves nothing.
	Check bool
	// NotifyTest, with Check, sends a test message to each notifier.
	NotifyTest bool
	// Version is writ's own version, which GET /v1/server reports.
	Version string
}

// IsolationProbeEnv, set in writ's environment, makes writ run the
// isolation probe instead of a command.
const IsolationProbeEnv = server.IsolationProbeEnv

// RunIsolationProbe is what writ runs, as a provider's user, for writ
// serve --check: it tries to read the files the check names and to reach
// the instance metadata addresses, and prints what happened as JSON.
func RunIsolationProbe(req string, out io.Writer) int { return server.RunIsolationProbe(req, out) }

// Serve runs writ serve, the control plane, until ctx ends.
func (a *App) Serve(ctx context.Context, o ServeOptions) error {
	if o.Config == "" {
		return usagef("serve needs --config FILE")
	}
	src, err := os.ReadFile(o.Config)
	if err != nil {
		return usagef("read the config: %v", err)
	}
	cfg, diags := server.ParseConfig(o.Config, src)
	if diags.HasErrors() {
		printDiags(a.humanOutput(), diags)
		return ErrFailed
	}
	switch {
	case o.NotifyTest && !o.Check:
		return usagef("--notify-test goes with --check")
	case o.Check:
		printDiags(a.humanOutput(), diags)
		if err := printServeCheck(a.humanOutput(), o.Config, cfg); err != nil {
			return err
		}
		return errors.Join(a.checkTofu(ctx, cfg), a.checkIsolation(ctx, cfg), a.checkLimits(cfg), a.checkNotify(ctx, cfg, o.NotifyTest))
	}
	log := slog.New(slog.NewJSONHandler(a.opts.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	for _, d := range diags {
		log.Warn("config", "at", d.Span.String(), "msg", d.Message)
	}
	if cfg.ProviderUIDs != nil {
		if err := provider.PrepareIsolation(); err != nil {
			return fmt.Errorf("provider_uids: %w", err)
		}
	}
	var assumer awsrole.Assumer
	if cfg.AWS != nil {
		if assumer, err = awsrole.NewSTS(ctx, awsrole.STSOptions{Region: cfg.AWS.Region, Endpoint: cfg.AWS.STSEndpoint}); err != nil {
			return err
		}
	}
	store, err := control.Open(ctx, control.Options{Location: cfg.Database})
	if err != nil {
		return err
	}
	defer store.Close()
	verifier, err := server.NewVerifier(ctx, cfg, log, a.opts.Now)
	if err != nil {
		return err
	}
	people, err := server.People(ctx, cfg, log, a.opts.Now)
	if err != nil {
		return err
	}
	runner, closeTofu, err := serveTofu(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer closeTofu()
	srv, err := server.New(ctx, server.Options{
		Config: cfg, Store: store, Verifier: verifier, People: people, Tofu: runner,
		Assumer: assumer, Version: o.Version, Log: log, Now: a.opts.Now,
	})
	if err != nil {
		return err
	}
	if cfg.UI {
		u, err := ui.New(ui.Options{Server: srv, Log: log, Now: a.opts.Now})
		if err != nil {
			return err
		}
		u.Mount()
	}
	return srv.Serve(ctx, nil)
}

// printServeCheck prints writ serve --check: the config's parts and what
// each team may do.
func printServeCheck(w io.Writer, file string, cfg *server.Config) error {
	fmt.Fprintf(w, "config %s: ok\n", file)
	fmt.Fprintf(w, "url: %s\n", cfg.URL)
	var parts []string
	for _, is := range cfg.Issuers {
		if is.Kind == oidc.People {
			parts = append(parts, fmt.Sprintf("%s (%s, %s, client %s)", is.Name, is.Kind, is.URL, is.ClientID))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s (%s, %s)", is.Name, is.Kind, is.URL))
	}
	fmt.Fprintf(w, "issuers: %s\n", strings.Join(parts, ", "))
	if cfg.UI {
		fmt.Fprintf(w, "control UI: %s/ui/, sign-in redirects to %s/ui/callback, sessions last %s\n", cfg.URL, cfg.URL, cfg.SessionTTL)
	}
	fmt.Fprintf(w, "envs: %s, state in %s\n", strings.Join(sortedKeys(cfg.Envs), ", "), redactURL(cfg.Database))
	if len(cfg.TrustedProxies) > 0 {
		ps := make([]string, len(cfg.TrustedProxies))
		for i, p := range cfg.TrustedProxies {
			ps[i] = p.String()
		}
		fmt.Fprintf(w, "trusted proxies: %s; X-Forwarded-For names the client only when one of them sends it\n", strings.Join(ps, ", "))
	}
	if r := cfg.ProviderUIDs; r != nil {
		fmt.Fprintf(w, "providers run as: uids %s, one for each running plan\n", r)
	} else {
		fmt.Fprintf(w, "providers run as: writ's own user\n")
	}
	if t := cfg.Tofu; t != nil {
		fmt.Fprintf(w, "tofu: %s, plugin cache %s, scratch directories in %s\n", t.Binary, t.PluginCache, t.ScratchDir)
	} else {
		fmt.Fprintln(w, "tofu: no tofu block, so stacks of .tf files are refused")
	}
	switch cfg.EgressMode {
	case egress.Report:
		fmt.Fprintln(w, "egress: report; each OpenTofu run reaches the network only through writ's egress proxy, which lets an unlisted destination through and records it")
	case egress.Enforce:
		fmt.Fprintln(w, "egress: enforce; each OpenTofu run reaches the network only through writ's egress proxy, which refuses a destination its env's egress does not list")
	}
	if cfg.EgressMode.On() {
		for _, name := range sortedKeys(cfg.Envs) {
			allows := "nothing"
			if e := cfg.Envs[name].Egress; len(e) > 0 {
				allows = strings.Join(e, ", ")
			}
			fmt.Fprintf(w, "egress: env %s allows %s\n", name, allows)
		}
		registries := "no registry"
		if len(cfg.ProviderRegistries) > 0 {
			registries = strings.Join(cfg.ProviderRegistries, ", ")
		}
		fmt.Fprintf(w, "egress: providers install only from %s\n", registries)
	}
	for _, r := range cfg.Repos {
		if r.WebhookSecretFile == "" {
			continue
		}
		if err := server.CheckHookSecret(r.WebhookSecretFile); err != nil {
			fmt.Fprintf(w, "webhook: repo %s: %v\n", r.Name, err)
			continue
		}
		fmt.Fprintf(w, "webhook: repo %s: secret file readable\n", r.Name)
	}
	if a := cfg.AWS; a != nil {
		sts := a.STSEndpoint
		if sts == "" {
			sts = "the regional endpoint"
		}
		fmt.Fprintf(w, "aws: sts in %s at %s, sessions of %s\n", a.Region, sts, strings.TrimSuffix(strings.TrimSuffix(a.Session.String(), "0s"), "0m"))
	}
	for _, name := range sortedKeys(cfg.Envs) {
		e := cfg.Envs[name]
		if e.PlanRole != "" {
			fmt.Fprintf(w, "role: %s plan %s\n", name, e.PlanRole)
		}
		if e.ApplyRole != "" {
			fmt.Fprintf(w, "role: %s apply %s\n", name, e.ApplyRole)
		}
	}
	if p := cfg.Policies; p != nil {
		if _, err := server.CompilePolicies(cfg); err != nil {
			return fmt.Errorf("%w; fix the set, then run writ serve --check again", err)
		}
		for _, s := range p.Sets {
			fmt.Fprintf(w, "policy: set %s (%s) compiles, %d files from %s\n", s.Name, s.Level, len(s.Files), s.Dir)
		}
		fmt.Fprintf(w, "policy: each decision has %s and %d MiB, at the plan point and again at the apply point\n", p.Timeout, p.Memory>>20)
	}
	if len(cfg.Policy.Approvals) == 0 {
		fmt.Fprintln(w, "approvals: no rule, so no plan needs an approval")
	}
	for i, a := range cfg.Policy.Approvals {
		fmt.Fprintf(w, "approvals: rule %d: %s in %s with %s needs %d\n", i+1, strings.Join(a.Stacks, ","), strings.Join(a.Envs, ","), strings.Join(a.Risks, " "), a.Need)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TEAM\tENV\tSTACKS\tCAN\tRISKS")
	for _, t := range cfg.Policy.Teams {
		for _, row := range cfg.Policy.Table([]string{t.Name}) {
			can := make([]string, len(row.Can))
			for i, op := range row.Can {
				can[i] = string(op)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", t.Name, strings.Join(row.Envs, ","), strings.Join(row.Stack, ","), strings.Join(can, " "), dash(strings.Join(row.Risks, " ")))
		}
	}
	return tw.Flush()
}

// checkTofu prints whether the tofu block's binary is OpenTofu
// tofu.Version, and whether its git, when set, has what a clone over https
// runs. A failed check fails --check.
func (a *App) checkTofu(ctx context.Context, cfg *server.Config) error {
	if cfg.Tofu == nil {
		return nil
	}
	bin, err := tofuProgram(cfg.Tofu.Binary)
	if err == nil {
		err = tofu.CheckVersion(ctx, bin)
	}
	if err != nil {
		fmt.Fprintf(a.humanOutput(), "tofu: FAILED: %v\n", err)
		return ErrFailed
	}
	fmt.Fprintf(a.humanOutput(), "tofu: %s is OpenTofu %s: ok\n", bin, tofu.Version)
	if cfg.Tofu.Git == "" {
		return nil
	}
	git, err := tofuProgram(cfg.Tofu.Git)
	if err == nil {
		err = tofu.CheckGit(ctx, git)
	}
	if err != nil {
		fmt.Fprintf(a.humanOutput(), "tofu: git: FAILED: %v\n", err)
		return ErrFailed
	}
	fmt.Fprintf(a.humanOutput(), "tofu: git %s: ok\n", git)
	return nil
}

// checkLimits prints what writ serve would enforce of the limits of each
// OpenTofu step, and warns of what it would not, and why. It moves writ
// into no cgroup, so it reports on the cgroup --check runs in: run it
// where writ serve runs, as the unit's ExecStartPre does.
func (a *App) checkLimits(cfg *server.Config) error {
	if cfg.Tofu == nil {
		return nil
	}
	w := a.humanOutput()
	l := runLimits(cfg)
	e := tofu.CheckLimits(l)
	var warn diag.List
	if e.Cgroup != "" {
		fmt.Fprintf(w, "limits: each OpenTofu step runs in a cgroup of its own under %s: memory %s with no swap, %d processes and threads, %v CPUs\n", e.Cgroup, sizeWords(l.Memory), l.Pids, l.CPUs)
		for _, msg := range sharedLimitWarnings(l, cfg.Limits.Plans+cfg.Limits.Applies, e) {
			warn.Warnf(diag.Usage, diag.Span{}, "%s", msg)
		}
	} else {
		warn.Warnf(diag.Usage, diag.Span{}, "limits not enforced: %s; writ serve runs each OpenTofu step without limits.run_memory, run_pids, and run_cpus", e.CgroupOff)
	}
	fmt.Fprintf(w, "limits: writ measures each run's directory, and stops a step past %s\n", sizeWords(l.Scratch))
	if e.Seccomp {
		fmt.Fprintln(w, "seccomp: each OpenTofu process runs under writ's seccomp filter")
	} else {
		warn.Warnf(diag.Usage, diag.Span{}, "seccomp filter not installed: %s", e.SeccompOff)
	}
	printDiags(w, warn)
	return nil
}

// sharedLimitWarnings says where slots steps at once, each at l's limits,
// can need more processes or memory than e leaves every step together:
// the limits of writ serve's cgroup less what writ serve keeps. Steps then
// fail to start processes, or the kernel kills one, before any reaches
// its own limit.
func sharedLimitWarnings(l tofu.Limits, slots int, e tofu.Enforcement) []string {
	var out []string
	if need := int64(l.Pids) * int64(slots); e.StepsPids > 0 && need > e.StepsPids {
		out = append(out, fmt.Sprintf("limits.run_pids, %d, times %d plans and applies at once is %d, more than the %d processes and threads that writ's cgroup leaves every step together, its pids.max less what writ serve keeps; raise the unit's TasksMax, or lower limits.run_pids, plans, or applies", l.Pids, slots, need, e.StepsPids))
	}
	if need := l.Memory * int64(slots); e.StepsMemory > 0 && need > e.StepsMemory {
		out = append(out, fmt.Sprintf("limits.run_memory, %s, times %d plans and applies at once is %s, more than the %s that writ's cgroup leaves every step together, its memory.max less what writ serve keeps; raise the unit's MemoryMax, or lower limits.run_memory, plans, or applies", sizeWords(l.Memory), slots, sizeWords(need), sizeWords(e.StepsMemory)))
	}
	return out
}

// sizeWords writes a size as the config does.
func sizeWords(n int64) string {
	switch {
	case n%(1<<30) == 0:
		return fmt.Sprintf("%dGiB", n>>30)
	case n%(1<<20) == 0:
		return fmt.Sprintf("%dMiB", n>>20)
	}
	return fmt.Sprintf("%dKiB", n>>10)
}

// checkNotify prints writ serve --check's notify part: each notifier,
// whether its files hold a URL writ serve may send to, and the delivery
// counts the control database keeps. With test, it sends each a test
// message. A notifier whose files fail, or whose test fails, fails --check.
func (a *App) checkNotify(ctx context.Context, cfg *server.Config, test bool) error {
	if len(cfg.Notifiers) == 0 {
		return nil
	}
	w := a.humanOutput()
	if _, err := server.LoadNotifiers(cfg); err != nil {
		fmt.Fprintf(w, "notify: FAILED: %v\n", err)
		return ErrFailed
	}
	statuses, err := control.PeekNotificationStatuses(ctx, cfg.Database)
	if err != nil {
		fmt.Fprintf(w, "notify: delivery counts not read: %v\n", err)
	}
	for _, n := range cfg.Notifiers {
		evs := make([]string, len(n.Events))
		for i, ev := range n.Events {
			evs[i] = string(ev)
		}
		scope := ""
		if len(n.Envs) > 0 {
			scope += " in " + strings.Join(n.Envs, ",")
		}
		if len(n.Stacks) > 0 {
			scope += " for " + strings.Join(n.Stacks, ",")
		}
		drift := ""
		if slices.Contains(n.Events, notify.EventDrift) {
			drift = ", drift " + string(n.DriftMode)
		}
		st := statuses[n.Name]
		fmt.Fprintf(w, "notify: %s (%s) hears %s%s%s; %d delivered, %d failed", n.Name, n.Kind, strings.Join(evs, " "), scope, drift, st.Delivered, st.Failed)
		if st.LastError != "" {
			fmt.Fprintf(w, ", last error at %s: %s", st.LastErrorAt.UTC().Format(time.RFC3339), st.LastError)
		}
		fmt.Fprintln(w)
	}
	if !test {
		return nil
	}
	results, err := server.TestNotifiers(ctx, cfg, a.opts.Now().UTC())
	if err != nil {
		fmt.Fprintf(w, "notify: FAILED: %v\n", err)
		return ErrFailed
	}
	failed := false
	for _, r := range results {
		if r.Err != nil {
			fmt.Fprintf(w, "notify: %s: test message FAILED: %v\n", r.Name, r.Err)
			failed = true
			continue
		}
		fmt.Fprintf(w, "notify: %s: test message sent\n", r.Name)
	}
	if failed {
		return ErrFailed
	}
	return nil
}

// checkIsolation prints writ serve --check's isolation part: writ runs
// itself as the first provider uid and reports what that uid could read
// and reach. A failed check fails --check, so a unit that runs it first
// does not start.
func (a *App) checkIsolation(ctx context.Context, cfg *server.Config) error {
	if cfg.ProviderUIDs == nil {
		return nil
	}
	w := a.humanOutput()
	exe, err := os.Executable()
	if err == nil {
		var checks []server.Check
		if checks, err = server.CheckIsolation(ctx, cfg, exe); err == nil {
			failed := false
			for _, c := range checks {
				switch {
				case c.Err == "":
					fmt.Fprintf(w, "isolation: %s: ok\n", c.What)
				case c.Moot:
					fmt.Fprintf(w, "isolation: %s: not checked: %s\n", c.What, c.Err)
				default:
					fmt.Fprintf(w, "isolation: %s: FAILED: %s\n", c.What, c.Err)
					failed = true
				}
			}
			if failed {
				return ErrFailed
			}
			return nil
		}
	}
	var l diag.List
	l.Warnf(diag.Usage, diag.Span{}, "isolation not checked: %v; writ serve checks its capabilities at start", err)
	printDiags(w, l)
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// redactURL drops the query of a database URL for printing; the URL holds
// no password, and its query may name local paths.
func redactURL(u string) string {
	base, _, _ := strings.Cut(u, "?")
	return base
}
