// Facts about the project that the site repeats in more than one place.
export const site = {
  name: 'Writ',
  tagline: 'Plan, approve, and apply OpenTofu on your own server',
  description:
    'Self-hosted, open-source control plane for OpenTofu. CI sends a commit via OIDC with no cloud keys; Writ plans in a sandbox, enforces Rego policy, waits for plan approval, and applies that exact sealed plan.',
  // The module path in go.mod. Change it here if the repository is published elsewhere.
  repo: 'https://github.com/mr-karan/writ',
  branch: 'main',
} as const;

/** The URL of a file or directory in the repository, from a path relative to its root. */
export function repoUrl(path: string): string {
  const kind = path.endsWith('/') ? 'tree' : 'blob';
  return `${site.repo}/${kind}/${site.branch}/${path.replace(/\/$/, '')}`;
}
