# Repositories

The control plane reads your OpenTofu code from its Git repository, by commit, and plans exactly the files of that commit. It never plans files a client sends, so the code Writ plans is the code in your repository, not a copy from someone's laptop.

Today GitLab is the only Git host it reads; GitHub isn't built yet. To add one, see [Add a repository](add-repository.md). To see how a commit's plan is approved and applied, see [Approvals](approvals.md).

## How repositories work

To the control plane, a repository is a row in the control database: a project at a Git host, the path in it where your stacks are, and a name. As an admin, you add it with `writ repo add` or the control UI's Add repository screen.

That row has no credential. It points to a `forge` block in your config, and the forge's read-only token is a file the config points to. No request can carry a token or a URL to fetch from.

When you plan, your plan refers to a repository and a commit. Then the control plane fetches the stack's files at that commit from the Git host, checks each file against its Git blob id, and plans exactly those files. Nothing a client sends gets planned.

## Forges and their tokens

In your config, you describe each Git host in a `forge` block:

```hcl
forge "gitlab" {
  kind            = "gitlab"
  url             = "https://gitlab.com"
  read_token_file = "/run/secrets/writ/gitlab_read"
}
```

- `kind` is `gitlab`. If you write `github`, Writ refuses it until GitHub is built.
- `url` is `https://`, or `http://` on a loopback address.
- `read_token_file` has a token with `read_api`. The control plane reads the file for each call, so you don't need to restart when you rotate the token. It sends the token only in the `PRIVATE-TOKEN` header, follows no redirect, and takes only the page token from a `Link` header, never its URL.
- `ca_file` adds roots for a GitLab instance with a private CA.
- `write_token_file` parses and warns: merge request notes aren't built.

Here's which token to create:

| GitLab | Read token |
| --- | --- |
| Self-Managed and Dedicated | A group or project access token, Reporter role, `read_api` |
| GitLab.com Premium and Ultimate | The same |
| GitLab.com Free | A personal access token of a bot user that the group has as Reporter, with `read_api` |

Don't use a deploy token. GitLab's API refuses it.

To set how often each default branch is polled, use `poll_every`. It's 60 seconds by default and at least 1 second. A repository with a [webhook](webhooks.md) is polled every `poll_every_with_hook` instead, 5 minutes by default. For local work, `writ serve --dev --config FILE` reads `forge` and `repo` blocks, `poll_every` and `poll_every_with_hook`, and `tofu` and `policy` blocks, from an HCL file that has only those.

**Limits.** Nobody has verified yet that `read_api` covers the tree and raw blob endpoints. The [test plan](development.md#test-plan-for-a-real-gitlab-forge) checks it.

## What the control plane fetches

For a commit, the control plane lists the tree under your repository's path and reads each `.terraform.lock.hcl`. It reads nothing else at this point. If the commit contains `.writ` files, it refuses the commit. Later, a plan of an [OpenTofu stack](serve.md#opentofu-stacks) reads the rest of that stack's files, within the stack's own bounds.

Here's what that means for each call and each file:

- On GitLab, it calls `GET /projects/:id/repository/tree?ref=SHA&path=PATH&recursive=true&pagination=keyset&per_page=100`, then `GET /projects/:id/repository/blobs/:sha/raw` for each blob the control database doesn't have.
- Your control database keeps each file once, by its Git blob id, in the table `blobs`. So a second commit costs the tree listing and its changed files. A test proves it: a merge request commit that changes one file fetches one blob.
- Every fetched file must hash to its blob id: the SHA-1 of `blob SIZE`, a NUL byte, and the content. If a file doesn't, Writ refuses it, and the commit with it.
- Writ refuses a symbolic link under the path, and leaves a submodule out.
- You never see the Git host's own text in an answer, a log line, or a row. A refused call says the status and Writ's reason for it. Writ refuses an answer that contains the read token, and every error passes a redactor that removes the token.

### When fetched files are stored

Writ stores the fetched files only once the whole commit has passed every check and its row is written. So a refused commit leaves no blob behind.

Writ gives the write 30 seconds, and the write goes on when the client leaves. It runs under the context of `writ serve`. On SIGTERM, `writ serve` first lets running requests finish for up to 30 seconds, and only then does `Server.Close` cancel that context. The write then stops within one blob: the blob it's writing is stored or its write is cancelled, and it writes no blob after that one.

If a file wasn't stored, the next read of the commit fetches it again. `TestKeepStopsBetweenBlobs` proves both.

### Size and time limits on a fetch

- 10 seconds for each call.
- 2,000 lock files and 16 MiB of them for a commit.
- 1 MiB for one file, which for an OpenTofu lock file refuses only its stack.
- 20,000 entries in a tree listing, counted as the Git host lists them, and 1 MiB for one page of the listing.
- 8 MiB for any other answer.

Writ checks the sizes of stored files before it loads any. An OpenTofu stack's other files are read only when you plan it, under the bounds of one stack in [What a stack can contain](serve.md#what-a-stack-can-contain).

## How the control plane learns about commits

You don't need an inbound webhook. The control plane learns about a commit in one of three ways:

1. **A CI job calls in.** Your merge request job sends its commit, and the control plane fetches it for the plan.
2. **The control plane polls.** Every `poll_every`, for each repository, it asks the Git host for the head of the default branch: one call. When the head moved, it reads the new commit and runs discovery. If a poll fails, it keeps the last head and records why on the row, and you can read that in `writ repo show` or on the Repository page.
3. **A webhook tells it.** For a pinned repository with `webhook_secret_file`, GitLab's push event makes the control plane run that same read within seconds, and the poll of that repository slows to `poll_every_with_hook`, 5 minutes by default. The delivery only says when to read: the head still comes from the Git host's API. See [Get changes in seconds with a webhook](webhooks.md).

## How Writ finds your stacks

Discovery finds each OpenTofu stack in the commit. That's each directory under the repository's path with a committed `.terraform.lock.hcl` and `.tf` or `.tofu` files. For each stack it records the providers its lock file pins.

To see what your config does with each stack, use `writ repo show NAME`, the Repository page, or `GET /v1/repos/NAME`. They add the environments the config serves the stack in, and whether it runs the stack's providers. When your config has a `tofu` block, a stack is served in every environment of the config, and in none otherwise.

If a commit has no OpenTofu stack under the path, Writ refuses it when you add the repository.

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

Output under `writ serve --dev`, 2026-11-16.

## Plan a commit

### From a work tree

When you run `writ plan --remote` in a stack directory, it looks at the Git work tree. It sends a plan of the commit you're on when all of these are true:

- Your work tree is a repository of the control plane: the same directory for a dev repository, or a remote URL whose host and project are the repository's.
- The stack's directory is under the repository's path.
- No file under the path differs from the commit.
- For a Git host, the commit is on a remote branch, so the Git host has it.

Otherwise it tells you why no commit can be planned, and sends nothing.

### From a CI job

Your CI job doesn't need a checkout. It gives the stack and its commit, and reads no file:

```
writ plan --remote --env prod --stack network --commit "$CI_COMMIT_SHA"
```

From the token, the control plane takes the repository whose forge URL is the token's issuer and whose project id is the token's `project_id`. If your project has stacks under two paths, that's two repositories, and the job picks one with `--repo`. Your commit must be the token's `sha`, and the ref is its `ref_path`.

A merge request from a fork runs in the fork's project, whose id matches no repository, so Writ refuses its plan. If the Git host doesn't have the commit, Writ refuses it with 404 before the plan starts.

Next, the control plane asks the Git host whether the commit is on the ref: the ref's commit or an ancestor of it. It uses GitLab's merge base of the two, or `git merge-base` under `writ serve --dev`. If the commit isn't on the ref, Writ refuses it with 403. If the Git host can't check, such as for a ref it lacks, Writ refuses it with the Git host's status.

When you plan as a person and set a ref, Writ checks it the same way. Your plan records the check in its `source` as `ref_check`, either `on-ref`, or `no-ref` when the plan has no ref.

Under `writ serve --dev`, you can set a commit the same way:

```
$ writ plan --remote --stack app --repo demo --commit 552091ed387347e8948abc28dd127d91513415e2
commit plan of app@dev: 552091ed, sent to http://127.0.0.1:7340
OpenTofu 1.13.9 plan of app@dev

  + random_id.suffix (create)
-/+ random_pet.name (replace, because of length)

Plan: 2 to add, 0 to change, 1 to destroy.
Outputs that change: name.
Plan 545df270380c53e2f138f94a8af47c27 for dev (app), by alice (dev), expires 2026-11-17T03:29:50Z.
It is a commit plan of demo at 552091ed.
It needs no approval. Apply it with: writ apply --remote 545df270380c53e2f138f94a8af47c27
```

Output of a real run, 2026-11-16.

## Next steps

- [Add a repository](add-repository.md).
- [Approvals](approvals.md): how a commit's plan is approved and applied.
- [Set up GitLab CI](set-up-gitlab-ci.md): the CI jobs that plan, approve, and apply.
- [Repository routes](api.md#repository-routes) and [JSON output](json-output.md#documents-of-writ-serve): the API and its documents.
