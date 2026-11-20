# Control database

The control database has everything `writ serve` keeps: plans, approvals, runs, sessions, the audit log, and the state of every stack on the http backend. One schema, the migrations in `internal/control/migrations`, runs on both engines. On Postgres its tables live in the schema `writ_control`. On SQLite, under `writ serve --dev`, the tables live in `control.db`. A stack on S3 keeps its state in its own bucket. Times are Unix milliseconds that `writ` sets from its own clock, and lists and documents are JSON in text columns.

## What the control database keeps

| Table | Contents |
| --- | --- |
| `configs` | Each config the server started with, by its SHA-256. Under `--dev`, a one-line record of the URL and the data directory. |
| `teams`, `grants`, `grant_teams` | The teams and grants of each config, and the teams each grant lists |
| `plans` | Each plan: status, author, the claims its teams matched on, origin, the plan document, the saved plan, sealed, and the approvals its rules required. If a request's plan is byte for byte the same as an earlier request's, it has no id of its own: it's `superseded`, and `superseded_by` is that plan's id. |
| `plan_events` | Each change to a plan's status, with who or what changed it, the run, and why |
| `token_uses` | Token ids that wrote something, until each token expires. The dev token has no id and records none. |
| `audit`, `audit_stacks` | A row for every request but the ones [Every answer waits for its audit row](api.md#every-answer-waits-for-its-audit-row) lists, a row for every decision the server makes on its own, and the stacks each row refers to |
| `approvals` | Each approval of a plan and its withdrawal, with the person, the time, and how they proved it |
| `runs` | Each apply: its plan, actor, the approvals that counted, the override's reason, how the request referred to the plan, the id of the `writ serve` process that began it, and its document when it ends: `writ.tofu-apply/v0`, or `writ.apply/v0` for an apply that couldn't start or that `writ serve` lost. One run for each plan, and one applying run for each `stack@env`. |
| `stack_turns` | One row for each `stack@env` that an apply began or a person [adopted](adopt-state.md): both lock it, so an adoption never commits while an apply of its `stack@env` runs |
| `apply_requests` | The key of each apply request with an outcome: `started` with the run, the principal, and the SHA-256 of the request's fields, or `none`, which the outcome route wrote. One row for each key. |
| `apply_key_floor` | One row: the greatest time of a key with a `none` row that the prune deleted. It never goes down. A key at or below it starts no run. |
| `tfstate_tokens` | The token of each run on the http state backend: only its SHA-256, the run, the `stack@env`, `plan` or `apply`, and its end. A token is gone when its run ends. |
| `tfstate_versions` | Each OpenTofu state a run wrote: its serial, lineage, SHA-256, run, and time, the state sealed under the environment's key, and its secret set, sealed under the same key, from the apply that wrote it or an [adoption](adopt-state.md). Only the newest versions of each `stack@env` are kept. |
| `tfstate_locks` | The lock of each `stack@env` on the http state backend: its id, the run that holds it, and the lock as OpenTofu sent it |
| `state_index`, `state_index_stacks` | The [state index](serve.md#the-state-index): each resource of each OpenTofu `stack@env`'s state, with its address, type, provider, and the attributes in the allow-list, and for each `stack@env` its backend, the state version or S3 serial its rows came from, the S3 state's lineage, and when, and for a stack on S3 the serial last read that no apply of `writ serve` wrote, and when, and when a person last [adopted](adopt-state.md) its state |
| `state_serials` | Each serial of a [stack on S3](serve.md#stacks-on-s3) that a run read: its lineage, the apply that wrote it or none, the serial that apply's plan read, when, and the secrets of that apply's mask or of an [adoption](adopt-state.md), sealed under the environment's key |
| `drift_checks` | The newest 20 [drift checks](serve.md#drift-checks) of each OpenTofu `stack@env`: when each started and ended, its status, the id of its plan, the commit it planned, its counts of changes outside OpenTofu and in the code, each changed address with its kind, action, and changed attribute names, why it failed, and when the stack began to drift. Never a value of the plan, and never a saved plan. |
| `sessions` | One table for `writ login` sessions and the control UI's browser sessions, with a public id, a carrier (`cli` or `browser`), the person, the teams, the end, the last use to the minute, who ended it, and only the SHA-256 of each token |

## Audit rows

Each audit row has the time, the request id, the route, the status, the principal and its kind, how it proved itself, the teams, the operation, the environment and stacks, the plan, the run, the addresses and risk classes it concerns, the decision (`allowed`, `denied`, `auth-failed`, `refused`, `error`, or `none`), the token check that failed, the reason, the grants behind an allowed call, and the config's id. Indexes serve the questions by time, by person, by environment, by stack, by plan, by run, and for refusals.

A row of the state backend has the run, its `stack@env`, and the operation: `state-read`, `state-lock`, `state-unlock`, or `state-write`. A `state-write` row is of kind `state-write`, and has the version, serial, lineage, and SHA-256 it stored or repeated. No row contains the state. In `TestTofuApproveApply`, OpenTofu 1.13.9 sent 2 requests to plan a stack and 7 to apply it: a lock, 4 reads, a write, and an unlock. So each plan and apply of a stack on the http backend adds that many rows.

A row with no route is the server's start or the end of a plan. Under `writ serve --dev`, each command asks `GET /v1/server` first, to find the dev control plane. That route needs no token and writes no row, like `GET /healthz`. Every other request gets its answer only once its row commits, as [Every answer waits for its audit row](api.md#every-answer-waits-for-its-audit-row) explains. `via` says how the principal proved itself. The row of a call that creates a plan commits with the plan and the token's use, so a call that stored nothing leaves no allowed row.

## Migrations

The schema counts its own versions in `schema_migrations`. A control database with another name for migration 1, such as the Postgres-only `0001_init.sql` of builds before 2026-11-10, is refused with that name.

A migration that the SQL both engines share can't say is a pair of files of one number, `NNNN_name.postgres.sql` and `NNNN_name.sqlite.sql`. Each engine runs its own and records it as `NNNN_name.sql`, and a file of a pair without its twin is refused. Migration 0019 is one: it lets a plan's status be `no-changes`. SQLite can't alter a table's CHECK, so on SQLite it edits the CHECK's text in place, the way SQLite's documentation gives for a change that leaves every row valid, and checks that the edit took. No row moves.

Migration 0016 dropped `stacks`, which recorded where builds from before the first release kept each `stack@env`'s state, and the tables of the CloudTrail change watcher. That state itself stays where it was: a schema named `state_ENV_STACK_HASH` in the database on Postgres, or a file under `state/` beside `control.db` under `writ serve --dev`. No version of `writ` reads it, so drop it when no one needs it.

## Connection timeouts and backups

On Postgres, each database session uses a 5-second connect timeout, a 10-second statement timeout, a 5-second lock timeout, and a 15-second idle-in-transaction timeout. On SQLite (`writ serve --dev`), Writ waits 5 seconds when the database is busy.

Never put a Postgres password directly in the `database` URL in your server config. Use Unix-socket peer authentication (as the [NixOS module](run.md#on-nixos) does), `?passfile=/path/to/pgpass`, or `PGPASSFILE`.

## Next steps

- [Back up and upgrade](upgrades.md): `pg_dump` backups, offline state keys, retention, and single-node upgrades.
- [Read the audit log](audit-log.md): query audit rows safely via `writ audit --remote` and `GET /v1/audit`.
- [Run a server](run.md): connect `writ serve` to Postgres.
