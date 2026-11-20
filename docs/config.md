# Server config

A team's `writ serve` reads one config file, written in HCL: settings such as `url = "..."`, and blocks such as `env "prod" { ... }`. It evaluates nothing, so every value is a literal: no variable, function, or `${...}`. It contains no secret: the state keys, the TLS key, and the files of `provider_env` are paths it points to. A relative path is relative to the config file's directory. [Run a control plane](run.md) shows how to start `writ serve` with it.

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
| `url` | The URL of `writ serve`, and the only audience a token can carry. `https://`, or `http://` on a loopback address behind a proxy that ends TLS. |
| `listen` | The address to listen on. Without `tls`, a loopback address. |
| `tls` | A block: `cert` and `key`, the certificate chain and key files. |
| `database` | The Postgres URL of the control database. It can't contain a password: point to a passfile with `?passfile=` or set `PGPASSFILE`. |
| `data` | A directory for work files. Default `/var/lib/writ`. |
| `provider_uids` | A range of uids from `1000` to `65533`, such as `"61000-61063"`, that no user on the host has, with at least `limits.plans` uids. Each running plan's providers run as one of them. Required when an environment sets a role or the config has a `tofu` block. See [Run isolation and egress](run-isolation.md). |
| `aws` | A block: how `writ serve` reaches STS. `region`, an optional `sts_endpoint` for an interface VPC endpoint, and `session`, how long credentials last, from `15m` to `12h`, `1h` by default. Required when an environment sets a role. |
| `plan_ttl` | How long a plan stays live, pending or approved, before it expires (`>= 1m`). Default `24h`. |
| `session_ttl` | How long a person's session lasts (`5m` to `168h`). There's no refresh. Default `12h`. |
| `ui` | A block. `ui { enabled = false }` turns the [control UI](sign-in.md#the-control-ui) off. It's on by default when an issuer of kind `people` exists, and refused when `enabled = true` with no `people` issuer. |
| `limits` | A block: `requests_per_second` (per principal, `>= 1`), `requests_per_address` (per client IP, `>= 1`), `plans_per_principal` (`>= 1`), `plans` (total concurrent plans, `>= 1`), `applies` (total concurrent applies, `>= 1`), `body` (largest request body after gzip decompression, `64KiB` to `64MiB`), `plan_timeout`, `apply_start_timeout`, and `apply_timeout` (each `>= 1s`). The defaults are in the sample above. See [Rate limits](api.md#rate-limits) and [Time limits](api.md#time-limits). |
| `trusted_proxies` | Optional. A list of CIDRs, such as `["127.0.0.1/32"]`, of the proxies in front of `writ serve`. `X-Forwarded-For` is read only on a request from one of them. See [Rate limits](api.md#rate-limits). |
| `retention` | A block: how long `audit` rows and spent `plans` are kept (each `>= 1h` when set). By default they're kept forever. See [Back up and upgrade](run.md#back-up-and-upgrade). |
| `issuer "NAME"` | A token issuer to trust, one block each: `kind` (`gitlab-ci`, `github-actions`, or `people`), `url`, `identity`, and an optional `ca_file` for an instance with a private CA. See below. |
| `team "NAME"` | A rule over token claims, one block each: `issuer`, and `match`, an object of claims and the values that match. See below and [Teams and grants](serve.md#teams-and-grants). |
| `grant` | What teams can do, one block each, numbered from 1 in the order written. See below. |
| `approval` | Optional. How many approvals a plan needs, one block for each rule, by `stacks`, `envs`, and `risks`, with `need` from 1 to 10. Without one, no plan needs an approval. See below and [Approvals](approvals.md#set-how-many-approvals-a-plan-needs). |
| `env "NAME"` | An environment, one block each: its state `key` file, and optionally `plan_role`, `apply_role`, `s3_endpoints`, `provider_env`, and `egress`. See below. |
| `forge "NAME"` | Optional. Where [repositories](repos.md#forges-and-their-tokens) are read from, one block each: `kind = "gitlab"`, `url`, `read_token_file` (a token with `read_api`), and optional `write_token_file` and `ca_file`. See below. |
| `repo` | Optional. Pins the list of repositories, one block each, with `name`, `forge`, `project`, and optional `path` and `prefix`. Without one, an admin adds them with `writ repo add`. See below. |
| `poll_every` | How often each repository's default branch is polled (`>= 5s`). Default `60s`. |
| `tofu` | Optional. A block: how `writ serve` runs OpenTofu for [stacks of `.tf` files](serve.md#opentofu-stacks). See below. |
| `drift` | Optional. A block: `every`, how often `writ serve` runs a [drift check](serve.md#drift-checks) of each OpenTofu `stack@env` it keeps state for, at least `1m`. It needs the `tofu` block and `limits.plans` of at least 2. Without it, `writ serve` runs no drift checks. |
| `egress_mode` | Optional. `"off"`, `"report"`, or `"enforce"`. Under `report` and `enforce`, each OpenTofu run gets a network namespace of its own, whose one way out is Writ's egress proxy: `enforce` refuses a destination its env's `egress` doesn't list, and `report` lets it through and records it. Default `"off"`: runs share the host's network, as they always have. `report` and `enforce` need the `tofu` block. See [Run isolation and egress](run-isolation.md#egress) to set it up and roll it out, and [Egress](serve.md#egress) for how it works. |
| `provider_registries` | Optional. A list of the registry hosts a provider install can ask under `egress_mode` `report` or `enforce`, such as `["registry.opentofu.org"]`. A stack's `.terraform.lock.hcl` chooses each provider's registry, so a plan whose lock file has a provider from another registry fails before the install. Each item is a host name as a provider's address starts with it, in lowercase, with no scheme or port. Default `["registry.opentofu.org", "registry.terraform.io"]`. See [The provider install](serve.md#the-provider-install). |
| `notify "NAME"` | Optional. A destination for [notifications](notifications.md), one block each: `kind`, `url_file`, `events`, and optional `secret_file`, `envs`, `stacks`, and `drift_mode`. See below. |
| `policy` | Optional. A block of Rego policy sets (`set "NAME" { dir = "..." }`) evaluated at the plan and apply points, with `timeout` and `memory` bounds. See [Policy](#policy) below. |

### issuer

- `identity` is the lowercase name of the Git host or identity provider that the user ids in the issuer's tokens come from, such as the name of a `forge` block. Issuers of one Git host share it. See [Who a person is](serve.md#who-a-person-is).
- `ca_file`: optional PEM bundle of root CAs trusted when connecting to `url`.
- An issuer of kind `people` is the identity provider people sign in to the control UI and `writ login` with. It also takes `client_id`, `client_secret_file` (the file that has the client secret; optional for public OAuth clients), and `groups`: `"userinfo"` reads the groups claim from the userinfo endpoint, and `"id_token"`, the default, from the ID token.
- The application whose id is `client_id` lists `URL/ui/callback` as a redirect URI for the control UI. For `writ login` through a browser, it also lists `http://127.0.0.1/callback`, which `writ login` needs when the identity provider has no device flow, as on GitLab.com. [Set up writ login](sign-in.md#set-up-writ-login) explains both.

### team, grant, and approval

- **`team "NAME"`**: `issuer` names a configured `issuer` block, and `match` is an object mapping non-empty claim names to non-empty lists of allowed string values (a token matches when every listed claim has one of its listed values).
- **`grant`**:
  - `teams`: non-empty list of configured `team` names.
  - `stacks`: non-empty list of [stack patterns](serve.md#stack-patterns) (such as `["*"]` or `["payments/*"]`).
  - `envs`: non-empty list of configured `env` names, or `["*"]`.
  - `can`: non-empty list of operations from `read`, `plan`, `approve`, `apply`, `override`, `adopt`, and `admin`.
  - `risks`: required when `can` includes `apply`, and forbidden otherwise. Lists the risk classes the grant permits applying, from `create`, `update`, `replace`, `delete`, and `data-loss`.
- **`approval`**:
  - `stacks`: non-empty list of stack patterns.
  - `envs`: non-empty list of environment names, or `["*"]`.
  - `risks`: non-empty list of risk classes (`create`, `update`, `replace`, `delete`, `data-loss`) that trigger the rule.
  - `need`: number of distinct approvals required, from `1` to `10`. When multiple rules match a plan, the highest `need` applies.

### env

- `key`: path to a 32-byte key file (raw 32 bytes or 64 hex digits) used to seal state on the `http` backend, saved plans, and secret sets.
- `plan_role`: the ARN of the AWS IAM role plans and drift checks assume.
- `apply_role`: the ARN of the AWS IAM role applies assume.
- `s3_endpoints`: a list of custom `https://` (or loopback `http://`) endpoint URLs a stack's [S3 backend](serve.md#stacks-on-s3) can point to.
- `provider_env`: an object of environment variables for providers, where each value is a file path whose contents are read and masked.
- `egress`: a list of the destinations the env's runs can reach under `egress_mode` `report` or `enforce`. Each item is `HOST`, `*.DOMAIN`, or an IP address, with an optional `:PORT`. An item with no port allows 443 only, so plain HTTP needs `HOST:80`. A wildcard matches subdomains, not the domain itself, and needs a domain of two labels or more. Write an IPv6 address with a port as `[ADDR]:PORT`. The list holds what the stack's providers and modules reach, not the providers' registry: see [What a run fetches](serve.md#what-a-run-fetches).

```hcl
egress_mode = "enforce"

env "prod" {
  key    = "/var/lib/writ/keys/prod.key"
  egress = ["*.amazonaws.com", "nomad.example.internal:80"]
}
```

### forge and repo

- **`forge "NAME"`**:
  - `kind`: `"gitlab"`.
  - `url`: base URL of the Git host (`https://`, or `http://` on loopback).
  - `read_token_file`: path to a file containing a token with `read_api` scope.
  - `write_token_file`: optional path to a file containing a write token (must differ from `read_token_file`).
  - `ca_file`: optional PEM bundle of root CAs for a private GitLab instance.
- **`repo`**:
  - `name`: unique repository name on the control plane (`[a-z0-9][a-z0-9._-]*`).
  - `forge`: name of a configured `forge` block.
  - `project`: `namespace/project` path on the forge.
  - `path`: optional subdirectory inside the repository under which stacks are discovered.
  - `prefix`: optional prefix prepended to every discovered stack name (such as `"payments/"`).

### notify

- **`notify "NAME"`**: a name of lowercase letters, digits, `_`, and `-`.
  - `kind`: `"slack"` for a Slack incoming webhook, or `"webhook"` for a JSON `POST` in the [`writ.notification/v0` format](notifications.md#the-payload).
  - `url_file`: path to a file containing the URL. It must be `https://`, or `http://` on a loopback address, with no user name or password.
  - `secret_file`: optional, for `kind = "webhook"` only. Path to a file containing the key that signs each request with [HMAC-SHA256](notifications.md#verify-the-signature).
  - `events`: non-empty list from `drift`, `plan-failed`, `apply-failed`, `applied`, `outside-write`, and `approval-needed`. See [Events](notifications.md#events).
  - `envs`: optional non-empty list of configured `env` names, or `["*"]`. Without it, every environment.
  - `stacks`: optional non-empty list of [stack patterns](serve.md#stack-patterns). Without it, every stack.
  - `drift_mode`: optional, only with `drift` in `events`. `"on-change"`, the default, sends `drift` when a stack's drift status changes; `"every-check"` sends it for every check that finds drift too.

`writ serve` reads both files once, when it starts, and fails to start when one can't be read or the URL isn't one it sends to. A config that subscribes to `drift` without a `drift` block gets a warning, because no drift check runs. `writ serve --check --notify-test` sends each notifier a test message.

### tofu

- `binary`: OpenTofu 1.13.9, a path or a name on `PATH`.
- `plugin_cache`: the providers' directory.
- `scratch_dir`: where each run gets a directory.
- `git`: optional. The `git` that each run's [filesystem view](serve.md#what-a-run-can-see) has, a path or a name on `PATH`. It's found when `writ serve` starts, which fails if it's missing. Each run's `PATH` has that `git`'s directory first. Without `git`, a module source that needs `git`, such as `git::https://...` or a `github.com/` address, fails, and the plan's error says to set `tofu.git`.

Both directories must be outside `data`. The block requires `provider_uids`, because a stack runs the code of any commit that `writ serve` plans. So OpenTofu and its providers run as a provider uid, never as `writ serve`'s own user, which can read the state keys, the database, and the tokens. Without the block, a plan of such a stack is refused. `writ serve --check` checks that the binary is OpenTofu 1.13.9, and that `git`, when set, has `git-remote-http` and `git-remote-https` in its exec path.

## Stack names and prefixes

A repository's `prefix` goes before each stack's directory to make its name on the control plane: with `prefix = "payments/"`, the stack `network` is `payments/network`. Grants, approvals, and state use that name. A request can't choose the name.

For every plan and apply, the control plane takes off the stack's name the prefix of the longest-prefixed repository it has that the name starts with. A prefix ends in a slash, so it matches at a slash. Registering a repository is refused when its prefix starts another repository's prefix, or is the start of one, or when one of its stack names starts with another repository's prefix. So one prefix is always the longest. See [Rules for adding a repository](add-repository.md#rules-for-adding-a-repository).

## State

State has no field. An OpenTofu stack's state is on the http backend that `writ serve` serves, in the control database, or in the stack's own S3 bucket, as [OpenTofu stacks](serve.md#opentofu-stacks) says. A request that sets a state location is refused.

## What writ serve --check refuses

`writ serve --check` refuses, with its position:

- An unknown setting or block, a block written as a setting and the reverse, and a block named twice.
- A reference to an issuer, team, operation, risk class, or environment that doesn't exist.
- An issuer without `identity`.
- An element of `trusted_proxies` that isn't a CIDR or that sets bits past its prefix length.
- A `drift` block without `every`, with an empty `every`, an `every` of zero or under a minute, in a config with no `tofu` block, or with `limits.plans` under 2.
- A `tofu` block without `provider_uids`.
- An `egress_mode` other than `off`, `report`, and `enforce`, and `report` or `enforce` in a config with no `tofu` block, or with an env whose `s3_endpoints` lists an `http://` endpoint on loopback, which a run's own namespace can't reach.
- An `egress` item that isn't `HOST`, `*.DOMAIN`, or an IP address with an optional port, a wildcard of one label, such as `*.com`, and an IP address the proxy never connects to: loopback, link-local, multicast, unspecified, `fd00:ec2::254`, or a NAT64 address of one of them.
- A `provider_registries` item that isn't a registry's host name, such as one with `https://` or a port.
- A stack pattern that isn't one, as [Stack patterns](serve.md#stack-patterns) says.
- An `approve` grant to a team of a CI issuer that matches no claim of the person who ran the job, and an `override` or `adopt` grant to any team of a CI issuer.

It warns on:

- A config with an `http://` URL and no `trusted_proxies`.
- A team that matches a path without its id, and a team that can apply but matches no ref.
- A config without `provider_uids`.
- Issuers of two identities whose URLs look like one host.
- An env that sets `egress` under `egress_mode` `off`, where the list has no effect.
- `provider_registries` under `egress_mode` `off`, where it has no effect.

It prints the approval rules, and under `egress_mode` `report` or `enforce`, the mode, each env's `egress` list, and `provider_registries`.

A config that sets one of these removed settings is refused, and the setting is named: `provider` and `providers_dir`, because a stack runs the providers its `.terraform.lock.hcl` pins; `limits.files`; and an environment's `secret_env` or `state`. Delete them.

## Policy

The `policy` block lists policy sets. Each is a directory of `.rego` files on the host, relative to the config file. The config is the only place policy comes from, so no commit can change the policy that checks it. [Policy](policy.md) describes the input, the rules Writ reads, and the verdicts.

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
| `timeout` | How long one decision can take, at either point, every set together | `"10s"` |
| `memory` | How much address space the evaluation process can add, at least `"64MiB"` | `"1GiB"` |
| `set "NAME"` | One policy set. At least one. Its name is lower case letters, digits, `-`, and `_` | |
| `set.dir` | The directory of the set's `.rego` files, relative to the config file | |
| `set.level` | `"mandatory"`, whose deny refuses, or `"advisory"`, whose deny only warns | `"mandatory"` |

A set has every `.rego` file under its directory, at any depth, except files with names that end in `_test.rego`. Those contain the set's tests for `conftest verify` and `opa test`. A set has at most 1 MiB of Rego.

`writ serve` reads and compiles every set at start, and doesn't start when one doesn't compile. `writ serve --check` compiles them and prints a line for each set, and one for the bounds. When a set doesn't compile, `--check` fails with the file and place:

```
policy set baseline: iam.rego:6:12: rego_type_error: undefined ref: input.planned_values.root_module.resources[__local0__]
```

`writ serve --dev --config FILE` takes the same block.
