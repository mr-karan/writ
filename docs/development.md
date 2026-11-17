# Development

If you change Writ itself, start here. Read [`AGENTS.md`](../AGENTS.md) for the repository rules and [Architecture](architecture.md) for the package map before you change code. [Writing style](style.md) sets the voice, terms, and rules for documentation.

## Commands

| Command | What it does |
| --- | --- |
| `just build` | Build `bin/writ` with `CGO_ENABLED=0`. |
| `just ci` | Run every local gate in order and stop at the first that fails: `check`, `e2e`, `e2e-import`, `e2e-import-hcl`, `e2e-aws-emulator`, `e2e-tofu-s3`, `verify-mask`, `policy-parity`, `test-serve`, `test-tfstate`, `test-tofu`, `website`, `check-nix-package`, and `check-nixos`. Print one line for each gate with its time. Set `TMPDIR` to a new directory under `/var/tmp` unless `TMPDIR` is set. Needs everything the gates need. |
| `just check` | Check that `vendorHash` matches the Go modules, then run gofmt, `go vet`, staticcheck, and `go test -race`. Run it before every commit. |
| `just e2e-import` | Plan imports of `hashicorp/aws` 6.70.0 objects through the importer's provider client, in a namespace with no network. Needs `unshare`. |
| `just e2e-import-hcl` | Take objects that `tofu` made into OpenTofu stacks with `writ import bulk`, and require a `tofu plan` that only imports and `writ import accept` passing on it. Needs Nix, `nomad`, `unshare`, and `ip`. |
| `just e2e-aws-emulator` | Create a launch template, a security group, an ALB, its listener, and a listener rule on AWS Emulator with `tofu` and `hashicorp/aws` 6.70.0, take them into an OpenTofu stack with `writ import bulk`, and require a `tofu plan` that shows exactly 5 imports and `writ import accept` passing on it. Needs Nix, `unshare`, `ip`, and `hashicorp/aws` 6.70.0 in OpenTofu's plugin cache, which `just e2e-aws-emulator-providers` fills. x86_64-linux only: elsewhere it skips. |
| `just e2e-aws-emulator-providers` | Fill OpenTofu's plugin cache, `TF_PLUGIN_CACHE_DIR` or `writ-e2e/tofu-plugins` in the user cache directory, with `hashicorp/aws` 6.70.0 from the OpenTofu registry, with every `AWS_` variable unset. Needs Nix and the network. |
| `just e2e` | Run the end-to-end test, with real `tofu` and `hashicorp/random`, `local`, and `tls`, in namespaces with only loopback: plans by commit under `writ serve --dev` judged by `examples/policy`, a commit that policy refuses, and approve, apply, plan again, and a stale plan under a team's `writ serve` with Postgres. Needs Nix, `unshare` with subordinate uids, and `ip`. Fills OpenTofu's plugin cache on first run. |
| `just e2e-tofu-s3` | Run a team's `writ serve` on an OpenTofu stack whose state is in a bucket of a local S3 stand-in, versitygw, with an STS stand-in for the plan and apply roles: a plan writes nothing to the bucket, an approved apply writes the state and the index, and a `tofu apply` outside Writ shows as drift. Needs what `just e2e` needs. |
| `just test-serve` | Run the tests of `control`, `tfstate`, `server`, `ui`, and `awsrole` with `-race` against a scratch Postgres, then the provider isolation tests in user and network namespaces. Needs Nix and `unshare` with subordinate uids. |
| `just test-tfstate` | Run OpenTofu 1.13.9 against the `http` state backend of `internal/tfstate`: plan, apply, a plan's token refused on writes, a second apply refused while one holds the lock, and a stale write refused. Needs Nix, `unshare`, and `ip`. |
| `just test-tofu` | Run `internal/tofu` against OpenTofu 1.13.9 with `random`, `local`, and `tls`, in namespaces, as `writ` and as a run uid. Needs Nix and `unshare` with subordinate uids. |
| `just fuzz-oidc` | Fuzz the OIDC token and key set parsers and the refusals of every flow, 2 minutes each by default: `just fuzz-oidc 5m`. |
| `just lint-gitlab-ci` | Lint the GitLab CI component and its example with yamllint. |
| `just verify-mask` | Plan and apply generated OpenTofu stacks twice through `writ serve --dev`, with two canaries, and require that what is stored and served is the same in both runs and holds neither canary; then fuzz the mask. `just verify-mask STACKS FUZZTIME` bounds it, 6 stacks and 30s by default. Needs what `just e2e` needs. |
| `just policy-parity` | Run `conftest verify` on `examples/policy`, then plan three OpenTofu stacks through `writ serve --dev` judged by `examples/policy` and again with `tofu` alone judged by `conftest`, and require the same verdicts and messages. Needs what `just e2e` needs. |
| `just proto` | Regenerate `internal/tfplugin5` and `internal/tfplugin6` from `proto/`. Needs Nix for `protoc`. |
| `just website` | Build the landing page and the docs site from `docs/` into `website/out`. |
| `just check-nix-package` | Build the flake's `writ` package. When `vendorHash` is stale, print the right hash and the line to change. Needs Nix. |
| `just update-nix-hash` | Write the right `vendorHash` into `deploy/nix/package.nix` and the module checksums into `deploy/nix/go-sum.sha256`. Needs Nix. |
| `just check-nixos` | Evaluate the flake's NixOS module, build the `writ` package its unit runs, and check a sample config with it. Needs Nix. |
| `just release-dry-run` | Build every release artifact into `dist/` and publish nothing, then install from `dist/`. Needs Nix. [release/README.md](../release/README.md) has the other release recipes. |

`just ci` is the full gate: run it before you merge to `main`. Run `lint-gitlab-ci` when a change touches what it tests.

### Hosted CI

GitHub Actions runs part of `just ci` on a GitHub-hosted Ubuntu runner, with Go and every other tool from the flake.

| Workflow | When | What it runs |
| --- | --- | --- |
| `.github/workflows/ci.yml` | A push to `main`, a pull request | `just build`; `go vet` with the build tags of the e2e, tofu, showcase, record, and failpoint tests; `just check` with Postgres 18 on `PATH`, so the `control`, `server`, `tfstate`, and `ui` tests also run against a scratch Postgres; and the control UI showcase test. |
| `.github/workflows/pages.yml` | A push to `main` that changes `website/`, `docs/`, or `README.md`, or a manual run | The build of `just website`, then a deploy of `website/out` to GitHub Pages at `writ.mrkaran.dev`. |
| `.github/workflows/release.yml` | A pushed tag `v*` | The release, as [release/README.md](../release/README.md#publish-a-release) describes. |

The runner has no user namespaces with subordinate uids, so these gates never run there: `e2e`, `e2e-import`, `e2e-import-hcl`, `e2e-aws-emulator`, `e2e-tofu-s3`, `verify-mask`, `policy-parity`, `test-tfstate`, `test-tofu`, and the provider isolation step of `test-serve`. In `just check`, the tests that need a user namespace skip and say so: the provider isolation tests and the `runas` tests. `check-nix-package`, `check-nixos`, and the fuzz recipes don't run either. Run `just ci` locally for all of them.

The integration tests in `just check` download `hashicorp/random`, `local`, and `tls` from `registry.opentofu.org`, so the CI job needs the registry to be reachable.

### Go module changes and the Nix package

`deploy/nix/package.nix` pins `vendorHash`, the hash of the Go modules that the Nix build downloads. Nix finds that download by `vendorHash` alone. If `go.mod` or `go.sum` changes and `vendorHash` doesn't, `nix build .#writ` fails with "inconsistent vendoring" or a hash mismatch.

`deploy/nix/go-sum.sha256` holds the SHA-256 of `go.mod`, `go.sum`, and `flake.lock` from when `vendorHash` was last computed. `flake.lock` is there because its nixpkgs supplies the Go that vendors the modules. `just check` compares these checksums with the files and fails on any difference. It doesn't run Nix.

After you change a Go dependency or update `flake.lock`, run `just update-nix-hash` and commit `deploy/nix/package.nix` and `deploy/nix/go-sum.sha256` with the change. Only that recipe writes `go-sum.sha256`, so don't edit the file by hand.

Don't edit `proto/tfplugin5.proto` or `proto/tfplugin6.proto`. They are copied from OpenTofu under MPL-2.0.

No test runs `apply` against a cloud provider, and no test reaches a real cloud host.

## How Writ is built

Writ is one Go module with no cgo. `cmd/writ` parses flags and calls `internal/cli`. Everything else is in `internal/`. [Architecture](architecture.md) has the package map, the path of one plan through the code, and the import rules that `internal/archtest` enforces on every `just check`.

### The provider protocol

`writ import bulk` reads objects through its own client of the Terraform plugin protocol, which speaks gRPC in version 5 and version 6 behind one interface. Version 5 is in wide use. On `registry.opentofu.org`, all 525 releases of `hashicorp/aws` serve only version 5, and so do the current `random`, `tls`, and `local`.

Writ decodes each version with code generated from its own `.proto` file, because the two versions disagree on field numbers in the schema messages. A version 5 schema decoded with version 6 types loses the `write_only` flag and the deprecation messages, with no error.

### State

OpenTofu owns a stack's state. A stack on `backend "http" {}` keeps it in `writ serve`'s control database, sealed under its environment's key, and a stack on `backend "s3"` keeps it in its own bucket. The control database is SQLite through `modernc.org/sqlite`, a pure Go driver, or Postgres through `pgx`. [How the control plane works](serve.md#opentofu-stacks) describes both backends.

### Tests

Unit tests run against real types. The `control`, `server`, `tfstate`, and `ui` tests use a real SQLite file, and a scratch Postgres when `initdb` is on `PATH` or `WRIT_TEST_POSTGRES` names a server.

`just e2e`, `just e2e-tofu-s3`, and `just verify-mask` build `writ` and run it through `writ serve` and `writ serve --dev` with OpenTofu stacks, and `just e2e-import-hcl` through `writ import bulk`.

The e2e tests that start `hashicorp/aws` run it in a namespace with no network and with mock credentials. `just e2e-import-hcl` gives the provider a stand-in of the AWS API that answers fixed replies, which pins exact responses.

`just e2e-aws-emulator` gives the provider AWS Emulator 1.0.385 instead, so real objects that `tofu` created are read back as the provider reads them. The flake's `aws-emulator` package, `deploy/nix/aws-emulator.nix`, is the vendor's prebuilt Debian package pinned by its SHA-256: a 187 MB download and 1 GB on disk. Only that recipe's `nix shell` puts it on `PATH`. The dev shell, `nix flake check`, and the `writ` package leave it out. Nix downloads it once, at build time. The test runs the emulator, `tofu`, and `writ` only in a user, mount, and network namespace with only loopback, so nothing in it can reach AWS or the internet. `tofu` there installs providers only from its plugin cache, read as a filesystem mirror, and never asks a registry, so fill the cache first with `just e2e-aws-emulator-providers`. In the namespace the test mounts an empty tmpfs over `~/.aws`, `~/.config/gcloud`, `~/.azure`, and `~/.docker`, binds an empty file over `~/.netrc`, in the caller's home and in `/root`, and masks the files that the caller's `AWS_SHARED_CREDENTIALS_FILE` and `AWS_CONFIG_FILE` name. It requires that synthetic credential files in a scratch home, masked the same way, read as absent or empty. The emulator listens on `127.0.0.1`, runs with `--telemetry off` and `EMULATOR_TELEMETRY=off`, and the test requires its log to say telemetry is disabled. The test removes every `AWS_` variable of the caller before it starts anything, sets `HOME` to a scratch directory, and points the provider at the emulator's own keys and the CA of its self-signed certificate through `AWS_SHARED_CREDENTIALS_FILE` and `AWS_CA_BUNDLE`. AWS Emulator 1.0.385 gives a new VPC security group no default egress rule, which the provider revokes on create, so the test untaints the group and applies again when that's the apply's only error, read from `tofu apply -json`. Most of a run is the provider's wait for the ALB to become active.

## Record a real token

The token fixtures in `internal/oidc/testdata/tokens` were signed in a test, in the claim shapes GitLab and GitHub document. A recorded token replaces the guess with fact. Each file holds a token, the key set that signed it, the clock and audience to check it with, and the check it should fail, `""` for none. The test runs every file in the directory.

1. In a sandbox project, add a job with `id_tokens: { WRIT_ID_TOKEN: { aud: https://writ.example.com } }` that writes `$WRIT_ID_TOKEN` to a protected artifact.
2. Save the key set from `https://gitlab.com/oauth/discovery/keys` at the same time.
3. Write a fixture with `at` set inside the token's life, and run `go test ./internal/oidc`.

A recorded token is expired by the time it's committed, and it names the sandbox project. Use a project that has nothing in it.

## Test plan for a real GitLab project

Each item needs a GitLab project in the owner's sandbox. No test here contacts GitLab.

| Check | How | Pass when |
| --- | --- | --- |
| The claims of a merge request job | Record a token as above | It passes every check, and `ref_protected` is the string `"true"` or `"false"` |
| The values of `user_access_level` | Record tokens of jobs that a developer and a maintainer start | The values are `developer` and `maintainer` |
| The claims of a manual job | Record the token of a manual job that a second user starts | `user_id` and `user_login` identify the user who started it, which approvals rely on |
| Input interpolation in a job name | Include the component with `stack: network` twice, with `env: staging` and `env: prod` | The pipeline has `writ-plan-network-staging` and `writ-plan-network-prod` |
| The plan job | Run a merge request pipeline against a `writ serve` the runner can reach, with the project added as a repository | The job, with no checkout, prints the plan of its commit and its id, and the audit log has its call with the job's user |

[The forge's test plan](#test-plan-for-a-real-gitlab-forge) lists the checks of the forge API and the read token.

## Test plan for a real GitLab forge

These checks need the owner's GitLab sandbox. Every test here runs against the stand-in in `internal/forge/forgetest`, which serves the shapes of GitLab's documents, read 2026-11-10. No test here contacts GitLab.

| Check | How | Pass when |
| --- | --- | --- |
| `read_api` covers the tree and raw blob endpoints | A project access token with Reporter and `read_api` only, `writ repo add` | The repository is added and its stacks are listed |
| A bot user's personal access token on GitLab.com Free | The same, with the bot user as Reporter of the group | The same |
| A service account's token | The same, with a GitLab service account | Record its tier and whether it works |
| Keyset pagination of the tree | A project with over 100 files under the path | Every file is listed, in more than one page, and the `Link` header has `page_token` |
| The tree lists blob ids that hash to the content | Any project | No file is refused as tampered |
| A renamed project keeps its row | Rename the project, wait for a poll | The poll works, by the numeric id |
| A deleted token | Revoke the token, wait for a poll | The row records the 401, and the message names the read token |
| A merge request job's `sha` | Run the component's job in a merge request pipeline | The plan is of the merge request's head commit, and the forge has it |
| The merge base takes a full ref | A merge request job, whose `ref_path` is `refs/heads/BRANCH` | The plan's `ref_check` is `on-ref`; the stand-in takes full refs, and GitLab isn't verified to |
| A merged results pipeline | Turn on merged results pipelines | Record whether the token's `sha` is fetchable through the commits API |
| A fork's merge request | Open one from a fork, with the pipeline in the parent project | Record the token's `project_id`, and that the plan is refused when it's the fork's |
| A stack name with `/` in a job name | Include the component with `stack: apps/api` | The pipeline has the job `writ-plan-apps/api-ENV` |

## Test plan for a real host

The tests here run in user and network namespaces on a developer's machine, with an STS stand-in. These checks need root on a real host, or AWS, and nothing has run them.

| Check | How | Pass when |
| --- | --- | --- |
| The unit's capabilities and the user switch under systemd | Deploy the NixOS module with `providerUids` on an EC2 instance with an instance role, and start `writ-serve` | `--check` in `ExecStartPre` prints `ok` on every isolation line, and both metadata lines say `ok`, not `not checked` |
| The metadata rule on EC2 | As root on that host, run `setpriv --reuid 61000 --regid 61000 --clear-groups curl -m 2 http://169.254.169.254/latest/meta-data/`, then the same `curl` as root | The first times out, and the second answers |
| Postgres refuses a provider's uid | As root, `setpriv --reuid 61000 --regid 61000 --clear-groups psql "postgres://writ@/writ?host=/run/postgresql"` | Peer authentication fails |
| A remote plan with a plan role | Plan an OpenTofu stack with `backend "s3"` in an environment whose `plan_role` is a real role of a sandbox account | The plan passes, and CloudTrail records the AssumeRole with the session name `writ-ci-REF` and the source identity `gitlab-4242-1849213` |
| A trust policy without `sts:SetSourceIdentity` | Remove it from the plan role's trust policy and plan | The plan fails with `denied` and names the role |

## The website

The site in `website/` holds no copy of the docs. `website/content.manifest.ts` says which file goes on which page, and `just website` fails when a selector no longer matches its source. [`website/README.md`](../website/README.md) has the details.
