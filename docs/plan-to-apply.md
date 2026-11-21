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

## The stages

| Stage | What happens | Detail |
| --- | --- | --- |
| 1. Commit fetch | Writ checks the caller and its `plan` grant, confirms the commit is on the claimed ref, and fetches and verifies the stack's files. Clients never upload stack files. | [Commit fetch and verification](plan-lifecycle.md#1-commit-fetch-and-verification) |
| 2. Isolated planning | `tofu plan` runs in the run sandbox with `plan_role`, and the plan JSON passes through the mask. | [Isolated planning and masking](plan-lifecycle.md#2-isolated-planning-and-masking) |
| 3. Policy and sealing | Rego runs at the plan point. A deny refuses the plan; otherwise Writ computes the plan ID and seals the saved plan. | [Policy evaluation and sealing](plan-lifecycle.md#3-policy-evaluation-plan-point-and-sealing) |
| 4. Approvals | Rules set how many approvals a plan needs. The author never approves their own plan. | [Approvals](approvals.md), [Plan statuses](plan-lifecycle.md#plan-statuses) |
| 5. Apply | Writ checks the plan is still current, runs the apply-point policy, starts the run atomically, and applies the saved plan with `apply_role`. | [Apply-point checks and execution](plan-lifecycle.md#apply-point-checks-and-execution) |
| 6. After the apply | Writ seals the state and secret set, updates the state index, audits every state request, and deletes the saved plan. | [After an apply](plan-lifecycle.md#after-an-apply), [The state index](plan-lifecycle.md#the-state-index) |

## Next steps

- [Plan and apply](apply.md): CLI and CI commands.
- [Plan lifecycle reference](plan-lifecycle.md): every stage in detail.
- [Security model](security-model.md): trust boundaries at every stage.
