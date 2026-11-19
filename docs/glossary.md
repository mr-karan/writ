# Glossary

Each term in Writ's docs has one meaning, listed here. Where Terraform or OpenTofu has a term for the same thing, the last column gives it.

| Term | Meaning | In Terraform |
| --- | --- | --- |
| stack | A directory of a repository that has `.tf` or `.tofu` files and a committed `.terraform.lock.hcl`, which `writ serve` plans with OpenTofu. Its name is its path under the repository's path, with the repository's prefix before it. See [OpenTofu stacks](serve.md#opentofu-stacks). | Root module |
| environment | A named target of the config, written `env "NAME" { ... }`: the key that seals its states and saved plans, its plan and apply roles, and the variables its providers get. A stack is planned in an environment, written `stack@env`, and each `stack@env` has its own state. | Workspace |
| resource | One object that a provider manages, declared as `resource "TYPE" "NAME" { ... }`. | Resource |
| object | The real thing that a provider created, such as a file or a security group. | |
| provider | A Terraform provider binary. OpenTofu runs it for a plan or an apply, and `writ import bulk` runs it through Writ's own client of the plugin protocol. | Provider |
| provider configuration | One `provider` block of OpenTofu code, identified by its address, such as `aws.west`. | Provider configuration, alias |
| provider uid | One of the config's `provider_uids`: the user that OpenTofu and its providers run as for one run, with no groups and no access to the keys or the database of `writ serve`. See [How providers are kept from the keys](serve.md#how-providers-are-kept-from-the-keys). | |
| address | OpenTofu's name of a resource in a stack, such as `random_pet.name` or `module.net.aws_subnet.a["eu"]`. | Resource address |
| variable | An input of a stack, declared with `variable`. One declared `sensitive = true` contains a secret. | Input variable |
| output | A value that a stack exposes, declared with `output`. | Output value |
| module | A directory of OpenTofu code that a stack calls with a `module` block. `writ serve` allows local paths, registry addresses, and remote addresses, and refuses every other source. | Module |
| annotation layer | Writ's table of facts that a provider schema doesn't have, which `writ import bulk` reads: the references between resource types, and the attributes a provider fills itself. | |
| secret | A value that the mask must keep out of everything `writ serve` stores or shows: a value OpenTofu marks sensitive, a variable declared `sensitive = true`, and a value of `provider_env`. See [Secrets and the mask](secrets.md). | Sensitive value |
| mask | The one package, `internal/mask`, that writes everything that leaves an OpenTofu run: the masked plan, the text and events of a plan and an apply, and the rows of the state index. It replaces each secret it knows or finds with `(sensitive)`. | |
| masked plan | The document of a plan that `writ serve` keeps and shows: OpenTofu's plan JSON after the mask, with only the keys the mask keeps. Policy checks it, and `writ plan --remote` prints it. | |
| secret set | The secrets that a run's mask knew, sealed with each state version or serial that its apply wrote, so that the mask of a later run of that state starts from them. A state with no secret set can't be approved until a person adopts it. | |
| seal | To encrypt with an environment's key: a state on the http backend, a saved plan, or a secret set. | |
| state | OpenTofu's record of the objects that a `stack@env` manages, in the stack's backend: `backend "http" {}`, which `writ serve` serves, or the stack's own `backend "s3"`. | State |
| state version | One state of a `stack@env` that the http backend stored. Each apply's write is a new version. | State version |
| serial | OpenTofu's number of a state, which each write raises. A stack on S3 identifies a state by its serial. | Serial |
| state token | The token a run gets for the http backend. A plan's token reads its stack's state and nothing else, and an apply's token also locks and writes it. It ends with its run, and `writ serve` keeps only its SHA-256. | |
| lock | OpenTofu's claim on a state while an apply writes it: the http backend's `LOCK` route, or the lock of the S3 backend. | State lock |
| state index | The rows of what each `stack@env`'s state manages, kept in the control database: each resource's address, type, and provider, and a few attributes that identify its object. Only the mask writes them. See [The state index](serve.md#the-state-index). | |
| row | One record of a table in the control database. | |
| plan | The changes that OpenTofu proposes for one commit of a stack in one environment, and the command that asks `writ serve` for them, `writ plan --remote`. | Plan |
| commit plan | A plan of the files of one commit, which the control plane fetched from the repository. | |
| speculative plan | A plan of files that a client sent, kept from before `writ serve` planned only commits. A team's control plane never applies one; `writ serve --dev` applies one that needs no approval. | Speculative plan |
| saved plan | OpenTofu's plan file of a plan, sealed under the environment's key. An apply runs `tofu apply` of it. No API route returns it. | Saved plan |
| risk class | What a change risks, which grants and approval rules list: `create`, `update`, `replace`, `delete`, or `data-loss`. In an OpenTofu plan, a forget counts as `delete` and an import as `create`, and no change carries `data-loss`. | |
| apply | The run of an approved saved plan on the control plane, and the command that asks for it, `writ apply --remote`. | Apply |
| run | One apply that `writ serve` started. `writ runs --remote` lists them. | Run |
| actor | Who started a run, as the run records it. | |
| audit log | The control database's rows of every request to `writ serve`, to the API, the control UI, or the state backend, and of every decision it makes on its own, with the stacks each refers to. `GET /healthz`, `GET /v1/server`, and requests that the limit on each client address refuses with 429 leave no row. | Audit trail |
| policy | Rego rules, in the policy sets that the config lists, that check each masked plan at the plan and again before its apply. See [Policy](policy.md). | Policy as code |
| policy set | A directory of `.rego` files that a `set` block of the config's `policy` block points to, `mandatory` or `advisory`. An advisory set's deny only warns. | Policy set |
| drift | A change to an object outside Writ. | Drift |
| drift check | A plan of an OpenTofu stack that `writ serve` creates on a timer, at the head of its default branch, to find drift and changes that no apply has made. It's never approved or applied. | Drift detection, health assessment |
| agent | A program driven by a language model that edits code and runs `writ`. | |
| control plane | The one `writ serve` process that plans for CI jobs that have no cloud credential and no state key, keeps the plans, and writes the audit log. See [How the control plane works](serve.md). | HCP Terraform, for remote runs |
| dev control plane | `writ serve --dev`: a control plane for one user, on loopback, with its store in SQLite. The remote commands find it with no flag. See [writ serve --dev](run.md#writ-serve---dev). | None |
| issuer | An OIDC token issuer that `writ serve` trusts, such as `https://gitlab.com`, with a kind: `gitlab-ci`, `github-actions`, or `people`. People sign in with an issuer of kind `people`. | |
| identity | The name an issuer's config gives the Git host or identity provider that the user ids in its tokens come from. Issuers of one Git host share it. See [Who a person is](serve.md#who-a-person-is). | |
| person id | `IDENTITY/user/ID`, such as `gitlab/user/1001`: the stable id of a person, which approvals compare. | |
| principal | What a verified token or a session speaks for: a CI job and the person who started it, or a signed-in person. | |
| session | A person's sign-in to `writ serve`, which lasts 12 hours by default. `writ serve` keeps only the SHA-256 of its token. A browser session's token lives in a cookie. | |
| control UI | The web pages that `writ serve` serves under `/ui/`. See [The control UI](sign-in.md#the-control-ui). | |
| team | A named rule over the claims of one issuer's tokens. A principal with a token that matches it is a member. | Team |
| grant | A rule that lets teams do some operations on some `stack@env`, and apply some risk classes. In its stack patterns, `*` matches within one part of a stack name, and a part `**` matches any number of parts. | Permission |
| repository | A GitLab project, or a local Git repository under `writ serve --dev`, that the control plane reads code from, with the path in it under which its stacks are. Short form "repo" in commands and the API. See [Repositories](repos.md). | VCS connection |
| forge | The Git host that hosts repositories, GitLab. The control plane reads it with a read-only token from a file the config points to. `forge` is also the config block. | VCS provider |
| approval | A person's record that they read one plan, by its id, and agree that it can be applied. The plan's author can't give one. See [Approvals](approvals.md). | Approval of a run |
| approval rule | A rule of the config that says how many approvals a plan of some stacks, environments, and risk classes needs. | Policy, in HCP Terraform |
| override | An apply of a plan without the approvals its rules need, by a person with `override`, with a reason that the run records. | |
| adoption | A person's `writ state adopt` of an OpenTofu state that has no secret set: `writ serve` seals with it each value the state marks sensitive, with a reason that the audit log records. See [Adopt a state](adopt-state.md). Taking existing objects into a stack is an import, never an adoption. | |
| import | Taking existing objects into a stack: `import` blocks in a commit, which `writ import bulk` can write, planned and applied like any other change. See [Import many objects](import-bulk.md). | Import |
| superseded | The status of a plan after its state or its branch moved. It can no longer be applied. | |
