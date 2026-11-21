# Approvals

Approving a merge request authorizes merging code. Approving a Writ plan authorizes applying that exact plan.

On a team's control plane, every apply comes from a commit plan. Teammates other than the author approve the plan ID under your `approval` rules, and you apply it by ID or with `--merged`. `writ serve` applies the sealed saved plan with the env's `apply_role`. Clients never send code or saved plans to an apply, and never hold cloud credentials or state keys. For commands, see [Plan and apply](apply.md).

## Set how many approvals a plan needs

Add `approval` blocks to `/etc/writ/serve`, matched by stack pattern, env, and risk class:

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

- Writ takes the highest `need` (1 to 10) of all matching rules.
- A plan with no risk classes needs no approval. A plan that changes nothing becomes [`no-changes`](#a-plan-that-changes-nothing).
- With no `approval` blocks, no plan needs approval.
- `stacks` uses the [grant glob syntax](config.md#stack-patterns): `*` matches within one path segment, `**` any number of segments (`data/**` matches `data` and everything under it).
- Writ records the required count at plan time, re-evaluates the current rules at apply time, and takes the larger. Stricter rules apply at once to existing plans.
- `writ serve --check` prints the active rules.

## Who can approve a plan

Any person with `approve` on the plan's `stack@env`, except the plan's author.

- A person is `IDENTITY/user/ID` (for example `gitlab/user/2002`): the issuer's configured `identity` plus the user ID. Issuers that share an `identity`, such as GitLab login and GitLab CI, map to the same person, so a user cannot approve their own CI plan from the CLI or browser. See [Who a person is](config.md#who-a-person-is).
- Once a plan has enough approvals, more approve requests succeed as no-ops, so a rerun pipeline approve job passes.
- `writ serve --check` rejects an `approve` grant to a CI issuer team unless the team matches a user claim (`user_access_level`, `user_id`, or `user_login`), and rejects any `override` or `adopt` grant to a CI issuer team.

### Approve from a CI job

- A CI job token approves only plans of its own pipeline: same issuer, project, and pipeline ID. Project and pipeline IDs are per GitLab instance, so a job on one trusted instance cannot approve a plan from another.
- If two pipelines produce byte-identical saved plans for the same commit and state, Writ deduplicates them and links the plan to both pipelines.
- A plan made from a personal session belongs to no pipeline, and no CI job can approve it.
- `writ approve --remote --pipeline` approves every live plan of the pipeline atomically. If any plan fails validation, none is approved. Each CI token can approve once.

### After you rename an identity

Renaming an issuer's `identity` changes every person ID under it. Sessions and plan authors with an undefined identity are refused until they sign in or plan again. Approvals recorded under the old identity stop counting, and the plan returns to `pending`, unless someone approves under the new identity or you roll back the rename.

## Track a plan's status

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

```
$ writ apply --remote b9a409594e580d10863ee1bd40f99e14
error: saved-plan: plan b9a409594e580d10863ee1bd40f99e14 is superseded: refs/heads/main moved to 552091ed, whose sources of app differ from the plan's, made at b1b6861d; plan the new commit
$ writ apply --remote --merged --env prod --stack app
error: state: plan 767f1426f30110318fdd547324fe200b is stale: plan d695de3fbecf1d88f2f865938cd7ca00 of app@prod is newer; plan again
```

Real runs, 2026-11-16 (under `writ serve --dev`, and against a local GitLab stand-in).

Starting a run is atomic: Writ checks token reuse, status, expiry, approvals, and active runs before it sets `applying`. A refusal never spends the caller's CI token. At most one run executes per plan, and at most one `applying` run per `stack@env`.

## What a run records

- **Database:** plan ID, actor (`ci-job`, `person`, or `dev`), counting approvals, override reason, apply mode (by ID or merged), role session, structured document, and masked output. Read it with `writ show --remote RUN` or `GET /v1/runs/{id}`.
- **Listing:** `writ runs --remote` lists runs, newest first:

  ```
  $ writ runs --remote --stack app
  RUN                         STARTED               STACK  STATUS   BY           PLAN
  TBQ5JOKPJRPUKH36ZGJQ6FYV3O  2026-11-16T03:29:54Z  app    applied  alice (dev)  f4d5e74e89b204ae4f1d53b2aef1441d
  M5ZS3ZUQTOJB256LMXBCOMRV5U  2026-11-16T03:29:53Z  app    applied  alice (dev)  ad00789c35428a3f8f642902039fc22e
  VL7RNRTHG3SYT7GRXC5O23VWV6  2026-11-16T03:29:49Z  app    applied  alice (dev)  931d8c004160c9e54cd554d70d1df149
  ```

- **Audit and logs:** synchronous audit rows for approvals, run start, and `apply-finished`. JSON logs for `approved`, `apply started`, `apply ended`, and overrides (`WARN`).

### When writ serve stops during an apply

OpenTofu's state keeps whatever finished before the exit, and the state lock stays held until recovery. State locks never expire on their own.

- Each `writ serve` process stores a unique process ID on its runs. At startup and every minute, it marks any `applying` run of another process (or with no active local work) `failed`, marks its plan `failed`, supersedes the other live plans of that `stack@env`, and revokes its state tokens and releases its HTTP state locks in one transaction.
- When a run or plan ends, Writ revokes its state tokens and deletes its locks in one transaction, and again inside the transaction that records the run's end.
- The sweep also cleans tokens and locks of any run no longer planning or applying, and writes a `state-unlock` audit row for each lock it releases.

Run only one `writ serve` process per database ([Supported platforms and limits](status.md)).

## How an OpenTofu plan is approved and applied

Approve with `writ approve` or the control UI. Apply with `writ apply --remote ID` or `writ apply --remote --merged`. The caller needs `approve` or `apply` on `stack@env`, and `risks` that cover every risk class in the plan. The steps of an apply are in [How a plan becomes an apply](plan-to-apply.md#5-apply-point-checks-and-execution).

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

- `forget` (`removed { lifecycle { destroy = false } }`) counts as `delete`, because the stack gives up control of the object.
- An import adds `create`, because it brings an unmanaged object into state.
- Unknown actions are refused, so a new action type never bypasses approval.

### What the plan id covers

A plan ID is the first 32 hex characters of SHA-256 over the masked plan document, the SHA-256 of the saved plan file, the commit SHA, the stack name, and the env.

Writ seals the `tofu plan -out` file with AES-256-GCM under a key derived from the env's `key` file, with an HKDF label separate from state's and the plan ID, stack, and env as associated data. No API route serves the saved plan, and Writ deletes it when the plan leaves live status.

### Review a plan in the control UI

The plan page renders only the masked document:

- summary counts (add, change, destroy, replace, forget, import);
- per-resource diffs grouped by module and address, with `action_reason`, `replace_paths`, import IDs, risk classes, and before and after attributes, sensitive values shown as `(sensitive)`;
- drift and output changes.

If the document cannot be parsed, the page disables approval. After an apply, the run page shows status, timing, per-resource events, and masked diagnostics on failure.

### What the run's document contains

- A completed apply stores `writ.tofu-apply/v0`: `ok`, `applied`, `status` (`applied` or `apply-failed`), `state_version`, `summary`, and masked `events` (`apply_start`, `apply_complete`, `apply_errored`, without provider-chosen resource IDs).
- A failed apply adds masked diagnostic summaries and source locations, never detail text or code snippets.
- An apply that never started, or was lost to a restart, stores `writ.apply/v0` with `status: "apply-failed"`.

## Next steps

- [Plan and apply](apply.md): commands, merged applies, lost-response recovery, and overrides.
- [How a plan becomes an apply](plan-to-apply.md): each step from commit to state index.
- [Server config](config.md): teams, grants, and stack patterns.
- [Add a repository](add-repository.md): stack rules, forge tokens, and polling.
