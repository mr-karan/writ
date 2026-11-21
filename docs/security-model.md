# Security model

Writ is built around one rule: **cloud credentials and state encryption keys stay on `writ serve`, never in CI pipelines or on developer laptops.**

Because planning an unmerged branch executes provider and module code, Writ enforces thirteen layers of defense across identity, secret masking, sandboxing, policy, and state integrity.

## Thirteen layers Writ enforces

1. **No cloud credentials in CI.** CI jobs authenticate with single-use OIDC tokens (`WRIT_ID_TOKEN`); AWS credentials and state keys never leave `writ serve`.
2. **Short-lived STS sessions per run.** Each run gets a dedicated loopback STS session for `plan_role` or `apply_role`, tagged with `SourceIdentity` for CloudTrail attribution.
3. **Dedicated provider UIDs and metadata firewall.** Concurrent OpenTofu runs execute under distinct UIDs from `provider_uids`, blocked by `nftables` from `169.254.169.254` and `fd00:ec2::254`.
4. **Linux user, mount, PID, and IPC namespaces.** Every step runs in a `pivot_root` read-only `tmpfs` view with `no_new_privs`, empty capabilities, and no server keys, database files, or shells mounted.
5. **Single secret-masking boundary.** `internal/mask` scrubs marked values, learned secrets (plain and encoded), secret-named fields, and per-run STS credentials from every plan, diagnostic, log, and state index row.
6. **Sealed, immutable saved plans.** Saved plans and HTTP-backend states are sealed per environment with AES-256-GCM; apply runs only the exact approved binary and refuses stale or expired (`plan_ttl`) plans.
7. **Server-owned grants, approvals, and Rego policy.** Teams, grants, approval rules, and mandatory Rego policies live only in server config, out of reach of merge requests and CI YAML edits.
8. **No self-approval.** Wherever an approval rule applies, the plan's author (by stable `IDENTITY/user/ID` across CI, CLI, and browser) can never approve their own plan.
9. **Synchronous audit logging.** Every API, UI, and state request (except `/healthz`, `/v1/server`, and per-IP `429`s) commits an audit row before returning a response.
10. **Scheduled drift checks and outside-write detection.** Drift checks detect cloud changes and out-of-band S3 state writes, blocking approvals until an admin runs `writ state adopt`.
11. **Rate limits and execution deadlines.** Per-IP and per-principal rate limits, bounded concurrent plans/applies, and hard plan (`30m`) and apply (`4h`) timeouts protect the control plane.
12. **Opt-in per-run network egress proxy.** Under `egress_mode = "enforce"`, each run executes in a loopback-only network namespace whose sole exit is Writ's egress proxy, restricted to `env.egress` and refusing metadata, loopback, link-local, and wildcard-matched private IPs ([Run isolation and egress](run-isolation.md#egress)).
13. **Per-step cgroup limits and seccomp filter.** Each OpenTofu step is bounded by `run_memory` (no swap), `run_pids`, `run_cpus`, and `run_scratch`, cleaned up via `cgroup.kill`, and confined by a seccomp filter blocking mount, namespace, `ptrace`, keyring, `bpf`, `userfaultfd`, `io_uring`, and kernel module syscalls ([Limit what each run uses](run-isolation.md#limit-what-each-run-uses)).

## Who holds what

| Party | Cloud credentials | State key and saved plans | Proof of identity |
| --- | --- | --- | --- |
| Developer laptop | None (except local `writ import bulk`) | None | `writ login` bearer token (`cli` carrier, 12h default) |
| Browser (Control UI) | None | None | `HttpOnly` `SameSite=Lax` `__Host-` cookie (`browser` carrier) |
| CI job | None | None | Single-use OIDC JWT (`WRIT_ID_TOKEN`) |
| `writ serve` (plan) | Environment `plan_role` via STS | Reads sealed state | Host AWS identity + verified OIDC/session |
| `writ serve` (apply) | Environment `apply_role` via STS | Locks/writes sealed state, opens sealed plan | Approved plan ID + apply grant + policy pass |

## Identity and token verification

- **Strict OIDC checks:** `internal/oidc` accepts only compact JWS tokens with `RS256` or `ES256`, requires `kid`, rejects header keys (`jku`/`jwk`/`x5u`/`x5c`) and duplicate JSON keys, enforces exact `iss` and single-element `aud`, requires `exp` and `iat`, and tracks `jti` on writes.
- **No IdP text leaks:** Exported `internal/oidc` functions return typed integer `oidc.Refusal` values (enforced by `internal/archtest`), never raw IdP text.
- **Cross-issuer person IDs:** Issuers for one Git host share a configured `identity`, mapping CI jobs and CLI/UI sessions to one `IDENTITY/user/ID`.
- **Verified webhooks:** `POST /v1/hooks/{forge}/{repo}` verifies the pinned repo's secret in constant time (or `whsec_` HMAC-SHA256 with a 5-minute replay window), ignores the payload body completely, and returns uniform `401` on all verification failures ([Webhooks](webhooks.md#security-notes)).
- **Carrier isolation:** Browser cookies aren't accepted on `/v1/`; CLI bearer tokens can't open `/ui/`. `/ui/` enforces strict CSP (`default-src 'none'; script-src 'self'; ...`) and `http.CrossOriginProtection`.

## Authorization and approval integrity

- **Server-side config:** Teams, grants, approval rules, and Rego policies live only on the server; CI YAML and repository commits can't weaken them.
- **No self-approval:** Authors can't approve their own plans under any approval rule. `writ serve --check` rejects CI `approve` grants without user-claim matching and rejects `override` / `adopt` grants for CI teams.
- **Immutable plan binding:** Approvals sign off on the plan ID (hashing masked plan, saved plan, commit, stack, and env). `tofu show` and `tofu apply` read the saved plan from a sealed in-memory file unwritable by run processes.

## Run isolation

On Linux, a team's `writ serve` isolates every run with:

- Dedicated `provider_uids` (no supplementary groups, `no_new_privs`, non-dumpable server process).
- User, mount, PID, and IPC namespaces with read-only `pivot_root` `tmpfs` (no `data`, keys, shells, `ssh`, or `hg`).
- Seccomp filter and per-step cgroup v2 limits (`run_memory`, `run_pids`, `run_cpus`, `cgroup.kill`) plus `run_scratch` monitoring.
- `nftables` drop rule for `169.254.169.254` and `fd00:ec2::254` (tested by `writ serve --check`).
- Git blob SHA-1 verification, symlink/JSON/`.terraform` rejection, and strict module-source allow-listing.
- Per-run loopback STS credentials and optional `egress_mode = "enforce"` proxy.

See [Run isolation and egress](run-isolation.md) for setup and verification.

## Secret masking and encryption at rest

- **Single masking boundary:** `internal/mask` writes every run output; `internal/archtest` prevents other packages from reading raw state JSON except via `mask.Index`.
- **Four rules:** OpenTofu sensitive marks, value sweep (plain + URL/JSON/hex/base64 + run STS credentials), secret-like attribute names, and strict key allow-list.
- **Secret sets:** Every apply seals its mask's known secrets alongside the state version or S3 serial; externally written states require explicit [adoption](adopt-state.md) before approval.
- **Per-environment sealing:** HTTP states, saved plans, and secret sets use AES-256-GCM with HKDF-derived keys from each environment's 32-byte `key` file.

See [Secrets and the mask](secrets.md) for `just verify-mask` and known gaps.

## Notifications

- [Notifications](notifications.md) contain only Writ's fixed wording, stack/env names, counts, and error classes, never secrets, plan diffs, diagnostic text, or policy messages.
- `url_file` and `secret_file` are checked unreadable by `provider_uids` and never appear in APIs, logs, or UI. Delivery targets `https://` only, follows no redirects, and uses bounded queues.

## Policy isolation

- Rego evaluates in a separate child process with empty environment, closed FDs, strict `memory` (`1GiB`) and `timeout` (`10s`) bounds, and a pure built-in allow-list (no network, filesystem, clock, randomness, or `print`).
- Policies read only the masked plan document + `input.writ`, and all output messages pass through the mask. Errors fail closed at mandatory level.

## Mandatory audit logging

- Every API, UI, and state request (except `/healthz`, `/v1/server`, and per-IP `429`s) commits an audit row before responding (5-second timeout, else `503`).
- `GET /v1/audit` filters by caller grants, hides client IPs and state SHA-256 hashes, and withholds refusal reasons (`reason_withheld: true`).

## Rate limits and execution bounds

- Per-IP `limits.requests_per_address` (`20`/s, IPv6 `/64`, before auth) and per-principal `limits.requests_per_second` (`10`/s).
- Webhooks: `10`/s per IP, `5`/s per repo, `1 MiB` max body, 4,096 deduplicated delivery IDs.
- Pending `writ login` handles: `10` per IP, `1,000` global, 10-minute expiry.
- Concurrency and timeouts: `plans_per_principal` (`4`), `plans` (`8`), `applies` (`4`), `plan_timeout` (`30m`), `apply_start_timeout` (`10m`), `apply_timeout` (`4h`).
- Per-step bounds: `run_memory` (`4GiB`), `run_pids` (`1024`), `run_cpus` (`2`), `run_scratch` (`8GiB`), stdout/stderr `64 MiB`.

## Known limits and assumptions

- **Host root and server operator are trusted:** Root access or access to `env.*.key` + Postgres exposes sealed states and configured IAM roles.
- **IAM `plan_role` scope is your responsibility:** Keep `plan_role` strictly read-only in AWS IAM.
- **S3 state isolation:** IAM policies (not Writ grants) isolate S3 keys across stacks within one environment.
- **Egress defaults to `off`:** Under `off`, runs share host networking (aside from metadata blocking). Under `enforce`, allow-listed hosts like `*.amazonaws.com` aren't inspected past host and port ([What egress doesn't cover](run-isolation.md#what-egress-doesnt-cover)).
- **Cgroup and scratch bounds:** Cgroup limits require a delegated cgroup; `run_scratch` is polled rather than kernel-enforced (use filesystem quotas for hard bounds; disk I/O bandwidth isn't capped).
- **Mask limits:** Unmarked secrets in unmarked attributes, hashed values under `nonsensitive()`, and line-wrapped unmarked base64 aren't masked ([What the mask can't do](secrets.md#what-the-mask-cant-do)).

See [Supported platforms and limits](status.md) for platform requirements.

## Next steps

- [Run isolation and egress](run-isolation.md): configure `provider_uids`, `nftables`, cgroups, and egress.
- [Secrets and the mask](secrets.md): run `just verify-mask`.
- [API: What the API refuses](api.md#what-the-api-refuses): threat-by-threat API defense table.
- [`SECURITY.md`](../SECURITY.md): private security reporting.
