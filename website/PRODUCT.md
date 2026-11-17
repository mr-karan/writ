# Product

<!-- impeccable:product-schema 1 -->

The product record for the website: the landing page and the docs site. `DESIGN.md` has the visual system. `docs/style.md` has the voice and the terms, and it wins over any other guide for prose on a docs page.

## Platform

web

## Users

Platform and infrastructure engineers, and the teams they work in, who run OpenTofu today. They apply from CI jobs or laptops, or they're weighing a paid control plane such as Spacelift or HCP Terraform. They know OpenTofu, CI, and cloud roles well. They arrive from a link, a search, or the repository, and they're deciding whether to spend an hour trying Writ and whether to trust it with production later.

They read like reviewers. They look for who holds the credentials, what isn't built, and how it's tested, and they leave when a page sounds like an advert.

## Product Purpose

Writ is an open-source control plane for OpenTofu that you host yourself. You or your CI job send it a commit. It plans that commit on your server, waits for your team to approve the plan, and applies the plan they approved.

The website succeeds when a reader knows what Writ is from the first screen, and can start `writ serve --dev` on their own machine from the page or the quick start.

## Positioning

The cloud credentials and the state keys stay on one server you run. For plans and applies, CI jobs and laptops hold a session or an OIDC token, and no cloud credential. The one local exception is `writ import bulk` (and `writ mcp --allow-providers`), which reads existing objects through providers with the caller's own credentials. An approval is for one plan id, and the apply runs the plan Writ kept. It's one Go binary under Apache 2.0, with no limit on users, runs, or resources.

## Operating Context

- A reader tries Writ with `writ serve --dev`: loopback, SQLite, no cloud account. They need Linux, minisign, Git, and OpenTofu 1.13.9.
- A team runs `writ serve` with Postgres, sign-in through their identity provider, and approval rules. Requests come from `writ` on a laptop, from GitLab CI jobs, or from the control UI.
- The control UI comes with `writ serve`. You can approve a plan in it. You apply with `writ apply --remote` or the CI job.
- The docs are built from `docs/` in the repository. The site makes no request to another host and sets no cookie.

## Capabilities and Constraints

What it does: plans on request, approvals for one plan id, a plan role and an apply role, a mask for secrets, policy in Rego at the plan and again before the apply, drift checks on a schedule, an audit log, `writ import bulk`, and `writ mcp` for coding agents.

Limits the site must keep stating:

- A plan starts when you or your CI job ask for it, not on every commit.
- The apply role is used only for an approved plan, or for one overridden by someone with the override grant, with a recorded reason.
- The plan role is whatever the operator configures. They make it read-only.
- The audit log records every request except `GET /healthz`, `GET /v1/server`, and rate-limited requests.
- Version 0. OpenTofu 1.13.9 only, and no Terraform.
- GitLab is the only Git host. The roles are AWS roles. One node, on Linux. No notifications.
- No test has reached a real cloud account. The macOS binaries haven't run on a Mac.

`docs/status.md` is the full list. A claim on the site is true at the commit it ships from, and the site adds no claim the code can't back.

## Brand Commitments

- The name is Writ. The mark is a proofreader's caret, laid in ochre slabs on an indigo square.
- The voice is an engineer on the team explaining Writ to another engineer: plain, technical, direct, and honest about limits. No praise for Writ, no hype words, no em dashes, no exclamation marks.
- Every claim names its limit on the same page.
- Every output shown came from a real run, with a caption that says which. Every screenshot is a real capture of the control UI.
- No organization's details, internal hosts, or account ids appear anywhere.

## Evidence on Hand

- Terminal output cut from the README and the docs by `scripts/gen-content.ts` into `content/landing.json`. A fence that no longer matches fails the build.
- Screenshots of the control UI in `public/ui/`, shot by `just ui-shots`, in light and dark at 1280 and 390 px.
- `docs/compare.md`, with a source and a date for each fact about another tool.
- The end-to-end tests, described in `docs/status.md`.

There are no customers, testimonials, benchmarks against other control planes, or logos. Don't invent any.

## Product Principles

1. Say what it is, then what it isn't.
2. Show the real thing: output and screenshots, never a drawing of them.
3. The reader checks. Give them the source, the test, and the date.
4. The keys stay on the server. Every page that touches credentials says who holds what.
5. One path to try it, and it works without a cloud account.

## Accessibility & Inclusion

Text contrast is at least 4.5:1 in both themes. The page reads and its tabs work without script. Motion stops under `prefers-reduced-motion`. Keyboard focus is visible on every control.
