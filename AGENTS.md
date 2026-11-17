# Writ

Writ is a control plane that runs OpenTofu for many applications and teams. Read `docs/architecture.md` for the package map and why it is shaped this way.

## Layout

- `cmd/writ` holds the CLI. It parses flags and calls into `internal/`. It holds no logic of its own.
- `internal/` holds everything else. Read `docs/architecture.md` for the package map before you add a package.
- `proto/tfplugin5.proto` and `proto/tfplugin6.proto` are the Terraform plugin protocols, versions 5.10 and 6.10, copied from OpenTofu under MPL-2.0. Do not edit them. Regenerate the Go code in `internal/tfplugin5` and `internal/tfplugin6` with `just proto`.

## Commands

- `just check` runs gofmt, `go vet`, staticcheck, and `go test -race`. Run it before every commit.
- `just e2e` builds `writ` and runs the end-to-end tests with OpenTofu 1.13.9, Postgres, and the real `hashicorp/random`, `tls`, and `local` providers: plans by commit under `writ serve --dev`, and approve, apply, and drift under a team's `writ serve`. It runs in user, network, mount, and pid namespaces with only loopback, so it needs `unshare` with subordinate uids and `ip`.

## Rules

- Go conventions: `log/slog` text output to stderr, lowercase errors without trailing punctuation, table-driven tests with `t.Run`, `CGO_ENABLED=0`.
- Tests run against real providers that need no cloud account: `hashicorp/random`, `hashicorp/tls`, and `hashicorp/local`. Never run `apply` against a cloud provider from a test.
- No test, probe, or before-and-after check may reach a real cloud host. Code that builds requests to AWS or other cloud endpoints is tested against local stand-ins, and any run of such code outside the normal suites (a shim of old code, a scratch probe) runs inside `unshare -rn`, where only loopback exists. Twice a test reached real AWS endpoints because a stand-in was missed.
- Work only in your own worktree or a clone of your own. Never commit to `main`, never run `git config` on the shared repository, and never change the checkout at the repository root. A review agent once set `core.worktree` and committed scratch tests to `main`; a hook now refuses any update of `main` but the coordinator's.
- Wire types (protobuf messages, cty values, SQL rows) stay inside the package that owns that boundary. Other packages see Writ's own types.
- Pin every dependency to an exact version, and say why it was added in the commit body.
- Authentication protocols use established libraries, never code written from the spec: OAuth device flow and PKCE through `golang.org/x/oauth2`, OIDC token verification through `github.com/coreos/go-oidc/v3`, WebAuthn through `github.com/go-webauthn/webauthn`, TOTP through `github.com/pquerna/otp`. Token generation and comparison use `crypto/rand` and `crypto/subtle`.
- Documentation follows `docs/style.md`. Use the terms in `docs/glossary.md`, and show only output that a real run printed.
