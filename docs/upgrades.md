# Back up and upgrade

Back up your Writ control plane's database and state keys, configure retention, and upgrade `writ serve` safely across releases.

Writ is at version 0, and a team's control plane runs as a single node. Plan a brief maintenance window when upgrading.

## Prerequisites

- Access to the Postgres instance backing `writ serve` (for `pg_dump`).
- Access to the host where `writ serve` runs and its environment state key files (`env.*.key`).
- The new `writ` binary (or container image / Nix package) verified as described in [Install](install.md).

## Back up Writ

You must back up **both** the control database and every environment's state key file.

### 1. Back up the control database

On a team server, Postgres holds all plans, approvals, runs, sessions, audit rows, state indexes, and `http`-backend states in the `writ_control` schema:

```sh
pg_dump --format=custom --file=writ-$(date -u +%Y%m%dT%H%M%SZ).dump "postgres://writ@/writ?host=/run/postgresql"
```

Under `writ serve --dev`, back up the entire data directory (`~/.local/share/writ/dev` by default), which contains `control.db` and `keys/`.

### 2. Back up every environment's state key offline

Each environment's `key` file (32 random bytes) [seals](glossary.md) its `http`-backend states, saved plans, and secret sets. Store encrypted offline copies of every `env.*.key` file separately from database backups.

If you lose an environment's key file, you cannot decrypt its sealed states, secret sets, or saved plans.

### 3. Configure retention (optional)

By default, `writ serve` keeps spent plans and audit rows forever. Every hour it automatically:

- marks live plans past `plan_ttl` as `expired`,
- deletes expired CI token `jti` rows, and
- deletes `none` rows from `apply_requests` 25 hours after the key's timestamp (while recording the highest deleted timestamp in `apply_key_floor` so clock skew cannot reopen a closed key).

To prune old plans and audit rows automatically, add a `retention` block to `/etc/writ/serve`:

```hcl
retention {
  audit = "400d"
  plans = "30d"
}
```

`retention.plans` deletes `refused`, `failed`, `expired`, and `superseded` plans (and their events) older than the duration, unless an approval or run references them. `retention.audit` deletes audit rows older than the duration.

## Upgrade `writ serve`

### 1. Check for running applies and back up

Check the control UI (`/ui/runs`) or run `writ runs --remote` to verify no apply is currently running. Then take a fresh Postgres backup.

### 2. Validate your config with the new binary

Before stopping the running server, test your existing config against the new binary:

```sh
/path/to/new/writ serve --config /etc/writ/serve --check
```

Fix any removed settings or new validation warnings before proceeding.

### 3. Stop the old node and start the new binary

Never run two `writ serve` instances against the same database at the same time. Stop the existing service first, swap the binary (or container image / NixOS generation), and start it:

```sh
systemctl stop writ-serve
# replace binary or update image / NixOS configuration
systemctl start writ-serve
```

On `SIGTERM`, `writ serve` stops accepting new requests and waits up to 30 seconds for in-flight requests to finish. On start, the new binary automatically runs any pending SQL schema migrations in a single transaction, then verifies OIDC issuer discovery documents and key sets.

### 4. Verify health

Check `/healthz` and the server version:

```sh
curl -sS https://writ.example.com/healthz
```

### What you should see

`/healthz` returns `200 OK` with `ok: true` and `database: true`:

```json
{"format":"writ.health/v0","ok":true,"database":true,"issuers":[],"diagnostics":[]}
```

Output of a real run, 2026-11-16 (a team server lists each configured issuer in `issuers`).

## Version-specific upgrade notes

- **Downgrades are refused**: If you roll back to an older `writ` binary against a database migrated by a newer version, `writ serve` refuses to start. Restore your pre-upgrade Postgres backup before downgrading.
- **Upgrading from pre-release builds (before 2026-11-10 / 2026-11-12)**:
  - Migration 1 expects the unified SQLite/Postgres schema; older Postgres-only `0001_init.sql` databases are refused.
  - Migration 5 expires any live plans created before approvals existed (`made before approvals`); re-plan those stacks after upgrading.
  - Migration 16 dropped legacy `stacks` and CloudTrail watcher tables; old `state_ENV_STACK_HASH` schemas in Postgres are no longer read and can be dropped once unneeded.
  - Stack patterns with `**` now match any number of path segments (previously `**` matched one segment like `*`). Review `stacks` patterns in grants and approval rules before restarting.

## Troubleshooting

- **New binary fails at start with a migration or schema version error**: Confirm you are not running an older binary against a newer database, and verify Postgres connectivity and permissions on the `writ_control` schema.
- **In-flight plans or applies show `failed` after restart**: Any plan or apply left in flight when a process stops is marked `failed` on the next start, and its `http`-backend state locks are automatically released. Re-plan and apply cleanly.
- **In-progress `writ login` sessions stop working after restart**: Pending device and browser login handles live in server memory; users simply run `writ login` again. Existing completed sessions stored in Postgres remain valid until `session_ttl`.
- **Two nodes started by accident**: Stop the second node immediately. Each `writ serve` process marks runs owned by other process IDs as `failed` and releases their locks on start and every minute after.

## Next steps

- [Operate the database](database.md): inspect the control database tables and migrations.
- [Run a server](run.md): systemd, container, and NixOS service setup.
- [Status and limits](status.md): version 0 compatibility expectations.
