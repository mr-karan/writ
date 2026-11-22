# What Writ is

Writ is an open-source, self-hosted control plane for [OpenTofu](https://opentofu.org). You point `writ serve` at a Git repository of [stacks](glossary.md) and submit a commit. Writ plans it on your server, enforces Rego policies, waits for the approvals your rules require, and applies that exact [sealed](glossary.md) plan. CI jobs and laptops never hold cloud credentials or state keys.

Writ is one Go binary under Apache 2.0, with no limits on users, runs, or resources. A team server keeps its records in Postgres. `writ serve --dev` runs on loopback with SQLite and needs no cloud account.

## Start here

| I want to | Read |
| --- | --- |
| Try Writ locally in four steps | [Quick start](quick-start.md) |
| Install the binary, container image, or Nix package | [Install](install.md) |
| Check platforms, tests, and limits | [Supported platforms and limits](status.md) |
| Decide if Writ fits | [Writ vs plain GitLab CI](compare-gitlab-ci.md), [Writ vs other control planes](compare.md) |

## Do a task

| Task | Guide |
| --- | --- |
| Connect a local repository or GitLab project | [Add a repository](add-repository.md) |
| Get pushes in seconds | [Webhooks](webhooks.md) |
| Plan, approve, and apply from GitLab CI | [Set up GitLab CI](set-up-gitlab-ci.md) |
| Plan, approve, apply, recover a lost reply, or override | [Plan and apply](apply.md) |
| Require approvals by stack, env, and risk | [Configure approvals](approvals.md) |
| Send events to Slack or a signed webhook | [Get notified](notifications.md) |
| Write, test, and load Rego policies | [Write and test policies](write-a-policy.md), [Policy cookbook](policy-cookbook.md) |
| Bring in state Writ did not write | [Adopt existing state](adopt-state.md) |
| Move from Terraform | [Migrate from Terraform](migrate-from-terraform.md) |
| Keep secrets out of plans and output | [Handle secrets and masking](secrets.md) |
| Find drift | [Enable drift checks](drift-checks.md) |
| Query who did what | [Read the audit log](audit-log.md) |
| Import existing infrastructure | [Import existing objects](import-bulk.md) |
| Use Writ from a coding agent | [Use Writ with a coding agent](agents.md) |

## Operate a server

| Task | Guide |
| --- | --- |
| Run `writ serve` with Postgres, a container, or NixOS | [Run a server](run.md) |
| Set up OIDC for `writ login` and the control UI | [Configure sign-in](sign-in.md) |
| Configure `plan_role`, `apply_role`, S3 state, and `provider_env` | [AWS roles and credentials](aws-credentials.md) |
| Sandbox runs and limit egress | [Run isolation and egress](run-isolation.md) |
| Back up, prune, and upgrade | [Back up and upgrade](upgrades.md) |

## Reference

[Commands](commands.md) · [Server config](config.md) · [API](api.md) · [JSON output](json-output.md) · [Errors](errors.md) · [Policy reference](policy-reference.md) · [Agent MCP reference](agent-reference.md) · [Glossary](glossary.md)

## Concepts

- [How a plan becomes an apply](plan-to-apply.md): each step from commit to state index.
- [Security model](security-model.md): credential boundaries, isolation, masking, and limits.
- [Plan lifecycle reference](plan-lifecycle.md): statuses, staleness checks, apply steps, and run records.
- How it works: [run isolation](how-run-isolation-works.md), [sign-in](how-sign-in-works.md), [import bulk](how-import-bulk-works.md), [writ mcp](how-writ-mcp-works.md), [the mask](how-the-mask-works.md), [AWS credentials](how-aws-credentials-work.md).
