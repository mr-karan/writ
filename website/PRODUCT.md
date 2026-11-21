# Product

<!-- impeccable:product-schema 1 -->

The product record for the website: the landing page and the docs site. `DESIGN.md` has the visual system. `docs/style.md` has the voice and the terms, and it wins over any other guide for prose on a docs page.

## Platform

web

## Users

Platform and infrastructure engineers, and the teams they work in, who run OpenTofu today. They apply from CI jobs or laptops, or they are weighing a paid control plane such as Spacelift or HCP Terraform. They scan fast: they want to see the product UI, understand the trust boundary in seconds, verify how policy and isolation work, and jump straight into docs or `writ serve --dev`.

## Product Purpose

Writ is an open-source control plane for OpenTofu that you host yourself. You or your CI job send it a commit. It plans that commit on your server, enforces OPA Rego policy, waits for peer approval, and applies the approved saved plan.

The landing page succeeds when a visitor sees the real control UI immediately, understands the credential boundary at a glance, and can either try `writ serve --dev` locally or click into `/docs` for depth.

## Positioning

Cloud credentials and state keys stay on one server you run. For plans and applies, CI jobs and laptops authenticate with OIDC or a session and hold no cloud credentials. The local exceptions are `writ import bulk` and `writ mcp --allow-providers`, which read existing objects through providers with the caller's own credentials. Approvals bind to one saved plan ID, and apply runs that exact plan. Writ ships as one Apache 2.0 Go binary with no seat, run, or resource metering.

## Operating Context

- Local trial: `writ serve --dev` runs on loopback with SQLite and no cloud account (Linux, `minisign`, Git, and OpenTofu 1.13.9).
- Team server: `writ serve` runs on Linux with Postgres, OIDC sign-in, approval rules, and optional run isolation (`provider_uids`, Linux namespaces, `nftables` metadata block, opt-in egress proxy, and cgroups/seccomp where host supports them).
- Built-in control UI: served by `writ serve` under `/ui/` for reviewing masked plans, resources, drift, runs, and approvals. Applies run via `writ apply --remote` or CI.
- Docs site: built from `docs/` in the repository. No third-party requests, no analytics, no cookies.

## Capabilities and Constraints

What ships today: commit-based plans, plan-bound peer approvals, separate AWS STS plan/apply roles, secret masking across plans/logs/diagnostics/state index, OPA Rego policy at plan and apply, scheduled drift checks, Slack and signed webhook notifications, a synchronous database audit log, Linux run isolation with opt-in egress proxy, `writ import bulk`, and `writ mcp` for coding agents.

Key limits stated clearly (full list in `docs/status.md`):

- OpenTofu 1.13.9 only; no Terraform.
- Team `writ serve` requires Linux; single-node per Postgres database.
- GitLab is the built-in remote Git host; built-in STS role assumption targets AWS IAM roles.
- Egress control defaults to `off`; cgroup limits and seccomp apply where the host allows them (`writ serve --check` reports status).
- End-to-end suites test against local stand-ins in isolated namespaces rather than live cloud accounts.

## Brand Commitments

- Name: Writ. Mark: a proofreader's caret in ochre slabs on an indigo square.
- Voice: plain, technical, direct English. No hype filler, no em dashes, no exclamation marks.
- Sparse landing page: keep landing prose under 250 words and paragraphs under 25 words; link to `/docs` for architecture, security layers, comparison tables, and FAQ.
- Real evidence only: real control UI captures from `public/ui/` and real code/output from `content/landing.json`. No fake customer logos, testimonials, or user counts.
- No organization details, private hostnames, or real account IDs anywhere.

## Accessibility & Inclusion

Text contrast is at least 4.5:1 in both themes. The landing page reads cleanly and switches UI preview tabs without JavaScript. Motion stops under `prefers-reduced-motion`. Keyboard focus is visible on every interactive control.
