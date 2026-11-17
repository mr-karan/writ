package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/mr-karan/writ/internal/server"
	"github.com/mr-karan/writ/internal/tfcode"
)

// CIOptions are the flags of `writ ci gitlab`.
type CIOptions struct {
	Repo string
	// Envs narrows the environments; empty means every one the control
	// plane serves for the repository's stacks.
	Envs []string
	// Component is the CI/CD component to include, with its version.
	Component string
	Image     string
}

// DefaultComponent is the GitLab CI/CD component writ ci gitlab includes,
// on the instance that runs the pipeline.
const DefaultComponent = "$CI_SERVER_FQDN/platform/writ-ci/writ-plan@0.1.0"

// CIGitLab is `writ ci gitlab`: it prints the lines a repository's
// .gitlab-ci.yml needs, a plan job and an apply job for each stack and
// environment the control plane serves and one approve job, and the
// teams, grants, and approval rule the control plane's config needs for
// the project, by its numeric id.
func (a *App) CIGitLab(ctx context.Context, o CIOptions) error {
	r, err := a.remote(ctx)
	if err != nil {
		return err
	}
	doc, err := r.get(ctx, "/v1/repos/"+url.PathEscape(o.Repo), remoteTimeout)
	if re, ok := errors.AsType[*remoteError](err); ok {
		return a.remoteFailed(false, re.doc)
	}
	if err != nil {
		return err
	}
	var d server.RepoDoc
	if err := json.Unmarshal(doc, &d); err != nil || d.Format != server.FormatRepo {
		return fmt.Errorf("writ serve sent a body that is not %s", server.FormatRepo)
	}
	if d.Repo.Kind != "gitlab" {
		return usagef("repository %s is a %s repository, not a GitLab project", o.Repo, d.Repo.Kind)
	}
	type job struct{ stack, env string }
	var jobs []job
	var envs, stacks []string
	for _, s := range d.Stacks {
		for _, env := range s.Served {
			if len(o.Envs) > 0 && !slices.Contains(o.Envs, env) {
				continue
			}
			jobs = append(jobs, job{s.Stack, env})
			if !slices.Contains(envs, env) {
				envs = append(envs, env)
			}
			if !slices.Contains(stacks, s.Stack) {
				stacks = append(stacks, s.Stack)
			}
		}
	}
	if len(jobs) == 0 {
		return usagef("repository %s has no stack this control plane serves%s; see writ repo show %s", o.Repo, envWords(o.Envs), o.Repo)
	}
	approveRef, applyRef, ok := siblings(o.Component)
	if !ok {
		return usagef("--component %s does not name the writ-plan component, PATH/writ-plan@VERSION, beside which writ-approve and writ-apply are", o.Component)
	}
	for _, v := range slices.Concat([]string{d.Repo.ProjectID, "refs/heads/" + d.Repo.DefaultBranch, strings.ReplaceAll(o.Repo, "-", "_")}, stacks, envs) {
		if !hclRoundTrips(v) {
			return usagef("%q cannot be written as an HCL string that reads back unchanged, so writ ci gitlab cannot write a rule for it", v)
		}
	}
	out := a.humanOutput()
	fmt.Fprintln(out, "# .gitlab-ci.yml: add these lines. A merge request pipeline plans each stack in each")
	fmt.Fprintln(out, "# environment, and its manual approve job blocks the merge until each plan has its")
	fmt.Fprintln(out, "# approvals. The pipeline on the default branch applies each approved plan whose sources")
	fmt.Fprintln(out, "# equal the merged commit's. No job needs a checkout.")
	fmt.Fprintln(out, "stages: [writ-plan, writ-approve, writ-apply]")
	fmt.Fprintln(out, "include:")
	component := func(ref string, inputs ...string) {
		fmt.Fprintf(out, "  - component: %s\n    inputs:\n", ref)
		for i := 0; i+1 < len(inputs); i += 2 {
			fmt.Fprintf(out, "      %s: %s\n", inputs[i], inputs[i+1])
		}
	}
	for _, j := range jobs {
		component(o.Component, "server", r.base, "env", j.env, "stack", j.stack, "image", o.Image)
	}
	component(approveRef, "server", r.base, "image", o.Image)
	for _, j := range jobs {
		component(applyRef, "server", r.base, "env", j.env, "stack", j.stack, "image", o.Image)
	}
	name := strings.ReplaceAll(o.Repo, "-", "_")
	q := func(xs []string) string {
		out := make([]string, len(xs))
		for i, x := range xs {
			out[i] = tfcode.Quoted(x)
		}
		return strings.Join(out, ", ")
	}
	id := tfcode.Quoted(d.Repo.ProjectID)
	fmt.Fprintf(out, "\n# The writ serve config: the teams and grants of this repository's pipelines, by the\n# project's id, %s, which a renamed project keeps, and an approval rule.\n", d.Repo.ProjectID)
	team := func(suffix, match string) {
		fmt.Fprintf(out, "team %s {\n  issuer = \"gitlab\"\n  match  = { project_id = [%s], %s }\n}\n", tfcode.Quoted(name+"_"+suffix), id, match)
	}
	team("mr", `pipeline_source = ["merge_request_event"]`)
	team("reviewers", `user_access_level = ["maintainer", "owner"]`)
	team("main", fmt.Sprintf(`ref_path = [%s], ref_protected = ["true"]`, tfcode.Quoted("refs/heads/"+d.Repo.DefaultBranch)))
	grant := func(suffix, can, risks string) {
		fmt.Fprintf(out, "grant {\n  teams  = [%s]\n  stacks = [%s]\n  envs   = [%s]\n  can    = [%s]\n", tfcode.Quoted(name+"_"+suffix), q(stacks), q(envs), can)
		if risks != "" {
			fmt.Fprintf(out, "  risks  = [%s]\n", risks)
		}
		fmt.Fprintf(out, "}\n")
	}
	grant("mr", `"read", "plan"`, "")
	grant("reviewers", `"approve"`, "")
	grant("main", `"read", "apply"`, `"create", "update", "replace", "delete"`)
	fmt.Fprintf(out, "approval {\n  stacks = [%s]\n  envs   = [%s]\n  risks  = [\"create\", \"update\", \"replace\", \"delete\", \"data-loss\"]\n  need   = 1\n}\n", q(stacks), q(envs))
	fmt.Fprintf(out, "\n# Project settings of %s: protect %s, and turn on \"Pipelines must succeed\".\n", d.Repo.Project, d.Repo.DefaultBranch)
	if r.dev {
		fmt.Fprintf(out, "# %s is the dev control plane, on loopback: a GitLab runner cannot reach it.\n", r.base)
	}
	return nil
}

// hclRoundTrips reports whether tfcode.Quoted(s) decodes back to s byte for
// byte. HCL normalizes Unicode and replaces invalid UTF-8, so a branch
// such as "e" plus a combining accent would otherwise match a different
// branch.
func hclRoundTrips(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	f, diags := hclsyntax.ParseConfig([]byte("x = "+tfcode.Quoted(s)+"\n"), "ci.hcl", hcl.InitialPos)
	if diags.HasErrors() {
		return false
	}
	v, diags := f.Body.(*hclsyntax.Body).Attributes["x"].Expr.Value(nil)
	return !diags.HasErrors() && v.AsString() == s
}

// siblings returns the approve and apply components beside the plan
// component ref names: the same project and version.
func siblings(ref string) (approve, apply string, ok bool) {
	before, after, ok := strings.Cut(ref, "/writ-plan@")
	if !ok {
		return "", "", false
	}
	return before + "/writ-approve@" + after, before + "/writ-apply@" + after, true
}

func envWords(envs []string) string {
	if len(envs) == 0 {
		return ""
	}
	return " in " + strings.Join(envs, ", ")
}
