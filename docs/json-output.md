# JSON output

Every `writ` command except `writ serve`, `writ mcp`, `writ ci`, and `writ help` accepts `--json` and writes exactly one JSON document to stdout on exit codes 0, 1, and 2 (`--help` and `-h` print plain usage text and exit 0). Logs go to stderr.

## Rules for every document

- `format` identifies the document schema and version (for example `writ.remote-plan/v0`). Within a version, fields are only added, never removed or retyped; ignore unknown fields.
- `ok` matches the exit code: `true` exits 0; `false` exits 1 or 2.
- Failed documents explain why in `diagnostics` (and `status` for applies).
- Writ-owned arrays are always `[]` when empty, never `null` or omitted. The embedded masked OpenTofu plan document always includes `errored`, `variables` (names only), `configuration`, and `resource_changes`, and inner values or lists may be `null`.
- [The mask](secrets.md) replaces known secrets with `(sensitive)` across plans, runs, state index entries, and diagnostics. See [What the mask can't do](secrets.md#what-the-mask-cant-do) for limits.

## Exit codes, and one document always

| Exit code | Meaning | `ok` | Diagnostic `class` |
| --- | --- | --- | --- |
| `0` | Succeeded (including `no-changes` merged/`--yes` applies) | `true` | Warnings only, if any |
| `1` | Failed: diagnostic, refused/failed plan or apply, server refusal, `--yes` needing approval, or runtime error | `false` | Any class |
| `2` | Invalid CLI usage or preconditions (unknown flag, missing arg, non-stack dir, no control plane). Note: `writ agent init` may write `AGENTS.md` before exiting 2. | `false` | `usage` |

Pre-start failures emit an empty document with `ok: false` and one diagnostic (their own format for `plan`, `apply`, `state adopt`, `plans`, `runs`, `audit`, `import`, `agent init`, `login`, `logout`; `writ.error/v0` for `whoami`, `show`, `approve`, `repo`, `ui`, and unknown commands):

```json
{"format":"writ.remote-plan/v0","ok":false,"plan":null,"document":null,"text":"","policy":null,"network":null,"diagnostics":[{"severity":"error","class":"usage","message":"plan runs on the control plane: writ plan --remote"}]}
```

Output of a real run, 2026-11-20.

- `--json` is detected from raw `argv`, so parse errors still emit JSON (and mirror the error to stderr as `writ: <message>`).
- `usage` exits 2 on bad CLI invocation/preconditions (with `run writ help for usage` on stderr), and exits 1 on server refusals, failed OpenTofu steps, or apply timeouts. `usage` warnings never fail a command.
- `internal` indicates I/O failures or bugs (bugs also print a stack trace to stderr).

## Diagnostic

```json
{"severity":"error","class":"usage","message":". is not an OpenTofu stack: writ import bulk needs .tf files, a .terraform.lock.hcl, and no .writ file; run tofu init first","fix":{"message":"run writ help for usage","safe":false,"edits":[]}}
```

Output of a real run, 2026-11-16 (`writ import bulk SELECTION.json --json` without a lock file).

- `severity`: `error` or `warning` (only errors fail).
- `class`: `syntax`, `provider`, `state`, `state-io`, `saved-plan`, `usage`, `internal`, `auth`, `denied`, or `policy` ([Errors](errors.md)).
- `file`, `line`, `col`, `end_line`, `end_col`: omitted when there's no source location (always omitted in `writ serve` responses).
- `fix`: omitted when none exists; `fix.message` gives advice (`fix.safe` is always `false` and `fix.edits` is always `[]` for compatibility).

## `writ import bulk --json` (`writ.import-bulk/v0`)

- `file`, `imports_file`: generated `.tf` path (`""` if nothing written) and companion manifest path. `imported` is `false`, `run` is `""`, `plan` is `null`.
- `objects`: declared objects in file order (`addr`, `type`, `id`, `origin`; sensitive import IDs set `id: ""` and `secret_id: true`).
- `groups`, `singles`, `linked`, `literals`: counts of `for_each` groups, single resources, resolved references, and unlinked typed literals.
- `params`: generated sensitive variable names (never values).
- `problems`: `kind` (`not imported`, `skipped`, `imported, needs attention`), `from`, `reason`. Any `not imported` aborts without writing.

## `writ import accept --imports FILE PLAN --json` (`writ.import-accept/v0`)

```json
{"format":"writ.import-accept/v0","ok":true,"objects":1,"imports":1,"refusals":[],"settings":[{"address":"nomad_job.nomad_job_import_web_default","attribute":"deregister_on_destroy","value":true,"provider":"registry.opentofu.org/hashicorp/nomad","version":"2.6.1"}],"diagnostics":[]}
```

- `objects`, `imports`: manifest object count and accepted import count.
- `refusals`: list of `{address, reason}` (`ok` is true when empty; JSON plan inputs refuse any in-place update; attribute names containing secrets appear as `(sensitive)`).
- `settings`: accepted allow-listed provider-only settings (`address`, `attribute`, `value`, `provider`, `version`); empty when refused.

A `writ import` command line that fails before the command reports prints `writ.import-bulk/v0` when its first non-flag argument is `bulk` and the next is a `.json` file, `-`, or nothing; `writ.import-accept/v0` when it's `accept` and one more argument follows; and `writ.error/v0` otherwise. So `writ import bulk wrong.txt --json` exits 2 with `writ.error/v0`.

## The `schema` tool of `writ mcp` (`writ.provider-schema/v0`)

- `providers`: locked providers (`source`, `version`).
- `types`: requested types (`type`, `kind`, `provider`, `schema`).
- `lists`: per requested provider name, its `resources` and `data_sources`.
- `diagnostics`: `severity`, `class`, `message`.

## `writ plans --remote --json` (`writ.remote-plans/v0`)

Returns `GET /v1/plans` (`plans` with `started_by` `person` or `ci`, plus `next` cursor).

## `writ runs --remote --json` (`writ.remote-runs/v0`)

Returns `GET /v1/applies` ([API: Routes](api.md#routes)).

## `writ audit --remote --json` (`writ.audit/v0`)

Returns `GET /v1/audit` ([API: Read the audit log](api.md#read-the-audit-log)).

## `writ agent init --json` (`writ.agent-init/v0`)

```json
{"format":"writ.agent-init/v0","ok":true,"files":[{"path":"AGENTS.md","action":"appended"},{"path":".agents/skills/writ/SKILL.md","action":"created"},{"path":".agents/skills/writ/reference.md","action":"kept"},{"path":".claude/skills/writ/SKILL.md","action":"created"},{"path":".claude/skills/writ/reference.md","action":"created"}],"diagnostics":[{"severity":"warning","class":"usage","message":"kept .agents/skills/writ/reference.md: it differs from what this writ writes","fix":{"message":"pass --force to replace it with this writ's","safe":false,"edits":[]}}]}
```

- `files`: written or removed paths in order with `action` (`created`, `appended`, `updated`, `unchanged`, `kept`, `removed`). Each `kept` emits a `usage` warning; broken `AGENTS.md` markers emit a `usage` error with `files: []` and `ok: false`.

## `writ version --json` (`writ.version/v0`)

```json
{"format": "writ.version/v0", "ok": true, "version": "v0.1.8"}
```

## Documents of `writ serve`

Remote plan `document` holds the masked OpenTofu plan ([Policy input](policy-reference.md#input)), or `writ.plan.json/v2` (`ok: false`) if planning failed before producing one. Remote run `document` holds `writ.tofu-apply/v0` ([Approvals](approvals.md#what-the-runs-document-contains)), or `writ.apply/v0` (`status: "apply-failed"`) if the run couldn't start or was lost.

| Format | Route | CLI command |
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
| `writ.plan-request/v1` | `POST /v1/plans` request | |
| `writ.remote-plan/v0` | `POST /v1/plans`, `GET /v1/plans/{id}` | `writ plan --remote --json`, `writ show --remote --json` |
| `writ.remote-plans/v0` | `GET /v1/plans` | `writ plans --remote --json` |
| `writ.approve-request/v0` | `POST /v1/approvals`, `POST /v1/plans/{id}/approvals` request | |
| `writ.approve/v0` | `POST /v1/plans/{id}/approvals`, `POST /v1/approvals`, `DELETE /v1/plans/{id}/approvals/mine` | `writ approve --remote --json` |
| `writ.run-request/v1` | `POST /v1/runs` request | |
| `writ.remote-run/v0` | `POST /v1/runs`, `GET /v1/runs/{id}` | `writ apply --remote --json`, `writ show --remote RUN --json` |
| `writ.remote-runs/v0` | `GET /v1/applies` | `writ runs --remote --json` |
| `writ.audit/v0` | `GET /v1/audit` | `writ audit --remote --json` |
| `writ.apply-outcome/v0` | `POST /v1/apply-requests/{key}/outcome` | `writ show --remote --request KEY --json` (and lost `writ apply --remote --json`) |
| `writ.config/v0` | `GET /v1/config` | |
| `writ.notification/v0` | Outbound `webhook` notifier `POST` ([The payload](notifications.md#the-payload)) | |
| `writ.policy/v0` | `GET /v1/policy` | `writ mcp` `policy` tool |
| `writ.repo-request/v0` | `POST /v1/repos` request | |
| `writ.repo/v0` | `POST /v1/repos`, `GET /v1/repos/{name}` | `writ repo add --json`, `writ repo show --json` |
| `writ.repos/v0` | `GET /v1/repos` | `writ repo list --json` |
| `writ.hook/v0` | `POST /v1/hooks/{forge}/{repo}` | |
| `writ.ui-link/v0` | `POST /v1/dev/ui-link` | `writ ui --json` |
| `writ.drift/v0` | `GET /v1/drift` | |
| `writ.drift-check/v0` | `GET /v1/drift/{id}` | |
| `writ.state-index/v0` | `GET /v1/index` | |
| `writ.overview/v0` | `GET /v1/overview` | |
| `writ.state-adopt-request/v0` | `POST /v1/state/adopt` request | |
| `writ.state-adopt/v0` | `POST /v1/state/adopt` | `writ state adopt --json` |
| `writ.policy-decision/v1` | `policy` in `writ.remote-plan/v0` and `writ.remote-run/v0` | |
| `writ.network/v1` | `network` in `writ.remote-plan/v0` and `writ.remote-run/v0` | |
| `writ.tofu-apply/v0` | `document` in completed `writ.remote-run/v0` | |
| `writ.apply/v0` | `document` in failed-to-start/lost `writ.remote-run/v0` | |
| `writ.plan.json/v2` | `document` in failed `writ.remote-plan/v0` | |
| `writ.error/v0` | Unknown routes or pre-handler `/v1/policy` refusals | CLI pre-start errors without a dedicated format |

Refusals use the route's format with `ok: false` and one diagnostic (except policy refusals on `POST /v1/runs`, which return `403` with one summary diagnostic plus one `policy` diagnostic per result: `OUTCOME: [SET] MESSAGE`). Audit write failures always return `503` with one `state-io` diagnostic.

Key document fields:

- `writ.server/v0`: `version`, `url`, `dev`, `issuers` (`name`, `kind`, `url`, `identity`), `login`, `device_issuers`.
- `writ.whoami/v0`: `principal` (`kind`, `issuer`, `subject`, `display`, `person`, `expires_at`, optional `session`), `teams`, `grants`.
- `writ.device/v0`: `issuer`, `user_code`, `verification_uri`, optional `verification_uri_complete`, `handle`, `interval_seconds`, `expires_at`.
- `writ.browser/v0`: `issuer`, `auth_url`, `handle`, `expires_at`.
- `writ.session/v0`: `status` (`pending`, `signed-in`, `ended`), `interval_seconds` (when `pending`), `session`, and `token` (set only on the response that creates the session).
- `writ.sessions/v0`: `sessions` (`id`, `carrier`, `person`, `display`, `issuer`, `subject`, `teams`, `created_at`, `ends_at`, `last_seen_at`; never tokens).
- `writ.login/v0`: `server` and `session`, an object as in `writ.session/v0` without the token. Login prompts go to stderr.
- `writ.logout/v0`: `server`, `session` (the session id, a string), and `ended` (`false` when the session had already ended on the server).
- `writ.config/v0`: `url`, `issuers`, `teams`, `grants`, `envs`, `forges`, `notifiers` (`name`, `kind`, `events`, `envs`, `stacks`, `drift_mode`, `delivered`, `failed`, `last_event`, `last_ok_at`, `last_error_at`, `error`; no URLs/paths/secrets), `poll_every`, `poll_every_with_hook`, `plan_ttl`, `session_ttl`, `ui`, and `admin` (`limits` and `retention` for admins, else `null`).
- `writ.policy/v0`: `sets` (`name`, `level`, `files` with relative `name` and `.rego` `text`; no host paths).
- `writ.plan-request/v1`: `env`, `stacks` (one stack), `hints`, `source` (`repo`, `ref`, `commit`; `{}` allowed in CI). Unknown fields are refused.
- `writ.remote-plan/v0`: `plan` (`ref`, `id`, `status`, `started_by`, `env`, `stacks`, `author`, `origin`, `source` with `ref_check`, `risks`, `superseded_by`, `superseded_why`, `approvals`, `run`, `history` on `GET /v1/plans/{id}`), `document`, `text`, `policy` (`writ.policy-decision/v1` or `null`), `network` (`writ.network/v1` or `null`), `diagnostics`.
- `writ.policy-decision/v1`: `format`, `point` (`plan`/`apply`), `outcome` (`pass`/`warn`/`deny`), `at`, `input` (SHA-256), `approvals` (SHA-256 at apply point), `results` (`set`, `level`, `rule`, `outcome`, masked `message` <= 1,000 bytes).
- `writ.network/v1`: `format`, `mode` (`report`/`enforce`), `egress`, `destinations` (`step`, masked `host` or `(withheld)`, `port`, `verdict` `allowed`/`unlisted`/`refused`/`unresolved`, `reason`, `count`), `unrecorded` (overflow past 256 per phase).
- `writ.approve-request/v0` / `writ.approve/v0`: `pipeline` bool on request; `plans` (`id`, `env`, `stack`, `status`, `need`, `approved_by`) on response.
- `writ.run-request/v1` / `writ.remote-run/v0`: request sets `plan` (+ optional `override`) or `env` + `stacks` + `merged`. Response holds `run` (`id`, `plan`, `status`, `env`, `stack`, `actor`, `approved_by`, `override`, `named_by`, `merged_commit`, `role_session`, `started_at`, `ended_at`), `document`, `text`, `policy`, `network`, plus `outcome` on 503 and `key_closed: true` on closed-key refusals (`run: null` + `plan` on 200 no-op merged applies).
- `writ.audit/v0`: `rows` (`at`, `request`, `kind`, `method`, `route`, `status`, `principal`, `principal_kind`, `person`, `issuer`, `subject`, `via`, `teams`, `op`, `env`, `stacks`, `plan`, `run`, `risks`, `decision` (`allowed`, `denied`, `auth-failed`, `refused`, `error`, or `none`), `check`, `reason`, `reason_withheld`, `grants`, `elapsed_ms`; no client IP or state SHA-256) and `next`.
- `writ.apply-outcome/v0`: `key`, `outcome` (`started`, `none`, `unknown`), `run`.
- `writ.repo/v0`: `repo`, `commit`, `stacks`, `diagnostics`. `repo` has `name`, `forge`, `kind` (`gitlab` or `dir`), `forge_url`, `project`, `project_id`, `path`, `prefix`, `default_branch`, `web_url`, `origin` (`api` or `config`), `added_by`, `added_at`, `head`, `head_read_at`, `polled_at`, `error`, `hook`, and `hook_at` (left out before the first delivery). `commit` is the head read (`sha`, `read_at`, `files`, `bytes`) or `null`. Each of `stacks` has `stack`, `dir`, `served`, `not_served`, and `providers` (`source`, `version`, `runs`).
- `writ.repos/v0` / `writ.hook/v0` / `writ.ui-link/v0`: repository list, webhook status (`repo`, `queued`, `duplicate`), and dev UI sign-in links (`url`, `expires_at`).
- `writ.drift/v0`: `every`, `stacks`, `more`, and `checks`. Each of `stacks` has `stack`, `env`, `backend`, `resources`, `indexed_version`, `indexed_at`, `outside_write` (`serial`, `seen_at`, or `null`), `checking`, and `check`, the newest check that ended, or `null`. `check` has `ref`, `started_by`, `plan`, `status` (`clean`, `drifted`, `failed`), `started_at`, `checked_at`, `repo`, `commit`, `outside`, `code`, `drifted_since`, `changes` (at most 200, each `address`, `kind`, `action`, optional `attributes`; never values), and `error`. `checks` (`ref`, `plan`, `status`, `started_at`, `checked_at`, `commit`, `outside`, `code`) lists the kept checks newest first, and appears only when the request names both `env` and `stack`.
- `writ.drift-check/v0`: top-level `env`, `stack`, `backend`, `every`, and `check`, shaped as in `writ.drift/v0`.
- `writ.state-index/v0`: `stack`, `env`, `backend`, `indexed_version`, `lineage`, `indexed_at`, `outside_write`, `resources` (`address`, `type`, `provider`, plus allow-listed `id`, `arn`, `identifier`, `region`, `namespace`, `modify_index`).
- `writ.overview/v0`: authorized per-environment counts (`envs`; when `plans_more` is true, the plan counts are lower bounds), up to 200 stacks (`stacks`, `stacks_more`), active/pending plans (`applying`, `pending`, `planning`), `alerts` (`alerts_more`), and `repos`.
- `writ.state-adopt-request/v0` / `writ.state-adopt/v0`: request (`env`, `stack`, `reason`, `version` or `serial`); response (`stack`, `env`, `backend`, `version`, `serial`, `lineage`, `repo`, `commit`, `resources`, `indexed`, plus warning diagnostic).
- `writ.tofu-apply/v0`: `format`, `ok`, `applied`, `status` (`applied`/`apply-failed`), `summary` (`add`, `change`, `import`, `remove`), masked `events` (`type`, `address`, `action`, `elapsed_seconds`), masked `diagnostics`.

## On-disk JSON files

- `writ.import-selected/v0` (`imported.imports.json`): `format`, `imports` (`type`, `name`, `key`, `id`).
- `writ.sessions-file/v0` (`$XDG_CONFIG_HOME/writ/sessions.json`, mode `0600`): `format`, `sessions` keyed by server URL.
- `writ.dev-client/v0` (`$XDG_DATA_HOME/writ/dev/client.json`, mode `0600`): `format`, `server`, `token`, `pid`.
