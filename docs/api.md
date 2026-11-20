# API

`writ serve` serves a REST API. The CLI, the control UI, and CI jobs all use it, so anything you can do there you can also do with `curl` and a token. Request bodies may be sent uncompressed or with `Content-Encoding: gzip` (bounded by `limits.body` after decompression). Every `/v1/` response is a Writ document with `format`, `ok`, and `diagnostics`. A refusal is the route's own document with `ok: false` and one diagnostic (except pre-handler refusals on `/v1/policy`, which use `writ.error/v0`). [JSON output](json-output.md#documents-of-writ-serve) lists the fields of each document.

## Routes

| Method and path | Grant | Response |
| --- | --- | --- |
| `GET /` | none | `303` redirect to `/ui/` when the control UI is on, or `writ.server/v0` when it is off |
| `GET /v1/server` | none | `writ.server/v0`: the version, the URL, whether it is `writ serve --dev`, the issuers, and which people issuers offer the device flow |
| `GET /healthz` | none | `writ.health/v0`: the database, each issuer's last key set and clock offset, 503 when the database or an issuer fails. |
| `POST /v1/auth/device` | none | `writ.device/v0`: starts a [`writ login`](sign-in.md#sign-in-with-the-device-flow) sign-in with the device flow. The body can set an `issuer`. 429 past [the sign-ins that can wait](sign-in.md#sign-ins-that-wait). |
| `POST /v1/auth/device/token` | none | `writ.session/v0`: `pending` with the interval, or `signed-in` with the session and, in this one answer only, its token |
| `POST /v1/auth/browser` | none | `writ.browser/v0`: starts a [`writ login`](sign-in.md#sign-in-through-a-browser) sign-in through a browser. The body sets the loopback `redirect_uri`, the `state`, the S256 `code_challenge`, and optionally an `issuer`. 429 past [the sign-ins that can wait](sign-in.md#sign-ins-that-wait). |
| `POST /v1/auth/browser/token` | none | `writ.session/v0`: `signed-in` with the session and, in this one answer only, its token. The body sets the `handle`, and the callback's `code`, `state`, and `code_verifier`. |
| `DELETE /v1/auth/session` | a session | `writ.session/v0` with status `ended`: ends the session the request sends |
| `GET /v1/sessions?person=` | `admin` | `writ.sessions/v0`: the live sessions, newest first, at most 1,000. Never a token. |
| `DELETE /v1/sessions/{id}` | `admin` | `writ.sessions/v0` with the session it ended |
| `GET /v1/whoami` | a token | `writ.whoami/v0`: the principal, its teams, and its grants. A session's principal carries the session's id. |
| `POST /v1/plans` | `plan` | 202 and `writ.remote-plan/v0` with status `planning` |
| `GET /v1/plans?env=&stack=&status=&started_by=&limit=&cursor=` | `read` | `writ.remote-plans/v0`, newest first, only the plans the token can read. `status` matches the status each plan shows, counted under the current config. `started_by` is `person` or `ci`; `drift` is refused with 400, since drift checks aren't plans and `GET /v1/drift` lists them. |
| `GET /v1/plans/{id}?wait=60s` | `read` | `writ.remote-plan/v0`, by id or by ref. With `wait`, the request waits for a running plan to end, at most 120 seconds. |
| `POST /v1/state/adopt` | `adopt`, for a person | `writ.state-adopt/v0`: seals a secret set with the newest state of an OpenTofu `stack@env` that has none, as [Adopt a state](adopt-state.md) says. The body is `writ.state-adopt-request/v0`. |
| `GET /v1/config` | `read` on any `stack@env` | `writ.config/v0`: issuers, teams, grants, environment names, and lifetimes. `admin` also gets the limits and the retention. It has no path and no secret. |
| `GET /v1/policy` | `read` or `admin` on any `stack@env` | `writ.policy/v0`: the [policy sets](policy.md#read-the-policy-the-server-runs) and the text of their `.rego` files. It has no path. |
| `POST /v1/repos` | `admin` on every stack in `*`, as [Repositories](add-repository.md#rules-for-adding-a-repository) says | 201 and `writ.repo/v0`: a [repository](#repository-routes) and the stacks found |
| `GET /v1/repos`, `GET /v1/repos/{name}` | `read` on any `stack@env` | `writ.repos/v0` and `writ.repo/v0` |
| `POST /v1/dev/ui-link` | the dev token | `writ.ui-link/v0`: a control UI sign-in link, under `writ serve --dev` only |
| `GET /v1/drift?env=&stack=&limit=` | `read` | `writ.drift/v0`: each OpenTofu `stack@env` that [drift checks](serve.md#drift-checks) plan and the token can read, by env and stack, at most `limit`, 50 by default and 200 at most: its backend, the resource count and version of its state index, the S3 `outside_write` mark, whether a check runs now, and its newest check that ended, with its `ref`. Stacks the token can't read are left out before the limit counts, and `more` says others were left out. With both `env` and `stack`, `checks` lists the checks `writ serve` keeps of that `stack@env`, newest first: each one's `ref`, `plan`, `status`, times, `commit`, and counts. |
| `GET /v1/drift/{id}` | `read` on the check's `stack@env` | `writ.drift-check/v0`: one drift check, by its `ref` or the id of the plan it made: its `env`, `stack`, `backend`, `every`, and `check`, with its changes, or its `error` when it failed. `ok` is `false` for a failed check. 404 for a check `writ serve` no longer keeps. |
| `GET /v1/index?env=&stack=` | `read` | `writ.state-index/v0`: the [state index](serve.md#the-state-index) of one OpenTofu `stack@env`. 404 when it has nothing for it. |
| `GET /v1/overview` | any token | `writ.overview/v0`: what `writ serve` does in each environment where the token can read a stack, in one answer, for the control UI's sidebar and overview. It has only the stacks, plans, and runs that `GET /v1/drift`, `GET /v1/plans`, and `GET /v1/applies` would answer the same token, and the repositories' polls only for a token that `GET /v1/repos` answers. A token that may read nothing gets no environment. |
| `POST /v1/plans/{id}/approvals`, `POST /v1/approvals`, `DELETE /v1/plans/{id}/approvals/mine` | `approve` | `writ.approve/v0`. See [Approval routes](#approval-routes). |
| `POST /v1/runs` | `apply`, with the plan's risk classes | 202 and `writ.remote-run/v0` with status `applying` |
| `GET /v1/runs/{id}?wait=60s` | `read` | `writ.remote-run/v0`. With `wait`, the request waits for a run that applies to end, at most 120 seconds. |
| `GET /v1/applies?env=&stack=&limit=` | `read` | `writ.remote-runs/v0`: the runs `writ serve` started, newest first, with each one's stack, plan, status, and times, only those of stacks the token can read. `more` says older runs were left out. Each run carries its plan's source digest in `sources`. |
| `POST /v1/apply-requests/{key}/outcome` | a token, and for a `started` key, the run's principal or `admin` | `writ.apply-outcome/v0`: `started` with the run, or `none`. A key with no row gets a `none` row. See [When an answer is lost](apply.md#when-an-answer-is-lost). |
| `GET /v1/audit?plan=&run=&env=&stack=&person=&op=&since=&until=&limit=&cursor=` | any token | `writ.audit/v0`: a page of the audit log, newest first, only the rows the token may see. See [Read the audit log](#read-the-audit-log). |
| `GET`, `POST`, `LOCK`, `UNLOCK /v1/tfstate/{stack}` (and `/v1/tfstate/{stack}/lock`) | a run's state token | OpenTofu `http` state backend protocol, reachable only by a running plan or apply's single-run token (`plan` tokens may only `GET`; `apply` tokens may `GET`, `POST`, `LOCK`, and `UNLOCK`). |
| `GET`, `POST /ui/...` | browser session cookie | HTML pages and actions of the [control UI](sign-in.md#the-control-ui) (`/ui/`, `/ui/plans`, `/ui/plans/{id}`, `/ui/runs/{id}`, `/ui/drift/{id}`, `/ui/audit`, `/ui/stacks/{env}/{stack...}`, `/ui/login`, `/ui/callback`, `/ui/logout`, `/ui/dev-login`). |

### Error classes

| Class | Status | Meaning |
| --- | --- | --- |
| `auth` | 401 | No token, a token that failed a check, or a token that already created a plan, an approval, or a run |
| `denied` | 403 | No grant allows it, the plan lacks its approvals, or its author approves it |
| `state` | 404, 409 | The plan isn't live, it expired, or state changed after it was created, or `writ serve` knows no state to adopt |
| `saved-plan` | 409 | No approved plan matches the sources, or the plan's branch moved to other sources |
| `usage` | 400, 404, 405, 409, 413, 429 | A malformed request, an unknown id, a body over the limit, too many requests, or a request the config doesn't allow, such as adding a repository to a config that pins them |
| `policy` | 403 | A [policy](policy.md) refused the apply |
| `provider` | 409, 502 | The Git host couldn't be read (502), or `POST /v1/state/adopt` couldn't read the stack's state on S3 (409) |
| `state-io` | 502, 503 | The control database failed, or it didn't write the request's audit row within 5 seconds, or an identity provider didn't start a sign-in (502). A 503 of `POST /v1/runs` and of the outcome route has `outcome`: `none` when the request started no run, `unknown` when `writ serve` can't tell. |
| `internal` | 500 | A bug |

[Errors](errors.md) lists the common messages of each class and what to do.

### Plans

A plan has no id until it ends, because its id is a hash of what OpenTofu planned. So the 202 refers to it by `ref`, and `Location` points at it. Here's the request a CI job would send without `writ`, with the dev token:

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

Output under `writ serve --dev`, 2026-11-16. A plan's document now also has `network`, `null` here, the record of its run's [egress](serve.md#egress), which [JSON output](json-output.md) describes.

The body is now `writ.plan-request/v1`: `env`, `stacks` with one stack, `hints`, and `source`, the commit of a repository to fetch, which [Repository routes](#repository-routes) describes. A request with any other field is refused. Here's a plan after it ended, of a stack that gives a sensitive variable to `random_pet`'s `keepers`, trimmed with `jq` to part of the plan and four fields of its document:

```
$ curl -sS "$SERVER/v1/plans/$ID" -H "Authorization: Bearer $TOKEN" | jq '{format, ok, plan: (.plan | {ref, id, status, env, stacks, author: {kind: .author.kind, person: .author.person}, source}), document: (.document | {format_version, terraform_version, variables, resource_changes: [.resource_changes[] | {address, change: {actions: .change.actions, after: .change.after}}]}), diagnostics}'
{
  "format": "writ.remote-plan/v0",
  "ok": true,
  "plan": {
    "ref": "MM4PBQUBYHS6B3KVAWW5WLALOC",
    "id": "a9443d1adb1bc57bfee682d5bf8fcf85",
    "status": "approved",
    "env": "dev",
    "stacks": [
      {
        "stack": "app",
        "changes": 1,
        "source_digest": "7d9984593cfb19f9b1655c9a54d6d5c57f029f4c8a83d5d69c1bb2364848605a"
      }
    ],
    "author": {
      "kind": "dev",
      "person": "dev/root"
    },
    "source": {
      "kind": "commit",
      "repo": "repo",
      "ref": "refs/heads/main",
      "commit": "6728f57a379c57399d0a6ff1978f78df26284936",
      "ref_check": "on-ref"
    }
  },
  "document": {
    "format_version": "1.2",
    "terraform_version": "1.13.9",
    "variables": [
      "canary"
    ],
    "resource_changes": [
      {
        "address": "random_pet.name",
        "change": {
          "actions": [
            "create"
          ],
          "after": {
            "keepers": {
              "canary": "(sensitive)"
            },
            "length": 2,
            "prefix": null,
            "separator": "-"
          }
        }
      }
    ]
  },
  "diagnostics": []
}
```

Output under `writ serve --dev` run as root of a user namespace with only loopback, 2026-11-16.

`superseded_by` is the id of a newer pending plan of the same `stack@env` from the same merge request, and is `""` when there's none. A plan with `superseded_by` set takes no approval and doesn't apply. Here are the plans of `network@prod` after a second plan from the same merge request:

```
$ curl -sS "$SERVER/v1/plans?env=prod&stack=network" -H "Authorization: Bearer $WRIT_ID_TOKEN" | jq -c '.plans[] | {id, superseded_by}'
{"id":"03bd0af91430f472335c13a6cd274a61","superseded_by":""}
{"id":"5f5963813d1e23e5d72d59f6d3570457","superseded_by":"03bd0af91430f472335c13a6cd274a61"}
```

Output of a real run, 2026-11-10.

`document` is the masked plan document: OpenTofu's plan JSON after the [mask](secrets.md), with only the keys the mask keeps, which [Policy input](policy-reference.md#input) describes. `text` is the plan as `writ plan --remote` prints it. The saved plan is never served. `source_digest` is the SHA-256 of the files OpenTofu planned: the stack's files, the files of the local modules it calls, and its lock file. An edit to any of them changes it. A plan that the code or the providers refuse ends as `refused`, with no id, and `document` is `{"format":"writ.plan.json/v2","ok":false,"diagnostics":[...]}` with the reason. A failed OpenTofu step is a diagnostic of class `usage`, so `writ plan --remote` exits 1.

Here's what a CI job gets for the same token a second time, a token for another audience, and a token from a project with no grant:

```
HTTP/1.1 401 Unauthorized
{"format":"writ.remote-plan/v0","ok":false,"diagnostics":[{"severity":"error","class":"auth","message":"this token already made a plan; each CI token makes one"}]}

HTTP/1.1 401 Unauthorized
{"format":"writ.remote-plan/v0","ok":false,"diagnostics":[{"severity":"error","class":"auth","message":"the token failed a check: audience: aud is not writ serve's URL"}]}

HTTP/1.1 403 Forbidden
{"format":"writ.remote-plan/v0","ok":false,"diagnostics":[{"severity":"error","class":"denied","message":"gitlab acme/infra #1849213 (alice) may not plan network@prod: no grant gives a principal in no team plan on network@prod"}]}
```

Output of a real run, 2026-11-10, with the other headers cut.

## Approval routes

| Method and path | Grant | Body | Response |
| --- | --- | --- | --- |
| `POST /v1/plans/{id}/approvals` | `approve` on the plan's `stack@env` | none, `{}`, or `writ.approve-request/v0` | `writ.approve/v0` |
| `POST /v1/approvals` | `approve` on each plan's `stack@env`, a CI token | `writ.approve-request/v0` with `pipeline: true` | `writ.approve/v0` |
| `DELETE /v1/plans/{id}/approvals/mine` | `approve` | none | `writ.approve/v0` |
| `POST /v1/runs` | `apply` with the plan's risk classes, and `override` for an override | `writ.run-request/v1` | 202 and `writ.remote-run/v0` |
| `POST /v1/state/adopt` | `adopt` on the `stack@env`, a person | `writ.state-adopt-request/v0` | `writ.state-adopt/v0`. See [Adopt a state](adopt-state.md). |
| `GET /v1/runs/{id}` | `read` | none | `writ.remote-run/v0`. With `?wait=DURATION`, up to 120 seconds, the request waits for a run that applies to end. |
| `POST /v1/apply-requests/{key}/outcome` | a token. The run of a `started` key, to its principal and to `admin` only. | none | `writ.apply-outcome/v0`: `outcome` `started` with `run`, or `none`. See [When an answer is lost](apply.md#when-an-answer-is-lost). |

The body of `POST /v1/runs` sets a plan, or a `stack@env` to apply after a merge:

```json
{"format": "writ.run-request/v1", "plan": "d3f700d860a97483a7ad509c31fe9e28"}
{"format": "writ.run-request/v1", "env": "prod", "stacks": ["network"], "merged": {}}
```

`POST /v1/runs` takes the request's key in the `Idempotency-Key` header. A refusal returns its route's document, `ok: false`, and one diagnostic, except a policy refusal, which carries a summary and one diagnostic for each policy result. The class and status say what kind of refusal it is, and the message gives the cause. The causes below are examples, not a complete list:

| Class | Status | Means | For example |
|---|---|---|---|
| `auth` | 401 | the request isn't signed in | no token, a token that isn't valid, an expired session, a token used twice |
| `denied` | 403 | the principal can't do this | a grant or an approval is missing; another principal's key; a CI job applying another pipeline's plan, its own override, or a commit, repository, or branch other than its own |
| `policy` | 403 | a policy denies the apply | |
| `state` | 409 | the plan can't be applied in its current state | the plan is a drift check, isn't live, or expired, state moved under it, its approvals changed while the policy was evaluated, another run applies to the `stack@env`, or the key is closed, with `key_closed: true` |
| `saved-plan` | 409 | the plan's sources don't allow this apply | the sources don't match, the saved plan is missing or speculative, its commit can't be checked, or the merged commit's sources were applied already, so there is nothing to apply |
| `usage` | 400, 404, 409, 413 | the request is wrong, and the caller can fix it | a malformed body or key; a plan, repository, or Git host object that doesn't exist or can't be seen; a key that expired or was used for another request; an override that isn't needed; a body past the size limit; an environment the config no longer serves |
| `usage` | 429 | too many requests | past `limits.applies` or `limits.requests_per_second` |
| the failure's class, or `internal` | 409, 500 | the plan's policy input can't be prepared | |
| `provider` | 502 | the Git host failed | the Git host can't be read to check the plan's commit |
| `state-io` | 503 | the control database failed, or didn't write the request's audit row | with `outcome`; see [When an answer is lost](apply.md#when-an-answer-is-lost) |

## Repository routes

| Method and path | Grant | Response |
| --- | --- | --- |
| `POST /v1/repos` | `admin` on every stack in `*` | 201 and `writ.repo/v0`. 409 when the config pins the list, or the name, the project and path, or a stack name is taken. 404 when the Git host has no such project or branch. |
| `GET /v1/repos` | `read` on any `stack@env` | `writ.repos/v0`: each repository, its last commit read and last poll, and `pinned` |
| `GET /v1/repos/{name}` | `read` on any `stack@env` | `writ.repo/v0`: the repository and the stacks found at its head |
| `POST /v1/plans` | `plan` | `writ.plan-request/v1` takes `source`, the commit to plan |
| `POST /v1/dev/ui-link` | the dev token | `writ.ui-link/v0`: a control UI sign-in link. Only `writ serve --dev` has it. |

The body of `POST /v1/repos` is `writ.repo-request/v0`. An unknown field, such as a token, is refused:

```json
{"format": "writ.repo-request/v0", "name": "infra", "forge": "gitlab", "project": "acme/infra", "path": "infra", "prefix": ""}
```

`writ.plan-request/v1` sets the commit to plan in `source`: `repo`, `ref`, and `commit`, and a CI job can send `{}`. A plan's `source` in `writ.remote-plan/v0` is `{"kind": "commit", "repo", "ref", "commit", "ref_check"}`; a plan kept from before writ serve planned only commits may say `{"kind": "speculative"}`. `ref` must be a full ref, such as `refs/heads/main`.

No answer, log line, audit row, or page contains a Git host token. `TestRepoAdd` and `TestForgeTextNeverLeaks` search every answer, the log, the audit rows, and the repository's poll error for the token, and fail when they find it.

## Token checks

`writ serve` accepts one token form, a compact JWS signed with RS256 or ES256, and checks it in this order. The first failure ends the request with an `auth` diagnostic that says which check failed.

1. **Shape.** Three parts of strict base64url, at most 16 KiB. Five parts, an encrypted JWT, are refused.
2. **Header.** `alg` is `RS256` or `ES256`. `none`, every `HS` algorithm, and every other value are refused. `jku`, `jwk`, `x5u`, `x5c`, `crit`, `b64`, `enc`, `zip`, and `epk` are refused, so a token can't name its own key. `kid` is required. A member name that appears twice, in the header or in the claims, refuses the token.
3. **Issuer.** `iss` must equal one configured issuer's `url` exactly. It only picks the key set.
4. **Signature.** The issuer's key with that `kid`. Its type fits `alg`, and its own `alg`, when set, equals it. `github.com/coreos/go-oidc/v3` checks the signature, with `github.com/go-jose/go-jose/v4`, and the issuer and the audience again, with only RS256 and ES256 allowed.
5. **Times.** `exp` and `iat` are required, `nbf` is optional, with 60 seconds of leeway.
6. **Audience.** `aud` must be the config's `url`, or a list with it as the only element. A list that also has another audience is refused, because that other relying party could replay the token here.
7. **Claims.** Every claim the issuer's kind needs, as a non-empty string, and `sub` and `jti`. A GitLab CI token needs `project_id`, `project_path`, `namespace_id`, `pipeline_id`, `pipeline_source`, `job_id`, `ref_path`, `ref_protected` (`"true"` or `"false"`), `sha`, `user_id`, and `user_login`. A GitHub Actions token needs `repository`, `repository_id`, `repository_owner_id`, `run_id`, `ref`, `sha`, `event_name`, `workflow_ref`, `actor`, and `actor_id`.
8. **Replay.** A token creates one plan. The control database keeps each token's `jti` until the token expires, in the transaction that keeps the plan. Reads can repeat.

Writ fetches key sets from the `jwks_uri` of each issuer's discovery document, which must be HTTPS on the issuer's host. It follows no redirect. A set is refreshed every hour, or by its `Cache-Control: max-age` within 5 minutes and an hour. An unknown `kid` causes at most one refresh a minute for each issuer. A failed refresh keeps the last good set for 24 hours. A fetch gets 10 seconds and 1 MiB, a set at most 100 keys, an RSA key 2,048 to 8,192 bits with the exponent 65537, and an EC key the P-256 curve with its point on the curve. go-jose parses each key after these bounds hold.

The libraries handle the protocol, and `internal/oidc` adds Writ's policy around them: checks 1 to 3 run before a library sees the token, the key set cache and its bounds serve go-oidc's signature check, and checks 5 to 8 run on the claims the signature covers. go-oidc's own expiry check is off, since check 5 is stricter: it requires `iat` and allows 60 seconds on each time. Its tests check each case above, key rotation, the refresh schedule with a fake clock, and fixtures in the claim shapes GitLab and GitHub document. `just fuzz-oidc` fuzzes the token check, the key set parser, and the refusals of every flow. The token fuzz target accepts only tokens a test key signed.

### Identity provider refusals

An identity provider controls much of what `writ serve` reads: answer bodies, status lines, error codes and descriptions, the discovery document, the key set, and every member of a token. A provider or a proxy can repeat a credential in any of them, such as the access token, a code, or the client secret. So `internal/oidc` returns no error text. A failure is a refusal of one stage for one reason, with at most numbers Writ computed: an HTTP status, a length, a count, or a key's position in the key set. A message says what kind of value failed, such as "the token's alg is not RS256 or ES256", never what it was. An RFC 6749 error code shows only when Writ knows it, and any other is "an error code writ does not know". `writ serve` renders a refusal only through its message: an architecture test in `just check` fails if a refusal reaches `fmt`, `log/slog`, or an error as a value.

| Stage | When | What a refusal says |
| --- | --- | --- |
| `config` | start | The issuer's `url` isn't HTTPS, or a people issuer lacks its client id or secret |
| `discovery` | start | The status, a redirect, an answer Writ can't read, another issuer than the configured URL, or which endpoint isn't an HTTPS URL |
| `key set` | start, and each refresh | The status, the size, the count of keys, or which key, by position, is malformed or out of bounds |
| `device authorization` | `POST /v1/auth/device` | The status and the error code, or which of `device_code`, `user_code`, `expires_in`, and `verification_uri` is unusable |
| `token endpoint` | a code exchange, and device polling | The status and the error code, no `id_token`, the person refused, or the code ran out |
| `userinfo` | sign-in with `groups = "userinfo"` | The status and the error code, or an answer that isn't a JSON object |
| `authorization endpoint` | `/ui/callback` with `error` | The error code |
| `shape`, `header`, `issuer`, `signature`, `times`, `audience`, `claims`, `nonce` | each ID token and CI token | Which member failed the check, and how |

A refused sign-in or token gets 401 with the message. `writ serve` logs it with the stage or check, and leaves an audit row. The row's `check` is the check's name for a token and the stage's name otherwise. A refusal at start stops `writ serve` with the issuer's name and the message. Here's a CI token for another audience, a `writ login` with an ID token for another client, and an issuer with another issuer in its discovery document:

```
{"format":"writ.remote-plan/v0","ok":false,"diagnostics":[{"severity":"error","class":"auth","message":"the token failed a check: audience: aud is not writ serve's URL"}]}
{"format":"writ.session/v0","ok":false,"diagnostics":[{"severity":"error","class":"auth","message":"the sign-in failed: audience: aud does not name the issuer's client id"}]}
level=WARN msg=auth request=RZ4BUB5QEUPBUFHMPEIRJOB2XL issuer=sso check=audience remote=127.0.0.1 reason="audience: aud does not name the issuer's client id"
issuer gitlab: discovery: the discovery document names another issuer than the configured URL
```

Output of a real run, 2026-11-10, with the log line's time cut.

`TestIdentityProviderTextNeverLeaks` in `internal/ui` puts a random marker into each endpoint's answer and each token field, drives it through the start, `writ login`, the control UI's sign-in, a CI token, and a key set refresh, and looks for it in every answer, log line, and row of the control database. `just fuzz-oidc` also fuzzes every flow against an identity provider that sends one answer the fuzzer chose.

## Every answer waits for its audit row

`writ serve` writes each request's audit row before it sends you the answer. If it can't write the row within 5 seconds, you get 503 with one `state-io` diagnostic instead. So no answer leaves the server without its row in the audit log. The rule covers the API, the [control UI](sign-in.md#the-control-ui), and the OpenTofu state backend under `/v1/tfstate/`. It covers every answer each of them sends: a refusal, a 404 or 405, a redirect, a cookie, and the redirect `writ serve` sends for a path it cleans, such as `//v1/whoami`.

These requests write no row:

- `GET /healthz` and `GET /v1/server`. They need no token, so they answer at once while the control database's writes wait. Only their exact paths are exempt, as the client sent them, so `GET //healthz` and `GET /%68ealthz` write a row.
- A request that the [limit on each client address](#rate-limits) refuses with 429.

A panic in any route, the exempt ones included, returns 500 with the request id.

### What the 503 looks like

The 503 comes in the route's format: the route's document for the API, a page for the control UI, and plain text for the state backend. The format follows the path, so a redirect to the control UI, such as `/ui` to `/ui/` or `//ui/login` to `/ui/login`, fails as a page. The 503 carries none of the answer it replaced: no document, no redirect, and no cookie.

### Rows that commit with the change

A plan, an approval or its withdrawal, an apply, an adoption, a new repository, the start or end of a session, a control UI sign-in or sign-out, and a state write, lock, or unlock each write their row in the transaction that stores the change. So nothing is stored without its row. A `?wait=` poll writes its row when its wait ends, so its answer can come up to 5 seconds after that.

### State reads

`writ serve` keeps each answer in memory until its row commits, except a state that the state backend serves. A state can be up to 64 MiB, so a state read commits its row first, with the state version and status 200, once the run's token is checked. Only then does `writ serve` read the state from the control database, open it, and send it to you. So a read that waits for its row keeps none of the state in memory.

**Limits.** If a state read fails after its row commits, it returns 500 and logs an error with the state version its row records. Its row still says 200. A panic at that point drops the connection instead of returning 500.

## Read the audit log

`GET /v1/audit` answers the rows of the audit log that the token may see, newest first, so you can follow a change from its plan through its approvals and its apply to its state writes without a database client. `writ audit --remote` prints the same rows as a table.

| Parameter | Rows |
| --- | --- |
| `plan` | Of the plan, by its id or its ref, and of its run, so its approvals, its apply, and the run's state reads, writes, and locks come with it |
| `run` | Of the run |
| `env`, and `stack` with it | Of the environment, and of the stack there. A row that lists several stacks matches each of them. `env` must be one `writ serve` plans in, as on `GET /v1/plans`. |
| `person` | Of the person, by the id `writ whoami` prints, such as `gitlab/user/1001` |
| `op` | Of the operation: `read`, `plan`, `apply`, `approve`, `override`, `adopt`, `admin`, `state-read`, `state-write`, `state-lock`, or `state-unlock`. Any other value returns 400. |
| `since`, `until` | At or after `since`, and before `until`, both RFC 3339 times |
| `limit` | At most this many, from 1 to 200, 50 by default |
| `cursor` | After the last row of the page whose `next` this is. A cursor works until `writ serve` restarts, and then returns 400: read the first page again. |

Which rows a token sees follows its grants:

- A row of a `stack@env` needs `read` on every stack the row lists there.
- A row of an environment that lists no stack, such as a list of an environment's plans, needs `read` on some stack there.
- A row of no environment, or of every one, needs `admin` on every stack in every environment, as `POST /v1/repos` does. These are the server's start, sign-ins and sign-outs, `GET /v1/whoami`, refused tokens, the added repositories, and requests refused before they set a `stack@env`. A principal with that grant sees every row.

An approval of a pipeline's plans in several environments leaves one row for each environment, and each row lists only that environment's stacks and grants. So a token that reads only staging sees the staging part of the approval and nothing of prod.

A row a token may not see takes no place in the page. A page reads at most 5,000 rows, so for a token that may see few of them it can hold fewer than `limit` rows while `next` is set, and that `next` is after a row the token didn't see. `next` is encrypted under a key that `writ serve` makes when it starts, so you can't read the time or the row it points to, or make one of your own.

A filter of a `stack@env` your token can't `read`, or of an environment where it can't `read` any stack, returns an empty page with `next` `""`, without reading the log. It can't match a row your token may see, and this way the number of rows it would read stays hidden.

Limits:

- A read of an environment where your token reads some stack, or of no environment, still reads the rows it can't see. So a page with fewer rows than `limit` and `next` set shows that at least that many hidden rows lie between its rows, and `since` and `until` can narrow down when they happened. So a token that can read one stack of prod can learn when there's activity on the other stacks of prod, but not on which stack or what it was, and a read of no environment shows when there's activity anywhere.

A row has no client address, and `method` is `OTHER` for a method outside the standard set and the state backend's `LOCK` and `UNLOCK`, because a client chooses that text. Its `reason` is shown only when its `decision` is `allowed` or `none`: `writ serve` wrote those reasons from what it holds, except the reason of an override or an adoption, which a person wrote for the log. The reason of a refused row is the message the request got, which can quote what the request sent, such as a path, a field, or a name, so `reason` is `""` and `reason_withheld` is `true`. The control database keeps that reason. A `state-write` row's reason is shown without the SHA-256 of the state it stored, because with it a token that reads the stack could test a guess at the whole state, and the control database keeps the hash. `env`, `stacks`, `plan`, and `run` contain only names and ids that `writ serve` checked. A stack name in a request is shown as what its row concerns, so a value that is a valid stack name, as a token pasted for one would be, shows to the principals who may read that stack. `TestAuditReadNeverShowsWhatARequestSent` sends a secret in a token, a method, a path, a query, and a body, and requires that the control database contains it and that no page of `GET /v1/audit` does.

The read leaves its own row, of operation `read`, with the `env`, `plan`, and `run` it asked for, and the `stack` only when your token can `read` it, so no read adds a row of a stack its token can't see. The row has none of the other parameters.

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

Output under `writ serve --dev` run as root of a user namespace with only loopback, 2026-11-19, after `writ runs --remote`, `writ show --remote` of a run that doesn't exist, and `writ runs --remote --stack app`.

## Rate limits

`writ serve` counts each request twice, each time in a token bucket that refills at its rate and has room for one second's worth:

1. **Each client address**, `limits.requests_per_address`, 20 by default. Every request counts, before the route, the token, or the body is read. Past it, `writ serve` returns 429 with `Retry-After: 1` and the route's own document. The request writes no audit row, so a client that sends bad tokens fast can't fill the audit log. `writ serve` counts these requests and logs them at most once in 10 seconds, with the address of the last one, the count since the last line, and the count since the start. An IPv6 address counts by its /64, because one host often has a whole /64.
2. **Each principal**, `limits.requests_per_second`, 10 by default, once the token is checked. A request past it gets 429 and has its audit row.

Each limit keeps at most 10,000 buckets. A bucket idle for a second is full again, so `writ serve` drops those first. When every bucket was used in the last second, it drops one more to make room, and that key starts again with a full bucket.

The client address is the address of the TCP peer. When the peer is in `trusted_proxies`, the client address is the right-most address in `X-Forwarded-For` that isn't a trusted proxy. A client can send its own `X-Forwarded-For`, but the proxy appends to it, so the addresses left of the proxy's entry are never read. A peer outside `trusted_proxies` has its `X-Forwarded-For` ignored. An `http://` config behind a TLS proxy on loopback needs `trusted_proxies = ["127.0.0.1/32"]`: without it every client counts as the proxy's address, and shares one bucket. Audit rows and logs record the client address.

`writ serve --dev` counts 100 requests a second for each client address and 100 for its one user.

Measured on 2026-11-11: one `curl` sent 500 requests to `/v1/whoami` with a bad token to `writ serve --dev`. The build before the address limit answered 500 401s and wrote 500 audit rows, at full speed, at 45 requests a second, and at 200. With the limit, at full speed, it answered 104 401s and 396 429s in 0.05 seconds, wrote 104 audit rows, and logged one line. At 200 requests a second it answered 370 401s and 130 429s in 2.7 seconds and wrote 370 audit rows. At 45 a second, under the dev limit of 100, every request got 401 and an audit row.

## Time limits

Three limits bound how long a plan or an apply holds its place under `limits.plans` or `limits.applies`, and its provider uid.

- **`limits.plan_timeout`**, 30 minutes by default, bounds a plan from its start to its end: the read of its commit, the wait for a provider uid, and OpenTofu's `init` and `plan`. Past it, OpenTofu's process group is killed, the plan ends `failed` with the reason, its place is free, and its uid goes back to the pool once its processes end. A plan runs with `-lock=false` and writes nothing, so a cancelled plan leaves nothing to clean up.
- **`limits.apply_start_timeout`**, 10 minutes by default, bounds what an apply waits for before OpenTofu starts: the saved plan, the credentials of the apply role, and a provider uid. Past it, the run ends `failed` with the reason, and state is as it was.
- **`limits.apply_timeout`**, 4 hours by default, interrupts a running apply. OpenTofu gets SIGINT: it starts no more changes, waits for the ones running, and writes the state they leave. The run ends `failed`, and the next plan has the changes that are left. `writ serve` kills OpenTofu if it hasn't ended 2 hours later.

If a provider uid's processes don't end after its run, the uid is withheld from the pool until `writ serve` restarts, and the log shows it. A restart ends each uid's processes again before the uid serves. When every uid is withheld or in use, a plan or an apply fails at its limit with how many uids are withheld:

```
the plan ran past limits.plan_timeout, 500ms, while it waited for a provider uid: 2 of the 2 provider_uids are withheld because their processes did not end, and the others serve running plans and applies
```

Output of `TestPlanTimeoutWithEveryUIDWithheld`, which sets `plan_timeout = "500ms"`. `TestTofuApplyTimeout` sets `apply_timeout` to 1 second, and requires that the run fails with this line in its text:

```
error: the apply ran past limits.apply_timeout, 1s: OpenTofu was interrupted, started no more changes, and wrote the state it reached; plan again
```

## What the API refuses

| Threat | What `writ serve` does |
| --- | --- |
| A forged token, `alg: none`, or algorithm confusion | Only RS256 and ES256, a key from the issuer's own set, of a type that fits the algorithm, no key in the header |
| Key rotation | An unknown `kid` refreshes the set, once a minute at most |
| A token for another service | The audience must be `writ serve` alone |
| A CI token replayed | One plan for each `jti` |
| An oversized token or header | 16 KiB for a token, 64 KiB for all headers |
| An oversized or compressed body | The token is checked before the body is read. 16 MiB after gzip decompression, counted while reading. |
| A flood | 20 requests a second for each client address, counted before the token is checked, 10 for each person, 4 running plans for each person, 8 in all. A request over the address limit leaves no audit row. See [Rate limits](#rate-limits). |
| A plan that never ends, or provider uids that never come free | A plan fails after 30 minutes, an apply that can't start fails after 10, and an apply starts no change after 4 hours. See [Time limits](#time-limits). |
| A request that sets a state location | The request can't carry a field the route doesn't define, so it can't set state. |
| A provider the stack didn't pin | A stack must commit its `.terraform.lock.hcl`. Before a run, `writ serve` installs into `tofu.plugin_cache` the providers that file pins, and no other, with `tofu init -lockfile=readonly` on a configuration it writes, as its own user, so none of the stack's code runs then. OpenTofu fetches each provider the cache doesn't have from the registry in its source address, with no CLI configuration, and checks it against the lock's hashes. A lock file that Terraform wrote is moved to OpenTofu's registry first, as [Lock files that Terraform wrote](serve.md#lock-files-that-terraform-wrote) explains. The config sets no provider: it refuses the old `provider` and `providers_dir` settings. To limit which providers a stack can pin, write a [policy](policy.md). |
| Code that reads the server's disk | A team's `writ serve` reads no source from its disk: it plans the files of a commit that it fetches from the Git host, each checked against its Git blob id. |
| A provider that reads the state keys or another plan's credentials | Each plan's providers run as their own uid, which can't read `data`, the keys, or the secret files, nor the environment of another plan's providers |
| A provider that asks for the host's identity | A firewall rule drops the provider uids' traffic to `169.254.169.254` and `fd00:ec2::254`, and providers get `AWS_EC2_METADATA_DISABLED=true` |
| Credentials that outlive their run | Providers get a token for the credential endpoint, which dies with the plan |
| An identity provider, a proxy, or a token that carries a credential in any field | `internal/oidc` reports a failure only as a typed refusal: a stage, a check, a reason, and numbers Writ computed. No field of it can hold text that came from the identity provider or the token, so no answer, log line, or audit row can either. See [Identity provider refusals](#identity-provider-refusals). |
| A secret a request sent, read back from the audit log | `GET /v1/audit` shows no client address and no method outside the standard set, and the reason of a row only when `writ serve` allowed the request or decided nothing, so no message that quotes a request reaches another principal. See [Read the audit log](#read-the-audit-log). |
| A secret in a response | The [mask](secrets.md) sweeps the plan document, its text, and a run's events and diagnostics. The saved plan is sealed under the environment's key, and the API never serves it. `provider_env` reads only the files the config lists. |

**Limits.** A provider that a stack pins can still do what its run's role allows, for as long as the run lasts. A config without `provider_uids` runs providers as the user of `writ serve`: don't give such a host a cloud identity.
