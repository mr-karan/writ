# Set up GitHub Actions

With GitHub Actions and Writ, every pull request and every push to the default branch plans each stack, people approve each plan, and after the merge a person applies the approved plan whose sources equal the merged commit's.

No job needs a Git checkout, cloud credentials, or state encryption keys. Each job authenticates with its GitHub Actions ID token. See [How a plan becomes an apply](plan-to-apply.md) and [Approvals](approvals.md) for the server-side rules.

**Limits.** The workflow is verified against a local GitHub Enterprise Server stand-in and Actions token endpoint, not github.com or a live GitHub Enterprise Server. A GitHub Actions token plans; approving a whole run and applying after a merge from a job are not built for GitHub yet, so people approve and apply.

## Before you begin

- A team's `writ serve` with a `github-actions` issuer named `github`: `url = "https://token.actions.githubusercontent.com"` for github.com, or `https://HOST/_services/token` for GitHub Enterprise Server ([Run a control plane](run.md)).
- Your GitHub repository registered as a [repository](add-repository.md) at a `forge` of kind `github`.
- A people issuer, so people can approve and apply ([Configure sign-in](sign-in.md)).

## Print the workflow and the config

Run `writ ci github --repo NAME` to generate the workflow and the `writ serve` team, grant, and approval rule for that repository's numeric ID. It checks that the repository is registered, is a GitHub repository, and has a stack the control plane serves, and refuses more than the 256 jobs one matrix makes.

| Flag | Meaning |
| --- | --- |
| `--writ-version V` | The release the workflow installs. Default: the CLI's own version. |
| `--runs-on LABEL` | The runner label. Default `ubuntu-24.04` for a repository at github.com; required for GitHub Enterprise Server, whose runner labels are its administrators'. |
| `--envs a,b` | Narrow the environments. |

`strategy.max-parallel` is the lesser of the server's `limits.plans` and `limits.plans_per_principal`, since every job of a run is one principal, its actor. `writ ci github` reads both from `GET /v1/config`, which shows the limits only to an admin; for anyone else it writes 1, with a comment to run it again as an admin.

Each plan step passes `--slot-wait 15m`, and the job's `timeout-minutes` is 45. When writ serve refuses a plan with 429 or 503 because its plan slots are busy, as when two runs overlap, `writ plan --remote` asks again with exponential backoff and jitter, from 5 seconds to a minute, for up to 15 minutes, fetching a fresh ID token before each try. A refused plan spends no token. If the slots stay busy past the wait, the step prints writ serve's refusal and fails.

```
$ writ ci github --repo web --writ-version v0.1.8 --runs-on self-hosted-linux
# .github/workflows/writ-plan.yml: each pull request and each push to the default
# branch plans each stack in each environment with writ serve, which takes the
# repository and the commit from the job's ID token and reads the commit itself, so no
# job checks it out or holds a cloud credential. People approve each plan, in /ui/plans
# or with writ approve --remote ID. After the merge, a person applies a pull request's
# approved plan with writ apply --remote --merged --env ENV --stack NAME --repo REPO
# --commit SHA, naming the merged commit, whose sources must be the plan's.
name: writ plan
on:
  pull_request:
  push:
    branches: ["main"]
permissions:
  contents: read
  id-token: write
jobs:
  plan:
    # A pull request from a fork and Dependabot's runs get no ID token, so the job skips
    # them, for any event that carries a pull request; the writ job below reports it.
    if: github.actor != 'dependabot[bot]' && (!github.event.pull_request || github.event.pull_request.head.repo.full_name == github.repository)
    runs-on: "self-hosted-linux"
    timeout-minutes: 45
    strategy:
      fail-fast: false
      # writ ci github could not read writ serve's limits, which only an admin may: an admin
      # who runs it again gets the lesser of limits.plans and limits.plans_per_principal.
      max-parallel: 1
      matrix:
        include:
          - stack: "app"
            env: "prod"
    steps:
      - name: Install writ
        env:
          WRIT_VERSION: "v0.1.8"
        # install.sh checks the release's minisign signature. A runner without minisign
        # gets it from apt-get with sudo, as an Ubuntu runner has them.
        run: |
          if ! command -v minisign >/dev/null; then
            sudo apt-get update -qq
            sudo apt-get install -y -qq minisign
          fi
          curl -fsSL "https://github.com/mr-karan/writ/releases/download/$WRIT_VERSION/install.sh" | sh -s -- --version "$WRIT_VERSION"
          echo "$HOME/.local/bin" >> "$GITHUB_PATH"
      - name: Plan
        env:
          WRIT_SERVER: "http://127.0.0.1:46435"
          WRIT_REPO: "web"
          WRIT_STACK: ${{ matrix.stack }}
          WRIT_ENV: ${{ matrix.env }}
        # writ asks GitHub Actions for the job's ID token, whose audience is WRIT_SERVER;
        # writ serve refuses a commit or a repository other than the token's. While writ serve's
        # plan slots are busy, as with overlapping runs, writ asks again for up to 15m.
        run: writ plan --remote --env "$WRIT_ENV" --stack "$WRIT_STACK" --repo "$WRIT_REPO" --commit "$GITHUB_SHA" --slot-wait 15m
  writ:
    # The one check to require. It passes when every plan passed, or when the plan job was
    # skipped, as for a fork's or Dependabot's pull request, whose per-stack checks GitHub
    # never makes; it fails when a plan failed or was cancelled.
    needs: plan
    if: always()
    runs-on: "self-hosted-linux"
    timeout-minutes: 5
    steps:
      - name: Check the plans
        env:
          RESULT: ${{ needs.plan.result }}
        run: |
          case "$RESULT" in
            success|skipped) echo "writ plan: $RESULT" ;;
            *) echo "writ plan: $RESULT" >&2; exit 1 ;;
          esac

# The writ serve config: the team and grant of this repository's workflow runs, by the
# repository's id, 777001, which a renamed or moved repository keeps, and an approval rule.
# The issuer named github is GitHub Actions' token issuer: kind github-actions, at
# https://token.actions.githubusercontent.com, or https://HOST/_services/token on GitHub
# Enterprise Server.
team "web_ci" {
  issuer = "github"
  match  = { repository_id = ["777001"] }
}
grant {
  teams  = ["web_ci"]
  stacks = ["app"]
  envs   = ["prod"]
  can    = ["read", "plan"]
}
approval {
  stacks = ["app"]
  envs   = ["prod"]
  risks  = ["create", "update", "replace", "delete", "data-loss"]
  need   = 1
}
# Grant approve and apply on these stacks to a team of a people issuer.

# Repository settings of acme/web: protect main, and require the one check named writ, not
# the per-stack plan checks.
# max-parallel is 1: an admin who runs writ ci github again gets writ serve's own limit.
```

Output of a real run against a local test `writ serve` on loopback, with a GitHub Enterprise Server stand-in, 2026-11-22. It shows the shape only: the `http://127.0.0.1:46435` server URL, the runner label, and the repository ID are the test's. Run the command against your own control plane and copy what it prints.

1. Save the YAML it printed as `.github/workflows/writ-plan.yml`.
2. Add the HCL blocks it printed to `/etc/writ/serve`, with a grant of `approve` and `apply` for a team of people, and restart `writ serve`.
3. Protect the default branch and require the one check named `writ`, not the per-stack `plan` checks.

The workflow uses no third-party action, so it has none to pin by commit SHA. `install.sh` checks the release's minisign signature and SHA-256 before it installs `writ`. The install step needs `minisign`; on a runner without it, the step installs it with `sudo apt-get`, so a runner without `minisign` must be Ubuntu or Debian with `sudo`. Matrix values reach the shell through `env`, never through an expression in `run`.

## Pull requests from forks and Dependabot

The plan job skips a pull request whose head is in a fork, and every run started by `dependabot[bot]`. GitHub gives neither an ID token, so the plan could only fail. The gate keys on `github.event.pull_request`, so it holds for any event that carries a pull request, and the plan job runs as usual if you add `workflow_dispatch` or `merge_group`.

GitHub evaluates a job's `if` before it expands its matrix, so a skipped plan job makes none of its per-stack checks, and a required per-stack check would stay pending. The `writ` job, with `needs: plan` and `if: always()`, is the one check to require: it passes when the plan job succeeded or was skipped, and fails when it failed or was cancelled. A fork's pull request then passes `writ` without a plan; review it, and plan it from a branch of the repository if it should be applied.

`writ serve` refuses a token whose signed `event_name` is `pull_request_target`, which runs with the repository's permissions for a pull request that may come from a fork; plan on `pull_request` or `push`. It refuses a fork's plan too. For a token whose ref is `refs/pull/N/merge` or `refs/pull/N/head`, it reads pull request `N` through the forge's read token and plans only when the pull request's head repository is the token's `repository_id`. Any error reading it refuses the plan.

## How a job gets its token

The workflow sets `permissions: id-token: write`. With `WRIT_ID_TOKEN` unset, `writ` sees `ACTIONS_ID_TOKEN_REQUEST_URL` and `ACTIONS_ID_TOKEN_REQUEST_TOKEN` and asks GitHub Actions for an ID token whose audience is `WRIT_SERVER`, which must equal `url` in `/etc/writ/serve`. `writ` never prints the token. `WRIT_ID_TOKEN`, when set, comes first.

`writ serve` binds the token to the repository whose numeric ID is the token's `repository_id` at the GitHub forge of the token's issuer: github.com for `token.actions.githubusercontent.com`, and `https://HOST` for `https://HOST/_services/token`. A job plans only its own `sha`, and the forge must show that the commit is on the token's `ref`.

## Apply after the merge

A pull request's plan is of its merge commit, `refs/pull/N/merge`, so it applies after the merge. A person whose team may apply names the merged commit of the default branch:

```sh
writ apply --remote --merged --env prod --stack app --repo web --commit MERGED_SHA
```

Writ applies the approved plan whose sources equal that commit's. See [Apply after a merge](apply.md#apply-after-a-merge).

## Troubleshooting

- **`no token for URL`**: Add `permissions: id-token: write` to the workflow or the job.
- **`GitHub Actions ... answered 403 to the ID token request`**: The job lacks `id-token: write`. GitHub gives no ID token to a pull request from a fork.
- **`401 auth: audience: aud is not writ serve's URL`**: Set `WRIT_SERVER` to `url` in `/etc/writ/serve` exactly (same scheme, no trailing slash).
- **`403 denied: project OWNER/REPO (id N) is not a repository of this control plane`**: The job's repository is not registered at a `github` forge whose `url` matches the token's issuer.
- **`a job of a pull_request_target event may not plan`**: Trigger the workflow on `pull_request`, not `pull_request_target`.
- **`still refused the plan with 429 after waiting 15m0s for a plan slot`**: Plans held writ serve's slots for the whole wait. Lower `max-parallel`, raise `limits.plans_per_principal`, or run again.
- **`pull request N of repository NAME comes from another repository`**: The pull request's head is in a fork. Writ plans only a pull request whose head is in the repository itself.
- **`--runs-on LABEL`**: The repository is at a GitHub Enterprise Server. Name the label of a Linux runner.
- **`issuer URL is not the CI issuer of a forge this control plane knows`**: The `github-actions` issuer's `url` is neither github.com's nor `https://HOST/_services/token`.

## Next steps

- [Plan and apply](apply.md): approve, apply, recover lost responses, and override rules.
- [Get changes in seconds with a webhook](webhooks.md): GitHub push webhooks.
- [Configure sign-in](sign-in.md): browser and CLI sign-in for people.
