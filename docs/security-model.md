# Security model

Writ's one rule: **cloud credentials and state encryption keys stay on `writ serve`, never in CI pipelines or on developer laptops.** Planning an unmerged branch runs provider and module code, so Writ layers defenses across identity, masking, sandboxing, policy, and state integrity.

## Thirteen layers Writ enforces

| # | Layer | What it does | Detail |
| --- | --- | --- | --- |
| 1 | No cloud credentials in CI | CI jobs authenticate with single-use OIDC tokens (`WRIT_ID_TOKEN`), checked strictly (`RS256` or `ES256`, exact `iss`, single `aud`, `jti` replay). Identity provider errors map to fixed refusals, never the provider's text. AWS credentials and state keys never leave `writ serve`. | [Token checks](api.md#token-checks) |
| 2 | A short-lived STS session per run | Each run gets a loopback STS session for `plan_role` or `apply_role`, tagged with `SourceIdentity` for CloudTrail. | [AWS roles and credentials](aws-credentials.md) |
| 3 | Dedicated provider UIDs and a metadata firewall | Concurrent runs get distinct UIDs from `provider_uids`, with no supplementary groups, `no_new_privs`, and a non-dumpable server. `nftables` drops `169.254.169.254` and `fd00:ec2::254`, tested by `writ serve --check`. | [Run isolation and egress](run-isolation.md) |
| 4 | Linux user, mount, PID, and IPC namespaces | A read-only `pivot_root` `tmpfs` with empty capabilities and no `data`, keys, database files, shells, `ssh`, or `hg`. Commits are blob-verified, and symlinks, JSON, and `.terraform` are refused. | [How run isolation works](how-run-isolation-works.md) |
| 5 | One secret-masking boundary | One mask writes every run output (plans, diagnostics, logs, state index rows) using sensitive marks, a value sweep, secret-like names, and a key allow-list. Raw state JSON never leaves a run unmasked. | [Secrets and the mask](secrets.md) |
| 6 | Sealed, immutable saved plans | Approvals sign off on the plan ID. Saved plans and HTTP states are sealed per env with AES-256-GCM, and an apply runs only the exact approved binary, refusing stale or expired plans. | [Plan lifecycle reference](plan-lifecycle.md#what-the-plan-id-covers) |
| 7 | Grants, approvals, and Rego owned by the server | Teams, grants, approval rules, and mandatory Rego policies live only in server config. Merge requests, CI YAML, and commits cannot weaken them. Rego runs in a bounded child process with a pure built-in allow-list, reads only the masked plan, and fails closed. | [Approvals](approvals.md), [Policy reference](policy-reference.md) |
| 8 | No self-approval | Wherever an approval rule applies, the author, identified by a stable `IDENTITY/user/ID` across CI, CLI, and browser, cannot approve their own plan. `writ serve --check` rejects CI `approve` grants without a user-claim match, and any CI `override` or `adopt` grant. | [Approvals](approvals.md#who-can-approve-a-plan) |
| 9 | Synchronous audit logging | Every API, UI, and state request, except `/healthz`, `/v1/server`, and per-IP `429`s, commits an audit row before it responds (5-second timeout, else `503`). Audit reads hide client IPs and state hashes and withhold refusal reasons. | [Every answer waits for its audit row](api.md#every-answer-waits-for-its-audit-row) |
| 10 | Drift checks and outside-write detection | Scheduled drift checks find cloud changes and out-of-band S3 state writes, and block approvals until someone with `adopt` runs `writ state adopt`. | [Drift checks](drift-checks.md) |
| 11 | Rate limits and execution deadlines | Per-IP and per-principal rate limits, bounded concurrency, and plan and apply timeouts. | [Bounds](#rate-limits-and-execution-bounds) |
| 12 | Opt-in per-run egress proxy | Under `egress_mode = "enforce"`, each run's only exit is Writ's proxy, limited to `env.egress` and refusing metadata, loopback, link-local, and wildcard-matched private IPs. | [Egress](run-isolation.md#egress) |
| 13 | Per-step cgroup limits and a seccomp filter | `run_memory` (no swap), `run_pids`, `run_cpus`, `run_scratch`, and a seccomp filter against mount, namespace, `ptrace`, keyring, `bpf`, `userfaultfd`, `io_uring`, and kernel module calls. The limits need a delegated cgroup and the filter Linux 4.14+ on amd64 or arm64; `--check` says when either is off. | [Limit what each run uses](run-isolation.md#limit-what-each-run-uses) |

Other boundaries:

- **Carriers:** browser cookies are not accepted on `/v1/`, and CLI bearer tokens cannot open `/ui/`. `/ui/` sets a strict CSP and checks cross-origin requests ([How sign-in works](how-sign-in-works.md#what-every-page-sends)).
- **Webhooks:** a delivery is verified by the pinned repository's secret in constant time (or a `whsec_` signature with a 5-minute replay window), its body is ignored, and every failure gets the same `401` ([Webhook route](api.md#webhook-route)).
- **Encryption at rest:** HTTP states, saved plans, and secret sets use AES-256-GCM with HKDF-derived keys from each env's 32-byte `key` file. Every apply seals its mask's known secrets with the state; a state without a secret set cannot be approved until [adopted](adopt-state.md).
- **Notifications** carry only fixed wording, names, counts, and error classes, go to `https://` only with no redirects, and their `url_file` and `secret_file` are checked unreadable by `provider_uids` ([Get notified](notifications.md)).

## Who holds what

| Party | Cloud credentials | State key and saved plans | Proof of identity |
| --- | --- | --- | --- |
| Developer laptop | None (except local `writ import bulk`) | None | `writ login` bearer token (`cli` carrier, 12h default) |
| Browser (Control UI) | None | None | `HttpOnly` `SameSite=Lax` `__Host-` cookie (`browser` carrier) |
| CI job | None | None | Single-use OIDC JWT (`WRIT_ID_TOKEN`) |
| `writ serve` (plan) | Env `plan_role` via STS | Reads sealed state | Host AWS identity + verified OIDC/session |
| `writ serve` (apply) | Env `apply_role` via STS | Locks/writes sealed state, opens sealed plan | Approved plan ID + apply grant + policy pass |

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
- **Egress defaults to `off`.** Runs and the provider install then share the host's network, apart from the metadata rule for provider UIDs, so code a commit chose can reach anything the host reaches. `report` records but does not restrict. Under `enforce`, an allowed host can still carry data out: `*.amazonaws.com` allows any S3 bucket, and the proxy decides by host and port, not URL or content ([What egress doesn't cover](how-run-isolation-works.md#what-egress-doesnt-cover)).
- **Cgroup and scratch bounds are partial.** `run_memory`, `run_pids`, and `run_cpus` hold only when `writ serve` runs alone in a cgroup delegated to its user; otherwise it warns and runs steps without them. `run_scratch` is polled, not enforced by the kernel: it bounds a runaway or careless run, not one that hides what it uses on disk, and a filesystem quota is the hard bound. Disk I/O bandwidth is not capped ([What run limits don't cover](how-run-isolation-works.md#what-run-limits-dont-cover)).
- **The mask has gaps.** Unmarked secrets in unmarked attributes, hashed values under `nonsensitive()`, and line-wrapped unmarked base64 are not masked ([What the mask can't do](secrets.md#what-the-mask-cant-do)).

Platform requirements are in [Supported platforms and limits](status.md).

## Next steps

- [Run isolation and egress](run-isolation.md): configure `provider_uids`, `nftables`, cgroups, and egress.
- [Secrets and the mask](secrets.md): masking rules and known gaps.
- [API: What the API refuses](api.md#what-the-api-refuses): threat-by-threat API defenses.
- [`SECURITY.md`](../SECURITY.md): private security reporting.
