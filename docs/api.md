# API

The JSON REST API of `writ serve`, used by the CLI, the control UI, and CI jobs.

- Request bodies are plain or `Content-Encoding: gzip`, capped at `limits.body` after decompression.
- Every `/v1/` response has `format`, `ok`, and `diagnostics`, and the header `X-Writ-Version` ([Version skew](commands.md#version-skew), [JSON output](json-output.md#documents-of-writ-serve)).
- Refusals use the route's own format with `ok: false`. Exception: pre-handler `/v1/policy` refusals return `writ.error/v0`.

## Routes

| Method and path | Grant | Response |
| --- | --- | --- |
| `GET /` | none | `303` redirect to `/ui/` (or `writ.server/v0` when UI is off) |
| `GET /v1/server` | none | `writ.server/v0`: version, URL, `dev`, issuers, `login`, `device_issuers` |
| `GET /healthz` | none | `writ.health/v0`: database and issuer status (`503` if unhealthy) |
| `POST /v1/auth/device` | none | `writ.device/v0`: starts device-flow [`writ login`](sign-in.md#sign-in-with-the-device-flow) (`429` past [pending limits](how-sign-in-works.md#sign-ins-that-wait)) |
| `POST /v1/auth/device/token` | none | `writ.session/v0`: `pending` or `signed-in` (token returned once) |
| `POST /v1/auth/browser` | none | `writ.browser/v0`: starts PKCE loopback [`writ login`](sign-in.md#sign-in-through-a-browser) (`429` past pending limits) |
| `POST /v1/auth/browser/token` | none | `writ.session/v0`: `signed-in` (token returned once) |
| `DELETE /v1/auth/session` | session | `writ.session/v0` (`ended`): revokes caller's session |
| `GET /v1/sessions?person=` | `admin` | `writ.sessions/v0`: up to 1,000 active sessions, newest first (never tokens) |
| `DELETE /v1/sessions/{id}` | `admin` | `writ.sessions/v0` with the revoked session |
| `GET /v1/whoami` | any token | `writ.whoami/v0`: principal, teams, grants |
| `POST /v1/plans` | `plan` | `202` and `writ.remote-plan/v0` (`planning`) |
| `GET /v1/plans?env=&stack=&status=&started_by=&limit=&cursor=` | `read` | `writ.remote-plans/v0`, newest first (`status` evaluated under current config; `started_by` is `person` or `ci`) |
| `GET /v1/plans/{id}?wait=60s` | `read` | `writ.remote-plan/v0` by ID or ref (`wait` up to `120s`) |
| `POST /v1/state/adopt` | `adopt` (person) | `writ.state-adopt/v0` ([Adopt a state](adopt-state.md)) |
| `GET /v1/config` | `read` on any stack | `writ.config/v0` (adds `limits` and `retention` for `admin`; no paths/secrets) |
| `GET /v1/policy` | `read` or `admin` on any stack | `writ.policy/v0`: active policy sets and `.rego` text ([Write and test policies](policy-reference.md#how-policy-decisions-work)) |
| `POST /v1/repos` | global `admin` | `201` and `writ.repo/v0` ([Repository routes](#repository-routes)) |
| `GET /v1/repos`, `GET /v1/repos/{name}` | `read` on any stack | `writ.repos/v0`, `writ.repo/v0` |
| `POST /v1/hooks/{forge}/{repo}` | webhook secret | `writ.hook/v0` ([Webhook route](#webhook-route)) |
| `POST /v1/dev/ui-link` | dev token | `writ.ui-link/v0` (`writ serve --dev` only) |
| `GET /v1/drift?env=&stack=&limit=` | `read` | `writ.drift/v0` (up to `limit`, default 50, max 200; adds `checks` history when both `env` and `stack` are set) |
| `GET /v1/drift/{id}` | `read` | `writ.drift-check/v0` by check `ref` or plan ID (`404` once pruned past 20 kept checks) |
| `GET /v1/index?env=&stack=` | `read` | `writ.state-index/v0` (`404` if unindexed) |
| `GET /v1/overview` | any token | `writ.overview/v0`: authorized counts, stacks, active/pending plans, alerts, and repo poll status |
| `POST /v1/plans/{id}/approvals`, `POST /v1/approvals`, `DELETE /v1/plans/{id}/approvals/mine` | `approve` | `writ.approve/v0` ([Approval routes](#approval-routes)) |
| `POST /v1/runs` | `apply` + risk classes | `202` and `writ.remote-run/v0` (`applying`), or `200` with `run: null` when merged apply has nothing to apply |
| `GET /v1/runs/{id}?wait=60s` | `read` | `writ.remote-run/v0` (`wait` up to `120s`) |
| `GET /v1/applies?env=&stack=&limit=` | `read` | `writ.remote-runs/v0`, newest first |
| `POST /v1/apply-requests/{key}/outcome` | token (`started`: caller or `admin`) | `writ.apply-outcome/v0`: `started` with run, or `none` (closes unused key) |
| `GET /v1/audit?plan=&run=&env=&stack=&person=&op=&changes=&since=&until=&limit=&cursor=` | any token | `writ.audit/v0` ([Read the audit log](#read-the-audit-log)) |
| `GET`, `POST`, `LOCK`, `UNLOCK /v1/tfstate/{stack}` (and `/lock`) | run state token | OpenTofu `http` backend (`plan` tokens: `GET` only; `apply` tokens: `GET`/`POST`/`LOCK`/`UNLOCK`) |
| `GET`, `POST /ui/...` | browser session | [Control UI](how-sign-in-works.md#the-control-ui) (`/ui/`, `/ui/stack?stack=&env=`, `/ui/plans`, `/ui/plans/{id}`, `POST /ui/plans/{id}/approve`, `/ui/runs`, `/ui/runs/{id}`, `/ui/drift/{id}`, `/ui/approvals`, `/ui/access`, `/ui/whoami`, `/ui/audit`, `/ui/repos`, `/ui/repos/new`, `/ui/repos/{name}`, `/ui/settings`, `/ui/start`, `/ui/login`, `/ui/login/dev`, `/ui/callback`, `/ui/logout`) |

### Error classes

| Class | HTTP status | Meaning |
| --- | --- | --- |
| `auth` | `401` | Missing/invalid token, spent CI token, or failed webhook verification |
| `denied` | `403` | Missing grant, missing approvals, or author self-approval |
| `policy` | `403` | Mandatory Rego policy refused the apply |
| `state` | `404`, `409` | Plan not live, `no-changes`, expired, stale, closed key, or unknown state to adopt |
| `saved-plan` | `409` | No approved plan matches sources, or source branch moved |
| `usage` | `400`, `404`, `405`, `409`, `413`, `429` | Invalid request, unknown ID, body too large, rate/concurrency limit, or disallowed by config |
| `provider` | `409`, `502` | Git host error (`502`) or S3 adoption read failure (`409`) |
| `state-io` | `502`, `503` | Database or audit commit error, or IdP sign-in start error (`502`) |
| `internal` | `500` | Server bug |

See [Errors](errors.md) for messages and recovery steps.

### Plans

A plan receives its hash `id` only after OpenTofu finishes; the `202` response references it by `ref` and `Location`:

```
$ curl -sS -i -X POST $SERVER/v1/plans -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' --data @req.json
HTTP/1.1 202 Accepted
Cache-Control: no-store
Content-Type: application/json
Location: /v1/plans/A37QLN5SAWRF4BPAFWTECPCBY2
X-Content-Type-Options: nosniff
X-Request-Id: DVMTCUXLO3LWMBRU44ORICSS7S
Date: Mon, 16 Nov 2026 03:29:55 GMT
Content-Length: 695

{"format":"writ.remote-plan/v0","ok":true,"plan":{"ref":"A37QLN5SAWRF4BPAFWTECPCBY2","id":"","status":"planning","env":"dev","stacks":[{"stack":"app","changes":0,"source_digest":""}],"author":{"kind":"dev","issuer":"dev","subject":"alice","display":"alice (dev)","person":"dev/alice","expires_at":""},"requested_at":"2026-11-16T03:29:55Z","risks":[],"teams_that_may_apply":[],"origin":{"kind":"dev"},"superseded_by":"","source":{"kind":"commit","repo":"demo","ref":"refs/heads/main","commit":"552091ed387347e8948abc28dd127d91513415e2","ref_check":"on-ref"},"approvals":{"need":0,"rules":[],"approved_by":[],"teams_that_may_approve":[]}},"document":null,"text":"","policy":null,"diagnostics":[]}
```

Output under `writ serve --dev`, 2026-11-16 (responses also include `network`, `null` when `egress_mode = "off"`).

`POST /v1/plans` takes `writ.plan-request/v1` (`env`, single-element `stacks`, `hints`, `source`); unknown fields are refused. A completed plan adds its `id`, final `status`, each stack's `changes` and `source_digest`, and the masked `document`.

`superseded_by` links to a newer pending plan in the same merge request:

```
$ curl -sS "$SERVER/v1/plans?env=prod&stack=network" -H "Authorization: Bearer $WRIT_ID_TOKEN" | jq -c '.plans[] | {id, superseded_by}'
{"id":"03bd0af91430f472335c13a6cd274a61","superseded_by":""}
{"id":"5f5963813d1e23e5d72d59f6d3570457","superseded_by":"03bd0af91430f472335c13a6cd274a61"}
```

Output of a real run, 2026-11-10. `document` is the masked plan ([Policy input](policy-reference.md#input)); the sealed saved plan is never served. `source_digest` hashes the stack's files, local modules, and `.terraform.lock.hcl`. Failed/refused plans store `{"format":"writ.plan.json/v2","ok":false,"diagnostics":[...]}` in `document`.

Example CI token refusals (replayed token, wrong audience, missing grant):

```
HTTP/1.1 401 Unauthorized
{"format":"writ.remote-plan/v0","ok":false,"diagnostics":[{"severity":"error","class":"auth","message":"this token already made a plan; each CI token makes one"}]}

HTTP/1.1 401 Unauthorized
{"format":"writ.remote-plan/v0","ok":false,"diagnostics":[{"severity":"error","class":"auth","message":"the token failed a check: audience: aud is not writ serve's URL"}]}

HTTP/1.1 403 Forbidden
{"format":"writ.remote-plan/v0","ok":false,"diagnostics":[{"severity":"error","class":"denied","message":"gitlab acme/infra #1849213 (alice) may not plan network@prod: no grant gives a principal in no team plan on network@prod"}]}
```

Output of a real run, 2026-11-10.

## Approval routes

| Method and path | Grant | Body | Response |
| --- | --- | --- | --- |
| `POST /v1/plans/{id}/approvals` | `approve` | empty, `{}`, or `writ.approve-request/v0` | `writ.approve/v0` |
| `POST /v1/approvals` | `approve` (CI token) | `writ.approve-request/v0` (`pipeline: true`) | `writ.approve/v0` (approves all live plans in the pipeline; passes 200 on all-`no-changes` pipelines) |
| `DELETE /v1/plans/{id}/approvals/mine` | `approve` | none | `writ.approve/v0` |
| `POST /v1/runs` | `apply` + risk classes (+ `override` if overriding) | `writ.run-request/v1` | `202` `writ.remote-run/v0`, or `200` with `run: null` for no-op merged apply |
| `POST /v1/state/adopt` | `adopt` (person) | `writ.state-adopt-request/v0` | `writ.state-adopt/v0` |
| `GET /v1/runs/{id}?wait=` | `read` | none | `writ.remote-run/v0` |
| `POST /v1/apply-requests/{key}/outcome` | token (`started`: caller or `admin`) | none | `writ.apply-outcome/v0` ([When an answer is lost](apply.md#when-an-answer-is-lost)) |

`POST /v1/runs` accepts `Idempotency-Key` and either a plan ID or merged-commit target:

```json
{"format": "writ.run-request/v1", "plan": "d3f700d860a97483a7ad509c31fe9e28"}
{"format": "writ.run-request/v1", "env": "prod", "stacks": ["network"], "merged": {}}
```

Direct apply of a `no-changes` plan by ID returns `409 state`. Merged applies return `200` (`run: null`, `plan: ID`) when a current `no-changes` plan matches or when the newest run already applied those sources and wrote the current HTTP state version (after checking the caller's `apply` grant).

## Apply request keys

The rules behind [When an answer is lost](apply.md#when-an-answer-is-lost).

### How the key works

Each `writ apply --remote` invocation makes one `Idempotency-Key`: Unix seconds, a dash, and 26 random characters, for example `1794405508-IDTB4TY5M2Z4M4RFR773KN4BKQ`. If a client omits the header, `writ serve` makes one. Once recorded, a key's outcome is final:

| Outcome | Meaning |
| --- | --- |
| `started` | A run started. The same request from the same principal returns `202` with that run, without a second run or a second CI token. The key on a different request returns `usage`. Another principal gets `denied`. |
| `none` | Closed with no run, by the outcome route or after a rolled-back commit. Any later apply with the key returns `state` with `key_closed: true`. |
| Unrecorded | No run started and no refusal committed the key. The client may reuse it. |

### Ask whether a run started

`POST /v1/apply-requests/{key}/outcome` returns `started` (with the run) or `none`. Asking about an unused key closes it to `none` atomically, so no delayed retry with that key can start a run.

### What writ apply does for you

After a transport error, a timeout, or a `503` whose `outcome` is not `none`, `writ apply --remote` asks the outcome route up to 5 times:

| Answer | CLI |
| --- | --- |
| `started` | Prints and follows the run. |
| `none` | Prints that no run started and exits 1. Rerun the command to use a new key. |
| Unreachable | Prints the key and `writ show --remote --request KEY`, then exits 1. |

- After a `429`, or a `503` with `outcome: "none"` and `key_closed: false`, it retries with the same key.
- After a `503` or `409` with `key_closed: true`, it retries with a new key.
- All retries share the 5-attempt cap. A `503` with `outcome: "unknown"` never rotates the key.

Messages it prints (real runs against a local stand-in, 2026-11-16):

| Case | Message |
| --- | --- |
| No answer, run had started | `The apply request got no answer: POST http://127.0.0.1:41971/v1/runs: EOF`, then `Asking writ serve whether it started a run.`, then the run as usual |
| No run started | `error: state-io: no run started: writ serve closed the key KEY of this command's requests with no run; run the command again` |
| Outcome route failed | `error: state-io: writ serve did not say whether the apply request with the key KEY started a run; ask later with: writ show --remote --request KEY` |
| Server rolled back, `503` with `key_closed: true` | `writ serve closed the key KEY with no run. Sending the request again with a new key.` |

### What each 503 means

- A `503` from `POST /v1/runs` sets `outcome: "none"` if no run started, or `outcome: "unknown"` if the server cannot tell. A `503` from the outcome route always sets `outcome: "unknown"`.
- Only `outcome: "none"` from the outcome route, or `key_closed: true` on `POST /v1/runs`, guarantees the key can never start a run.
- A `503` with `outcome: "none"` and no `key_closed` failed before commit, so the same key can be retried.
- If a database commit error leaves the outcome unknown, `writ serve` returns `503` with `outcome: "unknown"` and reconciles the run in the background, or marks an orphaned `applying` run `failed` on its minute sweep (see [What a run records](plan-lifecycle.md#what-a-run-records)).

### Who can learn a key's outcome

- A `started` key: only the principal that started the run (same issuer and subject) or a global `admin`. Anyone else gets `403 denied`, which is audited.
- An unused key: any authenticated principal can query it and close it to `none`.
- Keys have 130 random bits and cannot be guessed, but anyone who reads a key from a log before the apply runs can close it.
- In GitLab CI, a job's subject names the project and branch, so jobs on the same branch share a principal here.

### How long a key lives

- A key is accepted for 24 hours after its embedded Unix timestamp, and at most 5 minutes ahead of server time.
- `started` rows live as long as their run.
- Hourly pruning deletes `none` rows older than 25 hours and, in the same transaction, records a monotonic prune floor. A key at or below the floor is refused as `state` with `key_closed: true`, so a clock rollback cannot reopen pruned keys.

## Repository routes

| Method and path | Grant | Response |
| --- | --- | --- |
| `POST /v1/repos` | global `admin` | `201` `writ.repo/v0` (`409` if pinned or colliding; `404` if missing on forge) |
| `GET /v1/repos`, `GET /v1/repos/{name}` | `read` on any stack | `writ.repos/v0`, `writ.repo/v0` |
| `POST /v1/dev/ui-link` | dev token | `writ.ui-link/v0` (`--dev` only) |

```json
{"format": "writ.repo-request/v0", "name": "infra", "forge": "gitlab", "project": "acme/infra", "path": "infra", "prefix": ""}
```

Unknown fields (such as tokens) are refused; forge tokens never appear in responses, logs, audit rows, or UI pages.

## Webhook route

`POST /v1/hooks/{forge}/{repo}` receives GitLab webhooks for pinned repositories with `webhook_secret_file` ([Get changes in seconds with a webhook](webhooks.md)). It takes no bearer token; deliveries authenticate with `X-Gitlab-Token` or a `whsec_` `webhook-signature`.

| Delivery | Response |
| --- | --- |
| Verified `Push Hook` | `202`, `writ.hook/v0`, `queued: true`. Triggers an immediate default-branch read through the forge API. The body is capped at 1 MiB and never parsed, stored, or logged. |
| Duplicate `webhook-id` (or `Idempotency-Key` before GitLab 19.0) | `200`, `duplicate: true` |
| Verified, not a push | `200`, `queued: false` |
| Any verification failure: unknown repository or webhook, missing or wrong secret, bad signature or timestamp, body over 1 MiB (`check: hook-body`) | `401`, `auth`: `the delivery failed verification` |
| Over 10 deliveries/s from one client IP | `429`, `Retry-After: 1`. Dropped before the body is read; no audit row. |
| Over 5 deliveries/s for one repository | `429`, `Retry-After: 1`. Audited. |

Every other delivery writes an `op = hook` audit row.

### Audit rows and check codes

Deliveries that pass the per-address rate limit write an audit row with `op = hook`. When the URL matches a pinned repository with a webhook, `via` is `webhook of repository NAME`. Principals with `read` on any stack at the repository's current default branch can read its webhook rows; rows for earlier projects, unknown webhooks, or missing project identities require global `admin`. Query them with `writ audit --remote --op hook` or filter by `hook` in `/ui/audit`.

Every 401 returns the same message so senders learn nothing. Inspect `check` in the audit row to diagnose failures:

| `check` | Reason |
| --- | --- |
| `hook-body` | Body exceeds 1 MiB or failed to read |
| `hook-unknown` | No pinned repository with a webhook matches that forge and repo name |
| `hook-no-token` | Missing `X-Gitlab-Token` header |
| `hook-token` | `X-Gitlab-Token` doesn't match the secret |
| `hook-secret-file` | Secret file is unreadable, not a regular file <= 4 KiB, or has invalid base64 after `whsec_` (see server log) |
| `hook-no-signature` | Missing `webhook-id`, `webhook-timestamp`, or `webhook-signature` under a signing token |
| `hook-timestamp` | `webhook-timestamp` is more than 5 minutes from server time |
| `hook-signature` | Signature doesn't match the raw body |

GitLab treats any 4xx response as a failure. Normal project traffic stays far below these rate limits, so a 429 indicates unexpected traffic to the endpoint.

### Webhook security

- Secret tokens are compared in constant time via SHA-256 digests, hiding both value and length.
- Secrets and failed candidate tokens never appear in logs, audit rows, API responses, or the control UI.
- Webhooks are unauthenticated by bearer token; the per-IP rate limit runs before reading the body.
- Bodies are capped at 1 MiB and never stored or logged.
- The newest 4,096 delivery IDs are deduplicated in memory. After a restart, a resent delivery triggers at most one extra head read.
- A delivery never triggers plans or applies and never chooses what to fetch; `writ serve` always reads the stored project ID and default branch using the forge's read token.

## Token checks

Compact JWS tokens signed with `RS256` or `ES256`, checked in this order. The first failure returns `401 auth`.

| # | Check | Rule |
| --- | --- | --- |
| 1 | Shape | 3 strict base64url segments, at most 16 KiB. JWE refused. |
| 2 | Header | `alg` is `RS256` or `ES256`. `kid` required. `jku`, `jwk`, `x5u`, `x5c`, `crit`, `b64`, `enc`, `zip`, `epk`, and duplicate JSON keys refused. |
| 3 | Issuer | `iss` equals a configured issuer's `url` exactly. |
| 4 | Signature | Verified against the issuer's cached JWKS. The key's `kid`, type, and `alg` must match. |
| 5 | Times | `exp` and `iat` required, `nbf` optional, 60s leeway. |
| 6 | Audience | `aud` is `writ serve`'s `url` alone. |
| 7 | Claims | Non-empty `sub` and `jti`, and every required claim of the issuer's kind. GitLab CI: `project_id`, `project_path`, `namespace_id`, `pipeline_id`, `pipeline_source`, `job_id`, `ref_path`, `ref_protected`, `sha`, `user_id`, `user_login`. GitHub Actions: `repository`, `repository_id`, `repository_owner_id`, `run_id`, `ref`, `sha`, `event_name`, `workflow_ref`, `actor`, `actor_id`. |
| 8 | Replay | Each CI token `jti` allows one write, recorded until `exp`. |

JWKS refreshes hourly, or per `Cache-Control: max-age` clamped to 5m to 1h, and at most once a minute on an unknown `kid`. On a fetch error it falls back to the stale set for 24h. Fetch bounds: 10s timeout, 1 MiB, at most 100 keys, RSA 2048 to 8192 with `e=65537`, EC P-256, HTTPS on the issuer's host, no redirects.

### Identity provider refusals

Writ never echoes raw text from identity provider responses or tokens (preventing credential leaks in errors, logs, or audit rows). Every failure maps to a fixed stage/check refusal carrying only Writ-computed counts, positions, or known RFC 6749 codes:

| Stage | When | Reported detail |
| --- | --- | --- |
| `config` | startup | Non-HTTPS issuer URL or missing client ID/secret |
| `discovery` | startup | HTTP status, redirect, unreadable body, issuer mismatch, non-HTTPS endpoint |
| `key set` | startup / refresh | HTTP status, size, key count, or out-of-bounds key index |
| `device authorization` | `POST /v1/auth/device` | HTTP status/RFC error code or unusable response fields |
| `token endpoint` | code exchange / device poll | HTTP status/RFC error code, missing `id_token`, refusal, or expiry |
| `userinfo` | `groups = "userinfo"` | HTTP status/RFC error code or non-object JSON |
| `authorization endpoint` | `/ui/callback` error | Known RFC error code |
| `shape`, `header`, `issuer`, `signature`, `times`, `audience`, `claims`, `nonce` | CI / ID tokens | Failing field and check name |

```
{"format":"writ.remote-plan/v0","ok":false,"diagnostics":[{"severity":"error","class":"auth","message":"the token failed a check: audience: aud is not writ serve's URL"}]}
{"format":"writ.session/v0","ok":false,"diagnostics":[{"severity":"error","class":"auth","message":"the sign-in failed: audience: aud does not name the issuer's client id"}]}
level=WARN msg=auth request=RZ4BUB5QEUPBUFHMPEIRJOB2XL issuer=sso check=audience remote=127.0.0.1 reason="audience: aud does not name the issuer's client id"
issuer gitlab: discovery: the discovery document names another issuer than the configured URL
```

Output of a real run, 2026-11-10.

## Every answer waits for its audit row

Every HTTP response across `/v1/`, `/ui/`, and `/v1/tfstate/` waits for its audit row to commit (5-second timeout; failure returns `503 state-io` in the route's format with no cookies or redirects).

Exempt from audit rows:

- Exact paths `GET /healthz` and `GET /v1/server`.
- Requests rejected with `429` by per-client-address rate limits (server-wide or webhook per-address).

State-changing operations (plans, approvals, applies, adoptions, repo adds, sessions, state writes/locks/unlocks) commit their audit row in the same database transaction as the mutation. Because states can reach 64 MiB, `GET /v1/tfstate/{stack}` commits its `200` audit row before streaming the decrypted state (a subsequent read failure returns `500` and logs the version).

## Read the audit log

`GET /v1/audit` returns authorized rows newest first (see [Read the audit log](audit-log.md) for CLI and `/ui/audit` usage).

| Parameter | Filter |
| --- | --- |
| `plan` | Plan (by ID or ref) plus its approvals, run, and run state requests |
| `run` | Single apply run |
| `env`, `stack` | Environment and stack (`stack` requires `env`) |
| `person` | Stable person ID (for example `gitlab/user/1001`) |
| `op` | `read`, `plan`, `apply`, `approve`, `override`, `adopt`, `admin`, `state-read`, `state-write`, `state-lock`, `state-unlock`, `hook` |
| `changes` | `true` hides routine reads (`read`, `state-read`, allowed read-only `GET`/`HEAD`); incompatible with `op=read` / `state-read` |
| `since`, `until` | RFC 3339 inclusive start / exclusive end |
| `limit` | `1` to `200` (default `50`) |
| `cursor` | Opaque encrypted cursor from `next` (invalidated on server restart) |

Authorization: stack rows require `read` on every listed stack; env-only rows require `read` on any stack in that env; webhook rows require `read` on a stack of that pinned repo's current default branch (matching Git host and project ID); server-wide and unmatched rows require global `admin`. Each call scans up to 5,000 rows, omitting unauthorized rows and returning `next` if more remain (filtering an unauthorized env/stack returns an empty page immediately without scanning).

Sanitization: client IPs are omitted; non-standard HTTP methods become `OTHER`; `state-write` rows omit state SHA-256 hashes; refused rows return `reason: ""` with `reason_withheld: true` so untrusted request strings never leak across principals.

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

Output under `writ serve --dev`, 2026-11-19.

## Rate limits

Two token-bucket limiters, each with a 1-second burst and up to 10,000 buckets:

| Limiter | Default | Checked | Excess |
| --- | --- | --- | --- |
| Per client address, `limits.requests_per_address` | `20`/s; IPv6 grouped by `/64` | Before routing, auth, or body reads | `429` (`Retry-After: 1`), no audit row, server log throttled to once per 10s |
| Per principal, `limits.requests_per_second` | `10`/s | After auth | `429`, with an audit row |

- Webhooks also enforce 10/s per IP (no audit row) and 5/s per repository (audited).
- `X-Forwarded-For` is honored only when the TCP peer is inside `trusted_proxies`; Writ takes the rightmost untrusted IP.
- `writ serve --dev` uses 100/s for both limits.

## Time limits

| Limit | Default | Covers | On expiry |
| --- | --- | --- | --- |
| `limits.plan_timeout` | `30m` | Commit fetch, UID wait, `init`, `plan` | Kills the process group and marks the plan `failed`. |
| `limits.apply_start_timeout` | `10m` | The wait before an apply's first change: the commit fetch, the provider install, the saved plan, the role's credentials, and a provider UID | The apply fails as `usage` before any change starts, so state is left as it was. |
| `limits.apply_timeout` | `4h` | `tofu apply` | Sends OpenTofu `SIGINT` so running changes finish and no new ones start, fails the run, and kills OpenTofu if it is still alive 2 hours later. |

If a run's processes do not exit after termination, its UID is withheld until restart:

```
the plan ran past limits.plan_timeout, 500ms, while it waited for a provider uid: 2 of the 2 provider_uids are withheld because their processes did not end, and the others serve running plans and applies
```

```
error: the apply ran past limits.apply_timeout, 1s: OpenTofu was interrupted, started no more changes, and wrote the state it reached; plan again
```

## What the API refuses

| Threat | Defense |
| --- | --- |
| Forged JWT, `alg: none`, algorithm confusion | Strict `RS256`/`ES256`, issuer JWKS only, no header keys (`jku`/`jwk`/`x5u`/`x5c`) |
| Cross-service or replayed CI token | Single-audience `url` check; `jti` recorded until `exp` |
| Oversized tokens, headers, or gzip bombs | 16 KiB token, 64 KiB headers, auth before body read, 16 MiB post-gzip stream cap |
| Request/audit floods | Per-IP 429 before auth with no audit row; per-principal rate and plan/apply concurrency caps |
| Unpinned providers or server disk reads | Commits fetched from Git host and blob-verified; providers installed offline/read-only from `.terraform.lock.hcl` |
| Provider reading keys, other runs, or metadata | Per-run `provider_uids`, Linux namespaces, seccomp, cgroups, `nftables` metadata drop, short-lived loopback credential tokens |
| Secret leaks via IdP errors, audit reads, or run output | Fixed OIDC refusals, sanitized `/v1/audit`, single mask over all run outputs |

**Limits:** A pinned provider can still exercise its run's assumed role for the duration of the run.
