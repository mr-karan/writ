# Server config

A team's `writ serve` reads one static HCL file. Every value must be a literal (no variables, functions, or `${...}`). Secrets never live in the config file; key and token settings point to paths on disk (relative paths resolve from the config file's directory). See [Run a control plane](run.md) to validate and start the server.

## Sample config

```hcl
// /etc/writ/serve
url           = "https://writ.example.com"
listen        = "0.0.0.0:443"
database      = "postgres://writ@/writ?host=/run/postgresql"
data          = "/var/lib/writ"
provider_uids = "61000-61063"
plan_ttl      = "24h"
session_ttl   = "12h"

tls {
  cert = "/var/lib/writ/tls/fullchain.pem"
  key  = "/var/lib/writ/tls/key.pem"
}

aws {
  region  = "eu-west-1"
  session = "1h"
}

limits {
  requests_per_second  = 10
  requests_per_address = 20
  plans_per_principal  = 4
  plans                = 8
  applies              = 4
  body                 = "16MiB"
  plan_timeout         = "30m"
  apply_start_timeout  = "10m"
  apply_timeout        = "4h"
  run_memory           = "4GiB"
  run_pids             = 1024
  run_cpus             = 2
  run_scratch          = "8GiB"
}

retention {
  audit = "400d"
  plans = "30d"
}

issuer "gitlab" {
  kind     = "gitlab-ci"
  identity = "gitlab"
  url      = "https://gitlab.com"
}

issuer "sso" {
  kind               = "people"
  identity           = "gitlab"
  url                = "https://gitlab.com"
  client_id          = "8f1c0b7e4d2a"
  client_secret_file = "/run/secrets/writ/oidc_client_secret"
  groups             = "userinfo"
}

team "infra_mr" {
  issuer = "gitlab"
  match  = { project_id = ["4242"], pipeline_source = ["merge_request_event"] }
}

team "platform" {
  issuer = "sso"
  match  = { groups = ["acme/platform"] }
}

team "infra_reviewers" {
  issuer = "gitlab"
  match  = { project_id = ["4242"], user_access_level = ["maintainer", "owner"] }
}

team "infra_main" {
  issuer = "gitlab"
  match = {
    project_id        = ["4242"]
    ref_path          = ["refs/heads/main"]
    ref_protected     = ["true"]
    ci_config_ref_uri = ["gitlab.com/acme/infra//.gitlab-ci.yml@refs/heads/main"]
  }
}

grant {
  teams  = ["infra_mr"]
  stacks = ["*"]
  envs   = ["staging", "prod"]
  can    = ["read", "plan"]
  risks  = []
}

grant {
  teams  = ["platform"]
  stacks = ["*"]
  envs   = ["staging", "prod"]
  can    = ["read", "approve"]
  risks  = []
}

grant {
  teams  = ["infra_reviewers"]
  stacks = ["*"]
  envs   = ["prod"]
  can    = ["approve"]
}

grant {
  teams  = ["infra_main"]
  stacks = ["*"]
  envs   = ["staging", "prod"]
  can    = ["read", "apply"]
  risks  = ["create", "update", "replace", "delete"]
}

approval {
  stacks = ["*"]
  envs   = ["prod"]
  risks  = ["create", "update", "replace", "delete", "data-loss"]
  need   = 1
}

approval {
  stacks = ["*"]
  envs   = ["*"]
  risks  = ["delete", "data-loss"]
  need   = 1
}

env "staging" {
  key        = "/var/lib/writ/keys/staging.key"
  plan_role  = "arn:aws:iam::111111111111:role/writ-plan"
  apply_role = "arn:aws:iam::111111111111:role/writ-apply"
}

env "prod" {
  key          = "/var/lib/writ/keys/prod.key"
  plan_role    = "arn:aws:iam::222222222222:role/writ-plan"
  apply_role   = "arn:aws:iam::222222222222:role/writ-apply"
  provider_env = { NOMAD_TOKEN = "/run/secrets/prod/nomad_token" }
}
```

## Settings and blocks

| Field | Meaning |
| --- | --- |
| `url` | Public URL of `writ serve` and sole accepted token audience (`https://`, or loopback `http://` behind a TLS proxy). |
| `listen` | Bind address (must be loopback when `tls` is omitted). |
| `tls` | Optional block: `cert` and `key` PEM file paths. |
| `database` | Postgres DSN (passwords forbidden in URL; use peer auth, `?passfile=`, or `PGPASSFILE`). |
| `data` | Working directory for server files (default `/var/lib/writ`). |
| `provider_uids` | Unused UID range `"FIRST-LAST"` (`1000 <= first <= last < 2^31`, at most `65,536` UIDs, at least `limits.plans` UIDs). Required when any `env` sets a role or `tofu` is configured ([Run isolation and egress](run-isolation.md)). |
| `aws` | Block (`region`, optional `sts_endpoint`, `session` `15m` to `12h`, default `1h`). Required when any `env` sets an AWS role. |
| `plan_ttl` | Positive duration a plan stays live before expiring (default `24h`). |
| `session_ttl` | Positive duration for human sessions with no refresh (default `12h`). |
| `ui` | Optional block: `enabled = false` disables `/ui/` (enabled by default when a `people` issuer exists; `enabled = true` refused without one). |
| `limits` | Concurrency, size, and timeout caps (defaults in sample above): `requests_per_second` (`>= 1`), `requests_per_address` (`>= 1`), `plans_per_principal` (`>= 1`), `plans` (`>= 1`), `applies` (`>= 1`), `body` (`1B` to `2GiB`, post-gzip), positive durations `plan_timeout`, `apply_start_timeout`, `apply_timeout`, and per-step run limits `run_memory` (`128MiB` to `1024GiB`), `run_pids` (`>= 64`), `run_cpus` (`0.1` to `1024`), `run_scratch` (`64MiB` to `1024GiB`). |
| `trusted_proxies` | Optional CIDR list of reverse proxies permitted to supply `X-Forwarded-For` ([Rate limits](api.md#rate-limits)). |
| `retention` | Optional block: positive durations `audit` and `plans` (kept forever when unset) ([Back up and upgrade](upgrades.md)). |
| `issuer "NAME"` | Trusted OIDC issuer (`kind`: `gitlab-ci`, `github-actions`, or `people`; `url`; `identity`; optional `ca_file`). |
| `team "NAME"` | Maps claims from an `issuer` to a team name ([Teams, grants, and approvals](#team-grant-and-approval)). |
| `grant` | Authorizes `teams` on `stacks` and `envs` for `can` operations (numbered from 1). |
| `approval` | Optional approval count rules (`stacks`, `envs`, `risks`, `need` `1` to `10`). |
| `env "NAME"` | Environment definition (`key`, optional `plan_role`, `apply_role`, `s3_endpoints`, `provider_env`, `egress`). |
| `forge "NAME"` | Optional Git host (`kind = "gitlab"`, `url`, `read_token_file`, optional `write_token_file`, `ca_file`). |
| `repo` | Optional pinned repository blocks (`name`, `forge`, `project`, optional `path`, `prefix`, `webhook_secret_file`). Disables `POST /v1/repos`. |
| `poll_every` | Repository default-branch poll interval (`>= 1s`, default `60s`). |
| `poll_every_with_hook` | Backstop poll interval for repositories with webhooks (`>= poll_every`; default `5m`, or `poll_every` if longer). |
| `tofu` | OpenTofu execution block (`binary`, `plugin_cache`, `scratch_dir`, optional `git`). |
| `drift` | Optional scheduled drift check block (`every >= 1m`; requires `tofu` and `limits.plans >= 2`). |
| `egress_mode` | `"off"` (default), `"report"`, or `"enforce"` (requires `tofu`) ([Run isolation and egress](run-isolation.md#egress)). |
| `provider_registries` | Allowed provider registry hostnames during cache fills under `report`/`enforce` (default `["registry.opentofu.org", "registry.terraform.io"]`). |
| `notify "NAME"` | Optional Slack or webhook notifier ([Get notified](notifications.md)). |
| `policy` | Optional Rego policy sets block ([Policy](#policy)). |

### issuer

- `identity`: lowercase provider name (never `dev`) shared by issuers representing the same Git host or IdP.
- `ca_file`: optional PEM root CA bundle for `url`.
- `kind = "people"` requires `client_id` and `client_secret_file`, and accepts `groups` (`"id_token"` default, or `"userinfo"`). Register `URL/ui/callback` (and `http://127.0.0.1/callback` for browser `writ login`) on the OAuth application ([Set up writ login](sign-in.md#set-up-writ-login)).

#### Who a person is

Writ identifies people as `IDENTITY/user/ID` using `user_id` (GitLab CI), `actor_id` (GitHub Actions), or `sub` (`people` ID token). Issuers for the same Git host share one `identity` so a person's CI jobs and CLI/UI sessions collapse to one ID, preventing cross-channel self-approval. Separate Git hosts must use separate identities (`writ serve --check` warns if different identities point to the same normalized host).

Renaming an identity renames every person in it. Sessions and plan authors under the old identity are refused until they sign in or plan again, and approvals kept under the old identity stop counting (rolling back the rename restores them).

### team, grant, and approval

- **`team "NAME"`:** `issuer` name plus `match` object mapping claim names to non-empty lists of allowed string values. All `match` keys must match; list claims match any element; numbers and booleans compare as JSON text; trailing `/*` matches one path segment and `/**` matches any depth; missing claims never match; a team with no rule has no members.
- **`grant`:**
  - `teams`: non-empty list of team names.
  - `stacks`: non-empty list of [stack patterns](#stack-patterns).
  - `envs`: non-empty list of configured environments or `["*"]`.
  - `can`: non-empty list from `read`, `plan`, `approve`, `apply`, `override`, `adopt`, `admin`.
  - `risks`: optional list of risk classes (`create`, `update`, `replace`, `delete`, `data-loss`) the grant permits applying (defaults to empty; grants with `apply` need the plan's risk classes covered by one or more matching grants).
- **`approval`:** `stacks`, `envs`, `risks`, and `need` (`1` to `10`; highest matching `need` wins).

#### Stack patterns

Stack patterns split on `/`. Within a segment, `*`, `?`, and `[...]` match characters without crossing `/`. A standalone `**` segment matches zero or more segments (`data/**` matches `data`, `data/pg`, and `data/pg/replica`; `**` matches every stack). At most one `**` segment is allowed per pattern (`apps**` and `a/**/b/**` are refused).

### env

- `key`: path to a 32-byte key file (raw 32 bytes or 64 hex chars) sealing HTTP state, saved plans, and secret sets.
- `plan_role`, `apply_role`: AWS IAM role ARNs assumed for plans/drift checks and applies.
- `s3_endpoints`: custom `https://` (or loopback `http://`) endpoint URLs allowed in `backend "s3"`.
- `provider_env`: map of env var names to secret file paths (read and masked per run).
- `egress`: allowed `HOST`, `*.DOMAIN` (subdomains only, >= 2 labels), or IP/IPv6 with optional `:PORT` (default `443`) under `egress_mode` `report`/`enforce`:

```hcl
egress_mode = "enforce"

env "prod" {
  key    = "/var/lib/writ/keys/prod.key"
  egress = ["*.amazonaws.com", "nomad.example.internal:80"]
}
```

### forge and repo

- **`forge "NAME"`:** `kind = "gitlab"`, `url` (`https://` or loopback `http://`), `read_token_file` (`read_api` scope), optional `ca_file`, and optional `write_token_file` (reserved for future write operations; warns if set). `read_token_file` is re-read on every call, so it rotates without a restart, and is sent only in `PRIVATE-TOKEN` with redirects disabled. Writ never echoes raw forge response text in API answers, logs, or database rows; read tokens are redacted from errors, and refused if a forge echoes them back.
- **`repo`:** `name` (`[a-z0-9][a-z0-9._-]*`), `forge`, `project` (`namespace/project`), optional `path`, optional `prefix` (ending in `/`), and optional `webhook_secret_file` (plain token or GitLab 19.1+ `whsec_` signing token, re-read on each delivery) ([Get changes in seconds with a webhook](webhooks.md)).

### notify

- **`notify "NAME"`** (`[a-z0-9_-]+`):
  - `kind`: `"slack"` or `"webhook"`.
  - `url_file`: file holding `https://` (or loopback `http://`) URL with no embedded credentials.
  - `secret_file`: optional HMAC-SHA256 signing key file (`kind = "webhook"` only).
  - `events`: non-empty subset of `drift`, `plan-failed`, `apply-failed`, `applied`, `outside-write`, `approval-needed`.
  - `envs`, `stacks`: optional environment names / `["*"]` and [stack patterns](#stack-patterns) (default: all).
  - `drift_mode`: optional (`"on-change"` default or `"every-check"`, valid only when `events` includes `drift`).

Files are read once at startup (restart after edits). Subscribing to `drift` without a `drift` block emits a warning.

### Notification delivery

Each notifier has its own 128-event queue and background sender, so slow receivers never block plans or applies:

- **Retries:** Timeouts, connection errors, temporary DNS failures, `429`, and `5xx` retry at 1s, 4s, 16s, 1m, and 3m. A `Retry-After` header on `429` extends the delay up to 5 minutes (and paces the next queued event if retries exhaust). Other 4xx statuses fail immediately.
- **Immediate operator errors:** Untrusted/expired/mismatched TLS certificates, plaintext HTTP on an HTTPS URL, and nonexistent hostnames fail immediately with explicit `tls:` or `dns:` errors.
- **Bounds:** 10-second per-attempt timeout, 16 KiB max request body, 4 KiB max response read (discarded).
- **Target rules:** URLs must be `https://` (or loopback `http://` for tests) with no embedded credentials. Redirects (`3xx`) fail.
- **Queue overflow:** When all 128 slots are full, new events drop with a log warning and count as failed on the next flush.
- **Restarts:** `url_file` and `secret_file` are read once at startup; restart `writ serve` after changing them. Unsent queued events don't persist across restarts.

`writ serve --check` verifies that `provider_uids` can't read `url_file` or `secret_file`. Notifications originate from `writ serve` itself (not OpenTofu runs), so environment `egress` allow-lists don't apply and `HTTPS_PROXY` is honored.

### tofu

- `binary`: OpenTofu 1.13.9 path or name on `PATH`.
- `plugin_cache`, `scratch_dir`: directories outside `data`.
- `git`: optional `git` path/name on `PATH` (must have `git-remote-http` and `git-remote-https` in its exec path; required for `git::https://` or `github.com/` modules).

Requires `provider_uids` so OpenTofu never executes as `writ serve`'s OS user.

## When drift checks run

With a `drift` block, Writ checks every OpenTofu `stack@env` with HTTP state, a state index, or a known S3 read. It looks for due checks at startup and every quarter of `every` (at most every 30 seconds), and also checks after an apply finishes, after it detects an outside S3 write, and after [`writ state adopt`](adopt-state.md). Each check refreshes every resource with `plan_role`, never takes the last free plan slot, skips stacks with an active apply, and is discarded and queued again if an apply starts mid-check. Writ keeps the newest 20 checks per `stack@env`. See [Enable drift checks](drift-checks.md).

## writ serve --dev defaults

| Area | Behavior |
| --- | --- |
| Store | SQLite `control.db` under `$XDG_DATA_HOME/writ/dev` (or `~/.local/share/writ/dev`). `--data DIR` moves it. `--database postgres://...` uses Postgres instead. |
| Listen | `127.0.0.1:7340` by default. `--listen` takes loopback addresses only; `:0` picks an ephemeral port. |
| Identity | A 32-byte random token made at start, written with the URL to `client.json` (`0600` in a `0700` directory). Full permissions: team `dev`, principal `dev/USER`. |
| Environments | Any valid name (`[a-z][a-z0-9_-]{0,62}`) works on demand and gets `keys/ENV.key`. `--env` defaults to `dev`. |
| Credentials | No AWS role assumption. Runs get `PATH=/usr/local/bin:/usr/bin:/bin` (prefixed with `tofu.git`'s directory if set), `AWS_EC2_METADATA_DISABLED=true`, the starting shell's `TF_VAR_*`, and a per-run `HOME`. |
| Isolation | No `provider_uids`: OpenTofu runs as your user. On Linux, runs use the read-only filesystem view, so they cannot read your other files, but they share your network. Elsewhere, OpenTofu has your user's filesystem access, and `--data` must keep `TMPDIR` short enough for macOS's 103-byte Unix socket paths. Plan only stacks you trust with that access. |
| Single instance | Refuses to start if another dev server answers for the same data directory. A clean shutdown removes `client.json`. |
| Control UI | `/ui/`, with a one-time 10-minute sign-in link printed at startup and by `writ ui`. |
| Repositories | `writ repo add .` registers a local Git repository. With `--config FILE`, dev mode also accepts `forge`, `repo`, `poll_every`, `poll_every_with_hook`, `tofu`, and `policy` ([Add a repository](add-repository.md)). |

## Database

`writ serve` keeps all control plane records and HTTP-backend state (the newest 20 versions per `stack@env`) in the Postgres schema `writ_control`. S3-backend stacks keep their state in S3. Never put a Postgres password in the `database` URL: use Unix-socket peer authentication, `?passfile=/path/to/pgpass`, or `PGPASSFILE`.

| Database | Timeouts |
| --- | --- |
| Postgres | 5s connect, 10s statement, 5s lock, 15s idle in transaction |
| SQLite (`--dev`) | 5s busy |

## Repository and stack rules

The rules [Add a repository](add-repository.md) applies.

### Rules for adding a repository

Adding a repository needs `admin` on every stack in every env, because repository code can define any stack name. Most teams instead pin repositories with `repo` blocks in `/etc/writ/serve`, so config review governs them. On `writ serve --dev`, the dev team can add any repository.

| Rule | Detail |
| --- | --- |
| Admin grant scope | Only grants with `envs = ["*"]` count, and their `stacks` patterns must match every depth up to 64 parts (the most a 128-character name has). A pattern of `*` parts matches one exact depth (`"*"`, `"*/*"`). Mixing `*` and `**` matches at least as many parts as the `*`s, so `stacks = ["**"]` alone covers every depth. |
| Unique names and targets | No two repositories share a `name`, or the same `project` and `path`. |
| Unique stack names | State is stored by stack name, so stack names are global per control plane. On a collision, pass `--prefix payments/` (or `prefix = "payments/"` in HCL) to get `payments/network` and so on. |
| Disjoint prefixes | No prefix may start with another's: `payments/` and `payments/eu/` are refused together. Two repositories may share the exact same prefix, or one may have none. Pinned `repo` blocks follow the same rule; offending entries are skipped and logged. |
| No stack under another's prefix | An unprefixed stack `a/x` is refused next to a repository with prefix `a/`, and the reverse. Polling enforces this on new commits: Writ refuses the commit, keeps the previous head, and records the error until it is fixed. |

Writ runs these checks inside the transaction that stores the repository, so two concurrent requests cannot both succeed.

### What a stack may not contain

Writ refuses a commit or stack, before OpenTofu runs, for any of these:

| Rule | Refused |
| --- | --- |
| Files | A missing `.terraform.lock.hcl`; a committed `.terraform/` directory; any `.tf.json`, `.tofu.json`, or `.writ` file; any symlink under `path`. Submodules are ignored. |
| Backend | Anything but an empty `backend "http" {}` or an allow-listed `backend "s3"` ([AWS roles and credentials](aws-credentials.md#2-grant-s3-state-permissions-if-using-backend-s3)). A missing backend and `cloud {}` are refused. |
| Module `source` | A non-literal source, or one that is not: a relative path (`./`, `../`) inside the repository `path`; a registry address (`[HOST/]NS/NAME/SYS[//SUBDIR]`); or a remote address (`git::`, `hg::`, `s3::`, `gcs::`, `http::`, `https::`, `github.com/`, `bitbucket.org/`). |
| Module address parts | Any query parameter but a literal `ref` and `depth` (so `checksum`, `archive`, `filename`, `sshkey`, `rev`, S3 credential parameters, and so on), `#` fragments, `file:` (even percent-encoded), backslashes, absolute paths, `~`, Windows and UNC paths, `oci://`. |
| Size and fetch limits | `1 MiB` per `.tf`, `.tofu`, `.tfvars`, or lock file; `16 MiB` per other file; `2,000` files or `64 MiB` across a stack and its local modules; `2,000` lock files (`16 MiB` total); `20,000` tree entries; `1 MiB` per tree page; `8 MiB` per other API response; `10s` per Git host HTTP call. |

## Stack names and prefixes

A repository's `prefix` prepends to each stack directory (`prefix = "payments/"` + `network` = `payments/network`). Grants, approvals, and state key off this full name. Writ rejects overlapping prefixes or stack names colliding with another repository's prefix ([Rules for adding a repository](#rules-for-adding-a-repository)).

## State

State locations are controlled by the stack's backend (`backend "http" {}` in Postgres, or `backend "s3"` in S3). API requests can't override state location.

## What writ serve --check refuses

`--check` fails with source file/line on:

- Unknown or mistyped settings/blocks, duplicate names, or removed legacy keys (`provider`, `providers_dir`, `limits.files`, `env.secret_env`, `env.state`).
- Undefined issuer, team, operation, risk class, or environment references; issuer missing `identity`.
- Malformed `trusted_proxies` CIDRs or host bits set past prefix length.
- Invalid `drift` (`every < 1m`, missing `tofu`, or `limits.plans < 2`), `tofu` without `provider_uids`, or out-of-range `limits.run_*`.
- Invalid `egress_mode`, `report`/`enforce` without `tofu` or with loopback `http://` `s3_endpoints`, invalid `egress` entries (single-label wildcards, loopback/link-local/metadata/multicast/unspecified/NAT64 IPs), or invalid `provider_registries` hostnames.
- Invalid stack patterns; `approve` grant to a CI team matching no user claim; `override` or `adopt` grant to any CI team.

`--check` warns on:

- Missing `tls` block without `trusted_proxies`.
- Team matching a repo path without numeric ID, or `apply`-capable team matching no ref.
- Missing `provider_uids`; distinct issuer identities pointing at the same host.
- `env.egress` or `provider_registries` set while `egress_mode = "off"`.
- Unenforceable cgroup run limits or unavailable seccomp filter ([Check that the limits hold](run-isolation.md#check-that-the-limits-hold)).

## Policy

`policy` loads host-side Rego directories at startup (never from stack commits). See [Write and test policies](write-a-policy.md).

```hcl
policy {
  timeout = "10s"
  memory  = "1GiB"

  set "baseline" {
    dir = "policy/baseline"
  }

  set "advice" {
    dir   = "policy/advice"
    level = "advisory"
  }
}
```

| Key | Meaning | Default |
| --- | --- | --- |
| `timeout` | Max total duration per evaluation decision | `"10s"` |
| `memory` | Max added address space for the policy worker (`>= 64MiB`) | `"1GiB"` |
| `set "NAME"` | One or more sets (`[a-z0-9_-]+`) | |
| `set.dir` | Directory of `.rego` files (relative to config file; excludes `*_test.rego`; max 1 MiB per set) | |
| `set.level` | `"mandatory"` (denies block) or `"advisory"` (denies warn) | `"mandatory"` |

`writ serve` and `writ serve --check` compile every set against Writ's input schema at startup and fail with exact locations on type or built-in errors:

```
policy set baseline: iam.rego:6:12: rego_type_error: undefined ref: input.planned_values.root_module.resources[__local0__]
```

`writ serve --dev --config FILE` accepts the same `policy` block.
