# Writ compared with other control planes

Here's where Writ differs from the control planes you may already know: three open-source ones and four paid ones. Each fact comes from the tool's own pages, with its source and the date it was read. The last section says what those tools have that Writ doesn't.

No product was run for this page. "Not found" and "Not stated" mean the pages read didn't say, not that the tool lacks it. If you run OpenTofu from GitLab CI today, read [Writ compared with OpenTofu in plain GitLab CI](compare-gitlab-ci.md) instead.

## Among the open-source control planes

Four rows where Writ differs from Atlantis, OTF, and Terrakube, from each project's docs, read on 2026-11-15.

| | Atlantis | OTF | Terrakube | Writ |
| --- | --- | --- | --- | --- |
| Approval bound to the plan, by default | Checks the Git host's approval when apply runs. Its docs say it doesn't check whether the approval came after the latest plan, unless `--discard-approval-on-plan` or branch protection is set | No approval rule found in its docs | An approval step sets a team. Binding to the saved plan is opt-in, with `requireSavedPlan: true` | Yes. The approval is for the plan id, and the author's doesn't count |
| Secret mask, with a test | No masking setting in its server configuration | The UI hides sensitive outputs. No masking of plan output found | Fixed a leak of sensitive attributes in structured output on 2026-08-15. No masking page found | One mask over everything stored or shown, and a differential test |
| Run sandbox | Runs on the server, with the host's credentials | A server or agent process, or an optional Kubernetes job | An executor, agents, or ephemeral Kubernetes jobs | Each `tofu` process in its own filesystem view on Linux; on a team's server, also its own uid from `provider_uids` |
| Console to code | Not found | Not found | Not found | `writ import bulk` |

Sources: Atlantis's [command requirements](https://www.runatlantis.io/docs/command-requirements), [server configuration](https://www.runatlantis.io/docs/server-configuration), and [security](https://www.runatlantis.io/docs/security). OTF's [runners](https://docs.otf.ninja/runners/), [flags](https://docs.otf.ninja/config/flags/), and [issue 809](https://github.com/leg100/otf/issues/809). Terrakube's [README](https://github.com/terrakube-io/terrakube), [templates](https://docs.terrakube.io/user-guide/organizations/templates), and [pull request 3406](https://github.com/terrakube-io/terrakube/pull/3406).

They also have what Writ lacks. All three work with GitHub as well as GitLab, and Writ works with GitLab only. Each has years of use, and Writ had its first release on 2026-11-17.

## Among the paid control planes

Spacelift, HCP Terraform, env zero, and Scalr are paid control planes for the same job. Writ does the part of that job that decides what can change your infrastructure: open source, on your own host, with no meter. Each vendor cell links the page it comes from, read on 2026-11-16.

| | Spacelift | HCP Terraform | env zero | Scalr | Writ |
| --- | --- | --- | --- | --- | --- |
| License and hosting | Closed source, as SaaS. Self-hosted is "Available upon request" with Enterprise ([pricing][sl-pricing]) | Closed source, as SaaS. Terraform Enterprise is self-managed, at "Custom Pricing" ([pricing][h-pricing]) | Closed source, as SaaS. Self-hosted agents run the deployments ([self-hosted agents][e-agents]) | Closed source, as SaaS. Self-hosted agents at no extra charge ([pricing][sc-pricing]) | Apache 2.0. You run it on your own host. No hosted service ([README](../README.md)) |
| Price | Free for 2 users. Starter is "Starting at $399 / mo" ([pricing][sl-pricing]) | Free to 500 managed resources. Then from "$0.10 per month per resource" ([plans][h-overview], [pricing][h-pricing]) | Free for 250 runs a month. Paid plans are "Custom" ([pricing][e-pricing]) | Free up to 50 runs a month, then $0.99 a run ([pricing][sc-pricing]) | None. No limit on users, runs, or resources ([README](../README.md)) |
| What is metered | Not stated. Plans differ in users and workers ([pricing][sl-pricing]) | Managed resources ([pricing][h-pricing]) | Runs, on the Free tier. Paid plans have "no per-resource (RUM) pricing" ([pricing][e-pricing]) | Runs. Plans and applies both count ([pricing][sc-pricing]) | Nothing |
| OpenTofu | Yes. A founding member of OpenTofu ([OpenTofu page][sl-opentofu]) | Runs Terraform. The pages read don't mention OpenTofu ([product page][h-product]) | Yes. A founding member of OpenTofu ([home page][e-home]) | Yes, and Terraform ([docs][sc-docs]) | OpenTofu 1.13.9 only. No Terraform ([status](status.md)) |
| What an approval names | A run that waits as "Unconfirmed". Reviews attach to the run ([tracked runs][sl-tracked], [approval policy][sl-approval]) | A run in "Needs Confirmation". The apply "uses that plan's output" ([run states][h-states], [remote operations][h-remote]) | A deployment whose approval is `pending` ([approval policies][e-approval]) | A run that waits for approval before its apply ([runs][sc-runs]) | A plan id: a hash of the masked plan, the saved plan, the commit, the stack, and the environment ([approvals](approvals.md#what-the-plan-id-covers)) |
| Who can't approve | Anyone with write access can, by default. A rule you write can exclude the person who triggered the run ([approval policy][sl-approval]) | Not stated. "a user with permission to apply runs" confirms ([run states][h-states]) | Not stated. Approvers are set by role or by Rego ([approval policies][e-approval]) | Not stated. Users with `runs:apply` approve ([runs][sc-runs]) | The plan's author, wherever an approval rule applies. No policy to write ([approvals](approvals.md#who-can-approve-a-plan)) |
| Policy | OPA, from the Starter plan ([pricing][sl-pricing], [home page][sl-home]) | Sentinel and OPA, in Standard and Premium. Free has one policy set of up to five policies ([plans][h-overview]) | OPA approval policies ([approval policies][e-approval]) | OPA and Checkov. Policy enforcement is on all plans ([home page][sc-home], [pricing][sc-pricing]) | OPA, at the plan and again before the apply. A mandatory deny has no override ([policy](policy.md)) |
| Drift checks | Business plan and up ([pricing][sl-pricing]) | Standard and Premium, as health assessments ([health assessments][h-health]) | Basic drift management in the Free tier, where a drift run counts toward its 250 runs ([pricing][e-pricing]) | All plans. Scheduled drift runs aren't billed. Runs that fix drift are ([pricing][sc-pricing]) | On a schedule, shown in the control UI and the API. No alert ([drift checks](serve.md#drift-checks)) |
| Audit log | Enterprise plan ([pricing][sl-pricing]) | Premium plan ([plans][h-overview]) | Listed. The plan that has it could not be read from the page ([pricing][e-pricing]) | Enterprise plan ([home page][sc-home]) | A row before each answer, in your own database, except health checks, server discovery, and rate-limited requests. A request fails if its row can't be written. Read with `GET /v1/audit` or `writ audit --remote`; no UI page yet ([Read the audit log](audit-log.md)) |
| Where a run executes | Spacelift's workers, or your private workers ([worker pools][sl-workers]) | HashiCorp's environments, or your agents ([remote operations][h-remote]) | env zero's runners, or your self-hosted agents ([self-hosted agents][e-agents]) | Scalr's runners, or your agents ([pricing][sc-pricing]) | On your host, as its own uid in its own namespaces. Linux, one node ([OpenTofu stacks](serve.md#opentofu-stacks)) |

## What these tools have that Writ doesn't

A hosted service you don't run. Support, which Terraform Enterprise includes ([pricing][h-pricing]), and compliance reports, such as Spacelift's SOC 2 Type II ([security][sl-security]). Terraform as well as OpenTofu. Notifications, which Spacelift sends from policy ([home page][sl-home]). Cost estimates before an approval ([env zero][e-approval]). Self-service templates, such as Spacelift's Blueprints ([pricing][sl-pricing]). Years of use: Scalr says "1,000+ teams made the switch" ([home page][sc-home]).

Writ has none of these. It works with GitLab only, runs on one node, and is at version 0. If you need Terraform, GitHub, a hosted service, or support today, choose one of them.

## Next steps

- [Writ compared with OpenTofu in plain GitLab CI](compare-gitlab-ci.md): what Writ adds to a pipeline that runs `tofu` itself, and what it costs.
- [Status and limits](status.md): what's built, how it's tested, and what's missing.
- [Quick start](quick-start.md): try Writ on your own machine, with no cloud account.

[sl-pricing]: https://spacelift.io/pricing
[sl-home]: https://spacelift.io/
[sl-opentofu]: https://spacelift.io/opentofu
[sl-approval]: https://docs.spacelift.io/concepts/policy/approval-policy
[sl-tracked]: https://docs.spacelift.io/concepts/run/tracked
[sl-workers]: https://docs.spacelift.io/concepts/worker-pools
[sl-security]: https://docs.spacelift.io/product/security
[h-pricing]: https://www.hashicorp.com/en/pricing
[h-product]: https://www.hashicorp.com/en/products/terraform
[h-overview]: https://developer.hashicorp.com/terraform/cloud-docs/overview
[h-states]: https://developer.hashicorp.com/terraform/cloud-docs/run/states
[h-remote]: https://developer.hashicorp.com/terraform/cloud-docs/run/remote-operations
[h-health]: https://developer.hashicorp.com/terraform/cloud-docs/workspaces/health
[e-home]: https://www.envzero.com/
[e-pricing]: https://www.envzero.com/pricing
[e-approval]: https://docs.envzero.com/guides/policies-governance/approval-policies
[e-agents]: https://docs.envzero.com/guides/admin-guide/self-hosted-kubernetes-agent/overview.md
[sc-home]: https://scalr.com/
[sc-pricing]: https://scalr.com/pricing
[sc-docs]: https://docs.scalr.io/docs/introduction
[sc-runs]: https://docs.scalr.io/docs/workspaces-runs
