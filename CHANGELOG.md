# Changelog

Writ's notable changes, newest first. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow [Semantic Versioning](https://semver.org/). Until 1.0, a minor version may change behavior.

## Unreleased

### Added

- `GET /v1/audit` and `writ audit --remote` read the audit log: newest first, by plan (with its approvals, run, and state writes), run, `stack@env`, person, operation, and time, in pages. A token sees the rows of the stacks it may read, and the rows of no stack only with `admin` on every stack in every environment. A filter of a stack the token can't read returns an empty page, the cursor is encrypted, and an approval across environments leaves one row for each. A refused row's reason, which can quote what its request sent, is withheld, a state write's reason is shown without the state's hash, and no row shows a client address or a nonstandard method.

### Changed

- The control UI is redesigned: a sidebar with stack counts, every page set in cards with labels in one column, and no lines that say what a button does. A stack's resources group by service, module, or none, with a strip of categories and counts, a filter on address, type, and ID that works with scripts off, and rows that open to show the full ID, ARN, and region. A plan keeps its approval, apply, and facts in one head card, with Action and Risk pickers over its changes. Access no longer shows your own grants; Who you are does.

### Fixed

- A provider error or resource address that quoted the AWS role credentials the credential endpoint served a run kept the secret access key and session token in the stored plan or apply, because the mask knew only the endpoint's token. Each run's mask now knows the secret of every set of credentials the endpoint assumed for the run, refreshes included, on plans, refused plans, applies, index refreshes, and adoptions on S3. Those credentials aren't sealed into a state's secret set.
- A refused plan on S3 read and opened every secret set of the `stack@env` before the mask checked its 4 MiB limit. It now reads them one at a time, counts each secret once, and fails the plan when they pass the limit.
- A plan of a stack on S3 that tofu refused, as for a permission the plan role lacks, kept only `tofu plan exited with code 1`. It now keeps each diagnostic's summary and place, swept by a mask that knows every secret set `writ serve` keeps for the stack, with each instance key quoted once or twice shown as `["(sensitive)"]`.
- A sign-in through the browser that began while a `writ login` device login waited for its token read the provider's client config as the device login wrote to it, a data race.
- The docs for the control UI's sign-in and the `people` issuer now say that `writ login` through a browser needs `http://127.0.0.1/callback` as a redirect URI.

## 0.1.0 - 2026-11-17

The first release. Writ is a control plane for OpenTofu that you host yourself. It runs OpenTofu 1.13.9, reads repositories from GitLab, and gets cloud credentials from AWS roles. The release has static binaries for Linux and macOS on amd64 and arm64, signed with minisign key `D2D9A37AF1098999`, and the image `ghcr.io/mr-karan/writ:0.1.0`. The macOS binaries have not yet run on a Mac.

### The control plane

- `writ serve` plans the OpenTofu stacks of a Git repository by commit, from GitLab or, under `writ serve --dev`, from a local repository. It keeps the masked plan to show and the saved plan to apply, sealed with AES-256-GCM under the environment's key.
- `writ serve --dev` runs the same control plane for one person on loopback, with SQLite.
- A team's `writ serve` keeps its records and the state it serves in Postgres, signs people in with OIDC through `writ login` or the control UI, signs CI jobs in with their OIDC tokens, and has teams, grants, an audit log, rate limits, and time limits.
- The `http` state backend, sealed and versioned, where a plan's token can only read. A stack can instead keep its own `backend "s3"`, with a plan role that cannot write the bucket and an apply role for approved applies, from the credential broker's AWS roles.
- The state index of what each stack manages, `writ state adopt` for a state that `writ serve` did not write, and drift checks on a schedule with a `drift` block.

### Approvals and applies

- An approval names one plan id, a SHA-256 over the masked plan, the saved plan, the commit, the stack, and the environment. The apply runs that sealed saved plan.
- Approval rules by stack, environment, and risk class. The author's approval does not count, and a CI job that a person started counts as that person.
- A plan that a newer plan replaced, or whose state moved, is refused. `--override` applies without approvals, with a recorded reason, for a person with `override`.
- `writ apply --remote --merged` applies, after a merge, the approved plan whose sources equal the merged commit's.
- A GitLab CI component that plans each merge request, blocks the merge until each plan has its approvals, and applies after the merge, and `writ ci gitlab` to print its lines. No real GitLab instance has run it yet.

### Secrets and isolation

- One mask, `internal/mask`, over everything `writ serve` stores or shows: OpenTofu's sensitive marks, a sweep by value in plain, URL, JSON, hex, and base64 forms, names such as `password`, and an allow-list of keys. `just verify-mask` tests it with two canary secrets.
- On a team's `writ serve`, each `tofu` process runs as its own uid in new user, mount, and pid namespaces, on a root that holds only its run's files, with no capabilities. Module sources pass an allow-list, and `init` gets no cloud credential.

### Policy

- Rego policy sets in the server's config judge each masked plan when it is made and again before its apply. A mandatory set's deny refuses the plan, and nothing overrides it. `just policy-parity` requires the same verdicts from `conftest`.

### Console to code

- `writ import bulk` writes `resource` and `import` blocks for existing objects, read through their provider, with references in place of literal ids and `for_each` for objects of one shape.
- `writ import accept` passes only a plan that imports exactly those objects and changes nothing else, apart from a named list of provider-only settings.

### Agents

- `writ mcp` serves Model Context Protocol tools that plan a commit and read plans, runs, drift, the state index, and the policy. No tool applies or approves.
- `writ agent init` installs the writ skill into a repository.

### Packaging

- `--json` on every command but `serve`, `mcp`, `ci`, and `help`, with one versioned document on stdout.
- Static binaries for Linux and macOS on amd64 and arm64, a `writ serve` container image with OpenTofu 1.13.9, an install script that checks a minisign signature, a Nix flake with a NixOS module, and reproducible builds. `just release-dry-run` builds them all, signed with a throwaway key.

