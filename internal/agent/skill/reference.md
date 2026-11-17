# Writ reference for agents

Writ is a control plane for OpenTofu. A stack is a directory of `.tf` files with its `.terraform.lock.hcl`. Writ plans a commit of a stack in an environment, keeps the plan for people to approve, and applies an approved plan with credentials that no person or agent has.

When you work on an OpenTofu repository that Writ plans, you use the tools of `writ mcp`. This reference lists them, the documents they return, the policy that checks each plan, and how to recover from each refusal. The skill that `writ agent init` installs carries a copy of it as `reference.md`.

## The tools

`writ mcp` serves these tools on stdio. `dir` is a stack directory relative to the directory `writ mcp` serves. `env` defaults to `dev` on `writ serve --dev`, and a team's control plane needs it.

| Tool | Input | Returns | Changes |
| --- | --- | --- | --- |
| `plan` | `dir`, or `stack` and `commit` (and `repo`), `env`, `json` | The masked plan as text, its policy verdict first, with its id and approvals, or with `json: true` the `writ.remote-plan/v0` document | Asks the control plane for a plan. One at a time. Applies nothing |
| `get_plan` | `id`, `json`, `addresses` | The plan as text, its policy verdict first, or `writ.remote-plan/v0`. `addresses` keeps only those changes | Nothing |
| `policy` | `set`, `file` | `writ.policy/v0`: the policy sets and their `.rego` text. `set` keeps one set, and `file` one file of it | Nothing |
| `runs` | `env`, `stack`, `limit`, or `id` | `writ.remote-runs/v0`, or for `id` one run as `writ.remote-run/v0` | Nothing |
| `drift` | `env`, `stack`, `limit` | `writ.drift/v0` | Nothing |
| `state_index` | `stack`, `env`, `types` | `writ.state-index/v0` | Nothing |
| `schema` | `dir`, `types`, `fields`, `required_only` | `writ.provider-schema/v0` | Nothing. Reads Writ's provider cache |
| `import_accept` | `dir`, `imports`, `plan` | `writ.import-accept/v0` | Nothing |
| `import_bulk` | `dir`, `type`, `ids`, `provider`, `out`, `max_objects` | `writ.import-bulk/v0` | Writes two new files in `dir`. Listed only with `writ mcp --allow-providers` |

No tool applies, approves, or edits state. A result has at most 64 KiB and 50 diagnostics, and says so when it was cut.

## A plan

`plan` with `dir` plans the commit checked out in that directory. The work tree must be clean under the repository's path, and the commit must be in a repository the control plane reads: added with `writ repo add` under `writ serve --dev`, or pushed to a branch of the Git host the control plane polls. With `stack` and `commit`, it plans that commit and reads no file.

The text is a summary of the masked plan, then the plan's line:

```
OpenTofu 1.13.9 plan of pets@dev

  + random_pet.name (create)

Plan: 1 to add, 0 to change, 0 to destroy.
Plan d013404b1813f8b42b2678705e5a0eaa for dev (pets), by alice (dev), expires 2026-11-16T13:22:54Z.
It is a commit plan of repo at a62e9e28.
It needs no approval. Apply it with: writ apply --remote d013404b1813f8b42b2678705e5a0eaa
```

Output of `writ serve --dev`, 2026-11-15.

`writ.remote-plan/v0` has the plan's fields and, in `document`, the masked plan document:

- `plan.id` identifies the plan once it ends. `plan.ref` identifies it while it plans. `plan.status` is `planning`, `pending`, `approved`, `applying`, `applied`, `refused`, `failed`, `superseded`, or `expired`.
- `plan.approvals` has `need`, `approved_by`, and `teams_that_may_approve`. The author's approval never counts.
- `document` keeps OpenTofu's own keys: `format_version`, `terraform_version`, `resource_changes`, `resource_drift`, `output_changes`, `checks`. Each entry of `resource_changes` has `address`, `type`, and `change` with `actions`, `before`, `after`, `replace_paths`, and `importing` for an import. `variables` has only the variables' names.
- Wherever OpenTofu marks a value sensitive, and wherever a sensitive value appears elsewhere, the value is the string `(sensitive)`.
- `policy` is the policy's decision on the plan, or `null` when no policy checked it. See [Policy](#policy).

## Policy

The control plane can check every plan with policy sets written in Rego, which come from its config, never from a commit. A mandatory set's deny refuses the plan: its status is `refused`, and no approval, person, or flag can apply it. An advisory set's deny, and any `warn`, only warns. The recovery from a deny is a new commit that passes the rule.

The text of `plan` and `get_plan` starts with the verdict: `passed`, `passed with warnings`, or `denied`, then one line for each result, denies first, at most 20, as `OUTCOME: [SET, LEVEL] RULE: MESSAGE`:

```
Policy: denied, so this plan cannot be approved or applied, and nothing overrides a deny.
  deny: [pets, mandatory] deny_long_pet: random_pet.long: a pet name has at most 3 words

OpenTofu 1.13.9 plan of app@dev

  + random_pet.long (create)
 <- random_uuid.u (import)

Plan: 1 to import, 1 to add, 0 to change, 0 to destroy.
Plan 3bfba7e24b278285e32b8063bee56f89 for dev (app), by root (dev), is refused.
```

Output of `plan` from `TestTofuMCP`, 2026-11-16.

In `writ.remote-plan/v0`, `policy` is the decision: `outcome` (`pass`, `warn`, or `deny`), `point`, and `results`, each with `set`, `level`, `rule`, `outcome`, and `message`. A message went through Writ's mask. A document too long for a result keeps `policy`, with the results that fit and `omitted_results`.

A message can give the rule without the fix. Call `policy` with the set, and read the rule's conditions. A rule reads the masked plan document with OpenTofu's keys, such as `input.resource_changes` and each change's `type`, `change.actions`, and `change.after`. `policy` with `{"set": "pets"}`, from the same run:

```json
{"format":"writ.policy/v0","ok":true,"sets":[{"name":"pets","level":"mandatory","files":[{"name":"pets.rego","text":"package main\n\nimport rego.v1\n\ndeny_long_pet contains msg if {\n\tsome rc in input.resource_changes\n\trc.type == \"random_pet\"\n\t\"create\" in rc.change.actions\n\trc.change.after.length > 3\n\tmsg := sprintf(\"%s: a pet name has at most 3 words\", [rc.address])\n}\n"}]}],"diagnostics":[]}
```

## Drift, runs, and the state index

- `writ.drift/v0` lists each stack@env: `backend`, `resources` in its index, `indexed_version`, `outside_write` (a serial of a state on S3 that no apply of Writ wrote), and `check`, the newest drift check, with `status` and each `changes` entry found outside code.
- `writ.remote-runs/v0` lists runs with `id`, `plan`, `status`, `stack`, `actor`, `approved_by`, and times. `writ.remote-run/v0` adds `document`, `writ.tofu-apply/v0`: OpenTofu's `summary` counts and each resource's apply events.
- `writ.state-index/v0` lists `resources`: `address`, `type`, `provider`, and `id`, `arn`, `identifier`, `region`, `namespace`, `modify_index` where the state has them. It has no other attribute and no secret.

## Provider schemas

`schema` reads the providers that the stack's `.terraform.lock.hcl` pins, at the locked versions, from Writ's provider cache. `types` takes resource types such as `random_pet`, data sources such as `data.local_file`, and a provider's name, such as `random`, to list its types. Each type's `schema.block` has the shape `tofu providers schema -json` gives it:

```json
{"type":"random_pet","kind":"resource","provider":"registry.opentofu.org/hashicorp/random","schema":{"version":0,"block":{"attributes":{"length":{"type":"number","optional":true,"computed":true},"prefix":{"type":"string","optional":true}}}}}
```

An attribute's `type` is cty's type in JSON, such as `"string"` or `["map","string"]`. A nested block is under `block_types` with its `nesting_mode`. The cache keeps no descriptions.

## Import

`import_bulk` reads objects through the stack's providers and writes `imported.tf`, with one `resource` block and one `import` block for each group of objects, and `imported.imports.json`, the objects selected. Commit both files, plan the commit, and call `import_accept` with the plan's id. It passes when every change is a no-op and the plan imports exactly the selected objects, each once, at its address by its import id. A plan that also updates an imported object is refused by `import_accept`. A person checks that case with `writ import accept` and the saved plan.

## Refusals and what to do

| The result says | Do |
| --- | --- |
| `writ serve plans only by commit ... the work tree differs from commit` | Commit the files it lists, then plan again |
| `which the index marks assume-unchanged or skip-worktree` | Ask the user: the marks hide an edit to the files it lists from `git status`. Plan again once they clear the marks and commit the edit |
| `is not a repository of this control plane` | Ask the user to add it, with `writ repo add` on `writ serve --dev` |
| `is on no remote branch; push it` | Push the branch, or ask the user to |
| `env is required` | Pass `env`. `drift` lists each stack's environments |
| `no control plane` | Ask the user to start `writ serve --dev` or to sign in with `writ login URL`. Don't start one yourself |
| `Policy: denied` | Call `policy` with the set each deny shows, read the rule, change the code so it passes and keeps the user's change, commit, plan again. Nothing overrides a deny |
| A plan with status `refused` or `failed` | Read its text, fix the code, commit, plan again |
| `no provider that .terraform.lock.hcl pins has` | The stack needs the provider in `required_providers` and its lock. Ask the user to run `tofu init` and commit the lock |
| `is not in the provider cache` | Ask the person who runs `writ mcp` to fill Writ's provider cache, or to add `--allow-providers` |
| `import_accept` refuses an address | The code changes that object. Edit the code to match it, commit, plan, and check again |
