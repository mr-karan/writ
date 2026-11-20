# Set up GitLab CI

With GitLab CI and Writ, every merge request shows the exact plan it would apply, and nothing reaches production until someone other than the author approves that plan. It takes three jobs:

- a merge request pipeline plans each stack in each environment,
- a manual job blocks the merge until each plan has its approvals,
- and the pipeline on the default branch applies each approved plan.

None of your jobs needs a checkout, and none holds a cloud credential or the state key. Each job proves who it is with the OIDC token GitLab issues. To see what happens to each plan, read [How the control plane works](serve.md#how-a-remote-plan-runs). To see who can approve it, read [Approvals](approvals.md).

**Limits.** The components are verified against a local GitLab stand-in (`TestTofuApproveApply`). See [the test plan](development.md#test-plan-for-a-real-gitlab-project) and [`ci/gitlab/README.md`](../ci/gitlab/README.md) for the OIDC claims (`user_access_level` and manual job `user_id`) your GitLab instance must supply.

## Before you begin

- A team's `writ serve`, as in [Run a control plane](run.md), with an issuer of kind `gitlab-ci`.
- Your project, added as a [repository](add-repository.md).

## Know the three jobs

You get three templates from the component in `ci/gitlab`, in `ci/gitlab/templates`, each with `GIT_STRATEGY: none`:

| Template | Job | Runs | Does |
| --- | --- | --- | --- |
| `writ-plan.yml` | `writ-plan-STACK-ENV` | In each merge request pipeline | `writ plan --remote --commit "$CI_COMMIT_SHA"` |
| `writ-approve.yml` | `writ-approve` | In each merge request pipeline, manual, blocking | `writ approve --remote --pipeline` |
| `writ-apply.yml` | `writ-apply-STACK-ENV` | On a push to the default branch | `writ apply --remote --merged` |

For example, `ci/gitlab/templates/writ-plan.yml` is a CI/CD component that adds a `writ-plan-STACK-ENV` job to merge request pipelines, and `ci/gitlab/example.gitlab-ci.yml` includes it. That job runs `writ plan --remote --stack STACK --commit "$CI_COMMIT_SHA"`, and `writ serve` fetches the commit from the [repository](repos.md) whose project id is the token's.

Turn on "Pipelines must succeed". Then the blocking approve job keeps your merge request from merging until someone runs it, and it passes only when each plan of the pipeline has its approvals.

## Print the CI lines and the config

You don't have to write the jobs or the config by hand. Run `writ ci gitlab --repo NAME`, and it prints the `.gitlab-ci.yml` lines and the teams, grants, and approval rule your config needs, by the project's id. You get a plan job and an apply job for each stack and environment the control plane serves, and one blocking approve job:

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

Then:

1. Add the lines to `.gitlab-ci.yml`.
2. Add the blocks to your `writ serve` config.
3. Set up the project as the last comment says.

## How a job gets its token

Your job's token comes from `id_tokens`:

```yaml
id_tokens:
  WRIT_ID_TOKEN:
    aud: https://writ.example.com
```

## Pin the CLI to the server's version

Use the image of the `writ serve` version you run, and change both together. A CLI older than the server can meet an answer it doesn't know. `writ` 0.1.5 met the 200 that `writ serve` 0.1.7 sends when a merged apply has nothing to apply, and its apply job failed. Today's `writ` warns on stderr when the server is newer, and fails with an error that names both versions when it gets an answer it can't use. See [Version skew](commands.md#version-skew).

From there, the client reads `WRIT_SERVER` and `WRIT_ID_TOKEN`. It sends a token only over HTTPS, or over HTTP to a loopback address.

To see what a job did, use `writ plan --remote --json`, which prints the `writ.remote-plan/v0` document. You can also run `writ show --remote ID` and `writ runs --remote` to print what the control plane keeps.

## Troubleshooting

- **`401 auth: audience: aud is not writ serve's URL`**: Make sure `id_tokens.WRIT_ID_TOKEN.aud` matches `url` in `/etc/writ/serve` exactly, including scheme and no trailing slash.
- **`401 auth: this token already made a plan` (or `already applied`)**: Each CI OIDC token can perform only one write (plan, approve, or apply). Re-run the job so GitLab issues a fresh token with a new `jti`.
- **`403 denied` on merge request from a fork**: Fork merge requests run under the fork's `project_id`, which does not match your `repo` or `team` rules. Writ refuses fork plans by design.
- **Approve job fails with `403 denied` when the MR author runs it**: Wherever an approval rule applies, the plan's author cannot approve it. Ask a teammate in `infra_reviewers` to run the manual approve job or approve in the control UI.
- **A merge request changes no resource**: Its plan job ends with a `no-changes` plan, its approve job passes with nothing to approve, and after the merge the apply job prints that there is nothing to apply and passes. If a run applied to the stack between the plan and the merge, the apply job fails with `state` and asks you to plan again. See [A plan that changes nothing](approvals.md#a-plan-that-changes-nothing).
- **A job prints `writ: warning: writ serve is ... and this writ is ...`, or fails with `which this writ cannot use`**: The job's `writ` is older than the server. Use the image of the server's version. See [Pin the CLI to the server's version](#pin-the-cli-to-the-servers-version).
- **A retried apply job**: When the newest run on the stack applied the merged commit's sources, the retry prints that there is nothing to apply and passes. If the state moved since, it fails with `state` and asks you to plan again. See [Apply after a merge](apply.md#apply-after-a-merge).
- **Apply job on `main` fails with `saved-plan: no approved plan matches`**: The merge request was rebased or edited after the last approved plan. Re-run the plan job and approve the new plan, or plan the merged commit and approve it.

## Next steps

- [Plan and apply](apply.md): what the approve and apply jobs do, and how to recover when an answer is lost.
- [Configure approvals](approvals.md): customize approval counts by stack, environment, and risk class.
- [Configure sign-in](sign-in.md): sign-in for people.
