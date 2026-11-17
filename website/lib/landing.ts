// The landing page's copy, in one place for review. Strings take the inline
// Markdown that components/inline.tsx renders: `code`, **bold**, and links.
// Every terminal output and the list of limits come from content/landing.json
// instead, which scripts/gen-content.ts cuts from the README and the docs.

export const hero = {
  title: 'Plan, approve, and apply OpenTofu on your own server.',
  lede: 'Writ is an open-source control plane for OpenTofu. It plans the commits you or your CI job send it, waits for your team to approve, and applies the plan they approved. Your cloud keys stay on its server, and your CI jobs and laptops hold none.',
  facts: [
    { label: 'License', text: 'Apache 2.0' },
    { label: 'Runs on', text: 'Your own server, as one Go binary' },
    { label: 'Price', text: 'Free, with no limit on users, runs, or resources' },
    { label: 'Status', text: 'Version 0, and OpenTofu 1.13.9 only.' },
  ],
  notYet: { text: 'What Writ isn’t, yet', href: '#notyet' },
};

export const what = {
  title: 'Production keys live on one server, not in every pipeline',
  lede: 'Today every CI job and laptop that runs `tofu apply` holds keys that can change production. With Writ, only the server you host holds them. Your team and your CI job sign in and ask. Writ plans, waits for approval, and applies.',
  diagram: {
    left: [
      { name: 'Your Git host', text: 'Commits of your OpenTofu stacks' },
      { name: 'You and your CI job', text: 'Ask for a plan, approve it, ask for the apply' },
    ],
    center: {
      name: 'Writ, on your server',
      items: ['Plans the commits you send it', 'Takes approvals for one exact plan', 'Checks policy and drift', 'Keeps the state keys and the cloud roles'],
    },
    right: {
      name: 'Your cloud',
      text: 'Writ plans with the plan role you configure, so make it read-only. It takes the apply role only for a plan that’s approved, or one overridden by someone you allow.',
    },
    alt: 'Your Git host, you, and your CI job talk to Writ on your server. Only Writ talks to your cloud.',
  },
  without: 'What goes wrong without one',
  cards: [
    { title: 'You review code, not a plan', text: 'A merge request approval says the code can merge. It doesn’t tie the apply to the plan you read.' },
    {
      title: 'The job holds the keys',
      text: 'In plain CI, the job that runs `tofu` holds the cloud credentials, the state, and the plan. A merge request can change what that job runs.',
    },
    { title: 'Plans print secrets', text: '`tofu plan` and `tofu apply` print secrets. A log that keeps their output shows them to everyone who can read it.' },
  ],
} as const;

export const how = {
  title: 'How a change gets applied',
  lede: 'Writ runs OpenTofu on a server you host. From your laptop or your CI job, you send it requests: plan this commit, approve this plan, apply it.',
  steps: [
    {
      name: 'Plan',
      idle: { tone: 'idle', text: 'waiting for a commit' },
      live: { tone: 'info', text: 'plan 927b69ec' },
      text: 'Ask for a commit’s plan with `writ plan --remote`, or from your CI job. Writ fetches the commit from your Git host and runs `tofu plan` on your server. A plan writes nothing to your state.',
      output: '1. The plan your teammate reads',
    },
    {
      name: 'Approve',
      idle: { tone: 'warn', text: 'needs 1 approval' },
      live: { tone: 'ok', text: '1 of 1, by bob' },
      text: 'A teammate reads the plan, with its secrets hidden, and approves it by its id. Where an approval rule applies, the author’s own approval doesn’t count.',
      output: '2. alice made the plan, so only bob can approve it',
    },
    {
      name: 'Apply',
      idle: { tone: 'idle', text: 'waits for approval' },
      live: { tone: 'ok', text: 'applied' },
      text: 'Writ applies the plan it kept, not a new one. Only this step gets the apply role.',
      output: '3. The apply of the plan bob approved',
    },
  ],
  refusal: 'If a newer plan replaced the approved one, or the state moved, Writ refuses the apply. You plan and approve again.',
  caption:
    'Output of `TestTofuApproveApply`, 2026-11-16. The prompts `alice $` and `bob $` are added to show who ran each command. [How approvals work](/docs/approvals)',
} as const;

export const tour = {
  title: 'See what’s waiting, what ran, and what drifted',
  lede: 'The control UI comes with `writ serve`. It shows the same plans your terminal does.',
  tabs: [
    {
      id: 'overview',
      label: 'Overview',
      caption: 'Plans waiting for approval, applies in progress, failed runs, and drifted stacks, for each environment.',
      alt: 'The control UI’s overview: stat cards for plans waiting for approval, applies, failed runs, and drift, two alerts, and the plans waiting for approval.',
    },
    {
      id: 'plan',
      label: 'A plan',
      caption: 'One plan: who made it, how many approvals it needs, and every change. A secret shows as (sensitive).',
      alt: 'The control UI’s page for one plan, as a reviewer sees it: pending, 0 of 1 approvals, an Approve this plan button, and a summary of the changes.',
    },
    {
      id: 'stack',
      label: 'Drift',
      caption: 'A stack’s drift check: what changed outside OpenTofu, and what the code would change.',
      alt: 'The control UI’s page for the stack app in dev: drifted, with one change outside OpenTofu and five that the code would change.',
    },
    {
      id: 'runs',
      label: 'Runs',
      caption: 'Every apply Writ started, with its plan and who asked for it.',
      alt: 'The control UI’s list of runs: one applying, one applied, and one failed, each with its stack, plan, and who started it.',
    },
  ],
  limit: 'You can approve a plan in the control UI. To apply one, use `writ apply --remote` or your CI job.',
} as const;

export const benefits = {
  title: 'What changes when Writ runs OpenTofu',
  keys: {
    title: 'No cloud keys in CI or on laptops',
    text: 'Your CI job sends a commit and holds no credentials. Writ takes the plan role you configure for a plan, so make it read-only. Only an apply gets the apply role: an approved one, or one overridden by someone you allow, with the reason recorded.',
    limit: 'Roles are AWS roles today.',
    link: { text: 'Roles and credentials', href: '/docs/serve#roles-and-credentials' },
    holds: [
      { who: 'Your laptop', creds: 'none, only a session', state: 'no state key', yes: false },
      { who: 'Your CI job', creds: 'none, only its OIDC token', state: 'no state key', yes: false },
      { who: 'Writ, for a plan', creds: 'plan role', state: 'reads only', yes: true },
      { who: 'Writ, for an apply', creds: 'apply role', state: 'locks and writes', yes: true },
    ],
  },
  secrets: {
    title: 'Secrets stay out of what Writ stores and shows',
    text: 'One mask covers the plan you read, the apply’s output, and the errors. You see that a secret changed without seeing it.',
    limit: 'The mask is a filter. The docs list the cases it can’t catch.',
    link: { text: 'Secrets and the mask', href: '/docs/secrets' },
    alt: 'Part of a plan in the control UI: a replaced resource whose keepers show {"secret":"(sensitive)"} before and after, marked sensitive value changes.',
  },
  drift: {
    title: 'Drift checks on a schedule',
    text: 'Writ plans each stack on a timer. It marks what changed outside OpenTofu, and what your code would change.',
    limit: 'You see drift in the control UI and the API. Writ doesn’t notify anyone yet.',
    link: { text: 'Drift checks', href: '/docs/serve#drift-checks' },
    alt: 'A stack’s drift check in the control UI: drifted, with one change outside OpenTofu and five that the code would change, each with its address and action.',
  },
  policy: {
    title: 'Policy checks every plan, twice',
    text: 'You write rules in Rego. Writ checks each plan when it’s created and again before the apply. No approval gets past a mandatory deny.',
    limit: 'There’s no `writ policy test` yet. Use `conftest`.',
    link: { text: 'Policy', href: '/docs/policy' },
    caption: 'Output of `TestTofuPlanByCommit`, 2026-11-15, with lines cut.',
  },
  agents: {
    title: 'Your coding agent plans, and only people approve',
    text: 'With `writ mcp`, your coding agent plans a commit and reads the plan and its policy verdict. It never holds a cloud credential, and it can’t approve or apply.',
    link: { text: 'Use Writ with an agent', href: '/docs/agents' },
    caption: 'Output of Claude Code 2.1.318, 2026-11-10.',
  },
  also: [
    {
      title: 'The approved plan is exactly what applies',
      text: 'An approval is for one plan id, and the apply runs the plan Writ kept. [Approvals](/docs/approvals)',
    },
    {
      title: 'Sign in with your identity provider',
      text: '`writ login` signs you in from a terminal, by device code or through a browser. [Sign in](/docs/sign-in)',
    },
    {
      title: 'Bring what you already run',
      text: '`writ import bulk` writes resource and import blocks for cloud objects you already have. [Import many objects](/docs/import-bulk)',
    },
    { title: 'Your state can stay where it is', text: 'A stack on `backend "s3"` keeps its bucket. [Stacks on S3](/docs/serve#stacks-on-s3)' },
  ],
} as const;

export const compare = {
  title: 'Writ, Spacelift, or HCP Terraform?',
  lede: 'The paid control planes do this job as a hosted service, priced by plan. Writ does it on your own host, with nothing metered.',
  columns: ['Spacelift', 'HCP Terraform', 'Writ'],
  rows: [
    ['Source and hosting', 'Closed source, as SaaS. Self-hosted with the Enterprise plan', 'Closed source, as SaaS. Terraform Enterprise is self-managed', 'Apache 2.0, on your own host. There’s no hosted service'],
    ['Price', 'Free for 2 users. Starter starts at $399 a month', 'Free to 500 managed resources, then from $0.10 a resource a month', 'Free, with no limit on users, runs, or resources'],
    ['OpenTofu', 'Yes', 'Runs Terraform. Its pages don’t mention OpenTofu', 'OpenTofu 1.13.9 only, and no Terraform'],
    ['Policy', 'OPA, from the Starter plan', 'Sentinel and OPA, in Standard and Premium. Free has one policy set of up to five policies', 'OPA, at the plan and again before the apply'],
    ['Drift checks', 'Business plan and up', 'Standard and Premium', 'On a schedule, with no alerts yet'],
    ['Audit log', 'Enterprise plan', 'Premium plan', 'A row before each answer, in your database, except health checks, server discovery, and rate-limited requests. Nothing reads it yet'],
  ],
  caption:
    'Facts about Spacelift and HCP Terraform come from their pricing and docs pages, read 2026-11-16. [The comparison page](/docs/compare) has each source, and adds Atlantis, OTF, Terrakube, env zero, and Scalr.',
  missing:
    'They have what Writ doesn’t: a hosted service, support, Terraform, GitHub, and notifications. If you need one of those today, choose one of them.',
  gitlab: { text: 'Running `tofu` from GitLab CI today? Read Writ compared with plain GitLab CI', href: '/docs/compare-gitlab-ci' },
} as const;

export const trust = {
  title: 'What you can check yourself',
  items: [
    {
      title: 'The code.',
      text: 'Apache 2.0, one Go binary, with its records in your Postgres.',
      link: { text: 'The repository', href: 'https://github.com/mr-karan/writ' },
    },
    {
      title: 'The tests.',
      text: 'The end-to-end tests run `writ` against OpenTofu 1.13.9 and real provider binaries. None of them reaches a real cloud.',
      link: { text: 'How it’s tested', href: '/docs/status#how-its-tested' },
    },
    {
      title: 'The audit log.',
      text: 'Writ writes a row before it answers a request, and the request fails if it can’t. Health checks, server discovery, and rate-limited requests leave no row.',
      link: { text: 'The audit log', href: '/docs/api#every-answer-waits-for-its-audit-row' },
    },
  ],
  notYet: { title: 'What Writ isn’t, yet', link: { text: 'Status and limits', href: '/docs/status' } },
} as const;

export const tryIt = {
  title: 'Run it on your machine',
  lede: '`writ serve --dev` runs a control plane of your own, on loopback, with its records in SQLite.',
  needs: [
    'You don’t need a cloud account.',
    'You need Linux, minisign, Git, and OpenTofu 1.13.9.',
    'The install script checks the release’s signature. [Install](/docs/install) has the other ways.',
  ],
  build: 'Install Writ and start the dev control plane',
  run: 'In a second terminal, plan a commit and apply the plan',
  caption: 'Output of a real run, 2026-11-16, with some lines cut. The [quick start](/docs/quick-start) has the stack and every line.',
};

export const questions: { q: string; a: string }[] = [
  { q: 'Does Writ run Terraform?', a: 'No. Writ runs OpenTofu 1.13.9 and no other version.' },
  {
    q: 'Does it work with GitHub?',
    a: 'Not yet. GitLab is the only Git host Writ reads, and its CI jobs are for GitLab CI. Nobody has run those jobs on a real GitLab yet.',
  },
  {
    q: 'Should I move production to it?',
    a: 'Not yet. Writ is at version 0, and no test has reached a real cloud account. [Status and limits](/docs/status) lists what’s built, how it’s tested, and what’s missing.',
  },
  {
    q: 'What does it cost?',
    a: 'Writ is under the Apache 2.0 license, with no limit on users, runs, or resources. You pay for the host and the Postgres you run it on.',
  },
  {
    q: 'Where does my state live?',
    a: 'A new stack can use the `http` backend that `writ serve` serves, where each state is encrypted with its environment’s key and versioned. A stack on `backend "s3"` keeps its bucket.',
  },
  {
    q: 'How does it compare with Atlantis and the paid control planes?',
    a: '[The comparison page](/docs/compare) covers Atlantis, OTF, Terrakube, Spacelift, HCP Terraform, env zero, and Scalr, each with its source and date, and says what those tools have that Writ doesn’t.',
  },
];
