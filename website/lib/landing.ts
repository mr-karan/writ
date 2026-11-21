// Landing page copy, kept deliberately sparse so visuals lead every section.
// Code blocks and terminal outputs come from content/landing.json (generated
// from the README and docs by scripts/gen-content.ts).

export const hero = {
  title: {
    plain: 'The self-hosted control plane',
    accent: 'for OpenTofu',
  },
  subhead: 'Policy-checked, approved applies from your own server. CI sends a commit and never touches cloud credentials.',
  tabs: [
    {
      id: 'overview',
      label: 'Overview',
      alt: 'Writ control UI overview showing pending approvals, active applies, failed runs, drift alerts, and stacks with state.',
    },
    {
      id: 'plan',
      label: 'Plan',
      alt: 'Writ control UI plan review showing Plan, Approve, and Apply stations, Approve this plan button, and masked diffs.',
    },
    {
      id: 'resources',
      label: 'Resources',
      alt: 'Writ control UI stack Resources view grouping 63 resources by Networking, IAM, Compute, Nomad, Storage, and DNS.',
    },
    {
      id: 'stack',
      label: 'Drift',
      alt: 'Writ control UI stack Drift view separating outside cloud edits from unapplied code changes.',
    },
    {
      id: 'runs',
      label: 'Runs',
      alt: 'Writ control UI Runs list showing applying, applied, and failed runs linked to plans and actors.',
    },
  ],
  proof: [
    { label: 'Apache 2.0', detail: 'Open source' },
    { label: 'Self-hosted', detail: 'Single Go binary' },
    { label: 'OpenTofu 1.13.9', detail: 'Linux server' },
    { label: 'Unmetered', detail: 'No seat limits' },
  ],
} as const;

export const workflow = {
  title: 'Keep cloud keys off CI runners',
  note: 'Approvals bind to one saved plan; authors cannot self-approve.',
  link: { text: 'How it works', href: '/docs/plan-to-apply' },
  stages: [
    { id: 'push', label: 'Git push', zone: 'ci' },
    { id: 'plan', label: 'Plan', zone: 'server' },
    { id: 'policy', label: 'Policy', zone: 'server' },
    { id: 'approve', label: 'Approve', zone: 'server' },
    { id: 'apply', label: 'Apply', zone: 'server' },
    { id: 'audit', label: 'Audit', zone: 'server' },
  ],
  ciLabel: 'CI · OIDC only',
  serverLabel: 'writ serve · holds cloud roles & state keys',
  diagramAlt: 'Pipeline from Git push in CI using OIDC only to Plan, Policy, Approve, Apply, and Audit inside writ serve on your host.',
} as const;

export const features = {
  title: 'Isolation, policy, and visibility built in',
  items: [
    {
      id: 'keys',
      title: 'No CI cloud keys',
      text: 'CI uses OIDC; Writ assumes AWS STS roles.',
    },
    {
      id: 'policy',
      title: 'OPA Rego policy',
      text: 'Evaluates plans before approval and apply. Mandatory denials stand.',
    },
    {
      id: 'secrets',
      title: 'Masked secrets',
      text: 'Masks sensitive values in plans, logs, diagnostics, and state index.',
    },
    {
      id: 'drift',
      title: 'Scheduled drift checks',
      text: 'Flags outside edits separately from code via Slack or webhooks.',
    },
    {
      id: 'sandbox',
      title: 'Sandboxed Linux runs',
      text: 'Per-run UIDs, namespaces, opt-in egress proxy; cgroups and seccomp where supported.',
    },
    {
      id: 'mcp',
      title: 'MCP for agents',
      text: 'Agents plan commits and inspect masked diffs without apply rights.',
    },
  ],
} as const;

export const showcase = {
  review: {
    title: 'Secrets stay masked in every plan',
    text: 'Approve one saved plan ID in the UI or CLI while secrets stay hidden.',
    link: { text: 'Approvals', href: '/docs/approvals' },
    alt: 'A replaced resource in the Writ control UI whose keepers show {"secret":"(sensitive)"} before and after.',
  },
  policy: {
    title: 'Block unsafe plans with OPA Rego',
    text: 'Test Rego rules with conftest and block non-compliant plans before apply.',
    link: { text: 'Policy cookbook', href: '/docs/policy-cookbook' },
    regoTitle: 'policy/baseline/tls.rego',
    denyTitle: 'writ plan --remote (exit 1)',
  },
  drift: {
    title: 'Detect drift and log every request',
    text: 'Scheduled checks flag outside edits; audited API, UI, and state calls write to the database.',
    links: [
      { text: 'Drift checks', href: '/docs/drift-checks' },
      { text: 'Security model', href: '/docs/security-model' },
    ],
    alt: 'A stack drift table in the Writ control UI separating changes outside OpenTofu from code changes.',
  },
} as const;

export const tryIt = {
  title: 'Try Writ locally with no cloud account',
  text: 'Start one binary on loopback with SQLite and OpenTofu 1.13.9.',
  limit: 'Team server needs Linux, OpenTofu 1.13.9, and GitLab.',
  limitLink: { text: 'Limits', href: '/docs/status' },
  terminalTitle: 'Install and start writ serve --dev',
} as const;
