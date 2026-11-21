# Use Writ with an agent

Coding agents can edit OpenTofu code, commit, request a remote plan, and inspect masked output without ever holding cloud credentials, state access, or approval authority.

This guide covers installing the Writ skill and connecting the `writ mcp` server.

## Set up a repository

Run `writ agent init` (targets `--dir`, default `.`) to install:

- `.agents/skills/writ/SKILL.md` and `reference.md` (a copy of [Writ reference for agents](agent-reference.md)).
- `.claude/skills/writ/SKILL.md` and `reference.md` (exact copies, not symlinks, because `writ serve` refuses repositories with symlinks).
- A marked section in `AGENTS.md` directing agents to commit, plan remotely, and never apply or approve.

```
$ writ agent init
appended AGENTS.md
created .agents/skills/writ/SKILL.md
created .agents/skills/writ/reference.md
created .claude/skills/writ/SKILL.md
created .claude/skills/writ/reference.md
$ writ agent init
unchanged AGENTS.md
unchanged .agents/skills/writ/SKILL.md
unchanged .agents/skills/writ/reference.md
unchanged .claude/skills/writ/SKILL.md
unchanged .claude/skills/writ/reference.md
```

Output of a real run, 2026-11-15.

### How it treats your files

- `AGENTS.md` updates only between Writ's marker lines, leaving surrounding bytes untouched. Malformed or duplicate markers return an error without editing the file.
- Modified skill files are kept with a warning unless you pass `--force`.
- `--print` outputs the generated files without touching disk.

### Symbolic links

`AGENTS.md` may be a symlink pointing inside `--dir` (regular file only); symlinks escaping `--dir` or absolute symlinks on `.agents` / `.claude` fail. `writ agent init` never writes through symlinks at `.agents/skills/writ` or `.claude/skills/writ` (use `--force` to replace the symlink itself). Remember that `writ serve` refuses any symlink committed to a repository.

## The loop the skill teaches

1. Check resource attributes with `schema` before editing `.tf` files.
2. Commit changes (Writ plans commits, never dirty working trees).
3. Call `plan` in the stack directory (passing `env` on a team control plane).
4. Read the masked plan. Fix errors, commit, and re-plan.
5. On a [policy](write-a-policy.md) deny, inspect the rule via `policy`, update the code to satisfy it, commit, and re-plan.
6. Return the plan ID, summary of changes, and required approvers. Stop there.

The skill prohibits running `tofu` / `terraform` locally and forbids applying or approving. Even if an agent ignores the skill, the control plane enforces the boundary: authors can't approve their own plans, and `writ mcp` exposes no approve or apply tool.

## The MCP server

`writ mcp` runs a stdio Model Context Protocol server for stacks under `--dir`, reusing standard Writ auth (`--server`, `WRIT_SERVER`, `writ login`, or `writ serve --dev`). See [Writ reference for agents](agent-reference.md#the-tools) for schemas.

| Tool | Purpose |
| --- | --- |
| `plan` | Requests and waits for a commit plan (policy verdict first) |
| `get_plan` | Reads a plan as text or `writ.remote-plan/v0` (optional `addresses` filter) |
| `list_plans` | Lists environment plans (`writ.remote-plans/v0`) with filters and cursor |
| `policy` | Returns server policy sets and `.rego` source (`writ.policy/v0`) |
| `runs` | Lists environment applies or fetches one masked apply run |
| `drift` | Lists OpenTofu stacks with index version, outside writes, and latest drift check |
| `state_index` | Lists managed resources for one stack from the state index |
| `schema` | Returns provider resource/data schemas from `.terraform.lock.hcl` + local cache |
| `import_accept` | Verifies a remote plan only imports the objects in an imports manifest |
| `import_bulk` | Generates `resource` + `import` blocks (enabled only with `--allow-providers`) |

Key guarantees:

- **Minimal mutation:** `plan` creates a remote plan; `import_bulk` creates two new files without overwriting. All other tools are read-only. No tool approves, applies, or mutates state.
- **One concurrent plan:** `writ mcp` allows only one in-flight `plan` call per process so an agent can't exhaust shared server slots.
- **Masked output only:** Every plan, run, drift check, and state index comes from Writ's masked server documents.
- **Policy visibility:** `plan` and `get_plan` move the stored policy verdict and results (`OUTCOME: [SET, LEVEL] RULE: MESSAGE`, denies first) to the top of text output so truncation never hides a deny.
- **Import safety:** `import_accept` checks remote plans (refusing in-place updates; those require `writ import accept` on a saved plan) and omits raw import IDs from refusals.

### Bounds

- Max tool response: `64 KiB` and `50` diagnostics. Oversized JSON documents fall back to a valid truncated JSON envelope keeping `format`, `ok`, up to `32 KiB` of `policy` results (`omitted_results`), `truncated: true`, and `message`.
- Plan text includes up to `20` policy results (denies first) plus a count of the rest.
- Per-call caps: `schema` (1 to 20 types, 50 field patterns), `get_plan` (50 addresses), `state_index` (20 types), `import_bulk` (1 to 100 IDs).

### Files under --dir

Tool `dir` paths must stay inside `--dir`. The files a tool reads, `.terraform.lock.hcl` for `schema` and the imports file for `import_accept`, are confined to `--dir`, so escaping symlinks and absolute symlinks fail. That check is on paths: a hard link or a bind mount under `--dir` to a file outside it is read. `schema` reads providers from `WRIT_CACHE_DIR` verified against `.terraform.lock.hcl` hashes without network downloads unless `--allow-providers` is passed. If the cache lacks a provider's schema, Writ starts the provider once, with no configuration, to report it, even without `--allow-providers`. `import_bulk` is exposed only with `--allow-providers` because it runs stack providers using `writ mcp`'s local credentials and network access.

### A plan from a work tree

Calling `plan` with `dir` plans the checked-out commit (Linux only, regular `.git` directory inside `--dir`). Pass `stack` and `commit` instead to skip reading local files.

`plan` with `dir` refuses:

- Linked worktrees or submodules (`.git` as a file), or repos with `.git/commondir` or `.git/objects/info/alternates`.
- Repositories outside `--dir` or unregistered on the control plane.
- Dirty working trees in the stack or its local modules ([What counts as a change](#what-counts-as-a-change)).
- Files marked `assume-unchanged` or `skip-worktree` in the Git index (clear with `git update-index --no-assume-unchanged --no-skip-worktree`).

Note: under `writ serve --dev` with a local repo added via `writ repo add`, the agent can commit locally, so a commit plan proves clean local files rather than peer review.

#### What counts as a change

Writ runs `git status` scoped to the stack directory and every local module directory referenced by its `.tf`/`.tofu` files. Any edit, untracked non-ignored file, deletion, mode change, symlink replacement, or moved submodule commit refuses the plan (showing up to three paths). Files outside those directories don't block planning.

#### How Writ runs git

`writ mcp` runs only `git config`, `git rev-parse`, `git ls-files`, `git status`, `git symbolic-ref`, and `git branch --contains`, with `core.fsmonitor=false`, `core.hooksPath=/dev/null`, optional locks disabled, clean/smudge/process filters neutralized (Writ lists the filter drivers and runs `git status` as two commands, so a config that changes between them isn't covered), no system/global config, no inherited `GIT_*` vars, `GIT_ALLOW_PROTOCOL=""`, `GIT_NO_LAZY_FETCH=1`, and `GIT_NO_REPLACE_OBJECTS=1`. (`writ serve --dev` uses the same isolation when reading local commits.)

**Limits:** `.git/info/grafts`, `.git/shallow`, and symlinks or `include.path` inside `.git/` aren't confined to `--dir`.

### Add it to Claude Code

```
$ claude mcp add --scope project writ -- writ mcp
Added stdio MCP server writ with command: writ mcp to project config
```

Output of Claude Code 2.1.318, 2026-11-10 (tools appear as `mcp__writ__plan`, `mcp__writ__get_plan`, etc.).

## Troubleshooting

- **`writ agent init` warns `kept .agents/skills/writ/...`**: Existing files differ from this version; run `writ agent init --force` to overwrite.
- **Second `plan` call refused**: `writ mcp` runs one plan at a time; wait for the active plan to finish.
- **`import_bulk` missing**: Pass `--allow-providers` to `writ mcp` only when local cloud reads are intended.
- **Symlink errors in `writ agent init`**: Remove symlinks under `.agents/skills/writ` or `.claude/skills/writ` (or pass `--force`) and avoid symlinks pointing outside `--dir`.

## Next steps

- [Writ reference for agents](agent-reference.md): exact tool inputs, outputs, and refusal recovery.
- [Write and test a policy](write-a-policy.md): enforce rules agents can't override.
- [Plan and apply](apply.md): review and apply agent-created plans.
