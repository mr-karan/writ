# What Writ is

Writ is an open-source, self-hosted control plane for [OpenTofu](https://opentofu.org). Point `writ serve` at a Git repository of [stacks](glossary.md). When you or a CI job submit a commit, Writ plans it on your server, enforces Rego policies, waits for the approvals your rules require, and applies that exact [sealed](glossary.md) plan. CI jobs and laptops never hold cloud credentials or state keys.

Writ ships as a single Go binary under Apache 2.0 with no limits on users, runs, or resources. A team server stores records in Postgres; `writ serve --dev` runs on loopback with SQLite and needs no cloud account.

## Get started

| Goal | Page |
| --- | --- |
| Plan and apply a stack locally with `writ serve --dev` | [Quick start](quick-start.md) |
| Install the binary, container image, or Nix package | [Install](install.md) |
| Check supported platforms, tests, and limits | [Supported platforms and limits](status.md) |
| Compare Writ with plain GitLab CI or other control planes | [Writ vs plain GitLab CI](compare-gitlab-ci.md), [Writ vs other control planes](compare.md) |

## Guides

- [Add a repository](add-repository.md): connect a local Git repo or a GitLab project.
- [Get changes in seconds with a webhook](webhooks.md): configure a verified GitLab push webhook.
- [Set up GitLab CI](set-up-gitlab-ci.md): wire plan, blocking approve, and post-merge apply jobs using OIDC.
- [Plan and apply](apply.md): plan commits, approve, apply by ID or after merge, recover lost replies, and override approvals.
- [Configure approvals](approvals.md): set approval rules by stack, environment, and risk class.
- [Get notified](notifications.md): send drift, failure, apply, outside-write, and approval events to Slack or signed webhooks.
- [Write and test policies](write-a-policy.md): write Rego rules, test them with `conftest`, and load them in `writ serve`.
- [Policy cookbook](policy-cookbook.md): 12 Rego rules and tests for tags, S3, encryption, ingress, instance sizes, regions, windows, and IAM approvals.
- [Adopt existing state](adopt-state.md): seal a secret set for state Writ didn't write.
- [Migrate from Terraform](migrate-from-terraform.md): bring Terraform stacks, lock files, and state to Writ and OpenTofu.
- [Handle secrets and masking](secrets.md): how the mask strips secrets and what it can't catch.
- [Enable drift checks](drift-checks.md): schedule drift plans and read results in the UI and API.
- [Read the audit log](audit-log.md): query audit rows in the control UI, CLI, and API.
- [Import existing objects](import-bulk.md): generate `resource` and `import` blocks for existing infrastructure.
- [Use Writ with a coding agent](agents.md): install the `writ` skill and connect `writ mcp`.

## Operate a server

- [Run a server](run.md): start `writ serve --dev` or run a team server with Postgres, containers, or NixOS.
- [Configure sign-in](sign-in.md): set up OIDC for `writ login` and the control UI.
- [AWS roles and credentials](aws-credentials.md): configure `plan_role`, `apply_role`, S3 state permissions, and `provider_env`.
- [Run isolation and egress](run-isolation.md): configure `provider_uids`, namespaces, cgroup limits, seccomp, metadata firewall, and the egress proxy.
- [Back up and upgrade](upgrades.md): back up Postgres and state keys, configure retention, and upgrade single-node servers.

## Reference

- [Commands](commands.md): CLI subcommands, flags, exit codes, and environment variables.
- [Server config](config.md): HCL settings, blocks, defaults, and `--check` validation rules.
- [API](api.md): HTTP routes, token checks, rate and time limits, and audit rules.
- [JSON output](json-output.md): contract for every `--json` document.
- [Errors](errors.md): error classes, exit codes, HTTP statuses, and fixes.
- [Policy reference](policy-reference.md): Rego input schema, allowed built-ins, and execution bounds.
- [Agent MCP reference](agent-reference.md): `writ mcp` tools, output shapes, and refusal recovery.
- [Glossary](glossary.md): Writ terms and their Terraform equivalents.

## Concepts

- [How a plan becomes an apply](plan-to-apply.md): end-to-end lifecycle from commit fetch to state index.
- [Security model](security-model.md): credential boundaries, isolation layers, masking, and limits.
