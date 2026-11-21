# How the control plane works

`writ serve` is a single-process control plane with a REST API and control UI that plans and applies OpenTofu stacks. CI jobs authenticate with OIDC tokens, teams and grants in host config decide permissions, every request commits a synchronous audit row ([API](api.md#every-answer-waits-for-its-audit-row)), and cloud credentials and state keys never leave the server.

This page documents internal mechanics for OpenTofu execution, lock-file translation, S3 state tracking, drift checks, authorization, credentials, isolation, and egress. For setup guides, see [Run a control plane](run.md), [AWS roles and credentials](aws-credentials.md), [Run isolation and egress](run-isolation.md), and [Enable drift checks](drift-checks.md).

## How a remote plan runs

1. CI (or `writ plan --remote` in a clean work tree) sends a commit SHA and stack name, never source files.
2. `writ serve` verifies the OIDC/session token before reading the body, maps claims to teams, checks `plan` on `stack@env`, verifies the commit exists on the ref at the Git host, fetches uncached blobs, and checks every blob's Git SHA-1 ID.
3. `writ serve` assumes the environment's `plan_role` via AWS STS tagged with `SourceIdentity` ([Roles and credentials](#roles-and-credentials)).
4. OpenTofu runs `init -lockfile=readonly`, `plan -lock=false`, and `show -json` under a dedicated UID from `provider_uids` inside isolated Linux namespaces ([How providers are kept from the keys](#how-providers-are-kept-from-the-keys)).
5. `internal/mask` strips secrets, `policy` evaluates the masked document, Writ seals the saved plan under the environment key, and returns the SHA-256 plan ID.

## OpenTofu stacks

An OpenTofu stack is a directory containing `.terraform.lock.hcl`, `.tf`/`.tofu` files, and no `.writ` files. On each plan:

1. **Gather sources:** Collect the stack directory and local modules (`./`, `../`) at the commit and validate [What a stack can contain](#what-a-stack-can-contain).
2. **Validate backend:** Accepts only empty `backend "http" {}` (served by `writ serve` at `/v1/tfstate/` with per-run tokens) or `backend "s3"` ([Stacks on S3](#stacks-on-s3)). Missing backends, `cloud {}`, non-empty `http`, or other backends are refused.
3. **Fill plugin cache:** `writ serve` installs pinned providers into `tofu.plugin_cache` from a synthetic config as its own OS user (stack code never writes the cache).
4. **Run OpenTofu:** Runs `tofu init -lockfile=readonly` with no cloud credentials, then `tofu plan -lock=false` and `tofu show -json` with `provider_env` and `plan_role`. (S3 stacks run offline `init -backend=false`, then `init -get=false` with `plan_role` to configure the bucket.)
5. **Mask and seal:** Stores only the masked plan document, formatted text, and AES-256-GCM sealed saved plan; deletes the run's scratch directory.

### What a stack can contain

Writ rejects a stack before running OpenTofu if it violates any rule:

- Missing `.terraform.lock.hcl`, committed `.terraform/` directory, or any `.tf.json` / `.tofu.json` file.
- Non-literal `module` `source`, or a source outside: relative paths (`./`, `../`) staying inside the repo path; registry addresses (`[HOST/]NS/NAME/SYS[//SUBDIR]`); or remote network addresses (`git::`, `hg::`, `s3::`, `gcs::`, `http::`, `https::`, `github.com/`, `bitbucket.org/`).
- Any query parameter other than literal `ref` and `depth` (blocking `checksum`, `archive`, `filename`, `sshkey`, `rev`, S3 credential params, etc.), `#` fragments, `file:` (even percent-encoded), backslashes, absolute paths, `~`, Windows/UNC paths, or `oci://`.
- Size limits per stack: `1 MiB` for any `.tf`, `.tofu`, `.tfvars`, or lock file; `16 MiB` for any other file; `2,000` files or `64 MiB` total across the stack and its local modules.

### Lock files that Terraform wrote

Stacks migrated from Terraform pin providers at `registry.terraform.io`. For each run, `writ serve` translates a temporary copy of `.terraform.lock.hcl` to `registry.opentofu.org` at the same versions (leaving Git untouched and noting translated providers in plan warnings):

- **Non-`hashicorp/` providers:** Keep their committed hashes (OpenTofu serves the same author archives).
- **`hashicorp/` providers:** Drop Terraform's hashes (OpenTofu builds differ), verify OpenTofu registry signatures on install, and pin the installed hashes in memory for subsequent runs and applies (re-fetched once after server restart).

Explicit `source = "registry.terraform.io/hashicorp/..."` in HCL is refused (write `hashicorp/NAME` instead). State adoption and index refreshes similarly remap `registry.terraform.io` provider addresses on a temporary state copy for `tofu show -json`. See [Migrate from Terraform](migrate-from-terraform.md).

### Policy and the mask

Every plan is checked by configured [Rego policies](policy.md) at plan time and again before apply. Before masking any output from a plan, drift check, or apply, `internal/mask` initializes from `provider_env`, assumed STS credentials, and the **secret set** sealed with the state version/serial being read (carrying forward past secrets such as `nonsensitive(var.key)` resource keys).

On HTTP state, Writ verifies the state version didn't change between plan start and end (discarding output if an apply raced). On S3, Writ reads the serial from the saved plan (or loads all known secret sets for the stack on a failed plan).

### States with no secret set

States written outside Writ (or prior to secret sets) have no sealed secret set. Plans against them still record S3 serials, but fail with `state` (`no secret set for state version N of stack@env`) and cannot be approved; drift checks report `drift unknown: no secret set for state version N of stack@env`. Recover by inspecting the stack code and running [`writ state adopt`](adopt-state.md) (or applying a newer state from a plan against an already-sealed version).

### How an apply runs

Applies re-fetch and verify the commit's files, run `tofu init -lockfile=readonly` without cloud credentials, then run `tofu show -json` and `tofu apply` against the sealed saved plan (mounted via a sealed in-memory file at `/dev/fd/7`) under a provider UID with `provider_env` and `apply_role`. See [Approvals: How the apply runs](approvals.md#how-the-apply-runs).

### What a run can see

Every `tofu` process (`init`, `plan`, `show`, `apply`, cache fill, `--check` `tofu version`) executes on Linux inside private user, mount, PID, and IPC namespaces with `pivot_root` onto a read-only `tmpfs`, `no_new_privs`, empty capabilities, seccomp, and per-step cgroups. See [Run isolation and egress](run-isolation.md#what-each-run-can-see-on-disk) for the exact mount table and limits.

Additional execution details:

- `tofu.git` runs with `GIT_ALLOW_PROTOCOL=https:ssh`, `GIT_CONFIG_NOSYSTEM=1`, `GIT_CONFIG_GLOBAL=/dev/null`, no `ssh` binary, and `submodule` aliased to `submodule--helper` so HTTPS submodules work without `/bin/sh`.
- When a step fails before producing plan JSON, Writ keeps only masked diagnostic severity, summary, file, line, and column (never details or snippets). On S3 stacks, quoted instance keys (`["KEY"]` and `[\"KEY\"]`) in failure summaries are redacted to `["(sensitive)"]` to protect unmarked keys from un-adopted states.
- Under `writ serve --dev`, runs use the starting user (still inside namespaces on Linux; without namespaces on macOS, where `--data` must keep `TMPDIR` short enough for 103-byte Unix socket paths).

## The state index

Writ maintains a queryable resource index per `stack@env` (`GET /v1/index`, [Resources view](#the-resources-view)) storing only `address`, `type`, `provider`, and allow-listed identifying columns: `id`, `arn`, `identifier`, `region`, plus `namespace` and `modify_index` for `nomad_job`. Sensitive or masked values are stored empty. `internal/archtest` enforces that raw state JSON flows only into `mask.Index`.

Rows replace atomically per `stack@env` and never regress to an older state version. The index refreshes immediately after a successful apply (offline `tofu show -json`, non-fatal on error) and via a 5-minute background sweep (up to 50 lagging stacks per cycle). States without a sealed secret set aren't indexed until adopted or overwritten by a Writ apply.

### The Resources view

On `/ui/stack`, the Resources tab renders `GET /v1/index` grouped by service, module, or flat address, showing only allow-listed identifiers and the indexed version/serial, never raw state attributes.

## Stacks on S3

```hcl
terraform {
  backend "s3" {
    bucket       = "acme-tofu-states"
    key          = "network/terraform.tfstate"
    region       = "eu-west-1"
    use_lockfile = true
  }
}
```

Requirements for `backend "s3"` (see [AWS roles and credentials](aws-credentials.md#2-grant-s3-state-permissions-if-using-backend-s3) for IAM policies):

- Literal values only (no expressions/variables), with `use_lockfile = true` or non-empty `dynamodb_table`.
- Only allow-listed OpenTofu 1.13.9 attributes (`bucket`, `key`, `region`, `workspace_key_prefix`, `use_lockfile`, `dynamodb_table`, `encrypt`, `kms_key_id`, `use_path_style`, `force_path_style`, `use_dualstack_endpoint`, `use_fips_endpoint`, `skip_credentials_validation`, `skip_region_validation`, `skip_requesting_account_id`, `skip_s3_checksum`, `max_retries`, `retry_mode`, `endpoints`). Inline credentials, roles, profiles, ACLs, proxies, and TLS overrides are refused.
- Custom `endpoints = { s3 = "..." }` must match `env.s3_endpoints` exactly.
- Requires `plan_role` / `apply_role` (not supported under `writ serve --dev`). OpenTofu state/plan encryption isn't supported because Writ reads serials from the plan/state.

### How Writ reads the serial

Writ tracks up to 20 Writ-applied serials and 20 external serials per `stack@env` in `state_serials`:

- **Plans** read the serial from the saved plan.
- **Applies** run `tofu state pull` and `tofu show -json` after `tofu apply` finishes. If the apply succeeded (or failed after beginning resource changes) and the serial increased on the same lineage, Writ claims the serial, seals its secret set, and refreshes the index.
- **Background refresh** (startup + every 5 minutes, up to 50 stacks oldest-first) runs `tofu state pull` with `plan_role` (`RoleSessionName` `writ-index-...`).

**Limit:** Between `tofu apply` releasing the S3 lock and `tofu state pull` reading the serial, an external same-lineage write could be attributed to the apply.

### Writes outside Writ

Any S3 serial not written by a Writ apply is flagged as `outside_write` in `state_index_stacks`, `GET /v1/drift`, `GET /v1/index`, and `/ui/stack`, logged at `WARN` (`state written outside writ`), and sent to any [`notify`](notifications.md) block subscribed to `outside-write` (once per serial). Writ refuses to index or approve plans against that serial until someone runs [`writ state adopt`](adopt-state.md) or a Writ apply writes a newer version.

```
{"time":"2026-11-14T20:13:52.088355974+05:30","level":"WARN","msg":"state written outside writ","stack":"app","env":"prod","serial":2,"lineage":"6f8c0549-9d28-f161-88b8-188caefb612d","object":"s3://writ-states/app/terraform.tfstate"}
{"time":"2026-11-14T20:13:52.089272316+05:30","level":"ERROR","msg":"refresh the state index of a stack on S3; the next look tries again","stack":"app","env":"prod","err":"serial 2 of the state is not indexed: no apply of writ serve wrote it, so no mask knew its plan's secrets; the index stays at its older serial until an apply of writ serve writes a newer one"}
```

Output of `just e2e-tofu-s3`, 2026-11-14.

## Drift checks

For setup and troubleshooting, see [Enable drift checks](drift-checks.md).

When a `drift { every = "..." }` block is configured, Writ runs scheduled `tofu plan -lock=false` checks against each stateful `stack@env` using its repository's latest default-branch commit and `plan_role` (up to `limits.plans_per_principal` concurrently, always leaving at least 1 free plan slot for people/CI). Checks run at startup, every `every / 4` (capped at 30s intervals), after applies, after outside S3 writes, and after state adoptions.

Checks separate `outside` (`resource_drift`) from `code` (`resource_changes`), store only masked addresses, actions, and top-level changed attribute names (never values or saved plans, keeping the newest 20 checks per `stack@env`), skip policy evaluation, discard results if an apply ran concurrently, log `drift found` at `WARN`, and dispatch `drift` events to matching [`notify`](notifications.md) blocks.

Drifted HTTP stack example (`just e2e`, 2026-11-19):

```json
{
    "diagnostics": [],
    "every": "1h",
    "format": "writ.drift/v0",
    "more": false,
    "ok": true,
    "stacks": [
        {
            "backend": "http",
            "check": {
                "changes": [
                    {
                        "action": "create",
                        "address": "terraform_data.keyed[\"(sensitive)\"]",
                        "kind": "code"
                    },
                    {
                        "action": "delete",
                        "address": "terraform_data.keyed[\"(sensitive)\"]",
                        "kind": "code"
                    }
                ],
                "checked_at": "2026-11-19T18:45:30Z",
                "code": 2,
                "commit": "dda65cefe05a83f1433a335c880fdfb4a1d7a51d",
                "drifted_since": "2026-11-19T18:45:30Z",
                "error": "",
                "outside": 0,
                "plan": "a90814773ab14594f6566fa72b2d1aee",
                "repo": "infra",
                "started_at": "2026-11-19T18:45:30Z",
                "started_by": "drift",
                "status": "drifted"
            },
            "checking": false,
            "env": "prod",
            "indexed_at": "2026-11-19T18:45:30Z",
            "indexed_version": 3,
            "outside_write": null,
            "resources": 5,
            "stack": "app"
        }
    ]
}
```

Un-adopted outside S3 write example (`just e2e-tofu-s3`, 2026-11-15):

```json
{
    "diagnostics": [],
    "format": "writ.drift/v0",
    "more": false,
    "ok": true,
    "stacks": [
        {
            "backend": "s3",
            "check": {
                "changes": [],
                "checked_at": "2026-11-15T06:25:55Z",
                "code": 0,
                "commit": "c74e301354ad2a469009acb0b979b455c3966b8e",
                "drifted_since": "",
                "error": "drift unknown: no secret set for state version 3 of app@prod",
                "outside": 0,
                "plan": "",
                "repo": "infra",
                "started_at": "2026-11-15T06:25:54Z",
                "status": "failed"
            },
            "checking": false,
            "env": "prod",
            "indexed_at": "2026-11-15T06:24:57Z",
            "indexed_version": 2,
            "outside_write": {
                "seen_at": "2026-11-15T06:25:25Z",
                "serial": 3
            },
            "resources": 3,
            "stack": "app"
        }
    ]
}
```

## Teams and grants

Teams match OIDC claims from a single issuer (all `match` keys must match; list claims match any element; trailing `/*` matches one path segment and `/**` matches any depth; missing claims never match). Grants are additive over `teams`, `stacks`, `envs`, `can` (`read`, `plan`, `apply`, `approve`, `override`, `adopt`, `admin`), and `risks` (`create`, `update`, `replace`, `delete`, `data-loss`). Run `writ whoami` to inspect effective grants.

### Stack patterns

Stack patterns split on `/`. Within a segment, `*`, `?`, and `[...]` follow Go's `path.Match` without crossing `/`. A standalone `**` segment matches zero or more segments (`data/**` matches `data`, `data/pg`, and `data/pg/replica`; `**` matches every stack). At most one `**` segment is allowed per pattern (`apps**` and `a/**/b/**` are refused).

### Who a person is

Every issuer declares a lowercase `identity` name (never `dev`). Writ identifies people as `IDENTITY/user/ID` using `user_id` (GitLab CI), `actor_id` (GitHub Actions), or `sub` (`people` ID token). Issuers for the same Git host share one `identity` so a person's CI jobs and CLI/UI sessions collapse to one ID, preventing URL-spelling bypasses and cross-channel self-approval. Separate Git hosts must use separate identities (`writ serve --check` warns if different identities point to the same normalized host). Renaming an identity invalidates old sessions and stops old approvals from counting until re-approved or reverted.

## Roles and credentials

For setup instructions and trust policies, see [AWS roles and credentials](aws-credentials.md).

`writ serve` assumes `plan_role` or `apply_role` using its own AWS SDK default chain and serves credentials to each run over a loopback container credentials endpoint (`AWS_CONTAINER_CREDENTIALS_FULL_URI` + random `AWS_CONTAINER_AUTHORIZATION_TOKEN`, refreshing 15 minutes before expiry and revoking the token on run end). `provider_env` maps non-reserved env vars (`TF_VAR_*` allowed; `AWS_*`, non-`TF_VAR_` `TF_*`, `PLUGIN_*`, `WRIT_*`, `HOME`, `TMPDIR` refused) from host secret files.

Verified STS request and log (`just test-serve`, 2026-11-10):

```
signed by AKIACONTROLPLANE0001 for us-east-1/sts, Action=AssumeRole&DurationSeconds=3600&RoleArn=arn%3Aaws%3Aiam%3A%3A222222222222%3Arole%2Fwrit-plan&RoleSessionName=writ-ci-OH7MRPZTDKWIQBRXFHJDKV3GAD&SourceIdentity=gitlab-4242-1849213&Version=2011-06-15
```

```
{"time":"2026-11-10T19:21:56.87338551+05:30","level":"INFO","msg":"assumed role","component":"credentials","role":"arn:aws:iam::222222222222:role/writ-plan","session":"writ-ci-OH7MRPZTDKWIQBRXFHJDKV3GAD","source_identity":"gitlab-4242-1849213","why":"start","access_key_id":"ASIATEST000000000001","expires":"2026-11-10T14:51:56Z"}
```

## How providers are kept from the keys

For setup, firewall rules, seccomp, and cgroup limits, see [Run isolation and egress](run-isolation.md).

With `provider_uids` set, `writ serve` requires `CAP_SETUID`, `CAP_SETGID`, and `CAP_KILL` (clearing inheritable and ambient capabilities at startup), marks itself non-dumpable (temporarily toggling dumpability only while writing a child user namespace's UID map), assigns one exclusive UID per concurrent run, kills all processes of every range UID on startup and after each run, and enforces `no_new_privs` + empty capabilities on every child.

### Check the isolation

`writ serve --check` executes a probe as the first UID in `provider_uids` with an empty environment and fails if any protected path is readable or metadata endpoint is reachable:

```
$ writ serve --config /srv/writ/serve --check
config /srv/writ/serve: ok
url: https://writ.example.com
issuers: gitlab (gitlab-ci, https://gitlab.com), sso (people, https://gitlab.com, client 8f1c0b7e4d2a)
control UI: https://writ.example.com/ui/, sign-in redirects to https://writ.example.com/ui/callback, sessions last 12h0m0s
envs: prod, staging, state in postgres://writ@/writ
trusted proxies: 127.0.0.1/32; X-Forwarded-For names the client only when one of them sends it
providers run as: uids 61000-61063, one for each running plan
tofu: /nix/store/60vqi4v8n11afd5ks89j1kn7pgwfrww5-opentofu-1.13.9/bin/tofu, plugin cache /var/lib/writ-tofu/plugins, scratch directories in /var/lib/writ-tofu/runs
aws: sts in eu-west-1 at the regional endpoint, sessions of 1h
role: prod plan arn:aws:iam::222222222222:role/writ-plan
role: staging plan arn:aws:iam::111111111111:role/writ-plan
approvals: no rule, so no plan needs an approval
TEAM      ENV           STACKS  CAN           RISKS
infra_mr  staging,prod  *       read plan     -
platform  staging,prod  *       read approve  -
tofu: /nix/store/60vqi4v8n11afd5ks89j1kn7pgwfrww5-opentofu-1.13.9/bin/tofu is OpenTofu 1.13.9: ok
isolation: uid 61000 cannot read data, /var/lib/writ: ok
isolation: uid 61000 cannot read the state key of env prod, /var/lib/writ/keys/prod.key: ok
isolation: uid 61000 cannot read the state key of env staging, /var/lib/writ/keys/staging.key: ok
isolation: uid 61000 cannot read provider_env NOMAD_TOKEN of env prod, /var/lib/writ/secrets/nomad_token: ok
isolation: uid 61000 cannot reach 169.254.169.254:80: not checked: writ cannot reach it either, so this host has no metadata service there to protect
isolation: uid 61000 cannot reach [fd00:ec2::254]:80: not checked: writ cannot reach it either, so this host has no metadata service there to protect
```

Output of a real run, 2026-11-16. `just test-serve` verifies the same isolation against live loopback metadata servers:

```
the provider saw: key "open /tmp/TestProviderIsolation3679868081/001/keys/prod.key: permission denied", environ "open /proc/2405915/environ: permission denied", "Get \"http://169.254.169.254:80/latest/meta-data/iam/security-credentials/\": context deadline exceeded (Client.Timeout exceeded while awaiting headers)", "Get \"http://[fd00:ec2::254]:80/latest/meta-data/iam/security-credentials/\": context deadline exceeded (Client.Timeout exceeded while awaiting headers)"
```

## Egress

For rollout steps and operational limits, see [Run isolation and egress](run-isolation.md#egress).

Under `egress_mode` `report` or `enforce`, each step starts in a loopback-only network namespace with no external interface, route, or DNS. Before execing `tofu`, the view helper binds three loopback listeners and hands them to `writ serve` over a socket pair so `writ serve` serves them from the host namespace and identifies each run by listener:

| Run loopback port | Service | Environment variable |
| --- | --- | --- |
| `127.0.0.1:3128` | Egress proxy (`CONNECT` + HTTP) | `HTTP_PROXY`, `HTTPS_PROXY`, `http_proxy`, `https_proxy` |
| `127.0.0.1:3129` | Loopback STS credential endpoint | `AWS_CONTAINER_CREDENTIALS_FULL_URI` |
| `127.0.0.1:3130` | HTTP state backend (`/v1/tfstate/` only, per-run rate bucket, strips `X-Forwarded-For`) | `TF_HTTP_ADDRESS` |

`NO_PROXY` is set to `127.0.0.1,localhost,::1`. Each port caps at 256 connections (10s header timeout); the proxy caps at 128 connections per run (10m idle, 1h max) and closes all sockets when the step exits.

The proxy decides on `host:port` before dialing: always drops loopback, link-local (including `169.254.169.254`), multicast, broadcast, unspecified, `fd00:ec2::254`, and NAT64 equivalents (`64:ff9b::/96`, `64:ff9b:1::/48`); blocks unlisted names before DNS lookup under `enforce`; resolves listed names on the host and dials only the checked IP (defeating DNS rebinding); and allows private IPs (RFC 1918, ULA, CGNAT) only via exact host/IP match, never wildcards.

Every plan and run records masked `host:port` verdicts (`allowed`, `unlisted`, `refused`, `unresolved`; `(withheld)` if the mask couldn't read the plan) in `network` and logs a `run egress` summary:

```
writ: tofu init exited with code 1
error: writ's egress proxy refused 192.0.2.11:8080 (an IP address the list does not have); env prod's egress is [192.0.2.10:8080], under egress_mode enforce: add what the stack needs to the env's egress list
```

```
{"time":"2026-11-20T00:09:35.982321904+05:30","level":"WARN","msg":"run egress","run":"RXZIGH4YTSA4XR3RJPNCH5D2XE","env":"prod","mode":"enforce","destinations":"192.0.2.11:8080 refused x2","unrecorded":0}
```

Output of `TestTofuEgress`, 2026-11-20.

### What a run fetches

Providers come from `tofu.plugin_cache` (filled before the run), so `env.egress` only needs hosts reached by the run itself: cloud/provider APIs, S3 state endpoints, and remote module registries/Git hosts.

### The provider install

Under `egress_mode` `report` or `enforce`, provider cache fills verify every lock-file registry against `provider_registries` (default `registry.opentofu.org`, `registry.terraform.io`) and run in a dedicated network namespace behind a public-only install proxy (refusing private, loopback, link-local, and metadata IPs). Install destinations appear first in `network` with `step: "install"`. Private provider registries aren't supported under `report`/`enforce`.

## Next steps

- [Run a control plane](run.md) and [Server config](config.md)
- [API](api.md) and [JSON output](json-output.md)
- [Architecture](architecture.md)
