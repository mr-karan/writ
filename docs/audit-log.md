# Read the audit log

Query Writ's [audit log](glossary.md) from `/ui/audit`, the CLI, or the HTTP API to trace any change from plan through approvals, apply, and state writes without querying Postgres directly.

For route parameters and JSON fields, see [API: Read the audit log](api.md#read-the-audit-log).

## Prerequisites

- A running `writ serve` (or `writ serve --dev`) and a token from `writ login` or CI.
- Grants matching the rows you want to read:
  - Rows naming stacks require `read` on every stack in the row.
  - Environment-only rows require `read` on at least one stack in that environment.
  - [Webhook](webhooks.md) delivery rows require `read` on at least one stack of that pinned repository's current default branch (matching Git host and project ID).
  - Server-wide rows (server starts, sign-ins/sign-outs, `GET /v1/whoami`, failed auth, repository additions, and unmatched webhooks) require global `admin` (`envs = ["*"]`, `stacks = ["**"]`).

## Steps

### 1. List the newest audit rows

```sh
writ audit --remote
```

Prints the newest 50 rows your token can read.

### 2. Narrow the log with filters

```sh
# Follow one plan through approvals, apply, and state writes
writ audit --remote --plan 927b69ec7af23641c68c3ce3cce370ec

# Follow one apply run
writ audit --remote --run I3EP3I6DTYNC5PMVULSW6JYINL

# Filter by stack and environment (--stack requires --env)
writ audit --remote --env prod --stack app

# Filter by person ID (from writ whoami)
writ audit --remote --person gitlab/user/1001

# Filter by operation and RFC 3339 time window
writ audit --remote --op apply --since 2026-11-19T00:00:00Z --until 2026-11-19T23:59:59Z
```

Pass `--changes` to hide routine reads (`op` of `read` or `state-read`, and allowed/no-decision `GET`/`HEAD` requests without an op, such as `/v1/whoami` or UI page loads) while keeping all writes, webhooks, sign-ins, server starts, and refused requests:

```sh
writ audit --remote --changes
```

`--changes` can't be combined with `--op read` or `--op state-read`. Valid `--op` values are `read`, `plan`, `apply`, `approve`, `override`, `adopt`, `admin`, `state-read`, `state-write`, `state-lock`, `state-unlock`, and `hook`.

### 3. Page through older rows

Set `--limit` (1 to 200, default 50). When more rows match, the last line prints a `--cursor` value for the next page:

```sh
writ audit --remote --limit 2
writ audit --remote --limit 2 --cursor CURSOR_FROM_PREVIOUS_OUTPUT
```

### 4. Read the audit log in the control UI

Open `/ui/audit` from the sidebar. It displays the same rows `GET /v1/audit` authorizes, newest first:

- Defaults to **Changes** (`writ audit --remote --changes`); click **Show reads** (`?reads=1`) to include read requests. Selecting a specific **Op** shows all rows for that op.
- Filters use GET query parameters (`?env=prod&stack=app`, `?person=...`, `?since=...`), work without JavaScript, and produce shareable URLs.
- Plan and run pages include an **Audit trail** link to `/ui/audit?plan=ID` or `/ui/audit?run=ID`.
- Refused rows show a `reason withheld` chip; client IPs and state hashes aren't displayed.

### 5. Read the audit log as JSON or over HTTP

```sh
writ audit --remote --env prod --stack app --json
```

```sh
curl -sS "https://writ.example.com/v1/audit?env=prod&stack=app&limit=50" \
  -H "Authorization: Bearer $TOKEN"
```

### What you should see

```
$ writ audit --remote
AT                    BY          OP    ON       WHAT                    DECISION  PLAN  RUN  REASON
2026-11-19T15:54:09Z  root (dev)  read  app@dev  GET /v1/applies 200     allowed   -     -    -
2026-11-19T15:54:09Z  root (dev)  -     -        GET /v1/runs/{id} 404   refused   -     -    (withheld)
2026-11-19T15:54:09Z  root (dev)  -     -        GET /v1/plans/{id} 404  refused   -     -    (withheld)
2026-11-19T15:54:09Z  root (dev)  read  dev      GET /v1/applies 200     allowed   -     -    -
2026-11-19T15:54:09Z  -           -     -        started                 none      -     -    writ v0.1.5-0.20261119155342-d29b2589f15f
$ writ audit --remote --limit 2
AT                    BY          OP    ON       WHAT                 DECISION  PLAN  RUN  REASON
2026-11-19T15:54:09Z  root (dev)  read  -        GET /v1/audit 200    allowed   -     -    -
2026-11-19T15:54:09Z  root (dev)  read  app@dev  GET /v1/applies 200  allowed   -     -    -
More rows match: run the command again with --cursor 6sr58zWuNIVX3X5Gw2cVNdiLek78Ux4205UfZEcxsaffry8riQ9m31WxXzEpRCUA
$ curl -sS "$SERVER/v1/audit?env=dev&stack=app&limit=1" -H "Authorization: Bearer $TOKEN" | jq -c '{format, ok, rows: [.rows[] | {at, kind, route, status, principal, op, env, stacks, decision, reason, reason_withheld}], next}'
{"format":"writ.audit/v0","ok":true,"rows":[{"at":"2026-11-19T15:54:09Z","kind":"request","route":"/v1/applies","status":200,"principal":"root (dev)","op":"read","env":"dev","stacks":["app"],"decision":"allowed","reason":"","reason_withheld":false}],"next":""}
```

Output under `writ serve --dev` in a loopback-only user namespace, 2026-11-19.

## What every row guarantees

`writ serve` commits an audit row before sending any HTTP response across the API, control UI, and HTTP state backend. If the audit write doesn't commit within 5 seconds, `writ serve` returns `503`.

Only three categories write no row: `GET /healthz`, `GET /v1/server`, and requests rejected with `429` by per-client-address rate limits (server-wide or webhook per-address).

To avoid echoing untrusted input or leaking state:

- Refused rows (`refused`, `denied`, `auth-failed`, `error`) return `reason: ""` and `reason_withheld: true` in the API (the full reason stays in Postgres).
- `state-write` rows omit the state SHA-256 hash from API responses.
- Client IP addresses are omitted from API responses.
- Reading the audit log writes its own `read` audit row.

## Troubleshooting

- **Empty result when filtering by `--env` or `--stack`**: Your token lacks `read` on that target. Check `writ whoami`.
- **`400 usage` on `--cursor`**: Cursors are encrypted with an ephemeral per-process key; restart queries without `--cursor` after a server restart.
- **Fewer rows than `--limit` with `--cursor` returned**: Each request scans at most 5,000 database rows and skips unauthorized rows; pass `--cursor` to continue scanning.
- **`--stack` rejected without `--env`**: Always specify `--env` with `--stack`.
- **Missing `/healthz` or per-IP `429` rows**: Exempt by design so floods can't fill the audit table.

## Limits

- `/ui/audit` displays 50 entries per page; `writ audit --remote` supports up to `--limit 200`.
- Full refusal reasons, client IPs, and state SHA-256 hashes require querying Postgres `writ_control.audit` directly. HTTP state backend operations (`state-read`, `state-lock`, `state-unlock`, `state-write`) generate roughly 2 rows per plan and 7 per apply.
- Audit rows are kept indefinitely unless `retention.audit` is configured ([Config](config.md)).

## Next steps

- [API: Read the audit log](api.md#read-the-audit-log): complete parameter and authorization rules.
- [Back up and upgrade](upgrades.md): retention and Postgres backups.
- [Security model](security-model.md): how audit logging pairs with secret masking.
