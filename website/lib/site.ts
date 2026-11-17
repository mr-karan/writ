// Facts about the project that the site repeats in more than one place.
export const site = {
  name: 'Writ',
  tagline: 'Plan, approve, and apply OpenTofu on your own server',
  description:
    'Writ is an open-source control plane for OpenTofu that you host yourself. It plans the commits you or your CI job send it, waits for your team to approve, and applies that exact plan.',
  // The module path in go.mod. Change it here if the repository is published elsewhere.
  repo: 'https://github.com/mr-karan/writ',
  branch: 'main',
} as const;

/** The URL of a file or directory in the repository, from a path relative to its root. */
export function repoUrl(path: string): string {
  const kind = path.endsWith('/') ? 'tree' : 'blob';
  return `${site.repo}/${kind}/${site.branch}/${path.replace(/\/$/, '')}`;
}
