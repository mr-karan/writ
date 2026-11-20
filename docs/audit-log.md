# Read the audit log

Query Writ's [audit log](glossary.md) from the control UI, the CLI, or the HTTP API to trace a change from plan creation through approvals, apply, and state writes, without connecting directly to Postgres.

For the full route specification and grant rules, see [API: Read the audit log](api.md#read-the-audit-log). For the database schema behind it, see [Operate the database](database.md#audit-rows).

## Prerequisites

- A running `writ serve` (or `writ serve --dev`) and a valid token from `writ login` or CI.
- Appropriate grants for the rows you want to inspect:
  - Rows for a specific `stack@env` require `read` on every stack listed in that row.
  - Rows for an environment with no specific stack require `read` on at least one stack in that environment.
  - Server-wide rows (server starts, sign-ins, sign-outs, `GET /v1/whoami`, failed auth attempts, repository additions) require `admin` on every stack in every environment (`envs = ["*"]`, `stacks = ["**"]`).

## Steps

### 1. List the newest audit rows

Run `writ audit --remote` to print the newest 50 rows your token can read:

```sh
writ audit --remote
```

### 2. Narrow the log with filters

Combine flags to trace a single change, person, stack, or time window:

```sh
# Follow one plan, including its approvals, apply run, and state writes
writ audit --remote --plan 927b69ec7af23641c68c3ce3cce370ec

# Follow one apply run
writ audit --remote --run I3EP3I6DTYNC5PMVULSW6JYINL

# Filter by stack and environment (requires --env with --stack)
writ audit --remote --env prod --stack app

# Filter by person id (as printed by writ whoami)
writ audit --remote --person gitlab/user/1001

# Filter by operation and RFC 3339 time window
writ audit --remote --op apply --since 2026-11-19T00:00:00Z --until 2026-11-19T23:59:59Z
```

Valid `--op` values are `read`, `plan`, `apply`, `approve`, `override`, `adopt`, `admin`, `state-read`, `state-write`, `state-lock`, and `state-unlock`.

### 3. Page through older rows

Use `--limit` (1 to 200, default 50) to control page size. When more rows match, the last line prints a `--cursor` value you pass to the next command:

```sh
writ audit --remote --limit 2
writ audit --remote --limit 2 --cursor CURSOR_FROM_PREVIOUS_OUTPUT
```

### 4. Read the audit log in the control UI

Open Audit in the control UI's sidebar, `/ui/audit`. It shows the same rows `GET /v1/audit` gives your session, newest first, one row an entry: its decision, the method, route, and status, the operation, the `stack@env`, who sent it, with a person or a CI icon, and when.

- The Env, Op, and Time pickers, and More filters, narrow the page by environment, stack, person, operation, plan, run, and time. Each is a GET parameter of the page (`?env=prod&stack=app`, `?person=gitlab/user/1001`, `?since=2026-11-19T00:00:00Z`), so the page works with scripts off and its URL can be shared.
- Older entries opens the next page with the API's encrypted cursor.
- Each plan's and run's page has an Audit trail button, which opens `/ui/audit?plan=ID` or `/ui/audit?run=ID`.
- A refused row shows a `reason withheld` chip, apart from a row whose reason is empty. The page shows no client address and no state hash, as the API doesn't.

### 5. Read the audit log as JSON or over HTTP

Pass `--json` for machine-readable `writ.audit/v0` output, or call `GET /v1/audit` directly:

```sh
writ audit --remote --env prod --stack app --json
```

```sh
curl -sS "https://writ.example.com/v1/audit?env=prod&stack=app&limit=50" \
  -H "Authorization: Bearer $TOKEN"
```

### What you should see

Here is a real run of `writ audit --remote`, pagination with `--limit` and `--cursor`, and `GET /v1/audit`:

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

Output under `writ serve --dev` run as root of a user namespace with only loopback, 2026-11-19.

## What every row guarantees

`writ serve` writes an audit row before sending any response, across the API, the control UI, and the `http` state backend. If the control database cannot commit the audit row within 5 seconds, `writ serve` returns `503` instead of the response.

Only three kinds of requests write no row: `GET /healthz`, `GET /v1/server`, and requests rejected with `429` by the per-client-address rate limit.

For privacy and secret hygiene, `GET /v1/audit` holds back fields that could echo untrusted request input:

- Refused rows (`refused`, `denied`, `auth-failed`, `error`) show `(withheld)` (`reason: ""`, `reason_withheld: true`) because refusal messages can quote fields from the request. Postgres still stores the full reason.
- `state-write` rows omit the state's SHA-256 hash in the API response so a reader cannot test guesses against the full state.
- Client IP addresses are omitted from API responses.
- Reading the audit log writes its own `read` audit row.

## Troubleshooting

- **Empty page with no error when filtering by `--env` or `--stack`**: Your token does not have `read` on that stack or environment. Run `writ whoami` to inspect your grants.
- **Cursor rejected with `400 usage`**: Cursors are encrypted with an ephemeral key created when `writ serve` starts. If the server restarted since the previous page, run the query again without `--cursor`.
- **Fewer rows than `--limit` even though `--cursor` is printed**: Each API call scans at most 5,000 database rows and skips rows your token cannot read. Pass the returned `--cursor` to continue scanning older rows.
- **`--stack` rejected without `--env`**: Always pass `--env` alongside `--stack`.
- **Missing `GET /healthz` or rate-limited 429 requests**: Those are exempt by design so bad-token floods cannot fill the audit table.

## Limits

- The control UI's audit page shows 50 entries a page; `writ audit --remote --limit 200` reads more at once.
- Refusal reasons and client addresses are only available by querying the `audit` table in Postgres directly.
- Set `retention.audit` in your [server config](config.md) if you want old audit rows pruned; by default they are kept forever.

## Next steps

- [API: Read the audit log](api.md#read-the-audit-log): complete parameter and authorization rules.
- [Operate the database](database.md): retention, backups, and the `audit` table layout.
- [Security model](security-model.md): how Writ prevents request secrets from leaking through audit reads.
