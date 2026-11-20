// The docs site has no text of its own. Every page below is built from the
// Markdown in the repository (docs/) when the site builds, by
// scripts/gen-content.ts. This file only says which source goes on which page,
// in what order, and under what title. Each page is one whole file today. The
// other selectors remain for a page that joins parts of several files.

export type Part =
  // A whole file, without its H1.
  | { file: string; whole: true }
  // Everything between a file's H1 and its first H2.
  | { file: string; intro: true }
  // One section, named by its heading text. Its heading is dropped (the page
  // title replaces it) unless keepHeading is set.
  | { file: string; section: string; keepHeading?: boolean }
  // The block (paragraph, list, table, or code fence) that starts with this
  // text, and the count - 1 blocks after it.
  | { file: string; block: string; count?: number }
  // The rows of one table whose first cell starts with one of these strings.
  | { file: string; table: string; rows: string[] }
  // One list item, shown as a paragraph.
  | { file: string; item: string }
  // A heading that exists only on the site, to join parts cut from different
  // places.
  | { heading: string };

export interface Page {
  slug: string;
  title: string;
  description: string;
  parts: Part[];
}

export type NavEntry = string | { separator: string } | { folder: string; title: string; pages: string[] };

const whole = (file: string): Part[] => [{ file, whole: true }];

export const pages: Page[] = [
  // Get started
  {
    slug: 'index',
    title: 'What Writ is',
    description: 'What Writ is, how it keeps cloud credentials out of CI, and where to find each page of the docs.',
    parts: whole('docs/README.md'),
  },
  {
    slug: 'quick-start',
    title: 'Quick start',
    description: 'Try Writ locally with writ serve --dev and no cloud account, then set it up for a team where a second person approves the plan.',
    parts: whole('docs/quick-start.md'),
  },
  {
    slug: 'install',
    title: 'Install',
    description: 'Get writ with the install script, as a container image, with Nix, or from source.',
    parts: whole('docs/install.md'),
  },
  {
    slug: 'status',
    title: 'Supported platforms and limits',
    description: 'Supported platforms, how Writ is tested, and current feature limits.',
    parts: whole('docs/status.md'),
  },
  {
    slug: 'compare-gitlab-ci',
    title: 'Writ vs plain GitLab CI',
    description: 'What Writ adds to a pipeline whose jobs run tofu themselves, what it costs, and when plain CI is the better choice.',
    parts: whole('docs/compare-gitlab-ci.md'),
  },
  {
    slug: 'compare',
    title: 'Writ vs other control planes',
    description: 'Writ against Atlantis, OTF, Terrakube, Spacelift, HCP Terraform, env zero, and Scalr, with sources and dates.',
    parts: whole('docs/compare.md'),
  },

  // Guides
  {
    slug: 'add-repository',
    title: 'Add a repository',
    description: 'Add a local repository or a GitLab project, and the rules for names and prefixes.',
    parts: whole('docs/add-repository.md'),
  },
  {
    slug: 'set-up-gitlab-ci',
    title: 'Set up GitLab CI',
    description: 'Plan, approve, and apply from GitLab CI jobs with no checkout and no cloud credential.',
    parts: whole('docs/set-up-gitlab-ci.md'),
  },
  {
    slug: 'apply',
    title: 'Plan and apply',
    description: 'Plan a commit, approve it, apply by ID or after a merge, recover when an answer is lost, and override approval rules.',
    parts: whole('docs/apply.md'),
  },
  {
    slug: 'approvals',
    title: 'Configure approvals',
    description: 'Write approval rules by stack, environment, and risk class, see who can approve, and understand stale plans.',
    parts: whole('docs/approvals.md'),
  },
  {
    slug: 'write-a-policy',
    title: 'Write and test policies',
    description: 'Write a Rego policy, test it locally with conftest, wire it into writ serve, and start from the examples.',
    parts: whole('docs/write-a-policy.md'),
  },
  {
    slug: 'policy-cookbook',
    title: 'Policy cookbook',
    description: 'Twelve copy-pasteable Rego rules and unit tests for tags, S3, encryption, security groups, instance sizes, regions, change windows, and approvals.',
    parts: whole('docs/policy-cookbook.md'),
  },
  {
    slug: 'adopt-state',
    title: 'Adopt existing state',
    description: 'Seal a secret set with a state Writ didn’t write so its plans can be approved again.',
    parts: whole('docs/adopt-state.md'),
  },
  {
    slug: 'migrate-from-terraform',
    title: 'Migrate from Terraform',
    description: 'Bring a Terraform stack, lock file, provider sources, and existing state to Writ and OpenTofu.',
    parts: whole('docs/migrate-from-terraform.md'),
  },
  {
    slug: 'secrets',
    title: 'Handle secrets and masking',
    description: 'How Writ keeps secrets out of plans, run output, and the state index, how masking is tested, and its limits.',
    parts: whole('docs/secrets.md'),
  },
  {
    slug: 'drift-checks',
    title: 'Enable drift checks',
    description: 'Schedule periodic drift plans, read outside and code drift in the UI and API, and troubleshoot common failures.',
    parts: whole('docs/drift-checks.md'),
  },
  {
    slug: 'audit-log',
    title: 'Read the audit log',
    description: 'Query Writ’s synchronous audit log with writ audit --remote and GET /v1/audit.',
    parts: whole('docs/audit-log.md'),
  },
  {
    slug: 'import-bulk',
    title: 'Import existing objects',
    description: 'Generate resource and import blocks for existing infrastructure in one run and accept the import plan.',
    parts: whole('docs/import-bulk.md'),
  },
  {
    slug: 'agents',
    title: 'Use Writ with a coding agent',
    description: 'Install the writ skill with writ agent init and connect writ mcp so coding agents plan without cloud credentials.',
    parts: whole('docs/agents.md'),
  },

  // Operate a server
  {
    slug: 'run',
    title: 'Run a server',
    description: 'Start writ serve --dev locally or run a team’s writ serve with Postgres, containers, or NixOS.',
    parts: whole('docs/run.md'),
  },
  {
    slug: 'sign-in',
    title: 'Configure sign-in',
    description: 'Set up OIDC for writ login (device and browser flows) and the control UI, and manage active sessions.',
    parts: whole('docs/sign-in.md'),
  },
  {
    slug: 'database',
    title: 'Operate the database',
    description: 'Postgres and SQLite control database layout, tables, audit rows, timeouts, and schema migrations.',
    parts: whole('docs/database.md'),
  },
  {
    slug: 'aws-credentials',
    title: 'AWS roles and credentials',
    description: 'Configure separate plan and apply AWS IAM roles, S3 state permissions, CloudTrail tracing, and provider_env.',
    parts: whole('docs/aws-credentials.md'),
  },
  {
    slug: 'run-isolation',
    title: 'Run isolation and egress',
    description: 'Isolate OpenTofu runs with provider_uids, Linux namespaces, the EC2 metadata firewall rule, and module source rules.',
    parts: whole('docs/run-isolation.md'),
  },
  {
    slug: 'upgrades',
    title: 'Back up and upgrade',
    description: 'Back up Postgres and offline state keys, set retention, and upgrade single-node writ serve instances safely.',
    parts: whole('docs/upgrades.md'),
  },

  // Reference
  {
    slug: 'commands',
    title: 'Commands',
    description: 'Every command and flag, exit codes, and environment variables.',
    parts: whole('docs/commands.md'),
  },
  {
    slug: 'config',
    title: 'Server config',
    description: 'Every setting and block of the writ serve config, a valid sample config, and what writ serve --check refuses.',
    parts: whole('docs/config.md'),
  },
  {
    slug: 'api',
    title: 'API',
    description: 'Every route of writ serve, token checks, rate and time limits, audit guarantees, and what the API refuses.',
    parts: whole('docs/api.md'),
  },
  {
    slug: 'json-output',
    title: 'JSON output',
    description: 'The contract for every --json document: formats, fields, and what may change.',
    parts: whole('docs/json-output.md'),
  },
  {
    slug: 'errors',
    title: 'Errors',
    description: 'Each error class: what it means, its exit code and HTTP status, its common messages, and what to do.',
    parts: whole('docs/errors.md'),
  },
  {
    slug: 'policy-reference',
    title: 'Policy reference',
    description: 'The input a policy reads, the Rego built-ins it can call, and its execution bounds.',
    parts: whole('docs/policy-reference.md'),
  },
  {
    slug: 'agent-reference',
    title: 'Agent MCP reference',
    description: 'The tools of writ mcp, the documents they return, policy handling, and how to recover from each refusal.',
    parts: whole('docs/agent-reference.md'),
  },
  {
    slug: 'glossary',
    title: 'Glossary',
    description: 'The terms the docs use, each with one meaning and its Terraform equivalent.',
    parts: whole('docs/glossary.md'),
  },

  // Concepts
  {
    slug: 'architecture',
    title: 'Architecture',
    description: 'The package map, data flow, enforced invariants, and dependencies.',
    parts: whole('docs/architecture.md'),
  },
  {
    slug: 'plan-to-apply',
    title: 'How a plan becomes an apply',
    description: 'End-to-end lifecycle from commit fetch and sealed plan to approvals, apply-point policy, state indexing, and crash recovery.',
    parts: whole('docs/plan-to-apply.md'),
  },
  {
    slug: 'security-model',
    title: 'Security model',
    description: 'Credential boundaries, OIDC verification, run sandboxing, secret masking, policy isolation, and explicit limits.',
    parts: whole('docs/security-model.md'),
  },
  {
    slug: 'serve',
    title: 'Control plane internals',
    description: 'Deep dive into OpenTofu stacks, S3 state tracking, drift checks, teams and grants, and isolation.',
    parts: whole('docs/serve.md'),
  },
  {
    slug: 'repos',
    title: 'How repositories work',
    description: 'How the control plane fetches commits, verifies Git blob IDs, caches blobs, and discovers stacks.',
    parts: whole('docs/repos.md'),
  },
  {
    slug: 'policy',
    title: 'How policy works',
    description: 'Plan-point and apply-point Rego evaluation, mandatory vs advisory sets, and how policy handles secrets.',
    parts: whole('docs/policy.md'),
  },
  {
    slug: 'development',
    title: 'Contributing and testing',
    description: 'Build and test commands, hosted CI, Nix hash checks, and the test plan for a real GitLab instance.',
    parts: whole('docs/development.md'),
  },
];

export const nav: NavEntry[] = [
  { separator: 'Get started' },
  'index',
  'quick-start',
  'install',
  'status',
  'compare-gitlab-ci',
  'compare',
  { separator: 'Guides' },
  'add-repository',
  'set-up-gitlab-ci',
  'apply',
  'approvals',
  'write-a-policy',
  'policy-cookbook',
  'adopt-state',
  'migrate-from-terraform',
  'secrets',
  'drift-checks',
  'audit-log',
  'import-bulk',
  'agents',
  { separator: 'Operate a server' },
  'run',
  'sign-in',
  'database',
  'aws-credentials',
  'run-isolation',
  'upgrades',
  { separator: 'Reference' },
  'commands',
  'config',
  'api',
  'json-output',
  'errors',
  'policy-reference',
  'agent-reference',
  'glossary',
  { separator: 'Concepts' },
  'architecture',
  'plan-to-apply',
  'security-model',
  'serve',
  'repos',
  'policy',
  'development',
];

// Where a link to a source file lands on the site.
export const fileRoutes: Record<string, string> = {
  'docs/README.md': '/docs',
  'docs/quick-start.md': '/docs/quick-start',
  'docs/install.md': '/docs/install',
  'docs/status.md': '/docs/status',
  'docs/compare-gitlab-ci.md': '/docs/compare-gitlab-ci',
  'docs/compare.md': '/docs/compare',
  'docs/add-repository.md': '/docs/add-repository',
  'docs/set-up-gitlab-ci.md': '/docs/set-up-gitlab-ci',
  'docs/apply.md': '/docs/apply',
  'docs/approvals.md': '/docs/approvals',
  'docs/write-a-policy.md': '/docs/write-a-policy',
  'docs/policy-cookbook.md': '/docs/policy-cookbook',
  'docs/adopt-state.md': '/docs/adopt-state',
  'docs/migrate-from-terraform.md': '/docs/migrate-from-terraform',
  'docs/secrets.md': '/docs/secrets',
  'docs/drift-checks.md': '/docs/drift-checks',
  'docs/audit-log.md': '/docs/audit-log',
  'docs/import-bulk.md': '/docs/import-bulk',
  'docs/agents.md': '/docs/agents',
  'docs/run.md': '/docs/run',
  'docs/sign-in.md': '/docs/sign-in',
  'docs/database.md': '/docs/database',
  'docs/aws-credentials.md': '/docs/aws-credentials',
  'docs/run-isolation.md': '/docs/run-isolation',
  'docs/upgrades.md': '/docs/upgrades',
  'docs/commands.md': '/docs/commands',
  'docs/config.md': '/docs/config',
  'docs/api.md': '/docs/api',
  'docs/json-output.md': '/docs/json-output',
  'docs/errors.md': '/docs/errors',
  'docs/policy-reference.md': '/docs/policy-reference',
  'docs/agent-reference.md': '/docs/agent-reference',
  'docs/glossary.md': '/docs/glossary',
  'docs/architecture.md': '/docs/architecture',
  'docs/plan-to-apply.md': '/docs/plan-to-apply',
  'docs/security-model.md': '/docs/security-model',
  'docs/serve.md': '/docs/serve',
  'docs/repos.md': '/docs/repos',
  'docs/policy.md': '/docs/policy',
  'docs/development.md': '/docs/development',
};
