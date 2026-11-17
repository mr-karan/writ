# Writ

**Plan, approve, and apply OpenTofu on your own server.**

Writ is an open-source control plane for [OpenTofu](https://opentofu.org). You point it at a Git repository of stacks. It plans the commits you or your CI job send it, waits for your team to approve the plan, and applies that exact plan. Your CI jobs and laptops never hold a cloud credential or a state key.

It's one Go binary under the Apache 2.0 license, with no limit on users, runs, or resources. A team's control plane keeps its records in Postgres. On your own machine it uses SQLite.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="website/public/ui/overview-1280-dark.webp">
  <img src="website/public/ui/overview-1280-light.webp" alt="Writ's control UI: plans waiting for approval, applies in progress, failed runs, and drifted stacks, for each environment." width="900">
</picture>

Writ is at version 0. Read [Status](#status) before you put real infrastructure behind it.

## What you get

- **Plans on your server.** You or your CI job ask for a commit's plan. `writ serve` reads that commit from your Git host and runs `tofu plan` on your server. A plan writes nothing to your state.
- **Approvals for one exact plan.** You approve a plan by its id, and the apply runs the plan Writ kept, not a new one. Where an approval rule applies, the author's approval doesn't count. [Approvals](docs/approvals.md)
- **No cloud keys in CI.** A plan runs with the plan role you configure, so make that role read-only. Only an apply gets the apply role: one that its approval rules allowed, or one that someone with `override` overrode with a recorded reason. [Roles and credentials](docs/serve.md#roles-and-credentials)
- **Secrets hidden.** One mask covers every plan, run output, and error that Writ stores or shows. [Secrets and the mask](docs/secrets.md)
- **Policy in Rego.** Writ checks each plan when it's created and again before its apply. A mandatory deny has no override. [Policy](docs/policy.md)
- **Drift checks.** Writ plans each stack on a schedule and shows what changed outside OpenTofu. [Drift checks](docs/serve.md#drift-checks)
- **A control UI, a CLI, and an API.** Review and approve plans in the browser, or script everything with `--json`.
- **Imports and agents.** `writ import bulk` writes `resource` and `import` blocks for objects you already run. `writ mcp` lets a coding agent plan and read a plan, and never approve or apply. [Import many objects](docs/import-bulk.md), [Use Writ with an agent](docs/agents.md)

## Quick start

`writ serve --dev` runs a control plane on your machine, on loopback, with its records in SQLite. It's the same code as a team's `writ serve`, without sign-in or approval rules. You don't need a cloud account.

You need Linux, [`minisign`](https://jedisct1.github.io/minisign/), Git, and [OpenTofu 1.13.9](https://opentofu.org/docs/intro/install/) on `PATH`.

1. Install `writ` v0.1.0. The script checks the release's signature and checksum, then puts `writ` in `~/.local/bin`:

   ```
   curl -fsSL https://github.com/mr-karan/writ/releases/download/v0.1.0/install.sh | sh
   export PATH="$HOME/.local/bin:$PATH"
   ```

2. Start the dev control plane, and leave it running:

   ```
   writ serve --dev
   ```

3. Open a second terminal, where the first line puts `writ` on its `PATH` too. Then create a repository with one stack and add it. A stack is a directory of `.tf` files with a committed `.terraform.lock.hcl`:

   ```
   export PATH="$HOME/.local/bin:$PATH"
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
   EOF
   (cd app && tofu providers lock)
   git add .
   git commit -m "one stack"
   writ repo add .
   ```

4. Plan the commit, then apply the plan by its id:

   ```
   $ cd app
   $ writ plan --remote
   OpenTofu 1.13.9 plan of app@dev

     + random_pet.name (create)

   Plan: 1 to add, 0 to change, 0 to destroy.
   Plan 3f613cf08763b0bf56626574288f85af for dev (app), by alice (dev), expires 2026-11-16T04:40:49Z.
   It needs no approval. Apply it with: writ apply --remote 3f613cf08763b0bf56626574288f85af
   $ writ apply --remote 3f613cf08763b0bf56626574288f85af
   OpenTofu apply of app@dev
     random_pet.name: create complete
   Apply: 1 added, 0 changed, 0 destroyed.
   State version 1 of app@dev.
   ```

   Output of a real run, 2026-11-15, with some lines cut.

The banner that `writ serve --dev` prints has a link to the control UI, where you can read the plan and the run.

Next, [set up Writ for a team](docs/quick-start.md#set-up-writ-for-a-team): Postgres, sign-in with OIDC, approval rules, and a second person's approval.

## Install

- **The install script**, as in the quick start. It checks the release's minisign signature.
- **From source**, with Go 1.27.3 or later and `just`: `just build` puts `writ` in `bin/`.
- **Nix.** `nix run github:mr-karan/writ -- version`. The flake also has a NixOS module that runs a team's `writ serve` with Postgres.
- **Release binaries and the container image.** v0.1.0 has static binaries for Linux and macOS on amd64 and arm64, and `ghcr.io/mr-karan/writ:0.1.0`, a `writ serve` image with OpenTofu 1.13.9. The macOS binaries haven't run on a Mac yet.

[Install](docs/install.md) has the commands for each.

## Status

Writ is new. Here's what you should know before you rely on it:

- **Version 0.** Until 1.0, a minor version can change behavior. Don't move production infrastructure to it yet.
- **OpenTofu only.** Writ runs OpenTofu 1.13.9 and no other version, and it doesn't run Terraform. Each stack has one state, with no `tofu workspace`.
- **GitLab only.** GitLab is the only Git host Writ reads, and its CI jobs are for GitLab CI. Nobody has run those jobs on a real GitLab instance yet, and there's no GitHub flow.
- **AWS roles only.** The plan and apply roles are AWS roles. For another cloud, your provider gets its credentials from the server's config.
- **Not proven in a real cloud account.** The tests run real OpenTofu and real providers against local stand-ins. None reaches a real cloud.
- **One node, on Linux.** A second `writ serve` on the same database fails the other's work.
- **No notifications.** You see plans, drift, and runs in the control UI and the API. Nothing sends you a message about them.

[Status and limits](docs/status.md) has the full list, and how each part is tested.

## Documentation

The docs are at [writ.mrkaran.dev/docs](https://writ.mrkaran.dev/docs), and in [`docs/`](docs/README.md).

- [Quick start](docs/quick-start.md): on your own machine, then for a team.
- [How the control plane works](docs/serve.md): plans, runs, state, drift, and who can do what.
- [Run a control plane](docs/run.md) and the [server config](docs/config.md).
- [Set up GitLab CI](docs/set-up-gitlab-ci.md): the plan, approve, and apply jobs.
- [Commands](docs/commands.md), [API](docs/api.md), and [Errors](docs/errors.md).
- [Writ compared with other control planes](docs/compare.md) and [with plain GitLab CI](docs/compare-gitlab-ci.md).

## Contributing

Bug reports, design questions, and pull requests are welcome. [CONTRIBUTING.md](CONTRIBUTING.md) says how to build Writ, run its tests, and send a change. Report a security problem privately, as [SECURITY.md](SECURITY.md) says.

## License

[Apache 2.0](LICENSE). The files `proto/tfplugin5.proto` and `proto/tfplugin6.proto` are copied from OpenTofu under MPL 2.0, and [NOTICE](NOTICE) has the notices for the code Writ includes.
