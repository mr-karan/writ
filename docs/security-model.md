# Security model

Writ's one rule: **cloud credentials and state encryption keys stay on `writ serve`, never in CI pipelines or on developer laptops.**

Planning an unmerged branch runs provider and module code, so Writ layers defenses across identity, masking, sandboxing, policy, and state integrity.

## Thirteen layers Writ enforces

| # | Layer | Details |
| --- | --- | --- |
| 1 | No cloud credentials in CI | [Identity](#identity-and-token-verification) |
| 2 | A short-lived STS session per run | [Run isolation](#run-isolation) |
| 3 | Dedicated provider UIDs and a metadata firewall | [Run isolation](#run-isolation) |
| 4 | Linux user, mount, PID, and IPC namespaces | [Run isolation](#run-isolation) |
| 5 | One secret-masking boundary | [Masking](#secret-masking-and-encryption-at-rest) |
| 6 | Sealed, immutable saved plans | [Approval integrity](#authorization-and-approval-integrity) |
| 7 | Grants, approvals, and Rego policy owned by the server | [Approval integrity](#authorization-and-approval-integrity) |
| 8 | No self-approval | [Approval integrity](#authorization-and-approval-integrity) |
| 9 | Synchronous audit logging | [Audit](#mandatory-audit-logging) |
| 10 | Scheduled drift checks and outside-write detection | [Masking](#secret-masking-and-encryption-at-rest) |
| 11 | Rate limits and execution deadlines | [Limits](#rate-limits-and-execution-bounds) |
| 12 | Opt-in per-run egress proxy | [Run isolation](#run-isolation) |
| 13 | Per-step cgroup limits and a seccomp filter | [Run isolation](#run-isolation) |

## Who holds what

| Party | Cloud credentials | State key and saved plans | Proof of identity |
| --- | --- | --- | --- |
| Developer laptop | None (except local `writ import bulk`) | None | `writ login` bearer token (`cli` carrier, 12h default) |
| Browser (Control UI) | None | None | `HttpOnly` `SameSite=Lax` `__Host-` cookie (`browser` carrier) |
| CI job | None | None | Single-use OIDC JWT (`WRIT_ID_TOKEN`) |
| `writ serve` (plan) | Env `plan_role` via STS | Reads sealed state | Host AWS identity + verified OIDC/session |
| `writ serve` (apply) | Env `apply_role` via STS | Locks/writes sealed state, opens sealed plan | Approved plan ID + apply grant + policy pass |

## Identity and token verification

- **No cloud credentials in CI:** CI jobs authenticate with single-use OIDC tokens (`WRIT_ID_TOKEN`). AWS credentials and state keys never leave `writ serve`.
- **Strict OIDC checks:** only compact JWS tokens with `RS256` or `ES256`. `kid` required. Header keys (`jku`, `jwk`, `x5u`, `x5c`) and duplicate JSON keys rejected. Exact `iss`, a single-element `aud`, and `exp`, `iat`, and `nbf` checked. `jti` tracked on writes.
- **No IdP text leaks:** verification failures return fixed refusal codes, never the identity provider's text.
- **One person across issuers:** issuers for one Git host share a configured `identity`, so CI jobs and CLI and UI sessions map to one `IDENTITY/user/ID`.
- **Verified webhooks:** `POST /v1/hooks/{forge}/{repo}` checks the pinned repository's secret in constant time (or a `whsec_` HMAC-SHA256 signature with a 5-minute replay window), ignores the payload body, and returns the same `401` for every failure ([Webhooks](webhooks.md#security-notes)).
- **Carrier isolation:** browser cookies are not accepted on `/v1/`, and CLI bearer tokens cannot open `/ui/`. `/ui/` enforces a strict CSP (`default-src 'none'; script-src 'self'; ...`) and cross-origin request checks.

## Authorization and approval integrity

- **Server-side config:** teams, grants, approval rules, and mandatory Rego policies live only in server config. Merge requests, CI YAML, and repository commits cannot weaken them.
- **No self-approval:** wherever an approval rule applies, the plan's author, identified by a stable `IDENTITY/user/ID` across CI, CLI, and browser, cannot approve their own plan. `writ serve --check` rejects CI `approve` grants without a user-claim match, and any `override` or `adopt` grant for a CI team.
- **Immutable plan binding:** approvals sign off on the plan ID, a hash of the masked plan, saved plan, commit, stack, and env. Saved plans and HTTP-backend states are sealed per env with AES-256-GCM. An apply runs only the exact approved binary and refuses stale or expired (`plan_ttl`) plans. `tofu show` and `tofu apply` read the saved plan from a sealed in-memory file that run processes cannot write.

## Run isolation

On Linux, a team's `writ serve` isolates every run with:

- Dedicated `provider_uids`, so concurrent runs get distinct UIDs: no supplementary groups, `no_new_privs`, and a non-dumpable server process.
- User, mount, PID, and IPC namespaces on a read-only `pivot_root` `tmpfs` with empty capabilities, and no `data`, keys, database files, shells, `ssh`, or `hg`.
- A seccomp filter that blocks mount, namespace, `ptrace`, keyring, `bpf`, `userfaultfd`, `io_uring`, and kernel module system calls. It needs Linux 4.14+ on amd64 or arm64.
- Per-step cgroup v2 limits (`run_memory` with no swap, `run_pids`, `run_cpus`, cleanup by `cgroup.kill`), which need `writ serve` alone in a delegated cgroup, plus `run_scratch` monitoring. `writ serve --check` and the start log say when the filter or the limits are off, and why ([Limit what each run uses](run-isolation.md#limit-what-each-run-uses)).
- An `nftables` drop rule for `169.254.169.254` and `fd00:ec2::254`, tested by `writ serve --check`.
- Git blob SHA-1 verification, rejection of symlinks, JSON, and `.terraform`, and a strict module-source allow-list.
- A loopback STS session per run for `plan_role` or `apply_role`, tagged with `SourceIdentity` for CloudTrail.
- An optional egress proxy. Under `egress_mode = "enforce"`, each run's only exit is Writ's proxy, limited to `env.egress`, refusing metadata, loopback, link-local, and wildcard-matched private IPs ([Egress](run-isolation.md#egress)). The default, `off`, leaves runs and the provider install on the host's network.

See [Run isolation and egress](run-isolation.md) for setup and verification.

## Secret masking and encryption at rest

- **One masking boundary:** one mask writes every run output: plans, diagnostics, logs, and state index rows. Raw state JSON never leaves a run unmasked.
- **Four rules:** OpenTofu sensitive marks; a value sweep (plain, URL, JSON, hex, and base64 forms, plus the run's STS credentials); secret-like attribute names; a strict key allow-list.
- **Secret sets:** every apply seals its mask's known secrets with the state version or S3 serial. A state with no secret set (written outside Writ, before secret sets, or by an apply whose mask could not read its plan) cannot be approved until someone [adopts](adopt-state.md) it or a Writ apply writes a newer version.
- **Outside writes:** scheduled drift checks detect cloud changes and out-of-band S3 state writes, and block approvals until someone with the `adopt` grant runs `writ state adopt`.
- **Per-env sealing:** HTTP states, saved plans, and secret sets use AES-256-GCM with HKDF-derived keys from each env's 32-byte `key` file.

See [Secrets and the mask](secrets.md) for the rules and known gaps.

## Notifications

- [Notifications](notifications.md) carry only Writ's fixed wording, stack and env names, counts, and error classes. Never secrets, plan diffs, diagnostic text, or policy messages.
- `url_file` and `secret_file` are checked to be unreadable by `provider_uids`, and never appear in APIs, logs, or the UI. Delivery is `https://` only, follows no redirects, and uses bounded queues.

## Policy isolation

- Rego runs in a separate child process with an empty environment, closed file descriptors, `memory` (`1GiB`) and `timeout` (`10s`) bounds, and a pure built-in allow-list: no network, filesystem, clock, randomness, or `print`.
- Policies read only the masked plan document and `input.writ`. Every output message passes through the mask. Errors fail closed at the mandatory level.

## Mandatory audit logging

- Every API, UI, and state request, except `/healthz`, `/v1/server`, and per-IP `429`s, commits an audit row before it responds. The write has a 5-second timeout, after which the request gets `503`.
- `GET /v1/audit` filters by the caller's grants, hides client IPs and state SHA-256 hashes, and withholds refusal reasons (`reason_withheld: true`).

## Rate limits and execution bounds

| Scope | Limits |
| --- | --- |
| Requests | Per IP: `limits.requests_per_address` (`20`/s, IPv6 `/64`, before auth). Per principal: `limits.requests_per_second` (`10`/s). |
| Webhooks | `10`/s per IP, `5`/s per repository, `1 MiB` body, 4,096 deduplicated delivery IDs. |
| Pending `writ login` handles | `10` per IP, `1,000` per flow. Browser handles expire after 10 minutes, device handles at the issuer's expiry, at most 30 minutes. |
| Concurrency and timeouts | `plans_per_principal` (`4`), `plans` (`8`), `applies` (`4`), `plan_timeout` (`30m`), `apply_start_timeout` (`10m`), `apply_timeout` (`4h`). |
| Each step | `run_memory` (`4GiB`), `run_pids` (`1024`), `run_cpus` (`2`), `run_scratch` (`8GiB`), stdout and stderr `64 MiB`. |

## Known limits and assumptions

- **Host root and the server operator are trusted.** Root, or access to `env.*.key` and Postgres, can decrypt states and assume the configured AWS roles.
- **`plan_role` scope is your job.** Keep `plan_role` strictly read-only in AWS IAM.
- **S3 state isolation is IAM's job.** IAM policies, not Writ grants, isolate S3 keys across stacks within one env.
- **Egress defaults to `off`.** Runs and the provider install then share the host's network, apart from the metadata rule for provider UIDs, so code a commit chose can reach anything the host reaches. `report` records but does not restrict. Under `enforce`, an allowed host can still carry data out: `*.amazonaws.com` allows any S3 bucket, and the proxy decides by host and port, not URL or content ([What egress doesn't cover](run-isolation.md#what-egress-doesnt-cover)).
- **Cgroup and scratch bounds are partial.** `run_memory`, `run_pids`, and `run_cpus` hold only when `writ serve` runs alone in a cgroup delegated to its user; otherwise it warns and runs steps without them. `run_scratch` is polled, not enforced by the kernel: it bounds a runaway or careless run, not one that hides what it uses on disk, and a filesystem quota is the hard bound. Disk I/O bandwidth is not capped ([What run limits don't cover](run-isolation.md#what-run-limits-dont-cover)).
- **The mask has gaps.** Unmarked secrets in unmarked attributes, hashed values under `nonsensitive()`, and line-wrapped unmarked base64 are not masked ([What the mask can't do](secrets.md#what-the-mask-cant-do)).

Platform requirements are in [Supported platforms and limits](status.md).

## Next steps

- [Run isolation and egress](run-isolation.md): configure `provider_uids`, `nftables`, cgroups, and egress.
- [Secrets and the mask](secrets.md): masking rules and known gaps.
- [API: What the API refuses](api.md#what-the-api-refuses): threat-by-threat API defenses.
- [`SECURITY.md`](../SECURITY.md): private security reporting.
