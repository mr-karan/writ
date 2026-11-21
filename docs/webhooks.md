# Get changes in seconds with a webhook

By default, `writ serve` polls each repository's default branch every `poll_every` (60 seconds). Add a GitLab webhook so `writ serve` reads a pushed or merged head within seconds. Polling continues every `poll_every_with_hook` (5 minutes by default) as a backstop, because GitLab temporarily disables a webhook after 4 consecutive failures and permanently disables it after 40.

A webhook only triggers an early read of the default branch through GitLab's API with the forge's read token. It never starts a plan or apply, and `writ serve` never trusts or parses fields from the webhook body: the repository comes from the URL path, and the commit comes from GitLab's API.

## Before you begin

- The repository is pinned with a `repo` block in `/etc/writ/serve`. Only pinned repositories support webhooks because the secret path is governed by server config. Repositories added via `writ repo add` use polling only.
- GitLab can reach `writ serve`'s `url` over HTTPS.
- You have Maintainer or Owner access on the GitLab project.

## Set up a webhook

### 1. Create the secret

Generate a random secret readable only by the `writ serve` user:

```sh
openssl rand -hex 32 > /run/secrets/writ/infra_hook
chmod 0400 /run/secrets/writ/infra_hook
```

On NixOS, deliver it with `sops-nix` or `agenix`, like the forge read token.

### 2. Point the repository at it

Add `webhook_secret_file` to the `repo` block and restart `writ serve`:

```hcl
repo {
  name                = "infra"
  forge               = "gitlab"
  project             = "acme/infra"
  path                = "infra"
  webhook_secret_file = "/run/secrets/writ/infra_hook"
}
```

`writ serve` reads the file on each delivery and trims surrounding whitespace, so you can rotate the secret without restarting. Update GitLab and the file together; any delivery during the gap is refused and the poll catches up.

### 3. Add the webhook in GitLab

In the project, open **Settings > Webhooks > Add new webhook** and configure:

| Field | Value |
| --- | --- |
| URL | `https://writ.example.com/v1/hooks/gitlab/infra` (`url` + `/v1/hooks/{forge}/{repo}`) |
| Secret token | Contents of `webhook_secret_file` |
| Trigger | **Push events**, **Wildcard pattern** set to the default branch (for example `main`) |
| SSL verification | Enabled |

Leave other triggers off. Merging to the default branch fires a push event, so **Merge request events** isn't needed; pushes to other branches would only cause redundant reads of the default branch.

### 4. Test it

Click **Test > Push events** on the webhook row. Under **Recent events**, GitLab should show `202`. In the control UI, `/ui/repos` displays a `webhook` chip with the last delivery time, and `GET /v1/repos` returns `hook: true` and `hook_at`.

## Use a signing token instead

GitLab 19.1+ supports HMAC signing tokens starting with `whsec_`. Put that token in `webhook_secret_file` instead of a plain secret token. When the file starts with `whsec_`, `writ serve` verifies `webhook-signature` (HMAC-SHA256 of `webhook-id`, `webhook-timestamp`, and the raw body), rejects timestamps more than 5 minutes from its clock, and ignores `X-Gitlab-Token`.

Don't start a self-chosen plain secret token with `whsec_`, or every delivery will fail verification. Because `writ serve` ignores the payload body, the main benefit of a signing token is the 5-minute replay window.

## How often the poll runs with a webhook

Repositories with a webhook poll every `poll_every_with_hook` (5 minutes by default); repositories without one use `poll_every`. Any read, from a webhook or a poll, resets the timer.

```hcl
poll_every           = "60s"
poll_every_with_hook = "10m"
```

`poll_every_with_hook` can't be shorter than `poll_every`. Unset, it defaults to 5 minutes (or `poll_every` if longer). See [Config: Forge and repo](config.md#forge-and-repo).

## What writ serve answers

Deliveries that pass the per-address rate limit write an audit row with `op = hook`. When the URL matches a pinned repository with a webhook, `via` is `webhook of repository NAME`. Principals with `read` on any stack at the repository's current default branch can read its webhook rows; rows for earlier projects, unknown webhooks, or missing project identities require global `admin`. Query them with `writ audit --remote --op hook` or filter by `hook` in `/ui/audit`.

| Status | Body | When |
| --- | --- | --- |
| 202 | `writ.hook/v0`, `queued: true` | Verified push event; a read of the default branch is queued. |
| 200 | `writ.hook/v0`, `duplicate: true` | Duplicate `webhook-id` (or `Idempotency-Key` before GitLab 19.0); not acted on again. |
| 200 | `writ.hook/v0`, `queued: false` | Verified delivery, but not a push event. |
| 401 | class `auth`: `the delivery failed verification` | Verification failed for any reason, including a body over 1 MiB. |
| 429 | class `usage`, `Retry-After: 1` | Over 10 deliveries/s from one client IP (no audit row), or over 5 deliveries/s for one repository (audited). |

Every 401 returns the same message so senders learn nothing. Inspect `check` in the audit row to diagnose failures:

| `check` | Reason |
| --- | --- |
| `hook-body` | Body exceeds 1 MiB or failed to read |
| `hook-unknown` | No pinned repository with a webhook matches that forge and repo name |
| `hook-no-token` | Missing `X-Gitlab-Token` header |
| `hook-token` | `X-Gitlab-Token` doesn't match the secret |
| `hook-secret-file` | Secret file is unreadable, not a regular file <= 4 KiB, or has invalid base64 after `whsec_` (see server log) |
| `hook-no-signature` | Missing `webhook-id`, `webhook-timestamp`, or `webhook-signature` under a signing token |
| `hook-timestamp` | `webhook-timestamp` is more than 5 minutes from server time |
| `hook-signature` | Signature doesn't match the raw body |

GitLab treats any 4xx response as a failure. Normal project traffic stays far below these rate limits, so a 429 indicates unexpected traffic to the endpoint.

## Security notes

- Secret tokens are compared in constant time via SHA-256 digests, hiding both value and length.
- Secrets and failed candidate tokens never appear in logs, audit rows, API responses, or the control UI.
- Webhooks are unauthenticated by bearer token; the per-IP rate limit runs before reading the body.
- Bodies are capped at 1 MiB and never stored or logged.
- The newest 4,096 delivery IDs are deduplicated in memory. After a restart, a resent delivery triggers at most one extra head read.
- A delivery never triggers plans or applies and never chooses what to fetch; `writ serve` always reads the stored project ID and default branch using the forge's read token.

## Troubleshooting

- **GitLab shows 401**: Run `writ audit --remote --op hook` and check the `check` column for the exact cause (wrong secret, wrong URL, missing `webhook_secret_file`, or unreadable file).
- **GitLab disabled the webhook**: Fix the error, then click **Test > Push events**. A 2xx response re-enables it.
- **Head updates only every few minutes**: Check **Recent events** in GitLab. If no delivery fired, verify the push trigger's branch wildcard. Polling still updates the head every `poll_every_with_hook`.

## Limits

- GitLab only; GitHub's `X-Hub-Signature-256` isn't implemented.
- Repositories added with `writ repo add` can't use webhooks.

## Next steps

- [Add a repository](add-repository.md): forge tokens, stack rules, and repository config.
- [API: webhook route](api.md#webhook-route): route reference.
- [Read the audit log](audit-log.md): query webhook and other audit events.
