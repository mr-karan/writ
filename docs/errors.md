# Errors

Every error Writ prints carries a `class` (such as `saved-plan` or `denied`) so scripts and agents can branch reliably while humans read the message.

## How an error looks

Server refusals print `error: CLASS: MESSAGE`:

```
$ writ apply --remote --merged --stack app --repo demo --commit 7b47f2783365eda0f616386d196f42cf4fdf1897
error: saved-plan: no approved plan of app@dev matches the sources of demo at 7b47f278: plan the merge request again after a rebase, and approve the new plan
```

Output under `writ serve --dev`, 2026-11-16. Control characters in server messages are escaped; warnings print `warning:` and don't fail. With `--json`, stdout carries the server document ([JSON output](json-output.md#diagnostic)).

Local file checks (`writ serve --check`, `writ import bulk`) print `error[CLASS] FILE:LINE:COL`:

```
$ writ serve --config bad.hcl --check
error[syntax] bad.hcl:1:6
  unclosed configuration block: There is no closing brace for this block before the end of the file. This may be caused by incorrect brace nesting elsewhere in this file
```

Invalid CLI invocations print to stderr and exit 2:

```
$ writ plan
writ: plan runs on the control plane: writ plan --remote
run writ help for usage
```

Output of real runs, 2026-11-16.

## Classes at a glance

| Class | Meaning | Exit code | HTTP status |
| --- | --- | --- | --- |
| [`usage`](#usage) | Caller's CLI arguments or request are wrong | `2` (bad CLI/preconditions) or `1` | `400`, `404`, `405`, `409`, `413`, `429` |
| [`auth`](#auth) | Missing or invalid token, or failed webhook verification | `1` | `401` |
| [`denied`](#denied) | Grant missing, approvals missing, or author tried to approve | `1` | `403` |
| [`policy`](#policy) | Mandatory Rego policy refused the apply | `1` | `403` |
| [`state`](#state) | Plan stale/expired/`no-changes`, stack busy, or key closed | `1` | `409`, `404` |
| [`saved-plan`](#saved-plan) | Plan sources don't match or branch moved | `1` | `409` |
| [`state-io`](#state-io) | Database/audit write failed or apply response lost | `1` | `503`, `502` |
| [`provider`](#provider) | Provider, lock file, S3 read, or Git host failed | `1` | `502`, `409` |
| [`internal`](#internal) | Local I/O error or bug | `1` | `500` |
| [`syntax`](#syntax) | Server config HCL failed to parse | `1` | none |

Exit `0` indicates success (warnings only). See [Commands: Exit codes](commands.md#exit-codes) and [API: Error classes](api.md#error-classes). Capitalized words below are placeholders.

## `usage`

- **Exit code:** `2` for bad CLI flags/args/preconditions (`writ agent init` may still write `AGENTS.md` before exiting 2 on a symlink error); `1` for server refusals, failed OpenTofu steps, or timeouts.
- **HTTP status:** `400`, `404`, `405`, `409`, `413`, `429`.

Common messages:

- `plan runs on the control plane: writ plan --remote`
- `no control plane: start one with writ serve --dev, or sign in to your team's with writ login URL`
- `no plan ID`, `no route METHOD PATH`, `limit must be 1 to 200`
- `plan ID is of env ENV, which this config no longer serves`
- `the config pins the repositories with repos, so they are added there and reviewed with the config`
- `more than N requests a second from PRINCIPAL`
- `more than 10 webhook deliveries a second from ADDRESS`, `more than 5 webhook deliveries a second for repository NAME`
- `writ serve is running N applies; try again when one ends`
- `the apply ran past limits.apply_timeout, DURATION: OpenTofu was interrupted, started no more changes, and wrote the state it reached; plan again`

**Recovery:** Fix the flags or request per the message (`writ help`), or retry after `429`.

## `auth`

- **Exit code:** `1`. **HTTP status:** `401`.

Common messages:

- `no token: sign in with writ login, or send Authorization: Bearer with the OIDC token of a CI job`
- `the Authorization header is not Bearer TOKEN`
- `the session ended, ran out, or is not one writ login gave: sign in again with writ login URL`
- `the sign-in is unknown, finished, or ran out: run writ login again`
- `this token already applied; each CI token applies once`
- `the delivery failed verification` (any [webhook](webhooks.md#what-writ-serve-answers) failure, including a body over 1 MiB; inspect the audit row's `check`)

**Recovery:** Re-run `writ login`, re-run the CI job for a fresh OIDC token, or check the webhook secret/audit `check` ([Token checks](api.md#token-checks)).

## `denied`

- **Exit code:** `1`. **HTTP status:** `403`.

Common messages:

- `PRINCIPAL may not apply plan ID: REASON`, `PRINCIPAL may not approve plan ID: REASON`
- `PRINCIPAL made plan ID and cannot approve it: it needs 1 approval from someone else`
- `plan ID of STACK@ENV matches the sources at COMMIT and needs 1 approval; approve it with writ approve --remote ID`
- `a CI job applies by id only a plan of its own pipeline; after a merge, apply with merged`
- `a CI job applies its own commit: the token names COMMIT, not COMMIT`
- `commit COMMIT is not on REF of repository REPO, so it is not merged`
- `an override is a person's: a CI job cannot give one`
- `the key KEY started a run of another principal`

**Recovery:** Obtain required teammate approvals or missing grants ([Approvals](approvals.md)).

## `policy`

- **Exit code:** `1`. **HTTP status:** `403`.

Returns `policy refuses the apply of plan ID` followed by one diagnostic per result (`OUTCOME: [SET] MESSAGE`).

**Recovery:** Update the stack code to satisfy the Rego rule, commit, and plan again ([Write and test a policy](write-a-policy.md)).

## `state`

- **Exit code:** `1`. **HTTP status:** `409` (`404` when `writ state adopt` references an unknown version/serial).

Common messages:

- `plan ID is stale: plan ID of STACK@ENV is newer; plan again`
- `plan ID expired at TIME: plan again`
- `plan ID changes no resource; there is nothing to apply` / `nothing to approve or apply`
- `another run is applying to STACK@ENV; try again when it ends`
- `plan ID is a drift check of STACK@ENV, which writ serve never approves or applies: plan the stack to change it`
- `the key KEY is closed with no run: no run started; run the command again`
- `state version N of STACK@ENV is not its newest, version N: adopt the newest`
- `an apply of STACK@ENV runs, which may write a newer state: adopt after it ends`

**Recovery:** Plan again, wait for the active apply, or adopt the newest state ([Why an approved plan can still be refused](approvals.md#why-an-approved-plan-can-still-be-refused)).

## `saved-plan`

- **Exit code:** `1`. **HTTP status:** `409`.

Common messages:

- `no approved plan of STACK@ENV matches the sources of REPO at COMMIT: plan the merge request again after a rebase, and approve the new plan`
- `plan ID is superseded: REF moved to COMMIT, whose sources of STACK differ from the plan's, made at COMMIT; plan the new commit`
- `plan ID keeps no saved plan, so it cannot apply: plan its commit again`

**Recovery:** Plan the current commit and get the new plan approved ([Apply after a merge](apply.md#apply-after-a-merge)).

## `state-io`

- **Exit code:** `1`. **HTTP status:** `503` (`502` when an IdP fails to start sign-in).

Common messages:

- `the control database failed; the request id is ID`
- `the control database failed as run ID began, and writ serve cannot tell yet whether it started; ask with: writ show --remote --request KEY`
- `no run started: writ serve closed the key KEY of this command's requests with no run; run the command again`
- `writ serve did not say whether the apply request with the key KEY started a run; ask later with: writ show --remote --request KEY`
- `the identity provider of issuer ISSUER did not start a sign-in: REASON`

**Recovery:** Retry later. For a lost apply response, always run `writ show --remote --request KEY` before retrying ([When an answer is lost](apply.md#when-an-answer-is-lost)). Give admins the request ID.

## `provider`

- **Exit code:** `1`. **HTTP status:** `502` (Git host failure) or `409` (S3 adoption read failure).

Common messages:

- `WHAT: the forge answered STATUS`
- `the lock file is over N bytes, so a plan of the stack is refused`
- `provider SOURCE finds no TYPE with that import id`
- `N objects could not be imported, so nothing was written`

**Recovery:** Check Git host availability/read token, refresh `.terraform.lock.hcl` with `tofu providers lock`, or fix the `writ import bulk` selection ([Import many objects](import-bulk.md)).

## `internal`

- **Exit code:** `1`. **HTTP status:** `500`.

Common messages:

- `writ serve hit a bug; the request id is ID`
- `the mask could not read the saved plan, so the run keeps none of OpenTofu's events: REASON`

**Recovery:** Fix any local file permission named, or report bugs with the request ID and server log / stderr stack trace.

## `syntax`

- **Exit code:** `1`. **HTTP status:** none.

HCL parse error in server config. Fix the reported file/line/column and verify with `writ serve --config FILE --check` ([Server config](config.md)).
