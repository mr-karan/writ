# Approvals

Require teammates to approve a plan before it applies. Approving a merge request authorizes merging code; approving a Writ plan authorizes applying that exact plan, which `writ serve` applies with the env's `apply_role`. Clients never send code or saved plans to an apply, and never hold cloud credentials or state keys.

## Before you begin

- A team's `writ serve` with teams and grants ([Server config](config.md)).
- Reviewers with `approve` on the stacks and envs they review.

## Set how many approvals a plan needs

1. Add `approval` blocks to `/etc/writ/serve`, matched by stack pattern, env, and risk class:

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

2. Check the rules with `writ serve --check`, which prints them:

   ```
   approvals: rule 1: app in prod with create update replace delete data-loss needs 1
   ```

3. Restart `writ serve`.

How rules combine:

- Writ takes the highest `need` (1 to 10) of all matching rules.
- A plan with no risk classes needs no approval. A plan that changes nothing becomes [`no-changes`](plan-lifecycle.md#a-plan-that-changes-nothing).
- With no `approval` blocks, no plan needs approval.
- `stacks` uses the [grant glob syntax](config.md#stack-patterns): `*` matches within one path segment, `**` any number of segments (`data/**` matches `data` and everything under it).
- Writ records the required count at plan time, re-evaluates the current rules at apply time, and takes the larger. Stricter rules apply at once to existing plans.
- How each OpenTofu action maps to a risk class is in [How a change gets its risk class](plan-lifecycle.md#how-a-change-gets-its-risk-class).

## Who can approve a plan

Any person with `approve` on the plan's `stack@env`, except the plan's author.

- A person is `IDENTITY/user/ID` (for example `gitlab/user/2002`): the issuer's configured `identity` plus the user ID. Issuers that share an `identity`, such as GitLab login and GitLab CI, map to the same person, so a user cannot approve their own CI plan from the CLI or browser. See [Who a person is](config.md#who-a-person-is).
- Once a plan has enough approvals, more approve requests succeed as no-ops, so a rerun pipeline approve job passes.
- `writ serve --check` rejects an `approve` grant to a CI issuer team unless the team matches a user claim (`user_access_level`, `user_id`, or `user_login`), and rejects any `override` or `adopt` grant to a CI issuer team.
- A CI job approves only plans of its own pipeline. The full rules, and what renaming an identity does, are in [Approvals from CI jobs](plan-lifecycle.md#approvals-from-ci-jobs).

## Approve and track a plan

1. Approve with `writ approve --remote ID`, the **Approve** button on the plan page, or the CI `writ-approve` job ([Plan and apply](apply.md#approve-a-plan)).
2. Follow the plan's status on `GET /v1/plans/{id}`, which records every transition in `history`. Statuses and transitions are in [Plan statuses](plan-lifecycle.md#plan-statuses).
3. Apply it by ID or with `--merged`. An approved plan can still be refused if it went stale ([Why an approved plan can still be refused](plan-lifecycle.md#why-an-approved-plan-can-still-be-refused)).

## Troubleshooting

| Problem | Fix |
| --- | --- |
| `denied: PRINCIPAL made plan ID and cannot approve it` | Authors cannot approve their own plans. Ask a teammate with `approve`. |
| `state: plan ID changes no resource; there is nothing to approve or apply` | The plan is `no-changes`. Nothing needs approval. |
| `state: plan ID is stale` | A newer plan of the stack exists. Approve that one. |
| An approved plan returned to `pending` | An approval was withdrawn, a stricter rule now applies, or the issuer's `identity` was renamed. |
| `--check` refuses an `approve` grant to a CI team | Match the team on a user claim (`user_access_level`, `user_id`, or `user_login`). |

## Next steps

- [Plan and apply](apply.md): commands, merged applies, lost replies, and overrides.
- [Plan lifecycle reference](plan-lifecycle.md): statuses, staleness checks, plan IDs, and run records.
- [Server config](config.md): teams, grants, and stack patterns.
