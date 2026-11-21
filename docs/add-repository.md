# Add a repository

Register a Git repository so `writ serve` can read its commits and discover OpenTofu stacks. You can add a local repository to `writ serve --dev` or a GitLab project to a team's `writ serve`. For how polling and commit verification work, see [How repositories work](repos.md).

## Before you begin

- On a team's control plane, you need `admin` on every stack in every environment. See [Rules for adding a repository](#rules-for-adding-a-repository).
- `git` must be on `PATH` for both `writ` and `writ serve --dev`. The Nix package includes it; the standalone binary doesn't. The container image has no `git` because a team's `writ serve` reads GitLab over HTTPS. See [Install: Git](install.md#git).

## Add a local repository to writ serve --dev

From inside the repository, run `writ repo add .`. Writ registers the enclosing Git repository, sets its `path` to your current directory relative to the repo root, and names it after that directory:

```
$ writ repo add .
Repository demo: /home/alice/demo, path the root, branch main.
Read main at 42bf997f from the local repository: 1 file, 1 stack.
STACK  SERVED
app    dev
Next: in a clean work tree of it, writ plan --remote plans the commit you are on.
```

Output of a real run, 2026-11-16.

`writ serve --dev` never reads your working tree. It reads commits via `git` with hooks, `core.fsmonitor`, system and global config, `GIT_*` environment variables, replacement objects, and network transports disabled.

Unlike `writ mcp`'s `plan` tool, `writ serve --dev` follows `.git/objects/info/alternates`, `.git/commondir`, and `.git` files because a person explicitly registered the repository. Because `git` ignores global config (including `safe.directory`), the repository must be owned by the user running `writ serve --dev`.

## Add a GitLab project

Run `writ repo add NAME FORGE/PROJECT`, where `FORGE` is a `forge` block name or its host. Pass `--path DIR` to limit stack discovery to a subdirectory.

`writ serve` resolves the project path to its numeric project ID, reads the default branch's head using the forge's read-only token, lists its files, and discovers stacks. The output matches the local command above (showing the Git host, project path, and project ID) and suggests `writ ci gitlab --repo NAME` as the next step.

No real-project output is shown here yet: GitLab forge reads have been exercised against the test stand-in, not a live GitLab instance ([test plan](development.md#test-plan-for-a-real-gitlab-forge)).

## Add one from the control UI

Open `/ui/repos` to view registered repositories, their last read commit, last poll time, and webhook status.

Open `/ui/repos/new` to add a repository through the same validation path as `POST /v1/repos` (in-process, no extra token). After adding one, `/ui/repos/NAME` lists its discovered stacks. `/ui/start` summarizes what's configured and your next step. Under `writ serve --dev`, the startup banner prints a one-time 10-minute sign-in link, and `writ ui` prints another.

## Rules for adding a repository

Adding a repository requires `admin` on every stack in every environment, because repository code can define any stack name. Most teams pin repositories with `repo` blocks in `/etc/writ/serve` instead so config review governs them. On `writ serve --dev`, the dev team can add any repository.

- **Admin grant scope:** Only grants with `envs = ["*"]` count, and their `stacks` patterns must match every stack depth up to 64 parts (the maximum for a 128-character name). A pattern of `*` parts matches one exact depth (`"*"`, `"*/*"`). Mixing `*` and `**` matches at least as many parts as `*`, so `stacks = ["**"]` covers all depths alone.
- **Unique names and targets:** No two repositories can share a `name`, or the same `project` and `path`.
- **Unique stack names:** Writ stores state by stack name, so stack names are global per control plane. On a collision, pass `--prefix payments/` (or `prefix = "payments/"` in HCL) so stacks become `payments/network`, etc.
- **Disjoint prefixes:** One repository's prefix can't start with another's (`payments/` and `payments/eu/` are refused together). Two repositories may share the exact same prefix, or one may have none. Pinned `repo` blocks obey the same rule; offending entries are skipped and logged.
- **No stack name under another prefix:** An unprefixed stack `a/x` is refused alongside a repository with prefix `a/`, and vice versa. Polling enforces this on new commits: if a commit introduces a conflicting stack, Writ refuses the commit, keeps the previous head, and records the error until fixed.

Writ runs these checks inside the database transaction that stores the repository so concurrent requests can't both succeed.

**Pin repositories in config.** When `/etc/writ/serve` contains `repo` blocks, `POST /v1/repos` is disabled and the poller reconciles the database to match the config:

```hcl
repo {
  name    = "infra"
  forge   = "gitlab"
  project = "acme/infra"
  path    = "infra"
}
```

Pinned repositories can also receive GitLab webhooks for near-instant updates. See [Get changes in seconds with a webhook](webhooks.md).

## Troubleshooting

- **`409 usage: the config pins the repositories with repos`**: Your config defines `repo` blocks, which disables `POST /v1/repos`. Edit the config file and restart `writ serve`.
- **Stack name collision (`409`)**: Another repository already defines a stack with that name. Set `--prefix TEAM/` or `prefix = "TEAM/"`.
- **Prefix overlap refused**: Prefixes can't nest (for example `payments/` and `payments/eu/`). Pick disjoint prefixes.
- **`writ repo add .` fails with a Git ownership error**: `writ serve --dev` ignores global `safe.directory`. Ensure the repository is owned by the user running `writ serve --dev`.
- **Commit refused for symlinks or missing lock files**: Remove symlinks under the repository path and commit `.terraform.lock.hcl` in every stack directory.

## Next steps

- [Get changes in seconds with a webhook](webhooks.md): notify `writ serve` immediately on push.
- [Set up GitLab CI](set-up-gitlab-ci.md): generate pipeline jobs and server config with `writ ci gitlab --repo NAME`.
- [Plan and apply](apply.md): plan a commit and apply an approved plan.
- [Repository routes](api.md#repository-routes): the HTTP API behind `writ repo add`.
