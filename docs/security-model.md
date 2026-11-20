# Security model

Writ is designed around one rule: **cloud credentials and state encryption keys stay on the control plane server, never in CI pipelines or on developer laptops.**

This page summarizes Writ's trust boundaries, defensive controls, and explicit limits so platform and security teams can review what Writ protects and what it does not.

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
- **Carrier separation**: Browser cookies are ignored on `/v1/` API routes, and CLI bearer tokens cannot open `/ui/` pages. All `/ui/` responses send a strict Content Security Policy (`default-src 'none'; script-src 'self'; ...`) and CSRF protection via `http.CrossOriginProtection`.

## Authorization and approval integrity

- **Config-owned rules**: Teams, grants, approval rules, and Rego policies live only in `writ serve`'s config on the server. A merge request cannot edit CI YAML or repo files to weaken grants, skip approvals, or disable policies.
- **No self-approval**: Whenever an approval rule applies, the plan's author can never approve it. `writ serve --check` refuses `approve` grants to CI teams that do not match a person claim, and refuses `override` or `adopt` grants to any CI team.
- **Plan binding**: Approving a plan approves its SHA-256 plan ID (binding the masked plan, binary saved plan, commit SHA, stack, and environment). Applying a stale plan, a superseded plan, or a commit whose sources differ from the approved plan is refused.

## Run isolation

Because OpenTofu plans execute code from unmerged branches, a team's `writ serve` isolates every run on Linux:

- **Per-run UIDs**: Each concurrent run gets a dedicated UID from `provider_uids` with no supplementary groups and all capabilities cleared (`no_new_privs`). Concurrent runs cannot read each other's `/proc/PID/environ` or temporary directories.
- **Private filesystem view**: User, mount, and PID namespaces with `pivot_root` onto a read-only `tmpfs` hide `data`, state keys, TLS keys, `provider_env` files, shells, `ssh`, and `hg`.
- **Metadata firewall**: An `nftables` rule drops traffic from `provider_uids` to `169.254.169.254` and `fd00:ec2::254`, preventing providers from stealing the control plane host's AWS identity. `writ serve --check` actively tests both file permissions and metadata blocking as a provider UID.
- **Source hygiene**: Stack commits are verified against Git SHA-1 blob IDs. Symlinks, `.terraform` directories, `.tf.json`/`.tofu.json`, `file://` module sources, and dangerous `go-getter` query parameters are rejected before OpenTofu runs.
- **Per-run credentials**: AWS credentials are served over loopback behind a per-run random token rather than copied from the host environment.
- **Not dumpable**: With `provider_uids`, `writ serve` marks itself not dumpable at start, so no process of its own uid can trace it or read its memory, and it writes no core dump of its keys and secrets.

See [Run isolation and egress](run-isolation.md) for setup and verification.

## Secret masking and encryption at rest

- **One masking boundary**: `internal/mask` produces every document leaving a run (masked plan, run text, events, diagnostics, state index rows). `internal/archtest` forbids any package outside `internal/tofu` from reading raw state JSON except through `mask.Index`.
- **Four masking rules**: OpenTofu sensitive marks, value sweep (including URL, JSON, hex, and base64 encodings), secret-like attribute names, and a strict output key allow-list.
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

## Known limits and assumptions

Review these boundaries before deploying Writ:

- **Host root and server operator are trusted**: Anyone with root on the `writ serve` host or read access to `env.*.key` and Postgres can decrypt states and assume configured AWS roles.
- **Plan role permissions are your responsibility**: Writ assumes whatever `plan_role` you configure. Make sure its IAM policy is strictly read-only.
- **S3 state isolation**: On `backend "s3"`, IAM role permissions (not Writ grants) prevent stacks in the same environment from referencing each other's S3 state keys.
- **Network egress**: Aside from the EC2 metadata firewall rule, runs share the host network so providers can reach registries and cloud APIs. Enforce stricter egress allow-lists at your host or VPC firewall if required.
- **Mask limits**: The mask is an output filter, not information-flow tracking. Unmarked secrets in unmarked attributes, hashed secrets passed to `nonsensitive()`, and multi-line base64 without sensitive marks are not caught. See [What the mask can't do](secrets.md#what-the-mask-cant-do).
- **Version 0 maturity**: Writ has been tested extensively against real OpenTofu 1.13.9 and real providers using local stand-ins, but has not been proven against a live cloud account or real GitLab instance. See [Status and limits](status.md).

## Next steps

- [Run isolation and egress](run-isolation.md): configure `provider_uids` and the `nftables` firewall rule.
- [Secrets and the mask](secrets.md): test the mask with `just verify-mask`.
- [API: What the API refuses](api.md#what-the-api-refuses): threat-by-threat breakdown of API defenses.
- [SECURITY.md](../SECURITY.md): how to report a security issue privately.
