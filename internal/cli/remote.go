package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mr-karan/writ/internal/forge"
	"github.com/mr-karan/writ/internal/server"
)

// Remote commands talk to a control plane. The server is the first of
// --server (dev for the dev control plane), WRIT_SERVER, the one control
// plane the user holds a session of writ login for, and the dev control
// plane. The token is the
// first of WRIT_ID_TOKEN, the session for that server, and the dev token
// when the server is the dev control plane. No command starts a control
// plane: a process that a command leaves running has no owner.

// RemoteOptions are the flags every --remote command shares.
type RemoteOptions struct {
	// Stack names the stack on the server.
	Stack string
}

// remoteFailed prints a refused request's document, as JSON or as its
// diagnostics, and returns ErrFailed. A body that is no refusal, such as a
// proxy's error page or an answer of a newer writ serve that this writ
// does not know, is printed by neither: the error says what came, and
// writ writes the failure document.
func (a *App) remoteFailed(asJSON bool, doc []byte) error {
	if !json.Valid(doc) {
		return errors.New("writ serve, or a proxy before it, refused the request with a body that is not JSON")
	}
	diags, ok := refusalDiags(doc)
	if !ok {
		return a.unusable("an answer that is not a refusal and not one this command expects" + formatWords(doc))
	}
	if asJSON {
		a.opts.Stdout.Write(doc)
		return ErrFailed
	}
	for _, d := range diags {
		fmt.Fprintf(a.humanOutput(), "%s: %s: %s\n", d.Severity, d.Class, escapeHuman(d.Message, false))
	}
	return ErrFailed
}

// formatWords names a document's format and ok, for an error about it.
func formatWords(doc []byte) string {
	var head struct {
		Format string `json:"format"`
		OK     *bool  `json:"ok"`
	}
	if json.Unmarshal(doc, &head) != nil || head.Format == "" {
		return ""
	}
	w := " (" + escapeHuman(head.Format[:min(len(head.Format), 64)], false)
	if head.OK != nil {
		w += fmt.Sprintf(", ok %t", *head.OK)
	}
	return w + ")"
}

// tofuStack reports whether the stack directory holds OpenTofu code.
func (a *App) tofuStack() bool {
	ents, err := a.opts.Files.ReadDir(a.opts.Dir)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(ents, func(e fs.DirEntry) bool {
		ext := filepath.Ext(e.Name())
		return ext == ".tf" || ext == ".tofu"
	})
}

// RemotePlanOptions are the flags of `writ plan --remote`.
type RemotePlanOptions struct {
	RemoteOptions
	JSON bool
	// HintMergeRequest is the merge request the plan belongs to, which the
	// server keeps only for a merge request pipeline.
	HintMergeRequest int
	// Repo and Commit name the commit to plan, and no file is read: a CI
	// job with no checkout sends them. A CI job's token names its own
	// repository, so Repo may be empty there.
	Repo, Commit string
}

// RemotePlan sends a plan to writ serve, waits for it, and prints it. The
// plan names a commit, which the control plane fetches: --commit, or the
// commit of a clean work tree of a repository the control plane reads.
func (a *App) RemotePlan(ctx context.Context, o RemotePlanOptions) (err error) {
	defer escapeError(&err, o.JSON)
	_, doc, err := a.sendRemotePlan(ctx, o)
	if err != nil {
		return err
	}
	return a.printRemotePlan(o.JSON, doc)
}

// sendRemotePlan sends a plan to writ serve and waits for it to end. A
// refused request prints its document and returns ErrFailed.
func (a *App) sendRemotePlan(ctx context.Context, o RemotePlanOptions) (*remote, []byte, error) {
	if o.Commit != "" && !forge.IsSHA(o.Commit) {
		return nil, nil, usagef("--commit %q is not a commit's full id", o.Commit)
	}
	r, err := a.remote(ctx)
	if err != nil {
		return nil, nil, err
	}
	if a.opts.Env == "" && !r.dev {
		return nil, nil, usagef("--remote needs --env: writ serve plans in its environments")
	}
	req := server.PlanRequest{Format: server.FormatPlanRequest, Env: a.opts.Env, Hints: server.Hints{MergeRequest: o.HintMergeRequest}}
	env := cmp.Or(a.opts.Env, server.DevEnv)
	var line string
	switch {
	case o.Commit != "" || o.Repo != "":
		if o.Stack == "" {
			return nil, nil, usagef("--commit and --repo name a commit to plan, and no file is read, so name the stack with --stack")
		}
		req.Stacks, req.Source = []string{o.Stack}, &server.SourceRequest{Repo: o.Repo, Commit: o.Commit}
		line = fmt.Sprintf("commit plan of %s@%s: %s, sent to %s\n", o.Stack, env, cmp.Or(forge.Short(o.Commit), "the job's commit"), r.base)
	default:
		if !a.tofuStack() {
			return nil, nil, usagef("%s holds no .tf or .tofu file: writ serve plans OpenTofu stacks only", a.path("."))
		}
		dir, err := filepath.Abs(a.opts.Dir)
		if err != nil {
			return nil, nil, err
		}
		wt := a.findWorkTree(ctx, r, filepath.Clean(dir))
		if wt.source != nil && o.Stack != "" && o.Stack != wt.name {
			wt.source, wt.why = nil, "--stack "+o.Stack+" is not the stack's name "+wt.name+" in repository "+wt.repo.Name
		}
		if wt.source == nil {
			return nil, nil, usagef("%s is an OpenTofu stack, which writ serve plans only by commit, and this plan cannot be of a commit: %s", a.path("."), wt.why)
		}
		req.Stacks, req.Source = []string{wt.name}, wt.source
		line = fmt.Sprintf("commit plan of %s@%s: repository %s at %s%s, sent to %s\n", wt.name, env, wt.repo.Name, forge.Short(wt.source.Commit), refWords(wt.source.Ref), r.base)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, nil, err
	}
	if !o.JSON {
		io.WriteString(a.humanOutput(), line)
	}
	status, resp, err := r.call(ctx, http.MethodPost, "/v1/plans", body, remoteTimeout)
	if err != nil {
		return nil, nil, err
	}
	if status != http.StatusAccepted {
		return nil, nil, a.remoteFailed(o.JSON, resp)
	}
	var created server.RemotePlan
	if err := json.Unmarshal(resp, &created); err != nil || created.Plan == nil {
		return nil, nil, fmt.Errorf("writ serve answered 202 with a body that is not %s", server.FormatRemotePlan)
	}
	doc, err := a.waitPlan(ctx, r, created.Plan.Ref)
	if err != nil {
		return nil, nil, err
	}
	return r, doc, nil
}

var runIDRE = regexp.MustCompile(`^[A-Z2-7]{26}$`)

func refWords(ref string) string {
	if ref == "" {
		return ""
	}
	return " (" + strings.TrimPrefix(ref, "refs/heads/") + ")"
}

// waitPlan polls the plan until it leaves planning.
func (a *App) waitPlan(ctx context.Context, r *remote, ref string) ([]byte, error) {
	deadline := time.Now().Add(remoteDeadline)
	path := "/v1/plans/" + url.PathEscape(ref) + "?wait=" + remotePollWait.String()
	for {
		doc, err := r.get(ctx, path, remotePollWait+remoteTimeout)
		if err != nil {
			if re, ok := errors.AsType[*remoteError](err); ok {
				return re.doc, nil
			}
			return nil, err
		}
		var p server.RemotePlan
		if err := json.Unmarshal(doc, &p); err != nil || p.Plan == nil {
			return nil, fmt.Errorf("writ serve sent a plan that is not %s", server.FormatRemotePlan)
		}
		if p.Plan.Status != "planning" {
			return doc, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("plan %s still runs after %s; follow it with writ show --remote %s", ref, remoteDeadline, ref)
		}
	}
}

// printRemotePlan prints a writ.remote-plan/v0 document: as is with --json,
// else the plan's text and a line that names the plan.
func (a *App) printRemotePlan(asJSON bool, doc []byte) error {
	var p server.RemotePlan
	if err := json.Unmarshal(doc, &p); err != nil {
		return fmt.Errorf("writ serve sent a plan that is not %s", server.FormatRemotePlan)
	}
	if _, refused := refusalDiags(doc); !refused && (p.Format != server.FormatRemotePlan || p.Plan == nil) {
		return a.unusable("a body that is not " + server.FormatRemotePlan + " with a plan" + formatWords(doc))
	}
	if asJSON {
		if _, err := a.opts.Stdout.Write(doc); err != nil {
			return err
		}
	} else {
		out := a.humanOutput()
		for _, d := range p.Diagnostics {
			fmt.Fprintf(out, "%s: %s: %s\n", d.Severity, d.Class, escapeHuman(d.Message, false))
		}
		io.WriteString(out, p.Text)
		if p.Plan != nil {
			printPlanLine(out, p.Plan)
		}
	}
	if !p.OK || p.Plan == nil || p.Plan.Status == "refused" || p.Plan.Status == "failed" {
		return ErrFailed
	}
	return nil
}

func printPlanLine(w io.Writer, p *server.PlanInfo) {
	stacks := make([]string, len(p.Stacks))
	for i, s := range p.Stacks {
		stacks[i] = s.Stack
	}
	who := escapeHuman(p.Author.Display, false)
	from := "of files sent, never applied by a team's control plane"
	if p.Source.Kind == server.SourceCommit {
		from = "of " + p.Source.Repo + " at " + forge.Short(p.Source.Commit)
	}
	switch p.Status {
	case "pending", "approved":
		fmt.Fprintf(w, "Plan %s for %s (%s), by %s, expires %s.\nIt is a %s plan %s.\n", p.ID, p.Env, strings.Join(stacks, ", "), who, p.ExpiresAt, p.Source.Kind, from)
		printApprovals(w, p)
	case "planning":
		fmt.Fprintf(w, "Plan %s for %s (%s), by %s, is still planning.\n", p.Ref, p.Env, strings.Join(stacks, ", "), who)
	case "no-changes":
		fmt.Fprintf(w, "Plan %s for %s (%s), by %s, changes no resource: there is nothing to apply.\n", p.ID, p.Env, strings.Join(stacks, ", "), who)
	default:
		name := p.ID
		if name == "" {
			name = p.Ref
		}
		fmt.Fprintf(w, "Plan %s for %s (%s), by %s, is %s.\n", name, p.Env, strings.Join(stacks, ", "), who, p.Status)
		if p.SupersededWhy != "" {
			fmt.Fprintf(w, "Superseded: %s.\n", escapeHuman(p.SupersededWhy, false))
		}
		if p.Run != "" {
			fmt.Fprintf(w, "Run %s applied it: writ show --remote %s\n", p.Run, p.Run)
		}
	}
}

// printApprovals says what a live plan's approval rules need, who gave
// them, and the command that comes next.
func printApprovals(w io.Writer, p *server.PlanInfo) {
	ap := p.Approvals
	var by []string
	for _, a := range ap.ApprovedBy {
		by = append(by, escapeHuman(a.Display, false))
	}
	switch {
	case p.Source.Kind != "commit" && p.Status == "pending":
		return
	case ap.Need == 0:
		fmt.Fprintf(w, "It needs no approval. Apply it with: writ apply --remote %s\n", p.ID)
	case len(ap.ApprovedBy) >= ap.Need:
		fmt.Fprintf(w, "It has %d of %d approvals (%s). Apply it with: writ apply --remote %s\n", len(ap.ApprovedBy), ap.Need, strings.Join(by, ", "), p.ID)
	case len(by) > 0:
		fmt.Fprintf(w, "It has %d of %d approvals (%s). Approve it with: writ approve --remote %s\n", len(ap.ApprovedBy), ap.Need, strings.Join(by, ", "), p.ID)
	default:
		fmt.Fprintf(w, "It needs %s from someone other than its author (rule %s). Approve it with: writ approve --remote %s\n", plural(ap.Need, "approval"), joinInts(ap.Rules), p.ID)
	}
}

func joinInts(xs []int) string {
	s := make([]string, len(xs))
	for i, x := range xs {
		s[i] = strconv.Itoa(x)
	}
	return strings.Join(s, ", ")
}

// ShowOptions are the flags of `writ show --remote ID`: a plan's id or
// ref, or a run's id. Request, in place of ID, is the key of an apply
// request.
type ShowOptions struct {
	JSON    bool
	ID      string
	Request string
}

// Show prints a plan writ serve keeps, by its id or its ref, or a run, or
// the outcome of an apply request by its key.
func (a *App) Show(ctx context.Context, o ShowOptions) (err error) {
	defer escapeError(&err, o.JSON)
	r, err := a.remote(ctx)
	if err != nil {
		return err
	}
	if o.Request != "" {
		return a.showRequest(ctx, r, o)
	}
	doc, err := r.get(ctx, "/v1/plans/"+url.PathEscape(o.ID), remoteTimeout)
	// A plan's ref and a run's id look alike: an id that names no plan
	// may name a run.
	if re, ok := errors.AsType[*remoteError](err); ok && re.status == http.StatusNotFound && runIDRE.MatchString(o.ID) {
		doc, err = r.get(ctx, "/v1/runs/"+url.PathEscape(o.ID), remoteTimeout)
		if err == nil {
			return a.printRemoteRun(o.JSON, doc)
		}
	}
	if re, ok := errors.AsType[*remoteError](err); ok {
		return a.remoteFailed(o.JSON, re.doc)
	}
	if err != nil {
		return err
	}
	return a.printRemotePlan(o.JSON, doc)
}

// RemoteRuns is `writ runs --remote`: the runs writ serve started in
// --env, newest first, of the stack --stack names, or of every stack.
func (a *App) RemoteRuns(ctx context.Context, o RemoteOptions, limit int, asJSON bool) (err error) {
	defer escapeError(&err, asJSON)
	if limit < 1 || limit > 200 {
		return usagef("--limit with --remote must be 1 to 200, got %d", limit)
	}
	r, err := a.remote(ctx)
	if err != nil {
		return err
	}
	env := a.opts.Env
	if env == "" {
		if !r.dev {
			return usagef("--remote needs --env: writ serve lists the runs of one environment")
		}
		env = server.DevEnv
	}
	q := url.Values{"env": {env}, "limit": {strconv.Itoa(limit)}}
	if o.Stack != "" {
		if err := server.CheckStackName(o.Stack); err != nil {
			return usagef("--stack: %v", err)
		}
		q.Set("stack", o.Stack)
	}
	doc, err := r.get(ctx, "/v1/applies?"+q.Encode(), remoteTimeout)
	if re, ok := errors.AsType[*remoteError](err); ok {
		return a.remoteFailed(asJSON, re.doc)
	}
	if err != nil {
		return err
	}
	if asJSON {
		_, err := a.opts.Stdout.Write(doc)
		return err
	}
	var runs server.RemoteRuns
	if err := json.Unmarshal(doc, &runs); err != nil || runs.Format != server.FormatRemoteRuns {
		return fmt.Errorf("writ serve sent a body that is not %s", server.FormatRemoteRuns)
	}
	out := a.humanOutput()
	if len(runs.Runs) == 0 {
		fmt.Fprintf(out, "writ serve has started no run in %s.\n", env)
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RUN\tSTARTED\tSTACK\tSTATUS\tBY\tPLAN")
	for _, run := range runs.Runs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", run.ID, run.StartedAt, escapeHuman(run.Stack, true), run.Status, escapeHuman(cmp.Or(run.Actor.Display, run.Actor.Person), true), dash(run.Plan))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if runs.More {
		fmt.Fprintf(out, "Older runs are left out; raise --limit, up to 200.\n")
	}
	return nil
}

// Whoami is `writ whoami`: the control plane and how it was found, who
// the token speaks for and where it came from, its teams, and the grants
// that apply to them.
func (a *App) Whoami(ctx context.Context, asJSON bool) (err error) {
	defer escapeError(&err, asJSON)
	r, err := a.remote(ctx)
	if err != nil {
		return err
	}
	doc, err := r.get(ctx, "/v1/whoami", remoteTimeout)
	if re, ok := errors.AsType[*remoteError](err); ok {
		return a.remoteFailed(asJSON, re.doc)
	}
	if err != nil {
		return err
	}
	if asJSON {
		_, err := a.opts.Stdout.Write(doc)
		return err
	}
	var w server.Whoami
	if err := json.Unmarshal(doc, &w); err != nil || w.Principal == nil {
		return fmt.Errorf("writ serve sent a body that is not %s", server.FormatWhoami)
	}
	out := a.humanOutput()
	p := w.Principal
	var token string
	switch {
	case r.tokenFrom == tokenSession:
		token = fmt.Sprintf("session of %s (%s, issuer %s), ends %s", escapeHuman(p.Display, false), escapeHuman(dash(p.Person), false), p.Issuer, dash(p.Expires))
	case r.tokenFrom == tokenDev:
		token = "dev token of " + escapeHuman(p.Display, false)
	default:
		token = fmt.Sprintf("%s token of %s in %s: %s, person %s, expires %s", p.Kind, p.Issuer, r.tokenFrom, escapeHuman(p.Display, false), escapeHuman(dash(p.Person), false), dash(p.Expires))
	}
	fmt.Fprintf(out, "%s (from %s), %s\n", r.base, r.from, token)
	fmt.Fprintf(out, "teams: %s\n", dash(strings.Join(w.Teams, ", ")))
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, g := range w.Grants {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", strings.Join(g.Envs, ","), strings.Join(g.Stacks, ","), strings.Join(g.Can, " "), dash(strings.Join(g.Risks, " ")))
	}
	return tw.Flush()
}
