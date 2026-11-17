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
  {
    slug: 'index',
    title: 'Overview',
    description: 'What Writ is, and where to find each page of the docs.',
    parts: whole('docs/README.md'),
  },
  {
    slug: 'quick-start',
    title: 'Quick start',
    description: 'Try Writ on your own with writ serve --dev and no cloud account, then set it up for a team, where a second person approves the plan.',
    parts: whole('docs/quick-start.md'),
  },
  {
    slug: 'install',
    title: 'Install',
    description: 'Get writ with the install script, as a container image, with Nix, or from source.',
    parts: whole('docs/install.md'),
  },
  {
    slug: 'compare-gitlab-ci',
    title: 'Writ compared with OpenTofu in plain GitLab CI',
    description: 'What Writ adds to a pipeline whose jobs run tofu themselves, what it costs, and when plain CI is the better choice.',
    parts: whole('docs/compare-gitlab-ci.md'),
  },
  {
    slug: 'compare',
    title: 'Writ compared with other control planes',
    description: 'Writ against Atlantis, OTF, Terrakube, Spacelift, HCP Terraform, env zero, and Scalr, from each tool’s own pages, with sources and dates, and what they have that Writ doesn’t.',
    parts: whole('docs/compare.md'),
  },
  {
    slug: 'status',
    title: 'Status and limits',
    description: 'What works in version 0, how it is tested, and what is missing.',
    parts: whole('docs/status.md'),
  },
  {
    slug: 'serve',
    title: 'How the control plane works',
    description: 'How writ serve plans a commit, runs OpenTofu apart from its keys, keeps state, checks drift, and decides who can do what.',
    parts: whole('docs/serve.md'),
  },
  {
    slug: 'repos',
    title: 'Repositories',
    description: 'How the control plane reads your code by commit, finds stacks, and plans a commit.',
    parts: whole('docs/repos.md'),
  },
  {
    slug: 'approvals',
    title: 'Approvals',
    description: 'How a plan is approved: its lifecycle, approval rules, who can approve, stale plans, and what each run records.',
    parts: whole('docs/approvals.md'),
  },
  {
    slug: 'secrets',
    title: 'Secrets and the mask',
    description: 'How writ serve keeps secrets out of everything it stores and shows, how that is tested, and what the mask can’t catch.',
    parts: whole('docs/secrets.md'),
  },
  {
    slug: 'policy',
    title: 'Policy',
    description: 'Rego policies that check each OpenTofu plan when it is created and again before its apply.',
    parts: whole('docs/policy.md'),
  },
  {
    slug: 'run',
    title: 'Run a control plane',
    description: 'Start writ serve --dev for yourself, or a team’s writ serve with Postgres or on NixOS, and back it up.',
    parts: whole('docs/run.md'),
  },
  {
    slug: 'sign-in',
    title: 'Sign in',
    description: 'How the CLI finds a control plane, writ login, and the control UI.',
    parts: whole('docs/sign-in.md'),
  },
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
    title: 'Approve and apply a plan',
    description: 'Apply a plan by its id or after a merge, recover when an answer is lost, and override the approval rules.',
    parts: whole('docs/apply.md'),
  },
  {
    slug: 'adopt-state',
    title: 'Adopt a state',
    description: 'Seal a secret set with a state Writ didn’t write, so its plans can be approved again.',
    parts: whole('docs/adopt-state.md'),
  },
  {
    slug: 'migrate-from-terraform',
    title: 'Migrate from Terraform',
    description: 'Plan a stack Terraform ran, with its lock file, provider sources, and state.',
    parts: whole('docs/migrate-from-terraform.md'),
  },
  {
    slug: 'write-a-policy',
    title: 'Write and test a policy',
    description: 'Write a Rego policy, test it with conftest, and start from the examples.',
    parts: whole('docs/write-a-policy.md'),
  },
  {
    slug: 'import-bulk',
    title: 'Import many objects',
    description: 'Take over many existing objects into an OpenTofu stack in one run, and accept the plan that imports them.',
    parts: whole('docs/import-bulk.md'),
  },
  {
    slug: 'agents',
    title: 'Use Writ with an agent',
    description: 'Set up a repository for a coding agent: the skill writ agent init installs, writ mcp, and what has been measured.',
    parts: whole('docs/agents.md'),
  },
  {
    slug: 'commands',
    title: 'Commands',
    description: 'Every command and flag, exit codes, and environment variables.',
    parts: whole('docs/commands.md'),
  },
  {
    slug: 'config',
    title: 'Server config',
    description: 'Every setting and block of the writ serve config, and what writ serve --check refuses.',
    parts: whole('docs/config.md'),
  },
  {
    slug: 'api',
    title: 'API',
    description: 'Every route of writ serve, token checks, rate and time limits, and what the API refuses.',
    parts: whole('docs/api.md'),
  },
  {
    slug: 'database',
    title: 'Control database',
    description: 'What the control database keeps, table by table, and what each audit row records.',
    parts: whole('docs/database.md'),
  },
  {
    slug: 'policy-reference',
    title: 'Policy input and built-ins',
    description: 'The input a policy reads, the Rego built-ins it can call, and its bounds.',
    parts: whole('docs/policy-reference.md'),
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
    slug: 'agent-reference',
    title: 'Reference for agents',
    description: 'What an agent reads and calls: the tools of writ mcp, the documents they return, the policy, and how to recover from each refusal.',
    parts: whole('docs/agent-reference.md'),
  },
  {
    slug: 'glossary',
    title: 'Glossary',
    description: 'The terms the docs use, each with one meaning and its Terraform equivalent.',
    parts: whole('docs/glossary.md'),
  },
  {
    slug: 'development',
    title: 'Development',
    description: 'Build and test commands, how Writ is built, and the test plans that need a real host or GitLab.',
    parts: whole('docs/development.md'),
  },
  {
    slug: 'architecture',
    title: 'Architecture',
    description: 'The package map of version 0, the decisions behind it, and how an interrupted apply recovers.',
    parts: whole('docs/architecture.md'),
  },
];

export const nav: NavEntry[] = [
  { separator: 'Start' },
  'index',
  'quick-start',
  'install',
  'compare-gitlab-ci',
  'compare',
  'status',
  { separator: 'Concepts' },
  'serve',
  'repos',
  'approvals',
  'secrets',
  'policy',
  { separator: 'Guides' },
  'run',
  'sign-in',
  'add-repository',
  'set-up-gitlab-ci',
  'apply',
  'adopt-state',
  'migrate-from-terraform',
  'write-a-policy',
  'import-bulk',
  'agents',
  { separator: 'Reference' },
  'commands',
  'config',
  'api',
  'database',
  'policy-reference',
  'json-output',
  'errors',
  'agent-reference',
  'glossary',
  { separator: 'Project' },
  'development',
  'architecture',
];

// Where a link to a source file lands on the site.
export const fileRoutes: Record<string, string> = {
  'docs/README.md': '/docs',
  'docs/quick-start.md': '/docs/quick-start',
  'docs/compare-gitlab-ci.md': '/docs/compare-gitlab-ci',
  'docs/compare.md': '/docs/compare',
  'docs/install.md': '/docs/install',
  'docs/status.md': '/docs/status',
  'docs/serve.md': '/docs/serve',
  'docs/repos.md': '/docs/repos',
  'docs/approvals.md': '/docs/approvals',
  'docs/secrets.md': '/docs/secrets',
  'docs/policy.md': '/docs/policy',
  'docs/run.md': '/docs/run',
  'docs/sign-in.md': '/docs/sign-in',
  'docs/add-repository.md': '/docs/add-repository',
  'docs/set-up-gitlab-ci.md': '/docs/set-up-gitlab-ci',
  'docs/apply.md': '/docs/apply',
  'docs/adopt-state.md': '/docs/adopt-state',
  'docs/migrate-from-terraform.md': '/docs/migrate-from-terraform',
  'docs/write-a-policy.md': '/docs/write-a-policy',
  'docs/import-bulk.md': '/docs/import-bulk',
  'docs/agents.md': '/docs/agents',
  'docs/commands.md': '/docs/commands',
  'docs/config.md': '/docs/config',
  'docs/api.md': '/docs/api',
  'docs/database.md': '/docs/database',
  'docs/policy-reference.md': '/docs/policy-reference',
  'docs/json-output.md': '/docs/json-output',
  'docs/errors.md': '/docs/errors',
  'docs/agent-reference.md': '/docs/agent-reference',
  'docs/glossary.md': '/docs/glossary',
  'docs/development.md': '/docs/development',
  'docs/architecture.md': '/docs/architecture',
};
