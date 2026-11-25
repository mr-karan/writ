# Add a repository

Register a Git repository so `writ serve` can read its commits and discover OpenTofu stacks. Clients never upload stack files to a team control plane: `writ serve` fetches commits from Git by SHA, verifies each file's Git SHA-1 blob ID, and plans those exact bytes. GitHub (github.com or GitHub Enterprise Server, landing in the first release after v0.1.8; see [CHANGELOG.md](../CHANGELOG.md)) and GitLab are the supported remote Git hosts.

## Before you begin

- **Team control plane:** a `forge` block with a read-only GitHub or GitLab token. To add repositories from the CLI or API rather than pinning them in config, you need global `admin` (`envs = ["*"]`, `stacks = ["**"]`).
- **Git:** `git` on `PATH` for `writ` and `writ serve --dev`. The Nix package includes it; the standalone binary does not. The container image has no `git`, because a team's `writ serve` reads GitHub and GitLab over HTTPS. See [Install: Git](install.md#git).

## Forges and their tokens

### GitHub

1. Create a fine-grained personal access token with access to the repositories and the repository permissions **Contents: read** and **Metadata: read**. A GitHub App is not built yet.

2. Add a `forge` block of kind `github` to `/etc/writ/serve` (`url` defaults to `https://github.com`; set `https://HOST` for GitHub Enterprise Server):

   ```hcl
   forge "github" {
     kind            = "github"
     read_token_file = "/run/secrets/writ/github_read"
   }
   ```

   Optional settings (`ca_file`, `write_token_file`, `poll_every`, `poll_every_with_hook`) and token handling are in [forge and repo](config.md#forge-and-repo). `poll_every` defaults to `60s`; `poll_every_with_hook` to `5m`.

### On GitLab

1. Create a read token with `read_api` scope:

   | GitLab tier | Read token |
   | --- | --- |
   | Self-Managed, Dedicated, GitLab.com Premium or Ultimate | Group or project access token, Reporter role, `read_api` |
   | GitLab.com Free | Bot user personal access token, Reporter role on the group, `read_api` |

   Do not use deploy tokens. GitLab's REST API rejects them.

2. Add a `forge` block to `/etc/writ/serve`:

   ```hcl
   forge "gitlab" {
     kind            = "gitlab"
     url             = "https://gitlab.com"
     read_token_file = "/run/secrets/writ/gitlab_read"
   }
   ```

**Limits:** GitHub reads have been tested against a local GitHub stand-in, not github.com or a live GitHub Enterprise Server. `read_api` coverage of GitLab's tree and raw blob endpoints has not been verified against a live GitLab instance.

## Add a local repository to writ serve --dev

From inside the repository, run `writ repo add .`. Writ registers the enclosing Git repository, sets `path` to your current directory relative to the repository root, and names it after that directory:

```
$ writ repo add .
Repository demo: /home/alice/demo, path the root, branch main.
Read main at 42bf997f from the local repository: 1 file, 1 stack.
STACK  SERVED
app    dev
Next: in a clean work tree of it, writ plan --remote plans the commit you are on.
```

- `writ serve --dev` never reads your working tree. It reads commits with `git`, with hooks, `core.fsmonitor`, system and global config, `GIT_*` environment variables, replacement objects, and network transports disabled.
- Unlike `writ mcp`'s `plan` tool, it follows `.git/objects/info/alternates`, `.git/commondir`, and `.git` files, because a person registered the repository.
- `git` ignores global config, `safe.directory` included, so the repository must be owned by the user running `writ serve --dev`.

## Add a GitHub repository or GitLab project

Pin it in `/etc/writ/serve`, which most teams prefer so config review governs it. With `repo` blocks present, `POST /v1/repos` is disabled and the poller makes the database match the config:

```hcl
repo {
  name    = "infra"
  forge   = "github"
  project = "acme/infra"
  path    = "infra"
}
```

On GitLab, set `forge = "gitlab"` and `project = "namespace/project"`.

Or, with global `admin`, add it from the CLI:

```sh
writ repo add NAME FORGE/PROJECT [--path DIR] [--prefix TEAM/]
```

`FORGE` is a `forge` block's name or its host, and `--path` limits stack discovery to a subdirectory. `PROJECT` is `OWNER/REPO` on GitHub (such as `writ repo add infra github.com/acme/infra`) and `namespace/project` on GitLab. `writ serve` resolves the repository to its numeric ID, reads the default branch's head with the read token, and discovers stacks. The output matches the local command's, shows the Git host, project path, and project ID, and suggests `writ ci github --repo NAME` or `writ ci gitlab --repo NAME` next. Pinned repositories can also take [webhooks](webhooks.md).

> **Warning:** stack names are global per control plane. If another repository already has a stack of that name, the add is refused; set a prefix. Adding a repository needs `admin` on every stack in every env, because its code can define any stack name. The full rules are in [Rules for adding a repository](config.md#rules-for-adding-a-repository).

## Add one from the control UI

`/ui/repos/new` adds a repository through the same validation as `POST /v1/repos`. `/ui/repos` lists repositories with their last read commit, poll time, and webhook status; `/ui/repos/NAME` lists stacks; `/ui/start` shows your next step. Under `writ serve --dev`, the startup banner prints a one-time sign-in link valid for 10 minutes, and `writ ui` prints another. On `writ serve --dev`, the dev team can add any repository.

## What a stack can contain

Any directory under `path` with a `.terraform.lock.hcl` and `.tf` or `.tofu` files is an OpenTofu stack. When `tofu` is configured, every discovered stack is served in every configured env, limited only by grants. A repository with no stacks under `path` is refused.

```
$ writ repo list
NAME  PROJECT           PATH      BRANCH  HEAD      STACKS  LAST POLL
demo  /home/alice/demo  the root  main    42bf997f  1       2026-11-16T03:29:49Z
```

`writ repo show NAME` lists one repository's stacks. Before OpenTofu runs, Writ refuses commits with symlinks, `.tf.json` files, a committed `.terraform/`, a missing lock file, an unsupported backend, a disallowed module source, or files past the size limits ([What a stack may not contain](config.md#what-a-stack-may-not-contain)).

## Troubleshooting

| Problem | Fix |
| --- | --- |
| `409 usage: the config pins the repositories with repos` | `repo` blocks disable `POST /v1/repos`. Edit the config and restart `writ serve`. |
| Stack name collision (`409`) | Another repository defines that stack. Set `--prefix TEAM/` or `prefix = "TEAM/"`. |
| Prefix overlap refused | Prefixes cannot nest (`payments/` and `payments/eu/`). Pick disjoint ones. |
| `writ repo add .` fails with a Git ownership error | `writ serve --dev` ignores global `safe.directory`. The repository must be owned by the user running it. |
| Commit refused for symlinks or a missing lock file | Remove symlinks under the repository path, and commit `.terraform.lock.hcl` in every stack directory. |
| `404` adding a GitHub repository | The repository is missing, or the fine-grained token was not given access to it. GitHub answers 404 for both. |
| `the forge limits the rate of calls until ...` | GitHub's rate limit, primary or secondary. `writ serve` sends that forge no call before the time shown, then reads again at the next poll. |
| `404` adding a GitLab project | The project is missing on the forge, or the read token cannot see it (it needs Reporter and `read_api`). Check `FORGE/PROJECT` and the token's access. |

## Next steps

- [Webhooks](webhooks.md): tell `writ serve` about a push at once.
- [Set up GitHub Actions](set-up-github-actions.md): generate a workflow and server config with `writ ci github --repo NAME`.
- [Set up GitLab CI](set-up-gitlab-ci.md): generate jobs and server config with `writ ci gitlab --repo NAME`.
- [Repository and stack rules](config.md#repository-and-stack-rules): naming, prefix, and stack content rules.
- [Repository routes](api.md#repository-routes): the HTTP API behind `writ repo add`.
