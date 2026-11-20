# Supported platforms and limits

Writ plans, approves, and applies OpenTofu 1.13.9 stacks from Git repositories, with server-enforced OPA/Rego policy, Linux run isolation, scheduled drift checks, bulk imports, and a single secret-masking boundary across plans, run output, and state index rows.

This page lists what ships today, how it is verified, and current platform and feature limits so you can plan your deployment around them.

## What you can use today

- A control plane, `writ serve`, for OpenTofu 1.13.9 stacks in a Git repository: a plan of each commit you or your CI job sends it, approvals bound to one exact plan, and an apply of the saved plan that was approved. See [How the control plane works](serve.md) and [Approvals](approvals.md).
- State on the `http` backend that `writ serve` provides, sealed and versioned, or on a stack's own `backend "s3"`, where a plan runs with a plan role that can't write the state.
- One mask over everything `writ serve` stores or shows, checked by `just verify-mask`. See [Secrets and the mask](secrets.md).
- A private filesystem view and its own provider uid for each `tofu` process on a team's control plane, and an allow-list of module sources.
- OPA policy, checked at the plan and again before the apply, with the same verdict as `conftest`. See [Policy](policy.md) and the [Policy cookbook](policy-cookbook.md).
- Drift checks on a schedule, the state index of what each stack manages, and `writ state adopt` for a state that `writ serve` didn't write.
- `writ import bulk`, for many existing objects into an OpenTofu stack, with references between them and a `for_each` for objects of one shape, and `writ import accept` of the plan that imports them.
- `writ mcp` and the writ skill, so your agent plans a commit, reads the masked plan and its policy verdict, and imports objects. See [Use Writ with an agent](agents.md).
- `--json` on every command but `serve`, `mcp`, `ci`, and `help`.

For every command, see the [commands page](commands.md).

## How it's tested

Writ's end-to-end suites build `writ` and run it against real OpenTofu 1.13.9 and real provider binaries inside isolated namespaces, offline once the providers are cached:

- `hashicorp/random`, `hashicorp/tls`, and `hashicorp/local`, through `writ serve` on its `http` backend.
- `hashicorp/random` and the built-in `terraform_data`, through `writ serve` on a stack's `backend "s3"`, against an S3 stand-in with per-key policies.
- `hashicorp/nomad`, against a local dev agent, for `writ import bulk`.
- `hashicorp/aws`, offline, against a stand-in and against AWS Emulator, for `writ import bulk` and `writ import accept`.

None of these tests reaches a real cloud host.

## What you'll run into today

- **OpenTofu 1.13.9 on Linux for `writ serve`**: `writ serve` requires OpenTofu `1.13.9` and, for a team's server, Linux user, mount, PID, and IPC namespaces with `provider_uids` and `nftables`. The CLI (`writ plan --remote`, `writ approve --remote`, `writ apply --remote`, `writ login`) runs on Linux and macOS (`amd64` and `arm64`).
- **Single-node server**: `writ serve` runs as a single process against Postgres (or local files under `--dev`). A second `writ serve` on the same database fails the other's in-flight runs.
- **Pending sign-in capacity**: Many client addresses together can fill the 1,000 slots for `writ login` sign-ins waiting on browser completion and block new sign-ins for up to 10 minutes. One client address can hold at most 10 slots. See [Sign-ins that wait](sign-in.md#sign-ins-that-wait).
- **AWS STS credential broker**: Per-environment `plan_role` and `apply_role` assume AWS IAM roles via STS. Providers for other clouds read credentials from `provider_env`.
- **Stand-in test verification**: S3 state, STS role assumption, GitLab CI components, and bulk imports are verified against local stand-ins and emulators in CI rather than live cloud accounts.
- **Focused scope**: Writ does not include chat notifications, a `test` wrapper command, editor plugins, or cloud cost estimation.
- **Bulk import coverage**: `writ import bulk` covers the AWS and Nomad resource types it has been measured against; see [Import many objects](import-bulk.md).

## Not built yet

These features are not implemented today. If your workflow depends on one, plan around it.

### writ serve

- `writ login --print-token` and `writ serve --init`.
- Automatic GitHub Actions OIDC token requests inside the CLI. In GitHub Actions, your workflow fetches the OIDC token and sets `WRIT_ID_TOKEN` itself.
- Built-in STS role brokers for clouds other than AWS. For other providers, pass credentials through `provider_env`.
- Multi-stack plans and `--changed` on the server.
- Under `writ serve --dev`: the issuers, teams, grants, approval rules, and environments of a full config. `--config` takes only `forge`, `repo`, `tofu`, and `policy` blocks and `poll_every`.
- `GET /v1/plans/{id}/events`, `POST /v1/lookups`, drift alerts, and merge request notes.
- Console to code: generating HCL and a merge request for an object created in a cloud console.
- Multi-node active-active `writ serve`. Sign-ins of `writ login` waiting for browser completion live in one process's memory. If two `writ serve` processes share one Postgres database, each fails the plans and runs the other is executing and releases their state locks.

### Approvals and applies

- Streaming run events and run cancellation: `GET /v1/runs/{id}/events`, `writ.event/v0` streams, and `POST /v1/runs/{id}/cancel`. Clients poll `GET /v1/runs/{id}` until completion.
- Withdrawing approvals, starting applies, and cancelling runs from the control UI. The control UI supports reviewing plans and approving them; read the audit log with `GET /v1/audit` or `writ audit --remote`.
- Refusal reasons in `GET /v1/audit`. Because a refusal reason may echo untrusted request input, `GET /v1/audit` withholds refusal reasons (`reason_withheld: true`) and writes the full text to server stderr logs.
- Merge request notes and multi-stack atomic plans.
- `tofu destroy` through the control plane, and a bundled GitHub Actions workflow template.
- A separate hardware second-factor prompt at apply time beyond the caller's session or OIDC token.

### Repositories

- GitHub repository polling via a GitHub App (use `git_token_file` over HTTPS or local repositories).
- Multi-stack change detection across a merge request base branch.
- Merge request notes, commit status publishing, and `write_token_file` usage.
- Webhook hints to trigger an immediate repository poll ahead of `poll_every`.
- Pre-plan stack source digest discovery (plans record their own commit and source blobs).
- Deleting a repository record through the HTTP API (pinned `repo` blocks reconcile automatically when removed from config).
- Garbage collection of unreferenced Git blobs in the database.

### Policy

- Dynamically raising the plan-point `approval` block's `count` from a Rego rule (note: an apply-point rule in Rego *can* inspect `count(input.writ.approvals)` and deny an apply until enough approvals are present; see the [Policy cookbook](policy-cookbook.md)).
- Overriding a mandatory policy `deny` (`--override` bypasses approval rules only).
- Loading Rego policies from an application Git repository (`writ serve` loads policies exclusively from host paths in the server config).
- `writ policy test` and `writ policy eval` subcommands (use `conftest verify` and `conftest test`).
