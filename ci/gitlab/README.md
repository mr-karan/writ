# GitLab CI/CD components for writ serve

Three templates, one component each, give the merge request flow of [Approvals and applies](../../docs/set-up-gitlab-ci.md):

- `templates/writ-plan.yml` adds `writ-plan-STACK-ENV` to merge request pipelines. The job asks GitLab for an OIDC token with writ serve's URL as its audience and names its commit, with no checkout. writ serve takes the project from the token, fetches the commit's estate files with its read-only token, plans them with state opened read-only, and keeps the plan.
- `templates/writ-approve.yml` adds `writ-approve`, a manual job of merge request pipelines with `allow_failure: false`, so the pipeline waits at it. Its `writ approve --remote --pipeline` approves every plan of the pipeline as the person who runs it. writ serve refuses the approval of the plans' author, and passes the job once each plan has its approvals, whoever gave them.
- `templates/writ-apply.yml` adds `writ-apply-STACK-ENV` to pipelines of pushes to the default branch. Its `writ apply --remote --merged` names no plan: writ serve applies the approved plan whose sources of the stack equal the merged commit's, and refuses when there is none.

No job holds a cloud credential, a state key, or a source. `example.gitlab-ci.yml` includes all three, and `writ ci gitlab --repo NAME` prints the lines for a repository writ serve reads, with the teams, grants, and approval rule of its config. See [Repositories](../../docs/repos.md).

These are sketches. No GitLab pipeline has run them. `just lint-gitlab-ci` checks them as YAML with yamllint. GitLab's own CI lint needs a GitLab instance, which no test here contacts.

## Not verified yet

Each item needs a real GitLab project. The [test plan](../../docs/development.md#test-plan-for-a-real-gitlab-project) in `docs/development.md` says how to check it.

- GitLab interpolates `$[[ inputs.stack ]]` and `$[[ inputs.env ]]` in a job name, and takes a stack name with `/` there.
- A job's token carries every claim writ serve requires, as strings, including `ref_protected` as `"true"` or `"false"`, and its `sha` is `$CI_COMMIT_SHA`.
- The values of `user_access_level`.
- In a manual job, the token's `user_id` and `user_login` name the person who ran the job, not the one who started the pipeline. The approve job is a person's approval only if they do.
- A blocking manual job keeps a merge request from merging under "Pipelines must succeed", and the `pipeline_id` of the approve job's token equals that of the plan jobs' tokens in the same pipeline.
- The apply job's token has `ref_path` `refs/heads/main` and `ref_protected` `"true"` on a push to a protected default branch.
- `GIT_STRATEGY: none` leaves the job with no checkout, and the image needs only the `writ` binary.
