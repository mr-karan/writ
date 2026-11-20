# What Writ is

Writ is an open-source, self-hosted control plane for [OpenTofu](https://opentofu.org). You point `writ serve` at a Git repository of [stacks](glossary.md). It plans the commits you or your CI job send it, waits for the approvals your rules require, and applies the exact [sealed](glossary.md) plan your team approved. Your CI jobs and laptops never hold a cloud credential or a state key.

Writ is a single Go binary under the Apache 2.0 license, with no limit on users, runs, or resources. A team's server stores its records in Postgres; on your own machine, `writ serve --dev` runs on loopback with SQLite and needs no cloud account.

## Get started

New to Writ? Run it locally in five minutes with no cloud account, then see how it fits your workflow.

| If you want to | Read |
| --- | --- |
| Plan and apply a stack locally with `writ serve --dev` | [Quick start](quick-start.md) |
| Install the `writ` binary, container image, or Nix package | [Install](install.md) |
| Check supported platforms, test coverage, and current limits | [Supported platforms and limits](status.md) |
| Compare Writ with plain GitLab CI or paid control planes | [Writ vs plain GitLab CI](compare-gitlab-ci.md) or [Other control planes](compare.md) |

## Guides

Task-focused guides with prerequisites, numbered steps, expected output, and troubleshooting.

- [Add a repository](add-repository.md): connect a local Git repository or a GitLab project.
- [Set up GitLab CI](set-up-gitlab-ci.md): run plan, blocking approve, and post-merge apply jobs with OIDC and no cloud keys.
- [Plan and apply](apply.md): plan a commit, approve it, apply by ID or after merge, recover from lost answers, and use overrides.
- [Configure approvals](approvals.md): write approval rules by stack, environment, and risk class, and see who can approve.
- [Write and test policies](write-a-policy.md): write Rego rules, test them locally with `conftest`, and enforce them in `writ serve`.
- [Policy cookbook](policy-cookbook.md): 12 copy-pasteable Rego rules and unit tests for tags, S3, encryption, security groups, instance sizes, regions, change windows, and approvals.
- [Adopt existing state](adopt-state.md): seal a secret set for a state Writ didn't write so its plans can be approved again.
- [Migrate from Terraform](migrate-from-terraform.md): bring a Terraform stack, lock file, and state to Writ and OpenTofu.
- [Handle secrets and masking](secrets.md): keep secrets out of plans, logs, and the state index, and know what the mask can't catch.
- [Enable drift checks](drift-checks.md): schedule periodic drift plans and read results in the UI and API.
- [Read the audit log](audit-log.md): query who planned, approved, and applied changes with `writ audit --remote` and `GET /v1/audit`.
- [Import existing objects](import-bulk.md): generate `resource` and `import` blocks for existing infrastructure in one run.
- [Use Writ with a coding agent](agents.md): install the `writ` skill and connect `writ mcp` so agents plan safely without cloud keys.

## Operate a server

Guides for platform engineers running a team's `writ serve` in production or staging.

- [Run a server](run.md): start `writ serve --dev` locally or run a team's `writ serve` with Postgres, containers, or NixOS.
- [Configure sign-in](sign-in.md): set up OIDC for `writ login` (device or browser flow) and the built-in control UI.
- [Operate the database](database.md): understand the Postgres and SQLite schema, tables, audit rows, and migrations.
- [AWS roles and credentials](aws-credentials.md): configure separate `plan_role` and `apply_role` IAM roles, S3 state permissions, and `provider_env`.
- [Run isolation and egress](run-isolation.md): isolate runs with `provider_uids`, Linux namespaces, and the `nftables` metadata firewall rule, and limit the hosts they reach with the opt-in egress proxy.
- [Back up and upgrade](upgrades.md): back up Postgres and offline state keys, set retention, and upgrade single-node servers safely.

## Reference

Complete, terse specifications of every CLI flag, config key, HTTP route, JSON document, and error class.

- [Commands](commands.md): every `writ` subcommand, flag, exit code, and environment variable.
- [Server config](config.md): every setting and block in the `writ serve` HCL config, plus a complete sample.
- [API](api.md): every HTTP route, token verification step, rate limit, time limit, and security refusal.
- [JSON output](json-output.md): the contract for every `--json` document.
- [Errors](errors.md): every error class, exit code, HTTP status, common message, and fix.
- [Policy reference](policy-reference.md): the Rego input schema, allowed built-ins, and execution bounds.
- [Agent MCP reference](agent-reference.md): every `writ mcp` tool, response shape, and recovery rule.
- [Glossary](glossary.md): exact definitions of Writ terms and their Terraform equivalents.

## Concepts

How Writ works under the hood and why it is designed this way.

- [Architecture](architecture.md): package map, data flow, enforced boundaries, and dependencies.
- [How a plan becomes an apply](plan-to-apply.md): end-to-end lifecycle from commit fetch to sealed plan, approval, apply, and crash recovery.
- [Security model](security-model.md): credential boundaries, OIDC checks, run sandboxing, masking, and trust assumptions.
- [Control plane internals](serve.md): deep dive into OpenTofu stacks, S3 state tracking, drift mechanics, and teams and grants.
- [How repositories work](repos.md): GitLab API fetching, blob caching, stack discovery, and polling.
- [How policy works](policy.md): plan-point and apply-point evaluation, mandatory vs advisory sets, and secret handling.
- [Contributing and testing](development.md): build commands, local test gates, hosted CI, and the real-GitLab test plan.
