# Glossary

Each Writ term has one meaning across the docs, CLI, and UI. Where Terraform or OpenTofu uses another word, the last column lists it.

| Term | Meaning | In Terraform |
| --- | --- | --- |
| stack | A repository directory with `.tf` or `.tofu` files and a committed `.terraform.lock.hcl`. Its name is its path under the repo's `path`, with the repo's `prefix` prepended. See [What a stack can contain](add-repository.md#what-a-stack-can-contain). | Root module |
| environment | A target defined by `env "NAME" { ... }` with its own state sealing key, roles, `provider_env`, and `egress` list. Plans run against `stack@env`, and each `stack@env` has its own state. | Workspace |
| resource | A managed infrastructure declaration: `resource "TYPE" "NAME" { ... }`. | Resource |
| object | The real infrastructure item a provider manages, such as a file or security group. | |
| provider | A Terraform provider binary run by OpenTofu or by `writ import bulk`'s plugin protocol client. | Provider |
| provider configuration | A `provider` block identified by address, such as `aws.west`. | Provider configuration, alias |
| provider uid | A UID from `provider_uids` that OpenTofu and its providers run as, with no supplementary groups and no access to `writ serve`'s keys or database. See [Run isolation and egress](run-isolation.md). | |
| egress proxy | The proxy inside `writ serve` used when `egress_mode` is `report` or `enforce`. Each run executes in a loopback-only network namespace and reaches allowed hosts through this proxy. See [Egress](run-isolation.md#egress). | |
| address | OpenTofu's resource identifier in a stack, such as `random_pet.name` or `module.net.aws_subnet.a["eu"]`. | Resource address |
| variable | A stack input declared with `variable`. Variables with `sensitive = true` are secrets. | Input variable |
| output | A value exposed by a stack with `output`. | Output value |
| module | A directory of OpenTofu code called via a `module` block. `writ serve` allows local paths inside the repo, registry addresses, and vetted remote URLs. | Module |
| secret | Any value Writ's mask must strip: OpenTofu `sensitive` values, `sensitive = true` variables, `provider_env` values, and assumed STS credentials. See [Secrets and the mask](secrets.md). | Sensitive value |
| mask | Writ's single masking boundary over every output leaving an OpenTofu run (masked plans, logs, diagnostics, egress hosts, and state index rows), replacing secrets with `(sensitive)`. | |
| masked plan | OpenTofu's plan JSON after passing through the mask and key allow-list. Stored by `writ serve`, checked by Rego policy, and displayed by the CLI and UI. | |
| secret set | The secrets a run's mask knew, sealed alongside each state version or S3 serial so later runs mask older secrets. States without a secret set can't be approved until adopted. | |
| seal | Encrypt with an environment's 32-byte key (AES-256-GCM): used for `http`-backend states, saved plans, and secret sets. | |
| state | OpenTofu's record of objects managed by `stack@env`, stored in `writ serve`'s `backend "http" {}` or the stack's `backend "s3"`. | State |
| state version | One stored state snapshot for `stack@env` on the `http` backend. Each apply writes a new version. | State version |
| serial | OpenTofu's monotonic state counter, incremented on each write. Used to identify S3 states. | Serial |
| state token | A per-run token sent via HTTP Basic auth (`TF_HTTP_USERNAME=writ`) for the `http` state backend. Plan tokens are read-only; apply tokens can also lock and write. Revoked when the run ends; only its SHA-256 is stored. | |
| lock | OpenTofu's exclusive claim on state during an apply (`LOCK` on the `http` backend, or lockfile/DynamoDB on S3). | State lock |
| state index | Control-database rows recording each managed resource's address, type, provider, and allow-listed identifying attributes (`id`, `arn`, `identifier`, `region`, `namespace`, `modify_index`). Written only by the mask. See [The state index](plan-lifecycle.md#the-state-index). | |
| row | One record in a control database table. | |
| plan | The changes OpenTofu proposes for one commit of `stack@env`, or `writ plan --remote`. | Plan |
| commit plan | A plan generated from a Git commit fetched by `writ serve`. | |
| speculative plan | Legacy plan created from client-uploaded files. Team servers never approve or apply one. | Speculative plan |
| saved plan | Binary output of `tofu plan -out`, sealed with the environment key. Applied by `tofu apply`; never served by any API route. | Saved plan |
| risk class | Change category used by grants and approval rules: `create`, `update`, `replace`, `delete`, or `data-loss`. In OpenTofu plans, `forget` maps to `delete` and `import` adds `create`. `data-loss` is a valid class for grants and approval rules, but no OpenTofu change produces it today. | |
| apply | Running an approved saved plan on `writ serve`, or `writ apply --remote`. | Apply |
| run | One apply started by `writ serve`, listed by `writ runs --remote`. | Run |
| actor | The principal recorded as starting a run. | |
| audit log | Synchronous database log of every request to the API, control UI, and state backend, plus internal server events. Excludes `GET /healthz`, `GET /v1/server`, and per-address rate-limited 429s. | Audit trail |
| policy | Rego rules from `policy` blocks in server config, evaluated at plan creation and again before apply. See [Write and test policies](write-a-policy.md). | Policy as code |
| policy set | A directory of `.rego` files configured in a `policy` `set` block as `mandatory` or `advisory`. | Policy set |
| drift | A difference between real infrastructure and state or default-branch code. | Drift |
| drift check | A scheduled plan of a stack's default-branch head to detect outside and code drift. Never approved or applied. | Drift detection, health assessment |
| notifier | A `notify` block target (`slack` or `webhook`) that receives event notifications from `writ serve`. See [Get notified](notifications.md). | |
| agent | A language-model-driven program that edits code and calls `writ` or `writ mcp`. | |
| control plane | The `writ serve` server that plans commits, enforces grants and policies, applies approved plans, and writes the audit log. | HCP Terraform (remote runs) |
| dev control plane | `writ serve --dev`: single-user server on loopback with SQLite, auto-discovered by CLI commands. | |
| issuer | A trusted OIDC issuer configured in `issuer "NAME"`: `gitlab-ci`, `github-actions`, or `people`. | |
| identity | Config-defined name shared by issuers belonging to the same Git host or IdP, used to build stable person IDs. See [Who a person is](config.md#who-a-person-is). | |
| person id | `IDENTITY/user/ID` (for example `gitlab/user/1001`), compared by approval rules to prevent self-approval across CI, CLI, and UI. | |
| principal | The verified caller behind a token or session: a CI job (and its triggering user) or a signed-in person. | |
| session | A `writ login` (`cli`) or control UI (`browser`) sign-in, valid for `session_ttl` (12h default) with no refresh. Only the token's SHA-256 is stored. | |
| control UI | Built-in web interface served by `writ serve` under `/ui/`. See [The control UI](sign-in.md#the-control-ui). | |
| team | A named claim-matching rule on one issuer. Any token matching all claims in `match` belongs to the team. | Team |
| grant | A rule granting teams operations (`read`, `plan`, `approve`, `apply`, `override`, `adopt`, `admin`) and risk classes on matching `stack@env`. | Permission |
| repository | A GitLab project (or local Git dir under `--dev`) registered with `writ serve`, short form "repo". See [Add a repository](add-repository.md). | VCS connection |
| forge | A configured Git host (`forge` block, currently `gitlab` only) accessed with a read-only token file. | VCS provider |
| approval | A person's confirmation that one specific plan ID may apply. Plan authors can't approve their own plans. See [Approvals](approvals.md). | Approval of a run |
| approval rule | An `approval` config block setting `need` (1 to 10 approvals) for matching stacks, environments, and risk classes. | Policy (HCP Terraform) |
| override | Applying a plan without required approvals using an `override` grant and a recorded 10 to 500 character reason. Never bypasses mandatory Rego policies. | |
| adoption | `writ state adopt`: sealing a secret set for state Writ didn't write, with a recorded reason. Distinct from importing objects into code. See [Adopt existing state](adopt-state.md). | |
| import | Bringing existing objects under OpenTofu management via `import` blocks (often generated by `writ import bulk`). See [Import existing objects](import-bulk.md). | Import |
| superseded | Terminal plan status when state, the source branch, or a newer plan replaces it. | |
