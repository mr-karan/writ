# Security model

Writ is designed around one rule: **cloud credentials and state encryption keys stay on the control plane server, never in CI pipelines or on developer laptops.**

Because an OpenTofu plan on an unmerged branch executes provider and module code, Writ enforces thirteen independent layers of defense across identity, secret handling, sandboxing, policy, and state integrity.

## Thirteen layers Writ enforces

1. **No cloud credentials in CI.** CI jobs authenticate to Writ with single-use platform OIDC tokens (`WRIT_ID_TOKEN`); AWS role credentials and state encryption keys never leave `writ serve`.
2. **Short-lived, narrowly scoped STS sessions per run.** Each run receives a dedicated STS session for the environment's `plan_role` or `apply_role` over loopback, tagged with the caller's `SourceIdentity` for CloudTrail attribution.
3. **Dedicated provider UIDs and metadata firewall.** Every concurrent OpenTofu run executes under its own Linux UID from `provider_uids`, blocked by `nftables` from the instance metadata endpoints (`169.254.169.254` and `fd00:ec2::254`).
4. **Linux user, mount, and PID namespaces.** Runs execute inside a `pivot_root` read-only `tmpfs` filesystem view with all capabilities cleared (`no_new_privs`) and server keys, database credentials, and shells unmounted.
5. **Secrets masked from every output and stored record.** `internal/mask` strips sensitive values, secret-named attributes, and the STS credentials served to a run before any plan, diagnostic, log, or state index row is stored or shown.
6. **Sealed, immutable saved plans.** Saved plans and `http`-backend states are encrypted per environment with AES-256-GCM; `writ apply --remote` applies the exact approved plan binary and refuses if state moved or the plan expired (`plan_ttl`, default `24h`).
7. **Server-enforced grants, approvals, and OPA/Rego policy.** Teams, grants, approval rules, and mandatory Rego policy sets live in `writ serve`'s host config, where no merge request or CI YAML change can weaken them.
8. **Authors cannot approve their own plans.** Self-approval is refused whenever an approval rule applies, matched by a person's stable `IDENTITY/user/ID` across CI OIDC jobs, CLI tokens, and browser sessions.
9. **Synchronous audit log of every request.** Every API, UI, and state request commits an audit row with the caller's verified identity before `writ serve` sends a response, readable via `GET /v1/audit` and `writ audit --remote`.
10. **Scheduled drift checks and out-of-band state detection.** `writ serve` runs scheduled drift checks and detects S3 state versions written outside Writ, blocking approvals until an administrator reviews and runs `writ state adopt`.
11. **Per-address and per-principal rate limits and execution timeouts.** Token-bucket rate limits per client IP and per authenticated principal, bounded concurrent runs (`limits.plans` and `limits.applies`), and hard plan (`30m`) and apply (`4h`) deadlines protect the server.
12. **Per-run network egress control.** With `egress_mode = "enforce"`, each run gets a network namespace with only loopback and reaches the network only through Writ's egress proxy, which allows the hosts the environment's `egress` list names, never connects to metadata, loopback, or link-local addresses, and refuses private addresses a wildcard would reach. Provider installs are confined the same way, to the registries `provider_registries` lists. See [Run isolation and egress](run-isolation.md#egress).
13. **Per-run resource limits and a seccomp filter.** Each OpenTofu step runs in a cgroup of its own, with `limits.run_memory` (no swap), `run_pids`, and `run_cpus`, and is killed whole with `cgroup.kill` when it ends, so one stack's providers can't starve `writ serve` or other runs; `run_scratch` bounds the run's directory on disk. Every `tofu` process runs under a seccomp filter that refuses the mount family, new namespaces, `ptrace`, the keyrings, `bpf`, `perf_event_open`, `userfaultfd`, `io_uring_setup`, and loading kernel code. The cgroup limits need `writ serve` alone in a delegated cgroup; `writ serve --check` says when they're off, and why. See [Limit what each run uses](run-isolation.md#limit-what-each-run-uses).

## Who holds what

| Party | Cloud credentials | State key & saved plans | Proof of identity |
| --- | --- | --- | --- |
| Developer laptop | None (except local `writ import bulk`) | None | `writ login` session token (`cli` carrier, 12h default, no refresh) |
| Browser (Control UI) | None | None | `HttpOnly` `SameSite=Lax` `__Host-` session cookie (`browser` carrier) |
| CI job | None | None | Platform-issued OIDC JWT (`WRIT_ID_TOKEN`), single-use per write |
| `writ serve` (plan) | Environment `plan_role` via STS | Reads state (sealed at rest) | Server's AWS identity + OIDC/session verification |
| `writ serve` (apply) | Environment `apply_role` via STS | Locks & writes state, opens sealed saved plan | Approved plan ID + apply grant + policy pass |

## Identity and token verification

- **Strict OIDC verification**: `internal/oidc` accepts only compact JWS tokens signed with `RS256` or `ES256`, requires `kid`, rejects embedded key headers (`jku`, `jwk`, `x5c`, etc.) and duplicate JSON keys, requires exact `iss` and single-element `aud` matching `writ serve`'s `url`, checks `exp`/`iat`/`nbf`, and records `jti` on writes to prevent replay.
- **No identity provider text leaks**: `internal/oidc` never returns raw error strings from an identity provider or token. Failures are typed integer refusals (`oidc.Refusal`), enforced by `internal/archtest`, so a misbehaving proxy or IdP cannot reflect tokens or secrets into logs or API responses.
- **Stable cross-issuer person IDs**: Issuers for the same Git host share a config-defined `identity` name (not a URL, preventing URL-spelling tricks). A person's CI job and CLI/UI session share one `IDENTITY/user/ID`, preventing self-approval across channels.
- **Verified webhooks**: `POST /v1/hooks/{forge}/{repo}` takes no token. A delivery proves itself with the pinned repository's webhook secret, compared in constant time, or a GitLab signing token's signature with a 5-minute timestamp window. A delivery that fails gets 401 with the same words whatever the reason. One that passes only makes the poll read the default branch's head now, through the forge's read token: its body is never parsed, so no commit, branch, or URL in it is trusted, and it never plans or applies. See [Webhooks](webhooks.md#security-notes).
- **Carrier separation**: Browser cookies are ignored on `/v1/` API routes, and CLI bearer tokens cannot open `/ui/` pages. All `/ui/` responses send a strict Content Security Policy (`default-src 'none'; script-src 'self'; ...`) and CSRF protection via `http.CrossOriginProtection`.

## Authorization and approval integrity

- **Config-owned rules**: Teams, grants, approval rules, and Rego policies live only in `writ serve`'s config on the server. A merge request cannot edit CI YAML or repo files to weaken grants, skip approvals, or disable policies.
- **No self-approval**: Whenever an approval rule applies, the plan's author can never approve it. `writ serve --check` refuses `approve` grants to CI teams that do not match a person claim, and refuses `override` or `adopt` grants to any CI team.
- **Plan binding**: Approving a plan approves its SHA-256 plan ID (binding the masked plan, binary saved plan, commit SHA, stack, and environment). Applying a stale plan, a superseded plan, or a commit whose sources differ from the approved plan is refused. The masked plan is rendered from the saved plan that Writ seals: Writ reads the saved plan when `tofu plan` exits, and `tofu show` and `tofu apply` read it from a sealed in-memory file that no process of the run can write.

## Run isolation

Because OpenTofu plans execute code from unmerged branches, a team's `writ serve` isolates every run on Linux:

- **Per-run UIDs**: Each concurrent run gets a dedicated UID from `provider_uids` with no supplementary groups and all capabilities cleared (`no_new_privs`). Concurrent runs cannot read each other's `/proc/PID/environ` or temporary directories.
- **Private filesystem view**: User, mount, and PID namespaces with `pivot_root` onto a read-only `tmpfs` hide `data`, state keys, TLS keys, `provider_env` files, shells, `ssh`, and `hg`.
- **Seccomp filter**: Every `tofu` process runs under a seccomp filter, so no process of a run can mount, make a namespace, trace a process, or reach the keyrings, BPF, perf events, `userfaultfd`, or `io_uring`. See [System calls a run can't make](run-isolation.md#system-calls-a-run-cant-make).
- **Resource limits**: Each step runs in a cgroup of its own with `limits.run_memory`, `run_pids`, and `run_cpus`, and `cgroup.kill` ends whatever the step left when it ends. `run_scratch` bounds the run's directory on disk.
- **Metadata firewall**: An `nftables` rule drops traffic from `provider_uids` to `169.254.169.254` and `fd00:ec2::254`, preventing providers from stealing the control plane host's AWS identity. `writ serve --check` actively tests both file permissions and metadata blocking as a provider UID.
- **Source hygiene**: Stack commits are verified against Git SHA-1 blob IDs. Symlinks, `.terraform` directories, `.tf.json`/`.tofu.json`, `file://` module sources, and dangerous `go-getter` query parameters are rejected before OpenTofu runs.
- **Per-run credentials**: AWS credentials are served over loopback behind a per-run random token rather than copied from the host environment.
- **Not dumpable**: With `provider_uids`, `writ serve` marks itself not dumpable at start, so no process of its own uid can trace it or read its memory, and it writes no core dump of its keys and secrets.
- **Egress proxy (opt-in)**: Under `egress_mode` `report` or `enforce`, each run gets a network namespace with only loopback, and reaches the network only through Writ's egress proxy, which checks every host and port against the env's `egress` list before it connects. It never connects to loopback, link-local, or metadata addresses, connects only to an address it resolved and checked, and keeps a wildcard out of private addresses. `enforce` refuses what the list lacks. The provider install runs in a namespace of its own behind a proxy that refuses every private address, and installs only from the registries `provider_registries` lists. Every plan and run records the destinations it asked for. The default, `off`, leaves runs and the provider install on the host's network.

See [Run isolation and egress](run-isolation.md) for setup and verification.

## Secret masking and encryption at rest

- **One masking boundary**: `internal/mask` produces every document leaving a run (masked plan, run text, events, diagnostics, state index rows). `internal/archtest` forbids any package outside `internal/tofu` from reading raw state JSON except through `mask.Index`.
- **Four masking rules**: OpenTofu sensitive marks, value sweep (including URL, JSON, hex, and base64 encodings), secret-like attribute names, and a strict output key allow-list. The value sweep also includes the STS access key, secret key, and session token served to the run.
- **Secret sets**: Each apply seals the secrets its mask knew alongside the state version or S3 serial so later plans and drift checks still mask old secrets (such as keys exposed via `nonsensitive()`). States without a secret set cannot be approved until explicitly [adopted](adopt-state.md).
- **Per-environment sealing**: States on the `http` backend, saved plans, and secret sets are sealed with AES-256-GCM using HKDF-derived keys from each environment's 32-byte `key` file.

See [Secrets and the mask](secrets.md) for the differential verification suite (`just verify-mask`) and known mask limits.

## Policy isolation

- Rego policy sets run in a separate child process with no environment, closed inherited file descriptors, strict memory (`memory`, default 1 GiB) and wall-clock (`timeout`, default 10s) limits, and an explicit allow-list of pure Rego built-ins (no network, no filesystem, no clock/randomness, no `print`).
- Policies only receive the masked plan document plus `input.writ`, and policy messages are swept through the mask before storage or display.
- Every policy failure fails closed at a mandatory set's level.

## Mandatory audit logging

- Every API, control UI, and state backend request (except `GET /healthz`, `GET /v1/server`, and per-address rate-limited `429`s) commits an audit row before `writ serve` sends a response. If the database cannot commit the row within 5 seconds, the request fails with `503`.
- `GET /v1/audit` filters rows by the caller's grants, omits client IP addresses and state SHA-256 hashes, and withholds refusal reasons (`reason_withheld: true`) so secrets accidentally pasted into a request cannot be read back through the audit API.

## Rate limits and execution bounds

- **Per-address rate limit**: `limits.requests_per_address` (default 20 requests/second per client IP, or `/64` for IPv6) counted before the route, token, or body is read.
- **Per-principal rate limit**: `limits.requests_per_second` (default 10 requests/second) per authenticated person or CI job.
- **Webhook bounds**: 10 deliveries a second from one client address, counted first, 5 a second for one repository, a 1 MiB body, and the newest 4,096 delivery ids remembered to drop a resend.
- **Sign-in queue bounds**: At most 10 pending `writ login` sign-ins per client IP and 1,000 across the server, expiring after 10 minutes.
- **Run concurrency and deadlines**: Bounded by `limits.plans_per_principal` (default 4), `limits.plans` (default 8), `limits.applies` (default 4), `limits.plan_timeout` (default `30m`), `limits.apply_start_timeout` (default `10m`), and `limits.apply_timeout` (default `4h`).
- **Per-step resources**: Each OpenTofu step is bounded by `limits.run_memory` (default `4GiB`), `limits.run_pids` (default 1024), `limits.run_cpus` (default 2), and `limits.run_scratch` (default `8GiB`), and each stdout and stderr by 64 MiB.

## Known limits and assumptions

Review these boundaries when deploying Writ:

- **Host root and server operator are trusted**: Anyone with root on the `writ serve` host or read access to `env.*.key` and Postgres can decrypt states and assume configured AWS roles.
- **Plan role permissions are your responsibility**: Writ assumes whatever `plan_role` you configure. Make sure its IAM policy is strictly read-only.
- **S3 state isolation**: On `backend "s3"`, IAM role permissions (not Writ grants) prevent stacks in the same environment from referencing each other's S3 state keys.
- **Network egress is off by default**: Under the default `egress_mode = "off"`, runs share the host's network, apart from the metadata firewall rule, so code a commit chose can reach anything the host reaches. Under `enforce`, an allowed host can still carry data out: `*.amazonaws.com` allows any S3 bucket, and the proxy decides by host and port, not by URL or content. `report` records but doesn't restrict. See [What egress doesn't cover](run-isolation.md#what-egress-doesnt-cover).
- **Run limits need a delegated cgroup, and scratch is measured**: `run_memory`, `run_pids`, and `run_cpus` hold only when `writ serve` runs alone in a cgroup delegated to its user, as systemd's `Delegate=yes` gives it; otherwise it warns and runs steps without them. `run_scratch` is a measure `writ serve` takes every half second, not a kernel limit, so a step can pass it briefly, and disk bandwidth isn't limited. See [What run limits don't cover](run-isolation.md#what-run-limits-dont-cover).
- **Mask limits**: The mask is an output filter, not information-flow tracking. Unmarked secrets in unmarked attributes, hashed secrets passed to `nonsensitive()`, and multi-line base64 without sensitive marks are not caught. See [What the mask can't do](secrets.md#what-the-mask-cant-do).

See [Supported platforms and limits](status.md) for current platform requirements and scope.

## Next steps

- [Run isolation and egress](run-isolation.md): configure `provider_uids`, the `nftables` firewall rule, and the egress proxy.
- [Secrets and the mask](secrets.md): test the mask with `just verify-mask`.
- [API: What the API refuses](api.md#what-the-api-refuses): threat-by-threat breakdown of API defenses.
- [SECURITY.md](../SECURITY.md): how to report a security issue privately.
