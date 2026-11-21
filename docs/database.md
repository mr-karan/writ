# Control database

The control database stores all control plane state: configs, repositories, plans, approvals, runs, sessions, audit logs, notification status, and HTTP-backend OpenTofu state. One migration set (`internal/control/migrations`) targets both Postgres (schema `writ_control`) and SQLite (`control.db` under `writ serve --dev`). S3-backend stacks keep their state in S3. Timestamps are Unix milliseconds from `writ`'s clock; structured fields are JSON text.

## What the control database keeps

| Table | Contents |
| --- | --- |
| `configs` | Every server config started (keyed by SHA-256), or the dev server record under `--dev` |
| `teams`, `grants`, `grant_teams` | Teams and grants per config |
| `repos` | Registered repositories (name, forge, project, numeric project ID, path, prefix, default branch, last poll/webhook times, poll error) |
| `repo_commits` | Cached commits read per repository with discovered stacks and source digests |
| `blobs` | Cached Git blob contents keyed by blob SHA |
| `plan_seqs` | Per-`stack@env` sequence counters ordering concurrent plans |
| `plans` | Plans: status, author, matched claims, origin, masked plan document, sealed saved plan, required approvals, and deduplication `superseded_by` links |
| `plan_events` | Append-only plan status transition history with actor, run, and reason |
| `token_uses` | Single-use CI token `jti`s kept until token expiry (dev token records none) |
| `audit`, `audit_stacks` | Synchronous audit rows for requests and server events, plus referenced stacks ([Every answer waits for its audit row](api.md#every-answer-waits-for-its-audit-row)) |
| `approvals` | Approvals and withdrawals per plan with person ID, timestamp, and auth carrier |
| `runs` | Apply runs: plan ID, actor, counting approvals, override reason, apply mode, server process ID, and masked `writ.tofu-apply/v0` or `writ.apply/v0` document. Unique indexes enforce one run per plan and one `applying` run per `stack@env` |
| `stack_turns` | Per-`stack@env` serialization row locking concurrent applies and [state adoptions](adopt-state.md) |
| `apply_requests` | Apply idempotency keys (`started` with run/principal/request digest, or `none`) |
| `apply_key_floor` | Monotonic highest timestamp of pruned `none` keys so clock rollback can't reopen closed keys |
| `role_sessions` | Assumed AWS STS role sessions linked to plans and runs |
| `tfstate_tokens` | Active HTTP-backend run tokens (SHA-256 only, run ID, `stack@env`, `plan`/`apply`, expiry); deleted on run end |
| `tfstate_versions` | Versioned HTTP-backend states: serial, lineage, SHA-256, run ID, timestamp, sealed state bytes, and sealed secret set |
| `tfstate_locks` | Active HTTP-backend locks per `stack@env` (lock ID, run ID, OpenTofu lock payload) |
| `state_index`, `state_index_stacks` | [State index](serve.md#the-state-index) resources (allow-listed attributes only) and per-`stack@env` backend, indexed version/serial, lineage, outside-write serial, and last adoption time |
| `state_s3_stacks` | Per-`stack@env` S3 state tracking metadata |
| `state_serials` | Observed S3 state serials per `stack@env` with lineage, writing run (or none), read serial, and sealed secret set |
| `drift_checks` | Newest 20 [drift checks](drift-checks.md) per `stack@env` (addresses, actions, changed top-level attribute names, never values or saved plans) |
| `sessions` | `cli` and `browser` sessions (public ID, SHA-256 token hash only, person, teams, expiry, last use, revocation) |
| `notification_status` | Per-notifier delivery/failure counts, last delivery timestamp, and last error |

## Audit rows

Each audit row records timestamp, request ID, route, HTTP status, principal and kind, `via`, teams, operation, environment, stacks, plan, run, addresses, risk classes, decision (`allowed`, `denied`, `auth-failed`, `refused`, `error`, or `none`), failed token check, reason, matching grants, and config ID. Indexes cover time, person, environment, stack, plan, run, and refusals.

HTTP state backend requests record `state-read`, `state-lock`, `state-unlock`, or `state-write` (the last includes version, serial, lineage, and SHA-256; never raw state). OpenTofu 1.13.9 issues roughly 2 state requests per plan and 7 per apply.

Server lifecycle events (startup, plan completion, apply completion) write rows without an HTTP route. Plan creation commits its audit row in the same transaction as the plan and token use.

## Migrations

Schema versions are tracked in `schema_migrations`. Migrations that differ between Postgres and SQLite ship as paired files (`NNNN_name.postgres.sql` and `NNNN_name.sqlite.sql`) recorded under `NNNN_name.sql`. `writ serve` applies pending migrations in a transaction at startup and refuses to start against a database migrated by a newer version of Writ. See [Back up and upgrade](upgrades.md#version-specific-upgrade-notes) for upgrade notes.

## Connection timeouts and backups

- **Postgres:** 5s connect timeout, 10s statement timeout, 5s lock timeout, 15s idle-in-transaction timeout.
- **SQLite (`--dev`):** 5s busy timeout.

Never embed Postgres passwords in `database` URLs in `/etc/writ/serve`; use Unix-socket peer authentication ([On NixOS](run.md#on-nixos)), `?passfile=/path/to/pgpass`, or `PGPASSFILE`.

## Next steps

- [Back up and upgrade](upgrades.md): `pg_dump`, offline key backups, retention, and upgrades.
- [Read the audit log](audit-log.md): query audit rows via CLI, UI, and API.
- [Run a control plane](run.md): connect `writ serve` to Postgres.
