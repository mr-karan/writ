# Architecture

Read this before you change Writ's code: it's the package map, and the reasons behind it.

## How a plan runs

Here's one plan of an OpenTofu stack, followed through the code, as `writ serve` runs it for `writ plan --remote`.

### How `writ serve` plans an OpenTofu stack

`createPlan` in `internal/server/plans.go` records the request as `planning` and starts `runPlan`, which fetches the commit and calls `planTofu` in `internal/server/tofuplan.go`. A short form of what it runs:

```go
// internal/server/tofuplan.go, planTofu
ts, err := repo.TofuSources(ctx, in.client, in.repo.ProjectID, snap, in.dir, s.store, repo.DefaultLimits)
launch, done, err := s.launch(ctx, ref, env, planRole, p) // a provider uid and the plan role's credentials
lock, err := s.tofuRunner.Fill(ctx, ts.Lock)             // the providers of .terraform.lock.hcl, into the plugin cache; stk.Lock = lock
raw, err := s.tofuRunner.Plan(ctx, stk)                  // tofu init and tofu plan as the provider uid
m, version, err := s.planMask(ctx, read, raw.File, refused)
doc, err := m.Plan(raw.JSON)                             // only the masked document leaves the run
body, err := json.Marshal(doc)
res.ID, res.Status = tofuPlanID(body, raw.File, in.source.Commit, stack, env), control.Pending
res.Saved = key.SealPlan(raw.File, res.ID, env, stack)
```

`runPlan` then records the plan with `control.Store.FinishPlan`.

## Constraints

`writ serve` runs OpenTofu on code from any commit it plans, for many teams, and has the keys of their states. Four constraints shape the code:

- OpenTofu and its providers run the commit's code. On a team's control plane they never run as `writ`'s own user: the config refuses a `tofu` block without `provider_uids`. Each run gets a provider uid, a private view of the filesystem, and only the credentials of its step.
- `tofu plan` and `tofu apply` print secrets. Nothing of a run's raw output is stored or shown: one mask writes every document that leaves it.
- An apply applies the saved plan that people approved, and nothing else. A plan's id binds the masked plan, the saved plan, the commit, the stack, and the environment.
- The repository's `AGENTS.md` keeps wire types inside the package that owns that boundary: protobuf messages, cty values, and SQL rows.

## Package map

| Package | Owns | Imports (internal) |
| --- | --- | --- |
| `diag` | `Span`, `Diagnostic`, classes, fixes | none |
| `addr` | `Addr`, the canonical address of an object an import names, and quoting for messages and shell words | none |
| `types` | the static types of provider schemas, `Assign`, printing | none |
| `value` | the values the importer and the provider client pass: known values, `Unknown`, secret marks, canonical encodings and keyed digests, redaction, secret-aware path printing | addr, schema |
| `schema` | annotated provider schemas, the annotation table | types |
| `provider` | registry install, the lock of installed providers, schema cache, processes, gRPC, cty, and starting a process with its own environment as another uid, for the importer's provider client and the isolation check | schema, types, value, tfplugin5, tfplugin6, runas |
| `runas` | acting as a run's uid: starting a process as it with no groups, file work as it through the thread's filesystem ids, checking that a started process holds no uid, group, or capability of writ's, and killing every process of a uid after its run, never of root or of writ's own uids. It imports only the standard library | none |
| `tofu` | starting OpenTofu, the one pinned version: init, a saved plan, its JSON, and the apply of a saved plan, each in a scratch directory of its own under a scratch root that no other user can change, which it opens once and reaches only through that descriptor, in a process group that a timeout or too much output kills and that's killed when tofu exits, as the run's uid, never root or writ's own, with no groups or capabilities, whose every process is killed after each step; and the fill of the shared plugin cache with a lock file's providers, as writ, from a configuration it writes itself, which then leaves the cache writable by writ alone | runas |
| `sqldb` | the SQL drivers: opening SQLite and Postgres, the `?` placeholder rewrite, `*IOError`, transactions, and the numbered migrations of the control schema | none |
| `confine` | reading and writing files by path, anywhere or only beneath one directory through an `os.Root`, and refusing a file that isn't a regular file without blocking on it | none |
| `tfcode` | reading the resource, provider, and import blocks of OpenTofu code, and writing HCL literals | schema, types, value |
| `importer` | taking existing objects into an OpenTofu stack: reading each through its provider, inferring the configuration that plans no change, and writing resource and import blocks with a variable for each secret (`bulk.go`, `hcl.go`); refusing an object that would write a secret in the clear (`secrets.go`); and checking a saved plan that adopts them (`accept.go`) | addr, diag, mask, provider, schema, tfcode, tofu, types, value |
| `oidc` | checking OIDC tokens through go-oidc with Writ's checks around it: compact JWS with RS256 or ES256, discovery, cached key sets; people's sign-in and the device flow through x/oauth2. A failure is a typed `Refusal` that holds no text from the identity provider or the token | none |
| `authz` | teams as rules over token claims, and grants over `stack@env` | none |
| `forge` | reading a repository where it's hosted: GitLab's REST API with a read-only token, and a local Git repository through the `git` binary for `writ serve --dev`; Git blob ids. `forgetest` serves GitLab's API shapes in a test's process. | none |
| `repo` | one commit's files, fetched only where the store lacks the blob and checked against its id; OpenTofu stacks found by their committed `.terraform.lock.hcl`, and one gathered for a plan with the local modules it calls, its lock file, moved to OpenTofu's registry if Terraform wrote it, and its backend block, within bounds of its own (`tofu.go`) | forge, tofu, diag |
| `awsrole` | `sts:AssumeRole` for `writ serve`'s runs, the loopback endpoint that serves each run's credentials to its providers behind a token, and the `Keeper` of one long-lived session | none |
| `control` | `writ serve`'s store, one schema on SQLite and Postgres: configs, teams, grants, plans, approvals, runs, overrides, sessions, token ids, the audit log, repositories, the commits read and their blobs, the state index, and the tokens, sealed state versions, and locks of the http state backend (`tfstate.go`) | authz, awsrole, sqldb |
| `mask` | what leaves an OpenTofu run for storage or display: the masked plan document, the state index rows, and each event line, with marked values, sensitive variables, secret-named keys, and secrets behind up to two URL, JSON, hex, or base64 encodings replaced; it refuses input past its size limits | value |
| `tfstate` | OpenTofu's `http` state backend for `writ serve`'s runs: the `GET`, `POST`, `LOCK`, and `UNLOCK` routes under `/v1/tfstate/`, a token for each run kept as its hash, a plan's token that only reads, and each state sealed under its environment's key. It reads only a state's serial and lineage. `server` mounts it | control |
| `policy` | Rego policies on the masked plan document of an OpenTofu plan, at the plan point and the apply point: it compiles each set against the input's schema and evaluates every set of a decision in a child `writ` with a deadline and a memory limit. It's the only package that imports OPA, and it imports no package of writ | none |
| `server` | `writ serve`'s REST API, its config, the dev identity and environments of `writ serve --dev` (`dev.go`), limits, remote plans of a commit, repositories and the poll of their default branches (`repos.go`), each plan's role and provider uid, and the isolation check; plans of OpenTofu stacks by commit through a `TofuRunner`, with a plan token on the http state backend it mounts, and only the masked plan kept (`tofuplan.go`), judged by the config's policies at the plan and apply points (`policy.go`) | oidc, authz, awsrole, control, forge, repo, provider, runas, diag, mask, tfstate, tofu, policy |
| `ui` | the control UI under `/ui/`: pages that render the API's documents, which it reaches only through `server`, and plan review of the masked OpenTofu plan | mask, server |
| `agent` | the writ skill for OpenTofu repositories and the section of `AGENTS.md` that `writ agent init` writes, embedded in the binary, and the merge that rewrites that section and nothing else | none |
| `mcp` | `writ mcp`: the Model Context Protocol and its SDK, the tools, `import_bulk` only with `--allow-providers`, their input schemas, and the bounds on what a result holds, behind a `Tools` interface that `cli` implements | none |
| `cli` | commands, human output, stable JSON, `writ import bulk` and `writ import accept` of an OpenTofu stack, the tools of `writ mcp`, `writ serve --dev` (`servedev.go`), and the `--remote` client with the order it finds a control plane in | everything above |
| `servetest` | a dev control plane inside a test's process: the real server and runner on `httptest`, SQLite in the test's directory | cli, control, server |
| `cmd/writ` | flag parsing, signals, exit codes, and asking `cli` for the failure document when a command stops without printing one | cli only |
| `tfplugin5`, `tfplugin6` | generated protobuf code (`just proto`) | none |
| `archtest` | tests that enforce the import rules and the `oidc` refusal boundary below | none |

`internal/archtest` fails the build when: anything but `provider` imports `tfplugin5`, `tfplugin6`, grpc, or protobuf, or anything but `provider`, `tfcode`, and `mask` imports go-cty; anything but `sqldb` imports `database/sql`, pgx, or the SQLite driver, or anything but `awsrole` imports the AWS SDK, or anything but `mcp` imports the MCP SDK; `addr`, `diag`, `schema`, `types`, or `value` import `os`, `net`, `io/fs`, `time`, or `crypto/rand`; `cmd/writ` imports any internal package other than `cli`; the control UI imports `control` or pgx; `tofu` imports a package of writ other than `runas`, or `runas` imports anything outside the standard library; anything but `policy` imports OPA, anything imports `lestrrat-go/jwx`, which OPA links, or `policy` imports a package of writ; outside `tofu`, a state's raw `tofu show -json` is read anywhere but as the input of the mask's index; an exported function of `oidc` returns an `error`, a field of `oidc.Refusal` isn't an integer, or `server`, `ui`, `cli`, or `cmd/writ` turn an `oidc.Refusal` into an interface value, such as an argument to `fmt` or `log/slog`. So a test enforces the wire-type rule and the boundaries around secrets, and neither is a convention a contributor can miss.


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

1. **One package writes what leaves a run.** `internal/mask` produces the masked plan, the text and events of a plan and an apply, the state index rows, and the diagnostics of a failed run. Each run's mask starts from the values of `provider_env` and the secret set of the state it reads, and follows the role credentials `awsrole.Broker` serves the run, refreshes included. `internal/archtest` refuses code outside `tofu` that reads a state's raw `tofu show -json` other than as the input of `mask.Index`. [Secrets and the mask](secrets.md) has the rules.
2. **A plan's id is a hash of what an apply relies on.** `tofuPlanID` is a SHA-256 over the masked plan, the SHA-256 of the saved plan, the commit, the stack, and the environment, each prefixed with its length. An approval is for that id, so it covers exactly one saved plan.
3. **Keys are per environment.** An environment's key seals its states on the http backend, its saved plans, and the secret set of each state version. No API route returns a saved plan or a secret set.
4. **A run's tokens end with the run.** `tfstate` gives each run a token on the http backend: a plan's token reads its stack's state, and an apply's token also locks and writes it. `control` keeps only each token's SHA-256.
5. **writ fills the plugin cache, and the run only reads it.** `tofu.Runner.Fill` installs the providers a lock file pins, as `writ`, from a configuration it writes itself, so no code of the stack runs as `writ`. A provider uid can read the cache and can't write it. Fill returns the lock file the run's init reads: for a provider pinned with no hashes, as `tofu.FromTerraform` leaves a `hashicorp/` provider of a lock file Terraform wrote, it adds the hashes tofu recorded as it installed it, because a run's init trusts the cache only through them.
6. **The lifecycle of a plan is one table.** `internal/control/lifecycle.go` lists every move of a plan's status, and the store refuses any other. [Approvals](approvals.md#track-a-plans-status) has the table.
7. **Both plugin protocols, for the importer.** `writ import bulk` reads objects through `provider`, Writ's own client of the plugin protocol. `random`, `tls`, and `local` serve only protocol 5, and the OpenTofu registry lists only protocol 5 for every `hashicorp/aws` release. `provider` speaks both. `install` accepts a release that lists protocol `5.x` or `6.x`, and the handshake accepts `5` or `6`. A private interface inside `provider` has one adapter for each protocol, over `tfplugin5` and `tfplugin6`. A protocol 5 message is never decoded with protocol 6 types: the schema messages disagree on field numbers 10 to 12, and the decode loses `write_only` and the deprecation messages without an error.
8. **Provider processes of the importer.** The handshake needs only `TF_PLUGIN_MAGIC_COOKIE`. `provider` sets `PLUGIN_UNIX_SOCKET_DIR` to a short temporary directory for each process and removes it after. It sets `TF_LOG_SDK`, `TF_LOG_SDK_PROTO`, and `TF_LOG_SDK_FRAMEWORK` to `off`. `StopProvider`, a closed connection, and SIGINT don't end a provider process. `provider` calls `plugin.GRPCController/Shutdown`, treats its `Unavailable` error as success, waits with a deadline, and then kills. The gRPC receive limit is 64 MiB.

## Invariants encoded in types

- `addr.Addr` and `types.Type` have unexported fields. Only constructors make them, so no address or type is malformed.
- An exported function of `oidc` never returns an `error`. A failure is an `oidc.Refusal`, whose fields are integers, so no text from an identity provider or a token reaches a response or a log. `internal/archtest` enforces both.
- The risk class of each action list of an OpenTofu plan is one table, `tofuActions` in `internal/server/tofuplan.go`. A plan with an action list the table lacks is refused.
- The diagnostic classes (`diag`) and the risk classes (`authz.Risks`) are each one list.

## One owner for each piece of state

| State | Single owner | Everyone else |
| --- | --- | --- |
| the control database | `control`, through `sqldb` | `server` and `tfstate` call the store, and `ui` reaches it only through `server` |
| the state versions, tokens, and locks of the http backend | `control` (`tfstate.go`) | `tfstate` serves them to OpenTofu, and a run reaches them only with its token |
| a stack's state on S3 | the stack's bucket | a run reaches it with the environment's role, and OpenTofu takes the bucket's lock |
| the plugin cache | `tofu`, which fills it as `writ` | provider uids read it |
| a run's scratch directory | `tofu`, through one descriptor of the scratch root | |
| `writ serve`'s free provider uids and each run's credential token | `server`, a channel of uids, and `awsrole.Broker` | providers fetch credentials over loopback |
| fetched files of a commit | `control`, in the table `blobs`, by Git blob id | `repo` fetches only the blobs the store lacks |
| the importer's provider binaries and schema cache | `provider`, under `WRIT_CACHE_DIR` | `importer` and the `schema` tool of `writ mcp` |

## Testing

- **Unit tests per package**, table-driven with `t.Run`, against real types. The `control`, `server`, `tfstate`, and `ui` tests use a real SQLite file in `t.TempDir()`, and a scratch Postgres when `initdb` is on `PATH` or `WRIT_TEST_POSTGRES` names a server.
- **`internal/archtest`** enforces the import rules and the boundaries around secrets on every `just check`.
- **End to end**, `just e2e` (build tags `e2e` and `e2e_tofu`): builds `writ`, fills an OpenTofu plugin cache with the real `random`, `tls`, and `local` providers once, and drives the binary and real `tofu` through `writ serve` and `writ serve --dev` with OpenTofu stacks, in namespaces with only loopback: plans by commit, approvals and applies, stale plans, and the state index. `just e2e-tofu-s3` does the same for a stack whose state is on an S3 stand-in, and `just verify-mask` for the mask.
- **Other targets.** `just test-serve` runs the server packages against Postgres and the provider isolation test, `just test-tfstate` and `just test-tofu` run `tfstate` and `tofu` against real OpenTofu, `just policy-parity` compares policy verdicts with `conftest`, `just e2e-import` plans imports through the importer's provider client against `hashicorp/aws` offline, and `just e2e-import-hcl` takes objects into OpenTofu stacks with `writ import bulk`.

### Test clocks and database budgets

`control.Options.ConnectTimeout` bounds Postgres connection setup, five seconds
by default. `control.Options.Location` names the SQLite file or Postgres
database. A Postgres session has a statement timeout of 10 seconds, a lock
timeout of five seconds, and an idle-in-transaction timeout of 15 seconds.
SQLite waits five seconds for a busy database.

`server.Options.WithTimeout` replaces deadline construction for tests that
advance time after the runner enters an apply. It must preserve the difference
between cancellation and deadline expiry. Production uses
`context.WithTimeoutCause`.

Only builds with the `failpoint` tag read `WRIT_TEST_DATABASE_TIMEOUT`, a
positive duration that replaces each Postgres budget above. Normal binaries
ignore it.

## Dependencies

Each is pinned at an exact version, and the commit that adds it says why. Each date is the day its version was tagged.

| Module | Version | Why |
| --- | --- | --- |
| `google.golang.org/grpc` | v1.85.0 (2026-10-27) | plugin protocol client; OPA v1.22.4 requires v1.85.0 |
| `google.golang.org/protobuf` | v1.36.12 (2026-08-10) | generated messages; `protoc-gen-go` comes from the same module |
| `google.golang.org/grpc/cmd/protoc-gen-go-grpc` | v1.6.2 (2026-05-07) | generator for `just proto`, not linked |
| `github.com/zclconf/go-cty` | v1.19.0 (2026-07-06) | msgpack `DynamicValue` encoding and schema type decoding in `provider`, and HCL values in `tfcode` and `mask`; v1.20.0 is two days old |
| `github.com/hashicorp/hcl/v2` | v2.24.0 (2025-07-07) | `writ serve`'s config, and reading and writing OpenTofu code |
| `modernc.org/sqlite` | v1.61.0 (2026-10-21) | pure Go SQLite for the control database of `writ serve --dev`, so `CGO_ENABLED=0` holds |
| `github.com/jackc/pgx/v5` | v5.11.0 (2026-09-07) | `database/sql` driver for the control database of a team's `writ serve` |
| `github.com/aws/aws-sdk-go-v2/config` | v1.33.11 (2026-10-26) | the credential chain of `writ serve`'s own AWS identity: SSO profiles, assumed roles, web identity, container and instance metadata |
| `github.com/aws/aws-sdk-go-v2/service/sts` | v1.51.5 (2026-10-22) | `sts:AssumeRole` for each environment's plan and apply roles, in `awsrole` |
| `github.com/aws/aws-sdk-go-v2/service/s3` | v1.114.5 (2026-10-22) | the S3 client of the end-to-end test of a stack on S3, against its stand-in; v1.114.6 was two weeks old to the day |
| `github.com/aws/aws-sdk-go-v2/credentials` | v1.20.11 (2026-10-26) | static credentials for that test's stand-in |
| `github.com/coreos/go-oidc/v3` | v3.21.0 (2026-09-01) | OIDC discovery, ID token verification, and userinfo for `writ serve`, as `AGENTS.md` requires; only `internal/oidc` imports it |
| `github.com/go-jose/go-jose/v4` | v4.1.6 (2026-10-14) | JWS and JWK parsing under go-oidc; v4.1.6 fixes GHSA-mgcc-cww5-fp2q in JWK parsing of RSA keys, and v4.1.7 to v4.1.9 are under two weeks old |
| `github.com/open-policy-agent/opa` | v1.22.4 (2026-11-12) | compiles and evaluates the Rego of `internal/policy`, the only package that imports it; the latest release, as the owner chose on 2026-11-16. v1.21.0 to v1.22.1 break two rule forms, so a deny can fail to fire |
| `golang.org/x/oauth2` | v0.37.0 (2026-08-25) | the device flow of `writ login`, the code flow with PKCE of `writ login` in a browser and of the control UI; v0.38.0 is four days old |
| `github.com/modelcontextprotocol/go-sdk` | v1.8.0 (2026-09-04) | the Model Context Protocol of `writ mcp` |
| `github.com/mattn/go-isatty` | v0.0.24 (2026-07-23) | whether `writ` writes to a terminal, which decides color |
| `golang.org/x/net` | v0.61.0 (2026-10-09) | `idna`, to compare hosts in the config's identity check, and `html` in the control UI's tests |
| `golang.org/x/sys` | v0.49.0 (2026-10-06) | `Fchmodat` with no link followed, for the mode of the SQLite file |
| `golang.org/x/tools` | v0.52.0 (2026-10-10) | `go/packages` for `internal/archtest` |

`protoc` comes from nixpkgs. Not added: `hashicorp/go-plugin` (the handshake is about 100 lines and the library brings hclog and yamux), and cobra or pflag (the standard `flag` package handles subcommands and `--long` flags).
