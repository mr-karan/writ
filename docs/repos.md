# Repositories

`writ serve` reads OpenTofu code directly from Git by commit SHA and plans only those verified files. Clients never upload stack files to a team control plane.

GitLab is currently the only supported remote Git host (GitHub isn't built yet). For setup instructions, see [Add a repository](add-repository.md); for commit approvals, see [Approvals](approvals.md).

## How repositories work

A repository record stores a GitLab project, stack subdirectory (`path`), optional name `prefix`, and display name. It holds no credentials: it references a `forge` block whose read-only token lives in a file on the server. When a plan runs, `writ serve` fetches missing blobs from the forge, verifies every file's Git SHA-1 blob ID, and plans those exact bytes.

## Forges and their tokens

```hcl
forge "gitlab" {
  kind            = "gitlab"
  url             = "https://gitlab.com"
  read_token_file = "/run/secrets/writ/gitlab_read"
}
```

- `kind`: `"gitlab"` only (`"github"` is refused until built).
- `url`: `https://` (or loopback `http://`).
- `read_token_file`: token with `read_api` scope, re-read on every call (rotate without restart), sent only via `PRIVATE-TOKEN` with redirects disabled and pagination reading only the `Link` page token.
- `ca_file`: optional root CA PEM bundle for self-managed GitLab.
- `write_token_file`: optional, emits a warning (merge request comments aren't built yet).

| GitLab tier | Recommended read token |
| --- | --- |
| Self-Managed, Dedicated, GitLab.com Premium/Ultimate | Group or project access token, Reporter role, `read_api` |
| GitLab.com Free | Bot user personal access token, Reporter role on group, `read_api` |

Don't use deploy tokens (GitLab's REST API rejects them).

Configure polling with `poll_every` (default `60s`, min `1s`) and `poll_every_with_hook` (default `5m` for webhook-enabled repos). `writ serve --dev --config FILE` accepts `forge`, `repo`, `poll_every`, `poll_every_with_hook`, `tofu`, and `policy`.

**Limits:** `read_api` coverage of GitLab's tree and raw blob endpoints hasn't been verified against live GitLab yet ([test plan](development.md#test-plan-for-a-real-gitlab-forge)).

## What the control plane fetches

On commit discovery, Writ lists the tree under `path` and fetches `.terraform.lock.hcl` files only (commits containing `.writ` files are refused). Remaining stack and local module files are fetched on demand when planning ([What a stack can contain](serve.md#what-a-stack-can-contain)).

- Uses GitLab keyset pagination (`GET /projects/:id/repository/tree?...&recursive=true&pagination=keyset&per_page=100`) and `GET /projects/:id/repository/blobs/:sha/raw` for uncached blobs.
- Caches blobs by Git SHA-1 in `blobs`, so subsequent commits fetch only changed files.
- Verifies every blob against `sha1("blob <size>\0<content>")`; mismatches refuse the commit.
- Refuses symlinks under `path` and ignores submodules.
- Never echoes raw forge response text in API answers, logs, or database rows; read tokens are redacted from errors and rejected if echoed back.

### When fetched files are stored

Blobs commit only after the entire commit passes validation, so refused commits leave no orphaned blobs. Blob writes run under the server context (30-second budget, stopping cleanly between blobs on `SIGTERM`); any skipped blob is simply re-fetched on the next read.

### Size and time limits on a fetch

- `10s` per HTTP call.
- `2,000` lock files / `16 MiB` total lock files per commit; `1 MiB` per individual lock file (refuses only that stack).
- `20,000` tree entries; `1 MiB` per tree page; `8 MiB` for other API responses.

## How the control plane learns about commits

1. **CI jobs:** Merge request jobs send `$CI_COMMIT_SHA` to `POST /v1/plans`.
2. **Polling:** Every `poll_every`, Writ checks each repository's default branch head and runs discovery when it moves (poll errors stay visible in `writ repo show` and `/ui/repos`).
3. **Webhooks:** Pinned repositories with `webhook_secret_file` trigger an immediate API head read on GitLab push events and slow background polling to `poll_every_with_hook` ([Get changes in seconds with a webhook](webhooks.md)).

## How Writ finds your stacks

Any directory under `path` with a committed `.terraform.lock.hcl` and `.tf`/`.tofu` files is discovered as an OpenTofu stack. When `tofu` is configured, every discovered stack is served in every configured environment. Adding a repository with zero stacks under `path` is refused.

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

`writ plan --remote` sends the checked-out commit when the repository is registered, the stack sits under `path`, no file under `path` differs from `HEAD`, and (for remote forges) the commit is pushed to a remote branch.

### From a CI job

```sh
writ plan --remote --env prod --stack network --commit "$CI_COMMIT_SHA"
```

Writ matches the token's issuer URL and `project_id` to the registered repository (`--repo` disambiguates multiple paths in one project), requires `commit == sha`, rejects fork MRs (unmatched `project_id`), and verifies via merge base that the commit belongs to `ref_path` (`ref_check: "on-ref"`, or `"no-ref"` when no ref is supplied).

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

- [Add a repository](add-repository.md)
- [Approvals](approvals.md)
- [Set up GitLab CI](set-up-gitlab-ci.md)
- [API: Repository routes](api.md#repository-routes) and [JSON output](json-output.md#documents-of-writ-serve)
