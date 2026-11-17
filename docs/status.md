# Status and limits

Writ is at version 0. You can plan, approve, and apply OpenTofu stacks from a Git repository with it. It has policy, drift checks, imports, and one mask over the plans, run output, and state index rows it shows.

Here's what's built, how it's tested, and what's missing, so you can judge what to trust it with. Our advice today: don't move production infrastructure to Writ.

## What you can use today

- A control plane, `writ serve`, for OpenTofu 1.13.9 stacks in a Git repository: a plan of each commit you or your CI job sends it, approvals bound to one exact plan, and an apply of the saved plan that was approved. See [How the control plane works](serve.md) and [Approvals](approvals.md).
- State on the `http` backend that `writ serve` provides, sealed and versioned, or on a stack's own `backend "s3"`, where a plan runs with a plan role that can't write the state.
- One mask over everything `writ serve` stores or shows, checked by `just verify-mask`. See [Secrets and the mask](secrets.md).
- A private filesystem view and its own provider uid for each `tofu` process on a team's control plane, and an allow-list of module sources.
- OPA policy, checked at the plan and again before the apply, with the same verdict as `conftest`. See [Policy](policy.md).
- Drift checks on a schedule, the state index of what each stack manages, and `writ state adopt` for a state that `writ serve` didn't write.
- `writ import bulk`, for many existing objects into an OpenTofu stack, with references between them and a `for_each` for objects of one shape, and `writ import accept` of the plan that imports them.
- `writ mcp` and the writ skill, so your agent plans a commit, reads the masked plan and its policy verdict, and imports objects. See [Use Writ with an agent](agents.md).
- `--json` on every command but `serve`, `mcp`, `ci`, and `help`.

For every command, see the [commands page](commands.md).

## How it's tested

You don't have to take the list above on faith. Writ's end-to-end tests build `writ` and run it against real OpenTofu 1.13.9 and real provider binaries, offline once the providers are cached:

- `hashicorp/random`, `hashicorp/tls`, and `hashicorp/local`, through `writ serve` on its `http` backend.
- `hashicorp/random` and the built-in `terraform_data`, through `writ serve` on a stack's `backend "s3"`, against an S3 stand-in with per-key policies.
- `hashicorp/nomad`, against a local dev agent, for `writ import bulk`.
- `hashicorp/aws`, offline, against a stand-in and against AWS Emulator, for `writ import bulk` and `writ import accept`.

None of these tests reaches a real cloud host.

## What you'll run into today

- There's no proof in a real cloud account yet. S3 state, the credential broker's roles, and imports run against stand-ins only.
- You get one node. A second `writ serve` on the same database fails the other's runs.
- Many client addresses together can fill the 1,000 places of `writ login` sign-ins that wait, and lock everyone out of signing in for up to 10 minutes. One address can hold only 10. See [Sign-ins that wait](sign-in.md#sign-ins-that-wait).
- Nobody has run the GitLab CI component on a real GitLab yet, and there's no GitHub flow.
- You won't find notifications, a test command, editor support, or cost estimates.
- `writ import bulk` covers the types it was measured on; see [Import many objects](import-bulk.md).

## Not built yet

These are missing today. If you need one of them, plan around its absence.

### writ serve

- `writ login --print-token` and `writ serve --init`. A sign-in through a real GitLab, in either flow. That GitLab takes `writ login`'s loopback redirect on any port is inferred from the Doorkeeper version it pins, as [Set up writ login](sign-in.md#set-up-writ-login) explains.
- The GitHub Actions token request in the CLI. In GitHub Actions, your job sets `WRIT_ID_TOKEN` itself.
- Roles for clouds other than AWS. For another cloud, your provider gets its credentials from `provider_env`.
- Plans of several stacks and `--changed` on the server.
- Under `writ serve --dev`: the issuers, teams, grants, approval rules, and environments of a full config. `--config` takes only `forge`, `repo`, `tofu`, and `policy` blocks and `poll_every`.
- `GET /v1/plans/{id}/events`, `POST /v1/lookups`, alerts, among them alerts of drift, and merge request notes.
- Console to code: code and a merge request for an object created in the console.
- A second node. Postgres makes one possible later. Sign-ins of `writ login` that wait for their person live in one process's memory. If you run two `writ serve` processes on one database, they fail each other's work. At its start, each one fails the plans and runs the other is making, then every minute after it fails the runs the other applies, and releases their state locks.

### Approvals and applies

- Run events and cancel: `GET /v1/runs/{id}/events`, run events streamed as `writ.event/v0`, and `POST /v1/runs/{id}/cancel`. Your client polls the run.
- Withdrawing, applying, and cancelling from the control UI. You can only approve there. The control UI's audit page waits for `GET /v1/audit`.
- Merge request notes, and plans of several stacks as one plan.
- Destroy through the control plane, and the GitHub Actions jobs.
- A second factor before an apply. Writ would check it in the transaction that moves a plan to `applying`.

### Repositories

- GitHub, with a GitHub App.
- Plans of several stacks, `base`, and the stacks a merge request changed.
- Merge request notes, commit statuses, and the write token.
- A webhook hint that makes a poll happen sooner.
- Discovery of a stack's source digest. Plans record their own.
- Removing a repository through the API. If you use a pinned list, it removes rows when your config drops them.
- Garbage collection of blobs that no commit refers to.

### Policy

- A policy that requires more approvals.
- An override of a deny.
- Policy read from a repository that an admin activates. Today policy comes only from the server's config.
- `writ policy test` and `writ policy eval`. For now, use `conftest verify` and `conftest test`.
