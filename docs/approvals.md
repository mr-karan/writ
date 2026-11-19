# Approvals

When you approve a merge request, you're saying the code can merge. When you approve a Writ plan, you're saying that exact plan can run. Writ is built around the second kind of approval.

On a team's control plane, every apply starts from a commit's plan. Someone other than the plan's author approves it, as many times as your approval rules require. Then you apply it, either by the plan's id or by a merged commit whose sources equal the plan's. `writ serve` applies the saved plan it kept, with the environment's apply role, to the state it holds.

Clients never send code or a saved plan to an apply. No laptop or CI job holds the state key or a cloud credential, either. For the commands, see [Approve and apply a plan](apply.md).

Today `writ serve` plans only commits. If you kept plans from before that, some may be speculative plans, made from files a client sent. No control plane takes an approval of one, `writ serve --dev` included, and no control plane applies it.

## Set how many approvals a plan needs

You write approval rules in the config. Each `approval` block is one rule, and it says how many approvals a plan needs, by stack, environment, and risk class. In this example, the first rule requires one approval for every kind of change in prod, and the second requires one for a delete or a data loss anywhere:

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

Writ takes the largest `need` among the rules that match the plan's stack, its environment, and any of its risk classes. If a plan has no changes, it needs no approval. You can set `need` from 1 to 10.

In `stacks` you write patterns, the same as in [grants](serve.md#stack-patterns). Use `*` to match within one part of a stack name, and a part `**` to match any number of parts, so `data/**` covers `data` and every stack under it.

When Writ creates a plan, it records what the rules required at that moment. At apply time it requires the larger of that and what your current rules require. So if you make the config stricter, that counts at once.

If you write no rules, no plan needs an approval. To print your rules, run `writ serve --check`.

## Who can approve a plan

Anyone with `approve` on the plan's `stack@env` can approve it, except the plan's author. The approver must be a person. The rest of this section covers how Writ counts people, approvals from CI jobs, and what happens when you rename an identity.

Writ counts each person once, by their stable id, `IDENTITY/user/ID`, such as `gitlab/user/2002`. That id combines the `identity` your config gives the token's issuer with the user id. The issuers of one Git host share an identity, so the GitLab CI job alice started and alice's own session count as one person. To see why the id uses a name from your config and not the issuer's host, read [Who a person is](serve.md#who-a-person-is).

If a plan already has its approvals, another approve request passes, whoever asks, and records nothing more. That's why your pipeline's approve job passes when the author runs it after a reviewer approved the pipeline's plans.

Some grants never get as far as a request. `writ serve --check` refuses an `approve` grant to a CI issuer's team unless the team matches a claim about the person who ran the job, such as `user_access_level`, `user_id`, or `user_login`. It also refuses any `override` or `adopt` grant to a CI issuer's team.

### Approve from a CI job

When you approve through a CI job's token, you can approve only plans from that job's own pipeline: the same issuer, project, and pipeline.

Sometimes two pipelines produce the same plan. If OpenTofu writes a plan byte for byte the same as an earlier pipeline's, for the same commit and state, the new pipeline gets the earlier plan, with its id and its approvals. That plan then belongs to both pipelines.

Project and pipeline ids are unique only within one GitLab instance. So if your config trusts two GitLab instances, a job on the second can't approve a plan from the first, whatever its ids. And if you create a plan with your own session, it belongs to no pipeline, so no CI job can approve it.

To approve a whole pipeline, run `writ approve --remote --pipeline`. It approves every live plan in the pipeline, or none. If the job can't approve one of them, Writ refuses the whole request and records nothing. Each token approves once.

### After you rename an identity

Renaming an `identity` in the config changes the id of every person under it. Writ refuses an approval from a person whose id has an identity the config doesn't define, such as a session from before the rename. It also refuses to approve a plan whose author's id has such an identity. Sign in again, or create the plan again.

Approvals you gave before the rename stop counting, because their ids have the old identity. A plan they approved reads as `pending`, and no apply counts them. Approve again under the new identity, and you count once. If you roll the rename back, the old approval counts again and the plan reads as `approved`.

## Track a plan's status

At any moment a plan has one status. Writ changes it with a conditional update in the control database, and `plan_events` records each change with who or what made it and why. You can read those changes as `history` from `GET /v1/plans/{id}`.

| Status | Meaning |
| --- | --- |
| `planning` | `writ serve` is creating the plan. |
| `pending` | The plan is saved and waits for its approvals. A kept speculative plan stays here. |
| `approved` | The plan has the approvals its rules need, or needs none. |
| `applying` | A run is applying the plan. |
| `applied` | The run applied every change. |
| `refused` | The plan ended with errors, so it was never saved. |
| `failed` | The plan couldn't run, or its apply failed. |
| `expired` | `plan_ttl` passed before the plan was applied. |
| `superseded` | State, or the branch the plan was created from, moved after the plan. `superseded_why` says how. |

For a live plan, Writ works out the status from its approvals under your current config. A commit's plan is `approved` when the approvals that count reach what it needs, and `pending` until then. That's the status `GET /v1/plans/{id}` reports, the one `GET /v1/plans?status=` lists by, and the one each apply checks.

So when you change the config, an approval can stop counting, or count again, without any write to the control database. The stored status catches up at the plan's next approval, withdrawal, or run, and `history` records the move.

A plan can move only along the rows below. They live in one table in `internal/control/lifecycle.go`, and the store refuses any other move.

| From | To | When |
| --- | --- | --- |
| `planning` | `pending`, `approved`, `refused`, `failed` | The plan ends. A plan that needs no approval is `approved`. |
| `planning` | `superseded` | An apply on the same `stack@env` ran while the plan was being created. |
| `pending` | `approved` | The last approval the rules need arrives, or a run begins after a change of config made approvals count again. |
| `approved` | `pending` | An approval is withdrawn, or a change of config stopped one counting, and the plan falls short. The next approval, withdrawal, or run records it. |
| `pending`, `approved` | `applying` | A run begins. From `pending`, only with an override. |
| `pending`, `approved` | `expired` | `plan_ttl` passes. |
| `pending`, `approved` | `superseded` | Another run on the `stack@env` ends, or the apply finds the plan stale. |
| `applying` | `applied`, `failed` | The run ends. |

Once a plan is `applied`, `refused`, `failed`, `expired`, or `superseded`, it stays there.

**If you're upgrading from a build without approvals,** plan again after the upgrade. The upgrade moved plans once, outside this table. No earlier build had approvals, so none of its plans can have been approved. Its person ids also contained the issuer's host, and another spelling of the host could split one person in two. So migration 5 of the control database moves every live plan from an earlier build, `planning`, `pending`, or `approved`, to `expired`, and `history` gives the reason `made before approvals`. The author's fields stay as they were.

## Why an approved plan can still be refused

You approve one plan id, and that plan applies what it showed, or nothing. So before a run begins, Writ runs these checks:

- **Expired.** If the plan is past `plan_ttl`, Writ refuses it as `state`, and it becomes `expired`.
- **State changed.** Because `writ serve` is the only writer of the state it holds, its own runs are every change that state has had. So if a run on the plan's `stack@env` was applying at or after you requested the plan, Writ refuses the plan as `state`, and the plan becomes `superseded`. When a run ends, every other live plan for that `stack@env` becomes `superseded` too.
- **The branch moved.** When you apply by id a plan created from a branch, `writ serve` reads the branch's head at the Git host. If the head's sources for the stack equal the plan's, the plan passes. If they differ, or the branch is gone, Writ refuses the apply as `saved-plan`, and the plan becomes `superseded`. Writ can't check a plan of a ref that isn't a branch, so you can apply that plan only after a merge. Here's a plan created before the next commit on `main`:

  ```
  $ writ apply --remote b9a409594e580d10863ee1bd40f99e14
  error: saved-plan: plan b9a409594e580d10863ee1bd40f99e14 is superseded: refs/heads/main moved to 552091ed, whose sources of app differ from the plan's, made at b1b6861d; plan the new commit
  ```

  Output under `writ serve --dev`, 2026-11-16.

Because every plan is for an [OpenTofu stack](serve.md#opentofu-stacks), two more checks run, and OpenTofu runs a third:

- **A newer plan.** Only the newest plan for a `stack@env` applies. If someone created a newer plan after yours, Writ refuses yours as `state`, and it becomes `superseded`. You also can't approve a plan whose `superseded_by` is a newer plan of its merge request; Writ refuses that with the same message. Each plan takes the next number for its `stack@env` in the transaction that records it, so even two plans requested in the same millisecond have an order. Here, the sources of a merged commit equal an approved plan, but a newer plan of the stack replaced that plan:

  ```
  $ writ apply --remote --merged --env prod --stack app
  error: state: plan 767f1426f30110318fdd547324fe200b is stale: plan d695de3fbecf1d88f2f865938cd7ca00 of app@prod is newer; plan again
  ```

  Output of `TestTofuApproveApply` in `e2e/`, 2026-11-16.

  Under `writ serve --dev`, applying by id a plan of an earlier commit fails the same way:

  ```
  $ writ apply --remote 545df270380c53e2f138f94a8af47c27
  error: state: plan 545df270380c53e2f138f94a8af47c27 is stale: plan ad00789c35428a3f8f642902039fc22e of app@dev is newer; plan again
  ```

  Output of a real run, 2026-11-16.

- **The state moved.** For a stack on the http backend, each plan records which version of the state it read. If the newest version is a different one, Writ refuses the plan as `state`, and it becomes `superseded`. A stack on S3 has no such version. There, `tofu apply` refuses a saved plan whose state's serial or lineage moved.
- **OpenTofu's own check.** OpenTofu refuses to apply a saved plan whose state changed after the plan. The run then fails with `OpenTofu refused plan ID: the state of STACK@ENV changed after the plan was made; plan again`. `just test-tofu` proves this refusal on OpenTofu 1.13.9.

Writ starts a run in one transaction, the move to `applying`. That transaction records the token's use, locks the plan, and checks its status, expiry, approvals, and the runs of its `stack@env`. Then it moves the plan with a conditional update and inserts the run.

If Writ refuses the apply, your CI token isn't spent. Most refusals roll the transaction back. When the plan expired, or state moved under it, the refusal commits the plan's move instead, and deletes the token's use and the key's row before that commit. Once a token has started a run, Writ refuses it if you use it again. `TestRefusalSpendsNoToken` proves this on SQLite and Postgres.

Two applies of one plan, or of two plans for one `stack@env`, can't both begin. Two unique indexes enforce that on SQLite and Postgres alike: one on a plan's runs, and one on a `stack@env`'s applying run.

## What a run records

Writ keeps a record of every run, so you can see who applied what, and on whose approval:

- The control database keeps the run: its plan, its actor, the approvals that counted when it began, the override, whether it referred to the plan or a merged commit, its role session, and, when it ends, its document and printed text. To see one, run `writ show --remote RUN` or call `GET /v1/runs/{id}`. The actor is the person's stable id, with its kind: `ci-job`, `person`, or `dev`.
- To list runs, newest first, run `writ runs --remote`:

  ```
  $ writ runs --remote --stack app
  RUN                         STARTED               STACK  STATUS   BY           PLAN
  TBQ5JOKPJRPUKH36ZGJQ6FYV3O  2026-11-16T03:29:54Z  app    applied  alice (dev)  f4d5e74e89b204ae4f1d53b2aef1441d
  M5ZS3ZUQTOJB256LMXBCOMRV5U  2026-11-16T03:29:53Z  app    applied  alice (dev)  ad00789c35428a3f8f642902039fc22e
  VL7RNRTHG3SYT7GRXC5O23VWV6  2026-11-16T03:29:49Z  app    applied  alice (dev)  931d8c004160c9e54cd554d70d1df149
  ```

  Output of a dev control plane, 2026-11-16.

- Every request leaves an audit row, in the same transaction as the approval or the run it created. When a run ends, it leaves one more, of kind `apply-finished`.
- In its JSON log, `writ serve` writes `approved`, `apply started`, `apply ended`, and an override at `WARN`.

### When writ serve stops during an apply

If `writ serve` stops during an apply, the stack's state has what OpenTofu wrote before it stopped. The run's lock on that state stays until the sweep below releases it.

Every `writ serve` process has an id, and each run records the process that began it. At start, and every minute after, `writ serve` marks as `failed` each run that another process left applying, and each run of its own process that it does no work for, along with its plan. It supersedes the plans of the same `stack@env`.

The sweep each minute also catches a run whose start an earlier process committed after this one started. That can happen on Postgres, where the commit of a process that stopped can complete later. `TestRestartWithAnUnresolvedCommit` holds such a commit while a second process starts, and requires that the second process's sweep fails the run.

Run one `writ serve` process per control database, as [Status and limits](status.md) says. `TestTofuRestartEndsState` stops a run while it holds the stack's lock, starts a second process, and requires that the run's token is refused and that the next apply takes the lock.

## How an OpenTofu plan is approved and applied

You approve an OpenTofu stack's plan with `writ approve` or the control UI's approve button. You apply it with `writ apply --remote ID` or `writ apply --remote --merged`. The grant checks are the same as for any commit's plan: you need `approve` or `apply` on the `stack@env`, and `risks` that cover the plan's risk classes.

### How a change gets its risk class

Writ gives each resource change a risk class from its actions, and your approval rules and grants match on that class:

| OpenTofu actions | Shown as | Risk class |
|---|---|---|
| `create` | create | `create` |
| `update` | update | `update` |
| `delete` | delete | `delete` |
| `delete, create` or `create, delete` | replace | `replace` |
| `forget` | forget | `delete` |
| `forget, create` | replace | `replace` |
| `no-op` or `read` | nothing | none |

A forget, as `removed { lifecycle { destroy = false } }` plans, leaves the object and stops managing it. The stack loses control of the object as a delete loses it, so a forget counts as `delete`.

An import adds `create` to its change's class, because it writes state for an object the stack didn't manage.

If a plan has any other actions, Writ refuses it, with the resource's address and the actions. That way, a kind of change Writ doesn't know never applies without approval.

### What the plan id covers

When you approve, you approve a plan id. The plan id is the first 32 hex digits of a SHA-256 over the masked plan document, the SHA-256 of the saved plan, the commit, the stack, and the environment.

So a new commit creates a new id. Planning the same commit again creates a new id too, unless OpenTofu writes the same saved plan and masked document byte for byte, as for two plans in the same second. In that case the request gets the earlier plan, with its id and its approvals.

Writ seals the saved plan that `tofu plan -out` wrote with AES-256-GCM, under a key derived from the environment's `key` file. The seal has its own HKDF label, separate from the state's, and takes the plan id, the stack, and the environment as associated data. So a sealed plan opens only as the plan it was made for. No API route returns it, and Writ deletes it when the plan stops being live: applied, failed, superseded, or expired.

### Review a plan in the control UI

Before you approve in the control UI, the plan page shows you what will change. It reads only the masked document.

You see the counts to add, change, destroy, replace, forget, and import, and then each resource change that changes something, by module and then address. For each change you get its actions, OpenTofu's `action_reason` in words, the `replace_paths` that force a replace, the import marker and id, its risk classes, and its attributes before and after. A value under a sensitive mark shows as `(sensitive)`, with a note when it changes. Drift and the outputs that change have their own sections.

If the page can't read the document, it says so and offers no approval.

After the apply, the run's page shows the run's status, its plan, its timing, what the apply did to each resource, and the masked diagnostics of a failed apply.

### How the apply runs

When you apply, Writ reads the commit's files again from the Git host, checks them against the plan's sources, and writes them into a new directory. `tofu init -input=false -lockfile=readonly` and `tofu show -json PLAN` run with no cloud credential. Then `tofu apply -input=false -json PLAN` runs with the environment's `provider_env` and its `apply_role` from the credential broker. Both run as a provider uid in the [filesystem view](serve.md#what-a-run-can-see).

Before the mask reads the apply's events, it starts from the [secret set](serve.md#policy-and-the-mask) of the state the plan read. Then it learns the saved plan's secrets from that `show`, as it did when it masked the plan. It needs both because an address can contain a value that only the plan marks, as a `for_each` key made with `nonsensitive()` does, or a past value that only the set lists. If the state has no secret set, the plan fails, so no one can approve or apply it.

On the http backend, the run gets a token that can `LOCK`, `POST`, and `UNLOCK` the state of its one `stack@env`. The token ends with the run, however the run ends.

A [stack on S3](serve.md#stacks-on-s3) gets no token. Instead, a second `init`, which fetches nothing, configures its backend with the apply role. `tofu apply` takes and releases the bucket's lock file, and `tofu state pull` with the apply role reads the serial the apply left.

Here's the run's text for the job on `main` after a merge:

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

If an apply runs past `limits.apply_timeout`, OpenTofu gets SIGINT. It starts no more changes, waits for the ones running, and writes the state they left, and the run fails with that reason. If it still hasn't ended 2 hours later, `writ serve` kills it.

### What the run's document contains

When the run ends, Writ keeps its document as `writ.tofu-apply/v0`:

- `ok` and `applied`.
- `status`: `applied` or `apply-failed`.
- `state_version`: the newest version of the stack's state when the run ended.
- `summary`: OpenTofu's count of what it added, changed, imported, and destroyed.
- `events`: each `apply_start`, `apply_complete`, and `apply_errored` event with the resource's address and action, swept by the mask. An event keeps no resource id, because a provider picks the id and nothing marks one that contains a secret.

If the apply fails, the document keeps each diagnostic's severity, summary, file, line, and column, swept by the mask. It never keeps the detail or snippet, the same as for a failed plan.

If the apply couldn't start, or `writ serve` lost it by stopping during the run, Writ keeps `writ.apply/v0` instead, with `status` `apply-failed` and one diagnostic that says why.

### What else an apply does

**It refreshes the state index.** After a successful apply, the run runs `tofu show -json` on the state it left, with no cloud credential, and the mask reads it into the rows of the [state index](serve.md#the-state-index). If that step fails, the apply doesn't. The index is refreshed within five minutes.

**It checks your policy again.** If your config has a `policy` block, a plan that a policy denies when it's created is `refused` and keeps no saved plan, so no one can approve or apply it. A plan that passes is checked again when someone asks to apply it, with `input.writ.decision = "apply"`, the person or CI job that asks, and the approvals that count then. A deny at that point returns 403 with class `policy`, starts no run, and leaves the plan approved.

Nothing overrides a deny. Neither an approval nor `--override` does, because `--override` skips only the approval rules. The run keeps the decision of a pass or a warning. [Policy](policy.md) has the details.

**It writes the audit log.** Each request the run sends to the state backend gets a row. A state write, a retried one included, gets a `state-write` row with the version, serial, lineage, and SHA-256. Each read, lock, and unlock gets a row too. The run gets an `apply-finished` row.

**It won't touch a state with no secret set.** If no apply of `writ serve` sealed a secret set for the plan's state version or S3 serial, the plan fails and no one can approve it. Such a state may keep a past secret where nothing marks it, as [States with no secret set](serve.md#states-with-no-secret-set) explains.

To fix that, someone with `adopt` on the `stack@env` seals a set for the newest such state with `writ state adopt`, giving a reason of 10 to 500 characters. Its plans can then be approved. The set has only what the state marks sensitive and the environment's `provider_env` values, so a secret the state contains unmarked may show in later plans. [Adopt a state](adopt-state.md) covers the details and the risk.

### How Writ releases locks after a crash

A stack's lock has no expiry, so a run that a crash stopped would hold it for good. That's why each end of a run's state access deletes the run's tokens and then its locks, in one transaction. If the run takes a lock while its access ends, that lock is released too.

The transaction that records how a run or a plan ended also ends its tokens and releases its locks. So a run never reads as ended while its token still works, even when its own end of that access failed.

When `writ serve` marks a run left applying as `failed`, it ends the run's state tokens and releases its locks in the same transaction. At start, and every minute after, it also ends the tokens, and releases the locks, of every run that neither applies nor plans. Each lock released this way leaves an audit row of kind `state-unlock`, with the lock, the run, and the `stack@env`.

These tests prove it:

- `TestTofuRestartEndsState` stops a run while it holds the lock, starts a second process, and requires that the run's token is refused and that the next apply takes the lock.
- `TestTofuRunEndEndsState` fails the run's own end of its state access, and requires that the ended run's token is refused and its lock released.
- `TestTofuSweepEndsState` fails that end and the record of the run's end too, and requires the same after the sweep each minute.
- `TestEndStateAfterLockInFlight` holds a lock request inside its transaction on Postgres while the run's access ends, and requires that no lock is left.

## Next steps

- [Approve and apply a plan](apply.md): the commands, applies after a merge, and overrides.
- [How the control plane works](serve.md): teams, grants, and roles.
- [Repositories](repos.md): how a plan refers to a commit.
