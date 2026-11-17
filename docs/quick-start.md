# Quick start

Plan and apply a real OpenTofu stack with Writ on your own machine. You don't need a cloud account, and it takes four steps. When you're ready, [set it up for a team](#set-up-writ-for-a-team): a merge request job plans a stack, its author can't approve the plan, and a second person does.

## Try Writ on your own

`writ serve --dev` runs a [control plane](glossary.md) for you alone, on loopback, with its records in SQLite. It's the same code as a team's `writ serve`, without sign-in or approval rules. The stack below uses `hashicorp/random`, which doesn't reach any cloud.

### Before you begin

- Linux, and [`minisign`](https://jedisct1.github.io/minisign/), which the install script needs to check the release's signature.
- Git, with your commit name and email set.
- [OpenTofu 1.13.9](https://opentofu.org/docs/intro/install/) on your `PATH`, and network access to `registry.opentofu.org`.

### 1. Install writ

The install script puts `writ` v0.1.0 in `~/.local/bin`, after it checks the release's signature and checksum:

```
curl -fsSL https://github.com/mr-karan/writ/releases/download/v0.1.0/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
writ version
```

To build from source instead, you need Go 1.27.3 or later and [`just`](https://github.com/casey/just). Run `git clone https://github.com/mr-karan/writ`, then `just build` in it, and put its `bin` on your `PATH`. With Nix, `nix develop` in the repository gives you Go, `just`, and OpenTofu 1.13.9. [Install](install.md) has the other ways to get `writ`.

### 2. Start the dev control plane

Run this in one terminal, and leave it running:

```
$ writ serve --dev
writ serve --dev: http://127.0.0.1:7340, for alice only
data: /home/alice/.local/share/writ/dev (control.db, state/, keys/)
OpenTofu runs get PATH=/usr/local/bin:/usr/bin:/bin and this shell's TF_VAR_ variables, and no other variable of this shell.
control UI: http://127.0.0.1:7340/ui/login/dev?code=KLBC-ZBYU (works once, for 10 minutes; writ ui prints another)
ready in 102 ms
```

Output of a real run, 2026-11-16.

Open the `control UI` link to sign your browser in to the control UI, where you can see each plan and run.

Your OpenTofu runs get no other variable from that shell, and each run's `HOME` is its own directory. So a provider won't find a cloud credential in your shell's environment or your home directory.

### 3. Create a stack and add its repository

Open a second terminal, and check that `writ` is on its `PATH` there too. Then create a Git repository with one [stack](glossary.md): a directory of `.tf` files with a committed `.terraform.lock.hcl`. With `backend "http" {}`, `writ serve` keeps its state for you.

```
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

Then add the repository to the control plane:

```
$ writ repo add .
Repository writ-demo: /home/alice/writ-demo, path the root, branch main.
Read main at 570edf03 from the local repository: 1 file, 1 stack.
STACK  SERVED
app    dev
Next: in a clean work tree of it, writ plan --remote plans the commit you are on.
```

### 4. Plan and apply

Plan the commit, then apply the plan by its id. Your first plan downloads the provider, so it takes longer. The last command plans again, to show that nothing is left to change:

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
$ writ plan --remote
commit plan of app@dev: repository writ-demo at 570edf03 (main), sent to http://127.0.0.1:7340
OpenTofu 1.13.9 plan of app@dev

No changes.
Plan 096e5fe23db797b5b8b81eb9722c2199 for dev (app), by alice (dev), expires 2026-11-17T05:32:20Z.
It is a commit plan of writ-demo at 570edf03.
It has no change to apply.
```

Output of a real run, 2026-11-16.

To list the runs, newest first, use `writ runs --remote`:

```
$ writ runs --remote --stack app
RUN                         STARTED               STACK  STATUS   BY           PLAN
I3EP3I6DTYNC5PMVULSW6JYINL  2026-11-16T05:32:18Z  app    applied  alice (dev)  7ff2ad97b106fcbf4b4633baee25626a
```

The dev control plane has no approval rule, so your plans don't need approvals. When you're done, stop it with Ctrl-C.

## Set up Writ for a team

On a team, each merge request should show the plan it would apply, and someone other than its author should approve that plan. A team's `writ serve` adds what the dev control plane leaves out: Postgres, OIDC sign-in for CI jobs and people, teams and grants, approval rules, and its own uid for each run.

Here you set up one GitLab project, `acme/infra`, with one stack, `app`, in one environment, `prod`. CI jobs plan, approve, and apply. People sign in with `writ login` from a terminal, or to the control UI in a browser, to read the plans and runs.

**Limits.**

- No pipeline on a real GitLab has run these steps. The output below comes from `TestTofuApproveApply` in [`e2e/tofu_apply_test.go`](../e2e/tofu_apply_test.go), which runs them against a team's `writ serve` with Postgres, a test issuer, and a stand-in of GitLab.
- No run of `writ login` has used a real GitLab. GitLab.com offers no device flow, so `writ login` signs you in through a browser, with a redirect to a loopback port that changes on each sign-in. That GitLab accepts it on any port is inferred from the Doorkeeper version it pins, as [Set up writ login](sign-in.md#set-up-writ-login) explains. If it doesn't, sign in to the control UI instead.

### Before you begin

- A Linux host with Postgres, `writ`, and OpenTofu 1.13.9, behind a proxy that ends TLS for `https://writ.example.com`.
- The GitLab project `acme/infra` and its numeric id, here `4242`. A renamed project keeps its id.
- A `writ` image in a registry your runners can pull from, here `registry.example.com/writ:0.1.0`, a copy of [`ghcr.io/mr-karan/writ:0.1.0`](install.md#the-container-image).

### 1. Commit the stack to the default branch

Commit `app` to `main` before you add any CI jobs: its `.tf` files and its `.terraform.lock.hcl`. `writ serve` finds stacks on the default branch, so it serves `app` from its first read of the project. `writ ci gitlab` also refuses a repository whose default branch has no stack it serves.

### 2. Write the config

On the host, write one HCL file. It sets the GitLab issuer of CI tokens, the `people` issuer you sign in with, the Git host and its read-only token, the repository, four teams, a grant for each, one approval rule, the environment's state key, and how to run OpenTofu:

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

- `secrets/gitlab_read` has a GitLab token with `read_api`. [Repositories](repos.md#forges-and-their-tokens) says which token to create.
- `secrets/oidc_client_secret` has the secret of a GitLab application whose id is `client_id`, with the scopes `openid profile email` and two redirect URIs, one per line: `https://writ.example.com/ui/callback` for the control UI, and `http://127.0.0.1/callback` for `writ login`. [Sign in](sign-in.md#set-up-writ-login) explains both flows.
- The team `acme` is every person in the GitLab group `acme`. Its grant to read `app` in `prod` lets them see plans and runs, from a terminal or in the control UI.
- Approvals come from `infra_reviewers`, the project's maintainers and owners, through the pipeline's approve job. To let people approve from a plan's page in the control UI too, add `approve` to the `acme` grant. Both issuers have the identity `gitlab`, so a person and the CI jobs they start count as one person, and the author still can't approve.
- `keys/prod.key` has the 32 random bytes that [seal](glossary.md) the environment's state and saved plans: `head -c 32 /dev/urandom > keys/prod.key`, mode 0600. Back it up offline.
- `listen` is a loopback address, so your proxy ends TLS for `url`.
- A stack on S3 also needs AWS plan and apply roles. See [Roles and credentials](serve.md#roles-and-credentials), and the [server config](config.md) for every other setting.

### 3. Check the config, then start writ serve

Run `writ serve --config serve --check` first. It reads the config, prints what each team can do, and runs `tofu version`. It doesn't contact Postgres, GitLab, or the issuers.

When it can switch users, it also runs `writ` as the first provider uid. That uid tries to read the state keys, the TLS key, the `provider_env` files, and the data directory, and to connect to the EC2 instance metadata addresses, `169.254.169.254` and `fd00:ec2::254`, on port 80. For an address the uid couldn't reach, `writ` then tries the address itself, to tell an address the host blocks from one where no metadata service answers. If the uid can read a secret or reach an address, the check fails. This run was as a user who can't switch users, so the isolation check only warns:

```
$ writ serve --config serve --check
config serve: ok
url: https://writ.example.com
issuers: gitlab (gitlab-ci, https://gitlab.com), sso (people, https://gitlab.com, client 8f1c0b7e4d2a)
control UI: https://writ.example.com/ui/, sign-in redirects to https://writ.example.com/ui/callback, sessions last 12h0m0s
envs: prod, state in postgres://writ@/writ
trusted proxies: 127.0.0.1/32; X-Forwarded-For names the client only when one of them sends it
providers run as: uids 61000-61063, one for each running plan
tofu: tofu, plugin cache /var/lib/writ-tofu/plugins, scratch directories in /var/lib/writ-tofu/runs
approvals: rule 1: app in prod with create update replace delete data-loss needs 1
TEAM             ENV   STACKS  CAN         RISKS
acme             prod  app     read        -
infra_main       prod  app     read apply  create update replace delete
infra_mr         prod  app     read plan   -
infra_reviewers  prod  app     approve     -
tofu: /nix/store/60vqi4v8n11afd5ks89j1kn7pgwfrww5-opentofu-1.13.9/bin/tofu is OpenTofu 1.13.9: ok
warning[usage]
  isolation not checked: run writ as provider uid 61000: fork/exec /var/tmp/writ-landing2/qs/bin/writ: operation not permitted; writ serve checks its capabilities at start
```

Output of a real run, 2026-11-16.

Then start it with `writ serve --config serve`. On NixOS, [the module](run.md#on-nixos) runs it with Postgres, the capabilities that `provider_uids` needs, and `--check` before each start.

### 4. Sign in from your terminal

On your laptop, run `writ login https://writ.example.com`. GitLab.com offers no device flow, so `writ login` prints a GitLab URL, and opens it in your browser when it can. Sign in at GitLab, and GitLab sends your browser back to `writ login`:

```
$ writ login http://127.0.0.1:35309
Signing in to http://127.0.0.1:35309 through issuer sso.
To sign in, open https://127.0.0.1:36093/oauth/authorize?client_id=writ-e2e&code_challenge=QqueYeHu6tMmOgOABEnQeLVDZ4E2tW41WqKkByO5AdM&code_challenge_method=S256&nonce=BV_F4WYQLxLGVMYFi9phIfQaGZKgk6l7-QCRw0TZzEU&redirect_uri=http%3A%2F%2F127.0.0.1%3A36443%2Fcallback&response_type=code&scope=openid+profile+email&state=BGX7AJ5XD2T6KK7AUC6OMKYHR2 in a browser on this computer.
The sign-in expires in 10 minutes.
Signed in to http://127.0.0.1:35309 as bob (gitlab/user/2002).
Teams: acme
Session ends 2026-11-16T20:39:54Z.
$ writ whoami
http://127.0.0.1:35309 (from your session), session of bob (gitlab/user/2002, issuer sso), ends 2026-11-16T20:39:54Z
teams: acme
prod  app  read  -
```

Output of `TestTofuApproveApply`, 2026-11-16, where `writ serve` listens on a loopback address, a test issuer stands in for GitLab, and a script follows the URL as your browser would.

Your browser must run on the computer where you run `writ login`, because GitLab sends it to a loopback address there. If GitLab shows an error about the redirect URI instead of sending you back, check that the application lists `http://127.0.0.1/callback`.

You can also sign in in a browser only. Open `https://writ.example.com/ui/repos/infra` and sign in with GitLab. The page lists `app`, served in `prod`.

### 5. Add the CI jobs in a merge request

Open a merge request that adds these lines to `.gitlab-ci.yml`:

```yaml
stages: [writ-plan, writ-approve, writ-apply]
include:
  - component: $CI_SERVER_FQDN/platform/writ-ci/writ-plan@0.1.0
    inputs:
      server: https://writ.example.com
      env: prod
      stack: app
      image: registry.example.com/writ:0.1.0
  - component: $CI_SERVER_FQDN/platform/writ-ci/writ-approve@0.1.0
    inputs:
      server: https://writ.example.com
      image: registry.example.com/writ:0.1.0
  - component: $CI_SERVER_FQDN/platform/writ-ci/writ-apply@0.1.0
    inputs:
      server: https://writ.example.com
      env: prod
      stack: app
      image: registry.example.com/writ:0.1.0
```

The `include` lines point to the components in [`ci/gitlab`](../ci/gitlab/README.md), as a project `platform/writ-ci` on your GitLab, at version `0.1.0`. Put a copy there, or change the paths. None of the three jobs needs a checkout or holds a cloud credential:

- In each merge request pipeline, `writ-plan-app-prod` plans the commit.
- In each merge request pipeline, `writ-approve` is a manual job that blocks the merge until each plan has its approvals.
- On the default branch, `writ-apply-app-prod` applies the approved plan whose sources equal the merged commit's.

These are the lines `writ ci gitlab --repo infra --image registry.example.com/writ:0.1.0` prints for this config, with the teams, grants, and approval rule you already wrote. [Set up GitLab CI](set-up-gitlab-ci.md#print-the-ci-lines-and-the-config) shows its whole output. It needs a token that can read a stack, so run it in a CI job, with the job's `WRIT_ID_TOKEN`, as `TestTofuApproveApply` does. From a laptop it would need `writ login`.

Protect `main`, and turn on "Pipelines must succeed" in the project's settings.

### 6. Plan, approve, and apply

Nothing has applied `app` yet, so the plan job in alice's merge request creates every resource. It prints the masked plan, which the people of `acme` also see in the control UI:

```
OpenTofu 1.13.9 plan of app@prod

  + random_pet.name (create)
  + terraform_data.note (create)
  + terraform_data.secret (create)
  + module.cert.tls_private_key.key (create)

Plan: 4 to add, 0 to change, 0 to destroy.
Outputs that change: pet.
```

Output of `TestTofuApproveApply`, 2026-11-16.

If alice tries to approve the plan with a token from that pipeline, Writ refuses, because alice created it:

```
$ writ approve --remote 927b69ec7af23641c68c3ce3cce370ec
error: denied: gitlab acme/infra #1849401 (alice) made plan 927b69ec7af23641c68c3ce3cce370ec and cannot approve it: it needs 1 approval from someone else
```

If the merge request merges anyway, the job on `main` applies nothing:

```
$ writ apply --remote --merged --env prod --stack app
error: denied: plan 927b69ec7af23641c68c3ce3cce370ec of app@prod matches the sources at 3008b0c3 and needs 1 approval; approve it with writ approve --remote 927b69ec7af23641c68c3ce3cce370ec
```

bob, a maintainer, runs the approve job of the same pipeline:

```
$ writ approve --remote 927b69ec7af23641c68c3ce3cce370ec
Approved plan 927b69ec7af23641c68c3ce3cce370ec for prod (app): 1 of 1 approvals (gitlab acme/infra #1849401 (bob)), approved.
```

The job on `main` then applies the plan bob approved. It's the saved plan `writ serve` kept, so no file comes from the job:

```
OpenTofu apply of app@prod
  terraform_data.note: create complete
  terraform_data.secret: create complete
  random_pet.name: create complete
  module.cert.tls_private_key.key: create complete
Apply: 4 added, 0 changed, 0 destroyed.
State version 1 of app@prod.
```

Output of `TestTofuApproveApply`, 2026-11-16.

If alice pushes a new commit to the merge request, the pipeline creates a new plan, and the approved one stops applying. [Why an approved plan can still be refused](approvals.md#why-an-approved-plan-can-still-be-refused) shows that refusal.

## Next steps

- [Approvals](approvals.md) and [Approve and apply a plan](apply.md): approval rules, who can approve, stale plans, and overrides.
- [Set up GitLab CI](set-up-gitlab-ci.md): the three jobs, and how each gets its token.
- [Sign in](sign-in.md): `writ login` in both its flows, and the control UI.
- [Policy](policy.md): Rego policy that checks each plan.
- [Writ compared with OpenTofu in plain GitLab CI](compare-gitlab-ci.md): what Writ adds to a pipeline that runs `tofu` itself, and what it costs.
