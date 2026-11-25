# Quick start

Plan and apply an OpenTofu stack locally in four steps with no cloud account, then [set up Writ for a team](#set-up-writ-for-a-team) with Postgres, OIDC sign-in, and peer approvals.

## Try Writ on your own

`writ serve --dev` runs a single-user [control plane](glossary.md) on loopback with SQLite, no sign-in, and no approval rules. The demo stack uses `hashicorp/random` and contacts no cloud.

### Before you begin

- Linux and [`minisign`](https://jedisct1.github.io/minisign/), which the install script needs.
- Git with `user.name` and `user.email` set.
- [OpenTofu 1.13.9](https://opentofu.org/docs/intro/install/) on `PATH`, and network access to `registry.opentofu.org`.

### 1. Install writ

The script installs `writ` v0.1.9 to `~/.local/bin` and verifies its minisign signature and SHA-256 checksum:

```sh
curl -fsSL https://github.com/mr-karan/writ/releases/download/v0.1.9/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
writ version
```

Or build from source with Go 1.27.3+ and `just build`, or run `nix develop` for Go, `just`, and OpenTofu 1.13.9. [Install](install.md) covers the container image and Nix.

### 2. Start the dev control plane

Leave this running in one terminal:

```
$ writ serve --dev
writ serve --dev: http://127.0.0.1:7340, for alice only
data: /home/alice/.local/share/writ/dev (control.db, state/, keys/)
OpenTofu runs get PATH=/usr/local/bin:/usr/bin:/bin and this shell's TF_VAR_ variables, and no other variable of this shell.
control UI: http://127.0.0.1:7340/ui/login/dev?code=KLBC-ZBYU (works once, for 10 minutes; writ ui prints another)
ready in 102 ms
```

Open the `control UI` link to see plans and runs. OpenTofu runs get an isolated `HOME` and only `PATH` and `TF_VAR_` variables from your shell, so providers do not pick up local cloud credentials.

### 3. Create a stack and add its repository

In a second terminal, with `writ` on `PATH`, create a Git repository with one [stack](glossary.md) that uses `backend "http" {}` and a committed `.terraform.lock.hcl`:

```sh
mkdir -p ~/writ-demo/app
cd ~/writ-demo
git init -b main
cat > app/main.tf <<'EOF'
terraform {
  backend "http" {}

  required_providers {
    random = { source = "hashicorp/random", version = "3.9.1" }
  }
}

resource "random_pet" "name" {
  length = 2
}

output "name" {
  value = random_pet.name.id
}
EOF
(cd app && tofu providers lock)
git add .
git commit -m "one stack"
```

Register it with the dev server:

```
$ writ repo add .
Repository writ-demo: /home/alice/writ-demo, path the root, branch main.
Read main at 570edf03 from the local repository: 1 file, 1 stack.
STACK  SERVED
app    dev
Next: in a clean work tree of it, writ plan --remote plans the commit you are on.
```

### 4. Plan and apply

Plan the commit, then apply the plan ID. The first plan downloads `hashicorp/random` into the plugin cache.

```
$ cd app
$ writ plan --remote
commit plan of app@dev: repository writ-demo at 570edf03 (main), sent to http://127.0.0.1:7340
OpenTofu 1.13.9 plan of app@dev

  + random_pet.name (create)

Plan: 1 to add, 0 to change, 0 to destroy.
Outputs that change: name.
Plan 7ff2ad97b106fcbf4b4633baee25626a for dev (app), by alice (dev), expires 2026-11-17T05:32:18Z.
It is a commit plan of writ-demo at 570edf03.
It needs no approval. Apply it with: writ apply --remote 7ff2ad97b106fcbf4b4633baee25626a
$ writ apply --remote 7ff2ad97b106fcbf4b4633baee25626a
Run I3EP3I6DTYNC5PMVULSW6JYINL of plan 7ff2ad97b106fcbf4b4633baee25626a for dev (app), by alice (dev).
OpenTofu apply of app@dev
  random_pet.name: create complete
Apply: 1 added, 0 changed, 0 destroyed.
State version 1 of app@dev.
Run I3EP3I6DTYNC5PMVULSW6JYINL of plan 7ff2ad97b106fcbf4b4633baee25626a for dev (app), by alice (dev): applied at 2026-11-16T05:32:19Z.
```

Real run, 2026-11-16. A second `writ plan --remote` returns `no-changes`. List runs with `writ runs --remote`:

```
$ writ runs --remote --stack app
RUN                         STARTED               STACK  STATUS   BY           PLAN
I3EP3I6DTYNC5PMVULSW6JYINL  2026-11-16T05:32:18Z  app    applied  alice (dev)  7ff2ad97b106fcbf4b4633baee25626a
```

Press Ctrl-C in the first terminal when done.

## Set up Writ for a team

A team `writ serve` adds Postgres, OIDC sign-in for CI jobs and people, teams, grants, approval rules, and per-run `provider_uids`.

### On GitHub

Use a `github` forge with a fine-grained read token (a GitHub App is not built yet), a `github-actions` issuer, and a `people` issuer so teammates can approve and apply:

```hcl
issuer "github" {
  kind     = "github-actions"
  identity = "github"
  url      = "https://token.actions.githubusercontent.com"
}

forge "github" {
  kind            = "github"
  read_token_file = "secrets/github_read"
}

repo {
  name    = "infra"
  forge   = "github"
  project = "acme/infra"
}
```

Then run `writ ci github --repo infra` to generate `.github/workflows/writ-plan.yml` and the matching `team`, `grant`, and `approval` blocks, and protect `main` with the single required check named `writ`. See [Set up GitHub Actions](set-up-github-actions.md) for the complete GitHub workflow and server setup.

### End-to-end team walkthrough (GitLab)

The full walkthrough below connects one GitLab project (`acme/infra`, ID `4242`) with one stack (`app`) in one env (`prod`). Its transcripts come from a real run against Postgres, a local OIDC test issuer, and a local GitLab stand-in.

### Before you begin

- A Linux host with Postgres, `writ`, and OpenTofu 1.13.9, behind a TLS-terminating proxy for `https://writ.example.com`.
- A GitLab project (`acme/infra`, numeric ID `4242`).
- A `writ` container image your GitLab runners can pull, for example `registry.example.com/writ:0.1.9` copied from [`ghcr.io/mr-karan/writ:0.1.9`](install.md#the-container-image).

### 1. Commit the stack to the default branch

Commit `app/` (its `.tf` files and `.terraform.lock.hcl`) to `main`. `writ serve` discovers stacks on the default branch, and `writ ci gitlab` needs at least one discovered stack.

### 2. Write the server config

Create `/etc/writ/serve`:

```hcl
url             = "https://writ.example.com"
listen          = "127.0.0.1:8443"
trusted_proxies = ["127.0.0.1/32"]
database        = "postgres://writ@/writ?host=/run/postgresql"
provider_uids   = "61000-61063"

issuer "gitlab" {
  kind     = "gitlab-ci"
  identity = "gitlab"
  url      = "https://gitlab.com"
}

issuer "sso" {
  kind               = "people"
  identity           = "gitlab"
  url                = "https://gitlab.com"
  client_id          = "8f1c0b7e4d2a"
  client_secret_file = "secrets/oidc_client_secret"
  groups             = "userinfo"
}

forge "gitlab" {
  kind            = "gitlab"
  url             = "https://gitlab.com"
  read_token_file = "secrets/gitlab_read"
}

repo {
  name    = "infra"
  forge   = "gitlab"
  project = "acme/infra"
}

team "infra_mr" {
  issuer = "gitlab"
  match  = { project_id = ["4242"], pipeline_source = ["merge_request_event"] }
}

team "infra_reviewers" {
  issuer = "gitlab"
  match  = { project_id = ["4242"], user_access_level = ["maintainer", "owner"] }
}

team "infra_main" {
  issuer = "gitlab"
  match  = { project_id = ["4242"], ref_path = ["refs/heads/main"], ref_protected = ["true"] }
}

team "acme" {
  issuer = "sso"
  match  = { groups = ["acme"] }
}

grant {
  teams  = ["infra_mr"]
  stacks = ["app"]
  envs   = ["prod"]
  can    = ["read", "plan"]
}

grant {
  teams  = ["infra_reviewers"]
  stacks = ["app"]
  envs   = ["prod"]
  can    = ["approve"]
}

grant {
  teams  = ["infra_main"]
  stacks = ["app"]
  envs   = ["prod"]
  can    = ["read", "apply"]
  risks  = ["create", "update", "replace", "delete"]
}

grant {
  teams  = ["acme"]
  stacks = ["app"]
  envs   = ["prod"]
  can    = ["read"]
}

approval {
  stacks = ["app"]
  envs   = ["prod"]
  risks  = ["create", "update", "replace", "delete", "data-loss"]
  need   = 1
}

env "prod" {
  key = "keys/prod.key"
}

tofu {
  binary       = "tofu"
  plugin_cache = "/var/lib/writ-tofu/plugins"
  scratch_dir  = "/var/lib/writ-tofu/runs"
}
```

Create the three files it names, each mode `0600` and owned by the `writ` service user:

| File | Content |
| --- | --- |
| `secrets/gitlab_read` | A GitLab token with `read_api` scope ([Forges and their tokens](add-repository.md#forges-and-their-tokens)). |
| `secrets/oidc_client_secret` | The secret of your GitLab OAuth application (`client_id`), with scopes `openid profile email` and two redirect URIs, `https://writ.example.com/ui/callback` and `http://127.0.0.1/callback` ([Configure sign-in](sign-in.md#set-up-writ-login)). |
| `keys/prod.key` | 32 random bytes that [seal](glossary.md) state and saved plans: `head -c 32 /dev/urandom > keys/prod.key`. Back it up offline. |

Both issuers share `identity = "gitlab"`, so a person's browser or CLI session and the CI jobs they start count as one person, who cannot self-approve. To let people approve in the control UI too, add `"approve"` to the `acme` grant. For S3 state and AWS roles, see [AWS roles and credentials](aws-credentials.md).

### 3. Check the config and start the server

`--check` parses the config, prints grants and approval rules, and verifies `tofu version` is 1.13.9. Run with user-switching capabilities, it also tests that provider UIDs cannot read secret files or reach EC2 metadata endpoints:

```
$ writ serve --config serve --check
config serve: ok
approvals: rule 1: app in prod with create update replace delete data-loss needs 1
TEAM             ENV   STACKS  CAN         RISKS
infra_main       prod  app     read apply  create update replace delete
infra_reviewers  prod  app     approve     -
tofu: /nix/store/60vqi4v8n11afd5ks89j1kn7pgwfrww5-opentofu-1.13.9/bin/tofu is OpenTofu 1.13.9: ok
warning[usage]
  isolation not checked: run writ as provider uid 61000: fork/exec /var/tmp/writ-landing2/qs/bin/writ: operation not permitted; writ serve checks its capabilities at start
```

Excerpt of a real run, 2026-11-16, without `CAP_SETUID`, so isolation warned.

Start the server with `writ serve --config serve`. On NixOS, [the module](run.md#on-nixos) sets up Postgres, capabilities, the metadata firewall, and `--check`.

### 4. Sign in from your terminal

Run `writ login https://writ.example.com`. GitLab.com publishes no OAuth device endpoint, so `writ login` uses a browser sign-in and receives the callback on `127.0.0.1`:

```
$ writ login http://127.0.0.1:35309
To sign in, open https://127.0.0.1:36093/oauth/authorize?client_id=writ-e2e&... in a browser on this computer.
The sign-in expires in 10 minutes.
Signed in to http://127.0.0.1:35309 as bob (gitlab/user/2002).
$ writ whoami
http://127.0.0.1:35309 (from your session), session of bob (gitlab/user/2002, issuer sso), ends 2026-11-16T20:39:54Z
```

Or open `https://writ.example.com/ui/repos/infra` in a browser and sign in through GitLab.

### 5. Add the GitLab CI jobs

Add these jobs to `.gitlab-ci.yml` in a merge request, or generate them with `writ ci gitlab --repo infra --image registry.example.com/writ:0.1.9`:

```yaml
stages: [writ-plan, writ-approve, writ-apply]
include:
  - component: $CI_SERVER_FQDN/platform/writ-ci/writ-plan@0.1.0
    inputs:
      server: https://writ.example.com
      env: prod
      stack: app
      image: registry.example.com/writ:0.1.9
  - component: $CI_SERVER_FQDN/platform/writ-ci/writ-approve@0.1.0
    inputs:
      server: https://writ.example.com
      image: registry.example.com/writ:0.1.9
  - component: $CI_SERVER_FQDN/platform/writ-ci/writ-apply@0.1.0
    inputs:
      server: https://writ.example.com
      env: prod
      stack: app
      image: registry.example.com/writ:0.1.9
```

Host the components from [`ci/gitlab`](../ci/gitlab/README.md) at `platform/writ-ci`, or change the path. Protect `main` and enable **Pipelines must succeed** in the project settings. No job checks out code or holds cloud credentials ([Set up GitLab CI](set-up-gitlab-ci.md)).

### 6. Plan, approve, and apply

The merge request pipeline plans `app@prod` and prints the masked plan:

```
OpenTofu 1.13.9 plan of app@prod

  + random_pet.name (create)
  + terraform_data.note (create)
  + terraform_data.secret (create)
  + module.cert.tls_private_key.key (create)

Plan: 4 to add, 0 to change, 0 to destroy.
Outputs that change: pet.
```

If `alice`, the author, tries to approve it, Writ refuses:

```
$ writ approve --remote 927b69ec7af23641c68c3ce3cce370ec
error: denied: gitlab acme/infra #1849401 (alice) made plan 927b69ec7af23641c68c3ce3cce370ec and cannot approve it: it needs 1 approval from someone else
```

If the merge request merges without approval, the `main` apply job refuses:

```
$ writ apply --remote --merged --env prod --stack app
error: denied: plan 927b69ec7af23641c68c3ce3cce370ec of app@prod matches the sources at 3008b0c3 and needs 1 approval; approve it with writ approve --remote 927b69ec7af23641c68c3ce3cce370ec
```

`bob`, a maintainer, approves it:

```
$ writ approve --remote 927b69ec7af23641c68c3ce3cce370ec
Approved plan 927b69ec7af23641c68c3ce3cce370ec for prod (app): 1 of 1 approvals (gitlab acme/infra #1849401 (bob)), approved.
```

After merge, the `main` job applies the exact saved plan `bob` approved:

```
OpenTofu apply of app@prod
  terraform_data.note: create complete
  terraform_data.secret: create complete
  random_pet.name: create complete
  module.cert.tls_private_key.key: create complete
Apply: 4 added, 0 changed, 0 destroyed.
State version 1 of app@prod.
```

Real runs, 2026-11-16. If `alice` pushes another commit, a new plan is made and the old approved plan can no longer apply ([Why an approved plan can still be refused](plan-lifecycle.md#why-an-approved-plan-can-still-be-refused)).

## Troubleshooting

| Problem | Fix |
| --- | --- |
| `writ: no control plane` | Start `writ serve --dev` in another terminal, or make `XDG_DATA_HOME` match in both terminals. |
| `writ: git is not installed` | Put `git` on `PATH` ([Install: Git](install.md#git)). |
| OpenTofu version error | Writ requires OpenTofu `1.13.9`. Check `tofu version`. |
| Dirty work tree refused | Commit first. Writ plans commits only. |
| Dev UI sign-in link expired | A link works once, for 10 minutes. `writ ui` prints a new one. |
| GitLab redirect URI mismatch | List `https://writ.example.com/ui/callback` and `http://127.0.0.1/callback` on separate lines in the GitLab OAuth application. |

## Next steps

- [Set up GitHub Actions](set-up-github-actions.md) and [Set up GitLab CI](set-up-gitlab-ci.md)
- [Configure approvals](approvals.md), [Plan and apply](apply.md), and [Configure sign-in](sign-in.md)
- [Write and test policies](write-a-policy.md) and [Get notified](notifications.md)
- [Writ vs plain GitLab CI](compare-gitlab-ci.md)
