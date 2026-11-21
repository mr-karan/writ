# Contributing to Writ

Thank you for looking at Writ. This page says how to build it, how to test a change, and how to send one. The [`justfile`](justfile) has every command in full, and [Architecture](docs/architecture.md) has the package map.

## Before you start

- For a bug, open an issue with the steps that show it. A small fix can come straight as a pull request.
- For a new feature or a change of behavior, open an issue first, so we can agree on the shape before you write code. [Supported platforms and limits](docs/status.md) and its [Not built yet](docs/status.md#not-built-yet) section list what is known to be missing.
- For a security problem, do not open an issue. Follow [SECURITY.md](SECURITY.md).

## Set up

You need Linux, Go 1.27.3 or later, [`just`](https://github.com/casey/just), Git, and OpenTofu 1.13.9. With Nix, `nix develop` in the repository has all of them.

The end-to-end tests also need Nix, which supplies OpenTofu and Postgres, plus `unshare` with subordinate uids and `ip`: they run in user, network, mount, and pid namespaces with only loopback.

```
git clone https://github.com/mr-karan/writ
cd writ
just build
bin/writ version
```

## Test a change

| Command | When to run it |
| --- | --- |
| `just check` | Before every commit. It runs gofmt, `go vet`, staticcheck, and `go test -race`, and checks that the Nix package's `vendorHash` matches the Go modules. |
| `just e2e` | For any change to `writ serve`, plans, applies, or the CLI. It runs real OpenTofu 1.13.9 with `hashicorp/random`, `local`, and `tls` through `writ serve --dev` and a team's `writ serve`. |
| `just verify-mask` | For any change to what a run stores or shows. |
| `just policy-parity` | For any change to policy. |
| `just website` | For any change to `docs/`, `README.md`, or `website/`. |
| `just ci` | Every local gate in order. |

Writ has no hosted CI yet, so `just ci` is the CI. Run `just --list` or read [`justfile`](justfile) for the other gates and what each needs.

## Rules for code

- **No real cloud in tests.** Tests use providers that need no cloud account: `hashicorp/random`, `hashicorp/tls`, and `hashicorp/local`. Code that builds requests to AWS or another cloud is tested against local stand-ins. Never run `apply` against a cloud provider from a test.
- **Go conventions.** `log/slog` text output to stderr, lowercase errors without trailing punctuation, table-driven tests with `t.Run`, and `CGO_ENABLED=0`.
- **Tests protect behavior.** Each test should fail on a credible regression of something a user can observe. Do not add test-only hooks to production code.
- **Comments say why.** Keep a comment only when it explains something the code cannot show.
- **Boundaries.** Wire types, such as protobuf messages, cty values, and SQL rows, stay inside the package that owns that boundary. `internal/archtest` enforces the import rules on every `just check`.
- **Dependencies.** Ask in an issue before you add one. Pin it to an exact version, and say why it was added in the commit body. After any change to `go.mod`, `go.sum`, or `flake.lock`, run `just update-nix-hash` and commit `deploy/nix/package.nix` and `deploy/nix/go-sum.sha256` with it.
- **Authentication uses established libraries,** never code written from a spec: `golang.org/x/oauth2`, `github.com/coreos/go-oidc/v3`, `github.com/go-webauthn/webauthn`, and `github.com/pquerna/otp`. Tokens are made and compared with `crypto/rand` and `crypto/subtle`.
- **Generated and copied files.** Do not edit `proto/tfplugin5.proto` or `proto/tfplugin6.proto`, which are copied from OpenTofu. Regenerate `internal/tfplugin5` and `internal/tfplugin6` with `just proto`.

## Rules for docs

Docs follow [Writing style](docs/style.md) and use the terms in the [Glossary](docs/glossary.md). Show only output that a real run printed, and say when it was printed. The website is built from `docs/` and `README.md`, and `just website` fails when a page it cuts no longer matches its source.

## Send a change

1. Fork the repository and make a branch from `main`.
2. Make small commits with [Conventional Commits](https://www.conventionalcommits.org/) messages, scoped by area, as in `fix(cli): ...` or `docs(serve): ...`.
3. Run `just check` and the gates your change touches, from the table above.
4. Open a pull request. Say what it changes for a user, how you tested it, and which gates you ran. The pull request template asks for these.

By sending a contribution, you agree that it is licensed under the [Apache 2.0 license](LICENSE), as section 5 of that license says.
