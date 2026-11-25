package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/mr-karan/writ/internal/forge"
	"github.com/mr-karan/writ/internal/server"
	"github.com/mr-karan/writ/internal/tfcode"
)

// releaseRE is a Writ release tag, vMAJOR.MINOR.PATCH with no leading
// zeros, which go install takes as the module's version.
var releaseRE = regexp.MustCompile(`^v(0|[1-9][0-9]{0,5})\.(0|[1-9][0-9]{0,5})\.(0|[1-9][0-9]{0,5})$`)

// setupGo is actions/setup-go pinned by its commit, with its tag.
const setupGo = "actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7.0.0"

// minWorkflowRelease is the first release whose writ the workflow can
// run: it asks GitHub Actions for its ID token, takes --slot-wait, and
// fetches a new token when a long plan outlives the first.
const minWorkflowRelease = "v0.1.9"

// releaseParts are a release tag's major, minor, and patch, and whether
// it is one.
func releaseParts(tag string) ([3]int, bool) {
	m := releaseRE.FindStringSubmatch(tag)
	if m == nil {
		return [3]int{}, false
	}
	var p [3]int
	for i := range p {
		p[i], _ = strconv.Atoi(m[i+1])
	}
	return p, true
}

// workflowRelease is the release the workflow installs: --writ-version,
// or this writ's own version when it is a release, either at least
// minWorkflowRelease.
func (a *App) workflowRelease(flag string) (string, error) {
	least, _ := releaseParts(minWorkflowRelease)
	tag := cmp.Or(flag, a.opts.Version)
	p, ok := releaseParts(tag)
	switch {
	case flag != "" && !ok:
		return "", usagef("--writ-version %q is not a Writ release, such as %s: the workflow installs that release", flag, minWorkflowRelease)
	case flag == "" && !ok:
		return "", usagef("this writ is %q, not a release, so writ ci github cannot tell which release the workflow should install: name one with --writ-version, %s or later", tag, minWorkflowRelease)
	case slices.Compare(p[:], least[:]) < 0:
		return "", usagef("writ %s cannot run the workflow, which needs the GitHub Actions token fetch and --slot-wait of %s or later: name a later release with --writ-version", tag, minWorkflowRelease)
	}
	return tag, nil
}

// githubRunner is the runner label of a job at github.com that names
// none: a GitHub-hosted Ubuntu runner, which has sudo and apt-get.
const githubRunner = "ubuntu-24.04"

// maxMatrix is the most jobs one GitHub Actions matrix may make.
const maxMatrix = 256

// slotWait is how long a plan job waits for a busy plan slot, and
// jobTimeout the job's timeout-minutes, which leaves the plan time to run
// after the wait.
const (
	slotWait   = "15m"
	jobTimeout = 45
)

// planSlots is how many plans of one workflow run writ serve takes at
// once: the lesser of its limits.plans and limits.plans_per_principal,
// which GET /v1/config shows an admin, since every job of a run is one
// principal, its actor. Unread, it is 1, and read reports so.
func planSlots(ctx context.Context, r *remote) (slots int, read bool) {
	b, err := r.get(ctx, "/v1/config", remoteTimeout)
	if err != nil {
		return 1, false
	}
	var doc server.ConfigDoc
	if json.Unmarshal(b, &doc) != nil || doc.Format != server.FormatConfig || doc.Admin == nil {
		return 1, false
	}
	l := doc.Admin.Limits
	if l.Plans < 1 || l.PlansPerPrincipal < 1 {
		return 1, false
	}
	return min(l.Plans, l.PlansPerPrincipal), true
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
// The one action it uses, actions/setup-go, is pinned by its commit. writ
// serve reads the commit with its own token, so no job checks the
// repository out, and go install takes writ through the Go module proxy,
// checked against the Go checksum database, not from the release's own
// files. Each run step is bash with set -euo pipefail, so a failed step
// stops the job. Each value reaches the shell through env, never through
// an expression in run, so no stack or environment name can inject a
// command. The plan job skips a pull request from a fork and Dependabot's
// runs, which get no ID token, and the writ job reports that as a pass.
func writeGitHubCI(ctx context.Context, a *App, r *remote, d server.RepoDoc, o CIOptions, p ciPlan) error {
	release, err := a.workflowRelease(o.WritVersion)
	if err != nil {
		return err
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
	fmt.Fprintln(out, "defaults:")
	fmt.Fprintln(out, "  run:")
	fmt.Fprintln(out, "    shell: bash")
	fmt.Fprintln(out, "jobs:")
	fmt.Fprintln(out, "  plan:")
	fmt.Fprintln(out, "    # A pull request from a fork and Dependabot's runs get no ID token, so the job skips")
	fmt.Fprintln(out, "    # them, for any event that carries a pull request; the writ job below reports it.")
	fmt.Fprintln(out, "    if: github.actor != 'dependabot[bot]' && (!github.event.pull_request || github.event.pull_request.head.repo.full_name == github.repository)")
	fmt.Fprintf(out, "    runs-on: %s\n", yamlString(runsOn))
	fmt.Fprintf(out, "    timeout-minutes: %d\n", jobTimeout)
	fmt.Fprintln(out, "    strategy:")
	fmt.Fprintln(out, "      fail-fast: false")
	if read {
		fmt.Fprintf(out, "      # writ serve takes %d plans of one actor at once: the lesser of its limits.plans and\n      # limits.plans_per_principal.\n", slots)
	} else {
		fmt.Fprintln(out, "      # writ ci github could not read writ serve's limits, which only an admin may: an admin")
		fmt.Fprintln(out, "      # who runs it again gets the lesser of limits.plans and limits.plans_per_principal.")
	}
	fmt.Fprintf(out, "      max-parallel: %d\n", slots)
	fmt.Fprintln(out, "      matrix:")
	fmt.Fprintln(out, "        include:")
	for _, j := range p.jobs {
		fmt.Fprintf(out, "          - stack: %s\n            env: %s\n", yamlString(j.stack), yamlString(j.env))
	}
	fmt.Fprintln(out, "    steps:")
	fmt.Fprintf(out, "      - uses: %s\n", setupGo)
	fmt.Fprintln(out, "        with:")
	fmt.Fprintln(out, "          go-version: stable")
	fmt.Fprintln(out, "          cache: false")
	fmt.Fprintln(out, "      - name: Install writ")
	fmt.Fprintln(out, "        env:")
	fmt.Fprintf(out, "          WRIT_VERSION: %s\n", yamlString(release))
	fmt.Fprintln(out, "          CGO_ENABLED: \"0\"")
	fmt.Fprintln(out, "          GOTOOLCHAIN: auto")
	fmt.Fprintln(out, "          GOSUMDB: sum.golang.org")
	fmt.Fprintln(out, "          GOFLAGS: \"\"")
	fmt.Fprintln(out, "        # go install fetches the release through the Go module proxy and checks its modules")
	fmt.Fprintln(out, "        # against the Go checksum database, not the release's own files; a runner must not")
	fmt.Fprintln(out, "        # exempt github.com/mr-karan/writ with GONOSUMDB or GOPRIVATE. The binary goes to a")
	fmt.Fprintln(out, "        # directory of this job only, and a failed step stops the job, so a runner never")
	fmt.Fprintln(out, "        # plans with a writ left from before.")
	fmt.Fprintln(out, "        run: |")
	fmt.Fprintln(out, "          set -euo pipefail")
	fmt.Fprintln(out, `          export GOBIN="$RUNNER_TEMP/writ-bin"`)
	fmt.Fprintln(out, `          rm -rf "$GOBIN"`)
	fmt.Fprintln(out, `          go install "github.com/mr-karan/writ/cmd/writ@${WRIT_VERSION}"`)
	fmt.Fprintln(out, `          test "$("$GOBIN/writ" version)" = "writ ${WRIT_VERSION}"`)
	fmt.Fprintln(out, `          echo "$GOBIN" >> "$GITHUB_PATH"`)
	fmt.Fprintln(out, "      - name: Plan")
	fmt.Fprintln(out, "        env:")
	fmt.Fprintf(out, "          WRIT_SERVER: %s\n", yamlString(r.base))
	fmt.Fprintf(out, "          WRIT_REPO: %s\n", yamlString(o.Repo))
	fmt.Fprintln(out, "          WRIT_STACK: ${{ matrix.stack }}")
	fmt.Fprintln(out, "          WRIT_ENV: ${{ matrix.env }}")
	fmt.Fprintln(out, "        # writ asks GitHub Actions for the job's ID token, whose audience is WRIT_SERVER;")
	fmt.Fprintln(out, "        # writ serve refuses a commit or a repository other than the token's. While writ serve's")
	fmt.Fprintf(out, "        # plan slots are busy, as with overlapping runs, writ asks again for up to %s.\n", slotWait)
	fmt.Fprintln(out, "        run: |")
	fmt.Fprintln(out, "          set -euo pipefail")
	fmt.Fprintf(out, "          writ plan --remote --env \"$WRIT_ENV\" --stack \"$WRIT_STACK\" --repo \"$WRIT_REPO\" --commit \"$GITHUB_SHA\" --slot-wait %s\n", slotWait)
	fmt.Fprintln(out, "  writ:")
	fmt.Fprintln(out, "    # The one check to require. It passes when every plan passed, or when the plan job was")
	fmt.Fprintln(out, "    # skipped, as for a fork's or Dependabot's pull request, whose per-stack checks GitHub")
	fmt.Fprintln(out, "    # never makes; it fails when a plan failed or was cancelled.")
	fmt.Fprintln(out, "    needs: plan")
	fmt.Fprintln(out, "    if: always()")
	fmt.Fprintf(out, "    runs-on: %s\n", yamlString(runsOn))
	fmt.Fprintln(out, "    timeout-minutes: 5")
	fmt.Fprintln(out, "    steps:")
	fmt.Fprintln(out, "      - name: Check the plans")
	fmt.Fprintln(out, "        env:")
	fmt.Fprintln(out, "          RESULT: ${{ needs.plan.result }}")
	fmt.Fprintln(out, "        run: |")
	fmt.Fprintln(out, "          set -euo pipefail")
	fmt.Fprintln(out, `          case "$RESULT" in`)
	fmt.Fprintln(out, `            success|skipped) echo "writ plan: $RESULT" ;;`)
	fmt.Fprintln(out, `            *) echo "writ plan: $RESULT" >&2; exit 1 ;;`)
	fmt.Fprintln(out, "          esac")

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
	fmt.Fprintf(out, "\n# Repository settings of %s: protect %s, and require the one check named writ, not\n# the per-stack plan checks.\n", d.Repo.Project, d.Repo.DefaultBranch)
	if !read {
		fmt.Fprintln(out, "# max-parallel is 1: an admin who runs writ ci github again gets writ serve's own limit.")
	}
	if r.dev {
		fmt.Fprintf(out, "# %s is the dev control plane, on loopback: a GitHub-hosted runner cannot reach it.\n", r.base)
	}
	return nil
}
