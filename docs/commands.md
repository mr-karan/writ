# Commands

`writ` is a single binary for running the control plane (`writ serve`) and operating against it. Plans and applies execute on `writ serve`.

```sh
writ [--dir DIR] [--env ENV] [--server URL] [--verbose] <command> [flags] [args]
```

## Commands

| Command | Purpose |
| --- | --- |
| `writ serve --config FILE` | Run a team control plane. `--check` validates config, policies, isolation, and notifiers without serving; `--check --notify-test` sends a test event to each notifier ([Run a control plane](run.md)). |
| `writ serve --dev` | Run a single-user loopback control plane backed by SQLite (`$XDG_DATA_HOME/writ/dev`). `--data`, `--listen`, and `--database` override storage/address; `--config FILE` accepts `forge`, `repo`, `poll_every`, `poll_every_with_hook`, `tofu`, and `policy` ([writ serve --dev](run.md#writ-serve---dev)). |
| `writ plan --remote` | Plan the current commit of a clean working tree on `writ serve`, wait, and print the masked plan (`--env` required outside `--dev`). |
| `writ plan --remote --stack NAME --commit SHA` | Plan a commit without a local checkout (CI mode; `--repo NAME` optional in CI). |
| `writ approve --remote ID` | Approve another person's plan (`--withdraw` revokes your approval before apply starts) ([Plan and apply](apply.md)). |
| `writ approve --remote --pipeline` | In CI, approve all live plans in the current pipeline atomically (passes with no-op if all plans are `no-changes`). |
| `writ apply --remote ID` | Apply an approved plan and follow it (`--override REASON` skips approval rules for a person with `override`). First Ctrl-C detaches without stopping the run; uses an idempotency key for lost-response recovery ([When an answer is lost](apply.md#when-an-answer-is-lost)). |
| `writ apply --remote --merged --env ENV --stack NAME` | Apply the approved plan whose sources match the merged default-branch commit (CI token `sha`, or `--repo` + `--commit`). Exits 0 if a current `no-changes` plan matches. |
| `writ apply --remote --yes` | Plan and immediately apply when no approval rule requires approval (exits 0 with no run on `no-changes`). |
| `writ show --remote ID` | Print a stored plan (by ID or ref) or apply run (by ID). |
| `writ show --remote --request KEY` | Query the outcome of an apply idempotency `KEY` and close it if no run started. |
| `writ plans --remote` | List plans in `--env` newest first (`--stack`, `--status`, `--started-by`, `--limit`, `--cursor`). |
| `writ runs --remote` | List apply runs in `--env` newest first (`--stack`, `--limit`). |
| `writ audit --remote` | Query audit rows newest first (`--plan`, `--run`, `--env` + `--stack`, `--person`, `--op`, `--changes`, `--since`, `--until`, `--limit`, `--cursor`) ([Read the audit log](audit-log.md)). |
| `writ state adopt STACK --env ENV --version N\|--serial N --reason TEXT` | Seal a secret set for the newest HTTP version or S3 serial lacking one (requires human session + `adopt`) ([Adopt a state](adopt-state.md)). |
| `writ repo add NAME FORGE/PROJECT` | Register a GitLab repository (`--path`, `--prefix`; requires global `admin`) ([Add a repository](add-repository.md)). |
| `writ repo add [NAME] DIR` | Under `writ serve --dev`, register a local Git repository (such as `.`). |
| `writ repo list`, `writ repo show NAME` | List registered repositories or inspect one repository's discovered stacks. |
| `writ ci gitlab --repo NAME` | Generate `.gitlab-ci.yml` jobs and server HCL config for a repository's project ID (`--envs`, `--image`, `--component`). |
| `writ ui` | Print the control UI URL (and a one-time 10-minute sign-in link under `writ serve --dev`). |
| `writ login [URL]` | Sign in via OAuth device flow or loopback PKCE browser flow (`--device`, `--browser`, `--issuer`), saving the session to `~/.config/writ/sessions.json` ([Sign in](sign-in.md#people-writ-login)). |
| `writ logout [URL]` | Revoke your session on `writ serve` and remove it from `sessions.json`. |
| `writ whoami` | Print selected control plane, token source, principal, teams, and grants (accepts `--remote`). |
| `writ import bulk SELECTION.json` | Read existing objects via pinned providers and write `imported.tf` + `imported.imports.json` without modifying state (`--type` + `--id` skips the JSON file) ([Import many objects](import-bulk.md)). |
| `writ import accept --imports FILE PLAN` | Verify a saved OpenTofu plan (or JSON plan for no-op imports) imports only the objects in `FILE` ([Import and accept](import-bulk.md#import-and-accept)). |
| `writ agent init` | Install the Writ skill into `.agents/skills/writ` and `.claude/skills/writ` and add a marked section to `AGENTS.md` (`--print`, `--force`) ([Use Writ with an agent](agents.md#set-up-a-repository)). |
| `writ mcp` | Run the stdio MCP server for stacks under `--dir` (`--allow-providers` enables `import_bulk` and provider downloads in `schema`) ([The MCP server](agents.md#the-mcp-server)). |
| `writ version` | Print the version. |
| `writ help [COMMAND]` | Print usage text. |

Running `plan`, `apply`, `plans`, `runs`, or `audit` without `--remote` exits 2 with a message showing the `--remote` form.

## Flags

| Flag | Commands | Meaning |
| --- | --- | --- |
| `--json` | all except `serve`, `mcp`, `ci`, `help` | Emit one JSON document on stdout (`--help`/`-h` still prints plain usage) ([JSON output](json-output.md)). |
| `--config FILE`, `--check` | `serve` | Load HCL config at `FILE`. `--check` validates config, policies, isolation, and notifiers without serving. |
| `--notify-test` | `serve` | With `--check`, send a test event to each `notify` destination and fail if any fails. |
| `--dev`, `--data DIR`, `--listen ADDR`, `--database DSN` | `serve` | Run single-user dev mode on loopback (`127.0.0.1:7340` default), storing files under `--data` (`$XDG_DATA_HOME/writ/dev`) and SQLite or Postgres at `--database`. Use `--config FILE` for `policy`, `tofu`, `forge`, `repo`, `poll_every`, and `poll_every_with_hook`. |
| `--out FILE` | `import bulk` | Output `.tf` path (default `imported.tf`). |
| `--imports FILE` | `import accept PLAN` | Manifest written by `writ import bulk` (for example `imported.imports.json`). |
| `--type TYPE`, `--id ID` | `import bulk` | Select objects inline without a selection file (repeat `--id`). |
| `--provider ADDR` | `import bulk` | Provider configuration alias (`NAME.ALIAS`). |
| `--max-objects N`, `--max-bytes N` | `import bulk` | Cap read objects (default `500`) and formatted output size (default `1 MiB`). |
| `--print`, `--force` | `agent init` | Preview files without writing (`--print`) or overwrite modified skill files (`--force`). |
| `--allow-providers` | `mcp` | Enable `import_bulk` and allow `schema` to download uncached providers. |
| `--yes` | `apply --remote` | Plan and apply immediately if no approval rule matches. |
| `--merged` | `apply --remote` | Apply the approved plan matching the merged default-branch commit's sources. |
| `--override REASON` | `apply --remote` | Skip approval rules with a 10 to 500 character reason (requires `override`; never bypasses mandatory policy). |
| `--withdraw`, `--pipeline` | `approve --remote` | Revoke your approval (`--withdraw`) or approve all live plans in the CI pipeline (`--pipeline`). |
| `--request KEY` | `show --remote` | Query or close an apply request idempotency key. |
| `--version N`, `--serial N`, `--reason TEXT` | `state adopt` | Target HTTP state version or S3 serial and 10 to 500 character adoption reason. |
| `--device`, `--browser`, `--issuer NAME` | `login` | Force device or PKCE browser flow, or choose among multiple `people` issuers. |
| `--path DIR`, `--prefix PREFIX` | `repo add` | Subdirectory for stack discovery and prefix prepended to stack names. |
| `--repo NAME`, `--envs a,b`, `--image IMAGE`, `--component PATH` | `ci gitlab` | Repository name and overrides for generated `.gitlab-ci.yml`. |
| `--remote` | `plan`, `apply`, `approve`, `show`, `plans`, `runs`, `audit`, `state`, `whoami` | Target `writ serve` (`state` and `whoami` always target it). |
| `--limit N` | `plans`, `runs`, `audit` (`--remote`) | Page size, 1 to 200 (default `50`). |
| `--status STATUS` | `plans --remote` | Filter by effective plan status (`pending`, `applied`, `no-changes`, etc.). |
| `--started-by person\|ci` | `plans --remote` | Filter by initiator kind. |
| `--plan ID`, `--run ID`, `--person ID`, `--op OP` | `audit --remote` | Filter audit rows by plan, run, person ID, or operation. |
| `--changes` | `audit --remote` | Hide routine read requests (`read`, `state-read`, allowed read-only `GET`/`HEAD`). |
| `--since TIME`, `--until TIME` | `audit --remote` | RFC 3339 lower (inclusive) and upper (exclusive) time bounds. |
| `--cursor C` | `plans`, `audit` (`--remote`) | Pagination cursor from the previous page. |
| `--stack NAME` | `plan`, `apply`, `plans`, `runs`, `audit` (`--remote`) | Target stack name (`audit` requires `--env` alongside `--stack`). |
| `--hint-merge-request N` | `plan --remote` | Optional merge request number hint (recorded for MR pipelines only). |
| `--commit SHA`, `--repo NAME` | `plan`, `apply` (`--remote`) | Commit SHA and repository name without a local checkout. |

Global flags (accepted before or after the subcommand):

| Flag | Meaning |
| --- | --- |
| `--dir DIR` | Stack directory (default `.`). |
| `--env ENV` | Target environment (same as `WRIT_ENV`). |
| `--server URL` | Control plane URL or `dev` (implies `--remote`; takes precedence over `WRIT_SERVER` and sessions). |
| `--verbose` | Debug logging to stderr. |

## Output for people

Without `--json`, `writ plan --remote` and `writ apply --remote` print plain, uncolored text formatted by the control plane (control characters escaped):

- **Plans:** one line per changed resource (`+` create, `~` update, `-` delete, `-/+` replace with replace paths, `<-` import), summary counts, changed outputs, policy decision, and warnings.
- **Applies:** per-resource outcomes and summary on completion (re-printable via `writ show --remote RUN`).

## Exit codes

| Code | Meaning |
| --- | --- |
| `0` | Succeeded (including `no-changes` merged/`--yes` applies and warnings). |
| `1` | Operation failed: diagnostic, refused/failed plan or apply, server refusal, or runtime error. |
| `2` | Invalid command line or preconditions (unknown flag, missing arg, non-stack directory, no control plane). Note: `writ agent init` may write `AGENTS.md` before exiting 2 on a later symlink error. |
| `130` | Interrupted by `SIGINT` or `SIGTERM`. |

With `--json`, stdout always contains one JSON document on exit codes 0, 1, and 2.

## Version skew

Every `writ serve` response includes `X-Writ-Version`. If the server is a newer release than the CLI (any higher version prior to `1.0`, or higher major/minor after), `writ` warns once on stderr:

```
writ: warning: writ serve is v0.1.7 and this writ is v0.1.5: a newer writ serve can answer in ways an older writ cannot read; install writ v0.1.7, the version of writ serve
```

`dev` builds and older servers never trigger this warning. If a newer server returns an unrecognized status or shape, `writ` exits 1 naming both versions. Pin CI images to the server release ([Set up GitLab CI](set-up-gitlab-ci.md#pin-the-cli-to-the-servers-version)).

## Environment variables

| Variable | Meaning |
| --- | --- |
| `WRIT_ENV` | Default environment (same as `--env`). |
| `WRIT_SERVER` | Control plane URL (checked after `--server`). |
| `WRIT_ID_TOKEN` | CI OIDC token (checked before `writ login` sessions). |
| `XDG_CONFIG_HOME` | Base directory for `writ/sessions.json` (default `~/.config`). |
| `XDG_DATA_HOME` | Base directory for `writ/dev` and `client.json` (default `~/.local/share`). |
| `WRIT_CACHE_DIR` | Provider binary and schema cache used by `writ import bulk` and `writ mcp` `schema`. |
| `WRIT_CPUPROFILE` | Write a Go pprof CPU profile to this path on exit. |
| `PGPASSFILE` | Postgres password file read by `writ serve` (default `~/.pgpass`). |
| `DISPLAY`, `WAYLAND_DISPLAY` | Checked by `writ login` on Linux before calling `xdg-open`. |
| `USER` | Local display name under `writ serve --dev` (`dev/USER`, fallback `local`). |
| `NO_COLOR` | Non-empty disables terminal color. |

`writ import bulk` and `writ mcp`'s `import_bulk` pass the caller's environment to providers (so `AWS_PROFILE` and similar provider variables work).
