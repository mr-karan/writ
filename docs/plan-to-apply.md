# How a plan becomes an apply

Every change follows one chain: a Git commit produces a sealed saved plan, reviewers approve that exact plan ID, and `writ serve` applies that saved plan and nothing else.

## End-to-end flow

```
MR commit ──▶ writ plan --remote ──▶ writ serve fetches commit from Git host
                                           │
                                           ▼
                                  tofu plan (plan_role, -lock=false, provider UID)
                                           │
                                           ▼
                                  mask.Plan ──▶ OPA policy (plan point)
                                           │
                                           ▼
                                  plan ID = SHA-256(masked plan + saved plan + commit + stack + env)
                                  saved plan sealed under env key (never served via API)
                                           │
                                           ▼
                        Reviewer approves plan ID (author excluded)
                                           │
                                           ▼
MR merged ──▶ writ apply --remote --merged ──▶ OPA policy (apply point)
                                           │
                                           ▼
                                  tofu apply saved plan (apply_role, state lock, provider UID)
                                           │
                                           ▼
                                  state version + secret set sealed ──▶ state index updated ──▶ audit log
```

## 1. Commit fetch and verification

Planning starts from `writ plan --remote` in a clean Git working tree, or from a CI job that sends `$CI_COMMIT_SHA`. Clients never upload stack files to a team's `writ serve`.

1. Verify the caller's OIDC token or session, and its `plan` grant on `stack@env`.
2. Confirm with the Git host that the commit is on the claimed ref.
3. Fetch the stack and local module (`./`, `../`) files not already cached, and verify each file's Git SHA-1 blob ID.
4. Enforce the [stack structure and module-source rules](add-repository.md#what-a-stack-can-contain) before OpenTofu runs.

## 2. Isolated planning and masking

1. Install lock-file providers into `tofu.plugin_cache` from a synthetic config. Stack code never gets write access to the cache.
2. Take a free `provider_uids` UID and start Linux user, mount, PID, and IPC namespaces with a read-only filesystem view, the seccomp filter, and per-step cgroup limits ([Run isolation](run-isolation.md)).
3. Run `tofu init -lockfile=readonly` with no cloud credentials.
4. Assume `plan_role` through STS and run `tofu plan -lock=false`, with a read-only state token on the HTTP backend or a read-only role on S3. Copy the saved plan at once into a sealed in-memory file and run `tofu show -json` on that copy, so the plan document, policy input, plan ID, and sealed plan share the same bytes.
5. Pass the plan JSON through Writ's mask: sensitive marks, learned values and their encodings, secret-named keys, `provider_env`, the assumed STS credentials, and the prior state's sealed [secret set](glossary.md).

## 3. Policy evaluation (plan point) and sealing

If `policy` is configured, Writ evaluates every Rego set in a bounded child process with `input.writ.decision == "plan"`.

- A mandatory deny sets `refused` and discards the saved plan.
- A plan that changes nothing becomes [`no-changes`](approvals.md#a-plan-that-changes-nothing): no saved plan, nothing to approve or apply.
- Otherwise Writ computes the [plan ID](approvals.md#what-the-plan-id-covers) and seals the saved plan, bound to the plan ID, stack, and env.

## 4. Approvals and status transitions

[Approval rules](approvals.md#set-how-many-approvals-a-plan-needs) set how many approvals a plan needs, and [Track a plan's status](approvals.md#track-a-plans-status) lists each status. Live plans are evaluated against the current server config on every read and apply. Whenever an approval rule applies, the plan's author, identified by a stable `IDENTITY/user/ID` across CI, CLI, and browser, can never approve their own plan.

## 5. Apply-point checks and execution

When applying by ID or with `--merged`:

1. **Source match:** `--merged` reads the stack's files at the merged default-branch commit and picks the newest live approved plan with that source digest. This works for merge commits, squash merges, and fast-forwards.
2. **Staleness checks:** refuse expired plans, plans superseded by a newer plan, plans whose state moved, and branch plans whose branch moved ([details](approvals.md#why-an-approved-plan-can-still-be-refused)).
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

## 6. Post-apply indexing, audit, and crash recovery

- **State and secret set:** seal the resulting state (HTTP backend) and secret set (HTTP and S3) under the env key.
- **State index:** run offline `tofu show -json` with no cloud credentials to update the [state index](#the-state-index) through the mask. A refresh error does not fail the apply; the background refresh retries within 5 minutes.
- **Audit:** every state backend read, write (`state-write` with version, serial, lineage, and SHA-256), lock, and unlock, plus `apply-finished`.
- **Saved plan cleanup:** delete the sealed saved plan as soon as the plan leaves live status.
- **Secret set required:** a plan against a state version without a sealed secret set fails, so it cannot be approved. Run `writ state adopt` with an `adopt` grant and a reason of 10 to 500 characters to make one ([Adopt a state](adopt-state.md)).
- **Crash recovery:** see [When writ serve stops during an apply](approvals.md#when-writ-serve-stops-during-an-apply).

## The state index

Writ keeps a resource index per `stack@env`, served at `GET /v1/index` and shown in the `/ui/stack` Resources tab.

- It stores only `address`, `type`, `provider`, and the allow-listed columns `id`, `arn`, `identifier`, `region`, plus `namespace` and `modify_index` for `nomad_job`. Sensitive or masked values are stored empty.
- It refreshes after each successful apply (offline `tofu show -json`, with no cloud credentials or `provider_env`) and every 5 minutes in the background, up to 50 lagging stacks per sweep.
- A state without a sealed secret set is not indexed until it is adopted with [`writ state adopt`](adopt-state.md) or overwritten by a Writ apply.

## Next steps

- [Plan and apply](apply.md): CLI and CI commands.
- [Approvals](approvals.md): approval rules and staleness checks.
- [Security model](security-model.md): trust boundaries at every stage.
