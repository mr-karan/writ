# Supported platforms and limits

Writ plans, approves, and applies OpenTofu 1.13.9 stacks on your own Linux server, with Rego policy enforcement, Linux run isolation, scheduled drift checks, Slack and webhook notifications, a synchronous audit log, bulk imports, and an MCP server for coding agents.

## What ships today

- **Control plane (`writ serve`)**: Plans OpenTofu 1.13.9 stacks by Git commit, binds approvals to one SHA-256 plan ID, excludes plan authors from approving, and applies the exact sealed saved plan. [Control plane internals](serve.md), [Approvals](approvals.md)
- **State backends**: Built-in sealed and versioned `http` backend in Postgres (or SQLite under `--dev`), or existing `backend "s3"` buckets with read-only plan roles and `writ state adopt` for out-of-band state. [AWS roles and credentials](aws-credentials.md), [Adopt existing state](adopt-state.md)
- **Secret masking**: `internal/mask` strips secrets from plans, run logs, diagnostics, egress records, and the state index, verified with `just verify-mask`. [Secrets and the mask](secrets.md)
- **Run isolation and egress**: Dedicated `provider_uids`, Linux user/mount/PID/IPC namespaces, cgroup v2 resource limits, a seccomp filter, `nftables` EC2 metadata blocking, module-source allow-listing, and an opt-in per-run egress proxy. [Run isolation and egress](run-isolation.md)
- **OPA Rego policy**: Evaluated at plan and apply time with `conftest` parity and no override for mandatory denials. [Policy](policy.md)
- **Drift checks and notifications**: Scheduled drift checks (`drift` block) and asynchronous Slack or HMAC-signed JSON webhook notifications (`notify` block) for drift, failures, applies, outside writes, and pending approvals. [Drift checks](drift-checks.md), [Notifications](notifications.md)
- **GitLab webhooks**: Verified push webhooks (`POST /v1/hooks/{forge}/{repo}`) to trigger immediate default-branch reads with periodic polling as a backstop. [Webhooks](webhooks.md)
- **Control UI and audit log**: Built-in web UI at `/ui/` (overview, plan review, approvals, stack resources and drift, runs, repos, access, settings, and `/ui/audit`) backed by a synchronous audit log. [Configure sign-in](sign-in.md), [Read the audit log](audit-log.md)
- **Bulk import and coding agents**: `writ import bulk` and `writ import accept` for existing objects, plus `writ mcp` and `writ agent init`. [Import existing objects](import-bulk.md), [Use Writ with a coding agent](agents.md)
- **Structured output**: `--json` on every command except `serve`, `mcp`, `ci`, and `help`. [Commands](commands.md)

## How it's tested

End-to-end suites build `writ` and run real OpenTofu 1.13.9 and real providers inside isolated Linux namespaces, offline once providers are cached:

- `hashicorp/random`, `hashicorp/tls`, and `hashicorp/local` through `writ serve` on its `http` backend (`just e2e`, `just verify-mask`, `just policy-parity`).
- `hashicorp/random` and `terraform_data` through `writ serve` on `backend "s3"` against local `versitygw` and an STS stand-in (`just e2e-tofu-s3`).
- `hashicorp/nomad` against a local dev agent and `hashicorp/aws` against local stand-ins and AWS Emulator for `writ import bulk` and `writ import accept` (`just e2e-import`, `just e2e-import-hcl`, `just e2e-aws-emulator`).

No test contacts a live cloud account or real GitLab instance.

## Platform requirements and limits

- **OpenTofu 1.13.9 only**: Writ runs OpenTofu 1.13.9 and doesn't run Terraform. Each stack has one state (no `tofu workspace`).
- **Linux required for `writ serve`**: Team servers require Linux namespaces, `provider_uids`, `nftables` (on hosts with metadata services), and cgroup v2 on Linux 5.14+ for per-step memory/PID/CPU limits. The CLI builds for Linux and macOS (`amd64`, `arm64`), though macOS binaries are cross-built on Linux and haven't been tested on macOS.
- **Single-node server**: Run one `writ serve` process per Postgres database. A second process marks the first process's in-flight runs as `failed` and releases their locks.
- **GitLab only**: GitLab is the only remote Git host Writ polls or accepts webhooks from, and bundled CI components target GitLab CI (`writ serve --dev` also reads local Git repos).
- **AWS STS roles only**: Built-in `plan_role` and `apply_role` assumption supports AWS IAM roles. Other cloud providers read credentials from `provider_env` secret files.
- **Pending login slots**: At most 1,000 device logins and 1,000 browser logins can wait in memory (10 per client IP, 10-minute expiry). [Sign-ins that wait](sign-in.md#sign-ins-that-wait)
- **Stand-in verification**: S3 state, STS assumption, GitLab CI components, GitLab OIDC sign-in, webhooks, and bulk imports are verified against local stand-ins rather than live cloud or GitLab accounts.
- **Bulk import scope**: `writ import bulk` covers the AWS and Nomad patterns it has been tested against; see [Import existing objects](import-bulk.md).

## Not built yet

### Server and CI

- Multi-node active-active `writ serve`.
- GitHub repository polling (GitHub App) and GitHub webhooks (`X-Hub-Signature-256`).
- Automatic GitHub Actions OIDC token fetching in the CLI (workflows must fetch the token and set `WRIT_ID_TOKEN`) and a bundled GitHub Actions workflow template.
- Built-in STS role brokers for clouds other than AWS.
- Merge request plan comments and commit status publishing (`write_token_file` warns and has no effect).
- Multi-stack atomic plans and `--changed` stack detection on the server.
- `writ login --print-token` and `writ serve --init`.
- Full server config blocks under `writ serve --dev` (`--dev --config` accepts only `forge`, `repo`, `tofu`, `policy`, `poll_every`, and `poll_every_with_hook`).
- Console-to-code automatic MR generation, cloud cost estimation, editor plugins, or a `test` wrapper command.

### Approvals, applies, and UI

- Streaming plan or run events (`GET /v1/plans/{id}/events`, `GET /v1/runs/{id}/events`) and run cancellation (`POST /v1/runs/{id}/cancel`). Clients poll `GET /v1/runs/{id}` and the UI reloads every 5 seconds while applying.
- Starting applies, withdrawing approvals, or cancelling runs from the control UI (the UI reviews and approves plans; apply via CLI or CI).
- `tofu destroy` through the control plane.
- Refusal reasons in `GET /v1/audit` or `/ui/audit` (withheld by design to prevent echoing untrusted request input; stored in Postgres and stderr logs).
- Step-up hardware second-factor prompts at apply time beyond the caller's session or OIDC token.

### Repositories and policy

- Deleting a repository via the HTTP API (pinned `repo` blocks reconcile automatically on config reload).
- Garbage collection of unreferenced Git blobs in the `blobs` table.
- Dynamically raising `approval.need` from Rego at plan time (an apply-point Rego rule can inspect `count(input.writ.approvals)` and deny until enough approvals exist).
- Overriding a mandatory policy `deny`, loading Rego policies from application Git repos, or `writ policy test`/`eval` subcommands (use `conftest verify` and `conftest test`).
