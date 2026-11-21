# Architecture

Read this before changing Writ's code: it maps every package, data flow, and boundary enforced by `internal/archtest`.

## How a plan runs

### How `writ serve` plans an OpenTofu stack

`createPlan` (`internal/server/plans.go`) inserts the `planning` row and launches `runPlan`, which fetches the commit and invokes `planTofu` (`internal/server/tofuplan.go`):

```go
// internal/server/tofuplan.go, planTofu
ts, err := repo.TofuSources(ctx, in.client, in.repo.ProjectID, snap, in.dir, s.store, repo.DefaultLimits)
run, err := s.startRun(ctx, runSpec{id: ref, env: env, role: planRole /* ... */}) // the plan role's credentials, and a slot of the executor: a provider uid
err = run.stateToken(ctx, stack, control.StatePlan, ttl)                          // a token that reads the stack's state on the http backend
out, err := run.run(ctx, ctx, run.job(runner.Plan), fillMask)                     // the providers of .terraform.lock.hcl into the plugin cache, then tofu init, plan, and show as the provider uid
raw := out.Plan
m, version, err := s.planMask(ctx, read, raw.File, refused)
doc, err := m.Plan(raw.JSON)                                                      // only the masked document leaves the run
body, err := json.Marshal(doc)
res.ID, res.Status = tofuPlanID(body, raw.File, in.source.Commit, stack, env), control.Pending
res.Saved = key.SealPlan(raw.File, res.ID, env, stack)
```

`run.job` builds a data-only `runner.Job` (no keys, handlers, or UIDs) executed by `runner.Local` inside `writ serve`. `runPlan` then commits the result through `control.Store.FinishPlan`.

## Constraints

Four constraints shape the codebase:

- **Isolated execution:** Team servers refuse `tofu` without `provider_uids`; every run executes under its own UID, private namespaces, seccomp filter, cgroup limits, and step-scoped credentials.
- **Single masking boundary:** Raw `tofu` output never leaves a run; `internal/mask` produces every stored or displayed document.
- **Immutable plan binding:** Plan IDs hash the masked plan, saved plan binary, commit, stack, and environment; applies execute only that sealed plan.
- **Wire-type containment:** Protobuf messages, cty values, and SQL rows stay inside the package that owns that boundary (`AGENTS.md`).

## Package map

| Package | Owns | Internal imports |
| --- | --- | --- |
| `diag` | `Span`, `Diagnostic`, classes, fixes | none |
| `addr` | Canonical resource addresses and safe quoting | none |
| `types` | Static provider schema types and `Assign` | `addr` |
| `value` | Importer/provider values, `Unknown`, secret marks, canonical encodings, redaction | `addr`, `schema` |
| `schema` | Annotated provider schemas | `types` |
| `provider` | Registry install, schema cache, gRPC plugin client (v5/v6), cty, provider subprocesses | `schema`, `types`, `value`, `tfplugin5`, `tfplugin6`, `runas` |
| `runas` | UID switching, user namespace ID maps, capability checks, UID process cleanup (stdlib only) | none |
| `tofu` | Pinned OpenTofu execution (`init`, `plan`, `show`, `apply`, plugin cache fill), scratch dir confinement, namespaces, cgroups, seccomp, per-step loopback network sockets | `runas` |
| `sqldb` | SQLite/Postgres drivers, `?` placeholder rewrite, `*IOError`, transactions, migrations | none |
| `confine` | Confined regular-file reads/writes via `os.Root` | none |
| `tfcode` | Reading OpenTofu resource/provider/import blocks and formatting HCL literals | `schema`, `types`, `value` |
| `importer` | `writ import bulk` (read, infer, generate HCL/manifest, secret guard) and `writ import accept` | `addr`, `diag`, `mask`, `provider`, `schema`, `tfcode`, `tofu`, `types`, `value` |
| `oidc` | Compact JWS (`RS256`/`ES256`), discovery, JWKS cache, `go-oidc`/`oauth2` flows, typed integer `Refusal` | none |
| `authz` | Claim-matching teams and `stack@env` grants | none |
| `forge` | GitLab REST read client, local `git` reader for `--dev`, Git SHA-1 blob verification, `forgetest` | none |
| `repo` | Commit blob caching, stack discovery, lock-file translation, backend/module validation | `forge`, `tofu`, `diag` |
| `egress` | Per-run egress allow-list matching, CONNECT/HTTP proxy, connection bounds, destination recording (stdlib only) | none |
| `awsrole` | AWS STS `AssumeRole`, loopback container credential endpoint, session refresh `Keeper` | none |
| `notify` | Outbound Slack and HMAC-signed webhook notifications, bounded queues, retries, TLS/DNS classification | none |
| `runner` | Data-only `Job`/`Executor` interface and `Local` executor (UID pool, scratch dir, egress proxy, install proxy, loopback relay); holds no keys, DB, forge, or mask | `egress`, `runas`, `tofu` |
| `control` | Control database store on SQLite/Postgres (configs, teams, grants, repos, blobs, plans, approvals, runs, sessions, audit, state index, HTTP state backend) | `authz`, `sqldb` |
| `mask` | Single masking boundary for plan JSON, plan/apply text, events, diagnostics, state index, and egress hostnames | `value` |
| `tfstate` | OpenTofu `http` backend (`/v1/tfstate/`), per-run hashed tokens, sealed state storage | `control` |
| `policy` | Host-side Rego compilation against input schema and bounded child-process OPA evaluation (sole OPA importer) | none |
| `server` | REST API, config parsing/validation, `--dev` mode, repo polling, webhooks, credential/state relay, OpenTofu plan/apply orchestration, drift checks, policy and notification wiring | `oidc`, `authz`, `awsrole`, `control`, `forge`, `repo`, `runas`, `runner`, `diag`, `egress`, `mask`, `notify`, `tfstate`, `tofu`, `policy` |
| `ui` | Server-rendered `/ui/` HTML pages delegating exclusively to `server` handlers | `mask`, `server` |
| `agent` | Embedded skill files and marker-based `AGENTS.md` merge for `writ agent init` | none |
| `mcp` | Stdio MCP server, tool schemas, response truncation bounds behind `Tools` interface | none |
| `cli` | Subcommands, human/JSON formatting, `writ import`, `writ mcp` backend, `writ serve --dev`, remote client | all above |
| `servetest` | In-process dev control plane (`httptest` + SQLite) for tests | `cli`, `control`, `server` |
| `cmd/writ` | Flag dispatch, signal handling, exit codes | `cli` only |
| `tfplugin5`, `tfplugin6` | Generated protobuf code (`just proto`) | none |
| `archtest` | Automated import, wire-type, raw-state, and `oidc.Refusal` boundary enforcement | none |

`internal/archtest` enforces all package boundaries on every `just check`: protobuf/gRPC stay in `provider`, `go-cty` stays in `provider`/`tfcode`/`mask`, SQL drivers stay in `sqldb`, AWS SDK stays in `awsrole`, MCP SDK stays in `mcp`, OPA stays in `policy` (no `lestrrat-go/jwx`), `runas`/`egress` import stdlib only, `runner` never imports DB/keys/mask/oidc/forge/policy, raw `tofu show -json` state flows only into `mask.Index`, and `oidc` exports never return `error` or leak `oidc.Refusal` as an interface value.

## Data flow

```
commit ─ repo.TofuSources ─▶ stack files, lock file, backend block
                                 │
    tofu.Runner.Fill, as writ ───┤ providers of the lock file, into the plugin cache
                                 ▼
    tofu init, plan, show -json, as a provider uid, with a state token that reads
                                 │ raw plan JSON and plan file, which stay in the run
                                 ▼
    mask.Plan ─▶ masked plan ─ policy, plan point ─▶ control: plan row, saved plan sealed
                                                          │
    approvals ─ policy, apply point ─ tofu apply of the saved plan, as a provider uid,
                                      with a state token that locks and writes
                                                          │
    tfstate: a new state version, sealed, with its secret set ─ mask.Index ─▶ state index rows
```

## Decisions

1. **One masking package:** `internal/mask` produces every stored/displayed run output; `internal/archtest` prevents raw state access elsewhere.
2. **Content-addressed plan IDs:** `tofuPlanID` hashes length-prefixed masked plan, saved plan SHA-256, commit, stack, and env.
3. **Per-environment sealing:** One key file per env seals HTTP state, saved plans, and secret sets (via separate HKDF labels).
4. **Single-run state tokens:** `tfstate` issues read-only plan tokens and read/write/lock apply tokens stored only as SHA-256 hashes and revoked on run end.
5. **Trusted plugin cache fills:** `tofu.Runner.Fill` populates `tofu.plugin_cache` from a synthetic config as `writ` (behind a public-only install proxy under `egress_mode` `report`/`enforce`); run UIDs mount the cache read-only.
6. **Single lifecycle table:** `internal/control/lifecycle.go` owns all valid plan status transitions.
7. **Dual plugin protocol support:** `provider` speaks both protocol v5 and v6 with separate generated adapters because schema field numbers 10 to 12 differ between versions.
8. **Clean provider shutdown:** `provider` shuts down gRPC plugins via `GRPCController/Shutdown`, waits with deadline, then kills; sets short `PLUGIN_UNIX_SOCKET_DIR` and 64 MiB gRPC max receive size.
9. **Loopback-only run networking:** Under `egress_mode` `report`/`enforce`, each step's helper binds three loopback listeners inside the step's network namespace and passes them to `writ serve` over a socket pair so `writ serve` identifies each run by listener.

## Invariants encoded in types

- `addr.Addr` and `types.Type` have unexported fields constructed only via validated constructors.
- Exported `oidc` functions never return `error`; failures are integer-only `oidc.Refusal` structs.
- OpenTofu action-to-risk mapping lives in one table (`tofuActions` in `internal/server/tofuplan.go`); unknown actions fail closed.
- Diagnostic classes (`diag`) and risk classes (`authz.Risks`) each have one authoritative definition.

## One owner for each piece of state

| State | Single owner | Access by others |
| --- | --- | --- |
| Control database | `control` (via `sqldb`) | `server`/`tfstate` call store; `ui` goes through `server` |
| HTTP state versions, tokens, locks | `control` (`tfstate.go`) | `tfstate` serves to OpenTofu behind per-run tokens |
| S3 state | Stack's S3 bucket | OpenTofu accesses via `plan_role` / `apply_role` |
| Plugin cache | `tofu` (filled as `writ`) | Mounted read-only to provider UIDs |
| Run scratch directory | `tofu` (via open root FD) | Confined to single step |
| Provider UIDs and STS tokens | `runner.UIDs`, `awsrole.Broker` | Loopback credential endpoint |
| Per-step network | `tofu` | `runner.Local` serves egress/install proxies; `server` relays state/creds |
| Cached commit blobs | `control` (`blobs`) | `repo` fetches missing blobs |
| Importer provider/schema cache | `provider` (`WRIT_CACHE_DIR`) | `importer`, `writ mcp` `schema` |

## Testing

- **Unit tests:** Table-driven with `t.Run`; DB tests run against SQLite and scratch Postgres (when `initdb` or `WRIT_TEST_POSTGRES` is present).
- **Architecture tests:** `internal/archtest` runs on every `just check`.
- **End-to-end suites:** `just e2e`, `just e2e-tofu-s3`, `just verify-mask`, `just policy-parity`, `just test-serve`, `just test-tfstate`, `just test-tofu`, `just e2e-import`, `just e2e-import-hcl`, `just e2e-aws-emulator` ([Development](development.md)).

### Test clocks and database budgets

`control.Options.ConnectTimeout` defaults to `5s`; Postgres sessions enforce `10s` statement, `5s` lock, and `15s` idle-in-transaction timeouts; SQLite waits `5s` when busy. `server.Options.WithTimeout` lets tests replace `context.WithTimeoutCause`. Builds tagged `failpoint` honor `WRIT_TEST_DATABASE_TIMEOUT` (ignored by normal binaries).

## Dependencies

All dependencies are pinned to exact versions (tag date in parentheses):

| Module | Version | Purpose |
| --- | --- | --- |
| `google.golang.org/grpc` | v1.85.0 (2026-10-27) | Plugin protocol client (also required by OPA v1.22.4) |
| `google.golang.org/protobuf` | v1.36.12 (2026-08-10) | Generated plugin protocol messages |
| `google.golang.org/grpc/cmd/protoc-gen-go-grpc` | v1.6.2 (2026-05-07) | Codegen tool for `just proto` (not linked) |
| `github.com/zclconf/go-cty` | v1.19.0 (2026-07-06) | Msgpack `DynamicValue`, schema types, HCL values (`provider`, `tfcode`, `mask`) |
| `github.com/hashicorp/hcl/v2` | v2.24.0 (2025-07-07) | Server config and OpenTofu HCL parsing/writing |
| `modernc.org/sqlite` | v1.61.0 (2026-10-21) | Pure-Go SQLite (`CGO_ENABLED=0`) for `--dev` |
| `github.com/jackc/pgx/v5` | v5.11.0 (2026-09-07) | Postgres `database/sql` driver |
| `github.com/aws/aws-sdk-go-v2/config` | v1.33.11 (2026-10-26) | Host AWS identity credential chain |
| `github.com/aws/aws-sdk-go-v2/service/sts` | v1.51.5 (2026-10-22) | `sts:AssumeRole` in `awsrole` |
| `github.com/aws/aws-sdk-go-v2/service/s3` | v1.114.5 (2026-10-22) | S3 e2e test client against local stand-in |
| `github.com/aws/aws-sdk-go-v2/credentials` | v1.20.11 (2026-10-26) | Static credentials for S3 e2e stand-in |
| `github.com/coreos/go-oidc/v3` | v3.21.0 (2026-09-01) | OIDC discovery, token verification, userinfo (`internal/oidc` only) |
| `github.com/go-jose/go-jose/v4` | v4.1.6 (2026-10-14) | JWS/JWK parsing under `go-oidc` (includes GHSA-mgcc-cww5-fp2q fix) |
| `github.com/open-policy-agent/opa` | v1.22.4 (2026-11-12) | Rego compilation and evaluation (`internal/policy` only) |
| `golang.org/x/oauth2` | v0.37.0 (2026-08-25) | Device and PKCE code OAuth flows |
| `github.com/modelcontextprotocol/go-sdk` | v1.8.0 (2026-09-04) | `writ mcp` stdio server |
| `github.com/mattn/go-isatty` | v0.0.24 (2026-07-23) | TTY detection |
| `golang.org/x/net` | v0.61.0 (2026-10-09) | `idna` host comparison and UI HTML tests |
| `golang.org/x/sys` | v0.49.0 (2026-10-06) | Syscall helpers (`Fchmodat` without following symlinks) |
| `golang.org/x/tools` | v0.52.0 (2026-10-10) | `go/packages` for `internal/archtest` |

`protoc` comes from nixpkgs. Intentionally omitted: `hashicorp/go-plugin` (avoiding `hclog`/`yamux`) and `cobra`/`pflag` (stdlib `flag` suffices).
