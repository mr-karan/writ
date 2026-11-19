# Security policy

Writ guards cloud credentials, state keys, and the secrets in OpenTofu plans, so security reports matter here. Thank you for taking the time to send one.

## Report a vulnerability

Report it privately through GitHub: [open a private security advisory](https://github.com/mr-karan/writ/security/advisories/new). Only the maintainers can read it.

Do not open a public issue, pull request, or discussion about it.

Please include:

- the version or commit of Writ, and whether it was `writ serve`, `writ serve --dev`, or the CLI,
- the steps or a small config and stack that show the problem,
- what an attacker gains, and what access they need first.

We reply in the advisory and work out the fix with you there. The published advisory credits you, unless you ask it not to.

## Supported versions

Writ is at version 0. Fixes go to `main` and into the next release. Older releases get no fixes.

## In scope

- `writ serve`: sign-in, tokens, teams and grants, approval rules, the binding of an approval to one plan id, overrides, policy, and the `http` state backend.
- The mask: a secret that reaches anything Writ stores or shows, in a case that [Secrets and the mask](docs/secrets.md) does not list as one the mask cannot catch.
- The run's view: a way for a stack's code, a module, or a provider to read the control plane's database, keys, or another run's files, or to get the apply role without an allowed apply.
- A run's egress under `egress_mode` `report` or `enforce`: a way for a run's code to connect outside its own network namespace, or through Writ's egress proxy to a loopback, link-local, or metadata address, to a private address through a wildcard, or under `enforce` to a destination its env's egress list doesn't allow.
- The CLI, `writ mcp`, the control UI, the install script, the release artifacts, the container image, and the NixOS module.

## Out of scope

- Bugs in OpenTofu or in a provider. Report them upstream.
- The limits that the docs state: the cases the mask cannot catch, which [Secrets and the mask](docs/secrets.md#what-the-mask-cant-do) lists, a provider that uses the open network with the role its run holds, under `egress_mode` `off` or through a host its env's egress list allows, as [Egress](docs/serve.md#what-egress-doesnt-cover) lists, and a `writ serve --dev` that someone shares, which its banner says not to do.
