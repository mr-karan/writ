package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/mr-karan/writ/internal/forge"
	"github.com/mr-karan/writ/internal/server"
	"github.com/mr-karan/writ/internal/tfcode"
)

// releaseRE is a Writ release tag, which install.sh takes.
var releaseRE = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$`)

// githubRunner is the runner label of a job at github.com that names
// none: a GitHub-hosted Ubuntu runner, which has sudo and apt-get.
const githubRunner = "ubuntu-24.04"

// maxMatrix is the most jobs one GitHub Actions matrix may make.
const maxMatrix = 256

// planSlots is how many of one principal's plans writ serve runs at once:
// its limits.plans_per_principal, which GET /v1/config shows an admin,
// or the default, and whether it was read. Every job of a workflow run is
// one principal, its actor, so the matrix runs no more jobs at once.
func planSlots(ctx context.Context, r *remote) (int, bool) {
	b, err := r.get(ctx, "/v1/config", remoteTimeout)
	if err != nil {
		return server.DefaultPlansPerPrincipal, false
	}
	var doc server.ConfigDoc
	if json.Unmarshal(b, &doc) != nil || doc.Format != server.FormatConfig || doc.Admin == nil || doc.Admin.Limits.PlansPerPrincipal < 1 {
		return server.DefaultPlansPerPrincipal, false
	}
	return doc.Admin.Limits.PlansPerPrincipal, true
}

// yamlString is s as a YAML double-quoted scalar: JSON's quoting, which
// YAML reads back unchanged.
func yamlString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// writeGitHubCI prints a GitHub Actions workflow that plans each stack in
// each environment on each pull request and each push to the default
// branch, and the config's team, grant, and approval rule. Approval and
// apply stay with people: the workflow holds no apply job.
//
// The workflow uses no action, so it pins none: writ serve reads the
// commit with its own token, so no job checks the repository out, and
// install.sh checks the release's minisign signature and SHA-256. Each
// value reaches the shell through env, never through an expression in
// run, so no stack or environment name can inject a command. The job
// skips a pull request from a fork and Dependabot's runs, which get no ID
// token; a skipped check does not block a pull request.
func writeGitHubCI(ctx context.Context, a *App, r *remote, d server.RepoDoc, o CIOptions, p ciPlan) error {
	if !releaseRE.MatchString(o.WritVersion) {
		return usagef("--writ-version %q is not a Writ release, such as v0.1.8: the workflow installs that release", o.WritVersion)
	}
	if len(p.jobs) > maxMatrix {
		return usagef("repository %s has %d stacks and environments to plan, over the %d jobs a GitHub Actions matrix makes; narrow them with --envs, or split the stacks across repositories", o.Repo, len(p.jobs), maxMatrix)
	}
	runsOn := o.RunsOn
	if runsOn == "" {
		if d.Repo.ForgeURL != forge.GitHubURL {
			return usagef("repository %s is at %s, not github.com, so writ ci github cannot know its runners' labels: name the label of a Linux runner with --runs-on LABEL", o.Repo, d.Repo.ForgeURL)
		}
		runsOn = githubRunner
	}
	if len(runsOn) > 100 || strings.ContainsAny(runsOn, "\r\n") {
		return usagef("--runs-on %q is not a runner label", runsOn)
	}
	slots, read := planSlots(ctx, r)
	out := a.humanOutput()
	fmt.Fprintln(out, "# .github/workflows/writ-plan.yml: each pull request and each push to the default")
	fmt.Fprintln(out, "# branch plans each stack in each environment with writ serve, which takes the")
	fmt.Fprintln(out, "# repository and the commit from the job's ID token and reads the commit itself, so no")
	fmt.Fprintln(out, "# job checks it out or holds a cloud credential. People approve each plan, in /ui/plans")
	fmt.Fprintln(out, "# or with writ approve --remote ID. After the merge, a person applies a pull request's")
	fmt.Fprintln(out, "# approved plan with writ apply --remote --merged --env ENV --stack NAME --repo REPO")
	fmt.Fprintln(out, "# --commit SHA, naming the merged commit, whose sources must be the plan's.")
	fmt.Fprintln(out, "name: writ plan")
	fmt.Fprintln(out, "on:")
	fmt.Fprintln(out, "  pull_request:")
	fmt.Fprintln(out, "  push:")
	fmt.Fprintf(out, "    branches: [%s]\n", yamlString(d.Repo.DefaultBranch))
	fmt.Fprintln(out, "permissions:")
	fmt.Fprintln(out, "  contents: read")
	fmt.Fprintln(out, "  id-token: write")
	fmt.Fprintln(out, "jobs:")
	fmt.Fprintln(out, "  plan:")
	fmt.Fprintln(out, "    # A pull request from a fork and Dependabot's runs get no ID token, so the job skips")
	fmt.Fprintln(out, "    # them; a skipped check does not block a pull request.")
	fmt.Fprintln(out, "    if: github.actor != 'dependabot[bot]' && (github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository)")
	fmt.Fprintf(out, "    runs-on: %s\n", yamlString(runsOn))
	fmt.Fprintln(out, "    timeout-minutes: 30")
	fmt.Fprintln(out, "    strategy:")
	fmt.Fprintln(out, "      fail-fast: false")
	if read {
		fmt.Fprintf(out, "      # writ serve runs %d plans of one principal at once, its limits.plans_per_principal.\n", slots)
	} else {
		fmt.Fprintf(out, "      # writ serve's default limits.plans_per_principal is %d; set your server's value here.\n", slots)
	}
	fmt.Fprintf(out, "      max-parallel: %d\n", slots)
	fmt.Fprintln(out, "      matrix:")
	fmt.Fprintln(out, "        include:")
	for _, j := range p.jobs {
		fmt.Fprintf(out, "          - stack: %s\n            env: %s\n", yamlString(j.stack), yamlString(j.env))
	}
	fmt.Fprintln(out, "    steps:")
	fmt.Fprintln(out, "      - name: Install writ")
	fmt.Fprintln(out, "        env:")
	fmt.Fprintf(out, "          WRIT_VERSION: %s\n", yamlString(o.WritVersion))
	fmt.Fprintln(out, "        # install.sh checks the release's minisign signature. A runner without minisign")
	fmt.Fprintln(out, "        # gets it from apt-get with sudo, as an Ubuntu runner has them.")
	fmt.Fprintln(out, "        run: |")
	fmt.Fprintln(out, "          if ! command -v minisign >/dev/null; then")
	fmt.Fprintln(out, "            sudo apt-get update -qq")
	fmt.Fprintln(out, "            sudo apt-get install -y -qq minisign")
	fmt.Fprintln(out, "          fi")
	fmt.Fprintln(out, `          curl -fsSL "https://github.com/mr-karan/writ/releases/download/$WRIT_VERSION/install.sh" | sh -s -- --version "$WRIT_VERSION"`)
	fmt.Fprintln(out, `          echo "$HOME/.local/bin" >> "$GITHUB_PATH"`)
	fmt.Fprintln(out, "      - name: Plan")
	fmt.Fprintln(out, "        env:")
	fmt.Fprintf(out, "          WRIT_SERVER: %s\n", yamlString(r.base))
	fmt.Fprintf(out, "          WRIT_REPO: %s\n", yamlString(o.Repo))
	fmt.Fprintln(out, "          WRIT_STACK: ${{ matrix.stack }}")
	fmt.Fprintln(out, "          WRIT_ENV: ${{ matrix.env }}")
	fmt.Fprintln(out, "        # writ asks GitHub Actions for the job's ID token, whose audience is WRIT_SERVER;")
	fmt.Fprintln(out, "        # writ serve refuses a commit or a repository other than the token's.")
	fmt.Fprintln(out, `        run: writ plan --remote --env "$WRIT_ENV" --stack "$WRIT_STACK" --repo "$WRIT_REPO" --commit "$GITHUB_SHA"`)

	name := strings.ReplaceAll(o.Repo, "-", "_")
	stacks, envs := quotedList(p.stacks), quotedList(p.envs)
	fmt.Fprintf(out, "\n# The writ serve config: the team and grant of this repository's workflow runs, by the\n# repository's id, %s, which a renamed or moved repository keeps, and an approval rule.\n", d.Repo.ProjectID)
	fmt.Fprintln(out, "# The issuer named github is GitHub Actions' token issuer: kind github-actions, at")
	fmt.Fprintln(out, "# https://token.actions.githubusercontent.com, or https://HOST/_services/token on GitHub")
	fmt.Fprintln(out, "# Enterprise Server.")
	fmt.Fprintf(out, "team %s {\n  issuer = \"github\"\n  match  = { repository_id = [%s] }\n}\n", tfcode.Quoted(name+"_ci"), tfcode.Quoted(d.Repo.ProjectID))
	fmt.Fprintf(out, "grant {\n  teams  = [%s]\n  stacks = [%s]\n  envs   = [%s]\n  can    = [\"read\", \"plan\"]\n}\n", tfcode.Quoted(name+"_ci"), stacks, envs)
	fmt.Fprintf(out, "approval {\n  stacks = [%s]\n  envs   = [%s]\n  risks  = [\"create\", \"update\", \"replace\", \"delete\", \"data-loss\"]\n  need   = 1\n}\n", stacks, envs)
	fmt.Fprintln(out, "# Grant approve and apply on these stacks to a team of a people issuer.")
	fmt.Fprintf(out, "\n# Repository settings of %s: protect %s, and require the writ plan checks to pass.\n", d.Repo.Project, d.Repo.DefaultBranch)
	if !read {
		fmt.Fprintln(out, "# max-parallel is writ serve's default plans_per_principal: an admin's run of writ ci github reads the server's own.")
	}
	if r.dev {
		fmt.Fprintf(out, "# %s is the dev control plane, on loopback: a GitHub-hosted runner cannot reach it.\n", r.base)
	}
	return nil
}
