// Landing page copy, kept deliberately sparse so visuals lead every section.
// Code blocks and terminal outputs come from content/landing.json (generated
// from the README and docs by scripts/gen-content.ts).

export const hero = {
  title: 'The self-hosted control plane for OpenTofu',
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
} as const;

export const workflow = {
  title: 'Keep cloud keys off CI',
  body: 'GitHub Actions sends a commit via OIDC. Writ plans, checks policy, and applies.',
  link: { text: 'How it works', href: '/docs/plan-to-apply' },
  stages: [
    { id: 'push', label: 'Pull request', zone: 'ci' },
    { id: 'plan', label: 'Plan', zone: 'server' },
    { id: 'policy', label: 'Policy', zone: 'server' },
    { id: 'approve', label: 'Approve', zone: 'server' },
    { id: 'apply', label: 'Apply', zone: 'server' },
    { id: 'audit', label: 'Audit', zone: 'server' },
  ],
  ciLabel: 'GitHub Actions · OIDC',
  ciAlt: 'Also GitLab CI',
  serverLabel: 'writ serve · cloud roles & state keys',
  diagramAlt: 'Pipeline from Pull request in GitHub Actions (or GitLab CI) using OIDC only to Plan, Policy, Approve, Apply, and Audit inside writ serve on your server.',
} as const;

export const features = {
  title: 'Built for isolation, policy, and audit',
  items: [
    {
      id: 'keys',
      title: 'No CI cloud keys',
      text: 'GitHub Actions uses OIDC; Writ assumes AWS roles.',
    },
    {
      id: 'policy',
      title: 'OPA Rego policy',
      text: 'Blocks unsafe plans before approval and apply.',
    },
    {
      id: 'secrets',
      title: 'Masked secrets',
      text: 'Strips secrets from plans, logs, and state index.',
    },
    {
      id: 'drift',
      title: 'Scheduled drift checks',
      text: 'Flags outside cloud edits via Slack or webhooks.',
    },
    {
      id: 'sandbox',
      title: 'Sandboxed Linux runs',
      text: 'Per-run UIDs, namespaces, cgroups, and egress proxy.',
    },
    {
      id: 'mcp',
      title: 'MCP for agents',
      text: 'Agents plan commits without cloud keys or apply.',
    },
  ],
} as const;

export const showcase = {
  review: {
    title: 'Secrets stay masked in every plan',
    text: 'Approve one saved plan ID while sensitive values stay hidden.',
    link: { text: 'Approvals', href: '/docs/approvals' },
    alt: 'A replaced resource in the Writ control UI whose keepers show {"secret":"(sensitive)"} before and after.',
  },
  policy: {
    title: 'Block unsafe plans with Rego',
    text: 'Test rules with conftest; mandatory denials cannot be overridden.',
    link: { text: 'Policy cookbook', href: '/docs/policy-cookbook' },
    regoTitle: 'policy/baseline/tls.rego',
    denyTitle: 'writ plan --remote (exit 1)',
  },
  drift: {
    title: 'Catch drift and trace every change',
    text: 'Scheduled checks flag outside edits; every change lands in the audit log.',
    links: [
      { text: 'Drift checks', href: '/docs/drift-checks' },
      { text: 'Security model', href: '/docs/security-model' },
    ],
    alt: 'A stack drift table in the Writ control UI separating changes outside OpenTofu from code changes.',
  },
} as const;

export const tryIt = {
  title: 'Try locally or connect GitHub Actions',
  text: 'Start one Go binary on loopback, or plan pull requests with OIDC.',
  primaryCta: { text: 'Quick start', href: '/docs/quick-start' },
  secondaryLink: { text: 'GitHub Actions guide', href: '/docs/set-up-github-actions' },
  limit: 'Linux and OpenTofu 1.13.9. GitHub (read token) from v0.1.9; GitLab also works.',
  limitLink: { text: 'Limits', href: '/docs/status' },
  tabs: {
    github: 'GitHub Actions',
    local: 'Local --dev',
  },
} as const;
