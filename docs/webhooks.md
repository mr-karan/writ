# Get changes in seconds with a webhook

Add a GitLab or GitHub webhook so `writ serve` reads a pushed or merged head within seconds, instead of at the next poll (`poll_every`, 60 seconds by default). Polling continues every `poll_every_with_hook` (5 minutes by default) as a backstop, because GitLab disables a webhook for a while after 4 consecutive failures, and for good after 40.

A webhook only triggers an early read of the default branch through the forge's API, with the forge's read token. It never starts a plan or apply. `writ serve` never trusts the webhook body: the repository comes from the URL path, and the commit from the forge's API. It parses a GitHub body's `ref` and `after` only to tell a push from another event, and never parses a GitLab body.

## Before you begin

- The repository is pinned with a `repo` block in `/etc/writ/serve`. Only pinned repositories take webhooks, because server config governs the secret path. Repositories added with `writ repo add` only poll.
- The forge can reach `writ serve`'s `url` over HTTPS.
- You have Maintainer or Owner access on the GitLab project, or admin access on the GitHub repository.

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

`writ serve` reads the file on each delivery and trims surrounding whitespace, so you can rotate the secret without a restart. Update GitLab and the file together; a delivery in the gap is refused, and the poll catches up.

### 3. Add the webhook in GitLab

In the project, open **Settings > Webhooks > Add new webhook** and configure:

| Field | Value |
| --- | --- |
| URL | `https://writ.example.com/v1/hooks/gitlab/infra` (`url` + `/v1/hooks/{forge}/{repo}`) |
| Secret token | Contents of `webhook_secret_file` |
| Trigger | **Push events**, **Wildcard pattern** set to the default branch (for example `main`) |
| SSL verification | Enabled |

Leave other triggers off. A merge to the default branch fires a push event, so **Merge request events** is not needed, and pushes to other branches only cause redundant reads.

### 4. Test it

Click **Test > Push events** on the webhook row. Under **Recent events**, GitLab should show `202`. In the control UI, `/ui/repos` displays a `webhook` chip with the last delivery time, and `GET /v1/repos` returns `hook: true` and `hook_at`.

### On GitHub

In the repository, open **Settings > Webhooks > Add webhook** and configure:

| Field | Value |
| --- | --- |
| Payload URL | `https://writ.example.com/v1/hooks/github/web` (`url` + `/v1/hooks/{forge}/{repo}`, where `{forge}` is the `forge` block's name) |
| Content type | `application/json` |
| Secret | Contents of `webhook_secret_file` |
| SSL verification | Enabled |
| Events | **Just the push event** |

`writ serve` verifies `X-Hub-Signature-256`: `sha256=` and the hex HMAC-SHA256 of the raw body under the secret, compared in constant time. A missing signature is check `hook-no-signature`, a wrong one `hook-signature`; `X-Hub-Signature` (SHA-1) is ignored. A delivery queues a read when `X-GitHub-Event` is `push` and its signed body is a push's (a JSON object with `ref` and `after`), so a captured `ping` sent again as a push reads nothing. GitHub's `ping`, sent when the webhook is made, answers `200` and reads nothing. A signed push whose body lacks `ref` or `after` answers `200` and reads nothing. Deliveries are deduplicated by the SHA-256 of the signed body, since `X-GitHub-Delivery` is not signed: GitHub's own resend, and a captured delivery sent again under any delivery ID, answer `200` with `duplicate: true` while the body is among the newest 4,096 kept. GitHub signs no timestamp, so after a restart, or once the body has left that cache, a captured delivery passes verification again; it can only make `writ serve` read the head sooner, within the per-repository rate limit.

On the webhook's **Recent Deliveries** tab, the ping shows `200` and a push `202`.

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

A verified push gets `202`; a duplicate or a non-push event gets `200`; every verification failure gets the same `401` (class `auth`, `the delivery failed verification`), including a body over 1 MiB; over 10 deliveries a second from one IP or 5 for one repository gets `429`. Each delivery that passes the per-address limit writes an `op = hook` audit row whose `check` names the cause of a failure. Query them with `writ audit --remote --op hook`. The full table, check codes, and security rules are in [Webhook route](api.md#webhook-route).

## Troubleshooting

| Problem | Fix |
| --- | --- |
| GitLab or GitHub shows 401 | Run `writ audit --remote --op hook`. The `check` column names the cause: wrong secret, wrong URL, missing `webhook_secret_file`, or an unreadable file. |
| GitLab disabled the webhook | Fix the error, then click **Test > Push events**. A 2xx response re-enables it. |
| The head updates only every few minutes | Check **Recent events** in GitLab. If nothing fired, check the push trigger's branch wildcard. Polling still updates the head every `poll_every_with_hook`. |

## Limits

- Repositories added with `writ repo add` can't use webhooks.

## Next steps

- [Add a repository](add-repository.md): forge tokens, stack rules, and repository config.
- [API: webhook route](api.md#webhook-route): route reference.
- [Read the audit log](audit-log.md): query webhook and other audit events.
