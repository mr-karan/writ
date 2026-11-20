# Errors

Every error Writ prints has a class, such as `saved-plan` or `denied`. Scripts and agents branch on the class, and you read the message. Find the class below for what it means, its exit code and HTTP status, its common causes, and what to do.

## How an error looks

A request that [`writ serve`](glossary.md) refused prints one line: `error:`, the class, and the message. When `writ serve` knows what you can do, the message ends with it:

```
$ writ apply --remote --merged --stack app --repo demo --commit 7b47f2783365eda0f616386d196f42cf4fdf1897
error: saved-plan: no approved plan of app@dev matches the sources of demo at 7b47f278: plan the merge request again after a rebase, and approve the new plan
```

Output under `writ serve --dev`, 2026-11-16.

- Your CLI prints the message as `writ serve` sent it, with control characters escaped, such as a newline as `\u000a`.
- With `--json`, stdout has the document as `writ serve` sent it. [JSON output](json-output.md#diagnostic) has the fields.
- A warning prints `warning:` in place of `error:` and fails nothing.

A check of local files, such as `writ serve --check` or `writ import bulk`, prints the class in brackets and the place in the file. A fix it knows goes on a `fix:` line:

```
$ writ serve --config bad.hcl --check
error[syntax] bad.hcl:1:6
  unclosed configuration block: There is no closing brace for this block before the end of the file. This may be caused by incorrect brace nesting elsewhere in this file
```

A command line that `writ` can't run prints to stderr and exits 2:

```
$ writ plan
writ: plan runs on the control plane: writ plan --remote
run writ help for usage
```

Output of real runs, 2026-11-16.

## Classes at a glance

| Class | Means | Exit code | HTTP status |
| --- | --- | --- | --- |
| [`usage`](#usage) | The command line or the request is wrong, and you can fix it. | 2 for a wrong command line or input, else 1 | 400, 404, 405, 409, 413, 429 |
| [`auth`](#auth) | The request isn't signed in, or its token failed a check. | 1 | 401 |
| [`denied`](#denied) | No grant allows this, or the approvals are missing. | 1 | 403 |
| [`policy`](#policy) | A policy denies the apply. | 1 | 403 |
| [`state`](#state) | The plan no longer fits the state, or the key is closed. | 1 | 409, 404 |
| [`saved-plan`](#saved-plan) | The plan's sources don't allow this apply. | 1 | 409 |
| [`state-io`](#state-io) | The control database failed, or an answer was lost. The same command may pass later. | 1 | 503, 502 |
| [`provider`](#provider) | A provider, a lock file, or the Git host failed. | 1 | 502, 409 |
| [`internal`](#internal) | The command couldn't run for another reason, such as a file it couldn't read or a bug. | 1 | 500 |
| [`syntax`](#syntax) | The config of `writ serve` doesn't parse. | 1 | none |

Exit code 0 means success, with warnings only. [Commands](commands.md#exit-codes) has the exit codes, and [API](api.md#error-classes) has the statuses by route.

The messages below come from the code. Each value is a placeholder in capitals, such as `ID` for a plan id.

## `usage`

The command line or the request is wrong, and you can fix it.

- Exit code: 2 when the command line or the command's input is wrong, such as an unknown flag, a missing argument, a directory that isn't an OpenTofu stack, or no control plane to send to. Exit 1 when `writ serve` refused the request, or an OpenTofu step failed in a plan or an apply.
- HTTP status: 400 for a malformed body, key, or query. 404 for a plan, route, repository, or Git host object that doesn't exist or that you can't see. 405 for a method the route doesn't take. 409 for a request the config doesn't allow. 413 for a body past the size limit, or a pipeline with too many live plans to approve at once. 429 past `limits.requests_per_second` or `limits.applies`.

Common causes:

- `plan runs on the control plane: writ plan --remote`
- `no control plane: start one with writ serve --dev, or sign in to your team's with writ login URL`
- `no plan ID`
- `no route METHOD PATH`
- `limit must be 1 to 200`
- `plan ID is of env ENV, which this config no longer serves`
- `the config pins the repositories with repos, so they are added there and reviewed with the config`
- `more than N requests a second from PRINCIPAL`
- `writ serve is running N applies; try again when one ends`
- `the apply ran past limits.apply_timeout, DURATION: OpenTofu was interrupted, started no more changes, and wrote the state it reached; plan again`

What to do: fix the command line or the request, as the message says. Run `writ help` for usage. After a 429, try again later.

Exit code 2 doesn't mean nothing changed. `writ agent init` writes `AGENTS.md` first, so it can exit 2 after that, on a skill path that's a symbolic link.

## `auth`

The request has no token, or its token failed a check.

- Exit code: 1.
- HTTP status: 401.

Common causes:

- `no token: sign in with writ login, or send Authorization: Bearer with the OIDC token of a CI job`
- `the Authorization header is not Bearer TOKEN`
- `the session ended, ran out, or is not one writ login gave: sign in again with writ login URL`
- `the sign-in is unknown, finished, or ran out: run writ login again`
- `this token already applied; each CI token applies once`

What to do: run `writ login` again. A CI job sends a new `WRIT_ID_TOKEN` for each request that spends one: a plan, an approval, or an apply. [Token checks](api.md#token-checks) lists each check.

## `denied`

The token is good, but no grant allows the request, or the plan lacks approvals.

- Exit code: 1.
- HTTP status: 403.

Common causes:

- `PRINCIPAL may not apply plan ID: REASON`
- `PRINCIPAL may not approve plan ID: REASON`
- `PRINCIPAL made plan ID and cannot approve it: it needs 1 approval from someone else`
- `plan ID of STACK@ENV matches the sources at COMMIT and needs 1 approval; approve it with writ approve --remote ID`
- `a CI job applies by id only a plan of its own pipeline; after a merge, apply with merged`
- `a CI job applies its own commit: the token names COMMIT, not COMMIT`
- `commit COMMIT is not on REF of repository REPO, so it is not merged`
- `an override is a person's: a CI job cannot give one`
- `the key KEY started a run of another principal`

What to do: ask someone with `approve` on the `stack@env` to approve the plan, or ask an admin for the grant. A CI job acts only on its own pipeline, project, and commit. [Approvals](approvals.md) has the rules.

## `policy`

A [policy](policy.md) denies the apply. The first diagnostic names the plan, and one more follows for each result: an `error` for a result that denies, and a `warning` for one that warns.

- Exit code: 1.
- HTTP status: 403.

Common causes:

- `policy refuses the apply of plan ID`, then one diagnostic for each result, `OUTCOME: [SET] MESSAGE`.

What to do: change the code so the policy passes, then plan again. A result that only warns fails nothing. [Write a policy](write-a-policy.md) shows how a result is made.

## `state`

The plan no longer fits the state, or the request's key is closed. Plan again.

- Exit code: 1.
- HTTP status: 409. 404 when `writ state adopt` names a state version or serial that `writ serve` doesn't have.

Common causes:

- `plan ID is stale: plan ID of STACK@ENV is newer; plan again`
- `plan ID expired at TIME: plan again`
- `another run is applying to STACK@ENV; try again when it ends`
- `plan ID is a drift check of STACK@ENV, which writ serve never approves or applies: plan the stack to change it`
- `plan ID is a speculative plan, of files a client sent, which writ serve never approves: push the commit and plan it`
- `the key KEY is closed with no run: no run started; run the command again`
- `state version N of STACK@ENV is not its newest, version N: adopt the newest`
- `an apply of STACK@ENV runs, which may write a newer state: adopt after it ends`

What to do: plan again, or wait for the run that holds the `stack@env` and try again. [Why an approved plan can still be refused](approvals.md#why-an-approved-plan-can-still-be-refused) has each check.

## `saved-plan`

The control plane refused the plan it would apply, because the plan's sources don't allow this apply.

- Exit code: 1.
- HTTP status: 409.

Common causes:

- `no approved plan of STACK@ENV matches the sources of REPO at COMMIT: plan the merge request again after a rebase, and approve the new plan`
- `nothing to apply: plan ID, applied last to STACK@ENV, has the sources of REPO at COMMIT`
- `plan ID is superseded: REF moved to COMMIT, whose sources of STACK differ from the plan's, made at COMMIT; plan the new commit`
- `plan ID keeps no saved plan, so it cannot apply: plan its commit again`
- `plan ID is a speculative plan, of files a client sent, which a team's control plane never applies: push the commit and plan it`

What to do: plan the commit again, and get the new plan approved. `nothing to apply` means the merged commit's sources are already applied. [Apply after a merge](apply.md#apply-after-a-merge) shows how the plan is found.

## `state-io`

The control database failed, couldn't write the request's audit row, or an answer was lost. A `state-io` error doesn't tell you what happened to the request: an apply may still have started and finished. Before you try an apply again, ask what became of it with `writ show --remote --request KEY` when the message names a key, or look at the plan's runs.

- Exit code: 1.
- HTTP status: 503. 502 when the identity provider didn't start a sign-in.

Common causes:

- `the control database failed; the request id is ID`
- `the control database failed as run ID began, and writ serve cannot tell yet whether it started; ask with: writ show --remote --request KEY`
- `no run started: writ serve closed the key KEY of this command's requests with no run; run the command again`
- `writ serve did not say whether the apply request with the key KEY started a run; ask later with: writ show --remote --request KEY`
- `the identity provider of issuer ISSUER did not start a sign-in: REASON`

What to do: run the command again later. After a lost apply, first ask whether a run started, with `writ show --remote --request KEY`. Give an admin the request id, which the server's log and audit row also have. [When an answer is lost](apply.md#when-an-answer-is-lost) has the details.

## `provider`

A provider, a stack's lock file, or the Git host failed.

- Exit code: 1.
- HTTP status: 502 when the Git host failed; 409 when `POST /v1/state/adopt` could not read the stack's state on S3.

Common causes:

- `WHAT: the forge answered STATUS`
- `the lock file is over N bytes, so a plan of the stack is refused`
- `provider SOURCE finds no TYPE with that import id`
- `N objects could not be imported, so nothing was written`

What to do: check that the Git host answers and that the token can read the project, then try again. For a lock file, run `tofu providers lock` in the stack and commit the file. For `writ import bulk`, check the selection. [Import many objects](import-bulk.md) has the details.

## `internal`

The command couldn't run for a reason that isn't about your stack, such as a file it couldn't read or write, or a bug in Writ. The message is the error.

- Exit code: 1.
- HTTP status: 500.

Common causes:

- `writ serve hit a bug; the request id is ID`
- `the mask could not read the saved plan, so the run keeps none of OpenTofu's events: REASON`

What to do: read the message, fix the file or permission it names, and run the command again. For a bug, report it with the request id. A bug in the CLI also prints a stack trace on stderr, and the server's log has the error.

## `syntax`

The config of `writ serve` doesn't parse. The diagnostic gives the file, line, and column.

- Exit code: 1.
- HTTP status: none. `writ serve` doesn't start with a config that doesn't parse.

Common causes: the HCL parser's message, such as `unclosed configuration block`.

What to do: fix the config at the place it names, then check it with `writ serve --config FILE --check`. [Server config](config.md) has every block.

## Older classes

A plan or run that a build from before the first release kept can carry a class that no longer exists, such as `type` or `goal`.
