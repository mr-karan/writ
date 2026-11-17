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

	"github.com/mr-karan/writ/internal/awsrole"
	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/diag"
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
	// Version is writ's own version, which GET /v1/server reports.
	Version string
	// Ready, when set, is called with the address writ serve listens on.
	Ready func(addr string)
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
	case o.Check:
		printDiags(a.humanOutput(), diags)
		if err := printServeCheck(a.humanOutput(), o.Config, cfg); err != nil {
			return err
		}
		return errors.Join(a.checkTofu(ctx, cfg), a.checkIsolation(ctx, cfg))
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
	return srv.Serve(ctx, o.Ready)
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
