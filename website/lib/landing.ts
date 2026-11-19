// The landing page's copy, in one place for review. Strings take the inline
// Markdown that components/inline.tsx renders: `code`, **bold**, and links.
// Every terminal output, HCL snippet, and the list of limits come from
// content/landing.json instead, which scripts/gen-content.ts cuts from the
// README and the docs.

export const hero = {
  // The headline, with the phrase the page sets in color.
  title: { plain: 'Plan, approve, and apply infra changes.', accent: 'The cloud keys stay home.' },
  art: 'An island drawn in small cubes, seen from above. A stone keep stands on it, with ochre in its court and a tall tower that bears a key, and ochre cubes rise from the tower to a cloud. Two small huts on the shore face its gate. Part of the land at the lower left is drawn as a wire frame.',
  lede: 'Writ is a self-hosted, open-source control plane for OpenTofu. CI jobs prove identity with OIDC and hold no cloud credentials. Writ plans commits on your server, waits for your team to approve that exact sealed plan, applies it in a sandbox, and records every step in an audit log.',
  facts: [
    { label: 'License', text: 'Apache 2.0' },
    { label: 'Deployment', text: 'Self-hosted, single Go binary' },
    { label: 'Price', text: 'Free, no seat, run, or resource meter' },
    { label: 'Status', text: 'Version 0, OpenTofu 1.13.9 only.' },
  ],
  notYet: { text: 'What Writ isn’t, yet', href: '#notyet' },
};

export const what = {
  title: 'Cloud credentials live on one server, not in every pipeline',
  lede: 'When CI runners execute `tofu apply`, every job holds keys that can alter production. With Writ, only the server you operate holds cloud roles and state keys. People and CI jobs authenticate and request work; Writ plans, gates approvals, applies, and audits.',
  diagram: {
    left: [
      { name: 'Your Git host', text: 'Commits of your OpenTofu stacks, fetched by Git blob ID' },
      { name: 'You and your CI job', text: 'Authenticate via OIDC or session; hold no cloud keys' },
    ],
    center: {
      name: 'Writ, on your server',
      items: [
        'Runs OpenTofu in per-run UID namespaces',
        'Seals plans and binds approvals to one plan ID',
        'Enforces grants, approval rules, and Rego policy',
        'Masks secrets, checks drift, and writes an audit log',
      ],
    },
    right: {
      name: 'Your cloud',
      text: 'Writ assumes your read-only `plan_role` for plans, and your `apply_role` only for an approved plan or a recorded override.',
    },
    alt: 'Your Git host, you, and your CI job talk to Writ on your server. Only Writ talks to your cloud.',
  },
  without: 'What breaks when CI runs OpenTofu directly',
  problems: [
    {
      title: 'CI runners hold cloud credentials',
      text: 'A pipeline job that runs `tofu` carries production credentials and state access. A merge request can alter what that job runs.',
    },
    {
      title: 'Applies differ from reviewed plans',
      text: 'A merge request approval approves code, not the exact plan that runs after merge. State or branches can move in between.',
    },
    {
      title: 'No reliable audit log',
      text: 'CI logs scatter across jobs and expire, and raw `tofu` output prints secrets into logs anyone with repo access can read.',
    },
    {
      title: 'Drift goes unnoticed',
      text: 'Console edits and out-of-band applies accumulate silently in cloud accounts until the next deploy collides with them.',
    },
  ],
} as const;

export const how = {
  title: 'How a change gets applied',
  lede: 'Every change moves through one verifiable chain on your server. Approvals and grants live in server config, so a merge request cannot weaken its own gate.',
  rail: [
    { step: '01', name: 'MR', detail: 'Push OpenTofu changes' },
    { step: '02', name: 'Plan in Writ', detail: 'CI sends commit via OIDC' },
    { step: '03', name: 'Review & approve', detail: 'Teammate approves plan ID' },
    { step: '04', name: 'Merge', detail: 'Land commit on default branch' },
    { step: '05', name: 'Apply', detail: 'Writ applies exact sealed plan' },
    { step: '06', name: 'Audit', detail: 'Every step recorded in Postgres' },
  ],
  steps: [
    {
      name: 'Plan',
      idle: { tone: 'idle', text: 'waiting for a commit' },
      live: { tone: 'info', text: 'plan 927b69ec' },
      text: 'Your CI job or `writ plan --remote` sends a commit SHA. Writ fetches the commit from GitLab, runs `tofu plan -lock=false` in a sandbox, masks secrets, and seals the saved plan.',
      output: '1. The masked plan your teammate reads',
    },
    {
      name: 'Approve',
      idle: { tone: 'warn', text: 'needs 1 approval' },
      live: { tone: 'ok', text: '1 of 1, by bob' },
      text: 'A reviewer inspects the masked plan in the CLI or control UI and approves its SHA-256 plan ID. Where an approval rule applies, the author’s approval never counts.',
      output: '2. alice made the plan, so only bob can approve it',
    },
    {
      name: 'Apply',
      idle: { tone: 'idle', text: 'waits for approval' },
      live: { tone: 'ok', text: 'applied' },
      text: 'After merge, Writ matches the merged commit’s sources to the approved plan, re-runs Rego policy, assumes `apply_role`, and applies the exact sealed plan.',
      output: '3. The apply of the plan bob approved',
    },
  ],
  refusal: 'If a newer plan replaced the approved one, the state moved, or the branch changed, Writ refuses the apply. You plan and approve again.',
  caption:
    'Output of `TestTofuApproveApply`, 2026-11-16. Prompts `alice $` and `bob $` show who ran each command. [How a plan becomes an apply](/docs/plan-to-apply)',
} as const;

export const tour = {
  title: 'Calm control UI built for daily review',
  lede: 'The control UI ships inside the `writ serve` binary with no external assets. Review masked plans, inspect categorized stack resources, track drift, and approve changes in light or dark mode.',
  tabs: [
    {
      id: 'overview',
      label: 'Overview',
      caption: 'Plans waiting for approval, active applies, recent failures, drift alerts, and stacks with state across environments.',
      alt: 'Writ control UI overview showing stat cards for plans waiting for approval, applying, failed, and drifted stacks, alerts, queues, and stacks with state.',
    },
    {
      id: 'plan',
      label: 'Plan review',
      caption: 'Plan stations, approval status, Approve button, risk chips, attribute diffs, and sensitive values shown as (sensitive).',
      alt: 'Writ control UI plan review page showing Plan, Approve, and Apply stations, Approve this plan button, and masked attribute changes.',
    },
    {
      id: 'resources',
      label: 'Resources',
      caption: 'The state index for a stack, grouped by service category (Networking, IAM, Compute, Nomad, Storage, DNS) or by module, with search.',
      alt: 'Writ control UI stack Resources view showing 63 resources grouped into category cards for Networking, IAM, Compute, Nomad, Storage, DNS, Certificates, and Other.',
    },
    {
      id: 'stack',
      label: 'Drift',
      caption: 'A stack’s latest drift check, separating changes made outside OpenTofu from changes in your default branch code.',
      alt: 'Writ control UI stack Drift view showing outside changes and code changes for each affected resource address.',
    },
    {
      id: 'runs',
      label: 'Runs',
      caption: 'Every apply Writ started, linked to its plan ID, stack, actor, status, and duration.',
      alt: 'Writ control UI Runs list showing applying, applied, and failed runs with their stacks, plans, and actors.',
    },
  ],
  limit: 'You can inspect plans, resources, drift, and runs and approve plans in the control UI. To trigger an apply, use `writ apply --remote` or your CI job.',
} as const;

export const benefits = {
  title: 'Built to keep keys, secrets, and policy under control',
  keys: {
    title: 'CI holds no cloud credentials',
    text: 'CI jobs authenticate with OIDC tokens and never touch cloud keys or state keys. Writ assumes `plan_role` for plans and `apply_role` only for approved applies or recorded overrides, tagging every STS session for CloudTrail.',
    limit: 'Built-in role assumption supports AWS STS roles today; other providers use `provider_env` secret files on the server.',
    link: { text: 'AWS roles and credentials', href: '/docs/aws-credentials' },
    holds: [
      { who: 'Your laptop', creds: 'none, only a session', state: 'no state key', yes: false },
      { who: 'Your CI job', creds: 'none, only its OIDC token', state: 'no state key', yes: false },
      { who: 'Writ, for a plan', creds: 'plan role', state: 'reads only', yes: true },
      { who: 'Writ, for an apply', creds: 'apply role', state: 'locks and writes', yes: true },
    ],
  },
  config: {
    title: 'Approvals and grants live in server config, not CI YAML',
    text: 'Teams, grants, approval rules, policies, and environment IAM roles live in one HCL config on the server. A merge request cannot edit `.gitlab-ci.yml` to bypass reviewers or swap roles.',
    limit: 'Config changes take effect after `writ serve --check` and a server restart.',
    link: { text: 'Server config reference', href: '/docs/config' },
    caption: 'Excerpt from `docs/quick-start.md` and `docs/aws-credentials.md`. Checked with `writ serve --check`.',
  },
  secrets: {
    title: 'Secrets masked from every log and record',
    text: 'One mask covers plans, apply logs, diagnostics, and the state index. Sensitive values, `provider_env` secrets, and assumed STS keys become `(sensitive)`, including URL, JSON, hex, and base64 encodings.',
    limit: 'The mask is an output filter, not taint tracking. The docs list every edge case it cannot catch.',
    link: { text: 'Handle secrets and masking', href: '/docs/secrets' },
    alt: 'A replaced resource in the Writ control UI whose keepers show {"secret":"(sensitive)"} before and after, marked sensitive value changes.',
  },
  drift: {
    title: 'Scheduled drift checks',
    text: 'Add a `drift { every = "6h" }` block and Writ plans each stack on a timer, separating out-of-band cloud changes from unapplied code without storing secret values.',
    limit: 'Drift appears in the control UI, `GET /v1/drift`, and server logs. Push notifications are not built yet.',
    link: { text: 'Enable drift checks', href: '/docs/drift-checks' },
    alt: 'A stack drift table in the Writ control UI separating changes outside OpenTofu from code changes.',
  },
  audit: {
    title: 'Synchronous audit log in Postgres',
    text: 'Every API, control UI, and state-backend request commits an audit row before Writ sends a response; if the database cannot commit the row, the request fails with 503. Query it with `writ audit --remote` or `GET /v1/audit`.',
    limit: 'No audit page in the control UI yet. Refusal reasons are withheld from the API and kept in Postgres.',
    link: { text: 'Read the audit log', href: '/docs/audit-log' },
    caption: 'Output of `writ audit --remote` under `writ serve --dev`, 2026-11-19.',
  },
  policy: {
    title: 'Rego policy checked at plan and apply',
    text: 'Policies run in an isolated child process with a strict built-in allow-list and match `conftest` verdicts locally. Writ checks every plan at creation and again before apply; nothing overrides a mandatory deny.',
    limit: 'Test rules locally with `conftest verify` and `conftest test`; there is no `writ policy test` yet.',
    link: { text: 'Write and test policies', href: '/docs/write-a-policy' },
    caption: 'Output of `TestTofuPlanByCommit`, 2026-11-15, with lines cut.',
  },
  also: [
    {
      title: 'Sandboxed runs and egress controls',
      text: 'On Linux, each `tofu` run gets its own `provider_uids` UID, user/mount/PID namespaces, a read-only filesystem view, an `nftables` EC2 metadata block, and a strict module-source allow-list. [Run isolation and egress](/docs/run-isolation)',
    },
    {
      title: 'Exact sealed plan applies',
      text: 'Saved plans are sealed with AES-256-GCM per environment and deleted once no longer live. Stale plans and moved branches are refused. [How a plan becomes an apply](/docs/plan-to-apply)',
    },
    {
      title: 'Coding agents plan without keys',
      text: '`writ mcp` lets coding agents plan commits and inspect masked plans, drift, and policies, and never approve or apply. [Use Writ with a coding agent](/docs/agents)',
    },
    {
      title: 'Keep S3 state or import existing infra',
      text: 'Use Writ’s built-in sealed `http` backend or keep `backend "s3"`, and generate import blocks with `writ import bulk`. [Migrate from Terraform](/docs/migrate-from-terraform)',
    },
  ],
} as const;

export const whenToUse = {
  title: 'When to use Writ, and when plain CI is enough',
  lede: 'Running a control plane adds Postgres, a Linux host, and state keys to back up. Make sure the trade-off fits your team.',
  plainCi: {
    title: 'Plain CI + OIDC may be enough when',
    items: [
      'One small team owns the repo and runners, and everyone who can merge is already trusted to change production.',
      'You prefer zero extra server infrastructure beyond GitLab runners and your existing state bucket.',
      'You need GitHub Actions, Terraform (not OpenTofu), or merge-request plan comments right now.',
    ],
  },
  writ: {
    title: 'Reach for Writ when',
    items: [
      'CI pipelines and developer laptops must not hold cloud credentials or state encryption keys.',
      'Approvals must bind to the exact saved plan that applies, and a plan’s author must never self-approve.',
      'You want masked plans, Rego policy at plan and apply, scheduled drift checks, and a synchronous audit log without per-resource SaaS billing.',
    ],
  },
} as const;

export const compare = {
  title: 'Writ, Spacelift, or HCP Terraform?',
  lede: 'Hosted control planes meter seats, runs, or managed resources. Writ runs as one Apache 2.0 binary on your own server with nothing metered.',
  columns: ['Spacelift', 'HCP Terraform', 'Writ'],
  rows: [
    ['Source and hosting', 'Closed source, as SaaS. Self-hosted with Enterprise', 'Closed source, as SaaS. Terraform Enterprise is self-managed', 'Apache 2.0, on your own host. No hosted service'],
    ['Price', 'Free for 2 users. Starter starts at $399/mo', 'Free to 500 managed resources, then from $0.10/resource/mo', 'Free, with no limit on users, runs, or resources'],
    ['OpenTofu', 'Yes', 'Runs Terraform; docs do not mention OpenTofu', 'OpenTofu 1.13.9 only, no Terraform'],
    ['Policy', 'OPA, from Starter plan', 'Sentinel and OPA in Standard and Premium', 'OPA Rego, at plan and again before apply'],
    ['Drift checks', 'Business plan and up', 'Standard and Premium', 'Scheduled drift checks in UI and API; no push alerts yet'],
    ['Audit log', 'Enterprise plan', 'Premium plan', 'Synchronous row before each response in Postgres; read via `GET /v1/audit` or `writ audit --remote`'],
  ],
  caption:
    'Facts about Spacelift and HCP Terraform come from their pricing and docs pages, read 2026-11-16. [The comparison page](/docs/compare) cites every source and also covers Atlantis, OTF, Terrakube, env zero, and Scalr.',
  missing:
    'They offer what Writ doesn’t: a hosted service, commercial support, Terraform, GitHub, and notifications. If you need those today, pick one of them.',
  gitlab: { text: 'Running `tofu` in GitLab CI today? Read Writ vs plain GitLab CI', href: '/docs/compare-gitlab-ci' },
} as const;

export const trust = {
  title: 'What you can check yourself',
  items: [
    {
      title: 'The code.',
      text: 'Apache 2.0, single Go binary, records in your Postgres, import boundaries enforced by `internal/archtest`.',
      link: { text: 'The repository', href: 'https://github.com/mr-karan/writ' },
    },
    {
      title: 'The tests.',
      text: 'End-to-end tests run `writ` against real OpenTofu 1.13.9 and real providers in network namespaces, plus differential secret-mask verification.',
      link: { text: 'How it’s tested', href: '/docs/status#how-its-tested' },
    },
    {
      title: 'The audit log.',
      text: 'Every response waits for its Postgres audit row (except health checks, server discovery, and rate-limited 429s).',
      link: { text: 'Read the audit log', href: '/docs/audit-log' },
    },
  ],
  notYet: { title: 'What Writ isn’t, yet', link: { text: 'Status and limits', href: '/docs/status' } },
} as const;

export const tryIt = {
  title: 'Try it locally in four steps',
  lede: '`writ serve --dev` runs the same control plane on loopback with SQLite and a built-in UI, with no cloud account needed.',
  needs: [
    'No cloud account required; uses `hashicorp/random`.',
    'Needs Linux, `minisign`, Git, and OpenTofu 1.13.9.',
    'The install script checks the minisign signature and SHA-256. [Install](/docs/install) has container, Nix, and source builds.',
  ],
  build: 'Install Writ and start the dev control plane',
  run: 'In a second terminal, plan a commit and apply the plan',
  caption: 'Output of a real run, 2026-11-16, with some lines cut. Follow the [quick start](/docs/quick-start) for the full stack and every line.',
};

export const questions: { q: string; a: string }[] = [
  { q: 'Does Writ run Terraform?', a: 'No. Writ runs OpenTofu 1.13.9 only. If you are moving from Terraform, [Migrate from Terraform](/docs/migrate-from-terraform) shows how Writ handles existing lock files and state.' },
  {
    q: 'Does it work with GitHub?',
    a: 'Not yet. GitLab is the only Git host Writ fetches from today, and the CI components target GitLab CI. Nobody has run those jobs on a real GitLab instance yet.',
  },
  {
    q: 'Should I move production to it?',
    a: 'Not yet. Writ is at version 0, and end-to-end tests run against local stand-ins rather than live cloud accounts. Read [Status and limits](/docs/status) first.',
  },
  {
    q: 'How are runs isolated on the server?',
    a: 'On Linux, each `tofu` run executes under a dedicated `provider_uids` UID inside private user, mount, and PID namespaces with a read-only filesystem view, an `nftables` block on EC2 metadata, and a strict module-source allow-list. See [Run isolation and egress](/docs/run-isolation).',
  },
  {
    q: 'Where does my state live?',
    a: 'New stacks can use `writ serve`’s built-in `http` backend (AES-256-GCM sealed and versioned in Postgres). Existing stacks on `backend "s3"` can stay in their S3 bucket with separate read-only plan and write-capable apply IAM roles.',
  },
  {
    q: 'How does it compare with Atlantis, Spacelift, and HCP Terraform?',
    a: '[Writ vs other control planes](/docs/compare) compares Atlantis, OTF, Terrakube, Spacelift, HCP Terraform, env zero, and Scalr with sources and dates, and [Writ vs plain GitLab CI](/docs/compare-gitlab-ci) covers plain CI.',
  },
];
