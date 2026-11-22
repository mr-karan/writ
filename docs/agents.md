# Use Writ with an agent

Let a coding agent edit OpenTofu code, commit, request a remote plan, and read masked output, without cloud credentials, state access, or approval authority. Tool bounds and work-tree rules are in [How writ mcp works](how-writ-mcp-works.md).

## Before you begin

- A repository registered with a control plane ([Add a repository](add-repository.md)), or `writ serve --dev` with a local repository.
- `writ` on `PATH`, signed in with `writ login` or able to find `writ serve --dev`.

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
```

Real run, 2026-11-15. Running it again reports each file `unchanged`.

`writ agent init` edits `AGENTS.md` only between its marker lines, keeps skill files you changed unless you pass `--force`, and never writes through a symlink ([How writ agent init treats your files](how-writ-mcp-works.md#how-writ-agent-init-treats-your-files)). `--print` shows the files without writing them.

## The loop the skill teaches

1. Check resource attributes with `schema` before editing `.tf` files.
2. Commit changes (Writ plans commits, never dirty working trees).
3. Call `plan` in the stack directory (passing `env` on a team control plane).
4. Read the masked plan. Fix errors, commit, and re-plan.
5. On a [policy](write-a-policy.md) deny, inspect the rule via `policy`, update the code to satisfy it, commit, and re-plan.
6. Return the plan ID, summary of changes, and required approvers. Stop there.

The skill forbids running `tofu` or `terraform` locally, applying, and approving. If an agent ignores it, the control plane still holds the line: authors cannot approve their own plans, and `writ mcp` has no approve or apply tool.

## The MCP server

`writ mcp` runs a stdio Model Context Protocol server for stacks under `--dir`, with the usual Writ auth (`--server`, `WRIT_SERVER`, `writ login`, or `writ serve --dev`). Its only writing tools are `plan`, which creates a remote plan, and `import_bulk`, which exists only with `--allow-providers` because it runs providers with your local credentials. No tool approves, applies, or writes state. The tools are listed in [Tools and guarantees](how-writ-mcp-works.md#tools-and-guarantees).

### Add it to Claude Code

```
$ claude mcp add --scope project writ -- writ mcp
Added stdio MCP server writ with command: writ mcp to project config
```

Output of Claude Code 2.1.318, 2026-11-10 (tools appear as `mcp__writ__plan`, `mcp__writ__get_plan`, etc.).

> **Warning:** under `writ serve --dev` with a local repository, the agent can commit locally, so a commit plan proves clean local files, not peer review.

## Troubleshooting

| Problem | Fix |
| --- | --- |
| `writ agent init` warns `kept .agents/skills/writ/...` | Your files differ from this version. `writ agent init --force` overwrites them. |
| Second `plan` call refused | `writ mcp` runs one plan at a time. Wait for the active one. |
| `import_bulk` missing | Pass `--allow-providers` to `writ mcp`, only when you intend local cloud reads. |
| Symlink errors in `writ agent init` | Remove symlinks under `.agents/skills/writ` or `.claude/skills/writ` (or pass `--force`), and avoid symlinks that point outside `--dir`. |
| `plan` refused for a dirty tree, linked worktree, or index flags | Commit or clean the stack and its local modules; see [A plan from a work tree](how-writ-mcp-works.md#a-plan-from-a-work-tree). |

## Next steps

- [How writ mcp works](how-writ-mcp-works.md): tools, bounds, file rules, and Git isolation.
- [Writ reference for agents](agent-reference.md): exact tool inputs, outputs, and refusal recovery.
- [Write and test a policy](write-a-policy.md): enforce rules agents cannot override.
