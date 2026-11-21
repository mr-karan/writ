# Development

Read [`AGENTS.md`](../AGENTS.md) for repository rules, [Architecture](architecture.md) for the package map, and [Writing style](style.md) for documentation rules before making changes.

## Commands

| Command | Purpose |
| --- | --- |
| `just build` | Build `bin/writ` with `CGO_ENABLED=0`. |
| `just ci` | Run all local gates in order (`check`, `e2e`, `e2e-import`, `e2e-import-hcl`, `e2e-aws-emulator`, `e2e-tofu-s3`, `verify-mask`, `policy-parity`, `test-serve`, `test-tfstate`, `test-tofu`, `website`, `check-nix-package`, `check-nixos`). Sets `TMPDIR` under `/var/tmp` if unset. |
| `just check` | Verify Nix `vendorHash` checksums, then run `gofmt`, `go vet`, `staticcheck`, and `go test -race`. Run before every commit. |
| `just e2e-import` | Plan imports of `hashicorp/aws` 6.70.0 objects via Writ's provider client in a networkless namespace (needs `unshare`). |
| `just e2e-import-hcl` | Import objects created by `tofu` via `writ import bulk` and verify clean `tofu plan` + `writ import accept` (needs Nix, `nomad`, `unshare`, `ip`). |
| `just e2e-aws-emulator` | Create a launch template, security group, ALB, target group, listener, and listener rule on AWS Emulator with `tofu` and `hashicorp/aws` 6.70.0, import them with `writ import bulk`, and require 6 clean imports + `writ import accept` (x86_64-linux only; populate cache first with `just e2e-aws-emulator-providers`). |
| `just e2e-aws-emulator-providers` | Populate OpenTofu's plugin cache (`TF_PLUGIN_CACHE_DIR` or `writ-e2e/tofu-plugins`) with `hashicorp/aws` 6.70.0 with all `AWS_*` vars unset. |
| `just e2e` | Full end-to-end suite with OpenTofu 1.13.9, Postgres, and real `hashicorp/random`, `local`, and `tls` in loopback-only namespaces: `--dev` commit plans + policy, team approve/apply/stale plans/drift (needs Nix, `unshare` with subordinate UIDs, `ip`). |
| `just e2e-tofu-s3` | Team `writ serve` with `backend "s3"` against local `versitygw` + STS stand-in: read-only plans, approved applies, state index, outside-write drift, and adoption. |
| `just test-serve` | `control`, `tfstate`, `server`, `ui`, and `awsrole` tests with `-race` against scratch Postgres, plus UID isolation tests in namespaces (needs Nix, `unshare` with subordinate UIDs, and `systemd-run --user` for a delegated cgroup). |
| `just test-tfstate` | OpenTofu 1.13.9 against `internal/tfstate`: plan, apply, read-only plan tokens, lock contention, stale write refusal (needs Nix, `unshare`, and `ip`). |
| `just test-tofu` | `internal/tofu` against OpenTofu 1.13.9 in namespaces as `writ` and as a run UID (needs Nix and `unshare` with subordinate UIDs). |
| `just fuzz-oidc` | Fuzz OIDC token/JWKS parsers and refusals (`2m` each by default; `just fuzz-oidc 5m`). |
| `just lint-gitlab-ci` | Lint `ci/gitlab/` with `yamllint`. |
| `just verify-mask` | Differential canary test + `FuzzPlan` (`just verify-mask STACKS FUZZTIME`, default `6` stacks / `30s`). |
| `just policy-parity` | Run `conftest verify` on `examples/policy` and require identical verdicts/messages between `writ serve --dev` and `conftest` across three stacks. |
| `just proto` | Regenerate `internal/tfplugin5` and `internal/tfplugin6` from `proto/` (never hand-edit `.proto` files, copied from OpenTofu under MPL-2.0). |
| `just website` | Build the website and docs into `website/out`. |
| `just check-nix-package` | Build the flake's `writ` package and verify `vendorHash`. |
| `just update-nix-hash` | Recompute `vendorHash` in `deploy/nix/package.nix` and checksums in `deploy/nix/go-sum.sha256`. |
| `just check-nixos` | Evaluate the NixOS module, build its `writ` + OpenTofu packages, and validate its sample config. |
| `just release-dry-run` | Build release artifacts locally into `dist/` and test installing from them ([`release/README.md`](../release/README.md)). |

Run `just ci` before merging to `main`.

### Hosted CI

GitHub Actions runs on Ubuntu runners using tools from `flake.nix`:

| Workflow | Trigger | Runs |
| --- | --- | --- |
| `.github/workflows/ci.yml` | Push to `main`, PR | `just build`, tagged `go vet`, `just check` with Postgres 18, and UI showcase test |
| `.github/workflows/pages.yml` | Changes to `website/`, `docs/`, `README.md`, or manual | `just website` and GitHub Pages deploy (`writ.mrkaran.dev`) |
| `.github/workflows/release.yml` | Tag `v*` | Release publish ([`release/README.md`](../release/README.md#publish-a-release)) |

Because hosted runners lack subordinate UIDs, namespace/UID-switching e2e gates (`e2e`, `e2e-tofu-s3`, `verify-mask`, `policy-parity`, `test-tofu`, `test-tfstate`, etc.) and Nix package checks run locally via `just ci`. Integration tests in `just check` require `registry.opentofu.org` access for `random`, `local`, and `tls`.

### Go module changes and the Nix package

`deploy/nix/package.nix` pins `vendorHash`, and `deploy/nix/go-sum.sha256` records SHA-256 digests of `go.mod`, `go.sum`, and `flake.lock` (checked by `just check`). After changing Go dependencies or `flake.lock`, run `just update-nix-hash` and commit both files; don't edit `go-sum.sha256` by hand.

No test runs `apply` against a cloud provider or contacts a real cloud host.

## How Writ is built

Writ is a single `CGO_ENABLED=0` Go module: `cmd/writ` parses flags and delegates to `internal/cli`, and everything else lives in `internal/` ([Architecture](architecture.md)).

- **Provider protocol:** `writ import bulk` speaks both Terraform plugin protocol v5 and v6 behind one adapter interface (`hashicorp/aws`, `random`, `tls`, and `local` all use v5). Each version decodes with its own generated protobuf types because schema field numbers 10 to 12 differ.
- **State and database:** HTTP-backend state lives sealed in Postgres (`pgx`) or SQLite (`modernc.org/sqlite`); S3-backend state lives in S3.
- **AWS Emulator testing:** `just e2e-aws-emulator` runs pinned AWS Emulator 1.0.385 (`deploy/nix/aws-emulator.nix`, excluded from default dev shell) inside a loopback-only namespace with masked cloud credential dirs (`~/.aws`, `~/.config/gcloud`, `~/.azure`, `~/.docker`, `~/.netrc`), `--telemetry off`, and offline filesystem plugin mirror.

## Record a real token

Token fixtures in `internal/oidc/testdata/tokens` use documented GitLab/GitHub claim shapes. To record a real token from an empty sandbox project:

1. Add a CI job with `id_tokens: { WRIT_ID_TOKEN: { aud: https://writ.example.com } }` saving `$WRIT_ID_TOKEN` to a protected artifact.
2. Save `https://gitlab.com/oauth/discovery/keys` at the same time.
3. Add a fixture with `at` inside the token's lifetime and run `go test ./internal/oidc`.

## Test plan for a real GitLab project

Needs a GitLab sandbox project (automated tests use a local stand-in):

| Check | How | Pass condition |
| --- | --- | --- |
| MR job claims | Record a token as above | Passes all checks; `ref_protected` is `"true"` or `"false"` |
| `user_access_level` values | Record tokens for developer and maintainer jobs | Values are `developer` and `maintainer` |
| Manual job claims | Record manual job token started by a second user | `user_id` and `user_login` reflect the triggering user |
| Job name interpolation | Include component for `stack: network` with `staging` and `prod` | Pipeline creates `writ-plan-network-staging` and `writ-plan-network-prod` |
| Plan job | Run MR pipeline against reachable `writ serve` | Job plans commit without checkout and logs user in audit table |

## Test plan for a real GitLab forge

Automated tests use `internal/forge/forgetest` (modeled on GitLab API responses, 2026-11-10):

| Check | How | Pass condition |
| --- | --- | --- |
| `read_api` scope | Reporter project token (`read_api` only) + `writ repo add` | Repo added and stacks discovered |
| GitLab.com Free bot PAT | Bot user as group Reporter (`read_api`) | Repo added and stacks discovered |
| Service account token | GitLab service account | Record tier and outcome |
| Keyset pagination | Project with > 100 files under `path` | All pages fetched via `Link` `page_token` |
| Blob hash verification | Any project | Zero tampered-blob refusals |
| Project rename | Rename project and wait for poll | Poll succeeds by numeric `project_id` |
| Revoked token | Revoke read token and wait for poll | Row records `401` naming read token |
| MR job `sha` | Run component in MR pipeline | Forge resolves MR head commit |
| Full ref merge base | MR job with `ref_path = refs/heads/BRANCH` | `ref_check` returns `on-ref` |
| Merged results pipeline | Enable merged results pipelines | Verify token `sha` is fetchable via commits API |
| Fork MR | Open MR from fork | Refused when `project_id` is fork's |
| Nested stack name | Component with `stack: apps/api` | Job `writ-plan-apps/api-ENV` runs cleanly |

## Test plan for a real host

Needs root on a real Linux host and an AWS sandbox account:

| Check | How | Pass condition |
| --- | --- | --- |
| Systemd capabilities and UID switch | Deploy NixOS module with `providerUids` on EC2 | `ExecStartPre` `--check` prints `ok` for all isolation and metadata checks |
| EC2 metadata firewall | `setpriv --reuid 61000 --regid 61000 --clear-groups curl -m 2 http://169.254.169.254/latest/meta-data/` vs root `curl` | UID 61000 times out; root succeeds |
| Postgres peer auth isolation | `setpriv --reuid 61000 --regid 61000 --clear-groups psql "postgres://writ@/writ?host=/run/postgresql"` | Peer auth rejected |
| Real STS `plan_role` | Plan `backend "s3"` stack with sandbox IAM role | Plan passes; CloudTrail shows `RoleSessionName` and `SourceIdentity` |
| Missing `sts:SetSourceIdentity` | Remove from trust policy and plan | Fails `denied` naming the role |

## The website

`website/` renders directly from `docs/` via `website/content.manifest.ts`; `just website` fails if any selector breaks ([`website/README.md`](../website/README.md)).
