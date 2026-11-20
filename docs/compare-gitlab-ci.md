# Writ compared with OpenTofu in plain GitLab CI

You already run OpenTofu from GitLab CI, and you want to know what Writ would change. In plain CI, your jobs run `tofu` themselves. With Writ, they ask `writ serve` to plan and apply. Here's what Writ adds, what it costs you, and when plain CI is the better choice.

Here, "plain GitLab CI" means a merge request job that runs `tofu plan` and a job on the default branch that runs `tofu apply`, often through [GitLab's OpenTofu component][component], with state in [GitLab-managed state][state] and AWS credentials from [the job's OIDC token][aws]. Every fact about GitLab on this page comes from those pages and from [deployment approvals][approvals], read on 2026-11-16. See [Supported platforms and limits](status.md) for how Writ's GitLab CI component is tested.

## What's different

In plain CI, your job runs OpenTofu and holds what OpenTofu needs: the cloud credentials, the state, and the plan. With Writ, your job sends a commit and holds none of them. `writ serve` runs OpenTofu, and an approval is for one exact plan.

## How they compare

| | OpenTofu in plain GitLab CI | Writ |
| --- | --- | --- |
| Where `tofu` runs | In the CI job, on the runner | In `writ serve`, as its own uid for each run, in its own namespaces, on Linux |
| Cloud credentials | The job holds them. With OIDC, it trades its token for temporary credentials with `sts:AssumeRoleWithWebIdentity` [aws] | The job holds none. `writ serve` assumes the environment's plan role for a plan, and its apply role for an apply that's approved, or overridden by someone with `override`. Roles are AWS roles only |
| The code that runs with them | The commit the job runs, which a merge request can change | The commit the job sends, fetched from the Git host and checked against Git blob ids. Module sources must pass an allow-list |
| State | GitLab-managed state on the `http` backend, "encrypted with the Lockbox Ruby gem when at rest". Reading it needs the Developer role, and `tofu apply` needs Maintainer or Owner [state] | The `http` backend that `writ serve` serves, sealed with AES-256-GCM and versioned, where a plan's token can only read. Or the stack's own `backend "s3"` |
| What gets approved | A merge request, or with deployment approvals in Premium and Ultimate, a deployment job [approvals] | A plan id: a SHA-256 over the masked plan, the saved plan, the commit, the stack, and the environment |
| Who can't approve | "By default, the user who triggers a deployment pipeline can't also approve the deployment job", and a project can allow it [approvals] | The plan's author, wherever an approval rule applies. No setting allows it |
| The plan that applies | The component's `fetch-mr-plan` "retrieves the approved plan artifact from the associated merge request pipeline". It works only without merge trains, with the merge method "merge commit with semi-linear history" [component] | The saved plan `writ serve` kept. Writ refuses it after a newer plan of the stack, a moved state, or a moved branch |
| Policy | An experimental OPA template that evaluates the plan in the pipeline [component] | Rego from the server's config, checked when the plan is created and again before the apply. A mandatory deny has no override |
| Drift | A `detect-drift` template that fails or warns the job on drift [component] | Scheduled drift checks in `writ serve`, shown in the control UI and the API. No alert |
| What the log shows | What `tofu` prints, in the job log | One mask over everything Writ stores or shows, tested by `just verify-mask`, with [three named gaps](secrets.md#what-the-mask-cant-do) |
| Audit | The pipeline and job history | A row in the control database's audit log before each answer, except health checks, server discovery, and rate-limited requests. A request fails if its row can't be written. Read with `GET /v1/audit` or `writ audit --remote`; no UI page yet ([Read the audit log](audit-log.md)) |

## What plain CI gives you that Writ doesn't

- **Nothing more to run.** Plain CI needs runners and GitLab. Writ also needs a host for `writ serve`, Postgres, a range of `provider_uids`, and a backup of each state key.
- **Use on a real GitLab.** The OpenTofu component runs in real pipelines. Writ's GitLab CI component has run only against a stand-in of GitLab.
- **Notes on the merge request.** The component can post the plan as a comment with `post_mr_plan_comment` [component]. Writ doesn't post anything to GitLab yet.
- **Single-node deployment.** A team's `writ serve` runs as one node on Linux.
- **Any cloud's credentials, the same way.** Writ's roles are AWS roles only. A provider for another cloud gets its credentials from files on the server.

## Which should you choose?

- **Choose Writ when:**
  - Merge requests and CI runners must never hold cloud credentials or state encryption keys.
  - Every apply must execute the exact sealed plan that was reviewed and approved, and authors must never approve their own plans.
  - You want mandatory OPA/Rego policies, secret masking, scheduled drift checks, and synchronous audit logging enforced on the server. See [Supported platforms and limits](status.md).
- **Choose plain CI when:**
  - The people who can merge are the people who can change the infrastructure directly, and you accept the CI job holding credentials and state access.

## Next steps

- [Writ compared with other control planes](compare.md): Atlantis, OTF, Terrakube, and the paid control planes, each with its sources and dates.
- [Quick start](quick-start.md): try Writ on your own, then set it up for a team.
- [Set up GitLab CI](set-up-gitlab-ci.md): Writ's GitLab CI jobs and the config they need.
- [Roles and credentials](serve.md#roles-and-credentials): how `writ serve` assumes AWS roles for a run.

[component]: https://gitlab.com/components/opentofu
[state]: https://docs.gitlab.com/user/infrastructure/iac/terraform_state/
[aws]: https://docs.gitlab.com/ci/cloud_services/aws/
[approvals]: https://docs.gitlab.com/ci/environments/deployment_approvals/
