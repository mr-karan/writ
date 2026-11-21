# Plan and apply

Plan a commit on `writ serve`, approve that exact plan, and apply it by ID or after a merge. For approval rules and stale-plan checks, see [Approvals](approvals.md) and [How a plan becomes an apply](plan-to-apply.md).

## Before you begin

- A running `writ serve` (or `writ serve --dev`) with your [repository added](add-repository.md).
- A [`writ login`](sign-in.md#people-writ-login) session, a dev server, or a GitLab CI `WRIT_ID_TOKEN`.
- Grants for the action (`plan`, `approve`, `apply`, or `override`) and for the plan's [risk classes](glossary.md). Check with `writ whoami`.

## Plan a commit

1. From a clean Git working tree of a registered repository, run:

   ```sh
   writ plan --remote --env prod
   ```

   It sends the checked-out commit when the repository is registered, the stack sits under `path`, no file under `path` differs from `HEAD`, and, for remote forges, the commit is pushed to a remote branch. Under `writ serve --dev`, `--env` defaults to `dev`.

2. In CI, with no checkout, pass `--stack` and `--commit`:

   ```sh
   writ plan --remote --env prod --stack app --commit "$CI_COMMIT_SHA"
   ```

`writ serve` fetches the commit from the Git host, runs `tofu plan -lock=false`, masks secrets, evaluates policies, and returns the plan ID.

## Approve a plan

- **CLI:** `writ approve --remote ID` with a [`writ login`](sign-in.md#people-writ-login) session. It prints `Approved plan ID for ENV (STACK): N of M approvals (WHO), STATUS.` Writ records your stable person ID (such as `gitlab/user/2002`) with auth `person`.
- **Control UI:** click **Approve** on the plan page.
- **Withdraw** before the apply starts: `writ approve --remote --withdraw ID`.

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

Real run against a local GitLab stand-in, 2026-11-16. A maintainer then runs the manual `writ-approve` job (`writ approve --remote --pipeline`), which approves every live plan in that pipeline atomically. If the plan's author runs it, Writ refuses with exit code 1 and the plan stays `pending`.

## Apply a plan

Applying needs `apply` on the plan's `stack@env` and on every risk class in the plan, from one grant or several. `writ serve` decrypts the saved plan with the env's key (bound to that plan ID, stack, and env) and runs `tofu apply` with `apply_role` and a UID of its own from `provider_uids`.

The CLI streams the run until it finishes. One Ctrl-C detaches without stopping the run on the server. Inspect it later with `writ show --remote RUN`.

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

Output under `writ serve --dev`, 2026-11-16. A CI job can apply by ID only a plan of its own pipeline (same issuer, project, and pipeline, or a deduplicated plan shared with it). This allows pre-merge applies, such as to staging.

### Apply after a merge

```sh
writ apply --remote --merged --env ENV --stack NAME
```

- The commit is the token's `sha` on the default branch in GitLab CI. Outside CI, pass `--repo` and `--commit`. Either way, `writ serve` checks with the Git host that the commit is on the default branch.
- `writ serve` reads the stack's files at the merged commit, finds live commit plans for that `stack@env` with the same source digest, and applies the newest approved match.
- A newer unapproved plan does not shadow an older approved plan with the same sources.
- Matching by source digest, not commit SHA, makes merge commits, squash merges, and fast-forward merges all work.

The output has the same form as an apply by ID.

When no approved plan matches and there is no current `no-changes` plan:

| Case | Result |
| --- | --- |
| A matching plan lacks approvals | `403 denied`, with its plan ID once the caller's `apply` grant is verified. |
| The newest run on `stack@env` applied these exact sources and wrote the current state version on Writ's HTTP backend | A retried job exits 0 with no new run. |
| State moved since, or the stack uses S3 | `state`: plan again. |
| A newer `applying` or `failed` run exists | It blocks matching a retry against the earlier success. |
| No plan matches the commit's sources | `saved-plan` error, below. |

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

Real run against a local GitLab stand-in, 2026-11-20. If another run applied or state moved after that plan was requested, Writ checks whether the newest run applied those sources and wrote the current HTTP state version before it asks for a fresh plan. See [A plan that changes nothing](approvals.md#a-plan-that-changes-nothing).

The control plane enforces the gate, not the CI YAML. If someone removes the approve job from a branch and merges, the default-branch apply job still refuses to run without an approved plan.

### Plan and apply in one command

On `writ serve --dev`, `writ apply --remote --yes` plans the current commit and applies it at once if no approval rule matches. If approvals are required, it prints the plan and exits 1.

With `--json`, stdout holds one document: the plan if nothing ran, otherwise the run. A `no-changes` plan prints and exits 0 without a run. Interrupting while following leaves the run going and exits 1 with a failure document that names the run.

## When an answer is lost

A network drop or the 30-second timeout can cut off an apply request after the server started the run. `writ apply --remote` sends an idempotency key, so it can find out what happened and attach to the run on its own. If it cannot reach the server to confirm, it prints the key and a follow-up command:

```sh
writ show --remote --request KEY
```

That command asks the outcome route whether a run started, and closes the key if none did.

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

Real runs against a local stand-in, 2026-11-16. The connection dropped after the run committed:

```
$ writ apply --remote d3f700d860a97483a7ad509c31fe9e28
The apply request got no answer: POST http://127.0.0.1:41971/v1/runs: EOF
Asking writ serve whether it started a run.
Run LLGTGDJCKQ6LZQJWNBMA2ESMHV of plan d3f700d860a97483a7ad509c31fe9e28 for prod (network), by alice.
Applied 1 change in 5ms.
Run LLGTGDJCKQ6LZQJWNBMA2ESMHV of plan d3f700d860a97483a7ad509c31fe9e28 for prod (network), by alice: applied at 2026-11-11T10:00:05Z.
```

No run started, and then the outcome route failed:

```
error: state-io: no run started: writ serve closed the key 1794800081-K7VCN743YGPEVSILJOOIIB4H6V of this command's requests with no run; run the command again
error: usage: no route POST /v1/apply-requests/1794800081-PM5SZ64M6ISVYOSYKVCKABCWOC/outcome
error: state-io: writ serve did not say whether the apply request with the key 1794800081-PM5SZ64M6ISVYOSYKVCKABCWOC started a run; ask later with: writ show --remote --request 1794800081-PM5SZ64M6ISVYOSYKVCKABCWOC
```

The server rolled back a failed commit, settled the key to `none`, and returned `503` with `key_closed: true`:

```
writ serve closed the key 1794800081-RVCR4ZVQFGWXKR2LWDN5VV75BT with no run. Sending the request again with a new key.
```

### What each 503 means

- A `503` from `POST /v1/runs` sets `outcome: "none"` if no run started, or `outcome: "unknown"` if the server cannot tell. A `503` from the outcome route always sets `outcome: "unknown"`.
- Only `outcome: "none"` from the outcome route, or `key_closed: true` on `POST /v1/runs`, guarantees the key can never start a run.
- A `503` with `outcome: "none"` and no `key_closed` failed before commit, so the same key can be retried.
- If a database commit error leaves the outcome unknown, `writ serve` returns `503` with `outcome: "unknown"` and reconciles the run in the background, or marks an orphaned `applying` run `failed` on its minute sweep (see [What a run records](approvals.md#what-a-run-records)).

### Who can learn a key's outcome

- A `started` key: only the principal that started the run (same issuer and subject) or a global `admin`. Anyone else gets `403 denied`, which is audited.
- An unused key: any authenticated principal can query it and close it to `none`.
- Keys have 130 random bits and cannot be guessed, but anyone who reads a key from a log before the apply runs can close it.
- In GitLab CI, a job's subject names the project and branch, so jobs on the same branch share a principal here.

### How long a key lives

- A key is accepted for 24 hours after its embedded Unix timestamp, and at most 5 minutes ahead of server time.
- `started` rows live as long as their run.
- Hourly pruning deletes `none` rows older than 25 hours and, in the same transaction, records a monotonic prune floor. A key at or below the floor is refused as `state` with `key_closed: true`, so a clock rollback cannot reopen pruned keys.

## Override the approval rules

In an emergency, such as rotating a leaked key while approvers are away, a person with `override` can apply without approvals by giving a reason of 10 to 500 characters:

```sh
writ apply --remote ID --override "bob on leave, rotate leaked key, OPS-112"
```

- `--override` skips approval rules only. You still need `apply` and every risk class of the plan, and mandatory Rego policy denies still block the apply.
- CI jobs cannot use `--override`. If the plan already has its approvals, Writ refuses `--override` as `usage`.
- The reason is recorded on the run, the plan history, the audit log, the server log (`WARN`), and the stack state.

## Limits

| Limit | Value |
| --- | --- |
| `limits.applies` | Default `4` concurrent applies. Excess requests get `429`. Applies share `provider_uids` with plans and wait for a free UID. |
| Settling a failed state commit | Holds an apply slot for at most 2 minutes (5-second retries). |
| `limits.apply_start_timeout` | Default `10m`: the longest wait for a free UID. |
| `limits.apply_timeout` | Default `4h`: then OpenTofu gets `SIGINT`, so running changes finish and no new ones start ([Time limits](api.md#time-limits)). |
| Request keys | Live 24 hours, at most 5 minutes ahead of server time. Closed `none` keys are pruned after 25 hours. |
| `writ apply --remote` | 30-second timeout per request, up to 5 apply attempts and 5 outcome queries (backoff 2s, 4s, 8s, 16s). Follows a run for up to 30 minutes from attach. |
| Pipeline approval | At most 50 plans. |
| Override and adoption reasons | 10 to 500 characters. |

## Troubleshooting

| Error | Fix |
| --- | --- |
| `403 denied: PRINCIPAL made plan ID and cannot approve it` | Under an approval rule, authors cannot approve their own plans. Ask a teammate with `approve`. |
| `409 state: plan ID is stale` or `expired` | A newer plan exists, state moved, or `plan_ttl` elapsed. Plan and approve again. |
| `409 saved-plan: no approved plan matches` | The stack files of the merged commit match no live approved plan. Plan the latest commit and approve it. |
| `403 policy: policy refuses the apply` | A mandatory Rego policy denied it. `--override` does not bypass policy. Fix the code and plan again. |
| Connection dropped mid-request | Run the printed `writ show --remote --request KEY` before you retry. |

## Next steps

- [Approvals](approvals.md): approval rules, risk classes, and stale-plan checks.
- [Read the audit log](audit-log.md): trace plans, approvals, applies, and state writes.
- [Errors](errors.md): error classes and recovery steps.
