# Set up GitLab CI

With GitLab CI and Writ, every merge request plans each affected stack, a manual job blocks the merge until someone other than the author approves every plan, and the default-branch pipeline applies those exact approved plans.

None of the jobs needs a Git checkout, cloud credentials, or state encryption keys. Each job authenticates with its GitLab OIDC token. See [How the control plane works](serve.md#how-a-remote-plan-runs) and [Approvals](approvals.md) for the server-side rules.

**Limits.** The CI templates are verified against a local GitLab stand-in (`TestTofuApproveApply`). See [the test plan](development.md#test-plan-for-a-real-gitlab-project) and [`ci/gitlab/README.md`](../ci/gitlab/README.md) for the OIDC claims (`user_access_level` and manual-job `user_id`) your GitLab instance must emit.

## Before you begin

- A team's `writ serve` with a `gitlab-ci` issuer ([Run a control plane](run.md)).
- Your GitLab project registered as a [repository](add-repository.md).

## Know the three jobs

The component in `ci/gitlab/templates` provides three templates, each using `GIT_STRATEGY: none`:

| Template | Job | Runs | Command |
| --- | --- | --- | --- |
| `writ-plan.yml` | `writ-plan-STACK-ENV` | Merge request pipelines | `writ plan --remote --commit "$CI_COMMIT_SHA"` |
| `writ-approve.yml` | `writ-approve` | Merge request pipelines (manual, blocking) | `writ approve --remote --pipeline` |
| `writ-apply.yml` | `writ-apply-STACK-ENV` | Push to default branch | `writ apply --remote --merged` |

See `ci/gitlab/example.gitlab-ci.yml` for a full example. Turn on **Pipelines must succeed** in GitLab so the manual `writ-approve` job blocks merge until every plan in the pipeline has its required approvals.

## Print the CI lines and the config

Run `writ ci gitlab --repo NAME` to generate both the `.gitlab-ci.yml` snippet and the `writ serve` teams, grants, and approval rules for that repository's numeric project ID:

```
$ writ ci gitlab --repo infra --image registry.example.com/writ:0.1.0
# .gitlab-ci.yml: add these lines. A merge request pipeline plans each stack in each
# environment, and its manual approve job blocks the merge until each plan has its
# approvals. The pipeline on the default branch applies each approved plan whose sources
# equal the merged commit's. No job needs a checkout.
stages: [writ-plan, writ-approve, writ-apply]
include:
  - component: $CI_SERVER_FQDN/platform/writ-ci/writ-plan@0.1.0
    inputs:
      server: http://127.0.0.1:40185
      env: prod
      stack: app
      image: registry.example.com/writ:0.1.0
  - component: $CI_SERVER_FQDN/platform/writ-ci/writ-approve@0.1.0
    inputs:
      server: http://127.0.0.1:40185
      image: registry.example.com/writ:0.1.0
  - component: $CI_SERVER_FQDN/platform/writ-ci/writ-apply@0.1.0
    inputs:
      server: http://127.0.0.1:40185
      env: prod
      stack: app
      image: registry.example.com/writ:0.1.0

# The writ serve config: the teams and grants of this repository's pipelines, by the
# project's id, 4242, which a renamed project keeps, and an approval rule.
team "infra_mr" {
  issuer = "gitlab"
  match  = { project_id = ["4242"], pipeline_source = ["merge_request_event"] }
}
team "infra_reviewers" {
  issuer = "gitlab"
  match  = { project_id = ["4242"], user_access_level = ["maintainer", "owner"] }
}
team "infra_main" {
  issuer = "gitlab"
  match  = { project_id = ["4242"], ref_path = ["refs/heads/main"], ref_protected = ["true"] }
}
grant {
  teams  = ["infra_mr"]
  stacks = ["app"]
  envs   = ["prod"]
  can    = ["read", "plan"]
}
grant {
  teams  = ["infra_reviewers"]
  stacks = ["app"]
  envs   = ["prod"]
  can    = ["approve"]
}
grant {
  teams  = ["infra_main"]
  stacks = ["app"]
  envs   = ["prod"]
  can    = ["read", "apply"]
  risks  = ["create", "update", "replace", "delete"]
}
approval {
  stacks = ["app"]
  envs   = ["prod"]
  risks  = ["create", "update", "replace", "delete", "data-loss"]
  need   = 1
}

# Project settings of acme/infra: protect main, and turn on "Pipelines must succeed".
```

Output of `TestTofuApproveApply`, 2026-11-16, against a team's `writ serve` on loopback.

1. Add the YAML lines to `.gitlab-ci.yml`.
2. Add the HCL blocks to `/etc/writ/serve` and restart `writ serve`.
3. Protect the default branch and enable **Pipelines must succeed** in GitLab.

## How a job gets its token

Each job requests an OIDC token whose `aud` matches `url` in `/etc/writ/serve`:

```yaml
id_tokens:
  WRIT_ID_TOKEN:
    aud: https://writ.example.com
```

The CLI reads `WRIT_SERVER` and `WRIT_ID_TOKEN` and sends tokens only over HTTPS or loopback HTTP.

## Pin the CLI to the server's version

Pin the CI image tag to the same release as `writ serve` (for example `0.1.8`) and upgrade both together. When the server is newer than the CLI, `writ` warns on stderr and exits with an error naming both versions if it receives an unexpected response. See [Version skew](commands.md#version-skew).

Use `writ plan --remote --json` for machine-readable output (`writ.remote-plan/v0`), or inspect past work with `writ show --remote ID` and `writ runs --remote`.

## Troubleshooting

- **`401 auth: audience: aud is not writ serve's URL`**: Match `id_tokens.WRIT_ID_TOKEN.aud` to `url` in `/etc/writ/serve` exactly (same scheme, no trailing slash).
- **`401 auth: this token already made a plan` (or `already applied`)**: Each CI OIDC token allows one write. Re-run the job so GitLab issues a fresh `jti`.
- **`403 denied` on a fork merge request**: Fork pipelines carry the fork's `project_id`, which doesn't match your `repo` or `team` rules. Writ refuses fork plans by design.
- **`403 denied` when the MR author runs `writ-approve`**: Authors can't approve their own plans whenever an approval rule applies. Have a teammate in `infra_reviewers` run the job or approve in the control UI.
- **No-op merge request**: The plan job records a `no-changes` plan, `writ-approve` passes with nothing to approve, and `writ-apply` exits 0 with nothing to apply. If another run changed the stack between plan and merge, Writ checks whether the newest run applied those same sources and wrote the current HTTP state version; otherwise it asks for a fresh plan. See [A plan that changes nothing](approvals.md#a-plan-that-changes-nothing).
- **Version skew warning or `which this writ cannot use`**: Update the CI image tag to match `writ serve`'s version.
- **Retried apply job**: Passes with no new run if the newest run already applied the merged sources and wrote the current HTTP state version. On S3 (or if state moved), Writ refuses with `state` and asks for a new plan. See [Apply after a merge](apply.md#apply-after-a-merge).
- **`saved-plan: no approved plan matches` on `main`**: The branch changed after the approved plan (for example via rebase). Plan the latest commit and approve the new plan.

## Next steps

- [Plan and apply](apply.md): approve, apply, recover lost responses, and override rules.
- [Approvals](approvals.md): approval rules, risk classes, and stale-plan checks.
- [Configure sign-in](sign-in.md): browser and CLI sign-in for people.
