# Writ documentation

Writ is an open-source control plane for OpenTofu that you host yourself. You point `writ serve` at a Git repository of stacks. It plans the commits you or your CI job send it, waits for the approvals your rules require, and applies the plan it kept. Your CI jobs and laptops never hold a cloud credential or a state key.

Writ is at version 0. [Status and limits](status.md) lists what's built, how it's tested, and what's missing. Read it before you rely on Writ.

## Start here

New to Writ? Follow the [quick start](quick-start.md). You'll plan and apply a stack on your own machine, with no cloud account, then set Writ up for a team.

| If you want to | Read |
| --- | --- |
| Try Writ on your machine | [Quick start](quick-start.md) |
| Get the `writ` binary or the container image | [Install](install.md) |
| Run it for your team | [Run a control plane](run.md), then the [server config](config.md) |
| Plan and apply from CI | [Set up GitLab CI](set-up-gitlab-ci.md) |
| Know what it can't do yet | [Status and limits](status.md) |
| Compare it with what you use today | [Other control planes](compare.md), or [OpenTofu in plain GitLab CI](compare-gitlab-ci.md) |

## Concepts

How Writ works, and why it's built that way.

- [How the control plane works](serve.md): how `writ serve` plans a commit, runs OpenTofu apart from its keys, keeps state, checks drift, and decides who can do what.
- [Repositories](repos.md): how the control plane reads your code by commit.
- [Approvals](approvals.md): approval rules, who can approve, stale plans, and what each run records.
- [Secrets and the mask](secrets.md): how secrets stay out of everything `writ serve` stores or shows, and the cases the mask can't catch.
- [Policy](policy.md): Rego policies that check each plan when it's created and again before its apply.

## Guides

One task each, step by step.

- [Run a control plane](run.md): `writ serve --dev` for you, and a team's `writ serve`.
- [Sign in](sign-in.md): `writ login` and the control UI.
- [Add a repository](add-repository.md): a local repository or a GitLab project.
- [Set up GitLab CI](set-up-gitlab-ci.md): the plan, approve, and apply jobs.
- [Approve and apply a plan](apply.md): by id, after a merge, and with an override.
- [Adopt a state](adopt-state.md): bring in a state Writ didn't write.
- [Migrate from Terraform](migrate-from-terraform.md): plan a repository whose stacks Terraform ran.
- [Write and test a policy](write-a-policy.md): Rego, `conftest`, and the examples.
- [Import many objects](import-bulk.md): write code for objects you already run, in one pass.
- [Use Writ with an agent](agents.md): the skill `writ agent init` installs, and `writ mcp`.

## Reference

Exact values, flags, routes, and messages.

- [Commands](commands.md): every command, flag, and exit code.
- [Server config](config.md): every setting of the `writ serve` config.
- [API](api.md): every route, token checks, and limits.
- [Errors](errors.md): each error class, its exit code and status, and what to do.
- [JSON output](json-output.md): the contract for every `--json` document.
- [Control database](database.md): what `writ serve` keeps.
- [Policy input and built-ins](policy-reference.md): what a policy reads and can call.
- [Reference for agents](agent-reference.md): the tools of `writ mcp`, and how to recover from each refusal.
- [Glossary](glossary.md): the terms these pages use.

## Contribute

- [Development](development.md): how to build and test Writ.
- [Architecture](architecture.md): the package map, and the decisions behind it.
- [Writing style](style.md): the voice and terms these pages follow.
