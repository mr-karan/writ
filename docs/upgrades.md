# Back up and upgrade

Back up both the control database and every environment's state key, optionally configure retention, and upgrade your single-node `writ serve` cleanly.

## Prerequisites

- Postgres access (`pg_dump`) for `writ serve`.
- Host access to every `env.*.key` file.
- A verified new `writ` binary, container image, or Nix package ([Install](install.md)).

## Back up Writ

Back up **both** Postgres and every environment's state key file.

### 1. Back up the control database

```sh
pg_dump --format=custom --file=writ-$(date -u +%Y%m%dT%H%M%SZ).dump "postgres://writ@/writ?host=/run/postgresql"
```

Under `writ serve --dev`, back up the entire data directory (`~/.local/share/writ/dev`, containing `control.db` and `keys/`).

### 2. Back up every environment's state key offline

Each `env.*.key` (32 random bytes) [seals](glossary.md) HTTP-backend states, saved plans, and secret sets. Store encrypted offline copies separately from Postgres backups. Losing a key permanently prevents reading that environment's sealed states, secret sets, and saved plans.

### 3. Configure retention (optional)

Every hour, `writ serve` automatically marks plans past `plan_ttl` as `expired`, prunes expired CI token `jti`s, and prunes closed apply idempotency keys older than 25 hours.

By default, completed plans and audit rows are kept forever. Add a `retention` block to prune older records:

```hcl
retention {
  audit = "400d"
  plans = "30d"
}
```

`retention.plans` deletes `no-changes`, `refused`, `failed`, `expired`, and `superseded` plans (and their events) older than the duration unless referenced by an approval or run. `retention.audit` deletes older audit rows.

## Upgrade `writ serve`

### 1. Check for running applies and back up

Check `/ui/runs` or `writ runs --remote` to ensure no apply is running, then run `pg_dump`.

### 2. Validate your config with the new binary

```sh
/path/to/new/writ serve --config /etc/writ/serve --check
```

Resolve any removed settings or warnings before stopping the current server.

### 3. Stop the old node and start the new binary

Never run two `writ serve` processes against one database. Stop the service first, update the binary/image/NixOS generation, and start it:

```sh
systemctl stop writ-serve
# replace binary or update image / NixOS configuration
systemctl start writ-serve
```

On `SIGTERM`, `writ serve` drains in-flight requests for up to 30 seconds. On startup, the new binary applies pending schema migrations in Postgres schema `writ_control` in a single transaction (refusing to start against a database migrated by a newer version of Writ) and fetches OIDC issuer discovery/JWKS documents. HTTP-backend state retains the newest 20 versions per `stack@env`.

### 4. Verify health

```sh
curl -sS https://writ.example.com/healthz
```

### What you should see

```json
{"format":"writ.health/v0","ok":true,"database":true,"issuers":[],"diagnostics":[]}
```

Output of a real run, 2026-11-16 (a team server lists each issuer under `issuers`).

## Downgrades

Starting an older `writ` against a database migrated by a newer version fails immediately. Restore your pre-upgrade Postgres dump before rolling back.

## Troubleshooting

- **Startup fails with schema version error**: Verify you're not running an older binary against a newer database and check Postgres permissions on `writ_control`.
- **In-flight plans or applies marked `failed` after restart**: Expected recovery; Writ automatically revokes their state tokens and releases HTTP state locks. Re-plan and apply.
- **Pending `writ login` prompts fail after restart**: Unfinished login handles live in memory; run `writ login` again. Existing sessions in Postgres stay valid until `session_ttl`.
- **Two nodes started by accident**: Stop the extra node immediately; each process fails runs owned by other process IDs every minute.

## Next steps

- [Run a control plane](run.md): systemd, container, NixOS setup, and database timeouts.
- [Read the audit log](audit-log.md): query audit rows via CLI, UI, and API.
- [Supported platforms and limits](status.md): current limits and test coverage.
