# Writ compared with OpenTofu in plain GitLab CI

In plain GitLab CI, your pipeline jobs run `tofu` themselves. With Writ, those jobs send a commit SHA to `writ serve`, which runs OpenTofu on your server. Here's what changes, what it costs to run, and when plain CI is the better fit.

"Plain GitLab CI" here means a merge request job running `tofu plan` and a default-branch job running `tofu apply` through [GitLab's OpenTofu component][component], using [GitLab-managed state][state] and AWS credentials from [the job's OIDC token][aws]. GitLab details come from those pages and [deployment approvals][approvals], read on 2026-11-16. See [Supported platforms and limits](status.md) for how Writ's GitLab CI component is tested.

## What's different

In plain CI, the runner executes `tofu` and holds cloud credentials, state access, and the plan file. With Writ, the CI job holds none of them: it sends a commit SHA, `writ serve` runs OpenTofu inside a per-run sandbox, and approvals bind to one exact plan.

## How they compare

| | OpenTofu in plain GitLab CI | Writ |
| --- | --- | --- |
| Where `tofu` runs | In the CI job, on the runner | In `writ serve`, in per-run Linux namespaces and its own UID |
| Cloud credentials | Held by the job via OIDC `sts:AssumeRoleWithWebIdentity` [aws] | Never given to CI. `writ serve` assumes the environment's `plan_role` for plans and `apply_role` for approved or overridden applies. AWS roles only |
| Code executed | The runner's checkout, which a merge request can change | Fetched from GitLab by commit SHA and verified against Git blob IDs; module sources must match an allow-list |
| State | GitLab-managed HTTP state, "encrypted with the Lockbox Ruby gem when at rest" (Developer reads; Maintainer/Owner applies) [state] | `writ serve` HTTP backend sealed with AES-256-GCM and versioned (plan tokens are read-only), or the stack's `backend "s3"` |
| What gets approved | A merge request, or a deployment job with deployment approvals (Premium/Ultimate) [approvals] | A plan ID: SHA-256 over the masked plan, saved plan, commit, stack, and environment |
| Who can't approve | "By default, the user who triggers a deployment pipeline can't also approve the deployment job", configurable per project [approvals] | The plan's author, wherever an approval rule applies; no setting disables this |
| The plan that applies | `fetch-mr-plan` "retrieves the approved plan artifact from the associated merge request pipeline" (requires no merge trains and "merge commit with semi-linear history") [component] | The sealed plan stored on `writ serve`. Refused if a newer plan exists, state moved, or the branch moved |
| Policy | Experimental OPA template evaluated inside the pipeline [component] | Server-side Rego evaluated at plan time and again before apply; mandatory denies have no override |
| Drift | `detect-drift` template that fails or warns a scheduled job [component] | Scheduled drift checks in `writ serve`, shown in the control UI and API, with optional Slack or webhook alerts ([drift checks](drift-checks.md), [notifications](notifications.md)) |
| Logs and secrets | Raw `tofu` output in the job log | One mask over everything stored or shown, tested by `just verify-mask`, with [three documented gaps](secrets.md#what-the-mask-cant-do) |
| Audit | Pipeline and job history | Synchronous row in the control database before each response (except health checks, discovery, and rate-limited requests); request fails if the row can't commit. Read via `GET /v1/audit`, `writ audit --remote`, or `/ui/audit` ([Read the audit log](audit-log.md)) |

## What plain CI gives you that Writ doesn't

- **Less infrastructure to operate.** Plain CI uses GitLab and your runners. Writ adds a Linux host for `writ serve`, Postgres, a `provider_uids` range, and backups for each environment's state key.
- **Production GitLab history.** GitLab's OpenTofu component is widely used in production. Writ's GitLab CI component has been tested only against a local GitLab stand-in.
- **Merge request comments.** GitLab's component can post plans to merge requests with `post_mr_plan_comment` [component]. Writ doesn't post comments to GitLab yet.
- **Multi-runner scaling.** A team's `writ serve` runs as a single node on Linux.
- **Uniform non-AWS cloud auth.** Writ's credential broker assumes AWS roles only; other providers read credentials from files on the server.

## Which should you choose?

- **Choose Writ when:**
  - Merge requests and CI runners must never hold cloud credentials or state encryption keys.
  - Every apply must execute the exact reviewed plan, and authors must never approve their own plans.
  - You want server-enforced OPA/Rego policies, secret masking, scheduled drift checks, and synchronous audit logging. See [Supported platforms and limits](status.md).
- **Choose plain CI when:**
  - Anyone who can merge is already trusted to change infrastructure directly, and holding credentials in CI jobs is acceptable.

## Next steps

- [Writ compared with other control planes](compare.md): Atlantis, OTF, Terrakube, and paid control planes.
- [Quick start](quick-start.md): try Writ locally, then set it up for a team.
- [Set up GitLab CI](set-up-gitlab-ci.md): Writ's GitLab CI jobs and server config.
- [Roles and credentials](serve.md#roles-and-credentials): how `writ serve` assumes AWS roles per run.

[component]: https://gitlab.com/components/opentofu
[state]: https://docs.gitlab.com/user/infrastructure/iac/terraform_state/
[aws]: https://docs.gitlab.com/ci/cloud_services/aws/
[approvals]: https://docs.gitlab.com/ci/environments/deployment_approvals/
