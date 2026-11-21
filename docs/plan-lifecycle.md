# Plan lifecycle reference

Every stage a plan passes through, from commit fetch to state index: statuses, staleness checks, risk classes, sealing, and what each run records. For the commands, see [Plan and apply](apply.md) and [Approvals](approvals.md). For the overview, see [How a plan becomes an apply](plan-to-apply.md).

## Planning

### 1. Commit fetch and verification

Planning starts from `writ plan --remote` in a clean Git working tree, or from a CI job that sends `$CI_COMMIT_SHA`. Clients never upload stack files to a team's `writ serve`.

1. Verify the caller's OIDC token or session, and its `plan` grant on `stack@env`.
2. Confirm with the Git host that the commit is on the claimed ref.
3. Fetch the stack and local module (`./`, `../`) files not already cached, and verify each file's Git SHA-1 blob ID.
4. Enforce the [stack structure and module-source rules](add-repository.md#what-a-stack-can-contain) before OpenTofu runs.

### 2. Isolated planning and masking

1. Install lock-file providers into `tofu.plugin_cache` from a synthetic config. Stack code never gets write access to the cache.
2. Take a free `provider_uids` UID and start Linux user, mount, PID, and IPC namespaces with a read-only filesystem view, the seccomp filter, and per-step cgroup limits ([Run isolation](run-isolation.md)).
3. Run `tofu init -lockfile=readonly` with no cloud credentials.
4. Assume `plan_role` through STS and run `tofu plan -lock=false`, with a read-only state token on the HTTP backend or a read-only role on S3. Copy the saved plan at once into a sealed in-memory file and run `tofu show -json` on that copy, so the plan document, policy input, plan ID, and sealed plan share the same bytes.
5. Pass the plan JSON through Writ's mask: sensitive marks, learned values and their encodings, secret-named keys, `provider_env`, the assumed STS credentials, and the prior state's sealed [secret set](glossary.md).

### 3. Policy evaluation (plan point) and sealing

If `policy` is configured, Writ evaluates every Rego set in a bounded child process with `input.writ.decision == "plan"`.

- A mandatory deny sets `refused` and discards the saved plan.
- A plan that changes nothing becomes [`no-changes`](#a-plan-that-changes-nothing): no saved plan, nothing to approve or apply.
- Otherwise Writ computes the [plan ID](#what-the-plan-id-covers) and seals the saved plan, bound to the plan ID, stack, and env.

## Plan statuses

Every transition is recorded in `history` on `GET /v1/plans/{id}`.

| Status | Meaning |
| --- | --- |
| `planning` | `writ serve` is running `tofu plan`. |
| `pending` | Saved, waiting for approvals. |
| `approved` | Has all required approvals, or needs none. |
| `applying` | A run is applying the plan. |
| `applied` | The run succeeded. |
| `no-changes` | Nothing to change, approve, or apply. |
| `refused` | Failed validation or policy. Not saved. |
| `failed` | Plan execution or apply failed. |
| `expired` | `plan_ttl` elapsed before apply. |
| `superseded` | State, a newer plan, or the source branch moved (`superseded_why` explains). |

`applied`, `no-changes`, `refused`, `failed`, `expired`, and `superseded` are terminal.

For live plans, Writ computes the effective status (`pending` or `approved`) against the current config whenever plans are read, listed, or applied. Config changes take effect at once without rewriting rows. The stored status updates at the next approval, withdrawal, or run.

| From | To | When |
| --- | --- | --- |
| `planning` | `pending`, `approved`, `no-changes`, `refused`, `failed` | The plan finishes. |
| `planning` | `superseded` | Another apply ran on the same `stack@env` during planning. |
| `pending` | `approved` | Required approvals met, or a config change restores the count. |
| `approved` | `pending` | An approval is withdrawn, or a config change makes the count short. |
| `pending`, `approved` | `applying` | A run starts (from `pending` only with `--override`). |
| `pending`, `approved` | `expired` | `plan_ttl` elapsed. |
| `pending`, `approved` | `superseded` | Another run ended, or the apply found the plan stale. |
| `applying` | `applied`, `failed` | The run ends. |

### A plan that changes nothing

A plan is `no-changes` only when every resource change is a no-op with no move or import, no output changes, and no drift from outside OpenTofu. A plan that changes an output, moves a resource, reads an apply-time data source, or reconciles drift still writes state and stays `pending` or `approved`.

A `no-changes` plan:

- never expires and stores no saved plan;
- refuses a direct `approve` or `apply` by ID with `state`;
- is skipped by `writ approve --remote --pipeline`, which approves the pipeline's other plans;
- makes `writ apply --remote --merged` exit 0 with nothing to apply if no run or state write came after it. If one did, Writ checks whether the newest run applied those sources and wrote the current HTTP state version, and otherwise asks for a fresh plan.

## Approvals from CI jobs

- A CI job token approves only plans of its own pipeline: same issuer, project, and pipeline ID. Project and pipeline IDs are per GitLab instance, so a job on one trusted instance cannot approve a plan from another.
- If two pipelines produce byte-identical saved plans for the same commit and state, Writ deduplicates them and links the plan to both pipelines.
- A plan made from a personal session belongs to no pipeline, and no CI job can approve it.
- `writ approve --remote --pipeline` approves every live plan of the pipeline atomically. If any plan fails validation, none is approved. Each CI token can approve once.

### After you rename an identity

Renaming an issuer's `identity` changes every person ID under it. Sessions and plan authors with an undefined identity are refused until they sign in or plan again. Approvals recorded under the old identity stop counting, and the plan returns to `pending`, unless someone approves under the new identity or you roll back the rename.

## Why an approved plan can still be refused

Before an apply starts, Writ checks that the plan still matches current code and state:

| Check | Class | Result |
| --- | --- | --- |
| Expired | `state` | `plan_ttl` elapsed. The plan becomes `expired`. |
| State changed | `state` | A run was applying on `stack@env` at or after the plan's request time. The plan becomes `superseded`. Finishing any run also supersedes every other live plan of that `stack@env`. |
| Branch moved | `saved-plan` | For a branch plan applied by ID, Writ checks the branch head on the Git host. If the stack's files differ or the branch was deleted, the plan becomes `superseded`. Non-branch refs apply only with `--merged`. |
| A newer plan exists | `state` | Only the newest plan of a `stack@env` can apply, ordered by a per-stack sequence number. A plan superseded by a newer one in the same merge request cannot be approved either. |
| State version moved | `state` | On Writ's HTTP backend, the newest state version must match what the plan read. On S3, `tofu apply` itself refuses if the serial or lineage moved. |
| OpenTofu's own check | run fails | If state changed after planning, OpenTofu refuses the saved plan: `OpenTofu refused plan ID: the state of STACK@ENV changed after the plan was made; plan again`. |

Example refusals:

```
error: saved-plan: plan b9a409594e580d10863ee1bd40f99e14 is superseded: refs/heads/main moved to 552091ed, whose sources of app differ from the plan's, made at b1b6861d; plan the new commit
error: state: plan 767f1426f30110318fdd547324fe200b is stale: plan d695de3fbecf1d88f2f865938cd7ca00 of app@prod is newer; plan again
```

Real runs, 2026-11-16 (under `writ serve --dev`, and against a local GitLab stand-in).

Starting a run is atomic: Writ checks token reuse, status, expiry, approvals, and active runs before it sets `applying`. A refusal never spends the caller's CI token. At most one run executes per plan, and at most one `applying` run per `stack@env`.

## How a merged apply picks a plan

For `writ apply --remote --merged --env ENV --stack NAME`:

- The commit is the token's `sha` on the default branch in GitLab CI. Outside CI, pass `--repo` and `--commit`. Either way, `writ serve` checks with the Git host that the commit is on the default branch.
- `writ serve` reads the stack's files at the merged commit, finds live commit plans for that `stack@env` with the same source digest, and applies the newest approved match.
- A newer unapproved plan does not shadow an older approved plan with the same sources.
- Matching by source digest, not commit SHA, makes merge commits, squash merges, and fast-forward merges all work.

When no approved plan matches and there is no current `no-changes` plan:

| Case | Result |
| --- | --- |
| A matching plan lacks approvals | `403 denied`, with its plan ID once the caller's `apply` grant is verified. |
| The newest run on `stack@env` applied these exact sources and wrote the current state version on Writ's HTTP backend | A retried job exits 0 with no new run. |
| State moved since, or the stack uses S3 | `state`: plan again. |
| A newer `applying` or `failed` run exists | It blocks matching a retry against the earlier success. |
| No plan matches the commit's sources | `saved-plan`: `no approved plan of app@dev matches the sources of demo at de72518e: plan the merge request again after a rebase, and approve the new plan` |
| The newest plan for the merged sources is `no-changes` and current | Exit 0, nothing to apply: `Plan ID of app@prod has the sources of infra at 2357a3f0 and changes no resource: there is nothing to apply.` A direct `writ approve` of it fails with `state: plan ID changes no resource; there is nothing to approve or apply`. |

If another run applied or state moved after that `no-changes` plan was requested, Writ checks whether the newest run applied those sources and wrote the current HTTP state version before it asks for a fresh plan ([A plan that changes nothing](#a-plan-that-changes-nothing)).

The control plane enforces the gate, not the CI YAML. If someone removes the approve job from a branch and merges, the default-branch apply job still refuses to run without an approved plan.

## Apply-point checks and execution

When applying by ID or with `--merged`:

1. **Source match:** `--merged` reads the stack's files at the merged default-branch commit and picks the newest live approved plan with that source digest. This works for merge commits, squash merges, and fast-forwards.
2. **Staleness checks:** refuse expired plans, plans superseded by a newer plan, plans whose state moved, and branch plans whose branch moved ([details](#why-an-approved-plan-can-still-be-refused)).
3. **Apply-point policy:** re-evaluate every Rego set with `input.writ.decision == "apply"`, the caller, and the current approvals, binding an approvals digest so approvals cannot change before the start. A deny returns `403 policy`, leaves the plan approved, and no override bypasses it ([Write and test policies](write-a-policy.md)).
4. **Atomic start:** in one transaction, record the CI token `jti` (one apply per token) and the `Idempotency-Key`, enforce one active apply per `stack@env`, set the plan to `applying`, and insert the run.
5. **Execution:**
   1. Fetch the commit's files from the Git host, verify them against the plan's source digest, and write them to a fresh scratch directory.
   2. Decrypt the sealed saved plan into a sealed in-memory file.
   3. Run `tofu init -input=false -lockfile=readonly` and `tofu show -json PLAN` without cloud credentials inside the [run sandbox](run-isolation.md).
   4. Load the mask with the state's [secret set](secrets.md) plus secrets learned from `tofu show -json PLAN`, so unmarked values (such as `nonsensitive()` `for_each` keys or rotated secrets) are still scrubbed.
   5. Run `tofu apply -input=false -json PLAN` with `provider_env` and `apply_role`, as an isolated UID. On the HTTP backend it gets a single-run token scoped to `LOCK`, `POST`, and `UNLOCK` for that `stack@env`, revoked when the run ends. On S3, Writ runs a second offline `init` with `apply_role`, lets `tofu apply` manage the bucket lock file, and runs `tofu state pull` to record the new serial ([AWS roles and credentials](aws-credentials.md#2-grant-s3-state-permissions-if-using-backend-s3)).

```
OpenTofu apply of app@prod
  terraform_data.note: create complete
  terraform_data.secret: create complete
  module.cert.tls_private_key.key: create complete
  random_pet.name: create complete
Apply: 4 added, 0 changed, 0 destroyed.
State version 1 of app@prod.
```

Real run against a local GitLab stand-in, 2026-11-16.

Past `limits.apply_timeout`, Writ sends OpenTofu `SIGINT`, so running operations finish and write state and no new ones start, then fails the run. If OpenTofu has not exited 2 hours later, Writ kills it.

## After an apply

- **State and secret set:** seal the resulting state (HTTP backend) and secret set (HTTP and S3) under the env key.
- **State index:** run offline `tofu show -json` with no cloud credentials to update the [state index](#the-state-index) through the mask. A refresh error does not fail the apply; the background refresh retries within 5 minutes.
- **Audit:** every state backend read, write (`state-write` with version, serial, lineage, and SHA-256), lock, and unlock, plus `apply-finished`.
- **Saved plan cleanup:** delete the sealed saved plan as soon as the plan leaves live status.
- **Secret set required:** a plan against a state version without a sealed secret set fails, so it cannot be approved. Run `writ state adopt` with an `adopt` grant and a reason of 10 to 500 characters to make one ([Adopt a state](adopt-state.md)).
- **Crash recovery:** see [When writ serve stops during an apply](#when-writ-serve-stops-during-an-apply).

## The state index

Writ keeps a resource index per `stack@env`, served at `GET /v1/index` and shown in the `/ui/stack` Resources tab.

- It stores only `address`, `type`, `provider`, and the allow-listed columns `id`, `arn`, `identifier`, `region`, plus `namespace` and `modify_index` for `nomad_job`. Sensitive or masked values are stored empty.
- It refreshes after each successful apply (offline `tofu show -json`, with no cloud credentials or `provider_env`) and every 5 minutes in the background, up to 50 lagging stacks per sweep.
- A state without a sealed secret set is not indexed until it is adopted with [`writ state adopt`](adopt-state.md) or overwritten by a Writ apply.

## What a run records

- **Database:** plan ID, actor (`ci-job`, `person`, or `dev`), counting approvals, override reason, apply mode (by ID or merged), role session, structured document, and masked output. Read it with `writ show --remote RUN` or `GET /v1/runs/{id}`.
- **Listing:** `writ runs --remote` lists runs, newest first:

  ```
  $ writ runs --remote --stack app
  RUN                         STARTED               STACK  STATUS   BY           PLAN
  TBQ5JOKPJRPUKH36ZGJQ6FYV3O  2026-11-16T03:29:54Z  app    applied  alice (dev)  f4d5e74e89b204ae4f1d53b2aef1441d
  ```

- **Audit and logs:** synchronous audit rows for approvals, run start, and `apply-finished`. JSON logs for `approved`, `apply started`, `apply ended`, and overrides (`WARN`).

### When writ serve stops during an apply

OpenTofu's state keeps whatever finished before the exit, and the state lock stays held until recovery. State locks never expire on their own.

- Each `writ serve` process stores a unique process ID on its runs. At startup and every minute, it marks any `applying` run of another process (or with no active local work) `failed`, marks its plan `failed`, supersedes the other live plans of that `stack@env`, and revokes its state tokens and releases its HTTP state locks in one transaction.
- When a run or plan ends, Writ revokes its state tokens and deletes its locks in one transaction, and again inside the transaction that records the run's end.
- The sweep also cleans tokens and locks of any run no longer planning or applying, and writes a `state-unlock` audit row for each lock it releases.

Run only one `writ serve` process per database ([Supported platforms and limits](status.md)).

## How a change gets its risk class

| OpenTofu actions | Shown as | Risk class |
| --- | --- | --- |
| `create` | create | `create` |
| `update` | update | `update` |
| `delete` | delete | `delete` |
| `delete, create` or `create, delete` | replace | `replace` |
| `forget` | forget | `delete` |
| `forget, create` | replace | `replace` |
| `no-op` or `read` | nothing | none |

- `forget` (`removed { lifecycle { destroy = false } }`) counts as `delete`, because the stack gives up control of the object.
- An import adds `create`, because it brings an unmanaged object into state.
- Unknown actions are refused, so a new action type never bypasses approval.

## What the plan id covers

A plan ID is the first 32 hex characters of SHA-256 over the masked plan document, the SHA-256 of the saved plan file, the commit SHA, the stack name, and the env.

Writ seals the `tofu plan -out` file with AES-256-GCM under a key derived from the env's `key` file, with an HKDF label separate from state's and the plan ID, stack, and env as associated data. No API route serves the saved plan, and Writ deletes it when the plan leaves live status.

## Review a plan in the control UI

The plan page renders only the masked document:

- summary counts (add, change, destroy, replace, forget, import);
- per-resource diffs grouped by module and address, with `action_reason`, `replace_paths`, import IDs, risk classes, and before and after attributes, sensitive values shown as `(sensitive)`;
- drift and output changes.

If the document cannot be parsed, the page disables approval. After an apply, the run page shows status, timing, per-resource events, and masked diagnostics on failure.

## What the run's document contains

- A completed apply stores `writ.tofu-apply/v0`: `ok`, `applied`, `status` (`applied` or `apply-failed`), `state_version`, `summary`, and masked `events` (`apply_start`, `apply_complete`, `apply_errored`, without provider-chosen resource IDs).
- A failed apply adds masked diagnostic summaries and source locations, never detail text or code snippets.
- An apply that never started, or was lost to a restart, stores `writ.apply/v0` with `status: "apply-failed"`.

## Apply limits

| Limit | Value |
| --- | --- |
| `limits.applies` | Default `4` concurrent applies. Excess requests get `429`. Applies share `provider_uids` with plans and wait for a free UID. |
| Settling a failed state commit | Holds an apply slot for at most 2 minutes (5-second retries). |
| `limits.apply_start_timeout` | Default `10m`: bounds the wait for the saved plan, the role's credentials, and a free provider UID before the first change ([Time limits](api.md#time-limits)). |
| `limits.apply_timeout` | Default `4h`: then OpenTofu gets `SIGINT`, so running changes finish and no new ones start ([Time limits](api.md#time-limits)). |
| Request keys | Live 24 hours, at most 5 minutes ahead of server time. Closed `none` keys are pruned after 25 hours. |
| `writ apply --remote` | 30-second timeout per request, up to 5 apply attempts and 5 outcome queries (backoff 2s, 4s, 8s, 16s). Follows a run for up to 30 minutes from attach. |
| Pipeline approval | At most 50 plans. |
| Override and adoption reasons | 10 to 500 characters. |
