# Plan and apply

Plan a commit on `writ serve`, approve that exact plan, and apply it by ID or after a merge. This page also covers lost-response recovery and emergency overrides.

For approval rules and stale-plan checks, see [Approvals](approvals.md) and [How a plan becomes an apply](plan-to-apply.md).

## Before you begin

- A running `writ serve` (or `writ serve --dev`) with your [repository added](add-repository.md).
- A [`writ login`](sign-in.md#people-writ-login) session, a dev server, or a GitLab CI `WRIT_ID_TOKEN`.
- Grants covering the action (`plan`, `approve`, `apply`, or `override` plus the plan's [risk classes](glossary.md)). Run `writ whoami` to check.

## Plan a commit

From a clean Git working tree of a registered repository:

```sh
writ plan --remote --env prod
```

Under `writ serve --dev`, `--env` defaults to `dev`. In CI (with no checkout), pass `--stack` and `--commit`:

```sh
writ plan --remote --env prod --stack app --commit "$CI_COMMIT_SHA"
```

`writ serve` fetches the commit from the Git host, runs `tofu plan -lock=false`, masks secrets, evaluates policies, and returns the plan ID.

## Approve a plan

Approve from the CLI with a [`writ login`](sign-in.md#people-writ-login) session, or click **Approve** on the plan page in the control UI.

`writ approve --remote ID` prints `Approved plan ID for ENV (STACK): N of M approvals (WHO), STATUS.` Writ records your stable person ID (such as `gitlab/user/2002`) with auth `person`. Withdraw an approval before apply starts with `writ approve --remote --withdraw ID`.

In GitLab CI, a merge request job plans each stack and prints the masked plan:

```
OpenTofu 1.13.9 plan of app@prod

  + random_pet.name (create)
  + terraform_data.note (create)
  + terraform_data.secret (create)
  + module.cert.tls_private_key.key (create)

Plan: 4 to add, 0 to change, 0 to destroy.
Outputs that change: pet.
```

Output of `TestTofuApproveApply`, 2026-11-16, against a team's `writ serve` with a test issuer and a GitLab stand-in.

A maintainer then runs the manual `writ-approve` job (`writ approve --remote --pipeline`), which approves every live plan in that pipeline atomically. If the plan's author runs it, Writ refuses with exit code 1 and the plan stays `pending`.

## Apply a plan

Applying requires `apply` on the plan's `stack@env` and every risk class in the plan (from one grant or several). `writ serve` decrypts the saved plan using the environment's key (bound to that plan ID, stack, and environment) and runs `tofu apply` with `apply_role` and its own UID from `provider_uids`.

The CLI streams the run until it finishes. Pressing Ctrl-C once detaches without stopping the server-side run; inspect it later with `writ show --remote RUN`.

### Apply by id

```
$ writ apply --remote 931d8c004160c9e54cd554d70d1df149
Run VL7RNRTHG3SYT7GRXC5O23VWV6 of plan 931d8c004160c9e54cd554d70d1df149 for dev (app), by alice (dev).
OpenTofu apply of app@dev
  random_pet.name: create complete
Apply: 1 added, 0 changed, 0 destroyed.
State version 1 of app@dev.
Run VL7RNRTHG3SYT7GRXC5O23VWV6 of plan 931d8c004160c9e54cd554d70d1df149 for dev (app), by alice (dev): applied at 2026-11-16T03:29:49Z.
```

Output under `writ serve --dev`, 2026-11-16.

A CI job can apply by ID only a plan belonging to its own pipeline (same issuer, project, and pipeline, or a deduplicated plan shared with it), which supports pre-merge applies such as staging.

### Apply after a merge

Run `writ apply --remote --merged --env ENV --stack NAME` to apply the approved plan whose stack sources match the merged commit.

In GitLab CI, the commit comes from the token's `sha` on the default branch. Outside CI, pass `--repo` and `--commit`. Either way, `writ serve` verifies with the Git host that the commit is on the default branch.

`writ serve` reads the stack's files at the merged commit, looks up live commit plans for that `stack@env` with the same source digest, and applies the newest approved match. Newer unapproved plans don't shadow an older approved plan with the same sources. Comparing source digests rather than commit SHAs makes merge commits, squash merges, and fast-forward merges all work.

```
$ writ apply --remote --merged --stack app --repo demo --commit d9a4b37495d9d1c581d07a7895d9ef3a3401f4c2
Run T5QKSTXGRYE66SZBECBPGUV7QV of plan f38f9206b30b2f7ffbcaca647a9b8190 for dev (app), by alice (dev).
OpenTofu apply of app@dev
  random_pet.name: create complete
Apply: 1 added, 0 changed, 0 destroyed.
State version 1 of app@dev.
Run T5QKSTXGRYE66SZBECBPGUV7QV of plan f38f9206b30b2f7ffbcaca647a9b8190 for dev (app), by alice (dev): applied at 2026-11-16T03:34:58Z.
```

Output of a real run, 2026-11-16.

When no approved plan matches and there is no current `no-changes` plan:

- A matching plan missing approvals returns `403 denied` (including its plan ID once the caller's `apply` grant is verified).
- Retrying an apply job succeeds with exit 0 and no new run if the newest run on `stack@env` applied those exact sources and wrote the current state version on Writ's HTTP backend. If state moved or uses S3, Writ returns `state` and asks for a new plan.
- A newer `applying` or `failed` run blocks retry matching against an earlier success.

If no plan matches the commit's sources:

```
$ writ apply --remote --merged --stack app --repo demo --commit de72518e5a53e44a1fd8ed6389cb663a844dd929
error: saved-plan: no approved plan of app@dev matches the sources of demo at de72518e: plan the merge request again after a rebase, and approve the new plan
```

If the newest plan for the merged sources is `no-changes` and still current, the job exits 0 with nothing to apply:

```
$ writ show --remote abc0596aff0857c996028b716be98aee
OpenTofu 1.13.9 plan of app@prod

No changes.
Plan abc0596aff0857c996028b716be98aee for prod (app), by gitlab acme/infra #1849404 (alice), changes no resource: there is nothing to apply.
$ writ approve --remote abc0596aff0857c996028b716be98aee
error: state: plan abc0596aff0857c996028b716be98aee changes no resource; there is nothing to approve or apply
$ writ apply --remote --merged --env prod --stack app
Plan abc0596aff0857c996028b716be98aee of app@prod has the sources of infra at 2357a3f0 and changes no resource: there is nothing to apply.
```

Output of `TestTofuApproveApply` in `e2e/`, 2026-11-20. If another run applied or state moved after that plan was requested, Writ checks whether the newest run applied those sources and wrote the current HTTP state version before asking for a fresh plan. See [A plan that changes nothing](approvals.md#a-plan-that-changes-nothing).

Even if someone removes the approve job from a branch and merges, the default-branch apply job refuses to run without an approved plan. The control plane enforces the gate, not the CI YAML.

### Plan and apply in one command

On `writ serve --dev`, `writ apply --remote --yes` plans the current commit and applies it immediately if no approval rule matches. If approvals are required, it prints the plan and exits 1.

With `--json`, stdout prints one document: the plan if nothing ran, otherwise the run. A `no-changes` plan prints and exits 0 without starting a run. Interrupting while following leaves the server run going and exits 1 with a failure document naming the run.

## When an answer is lost

If a network drop or 30-second timeout interrupts an apply request, the server may still have started the run. `writ apply --remote` sends an idempotency key so it can check what happened and attach to the run automatically.

If the CLI can't reach the server to confirm, it prints the key and a follow-up command:

```sh
writ show --remote --request KEY
```

That command queries the outcome route, reports whether a run started, and closes the key if no run started.

### How the key works

Each `writ apply --remote` invocation generates one `Idempotency-Key`: Unix seconds, a dash, and 26 random characters from `crypto/rand` (for example `1794405508-IDTB4TY5M2Z4M4RFR773KN4BKQ`). If a client omits the header, `writ serve` generates one.

The key is written as the first write inside the transaction that starts the run, and its row is final:

- `started`: a run started. Repeating the same request as the same principal returns `202` with that run without starting a second run or spending a second CI token. Reusing the key for a different request returns `usage`; another principal gets `denied`.
- `none`: closed with no run (by the outcome route or after a rolled-back commit). Any later apply with that key returns `state` with `key_closed: true`.
- No row: no run started and no refusal committed the key, so the client may reuse it.

### Ask whether a run started

`POST /v1/apply-requests/{key}/outcome` returns `started` (with the run) or `none`. If the key has no row, the route writes `none` and answers after committing.

On Postgres and SQLite, that write serializes against any in-flight start transaction for the same key, so `none` can never race ahead of a committing run. Asking closes an unused key, so asking also cancels any delayed retry carrying that key.

### What writ apply does for you

After a transport error, timeout, or `503` whose `outcome` isn't `none`, `writ apply --remote` queries the outcome route up to 5 times:

- `started`: prints and follows the run.
- `none`: prints that no run started and exits 1; rerun the command to use a new key.
- Unreachable: prints the key and `writ show --remote --request KEY`, then exits 1.

After a `429`, or a `503` with `outcome: "none"` and `key_closed: false`, the CLI retries with the same key. After a `503` or `409` with `key_closed: true`, it retries with a new key. All retries share the 5-attempt cap; a `503` with `outcome: "unknown"` never rotates the key.

Examples from `TestRemoteApplyOutcome` (against a `writ serve` stand-in, 2026-11-16). First, connection dropped after the run committed:

```
$ writ apply --remote d3f700d860a97483a7ad509c31fe9e28
The apply request got no answer: POST http://127.0.0.1:41971/v1/runs: EOF
Asking writ serve whether it started a run.
Run LLGTGDJCKQ6LZQJWNBMA2ESMHV of plan d3f700d860a97483a7ad509c31fe9e28 for prod (network), by alice.
Applied 1 change in 5ms.
Run LLGTGDJCKQ6LZQJWNBMA2ESMHV of plan d3f700d860a97483a7ad509c31fe9e28 for prod (network), by alice: applied at 2026-11-11T10:00:05Z.
```

Next, when no run started, and when the outcome route fails:

```
$ writ apply --remote d3f700d860a97483a7ad509c31fe9e28
The apply request got no answer: POST http://127.0.0.1:42773/v1/runs: EOF
Asking writ serve whether it started a run.
error: state-io: no run started: writ serve closed the key 1794800081-K7VCN743YGPEVSILJOOIIB4H6V of this command's requests with no run; run the command again
$ writ apply --remote d3f700d860a97483a7ad509c31fe9e28
The apply request got no answer: POST http://127.0.0.1:36599/v1/runs: EOF
Asking writ serve whether it started a run.
error: usage: no route POST /v1/apply-requests/1794800081-PM5SZ64M6ISVYOSYKVCKABCWOC/outcome
error: state-io: writ serve did not say whether the apply request with the key 1794800081-PM5SZ64M6ISVYOSYKVCKABCWOC started a run; ask later with: writ show --remote --request 1794800081-PM5SZ64M6ISVYOSYKVCKABCWOC
```

When the server rolls back a failed commit, settles the key to `none`, and returns `503` with `key_closed: true`, the CLI retries with a new key:

```
$ writ apply --remote d3f700d860a97483a7ad509c31fe9e28
writ serve closed the key 1794800081-RVCR4ZVQFGWXKR2LWDN5VV75BT with no run. Sending the request again with a new key.
Run LLGTGDJCKQ6LZQJWNBMA2ESMHV of plan d3f700d860a97483a7ad509c31fe9e28 for prod (network), by alice.
Applied 1 change in 5ms.
Run LLGTGDJCKQ6LZQJWNBMA2ESMHV of plan d3f700d860a97483a7ad509c31fe9e28 for prod (network), by alice: applied at 2026-11-11T10:00:05Z.
```

### What each 503 means

A `503` from `POST /v1/runs` sets `outcome: "none"` if no run started, or `outcome: "unknown"` if the server can't tell. A `503` from the outcome route always sets `outcome: "unknown"`.

Only `outcome: "none"` from the outcome route or `key_closed: true` on `POST /v1/runs` guarantees the key can never start a run. A `503` with `outcome: "none"` and no `key_closed` failed before commit, so the same key can still be retried.

### When the commit returns an error

If committing the run's start returns an error, `writ serve` checks the database for the key's outcome (5-second timeout):

- `started`: continues the run and answers `202`.
- `none`: answers `503` with `outcome: "none"` and `key_closed: true`.
- Still failing: answers `503` with `outcome: "unknown"`, holds the apply slot, and retries every 5 seconds for up to 2 minutes. If `started` appears, it launches the run; if `none`, timeout, or server stop occurs, it releases the slot. Any orphaned `applying` row is later marked `failed` by the minute sweep ([What a run records](approvals.md#what-a-run-records)).

### Who can learn a key's outcome

For a `started` key, only the principal that started the run (same issuer and subject) or a global `admin` can read the outcome; anyone else gets `403 denied` (audited). For an unused key, any authenticated principal can query it and close it to `none`.

Keys have 130 random bits and can't be guessed, though anyone who reads a key from a log before the apply runs can close it. In GitLab CI, a job's subject identifies project and branch, so jobs on the same branch share a principal here.

### How long a key lives

A key is accepted for 24 hours after its embedded Unix timestamp and at most 5 minutes ahead of server time. `started` rows live as long as their run. Hourly pruning deletes `none` rows older than 25 hours and records a monotonic prune-floor timestamp in the same transaction; any key at or below that floor is refused as `state` with `key_closed: true`, preventing clock rollback from reopening pruned keys.

## Override the approval rules

For emergencies (such as rotating a leaked key when approvers are unavailable), a person with `override` can apply without waiting for approvals by supplying a reason (10 to 500 characters):

```sh
writ apply --remote ID --override "bob on leave, rotate leaked key, OPS-112"
```

- `--override` skips approval rules only. You still need `apply` and all plan risk classes, and mandatory Rego policy denies still block the apply.
- CI jobs can't use `--override`. If a plan already has its approvals, Writ refuses `--override` as `usage`.
- The reason is recorded on the run, plan history, audit log, server log (`WARN`), and stack state.

## Limits

- `limits.applies` (default `4`): concurrent applies; excess requests get `429`. Applies share `provider_uids` with plans and wait for a free UID.
- `limits.apply_start_timeout` (default `10m`): max wait for a free UID. `limits.apply_timeout` (default `4h`): sends `SIGINT` to OpenTofu so running changes finish without starting new ones ([Time limits](api.md#time-limits)).
- Failed-commit settlement holds an apply slot for at most 2 minutes (5-second retries).
- Request keys live 24 hours (max 5 minutes future skew); `none` rows prune after 25 hours.
- `writ apply --remote` uses a 30-second per-request timeout, up to 5 apply attempts and 5 outcome queries (backoff 2s, 4s, 8s, 16s), and follows a run for up to 30 minutes from attach time.
- Pipeline approval covers at most 50 plans.
- Override and adoption reasons must be 10 to 500 characters.

## Troubleshooting

- **`403 denied: PRINCIPAL made plan ID and cannot approve it`**: Authors can't approve their own plans under an approval rule. Ask a teammate with `approve`.
- **`409 state: plan ID is stale` or `expired`**: A newer plan exists, state moved, or `plan_ttl` elapsed. Plan and approve again.
- **`409 saved-plan: no approved plan matches`**: Stack files in the merged commit don't match any live approved plan. Plan the latest commit and approve it.
- **`403 policy: policy refuses the apply`**: A mandatory Rego policy denied the apply. `--override` doesn't bypass policy; fix the code and plan again.
- **Connection dropped mid-request**: Run the printed `writ show --remote --request KEY` command before retrying.

## Next steps

- [Approvals](approvals.md): approval rules, risk classes, and stale-plan checks.
- [Read the audit log](audit-log.md): trace plans, approvals, applies, and state writes.
- [Errors](errors.md): error classes and recovery steps.
