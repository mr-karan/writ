# Plan and apply

Plan a commit on `writ serve`, approve that exact plan, and apply it by ID or after a merge. Statuses, staleness checks, and the apply steps are in the [Plan lifecycle reference](plan-lifecycle.md).

## Before you begin

- A running `writ serve` (or `writ serve --dev`) with your [repository added](add-repository.md).
- A [`writ login`](sign-in.md#people-writ-login) session, a dev server, or a GitHub Actions or GitLab CI ID token.
- Grants for the action (`plan`, `approve`, `apply`, or `override`) and for the plan's [risk classes](glossary.md). Check with `writ whoami`.

## Plan a commit

1. From a clean Git working tree of a registered repository, run:

   ```sh
   writ plan --remote --env prod
   ```

   It sends the checked-out commit when the repository is registered, the stack sits under `path`, no file under `path` differs from `HEAD`, and, for remote forges, the commit is pushed to a remote branch. Under `writ serve --dev`, `--env` defaults to `dev`.

2. In CI, with no checkout, pass `--stack` and `--commit`:

   ```sh
   writ plan --remote --env prod --stack app --repo infra --commit "$GITHUB_SHA" --slot-wait 15m
   ```

   On GitLab CI, pass `--commit "$CI_COMMIT_SHA"` (`--repo` is optional in GitLab CI).

`writ serve` fetches the commit from the Git host, runs `tofu plan -lock=false`, masks secrets, evaluates policies, and returns the plan ID.

## Approve a plan

- **CLI:** `writ approve --remote ID` with a [`writ login`](sign-in.md#people-writ-login) session. It prints `Approved plan ID for ENV (STACK): N of M approvals (WHO), STATUS.` Writ records your stable person ID (such as `gitlab/user/2002`) with auth `person`.
- **Control UI:** click **Approve** on the plan page.
- **GitLab CI:** a maintainer runs the manual `writ-approve` job (`writ approve --remote --pipeline`), which approves every live plan of the pipeline atomically. If the plan's author runs it, Writ refuses with exit code 1 and the plan stays `pending`. (On GitHub, people approve via the CLI or control UI.)
- **Withdraw** before the apply starts: `writ approve --remote --withdraw ID`.

## Apply a plan

Applying needs `apply` on the plan's `stack@env` and on every risk class in the plan, from one grant or several. `writ serve` decrypts the saved plan with the env's key and runs `tofu apply` with `apply_role` and a UID of its own from `provider_uids`.

### Apply by id

```
$ writ apply --remote 931d8c004160c9e54cd554d70d1df149
Run VL7RNRTHG3SYT7GRXC5O23VWV6 of plan 931d8c004160c9e54cd554d70d1df149 for dev (app), by alice (dev).
Apply: 1 added, 0 changed, 0 destroyed.
Run VL7RNRTHG3SYT7GRXC5O23VWV6 of plan 931d8c004160c9e54cd554d70d1df149 for dev (app), by alice (dev): applied at 2026-11-16T03:29:49Z.
```

Excerpt under `writ serve --dev`, 2026-11-16. The CLI streams the run until it finishes. One Ctrl-C detaches without stopping the run on the server; inspect it later with `writ show --remote RUN`. A CI job can apply by ID only a plan of its own pipeline (same issuer, project, and pipeline, or a deduplicated plan shared with it), which allows pre-merge applies such as to staging.

### Apply after a merge

```sh
writ apply --remote --merged --env ENV --stack NAME --repo REPO --commit MERGED_SHA
```

Pass `--repo` and `--commit` from a `writ login` session (as after merging a GitHub pull request); in GitLab CI, the commit comes from the token's `sha` and `--repo` is optional. Writ applies the newest approved plan whose stack sources match the merged commit, and exits 0 with nothing to apply when a current `no-changes` plan matches. The control plane enforces the gate, not the CI YAML: without an approved plan the command refuses, even if someone removed the approve step. Matching rules are in [How a merged apply picks a plan](plan-lifecycle.md#how-a-merged-apply-picks-a-plan).

### Plan and apply in one command

On `writ serve --dev`, `writ apply --remote --yes` plans the current commit and applies it at once if no approval rule matches. If approvals are required, it prints the plan and exits 1. With `--json`, stdout holds one document: the plan if nothing ran, otherwise the run. A `no-changes` plan prints and exits 0 without a run. Interrupting while following leaves the run going and exits 1 with a failure document that names the run.

## When an answer is lost

A network drop or the 30-second timeout can cut off an apply request after the server started the run. `writ apply --remote` sends an idempotency key, so it finds out what happened and attaches to the run on its own. If it cannot reach the server, it prints the key and a follow-up command:

```sh
writ show --remote --request KEY
```

That command asks whether a run started, and closes the key if none did. Run it before you retry. Key and retry rules are in [Apply request keys](api.md#apply-request-keys).

## Override the approval rules

In an emergency, a person with `override` can apply without approvals by giving a reason of 10 to 500 characters:

```sh
writ apply --remote ID --override "bob on leave, rotate leaked key, OPS-112"
```

- `--override` skips approval rules only. You still need `apply` and every risk class of the plan, and mandatory Rego policy denies still block the apply.
- CI jobs cannot use `--override`. If the plan already has its approvals, Writ refuses `--override` as `usage`.
- The reason is recorded on the run, the plan history, the audit log, the server log (`WARN`), and the stack state.

## Troubleshooting

| Error | Fix |
| --- | --- |
| `403 denied: PRINCIPAL made plan ID and cannot approve it` | Under an approval rule, authors cannot approve their own plans. Ask a teammate with `approve`. |
| `403 denied: ... needs 1 approval` on a merged apply | The matching plan lacks approvals. Approve it, then rerun the job. |
| `409 state: plan ID is stale` or `expired` | A newer plan exists, state moved, or `plan_ttl` elapsed. Plan and approve again. |
| `409 saved-plan: no approved plan matches` | The stack files of the merged commit match no live approved plan. Plan the latest commit and approve it. |
| `403 policy: policy refuses the apply` | A mandatory Rego policy denied it. `--override` does not bypass policy. Fix the code and plan again. |
| `429` on apply | `limits.applies` concurrent applies are running ([Apply limits](plan-lifecycle.md#apply-limits)). |
| Connection dropped mid-request | Run the printed `writ show --remote --request KEY` before you retry. |

## Next steps

- [Approvals](approvals.md): approval rules and who can approve.
- [Plan lifecycle reference](plan-lifecycle.md): statuses, staleness checks, risk classes, and apply limits.
- [Errors](errors.md): error classes and recovery steps.
