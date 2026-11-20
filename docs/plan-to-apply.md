# How a plan becomes an apply

Every infrastructure change in Writ follows one verifiable chain: a Git commit produces a sealed saved plan, people approve that exact plan's ID, and Writ applies that saved plan and nothing else.

Understanding this lifecycle helps you trace why Writ refuses stale plans, why CI jobs never touch cloud credentials, and how Writ recovers cleanly if a server crashes mid-apply.

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

A plan starts when you run `writ plan --remote` from a clean Git work tree or when a GitLab CI job sends `$CI_COMMIT_SHA`. Clients never upload Terraform files to a team's `writ serve`.

1. `writ serve` verifies the caller's OIDC token or session and checks for a `plan` grant on `stack@env`.
2. It verifies with the Git host that the commit exists on the claimed ref (for example `refs/heads/main` or the merge request ref).
3. It fetches the stack's files and local modules (`./`, `../`) from the Git host, skips blobs already cached in the `blobs` table, and verifies every downloaded file against its Git SHA-1 blob ID.
4. It checks the stack against [structural and module-source rules](serve.md#what-a-stack-can-contain) before OpenTofu runs.

## 2. Isolated planning and masking

1. `writ serve` installs the providers pinned in `.terraform.lock.hcl` into `tofu.plugin_cache` using a synthetic config, so stack code never runs with write access to the cache.
2. It allocates a free `provider_uids` UID and creates a private Linux user, mount, and PID namespace with a read-only filesystem view.
3. It runs `tofu init -lockfile=readonly` with **no cloud credentials**.
4. It assumes the environment's `plan_role` via STS and runs `tofu plan -lock=false` and `tofu show -json`. On the `http` state backend, the run receives a short-lived read-only state token. On S3, `plan_role` has read-only bucket permissions. Writ reads the saved plan as soon as `tofu plan` exits, and `tofu show` reads that copy from a sealed in-memory file that no process can change, so the plan document, the policy input, the plan ID, and the sealed saved plan all come from the same bytes.
5. `internal/mask` strips secrets from the plan JSON using OpenTofu's sensitive marks, learned values (including encoded forms), secret-like attribute names, `provider_env` values, assumed role credentials, and the prior state's sealed [secret set](glossary.md).

## 3. Policy evaluation (plan point) and sealing

If your server config defines a `policy` block, Writ evaluates every Rego policy set against the masked plan document in a bounded child process (`input.writ.decision == "plan"`).

- A mandatory deny sets the plan status to `refused` and discards the saved plan. It cannot be approved or applied.
- Otherwise, Writ computes the **plan ID**: the first 32 hex characters of a SHA-256 over the masked plan, the SHA-256 of the binary saved plan, the commit SHA, the stack name, and the environment.
- Writ seals the binary saved plan with AES-256-GCM under a key derived from the environment's `key` file, bound to the plan ID, stack, and environment as associated data. No API route ever returns the saved plan.

## 4. Approvals and status transitions

Every plan has one status at a time, governed by a single transition table in `internal/control/lifecycle.go`:

| Status | Meaning |
| --- | --- |
| `planning` | `writ serve` is running OpenTofu to produce the plan. |
| `pending` | Saved and waiting for the approvals required by your `approval` rules. |
| `approved` | Has enough valid approvals (or no approval rule matched). |
| `applying` | A run is currently executing `tofu apply` on the saved plan. |
| `applied` | Every change succeeded. Terminal. |
| `refused` | Planning or plan-point policy failed; no saved plan kept. Terminal. |
| `failed` | Planning couldn't run or the apply failed. Terminal. |
| `expired` | `plan_ttl` elapsed before apply. Terminal. |
| `superseded` | State, the source branch, or a newer plan replaced this plan. Terminal. |

A live plan's effective status is always evaluated against your **current** server config. If you tighten an `approval` rule or rename an `identity`, previously approved plans immediately become `pending` until they satisfy the new rules.

Whenever any approval rule applies, the plan's author (matched by stable `person id`, `IDENTITY/user/ID`, across both CI jobs and browser/CLI sessions) can never approve their own plan.

## 5. Apply-point checks and execution

When you run `writ apply --remote ID` or `writ apply --remote --merged --env ENV --stack NAME`:

1. **Source match**: For `--merged`, Writ fetches the stack's files at the merged commit on the default branch, computes their source digest, and finds the newest live approved plan with that exact source digest. This works with merge commits, squash merges, and fast-forward merges.
2. **Staleness checks**: Writ refuses the apply if the plan expired, if a newer plan exists for that `stack@env`, if state changed since the plan, or (for branch applies by ID) if the branch moved to different stack files.
3. **Apply-point policy**: Writ evaluates every policy set again with `input.writ.decision == "apply"`, the caller's identity, and the current approvals (binding a digest of the approvals so they cannot change between evaluation and start). Nothing overrides a policy deny.
4. **Atomic start**: In one database transaction, Writ records the CI token's `jti` (one apply per token), records the `Idempotency-Key` (so lost network replies never trigger a duplicate apply), enforces one running apply per `stack@env`, transitions the plan to `applying`, and creates the run row.
5. **Execution**: Writ opens the sealed saved plan, assumes the environment's `apply_role`, grants a write-capable state token (for `http` backend), and runs `tofu apply` in an isolated provider UID namespace. `tofu show` and `tofu apply` read the saved plan from a sealed in-memory file, not from the run's scratch directory, so no process of the run can replace it between the two.

## 6. Post-apply indexing, audit, and crash recovery

- **State and secret set**: On the `http` backend, each state write is sealed under the environment key alongside a new sealed secret set carrying forward every secret the mask knew. On S3, Writ pulls the post-apply serial and seals its secret set.
- **State index**: Writ runs `tofu show -json` on the resulting state (with no cloud credentials) and updates the stack's resource index through `internal/mask`.
- **Saved plan deleted**: Once a plan leaves live status (`applied`, `failed`, `expired`, or `superseded`), its sealed saved plan is deleted.
- **Crash recovery**: Every `writ serve` process tags runs with its process ID. On restart (and every minute after), `writ serve` marks abandoned `applying` runs as `failed`, revokes their state tokens, and releases their `http`-backend state locks in one transaction.

## Next steps

- [Plan and apply](apply.md): run these steps from your terminal or CI.
- [Configure approvals](approvals.md): write approval rules and grants.
- [Security model](security-model.md): inspect the trust boundaries around each step.
- [Architecture](architecture.md): see the Go packages that implement each stage.
