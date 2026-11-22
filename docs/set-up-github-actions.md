# Set up GitHub Actions

With GitHub Actions and Writ, every pull request and every push to the default branch plans each stack, people approve each plan, and after the merge a person applies the approved plan whose sources equal the merged commit's.

No job needs a Git checkout, cloud credentials, or state encryption keys. Each job authenticates with its GitHub Actions ID token. See [How a plan becomes an apply](plan-to-apply.md) and [Approvals](approvals.md) for the server-side rules.

**Limits.** The workflow is verified against a local GitHub Enterprise Server stand-in and Actions token endpoint, not github.com or a live GitHub Enterprise Server. A GitHub Actions token plans; approving a whole run and applying after a merge from a job are not built for GitHub yet, so people approve and apply.

## Before you begin

- A team's `writ serve` with a `github-actions` issuer named `github`: `url = "https://token.actions.githubusercontent.com"` for github.com, or `https://HOST/_services/token` for GitHub Enterprise Server ([Run a control plane](run.md)).
- Your GitHub repository registered as a [repository](add-repository.md) at a `forge` of kind `github`.
- A people issuer, so people can approve and apply ([Configure sign-in](sign-in.md)).

## Print the workflow and the config

Run `writ ci github --repo NAME` to generate the workflow and the `writ serve` team, grant, and approval rule for that repository's numeric ID. It checks that the repository is registered, is a GitHub repository, and has a stack the control plane serves. `--writ-version` names the release the workflow installs, and defaults to the CLI's own version; `--envs` narrows the environments.

```
$ writ ci github --repo web --writ-version v0.1.8
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
    runs-on: ubuntu-24.04
    timeout-minutes: 30
    strategy:
      fail-fast: false
      matrix:
        include:
          - stack: "app"
            env: "prod"
    steps:
      - name: Install writ
        env:
          WRIT_VERSION: "v0.1.8"
        run: |
          sudo apt-get update -qq
          sudo apt-get install -y -qq minisign
          curl -fsSL "https://github.com/mr-karan/writ/releases/download/$WRIT_VERSION/install.sh" | sh -s -- --version "$WRIT_VERSION"
          echo "$HOME/.local/bin" >> "$GITHUB_PATH"
      - name: Plan
        env:
          WRIT_SERVER: "http://127.0.0.1:34977"
          WRIT_REPO: "web"
          WRIT_STACK: ${{ matrix.stack }}
          WRIT_ENV: ${{ matrix.env }}
        # writ asks GitHub Actions for the job's ID token, whose audience is WRIT_SERVER;
        # writ serve refuses a commit or a repository other than the token's.
        run: writ plan --remote --env "$WRIT_ENV" --stack "$WRIT_STACK" --repo "$WRIT_REPO" --commit "$GITHUB_SHA"

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

# Repository settings of acme/web: protect main, and require the writ plan checks to pass.
```

Output of a real run against a local test `writ serve` on loopback, 2026-11-22. It shows the shape only: the `http://127.0.0.1:34977` server URL and the repository ID are the test's. Run the command against your own control plane and copy what it prints.

1. Save the YAML it printed as `.github/workflows/writ-plan.yml`.
2. Add the HCL blocks it printed to `/etc/writ/serve`, with a grant of `approve` and `apply` for a team of people, and restart `writ serve`.
3. Protect the default branch and require the `writ plan` checks to pass.

The workflow uses no third-party action, so it has none to pin by commit SHA. `install.sh` checks the release's minisign signature and SHA-256 before it installs `writ`. Matrix values reach the shell through `env`, never through an expression in `run`.

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
- **`issuer URL is not the CI issuer of a forge this control plane knows`**: The `github-actions` issuer's `url` is neither github.com's nor `https://HOST/_services/token`.

## Next steps

- [Plan and apply](apply.md): approve, apply, recover lost responses, and override rules.
- [Get changes in seconds with a webhook](webhooks.md): GitHub push webhooks.
- [Configure sign-in](sign-in.md): browser and CLI sign-in for people.
