// The landing page's copy, in one place for review. Strings take the inline
// Markdown that components/inline.tsx renders: `code`, **bold**, and links.
// Every terminal output, HCL snippet, Rego snippet, and the list of limits
// come from content/landing.json instead, which scripts/gen-content.ts cuts
// from the README and the docs.

export const hero = {
  // The headline, with the phrase the page sets in color.
  title: { plain: 'Plan, approve, and apply infra changes.', accent: 'The cloud keys stay home.' },
  art: 'An island drawn in small cubes, seen from above. A stone keep stands on it, with ochre in its court and a tall tower that bears a key, and ochre cubes rise from the tower to a cloud. Two small huts on the shore face its gate. Part of the land at the lower left is drawn as a wire frame.',
  lede: 'Writ is a self-hosted, open-source control plane for OpenTofu. CI jobs authenticate with OIDC and hold no cloud credentials. Writ plans commits on your server, enforces OPA/Rego policy, waits for your team to approve that exact sealed plan, applies it in a sandbox, and writes every step to an audit log.',
  facts: [
    { label: 'License', text: 'Apache 2.0' },
    { label: 'Deployment', text: 'Self-hosted, single Go binary' },
    { label: 'Price', text: 'Free; no seat, run, or resource meter' },
    { label: 'Engine', text: 'OpenTofu 1.13.9, isolated per run.' },
  ],
  more: { text: 'Supported platforms', href: '/docs/status' },
};

export const what = {
  title: 'Cloud credentials live on one server, not in every pipeline',
  lede: 'When CI runners execute `tofu apply`, every job holds keys that can change production. With Writ, only the server you operate holds cloud roles and state keys. People and CI jobs authenticate and send a commit; Writ plans, checks policy, gates approvals, applies, and audits.',
  diagram: {
    left: [
      { name: 'Your Git host', text: 'Commits of your OpenTofu stacks, verified by Git blob ID' },
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
      text: 'Writ assumes `plan_role` for plans and `apply_role` only for approved plans or recorded overrides.',
    },
    alt: 'Your Git host, you, and your CI job talk to Writ on your server. Only Writ talks to your cloud.',
  },
  without: 'What breaks when CI runs OpenTofu directly',
  problems: [
    {
      title: 'CI runners hold cloud credentials',
      text: 'A runner executing `tofu` carries production credentials and state access. A merge request can alter what that job runs.',
    },
    {
      title: 'Applies drift from reviewed plans',
      text: 'Approving a merge request approves code, not the exact plan that runs after merge. State or branches can move in between.',
    },
    {
      title: 'Scattered logs that leak secrets',
      text: 'CI logs expire across jobs, and raw `tofu` output prints secrets into job logs anyone with repo access can read.',
    },
    {
      title: 'Drift goes unnoticed',
      text: 'Console edits and out-of-band applies accumulate silently until the next deploy collides with them.',
    },
  ],
} as const;

export const how = {
  title: 'How a change gets applied',
  lede: 'Every change moves through one chain on your server. Grants, approval rules, and policies live in host config, so a merge request cannot weaken its own gate.',
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
      text: 'CI or `writ plan --remote` sends a commit SHA. Writ fetches the commit from GitLab, runs `tofu plan -lock=false` in a sandbox, masks secrets, checks policy, and seals the saved plan.',
      output: '1. The masked plan your teammate reads',
    },
    {
      name: 'Approve',
      idle: { tone: 'warn', text: 'needs 1 approval' },
      live: { tone: 'ok', text: '1 of 1, by bob' },
      text: 'A reviewer inspects the masked plan in the CLI or control UI and approves its plan ID. Wherever an approval rule applies, the author’s approval never counts.',
      output: '2. alice made the plan, so only bob can approve it',
    },
    {
      name: 'Apply',
      idle: { tone: 'idle', text: 'waits for approval' },
      live: { tone: 'ok', text: 'applied' },
      text: 'After merge, Writ matches the merged commit’s sources to the approved plan, re-runs Rego policy, assumes `apply_role`, and applies that exact sealed plan.',
      output: '3. The apply of the plan bob approved',
    },
  ],
  refusal: 'If a newer plan replaced the approved one, state moved, or the branch changed, Writ refuses the apply. Plan and approve again.',
  caption:
    'Output of `TestTofuApproveApply`, 2026-11-16. Prompts `alice $` and `bob $` show who ran each command. [How a plan becomes an apply](/docs/plan-to-apply)',
} as const;

export const security = {
  title: 'Thirteen layers of defense around every run',
  lede: 'Because planning an unmerged branch executes provider and module code, Writ isolates credentials, state keys, processes, secrets, and approvals on the server.',
  layers: [
    {
      num: '01',
      title: 'No cloud credentials in CI',
      text: 'CI jobs authenticate with single-use OIDC JWTs (`WRIT_ID_TOKEN`); AWS credentials and state keys never leave `writ serve`.',
    },
    {
      num: '02',
      title: 'Short-lived STS sessions per run',
      text: 'Each run receives a dedicated loopback STS session for `plan_role` or `apply_role`, tagged with `SourceIdentity` for CloudTrail.',
    },
    {
      num: '03',
      title: 'Dedicated provider UIDs and metadata firewall',
      text: 'Every concurrent run executes under its own Linux UID from `provider_uids`, blocked by `nftables` from `169.254.169.254` and `fd00:ec2::254`.',
    },
    {
      num: '04',
      title: 'Linux user, mount, PID, and IPC namespaces',
      text: 'Runs execute inside a `pivot_root` read-only `tmpfs` view with `no_new_privs`, empty capabilities, and no server keys or shells mounted.',
    },
    {
      num: '05',
      title: 'Secrets masked from every output and record',
      text: '`internal/mask` strips sensitive values, secret-named attributes, and per-run STS credentials from plans, diagnostics, logs, and the state index.',
    },
    {
      num: '06',
      title: 'Sealed, immutable saved plans',
      text: 'Saved plans and `http`-backend states are sealed per environment with AES-256-GCM; apply runs the exact approved binary and refuses stale or expired plans.',
    },
    {
      num: '07',
      title: 'Server-enforced grants, approvals, and policy',
      text: 'Teams, grants, approval rules, and mandatory OPA/Rego policies live in host config, out of reach of merge requests and CI YAML edits.',
    },
    {
      num: '08',
      title: 'Authors cannot approve their own plans',
      text: 'Self-approval is refused under every approval rule, matched by stable `IDENTITY/user/ID` across CI OIDC tokens, CLI sessions, and browser sessions.',
    },
    {
      num: '09',
      title: 'Synchronous audit log',
      text: 'API, UI, and state requests commit an audit row before `writ serve` responds (health checks, discovery, and per-address rate-limit refusals excepted), readable via `GET /v1/audit`, `writ audit --remote`, and `/ui/audit`.',
    },
    {
      num: '10',
      title: 'Scheduled drift checks and S3 state verification',
      text: 'Drift checks detect out-of-band cloud edits, and S3 state written outside Writ blocks approvals until reviewed with `writ state adopt`.',
    },
    {
      num: '11',
      title: 'Per-address and per-principal rate and time limits',
      text: 'Token-bucket rate limits per client IP and principal, concurrency caps, and hard plan (`30m`) and apply (`4h`) deadlines protect the server.',
    },
    {
      num: '12',
      title: 'Per-run network egress control',
      text: 'Under `egress_mode = "enforce"`, each run sits in a loopback-only network namespace exiting solely through Writ’s proxy, restricted to each environment’s `egress` list.',
    },
    {
      num: '13',
      title: 'Per-step cgroup limits and seccomp filter',
      text: 'Where the host allows them, each OpenTofu step runs in its own cgroup (`run_memory`, `run_pids`, `run_cpus`, `cgroup.kill`) under a seccomp filter blocking mounts, new namespaces, `ptrace`, `bpf`, and kernel module calls. `writ serve --check` says when they are off.',
    },
  ],
  limit:
    'Egress control defaults to `egress_mode = "off"`, where runs share the host network apart from the metadata firewall. Under `enforce`, keep `env.egress` lists narrow.',
  link: { text: 'Read the full security model', href: '/docs/security-model' },
} as const;

export const policySection = {
  title: 'Your rules, as code',
  lede: 'Approval rules set how many teammates review a plan. Policy sets what no plan may do, whoever approves it. Write OPA Rego over OpenTofu plan JSON, test locally with `conftest`, and enforce on `writ serve` at plan and apply time.',
  verdicts: [
    {
      title: 'Deny at plan',
      text: 'Marks the plan `refused` and discards the saved plan bytes so it cannot be approved or applied.',
    },
    {
      title: 'Deny at apply',
      text: 'Re-evaluates live policies against `input.writ.now`, caller, and `input.writ.approvals` right before `tofu apply` starts.',
    },
    {
      title: 'Warn without blocking',
      text: '`warn*` rules and `advisory` sets attach warnings in the CLI, control UI, and API without blocking approval or apply.',
    },
    {
      title: 'No override for a mandatory deny',
      text: '`writ apply --remote --override` skips approval rules only. To clear a mandatory policy deny, fix the code and plan again.',
    },
  ],
  regoTitle: 'policy/baseline/tls.rego',
  regoCaption: 'Rules in `examples/policy/tls.rego` and `examples/policy/files.rego`, verified with `conftest verify --policy examples/policy`.',
  outputTitle: 'writ plan --remote (exit code 1)',
  outputCaption: 'Output of `TestTofuPlanByCommit`, 2026-11-15.',
  limit: 'Policies load only from host config so merge requests cannot weaken the rules checking them.',
  links: [
    { text: 'Write and test policies', href: '/docs/write-a-policy' },
    { text: 'Policy cookbook (12 recipes)', href: '/docs/policy-cookbook' },
    { text: 'Policy reference', href: '/docs/policy-reference' },
  ],
} as const;

export const tour = {
  title: 'Control UI built for plan review',
  lede: 'Ships inside the `writ serve` binary with zero external assets. Review masked plans, inspect indexed stack resources, track drift, read the audit log, and approve changes in light or dark mode.',
  tabs: [
    {
      id: 'overview',
      label: 'Overview',
      caption: 'Pending approvals, active applies, recent failures, drift alerts, and stacks with state across environments.',
      alt: 'Writ control UI overview showing stat cards for plans waiting for approval, applying, failed, and drifted stacks, alerts, queues, and stacks with state.',
    },
    {
      id: 'plan',
      label: 'Plan review',
      caption: 'Approval status, Approve button, risk classes, attribute diffs, and `(sensitive)` values.',
      alt: 'Writ control UI plan review page showing Plan, Approve, and Apply stations, Approve this plan button, and masked attribute changes.',
    },
    {
      id: 'resources',
      label: 'Resources',
      caption: 'Stack state index grouped by service category (Networking, IAM, Compute, Nomad, Storage, DNS) or module, with search.',
      alt: 'Writ control UI stack Resources view showing 63 resources grouped into category cards for Networking, IAM, Compute, Nomad, Storage, DNS, Certificates, and Other.',
    },
    {
      id: 'stack',
      label: 'Drift',
      caption: 'Latest drift check separating out-of-band cloud changes from unapplied default-branch code.',
      alt: 'Writ control UI stack Drift view showing outside changes and code changes for each affected resource address.',
    },
    {
      id: 'runs',
      label: 'Runs',
      caption: 'Apply history linked to each plan ID, stack, actor, status, and duration.',
      alt: 'Writ control UI Runs list showing applying, applied, and failed runs with their stacks, plans, and actors.',
    },
  ],
  limit: 'You can review plans, resources, drift, runs, and audit rows and approve plans in the control UI. Applies run via `writ apply --remote` or CI.',
} as const;

export const benefits = {
  title: 'Keys, secrets, and policy enforced on one server',
  keys: {
    title: 'CI holds no cloud credentials',
    text: 'CI jobs authenticate with OIDC tokens and never hold cloud keys or state keys. Writ assumes `plan_role` for plans and `apply_role` only for approved applies or recorded overrides, tagging every STS session for CloudTrail.',
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
    text: 'Teams, grants, approval rules, policies, and environment IAM roles live in one HCL file on the server. Merge requests cannot edit `.gitlab-ci.yml` to skip reviewers or swap roles.',
    limit: 'Config changes apply after `writ serve --check` and a server restart.',
    link: { text: 'Server config reference', href: '/docs/config' },
    caption: 'Excerpt from `docs/quick-start.md` and `docs/aws-credentials.md`. Checked with `writ serve --check`.',
  },
  secrets: {
    title: 'Secrets masked from every log and record',
    text: 'One mask covers plans, apply output, diagnostics, and the state index. Sensitive values, `provider_env` secrets, and assumed STS keys become `(sensitive)`, including URL, JSON, hex, and base64 encodings.',
    limit: 'The mask is an output filter, not taint tracking. The docs list every edge case it cannot catch.',
    link: { text: 'Handle secrets and masking', href: '/docs/secrets' },
    alt: 'A replaced resource in the Writ control UI whose keepers show {"secret":"(sensitive)"} before and after, marked sensitive value changes.',
  },
  drift: {
    title: 'Scheduled drift checks and notifications',
    text: 'Add `drift { every = "6h" }` and Writ plans each stack on a schedule, separating out-of-band cloud changes from unapplied code without storing secret values. Configure `notify` blocks to alert Slack or a webhook.',
    limit: 'Built-in alert channels are Slack and webhooks with optional HMAC signing; email is not built.',
    link: { text: 'Enable drift checks', href: '/docs/drift-checks' },
    alt: 'A stack drift table in the Writ control UI separating changes outside OpenTofu from code changes.',
  },
  audit: {
    title: 'Synchronous audit log in Postgres',
    text: 'Every API, control UI, and state-backend request commits an audit row before responding; if the database cannot commit, Writ returns `503`. Filter entries via `writ audit --remote`, `GET /v1/audit`, or `/ui/audit`.',
    limit: 'Refusal reasons and client IPs are withheld from API and UI responses and kept in Postgres.',
    link: { text: 'Read the audit log', href: '/docs/audit-log' },
    caption: 'Output of `writ audit --remote` under `writ serve --dev`, 2026-11-19.',
  },
  policy: {
    title: 'Rego policy checked at plan and apply',
    text: 'Policies execute in a bounded child process with a strict built-in allow-list and match `conftest` locally. Writ evaluates every plan at creation and again before apply; nothing overrides a mandatory deny.',
    limit: 'Test rules locally with `conftest verify` and `conftest test`; there is no `writ policy test` yet.',
    link: { text: 'Write and test policies', href: '/docs/write-a-policy' },
    caption: 'Output of `TestTofuPlanByCommit`, 2026-11-15, with lines cut.',
  },
  also: [
    {
      title: 'Sandboxed runs and egress controls',
      text: 'On Linux, each `tofu` step runs under a dedicated `provider_uids` UID inside user/mount/PID/IPC namespaces with an `nftables` metadata block and an optional egress proxy. Seccomp and cgroup limits apply where the host allows them. [Run isolation and egress](/docs/run-isolation)',
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
      text: 'Use Writ’s sealed `http` backend or keep `backend "s3"`, and generate import blocks with `writ import bulk`. [Migrate from Terraform](/docs/migrate-from-terraform)',
    },
  ],
} as const;

export const whenToUse = {
  title: 'When Writ fits your team',
  lede: 'Writ is for teams that want server-side credential isolation, plan-bound approvals, and OPA/Rego policy without per-resource SaaS pricing.',
  writ: {
    title: 'Choose Writ when',
    items: [
      'CI pipelines and developer laptops must never hold cloud credentials or state encryption keys.',
      'Approvals must bind to the exact sealed plan that applies, and plan authors must never self-approve.',
      'You want masked plans, OPA/Rego policy at plan and apply, scheduled drift checks, Slack/webhook alerts, and a synchronous audit log on your own server.',
    ],
  },
  plainCi: {
    title: 'Stick with plain CI + OIDC when',
    items: [
      'One small team owns the repo and runners, and anyone who can merge is already trusted to change production directly.',
      'You want zero extra server infrastructure beyond GitLab runners and your existing state bucket.',
      'You need GitHub Actions, Terraform (instead of OpenTofu), or merge-request plan comments right now.',
    ],
  },
} as const;

export const compare = {
  title: 'Writ, Spacelift, or HCP Terraform?',
  lede: 'Hosted control planes meter seats, runs, or managed resources. Writ runs as one Apache 2.0 binary on your own server with nothing metered.',
  columns: ['Spacelift', 'HCP Terraform', 'Writ'],
  rows: [
    ['Source and hosting', 'Closed source, SaaS; self-hosted on Enterprise', 'Closed source, SaaS; Terraform Enterprise is self-managed', 'Apache 2.0, self-hosted only; no hosted service'],
    ['Price', 'Free for 2 users; Starter from $399/mo', 'Free to 500 managed resources; then from $0.10/resource/mo', 'Free; no limit on users, runs, or resources'],
    ['OpenTofu', 'Yes', 'Runs Terraform; docs do not mention OpenTofu', 'OpenTofu 1.13.9 only; no Terraform'],
    ['Policy', 'OPA, from Starter plan', 'Sentinel and OPA in Standard and Premium', 'OPA Rego at plan and again before apply'],
    ['Drift checks', 'Business plan and up', 'Standard and Premium', 'Scheduled drift checks in UI and API, with Slack or webhook notifications'],
    ['Audit log', 'Enterprise plan', 'Premium plan', 'Synchronous Postgres row before each response; read via `/ui/audit`, `GET /v1/audit`, or `writ audit --remote`'],
  ],
  caption:
    'Spacelift and HCP Terraform details come from their pricing and docs pages, read 2026-11-16. [The comparison page](/docs/compare) cites every source and also covers Atlantis, OTF, Terrakube, env zero, and Scalr.',
  missing:
    'They offer what Writ doesn’t: a hosted service, commercial support, Terraform, and GitHub. If you need those today, pick one of them.',
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
      text: 'End-to-end suites run `writ` against real OpenTofu 1.13.9 and real providers in network namespaces, plus differential secret-mask checks.',
      link: { text: 'How it’s tested', href: '/docs/status#how-its-tested' },
    },
    {
      title: 'The audit log.',
      text: 'Every response waits for its Postgres audit row (except `/healthz`, `/v1/server`, and per-IP `429`s).',
      link: { text: 'Read the audit log', href: '/docs/audit-log' },
    },
  ],
  notYet: { title: 'Current scope and limits', link: { text: 'Supported platforms and limits', href: '/docs/status' } },
} as const;

export const tryIt = {
  title: 'Try it locally in four steps',
  lede: '`writ serve --dev` runs the same control plane on loopback with SQLite and the control UI, with no cloud account needed.',
  needs: [
    'No cloud account required; uses `hashicorp/random`.',
    'Needs Linux, `minisign`, Git, and OpenTofu 1.13.9.',
    'The install script verifies the minisign signature and SHA-256. [Install](/docs/install) covers containers, Nix, and source builds.',
  ],
  build: 'Install Writ and start the dev control plane',
  run: 'In a second terminal, plan a commit and apply the plan',
  caption: 'Output of a real run, 2026-11-16, with some lines cut. Follow the [quick start](/docs/quick-start) for the full stack.',
};

export const questions: { q: string; a: string }[] = [
  { q: 'Does Writ run Terraform?', a: 'No. Writ runs OpenTofu 1.13.9 only. If you are moving from Terraform, [Migrate from Terraform](/docs/migrate-from-terraform) shows how Writ handles existing lock files and state.' },
  {
    q: 'Does it work with GitHub?',
    a: 'Not yet. GitLab is the remote Git host Writ reads today (`writ serve --dev` also reads local Git repos), and the bundled CI components target GitLab CI. See [Supported platforms and limits](/docs/status).',
  },
  {
    q: 'How do Rego policies interact with approvals and overrides?',
    a: 'Approval rules set how many teammates must approve a plan; Rego policies set what no plan may do, whoever approves it. Writ evaluates policies at plan creation and again before apply, and `--override` never bypasses a mandatory deny. See the [Policy cookbook](/docs/policy-cookbook).',
  },
  {
    q: 'How are runs isolated on the server?',
    a: 'On Linux, each `tofu` step runs under a dedicated `provider_uids` UID inside private user, mount, PID, and IPC namespaces with a read-only filesystem view, an `nftables` EC2 metadata block, and an optional egress proxy. A seccomp filter and cgroup resource limits apply where the host allows them; `writ serve --check` reports whether they do. Dev mode sets no run limits. See [Run isolation and egress](/docs/run-isolation).',
  },
  {
    q: 'Where does my state live?',
    a: 'New stacks can use `writ serve`’s built-in `http` backend (AES-256-GCM sealed and versioned in Postgres). Existing stacks on `backend "s3"` can stay in their S3 bucket with separate read-only plan and write-capable apply IAM roles.',
  },
  {
    q: 'How does it compare with Atlantis, Spacelift, and HCP Terraform?',
    a: '[Writ vs other control planes](/docs/compare) compares Atlantis, OTF, Terrakube, Spacelift, HCP Terraform, env zero, and Scalr with sources and dates; [Writ vs plain GitLab CI](/docs/compare-gitlab-ci) covers plain CI.',
  },
];
