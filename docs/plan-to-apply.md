# How a plan becomes an apply

Every change in Writ follows one chain: a Git commit produces a sealed saved plan, reviewers approve that exact plan ID, and `writ serve` applies that saved plan and nothing else.

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

Planning starts from `writ plan --remote` in a clean Git working tree or a CI job sending `$CI_COMMIT_SHA`. Clients never upload stack files to a team's `writ serve`.

1. Verify the caller's OIDC token or session and `plan` grant on `stack@env`.
2. Confirm with the Git host that the commit is on the claimed ref.
3. Fetch stack and local module (`./`, `../`) blobs missing from `blobs` and verify every blob's Git SHA-1 ID.
4. Enforce [stack structure and module-source rules](add-repository.md#what-a-stack-can-contain) before running OpenTofu.

## 2. Isolated planning and masking

1. Install lock-file providers into `tofu.plugin_cache` from a synthetic config (stack code never gets write access to the cache).
2. Allocate a free `provider_uids` UID and launch Linux user, mount, PID, and IPC namespaces with a read-only filesystem view, seccomp filter, and per-step cgroup limits.
3. Run `tofu init -lockfile=readonly` with no cloud credentials.
4. Assume `plan_role` via STS and run `tofu plan -lock=false` (read-only state token on HTTP backend; read-only role on S3). Copy the saved plan immediately into a sealed in-memory file and run `tofu show -json` against that copy so plan document, policy input, plan ID, and sealed plan share the exact same bytes.
5. Pass plan JSON through `internal/mask` (sensitive marks, learned values and encodings, secret-named keys, `provider_env`, assumed STS credentials, and prior state's sealed [secret set](glossary.md)).

## 3. Policy evaluation (plan point) and sealing

If `policy` is configured, Writ evaluates all Rego sets in a bounded child process (`input.writ.decision == "plan"`):

- A mandatory deny sets status to `refused` and discards the saved plan.
- A no-op plan with no resource, output, move, import, or drift changes becomes `no-changes` (no saved plan, nothing to approve or apply).
- Otherwise, Writ computes the plan ID (first 32 hex chars of SHA-256 over masked plan, saved plan SHA-256, commit SHA, stack, and env) and seals the binary saved plan with AES-256-GCM bound to plan ID, stack, and env.

## 4. Approvals and status transitions

Plan statuses follow a single transition table in `internal/control/lifecycle.go`:

| Status | Meaning |
| --- | --- |
| `planning` | `writ serve` is running OpenTofu. |
| `pending` | Saved and waiting for required approvals. |
| `approved` | Has required approvals (or no rule matched). |
| `applying` | A run is executing `tofu apply`. |
| `applied` | Apply succeeded. Terminal. |
| `no-changes` | Nothing to change; nothing to approve or apply. Terminal. |
| `refused` | Validation or plan-point policy failed; no saved plan kept. Terminal. |
| `failed` | Plan execution or apply failed. Terminal. |
| `expired` | `plan_ttl` elapsed before apply. Terminal. |
| `superseded` | State, source branch, or a newer plan replaced this plan. Terminal. |

Live plans are evaluated against your current server config on every read and apply. Whenever an approval rule applies, the plan's author (by stable `IDENTITY/user/ID` across CI, CLI, and browser) can never approve their own plan.

## 5. Apply-point checks and execution

When applying by ID or via `--merged`:

1. **Source match:** `--merged` reads stack files at the merged default-branch commit and selects the newest live approved plan matching that source digest (works for merge commits, squash merges, and fast-forwards).
2. **Staleness checks:** Rejects expired plans, older plans superseded by a newer plan, plans whose state moved, or branch plans whose branch moved.
3. **Apply-point policy:** Re-evaluates all Rego sets with `input.writ.decision == "apply"`, caller, and current approvals (binding an approvals digest so approvals can't change before start). No override bypasses a deny.
4. **Atomic start:** In one transaction, records CI token `jti` (one apply per token), records `Idempotency-Key`, enforces one active apply per `stack@env`, sets plan status to `applying`, and inserts the run.
5. **Execution:** Decrypts the sealed saved plan into a sealed in-memory file, assumes `apply_role`, grants a write-capable HTTP state token (if applicable), and runs `tofu apply` inside an isolated provider UID namespace.

## 6. Post-apply indexing, audit, and crash recovery

- **State and secret set:** Seals the resulting state (on HTTP backend) and secret set (on HTTP and S3) under the environment key.
- **State index:** Runs offline `tofu show -json` to update allow-listed resource rows via `internal/mask`.
- **Saved plan cleanup:** Deletes the sealed saved plan as soon as the plan leaves live status.
- **Crash recovery:** On startup and every minute, `writ serve` marks `applying` runs belonging to other process IDs as `failed`, revokes their state tokens, and releases HTTP state locks in one transaction.

## The state index

Writ maintains a queryable resource index per `stack@env` (served at `GET /v1/index` and rendered in the `/ui/stack` Resources tab). It stores only `address`, `type`, `provider`, and allow-listed identifying columns: `id`, `arn`, `identifier`, `region`, plus `namespace` and `modify_index` for `nomad_job`. Sensitive or masked values are stored empty.

The index refreshes after each successful apply (using offline `tofu show -json` with no cloud credentials or `provider_env`) and every 5 minutes in the background (up to 50 lagging stacks per sweep). States without a sealed secret set aren't indexed until adopted with [`writ state adopt`](adopt-state.md) or overwritten by a Writ apply.

## Next steps

- [Plan and apply](apply.md): CLI and CI commands.
- [Approvals](approvals.md): approval rules and staleness checks.
- [Security model](security-model.md): trust boundaries across every stage.
