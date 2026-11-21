# Approvals

Approving a merge request authorizes merging code. Approving a Writ plan authorizes applying that exact plan.

On a team's control plane, every apply comes from a commit plan. Teammates other than the author approve the plan ID according to your `approval` rules, then you apply it by ID or via `--merged`. `writ serve` applies the sealed saved plan with the environment's `apply_role`; clients never send code or saved plans to an apply and never hold cloud credentials or state keys. For CLI commands, see [Plan and apply](apply.md).

## Set how many approvals a plan needs

Define `approval` blocks in `/etc/writ/serve` by stack pattern, environment, and risk class:

```hcl
approval {
  stacks = ["*"]
  envs   = ["prod"]
  risks  = ["create", "update", "replace", "delete", "data-loss"]
  need   = 1
}

approval {
  stacks = ["*"]
  envs   = ["*"]
  risks  = ["delete", "data-loss"]
  need   = 1
}
```

Writ takes the highest `need` (1 to 10) across all matching rules. A plan with no risk classes needs no approval; a plan that changes nothing becomes [`no-changes`](#a-plan-that-changes-nothing). With no `approval` blocks, no plan requires approval. Run `writ serve --check` to print active rules.

`stacks` uses the same glob syntax as [grants](config.md#stack-patterns): `*` matches within one path segment and `**` matches any number of segments (`data/**` matches `data` and everything under it).

Writ records the required count at plan time and re-evaluates current rules at apply time, taking the larger number. Stricter config rules apply immediately to existing plans.

## Who can approve a plan

Any person with `approve` on the plan's `stack@env` can approve it, except the plan's author.

Writ identifies each person by `IDENTITY/user/ID` (for example `gitlab/user/2002`), combining the issuer's configured `identity` with the user ID. Issuers sharing an `identity` (such as GitLab login and GitLab CI) map to the same person, so a user can't approve their own CI plan from the CLI or browser. See [Who a person is](config.md#who-a-person-is).

Once a plan has enough approvals, extra approve requests succeed as no-ops (so a pipeline's approve job passes if re-run after approval).

`writ serve --check` rejects an `approve` grant to a CI issuer team unless the team matches a user claim (`user_access_level`, `user_id`, or `user_login`), and rejects `override` or `adopt` grants to any CI issuer team.

### Approve from a CI job

A CI job token can approve only plans from its own pipeline (same issuer, project, and pipeline ID). If two pipelines produce byte-identical saved plans for the same commit and state, Writ deduplicates them and associates the plan with both pipelines.

Because project and pipeline IDs are scoped per GitLab instance, a job on one trusted GitLab instance can't approve a plan from another. Plans created from a personal session belong to no pipeline and can't be approved by a CI job.

`writ approve --remote --pipeline` approves every live plan in the pipeline atomically; if any plan fails validation, none are approved. Each CI token can approve once.

### After you rename an identity

Renaming an issuer's `identity` changes every person ID under it. Sessions or plan authors referencing an undefined identity are refused until they sign in or plan again. Approvals recorded under the old identity stop counting (reverting the plan to `pending`) unless you approve under the new identity or roll back the rename.

## Track a plan's status

Each plan has one status updated conditionally in the control database, with every transition logged to `plan_events` (`history` in `GET /v1/plans/{id}`).

| Status | Meaning |
| --- | --- |
| `planning` | `writ serve` is running `tofu plan`. |
| `pending` | Saved and waiting for approvals. |
| `approved` | Has all required approvals (or needs none). |
| `applying` | A run is applying the plan. |
| `applied` | The run succeeded. |
| `no-changes` | Nothing to change; nothing to approve or apply. |
| `refused` | Plan failed validation or policy; not saved. |
| `failed` | Plan execution or apply failed. |
| `expired` | `plan_ttl` elapsed before apply. |
| `superseded` | State, a newer plan, or the source branch moved (`superseded_why` explains). |

For live plans, Writ computes effective status (`pending` vs `approved`) against your current config whenever plans are read, listed, or applied. Config changes take effect immediately without rewriting database rows; the stored status updates at the next approval, withdrawal, or run.

Allowed state transitions (enforced in `internal/control/lifecycle.go`):

| From | To | When |
| --- | --- | --- |
| `planning` | `pending`, `approved`, `no-changes`, `refused`, `failed` | Plan finishes. |
| `planning` | `superseded` | Another apply ran on the same `stack@env` during planning. |
| `pending` | `approved` | Required approvals met (or config change restores count). |
| `approved` | `pending` | Approval withdrawn or config change makes count short. |
| `pending`, `approved` | `applying` | Run starts (from `pending` only with `--override`). |
| `pending`, `approved` | `expired` | `plan_ttl` elapsed. |
| `pending`, `approved` | `superseded` | Another run ended or apply found the plan stale. |
| `applying` | `applied`, `failed` | Run ends. |

`applied`, `no-changes`, `refused`, `failed`, `expired`, and `superseded` are terminal.

### A plan that changes nothing

A plan becomes `no-changes` only when every resource change is a no-op with no move or import, no output changes, and no outside-OpenTofu drift. Plans that change an output, move a resource, read an apply-time data source, or reconcile drift still write state and stay `pending` or `approved`.

A `no-changes` plan never expires, stores no saved plan, and rejects direct `approve` or `apply` by ID with `state`. `writ approve --remote --pipeline` skips it while approving other plans in the pipeline. After merge, `writ apply --remote --merged` exits 0 with nothing to apply as long as no run or state write intervened; if one did, Writ verifies whether the newest run applied those sources and wrote the current HTTP state version, otherwise asking for a fresh plan.

## Why an approved plan can still be refused

Before starting an apply, Writ verifies the plan still matches current code and state:

- **Expired (`state`):** `plan_ttl` elapsed; plan becomes `expired`.
- **State changed (`state`):** A run was applying on `stack@env` at or after plan request time; plan becomes `superseded`. Finishing any run also supersedes all other live plans for that `stack@env`.
- **Branch moved (`saved-plan`):** When applying a branch plan by ID, Writ checks the branch head on the Git host. If stack files differ or the branch was deleted, the plan becomes `superseded`. Non-branch refs can only be applied via `--merged`.

  ```
  $ writ apply --remote b9a409594e580d10863ee1bd40f99e14
  error: saved-plan: plan b9a409594e580d10863ee1bd40f99e14 is superseded: refs/heads/main moved to 552091ed, whose sources of app differ from the plan's, made at b1b6861d; plan the new commit
  ```

  Output under `writ serve --dev`, 2026-11-16.

- **A newer plan exists (`state`):** Only the newest plan for a `stack@env` can apply (ordered by a per-stack sequence number). Plans superseded by a newer plan in the same merge request can't be approved either.

  ```
  $ writ apply --remote --merged --env prod --stack app
  error: state: plan 767f1426f30110318fdd547324fe200b is stale: plan d695de3fbecf1d88f2f865938cd7ca00 of app@prod is newer; plan again
  ```

  Output of `TestTofuApproveApply` in `e2e/`, 2026-11-16.

  ```
  $ writ apply --remote 545df270380c53e2f138f94a8af47c27
  error: state: plan 545df270380c53e2f138f94a8af47c27 is stale: plan ad00789c35428a3f8f642902039fc22e of app@dev is newer; plan again
  ```

  Output of a real run, 2026-11-16.

- **State version moved (`state`):** On Writ's HTTP backend, the newest state version must match what the plan read. On S3, `tofu apply` itself refuses if serial or lineage moved.
- **OpenTofu's check:** If state changed after planning, OpenTofu refuses the saved plan and the run fails with `OpenTofu refused plan ID: the state of STACK@ENV changed after the plan was made; plan again`.

Starting a run happens in one transaction: check token reuse, lock the plan, verify status/expiry/approvals/runs, update status to `applying`, and insert the run. Refusals don't spend the caller's CI token (even when committing `expired` or `superseded`, Writ deletes the token-use and idempotency rows before commit). Unique indexes guarantee at most one run per plan and at most one `applying` run per `stack@env`.

## What a run records

- **Database:** Plan ID, actor (`ci-job`, `person`, or `dev`), counting approvals, override reason, apply mode (by ID or merged), role session, structured document, and masked output. View with `writ show --remote RUN` or `GET /v1/runs/{id}`.
- **Listing:** `writ runs --remote` lists runs newest first:

  ```
  $ writ runs --remote --stack app
  RUN                         STARTED               STACK  STATUS   BY           PLAN
  TBQ5JOKPJRPUKH36ZGJQ6FYV3O  2026-11-16T03:29:54Z  app    applied  alice (dev)  f4d5e74e89b204ae4f1d53b2aef1441d
  M5ZS3ZUQTOJB256LMXBCOMRV5U  2026-11-16T03:29:53Z  app    applied  alice (dev)  ad00789c35428a3f8f642902039fc22e
  VL7RNRTHG3SYT7GRXC5O23VWV6  2026-11-16T03:29:49Z  app    applied  alice (dev)  931d8c004160c9e54cd554d70d1df149
  ```

  Output of a dev control plane, 2026-11-16.

- **Audit and logs:** Synchronous audit rows for approvals, run start, and `apply-finished`; JSON logs for `approved`, `apply started`, `apply ended`, and overrides (`WARN`).

### When writ serve stops during an apply

If `writ serve` crashes or stops mid-apply, OpenTofu's state reflects whatever finished before exit, and the state lock stays held until recovered.

Each `writ serve` process has a unique process ID stored on its runs. At startup and every minute thereafter, `writ serve` marks any `applying` run belonging to another process (or without active local work) as `failed`, marks its plan `failed`, supersedes other live plans for that `stack@env`, and releases its state tokens and locks. Run only one `writ serve` process per database ([Supported platforms and limits](status.md)).

## How an OpenTofu plan is approved and applied

Approve with `writ approve` or the control UI; apply with `writ apply --remote ID` or `writ apply --remote --merged`. Callers need `approve` or `apply` on `stack@env`, plus `risks` covering every risk class in the plan.

### How a change gets its risk class

| OpenTofu actions | Shown as | Risk class |
| --- | --- | --- |
| `create` | create | `create` |
| `update` | update | `update` |
| `delete` | delete | `delete` |
| `delete, create` or `create, delete` | replace | `replace` |
| `forget` | forget | `delete` |
| `forget, create` | replace | `replace` |
| `no-op` or `read` | nothing | none |

`forget` (`removed { lifecycle { destroy = false } }`) counts as `delete` because the stack relinquishes control of the object. Imports add `create` because they bring an unmanaged object into state. Unknown actions are refused so new action types never bypass approval.

### What the plan id covers

A plan ID is the first 32 hex characters of SHA-256 over the masked plan document, the SHA-256 of the saved plan file, the commit SHA, the stack name, and the environment.

Writ seals the `tofu plan -out` file with AES-256-GCM under a key derived from the environment's `key` file (separate HKDF label from state, with plan ID, stack, and environment as associated data). No API route serves the saved plan, and Writ deletes it when the plan leaves live status.

### Review a plan in the control UI

The plan page renders only the masked document: summary counts (add, change, destroy, replace, forget, import), per-resource diffs grouped by module and address (`action_reason`, `replace_paths`, import IDs, risk classes, before/after attributes with sensitive values shown as `(sensitive)`), drift, and output changes. If the document can't be parsed, the page disables approval.

After apply, the run page shows status, timing, per-resource events, and masked diagnostics on failure.

### How the apply runs

1. Writ fetches the commit files from the Git host, verifies them against the plan's source digest, and writes them to a fresh scratch directory.
2. `tofu init -input=false -lockfile=readonly` and `tofu show -json PLAN` run without cloud credentials inside the [run sandbox](run-isolation.md).
3. The mask loads the state's [secret set](secrets.md) plus secrets learned from `tofu show -json PLAN` so unmarked values (such as `nonsensitive()` `for_each` keys or rotated secrets) are still scrubbed.
4. `tofu apply -input=false -json PLAN` runs with `provider_env` and `apply_role` as an isolated UID.
   - **HTTP backend:** Receives a single-run token scoped to `LOCK`, `POST`, and `UNLOCK` for that `stack@env`, revoked when the run ends.
   - **S3 backend:** Runs a second offline `init` with `apply_role`, lets `tofu apply` manage the bucket lock file, and runs `tofu state pull` to record the resulting serial ([AWS roles and credentials](aws-credentials.md#2-grant-s3-state-permissions-if-using-backend-s3)).

```
OpenTofu apply of app@prod
  terraform_data.note: create complete
  terraform_data.secret: create complete
  module.cert.tls_private_key.key: create complete
  random_pet.name: create complete
Apply: 4 added, 0 changed, 0 destroyed.
State version 1 of app@prod.
```

Output of `TestTofuApproveApply`, 2026-11-16.

Past `limits.apply_timeout`, Writ sends `SIGINT` to OpenTofu so running operations finish and write state without starting new ones, then fails the run. If OpenTofu hasn't exited 2 hours later, Writ kills it.

### What the run's document contains

Completed applies store a `writ.tofu-apply/v0` document containing `ok`, `applied`, `status` (`applied` or `apply-failed`), `state_version`, `summary`, and masked `events` (`apply_start`, `apply_complete`, `apply_errored` without provider-chosen resource IDs). Failed applies include masked diagnostic summaries and source locations (never detail text or code snippets). Applies that never started or were lost to a restart store `writ.apply/v0` with `status: "apply-failed"`.

### What else an apply does

- **Refreshes the state index:** Runs `tofu show -json` with no cloud credentials after a successful apply to update the [state index](plan-to-apply.md#the-state-index). A refresh error doesn't fail the apply; background refresh retries within 5 minutes.
- **Re-evaluates policy:** Policies run again at apply time with `input.writ.decision = "apply"`, the caller, and active approvals. A deny returns `403 policy`, leaves the plan approved, and can't be overridden. See [Write and test policies](write-a-policy.md).
- **Writes audit rows:** Every state backend read, write (`state-write` with version, serial, lineage, SHA-256), lock, and unlock is audited, plus `apply-finished`.
- **Requires a secret set:** Plans against state versions without a sealed secret set fail so they can't be approved. Run `writ state adopt` with an `adopt` grant and a 10 to 500 character reason to establish one ([Adopt a state](adopt-state.md)).

### How Writ releases locks after a crash

State locks don't expire on their own. When a run or plan ends, Writ revokes its state tokens and deletes its locks in one transaction (and also inside the transaction that records the run's end). The startup and per-minute sweep cleans up tokens and locks for any run that is no longer planning or applying, writing a `state-unlock` audit row for every lock released.

## Next steps

- [Plan and apply](apply.md): commands, merged applies, lost-response recovery, and overrides.
- [How a plan becomes an apply](plan-to-apply.md): end-to-end lifecycle from commit to state index.
- [Server config](config.md): teams, grants, and stack patterns.
- [Add a repository](add-repository.md): stack rules, forge tokens, and polling.
