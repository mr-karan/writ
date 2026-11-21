# Writ reference for agents

Writ is an OpenTofu control plane. A stack is a directory of `.tf` files with a committed `.terraform.lock.hcl`. Writ plans commits remotely, holds plans for human approval, and applies approved plans with server-side credentials no person or agent holds.

Use `writ mcp` tools when working in a Writ-managed repository. `writ agent init` installs a copy of this page as `reference.md`.

## The tools

`writ mcp` serves stdio MCP tools for stacks under its `--dir`. `dir` is relative to `--dir`. `env` defaults to `dev` on `writ serve --dev` and is required on a team control plane.

| Tool | Input | Returns | Side effects |
| --- | --- | --- | --- |
| `plan` | `dir`, or `stack` + `commit` (and `repo`), `env`, `json` | Masked plan text (policy verdict first, ID, approvals) or `writ.remote-plan/v0` | Creates a remote plan (one at a time); never applies |
| `get_plan` | `id`, `json`, `addresses` | Plan text (policy verdict first) or `writ.remote-plan/v0` filtered to `addresses` | Read-only |
| `list_plans` | `env`, `stack`, `status`, `started_by`, `limit`, `cursor` | `writ.remote-plans/v0` newest first, plus `next` cursor | Read-only |
| `policy` | `set`, `file` | `writ.policy/v0` (all sets, one `set`, or one `file`) | Read-only |
| `runs` | `env`, `stack`, `limit`, or `id` | `writ.remote-runs/v0` or one `writ.remote-run/v0` | Read-only |
| `drift` | `env`, `stack`, `limit` | `writ.drift/v0` | Read-only |
| `state_index` | `stack`, `env`, `types` | `writ.state-index/v0` | Read-only |
| `schema` | `dir`, `types`, `fields`, `required_only` | `writ.provider-schema/v0` from Writ's provider cache | Read-only |
| `import_accept` | `dir`, `imports`, `plan` | `writ.import-accept/v0` | Read-only |
| `import_bulk` | `dir`, `type`, `ids`, `provider`, `out`, `max_objects` | `writ.import-bulk/v0` | Writes two new files in `dir` (only with `--allow-providers`) |

No tool approves, applies, or writes state. Results cap at `64 KiB` and `50` diagnostics (truncation is noted explicitly).

## A plan

`plan` with `dir` plans the checked-out commit (working tree must be clean under the stack and its local modules, and the repo must be registered on the control plane). With `stack` and `commit`, it plans directly without reading local files.

Example plan text:

```
OpenTofu 1.13.9 plan of pets@dev

  + random_pet.name (create)

Plan: 1 to add, 0 to change, 0 to destroy.
Plan d013404b1813f8b42b2678705e5a0eaa for dev (pets), by alice (dev), expires 2026-11-16T13:22:54Z.
It is a commit plan of repo at a62e9e28.
It needs no approval. Apply it with: writ apply --remote d013404b1813f8b42b2678705e5a0eaa
```

Output of `writ serve --dev`, 2026-11-15.

Key fields in `writ.remote-plan/v0`:

- `plan.id` (final ID), `plan.ref` (while planning), `plan.status` (`planning`, `pending`, `approved`, `applying`, `applied`, `no-changes`, `refused`, `failed`, `superseded`, `expired`). `no-changes` means nothing to approve or apply.
- `plan.started_by`: `person` or `ci` (with `plan.origin`).
- `plan.approvals`: `need`, `approved_by`, `teams_that_may_approve`. The author's approval never counts.
- `document`: masked OpenTofu plan (`format_version`, `terraform_version`, `resource_changes`, `resource_drift`, `output_changes`, `checks`; `variables` has names only). Sensitive values appear as `(sensitive)`.
- `policy`: policy decision or `null` ([Policy](#policy)).

`list_plans` returns `writ.remote-plans/v0` (`plans` and `next`). `started_by` accepts `person` or `ci` (use `drift` for drift checks).

## Policy

Server-configured Rego policies check every plan. A mandatory deny sets status to `refused` with no override; fix the code, commit, and re-plan. Advisory denies and `warn` rules only warn.

`plan` and `get_plan` prepend the verdict (`passed`, `passed with warnings`, or `denied`) and up to 20 results (`OUTCOME: [SET, LEVEL] RULE: MESSAGE`, denies first):

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

In `writ.remote-plan/v0`, `policy` holds `outcome` (`pass`, `warn`, `deny`), `point`, and `results` (`set`, `level`, `rule`, `outcome`, `message`, all masked). Oversized truncated JSON responses still preserve `policy` (up to `32 KiB` plus `omitted_results`).

When a deny message doesn't state the full condition, call `policy` with the set name to read the `.rego` source:

```json
{"format":"writ.policy/v0","ok":true,"sets":[{"name":"pets","level":"mandatory","files":[{"name":"pets.rego","text":"package main\n\nimport rego.v1\n\ndeny_long_pet contains msg if {\n\tsome rc in input.resource_changes\n\trc.type == \"random_pet\"\n\t\"create\" in rc.change.actions\n\trc.change.after.length > 3\n\tmsg := sprintf(\"%s: a pet name has at most 3 words\", [rc.address])\n}\n"}]}],"diagnostics":[]}
```

## Drift, runs, and the state index

- `writ.drift/v0`: per `stack@env` `backend`, `resources`, `indexed_version`, `outside_write`, `check` (`started_by: "drift"`, `status`, `changes`), and schedule `every` (or `""`).
- `writ.remote-runs/v0` / `writ.remote-run/v0`: apply runs (`id`, `plan`, `status`, `stack`, `actor`, `approved_by`, timestamps, and `document` `writ.tofu-apply/v0` with `summary` and masked `events`).
- `writ.state-index/v0`: `resources` (`address`, `type`, `provider`, plus `id`, `arn`, `identifier`, `region`, `namespace`, `modify_index` when present; no secrets).

## Provider schemas

`schema` reads locked provider versions from Writ's cache. `types` accepts resource types (`random_pet`), data sources (`data.local_file`), or provider names (`random`) to list types:

```json
{"type":"random_pet","kind":"resource","provider":"registry.opentofu.org/hashicorp/random","schema":{"version":0,"block":{"attributes":{"length":{"type":"number","optional":true,"computed":true},"prefix":{"type":"string","optional":true}}}}}
```

Attribute `type` uses cty JSON notation (`"string"`, `["map","string"]`). Nested blocks sit under `block_types`. Descriptions aren't cached.

## Import

`import_bulk` generates `imported.tf` and `imported.imports.json`. Commit both, plan the commit, and call `import_accept` with the plan ID. It passes only when every change is a no-op and the plan imports the exact selected objects at their exact addresses and import IDs. Plans with provider-only in-place updates are refused here and must be checked by a human with `writ import accept` on a saved plan file.

## Refusals and what to do

| Refusal | Action |
| --- | --- |
| `writ serve plans only by commit ... the work tree differs from commit` | Commit the listed files and plan again |
| `which the index marks assume-unchanged or skip-worktree` | Ask the user to clear the Git index flags and commit |
| `is not a repository of this control plane` | Ask the user to register it (`writ repo add` on `--dev`) |
| `is on no remote branch; push it` | Push the branch or ask the user to |
| `env is required` | Pass `env` (check `drift` for stack environments) |
| `no control plane` | Ask the user to start `writ serve --dev` or run `writ login URL`; don't start one yourself |
| `Policy: denied` | Call `policy` for the denying set, update code to satisfy it while keeping the requested change, commit, and re-plan |
| Plan status `refused` or `failed` | Read diagnostics, fix code, commit, and re-plan |
| `no provider that .terraform.lock.hcl pins has` | Ask the user to add the provider, run `tofu init`, and commit `.terraform.lock.hcl` |
| `is not in the provider cache` | Ask the operator to populate Writ's cache or pass `--allow-providers` |
| `import_accept` refuses an address | Code differs from live object; update code, commit, re-plan, and re-check |
