# Add a repository

Before the control plane can plan anything, it needs your code. Add the Git repository that has your OpenTofu stacks, and the control plane reads it by commit and finds the stacks in it.

You can add a local repository to `writ serve --dev`, or a GitLab project to a team's `writ serve`. To learn how the control plane reads and polls a repository, see [Repositories](repos.md).

## Before you begin

- On a team's control plane, you need `admin` on every stack in every environment. See [Rules for adding a repository](#rules-for-adding-a-repository).
- You need `git` on `PATH` for both `writ` and `writ serve --dev`. The Nix package brings it. The release binary doesn't, and the container image, which is for a team's `writ serve`, has none, as [Install: Git](install.md#git) explains.

## Add a local repository to writ serve --dev

In the repository, run `writ repo add .`. It adds the Git repository that contains the current directory. The path is that directory, from the top of its Git work tree, and the name is the directory's name.

Here's a new repository whose one stack is `app`:

```
$ writ repo add .
Repository demo: /home/alice/demo, path the root, branch main.
Read main at 42bf997f from the local repository: 1 file, 1 stack.
STACK  SERVED
app    dev
Next: in a clean work tree of it, writ plan --remote plans the commit you are on.
```

Output of a real run, 2026-11-16.

Your work tree is never read. The dev control plane reads the repository's commits with the `git` binary. Git runs with no hook, no `core.fsmonitor` program, no system or global config, no `GIT_` variable from the environment of `writ serve --dev`, no replacement objects, and no transport. So if your repository's config asks for a fetch, that fetch fails.

Unlike the `plan` tool of `writ mcp`, the dev control plane follows `.git/objects/info/alternates`, `.git/commondir`, and a `.git` file, because it reads a repository a person added.

Because git reads no global config, it doesn't read a `safe.directory` there. So the repository must belong to the user that runs `writ serve --dev`.

## Add a GitLab project

To add a project, run `writ repo add NAME FORGE/PROJECT`. For `FORGE`, give a forge's name in the config or its host. Use `--path` to set the directory under which the control plane finds stacks.

The control plane resolves the path to the project's numeric id, reads the default branch's head, lists its files, and finds the stacks. You'll see the same lines as for a local repository, with the Git host, the project, and its id in place of the local path. It says that it read the commit with the forge's read-only token, and suggests `writ ci gitlab --repo NAME` as the next step.

There's no output here yet. Only the tests' stand-in of GitLab has served one, and [the test plan](development.md#test-plan-for-a-real-gitlab-forge) for a real project hasn't run.

## Add one from the control UI

If you prefer the browser, open Repositories at `/ui/repos`. It lists each repository with its last commit read and its last poll.

Add repository, at `/ui/repos/new`, is a form with the same fields, sent to `POST /v1/repos` by the server in process. It takes no token. If Writ refuses the form, it comes back with the refusal and what you typed.

After you add one, the repository's page at `/ui/repos/NAME` shows the stacks found. Getting started, at `/ui/start`, lists what the control plane has and your next step. Under `writ serve --dev`, the start banner prints a sign-in link that works once, for 10 minutes, and `writ ui` prints another.

## Rules for adding a repository

To add a repository, you need `admin` on every stack in every environment, because its code can contain any stack. That's why most teams pin their repositories with `repo` blocks instead, where the config's review covers them. On `writ serve --dev`, the dev team can add any repository.

**Which grants count.** Only grants with `envs = ["*"]` count, and their `stacks` must match every stack name of every depth up to 64 parts, the most a 128-character name can have. A pattern of `*` parts alone covers names of exactly that many parts (`"*"`, `"*/*"`, and so on). If you mix `*` and `**` parts, the pattern covers names of at least as many parts as its `*` parts, so `stacks = ["**"]` covers every depth on its own.

**Names and projects are unique.** You can't add a second repository with the same name, or the same project and path.

**Stack names are unique too.** Writ keeps state by stack name, so each stack name is unique on a control plane. If your repository has a stack whose name another repository has, Writ refuses it, and the message suggests a prefix. With `--prefix payments/`, its stacks are named `payments/network` and so on.

**Prefixes can't overlap.** A prefix can't start with another repository's prefix, or be the start of one: `payments/` and `payments/eu/` are refused together, and the message says which repository has the other. Two repositories can have the same prefix, and one can have none. The pinned list in your config follows this rule too: if a repository breaks it, it isn't added, and the control plane logs why.

**Stack names can't start with another prefix.** That's true in either repository. So an unprefixed repository with the stack `a/x` is refused next to a repository with the prefix `a/`, and the reverse, and the message lists both repositories.

The poll applies this rule to later commits. If a commit adds such a stack, or one whose name another repository's stack has, Writ doesn't record it. The repository's head stays where it was, and its error says why until a commit fixes it.

Writ checks a repository against the others inside the transaction that stores it, so two requests at once can't both pass.

**Pin the list in your config.** When your config has `repo` blocks, they pin the list. Writ then refuses `POST /v1/repos`, and the poll adds, replaces, and removes rows to match the config:

  ```hcl
  repo {
    name    = "infra"
    forge   = "gitlab"
    project = "acme/infra"
    path    = "infra"
  }
  ```

## Troubleshooting

- **`409 usage: the config pins the repositories with repos`**: Your server config defines `repo` blocks, which disables `POST /v1/repos`. Add the repository in your server config file instead and restart `writ serve`.
- **Stack name collision (`409`)**: Another repository already has a stack with the same name. Pass `--prefix TEAM/` (or set `prefix = "TEAM/"` in the `repo` block) so its stacks are named `TEAM/STACK`.
- **Prefix overlap refused**: One repository's prefix cannot start with another's (for example `payments/` and `payments/eu/`). Choose disjoint prefixes.
- **`writ repo add .` fails with a Git ownership error**: Because `writ serve --dev` runs `git` without global config (ignoring `safe.directory`), the local repository must be owned by the user running `writ serve --dev`.
- **Commit refused because of symlinks or missing lock files**: Remove symbolic links under the repository path and ensure every stack directory contains `.tf` or `.tofu` files and a committed `.terraform.lock.hcl`.

## Next steps

- [Set up GitLab CI](set-up-gitlab-ci.md): `writ ci gitlab --repo NAME` prints the jobs to include.
- [Plan and apply](apply.md): plan a commit and apply an approved plan.
- [Repository routes](api.md#repository-routes): the API behind `writ repo add`.
