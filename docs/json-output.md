# JSON output

Every `writ` command but `writ serve`, `writ mcp`, `writ ci`, and `writ help` takes `--json` and prints one JSON document on stdout. `--help` and `-h` print plain usage text and exit 0 even with `--json`. The sections below are the contract for each document. Logs go to stderr, so stdout is always exactly one document, on every exit code.

## Rules for every document

- `format` gives the shape and its version, such as `writ.remote-plan/v0`. Within a version, a field is only ever added, never removed or retyped. Ignore fields you don't know.
- `ok` is true when the command succeeded. It matches the exit code: `ok: true` exits 0, and `ok: false` exits 1, or 2 when the command couldn't start as given.
- A document with `ok: false` says why in `diagnostics`, or, for an apply, in `status`.
- Writ's own lists are always present. An empty list is `[]`, never `null` or missing. The masked plan document isn't one of these lists' documents: it's Writ's reduced form of OpenTofu's plan JSON. It keeps only the keys the mask lists, has `variables` as the list of variable names, always has `errored`, `variables`, `configuration`, and `resource_changes`, and a value or list in it can be `null`.
- The [mask](secrets.md) replaces with `(sensitive)` each secret it knows in every document of a plan, a run, the state index, and their diagnostics: a value OpenTofu marks sensitive, the value of a variable declared `sensitive = true`, a value `writ serve` gave the run, and the value of an attribute with a name that says it contains a secret. A secret it doesn't know reaches these documents in the clear. [What the mask can't do](secrets.md#what-the-mask-cant-do) lists those cases.

## Exit codes, and one document always

| Exit code | Meaning | `ok` | The diagnostic's `class` |
| --- | --- | --- | --- |
| 0 | The command succeeded, including `writ apply --remote --merged` or `--yes` whose plan changes no resource, so there is nothing to apply. | `true` | Warnings only, if any |
| 1 | The command failed: a diagnostic, a refused or failed plan or apply, a request that `writ serve` refused, a plan that `writ apply --remote --yes` can't apply because it needs approvals, or an error that stopped it before it could do its work. | `false` | Any class |
| 2 | The command couldn't start as given: an unknown command or flag, a missing argument, a value that doesn't parse, a directory that isn't an OpenTofu stack, or no control plane to send it to. It doesn't mean nothing changed: `writ agent init` can write `AGENTS.md` before it exits 2. | `false` | `usage` |

With `--json`, stdout has exactly one document on all three. A command that stops before it can do its work prints an empty document with `ok: false` and one diagnostic: its own format for `writ plan`, `writ apply`, `writ state adopt`, `writ plans`, `writ runs`, `writ audit`, `writ import`, `writ agent init`, `writ login`, and `writ logout`, and `writ.error/v0` for `writ whoami`, `writ show`, `writ approve`, `writ repo`, and `writ ui`. Here's `writ plan --json` with no `--remote`, which exits 2:

```json
{"format":"writ.remote-plan/v0","ok":false,"plan":null,"document":null,"text":"","policy":null,"network":null,"diagnostics":[{"severity":"error","class":"usage","message":"plan runs on the control plane: writ plan --remote"}]}
```

Output of a real run, 2026-11-20.

Two classes exist for these documents:

- `usage`: the input was wrong, and the caller can fix it. When the command line was wrong, or the command couldn't start, the command exits 2 and stderr says `run writ help for usage`; some documents also carry that advice in the diagnostic's `fix`, and the documents of `plan`, `apply`, and `runs` don't. A `usage` diagnostic in a document that the command printed after it started exits 1: a request that `writ serve` refused, such as one that names an environment that isn't a name, an OpenTofu step that failed in a plan or an apply, an apply that ran past `limits.apply_timeout`, or an `AGENTS.md` with markers that `writ agent init` can't read. A `usage` warning, such as a file that `writ agent init` kept, fails nothing.
- `internal`: the command couldn't run for a reason that isn't a finding about the stack: a file it couldn't read or write, or a bug in writ. The message is the error. A bug also prints a stack trace on stderr.

- `--json` is read from the raw command line, so a command line that doesn't parse is answered in JSON too. A command line with no command that writ knows prints `{"format":"writ.error/v0","ok":false,"diagnostics":[...]}`.
- The same message also goes to stderr as `writ: <message>`.
- `--help` prints usage text, not JSON.

## Diagnostic

Every document that can fail carries `diagnostics`, a list of:

```json
{"severity":"error","class":"usage","message":". is not an OpenTofu stack: writ import bulk needs .tf files, a .terraform.lock.hcl, and no .writ file; run tofu init first","fix":{"message":"run writ help for usage","safe":false,"edits":[]}}
```

Output of a real run, 2026-11-16: the `diagnostics` of `writ import bulk SELECTION.json --json` in a directory with no lock file.

- `severity` is `error` or `warning`. Only errors fail a command.
- `class` is one of `syntax`, `provider`, `state`, `state-io`, `saved-plan`, `usage`, `internal`, `auth`, `denied`, and `policy`. `syntax` means the config of `writ serve` doesn't parse. `provider` means a provider failed, such as when `writ import bulk` reads an object, or a stack's lock file or a Git host's answer couldn't be read. `state` means the plan no longer fits the state: plan again. `state-io` means the control database was busy, unreachable, or failed: the plan is still good, and the same command may pass later. `saved-plan` means the control plane refused a saved plan: make the plan again. `usage` means the caller's input was wrong, and `internal` that the command couldn't run for another reason; see [exit codes](#exit-codes-and-one-document-always). `auth` and `denied` come from `writ serve`: `auth` means the request had no token or its token failed a check, `denied` means no grant allows it, and `policy` means a [policy](policy.md) refused the apply. [Errors](errors.md) lists the common messages of each class and what to do.
- `file`, `line`, `col`, `end_line`, and `end_col` are omitted when the diagnostic has no source location, as for every diagnostic of `writ serve`. `file` is a path relative to the stack directory.
- `fix` is omitted when writ knows no fix. A fix is advice: `fix.message` says what to do, such as `run writ help for usage`.
- `fix.safe` is always `false`, and `fix.edits` is always `[]`. Both stay in the document for compatibility, so a reader that expects them still parses it.
- The documents of `writ serve`, and the commands that print one, carry diagnostics with `severity`, `class`, and `message` only.

A plan or run that a build from before the first release kept can carry a class that no longer exists, such as `type` or `goal`.

## `writ import bulk --json` (`writ.import-bulk/v0`)

`writ import bulk` reads many objects into an OpenTofu stack, writes one new `.tf` file that declares them and one that records them, writes no state, and plans nothing. [Import many objects](import-bulk.md) has the details.

- `file` is the new `.tf` file, `""` when none was written, and `imports_file` the file beside it that records each object for `writ import accept --imports`. `imported` is false and `run` is `""`, because no state is written, and `plan` is `null`.
- `objects` lists each object in the order the file declares it: `addr`, the OpenTofu address, `type`, OpenTofu's type name, `id`, the import id, and `origin`, the entry of the selection that picked it, such as `objects[1].values[0]`. When the import id is one of the object's own secrets, `id` is `""` and `secret_id` is true.
- `groups` counts the `for_each` groups, and `singles` the blocks of one object. `linked` counts the literals written as references, and `literals` the typed reference slots that kept a literal.
- `params` lists each variable the code declares for a sensitive value. The value is never printed.
- `problems` lists each object that wasn't imported or was skipped, and each field that needs attention: `kind` is `not imported`, `skipped`, or `imported, needs attention`, with `from`, the entry or address, and `reason`. An object `not imported` fails the run, and nothing is written.

## `writ import accept --imports FILE PLAN --json` (`writ.import-accept/v0`)

```json
{"format":"writ.import-accept/v0","ok":true,"objects":1,"imports":1,"refusals":[],"settings":[{"address":"nomad_job.nomad_job_import_web_default","attribute":"deregister_on_destroy","value":true,"provider":"registry.opentofu.org/hashicorp/nomad","version":"2.6.1"},{"address":"nomad_job.nomad_job_import_web_default","attribute":"deregister_on_id_change","value":true,"provider":"registry.opentofu.org/hashicorp/nomad","version":"2.6.1"},{"address":"nomad_job.nomad_job_import_web_default","attribute":"detach","value":true,"provider":"registry.opentofu.org/hashicorp/nomad","version":"2.6.1"},{"address":"nomad_job.nomad_job_import_web_default","attribute":"preserve_counts","value":false,"provider":"registry.opentofu.org/hashicorp/nomad","version":"2.6.1"},{"address":"nomad_job.nomad_job_import_web_default","attribute":"read_allocation_ids","value":false,"provider":"registry.opentofu.org/hashicorp/nomad","version":"2.6.1"},{"address":"nomad_job.nomad_job_import_web_default","attribute":"rerun_if_dead","value":false,"provider":"registry.opentofu.org/hashicorp/nomad","version":"2.6.1"}],"diagnostics":[]}
```

- `objects` counts the objects that `--imports` records, and `imports` those that the plan imports at their address and by their import id, as a no-op or as an update of provider-only settings alone.
- `refusals` lists each reason the plan isn't accepted: `address` is the resource change, `""` for the plan as a whole, and `reason` says what it plans. `ok` is true when there's none. An import update of a plan given as JSON, not as the plan file that `tofu plan -out` saved, is always refused, and its `reason` says to pass the saved plan file. A `reason` contains no secret of the plan: an attribute name that contains one shows `(sensitive)` in its place.
- `settings` lists each provider-only setting that an accepted plan sets in place on an object it imports, from the named list in [import-bulk.md](import-bulk.md): `address` is the resource change, `attribute` the setting, `value` the value the plan sets, and `provider` and `version` the provider it was measured on. It's empty when the plan isn't accepted.

`writ import` has two forms, and each has its own document. A command line that fails before the command reports prints `writ.import-bulk/v0` when its first argument that isn't a flag is `bulk` and the next is a `.json` file, `-`, or nothing, `writ.import-accept/v0` when it's `accept` and one more argument follows, and `writ.error/v0` otherwise.

## The `schema` tool of `writ mcp` (`writ.provider-schema/v0`)

The `schema` tool reads the cached schemas of the providers that a stack's `.terraform.lock.hcl` pins.

- `providers` lists each provider of the lock file with its `source` and `version`.
- `types` lists each type asked for: `type`, the resource type or `data.NAME` for a data source, `kind`, `provider`, and `schema`, the type's attributes and blocks as the provider describes them.
- `lists` has, for each provider asked for by name, its `resources` and `data_sources`.
- `diagnostics` carry `severity`, `class`, and `message`.

## `writ plans --remote --json` (`writ.remote-plans/v0`)

`writ plans --remote --json` prints the `writ.remote-plans/v0` document of `GET /v1/plans`, which [the API](api.md#routes) describes. Each plan has `started_by`, `person` or `ci`. While more plans match, `next` is the cursor of the next page.

## `writ runs --remote --json` (`writ.remote-runs/v0`)

`writ runs --remote --json` prints the `writ.remote-runs/v0` document of `GET /v1/applies`, which [the API](api.md#routes) describes.

## `writ audit --remote --json` (`writ.audit/v0`)

`writ audit --remote --json` prints the `writ.audit/v0` document of `GET /v1/audit`, which [Read the audit log](api.md#read-the-audit-log) describes.

## `writ agent init --json` (`writ.agent-init/v0`)

```json
{"format":"writ.agent-init/v0","ok":true,"files":[{"path":"AGENTS.md","action":"appended"},{"path":".agents/skills/writ/SKILL.md","action":"created"},{"path":".agents/skills/writ/reference.md","action":"kept"},{"path":".claude/skills/writ/SKILL.md","action":"created"},{"path":".claude/skills/writ/reference.md","action":"created"}],"diagnostics":[{"severity":"warning","class":"usage","message":"kept .agents/skills/writ/reference.md: it differs from what this writ writes","fix":{"message":"pass --force to replace it with this writ's","safe":false,"edits":[]}}]}
```

- `files` lists each path `writ agent init` writes or removes, from the repository root, in this order.
- `action` is `created` (the file wasn't there), `appended` (`AGENTS.md` had no Writ section and got one at its end), `updated` (Writ's section was rewritten, or with `--force` a skill file), `unchanged`, `kept` (the file differs and was left alone), or `removed` (an older `writ agent init` wrote it: `.agents/skills/writ/spec.md`, or the symbolic link at `.claude/skills/writ`, or with `--force` a symbolic link of the user's at a skill directory or a skill file, which is replaced). Each `kept` file has a `usage` warning.
- An `AGENTS.md` with broken markers, a marker without its pair or two sections, is a `usage` error. Nothing is written, `files` is `[]`, and `ok` is false.

## `writ version --json` (`writ.version/v0`)

```json
{"format": "writ.version/v0", "ok": true, "version": "v0.1.0"}
```

## Documents of `writ serve`

[The API](api.md#routes) answers with these documents. A remote plan's `document` is the masked plan document: OpenTofu's plan JSON after the [mask](secrets.md), with only the keys the mask lists, which [Policy](policy-reference.md#input) describes. A plan that ended without one has `{"format":"writ.plan.json/v2","ok":false,"diagnostics":[...]}` there instead. A run's `document` is `writ.tofu-apply/v0`, which [How an OpenTofu plan is approved and applied](approvals.md#how-an-opentofu-plan-is-approved-and-applied) describes, or `{"format":"writ.apply/v0","ok":false,"applied":false,"status":"apply-failed","diagnostics":[...]}` for an apply that couldn't start or that `writ serve` lost, as when it stopped during the run.

| Format | Route | Printed by |
| --- | --- | --- |
| `writ.server/v0` | `GET /v1/server` | |
| `writ.health/v0` | `GET /healthz` | |
| `writ.whoami/v0` | `GET /v1/whoami` | `writ whoami --json` |
| `writ.device/v0` | `POST /v1/auth/device` | |
| `writ.browser/v0` | `POST /v1/auth/browser` | |
| `writ.session/v0` | `POST /v1/auth/device/token`, `POST /v1/auth/browser/token`, `DELETE /v1/auth/session` | |
| `writ.sessions/v0` | `GET /v1/sessions`, `DELETE /v1/sessions/{id}` | |
| `writ.login/v0` | | `writ login --json` |
| `writ.logout/v0` | | `writ logout --json` |
| `writ.plan-request/v1` | the body of `POST /v1/plans` | |
| `writ.remote-plan/v0` | `POST /v1/plans`, `GET /v1/plans/{id}` | `writ plan --remote --json`, `writ show --remote --json` |
| `writ.remote-plans/v0` | `GET /v1/plans` | `writ plans --remote --json` |
| `writ.approve-request/v0` | the body of `POST /v1/approvals`, and optionally of `POST /v1/plans/{id}/approvals` | |
| `writ.approve/v0` | `POST /v1/plans/{id}/approvals`, `POST /v1/approvals`, `DELETE /v1/plans/{id}/approvals/mine` | `writ approve --remote --json` |
| `writ.run-request/v1` | the body of `POST /v1/runs` | |
| `writ.remote-run/v0` | `POST /v1/runs`, `GET /v1/runs/{id}` | `writ apply --remote --json`, `writ show --remote RUN --json` |
| `writ.remote-runs/v0` | `GET /v1/applies` | `writ runs --remote --json` |
| `writ.audit/v0` | `GET /v1/audit` | `writ audit --remote --json` |
| `writ.apply-outcome/v0` | `POST /v1/apply-requests/{key}/outcome` | `writ show --remote --request KEY --json`, and `writ apply --remote --json` when no run started or the route didn't answer |
| `writ.config/v0` | `GET /v1/config` | |
| `writ.notification/v0` | the body of a `webhook` notifier's `POST`, as [The payload](notifications.md#the-payload) describes | |
| `writ.policy/v0` | `GET /v1/policy` | the `policy` tool of `writ mcp` |
| `writ.repo-request/v0` | the body of `POST /v1/repos` | |
| `writ.repo/v0` | `POST /v1/repos`, `GET /v1/repos/{name}` | `writ repo add --json`, `writ repo show --json` |
| `writ.repos/v0` | `GET /v1/repos` | `writ repo list --json` |
| `writ.hook/v0` | `POST /v1/hooks/{forge}/{repo}` | none |
| `writ.ui-link/v0` | `POST /v1/dev/ui-link` | `writ ui --json` |
| `writ.drift/v0` | `GET /v1/drift` | |
| `writ.state-index/v0` | `GET /v1/index` | |
| `writ.overview/v0` | `GET /v1/overview` | |
| `writ.state-adopt-request/v0` | the body of `POST /v1/state/adopt` | |
| `writ.state-adopt/v0` | `POST /v1/state/adopt` | `writ state adopt --json` |
| `writ.policy-decision/v1` | `policy` inside `writ.remote-plan/v0` and `writ.remote-run/v0` | |
| `writ.tofu-apply/v0` | `document` inside `writ.remote-run/v0` | |
| `writ.apply/v0` | `document` inside `writ.remote-run/v0` when an apply fails to start or is lost | |
| `writ.plan.json/v2` | `document` inside `writ.remote-plan/v0` when a plan fails before producing a masked plan | |
| `writ.error/v0` | a route that doesn't exist, or pre-handler refusals on `/v1/policy` | CLI commands without a dedicated empty format on pre-start errors |

A refused request answers with its route's format, `ok: false`, and one diagnostic of class `auth`, `denied`, `usage`, `state`, `saved-plan`, `state-io`, `provider`, or `internal`. If `writ serve` can't write a request's audit row, it returns 503 with one `state-io` diagnostic, whatever it would have returned. An apply that a policy refuses answers 403 with a diagnostic of class `policy` that names the plan, and one of class `policy` for each result of the decision: an `error` for a result that denies, and a `warning` for one that warns, each `OUTCOME: [SET] MESSAGE`.

- `writ.server/v0` has `version`, `url`, `dev`, `issuers` (`name`, `kind`, `url`, `identity`), `login`, and `device_issuers`. `dev` is `true` for `writ serve --dev`, which is how a CLI finds the dev control plane, and `false` for every other. `login` is `true` when a people issuer exists, so `writ login` is set up. `device_issuers` lists the people issuers that offer the device flow, which `writ login` uses with them by default. It signs in with the others in a browser. A `writ serve` from before browser sign-in sends no `device_issuers`, and `writ login` uses the device flow with it.
- `writ.whoami/v0` has `principal` (`kind`: `ci-job`, `person`, or `dev`, `issuer`, `subject`, `display`, `person` (`IDENTITY/user/ID`, or `dev/USER` under `writ serve --dev`), `expires_at`, and `session`, the id of a person's session, absent for other kinds), `teams`, and `grants`.
- `writ.device/v0` has `issuer`, `user_code`, `verification_uri`, `verification_uri_complete` when the identity provider gave one, `handle`, `interval_seconds`, and `expires_at`. The handle refers to the sign-in in `POST /v1/auth/device/token` and works once.
- `writ.browser/v0` has `issuer`, `auth_url`, the identity provider's sign-in page for this sign-in, `handle`, and `expires_at`. The handle refers to the sign-in in `POST /v1/auth/browser/token` and works once.
- `writ.session/v0` has `status`: `pending` with `interval_seconds`, `signed-in`, or `ended`, and `session` once signed in or ended. `token` is set in one answer only: the poll, or the `POST /v1/auth/browser/token`, that signs the person in.
- `writ.sessions/v0` has `sessions`, each with `id`, `carrier` (`cli` or `browser`), `person`, `display`, `issuer`, `subject`, `teams`, `created_at`, `ends_at`, and `last_seen_at`, to the minute. It never contains a token.
- `writ.login/v0` has `server` and `session`, as in `writ.session/v0`, without the token. The prompts of `writ login --json` go to stderr.
- `writ.logout/v0` has `server`, `session` (the id), and `ended`, `false` when the session had already ended on the server.
- `writ.config/v0` has `url`, `issuers` (`name`, `kind`, `url`, `identity`), `teams` (`name`, `issuer`, `match` as `claim` and `values`), `grants` (`grant`, numbered from 1, `teams`, `stacks`, `envs`, `can`, `risks`), `envs` (`name`), `forges` (`name`, `kind`, `url`), `notifiers` (`name`, `kind`, `events`, `envs` and `stacks` when set, `drift_mode` when `events` has `drift`, `delivered`, `failed`, `last_event`, `last_ok_at`, `last_error_at`, and `error`, writ's own text for the last failure, `""` after a delivery succeeds), `poll_every`, `poll_every_with_hook`, `plan_ttl`, and `session_ttl` as Go durations, `ui`, and `admin`. `admin` has `limits` and `retention` for a principal with `admin`, and is `null` for every other. The document contains no file path, no database URL, and no secret. Under `writ serve --dev`, `envs` lists each environment that has a plan or a state.
- `writ.policy/v0` has `sets`, in config order, each with `name`, `level` (`mandatory` or `advisory`), and `files`, by name, each with `name`, relative to the set's directory, and `text`, the `.rego` file. It contains no path. The `policy` tool of `writ mcp` returns it with only the set its `set` names, and with `file` only that file of it.
- `writ.plan-request/v1` has `env`, `stacks` with one stack, `hints`, and `source`. `source` has `repo`, `ref`, and `commit`, the commit's full id. A CI job may send `{}`, and its token's project and `sha` fill it. A request with any other field, such as the retired `allow`, `target`, `exclude`, `replace`, `params`, or `bundle`, is refused. The plan of `writ.remote-plan/v0` has `source`: `{"kind": "commit", "repo", "ref", "commit", "ref_check"}`. `ref_check` is `on-ref` when the forge showed the commit is the ref's commit or an ancestor of it, and `no-ref` when the plan names no ref. A plan is refused when its ref doesn't contain its commit.
- The plan of `writ.remote-plan/v0` and of `writ.remote-plans/v0` has `started_by`: `person` for a plan a person started, from a session or as the dev user, and `ci` for a plan of a CI job's token, whose run `origin` names. Writ reads it from the author it stored with the plan, so a plan made before this field existed has it too.
- The plan of `writ.remote-plan/v0` has `status`: `planning`, `pending`, `approved`, `applying`, `applied`, `no-changes`, `refused`, `failed`, `expired`, or `superseded`, with `superseded_why` for the last. A `no-changes` plan has no `expires_at`, and its `approvals` need 0. `approvals` has `need`, `rules` (the 1-based positions of the approval rules that matched), `approved_by` (each `person`, `display`, `via`, and `at`), and `teams_that_may_approve`. `run` refers to the run that applied the plan. `GET /v1/plans/{id}` adds `history`, each change of status with `at`, `from`, `to`, `person` and `by` when a person made it, `run`, and `reason`. See [Track a plan's status](approvals.md#track-a-plans-status).
- `writ.remote-plan/v0` has `policy`, the plan point's `writ.policy-decision/v1` of a plan of an OpenTofu stack that a [policy](policy.md) evaluated, and `null` for any other plan. `writ.remote-run/v0` has `policy`, the apply point's decision that let the run begin, or `null`. A decision has `format`, `point` (`plan` or `apply`), `outcome` (`pass`, `warn`, or `deny`), `at`, `input`, the hex SHA-256 of the policy's input, at the apply point `approvals`, the hex SHA-256 of the ids and approvers of the approvals it judged, and `results`, each with `set`, `level` (`mandatory` or `advisory`), `rule` (the Rego rule, such as `deny_rsa`, or `error` when the set failed), `outcome` (`deny` or `warn`, what the result does), and `message`, swept by the mask and then cut at 1,000 bytes, or before a `(sensitive)` marker that the cut falls inside. A plan with the decision `deny` is `refused`.
- `writ.remote-plan/v0` has `network`, the `writ.network/v1` of the plan's run under `egress_mode` `report` or `enforce`, and `null` under `off` or when OpenTofu didn't run. `writ.remote-run/v0` has `network` of the run, `null` while it applies, under `off`, or when OpenTofu didn't run. `writ.network/v1` has `format`, `mode` (`report` or `enforce`), `egress`, the env's egress list when the run started, `destinations`, the provider install's first and then the run's steps', each part sorted by host, port, and verdict, each with `step`, `install` for a destination of the provider install and absent for one of a step, `host`, swept by the mask, or `(withheld)` when the mask couldn't read the plan, `port`, `verdict` (`allowed`, `unlisted`, `refused`, or `unresolved`), `reason`, which an `allowed` destination doesn't have, and `count`, and `unrecorded`, the requests to destinations past the first 256 of the install and of the steps, which no entry names. [Egress](serve.md#egress) describes the verdicts, and [The provider install](serve.md#the-provider-install) the install.
- `writ.approve-request/v0` has `pipeline`, true to approve every live plan of the CI job's own pipeline. `writ.approve/v0` has `plans`, each with `id`, `env`, `stack`, `status`, `need`, and `approved_by`, as the plans are after the request. An approval of a pipeline lists its `no-changes` plans too, which it doesn't approve.
- `writ.run-request/v1` has `plan`, a plan's id, with an optional `override` reason, or `env`, `stacks` with one stack, and `merged`, `{"repo", "commit"}`, which a CI job may send as `{}`. `writ.remote-run/v0` has `run`, `document`, the run's document once the run ends and `null` before, and `text`, the apply as `writ apply --remote` prints it. The 200 of `POST /v1/runs` that starts no run, because the newest plan of the merged sources is `no-changes`, has `run` `null` and `plan`, that plan's id, and `text` says there is nothing to apply. `run` has `id`, `plan`, `status` (`applying`, `applied`, or `failed`), `env`, `stack`, `actor`, `approved_by`, `override`, `named_by` (`plan` or `merged`), `merged_commit`, `role_session`, `started_at`, and `ended_at`. A 503 of `POST /v1/runs` adds `outcome`: `none` when the request started no run, and `unknown` when `writ serve` can't tell. When `POST /v1/runs` is refused because its key is closed with no run, the refusal adds `key_closed: true`: a 409, or a 503 with `outcome` `none`. No request with that key starts a run, so a client sends a new key.
- `writ.audit/v0` has `rows`, newest first, and `next`, the cursor of the following page, `""` on the last. A cursor is opaque and works until `writ serve` restarts. Each row has `at`, `request` (the request id; a row `writ serve` wrote on its own has the plan's ref, the run's id, or an id of its own), `kind` (`request`, `started`, `plan-finished`, `apply-finished`, `sign-in`, `state-unlock`, and others), `method` (`OTHER` for a method outside the standard set and `LOCK` and `UNLOCK`), `route` (the route's pattern, such as `/v1/plans/{id}`, `""` for a row of no request), `status`, `principal` (the display name), `principal_kind`, `person`, `issuer`, `subject`, `via`, `teams`, `op`, `env`, `stacks`, `plan`, `run`, `risks`, `decision` (`allowed`, `denied`, `auth-failed`, `refused`, `error`, or `none`), `check` (the check a refused token or sign-in failed), `reason`, `reason_withheld`, `grants` (the 1-based positions of the grants that allowed it), and `elapsed_ms`. `reason` is `""` with `reason_withheld` `true` on a row whose `decision` is neither `allowed` nor `none`. A `state-write` row's `reason` leaves out the state's SHA-256. A row has no client address.
- `writ.apply-outcome/v0` has `key`, `outcome` (`started` or `none`, and `unknown` in a 503 or when `writ apply --remote` got no answer), and `run`, as in `writ.remote-run/v0` for `started`, else `null`.
- `writ.repo/v0` has `repo`, `commit`, `stacks`, and `diagnostics`. `repo` has `name`, `forge`, `kind` (`gitlab` or `dir`), `forge_url`, `project`, `project_id`, `path`, `prefix`, `default_branch`, `web_url`, `origin` (`api` or `config`), `added_by`, `added_at`, `head`, `head_read_at`, `polled_at`, `error`, why the last poll failed or `""`, `hook`, true when the config gives the repository a webhook secret file, and `hook_at`, when the last webhook delivery that passed its check arrived, left out before the first. `commit` is the head read, with `sha`, `read_at`, `files`, and `bytes`, or `null`. Each of `stacks` is an OpenTofu stack, a directory with a committed `.terraform.lock.hcl` and `.tf` or `.tofu` files, with `stack`, its name on the control plane, `dir`, its directory in the project, `served` and `not_served`, the environments this control plane plans it in and the ones it doesn't, and `providers` (`source`, `version`, and `runs`, whether this control plane runs that provider). `diagnostics` are discovery's.
- `writ.repos/v0` has `pinned`, set when the config lists the repositories, and `repos`, each the `repo` of `writ.repo/v0` with `stacks`, the count found at its head.
- `writ.hook/v0` has `repo`, the repository the delivery's path names, `queued`, true when a read of its default branch's head is queued, and `duplicate`, true when a delivery with the same id was acted on already, left out otherwise.
- `writ.ui-link/v0` has `url`, a control UI sign-in link with a code that works once, and `expires_at`.
- `writ.drift/v0` has `every`, the config's `drift.every`, such as `6h`, or `""` when `writ serve` runs no drift checks, `stacks`, by `env` and then `stack`, and `more`, true when the limit left readable stacks out. Each stack has `stack`, `env`, `backend` (`http` or `s3`), `resources` (the rows of its state index), `indexed_version` (the state version or S3 serial the index was read from, 0 when nothing indexed it), `indexed_at`, `outside_write` (`serial` and `seen_at` of the newest serial on S3 that no apply of `writ serve` wrote, or `null`), `checking` (true while a drift check plans it), and `check`, the newest [drift check](serve.md#drift-checks) that ended, or `null`. `check` has `started_by` (always `drift`: Writ's drift timer started it), `plan` (the id of its plan, never approved or applied, `""` when it made none), `status` (`clean`, `drifted`, or `failed`), `started_at`, `checked_at`, `repo` and `commit` (what it planned), `outside` (resources OpenTofu found changed outside it) and `code` (changes an apply of the code would make), `drifted_since` (when the first of an unbroken run of drifted checks ended, `""` when not drifted), `changes`, and `error` (why a failed check made no plan, or `drift unknown: no secret set for state version N of stack@env` for a check of a state with no secret set, which keeps nothing else). Each of `changes`, at most 200, has `address`, `kind` (`outside` or `code`), `action` (`create`, `update`, `delete`, `replace`, `forget`, or `import`), and `attributes`, the names of the top-level attributes that change, left out when none. It holds no value.
- `writ.state-index/v0` has `stack`, `env`, `backend`, `indexed_version`, `lineage` (of a state on S3, `""` on the http backend), `indexed_at`, `outside_write` as in `writ.drift/v0`, and `resources`, by address, each with `address`, `type`, `provider`, and the attributes of the [state index](serve.md#the-state-index)'s allow-list: `id`, `arn`, `identifier`, `region`, `namespace`, and `modify_index`, `""` for one the state doesn't hold as a string, marks sensitive, or the mask swept.
- `writ.overview/v0` has `envs`, each environment where the principal may read a stack, with `name`, `plans` (the count of its plans in each live status: `planning`, `pending`, `approved`, and `applying`, each as `GET /v1/plans/{id}` shows it, from at most 200 plans of each status that the principal may read), `plans_more` (true when the principal may read more, so the counts are floors and a plan about to expire may be missing from `alerts`), `alerts_more` (true when `alerts` leaves out drift alerts or failed runs of the environment), `failed` (runs that failed in the last 24 hours), `stacks` (stacks whose state `writ serve` keeps or reads), `checked` (those with a drift check that ended), `drifted` (those whose last check found drift), and `check_failed` (those whose last check failed). The counts of `failed` and of stacks take in every run and stack the principal may read. Every count, list, and flag leaves out what the principal may not read before it counts or cuts, so a stack, plan, or run the principal may not read changes nothing in the answer. `stacks`, by `env` and then `stack`, are the first 200 whose state `writ serve` keeps or reads, with `stacks_more` when more have state, and the stacks a live plan names that have no state yet, each with `env`, `stack`, `plans` as in `envs`, and `drift`, its row of `writ.drift/v0`, or `null` for a stack with no state yet. A stack with state past the first 200 is left out, never listed with `drift` `null`; its environment's `stacks` count holds it. `applying`, `pending`, and `planning` are the plans of that status as `GET /v1/plans/{id}` shows it, newest first, each `{"plan", "run"}`: `plan` as in `writ.remote-plans/v0`, and `run`, its run as in `writ.remote-runs/v0`, or `null` before one began. `alerts` each have `kind` (`drifted`, `drift-check-failed`, `outside-write`, `run-failed`, `plan-expiring`, or `repo-poll-failed`), `env`, `stack`, `plan`, `run`, and `repo`, `""` where the kind names none, `message`, what happened in words, and `at`: the drift alerts of at most 200 stacks of each environment, by `stack`, then each environment's newest 200 failed runs, newest first, then pending and approved plans that expire within 2 hours or a quarter of `plan_ttl`, whichever is shorter, then failed polls. Each environment's `alerts_more` is true when the limits left its drift alerts or failed runs out. `repos` has `count`, `polled_at`, the newest poll, and `failed`, each repository whose last poll failed with `name`, `error`, and `error_at`, or is `null` for a principal that may read nothing.
- `writ.state-adopt-request/v0` has `env`, `stack`, `reason` (10 to 500 characters), and one of `version`, a state version of a stack on the http backend, and `serial`, a serial of a stack's state on S3. The other is `0` or absent. `writ.state-adopt/v0` has `stack`, `env`, `backend` (`http` or `s3`), `version` (the state version on the http backend, `0` on S3), `serial` and `lineage` (of the state), `repo` and `commit` (the code whose providers read the state), `resources` (the rows of the state index the adoption read), `indexed` (`true` when the index took them), and one `warning` diagnostic of class `state` that names what the secret set can't hold.
- `writ.tofu-apply/v0` (the `document` of a completed `writ.remote-run/v0`) has `format`, `ok`, `applied` (`true` when OpenTofu finished applying), `status` (`applied` or `apply-failed`), `summary` (`add`, `change`, `import`, and `remove` counts), `events` (each with `type`, `address`, `action`, and `elapsed_seconds`), and `diagnostics` (all swept through the mask).

## On-disk JSON files

Writ also writes three versioned JSON files on disk:

- `writ.import-selected/v0` (`imported.imports.json`, written by `writ import bulk` and read by `writ import accept --imports`): `format` and `objects` (each with `addr`, `type`, `id`, and optional `provider`).
- `writ.sessions-file/v0` (`$XDG_CONFIG_HOME/writ/sessions.json`, mode `0600`, written by `writ login`): `format` and `sessions`, keyed by server URL.
- `writ.dev-client/v0` (`$XDG_DATA_HOME/writ/dev/client.json`, mode `0600`, written by `writ serve --dev`): `format`, `server`, `token`, and `pid`.
