# Add a repository

Register a Git repository so `writ serve` can read its commits and discover OpenTofu stacks. Clients never upload stack files to a team control plane: `writ serve` fetches commits from Git by SHA, verifies each file's Git SHA-1 blob ID, and plans those exact bytes.

Add a local repository to `writ serve --dev`, or a GitLab project to a team's `writ serve`. GitLab is the only supported remote Git host.

## Before you begin

- **Team control plane:** a `forge` block with a read-only GitLab token. To add repositories from the CLI or API rather than pinning them in config, you need global `admin` (`envs = ["*"]`, `stacks = ["**"]`).
- **Git:** `git` on `PATH` for `writ` and `writ serve --dev`. The Nix package includes it; the standalone binary does not. The container image has no `git`, because a team's `writ serve` reads GitLab over HTTPS. See [Install: Git](install.md#git).

## Forges and their tokens

Add a `forge` block to `/etc/writ/serve` with a token file of `read_api` scope:

```hcl
forge "gitlab" {
  kind            = "gitlab"
  url             = "https://gitlab.com"
  read_token_file = "/run/secrets/writ/gitlab_read"
}
```

| Setting | Meaning |
| --- | --- |
| `kind` | `"gitlab"` only. |
| `url` | `https://`, or loopback `http://`. |
| `read_token_file` | Re-read on every call, so you can rotate it without a restart. Sent only in `PRIVATE-TOKEN`, with redirects disabled. |
| `ca_file` | Optional root CA PEM bundle for self-managed GitLab. |
| `write_token_file` | Optional. Logs a warning, because merge request comments are not built yet. |
| `poll_every` | Default-branch poll interval. Default `60s`, minimum `1s`. |
| `poll_every_with_hook` | Poll interval for repositories with a webhook. Default `5m`. |

| GitLab tier | Read token |
| --- | --- |
| Self-Managed, Dedicated, GitLab.com Premium or Ultimate | Group or project access token, Reporter role, `read_api` |
| GitLab.com Free | Bot user personal access token, Reporter role on the group, `read_api` |

- Do not use deploy tokens. GitLab's REST API rejects them.
- Writ never echoes raw forge response text in API answers, logs, or database rows. Read tokens are redacted from errors, and refused if a forge echoes them back.

**Limits:** `read_api` coverage of GitLab's tree and raw blob endpoints has not been verified against a live GitLab instance.

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

## Add a GitLab project

```sh
writ repo add NAME FORGE/PROJECT [--path DIR]
```

`FORGE` is a `forge` block's name or its host. `--path` limits stack discovery to a subdirectory. `writ serve` resolves the project path to its numeric ID, reads the default branch's head with the forge's read-only token, lists its files, and discovers stacks. The output matches the local command's, shows the Git host, project path, and project ID, and suggests `writ ci gitlab --repo NAME` next.

GitLab forge reads have been tested against a local GitLab stand-in, not a live instance, so no real-project output is shown.

## Add one from the control UI

- `/ui/repos` lists registered repositories, their last read commit, last poll time, and webhook status.
- `/ui/repos/new` adds a repository through the same validation as `POST /v1/repos`, in-process, with no extra token. `/ui/repos/NAME` then lists its stacks.
- `/ui/start` summarizes what is configured and your next step.
- Under `writ serve --dev`, the startup banner prints a one-time sign-in link valid for 10 minutes. `writ ui` prints another.

## Rules for adding a repository

Adding a repository needs `admin` on every stack in every env, because repository code can define any stack name. Most teams instead pin repositories with `repo` blocks in `/etc/writ/serve`, so config review governs them. On `writ serve --dev`, the dev team can add any repository.

| Rule | Detail |
| --- | --- |
| Admin grant scope | Only grants with `envs = ["*"]` count, and their `stacks` patterns must match every depth up to 64 parts (the most a 128-character name has). A pattern of `*` parts matches one exact depth (`"*"`, `"*/*"`). Mixing `*` and `**` matches at least as many parts as the `*`s, so `stacks = ["**"]` alone covers every depth. |
| Unique names and targets | No two repositories share a `name`, or the same `project` and `path`. |
| Unique stack names | State is stored by stack name, so stack names are global per control plane. On a collision, pass `--prefix payments/` (or `prefix = "payments/"` in HCL) to get `payments/network` and so on. |
| Disjoint prefixes | No prefix may start with another's: `payments/` and `payments/eu/` are refused together. Two repositories may share the exact same prefix, or one may have none. Pinned `repo` blocks follow the same rule; offending entries are skipped and logged. |
| No stack under another's prefix | An unprefixed stack `a/x` is refused next to a repository with prefix `a/`, and the reverse. Polling enforces this on new commits: Writ refuses the commit, keeps the previous head, and records the error until it is fixed. |

Writ runs these checks inside the transaction that stores the repository, so two concurrent requests cannot both succeed.

**Pin repositories in config.** When `/etc/writ/serve` has `repo` blocks, `POST /v1/repos` is disabled, and the poller makes the database match the config:

```hcl
repo {
  name    = "infra"
  forge   = "gitlab"
  project = "acme/infra"
  path    = "infra"
}
```

Pinned repositories can also take GitLab webhooks for near-instant updates ([Webhooks](webhooks.md)).

## What a stack can contain

Any directory under `path` with a `.terraform.lock.hcl` and `.tf` or `.tofu` files is an OpenTofu stack. When `tofu` is configured, every discovered stack is served in every configured env, limited only by grants. A repository with no stacks under `path` is refused. List stacks with `writ repo list` and `writ repo show NAME`:

```
$ writ repo list
NAME  PROJECT           PATH      BRANCH  HEAD      STACKS  LAST POLL
demo  /home/alice/demo  the root  main    42bf997f  1       2026-11-16T03:29:49Z
$ writ repo show demo
Repository demo: /home/alice/demo, path the root, branch main.
Read main at 42bf997f from the local repository: 1 file, 1 stack.
STACK  SERVED
app    dev
```

Writ refuses a commit or stack, before OpenTofu runs, for any of these:

| Rule | Refused |
| --- | --- |
| Files | A missing `.terraform.lock.hcl`; a committed `.terraform/` directory; any `.tf.json`, `.tofu.json`, or `.writ` file; any symlink under `path`. Submodules are ignored. |
| Backend | Anything but an empty `backend "http" {}` or an allow-listed `backend "s3"` ([AWS roles and credentials](aws-credentials.md#2-grant-s3-state-permissions-if-using-backend-s3)). A missing backend and `cloud {}` are refused. |
| Module `source` | A non-literal source, or one that is not: a relative path (`./`, `../`) inside the repository `path`; a registry address (`[HOST/]NS/NAME/SYS[//SUBDIR]`); or a remote address (`git::`, `hg::`, `s3::`, `gcs::`, `http::`, `https::`, `github.com/`, `bitbucket.org/`). |
| Module address parts | Any query parameter but a literal `ref` and `depth` (so `checksum`, `archive`, `filename`, `sshkey`, `rev`, S3 credential parameters, and so on), `#` fragments, `file:` (even percent-encoded), backslashes, absolute paths, `~`, Windows and UNC paths, `oci://`. |
| Size and fetch limits | `1 MiB` per `.tf`, `.tofu`, `.tfvars`, or lock file; `16 MiB` per other file; `2,000` files or `64 MiB` across a stack and its local modules; `2,000` lock files (`16 MiB` total); `20,000` tree entries; `1 MiB` per tree page; `8 MiB` per other API response; `10s` per Git host HTTP call. |

## Troubleshooting

| Problem | Fix |
| --- | --- |
| `409 usage: the config pins the repositories with repos` | `repo` blocks disable `POST /v1/repos`. Edit the config and restart `writ serve`. |
| Stack name collision (`409`) | Another repository defines that stack. Set `--prefix TEAM/` or `prefix = "TEAM/"`. |
| Prefix overlap refused | Prefixes cannot nest (`payments/` and `payments/eu/`). Pick disjoint ones. |
| `writ repo add .` fails with a Git ownership error | `writ serve --dev` ignores global `safe.directory`. The repository must be owned by the user running it. |
| Commit refused for symlinks or a missing lock file | Remove symlinks under the repository path, and commit `.terraform.lock.hcl` in every stack directory. |

## Next steps

- [Webhooks](webhooks.md): tell `writ serve` about a push at once.
- [Set up GitLab CI](set-up-gitlab-ci.md): generate jobs and server config with `writ ci gitlab --repo NAME`.
- [Plan and apply](apply.md): plan a commit and apply an approved plan.
- [Repository routes](api.md#repository-routes): the HTTP API behind `writ repo add`.
