# Get changes in seconds with a webhook

`writ serve` polls each repository's default branch every `poll_every`, 60 seconds by default. Add a GitLab webhook and GitLab tells `writ serve` the moment you push or merge, so it reads the new head within seconds. The poll keeps running, every 5 minutes for that repository, as a backstop: GitLab turns off a webhook that fails four times in a row, for a while, and for good after 40, so a delivery can be lost.

A delivery only makes `writ serve` read the default branch's head sooner, through the forge's API and its read token, exactly as the poll does. It never plans or applies. `writ serve` doesn't parse the delivery's body: the repository is the one the URL names, and the head is whatever GitLab's API says, never a commit, branch, project, or URL from the payload.

## Before you begin

- The repository is pinned with a `repo` block in your config. Only a pinned repository can have a webhook, because its secret lives in a file the config points to, and the config's review covers it. A repository added with `writ repo add` is polled and has no webhook.
- GitLab can reach `writ serve` at its `url` over HTTPS.
- You can change the project's webhooks in GitLab: the Maintainer role or above.

## Set up a webhook

### 1. Create the secret

Make a long random secret in a file that only `writ serve`'s user can read:

```
openssl rand -hex 32 > /run/secrets/writ/infra_hook
chmod 0400 /run/secrets/writ/infra_hook
```

On NixOS, deliver it with sops-nix or agenix like the forge's read token.

### 2. Point the repository at it

Add `webhook_secret_file` to the repository's `repo` block, and restart `writ serve`:

```hcl
repo {
  name                = "infra"
  forge               = "gitlab"
  project             = "acme/infra"
  path                = "infra"
  webhook_secret_file = "/run/secrets/writ/infra_hook"
}
```

`writ serve` reads the file for each delivery and trims the whitespace around the secret, so you can rotate the secret without a restart. Change it in GitLab and in the file together: a delivery between the two changes is refused, and the poll catches up.

### 3. Add the webhook in GitLab

In the project, open **Settings > Webhooks > Add new webhook**, and set:

| Field | Value |
| --- | --- |
| URL | `https://writ.example.com/v1/hooks/gitlab/infra`: your `url`, then `/v1/hooks/`, the `forge` block's name, `/`, and the `repo` block's name |
| Secret token | The secret in `webhook_secret_file` |
| Trigger | **Push events**, with **Wildcard pattern** set to the default branch, such as `main` |
| SSL verification | On |

Leave every other trigger off. A merge to the default branch is a push event, so you don't need **Merge request events**. A push to another branch would only make `writ serve` read the default branch's head once more for nothing.

### 4. Test it

On the webhook's row, select **Test > Push events**. GitLab shows the answer under **Recent events**: `202` when `writ serve` checked the secret and queued a read. The Repositories page then shows a `webhook` chip with the time of the last delivery, and the repository's page shows **Webhook** with `last delivery`. `GET /v1/repos` has `hook: true` and `hook_at`.

## Use a signing token instead

GitLab 19.1 and later can sign each delivery with a signing token, which GitLab shows starting with `whsec_`. Put that token in `webhook_secret_file` in place of a secret token. When the file starts with `whsec_`, `writ serve` checks the delivery's `webhook-signature` header, the HMAC-SHA256 of `webhook-id`, `webhook-timestamp`, and the raw body under the token's key, and refuses a delivery whose `webhook-timestamp` is more than 5 minutes from its clock. It then ignores `X-Gitlab-Token`.

A signing token proves the body too, and the timestamp stops a recorded delivery from being sent again later. `writ serve` doesn't read the body, so for Writ the gain is the replay window, not the body.

## How often the poll runs with a webhook

A repository with a webhook is polled every `poll_every_with_hook`, 5 minutes by default. A repository without one keeps `poll_every`. Any read, by a delivery or the poll, starts the wait again.

```hcl
poll_every           = "60s"
poll_every_with_hook = "10m"
```

`poll_every_with_hook` is never shorter than `poll_every`: a webhook only makes the poll rarer. Unset, it's 5 minutes, or `poll_every` when that's longer. See [Config](config.md#forge-and-repo).

## What writ serve answers

Every delivery gets an audit row, op `hook`, which only a principal with `admin` on every stack in every environment can read, as with the other rows of no environment. Read them with `writ audit --remote --op hook`.

| Status | Body | When |
| --- | --- | --- |
| 202 | `writ.hook/v0`, `queued: true` | The delivery passed its check and is a push event. A read of the default branch's head is queued. |
| 200 | `writ.hook/v0`, `duplicate: true` | GitLab sent this delivery already: the same `webhook-id`, or `Idempotency-Key` before GitLab 19.0. It's not acted on again. |
| 200 | `writ.hook/v0`, `queued: false` | The delivery passed its check, but isn't a push event, so nothing is read. |
| 401 | class `auth`: `the delivery failed verification` | The delivery didn't prove it came from the repository's Git host, for any reason. |
| 413 | class `usage` | The body is over 1 MiB. |
| 429 | class `usage`, `Retry-After: 1` | More than 10 deliveries a second from one client address, or more than 5 for one repository. |

A 401 says the same words whatever the reason, so a sender learns nothing. The audit row's `check` says which:

| `check` | Reason |
| --- | --- |
| `hook-unknown` | No pinned repository of that name has a webhook at that forge |
| `hook-no-token` | No `X-Gitlab-Token` header |
| `hook-token` | `X-Gitlab-Token` isn't the secret |
| `hook-secret-file` | `writ serve` couldn't read the secret file, or a `whsec_` token in it isn't base64. The log says why. |
| `hook-no-signature` | Under a signing token, no `webhook-id`, `webhook-timestamp`, or `webhook-signature` |
| `hook-timestamp` | Under a signing token, `webhook-timestamp` is more than 5 minutes from now |
| `hook-signature` | Under a signing token, the signature isn't the body's |

GitLab counts every 4xx answer as a failure. The limits are well above what one project sends, so a 429 means something else sends to the URL.

## Security notes

- The secret is compared in constant time: `writ serve` compares the SHA-256 digests of what came and of the secret, so the time a comparison takes says nothing of either, not even the length.
- The secret, and any wrong token a sender tries, never appear in a log line, an audit row, an API answer, or the control UI. `TestHookSecretNeverLeaks` and the control UI's page walk search all four.
- A delivery is unauthenticated by token: the secret is its proof. The rate limit on its client address comes first, before the body is read.
- `writ serve` never stores or logs a delivery's body. It reads at most 1 MiB of it, to bound it and to check a signature.
- Delivery ids are kept in memory, the newest 4,096. After a restart, GitLab's resend of an earlier delivery is acted on once more, which costs one read of the branch's head and changes nothing else.
- A delivery never starts a plan or an apply, and never changes what `writ serve` reads: the read uses the repository's project id and default branch from the control database, and the forge's read token.

## Troubleshooting

- **GitLab shows 401.** The secret token in GitLab differs from the file, the URL names another forge or repository, or the `repo` block has no `webhook_secret_file`. Find the row with `writ audit --remote --op hook` and read its `check`.
- **GitLab disabled the webhook.** Fix the cause, then select **Test > Push events**: a 2xx answer turns it back on.
- **The head moves only every few minutes.** Look for the delivery in GitLab's **Recent events**. With no event, check the trigger's branch pattern. The poll still reads the head every `poll_every_with_hook`.

## Limits

- GitLab is the only Git host with webhooks, as it's the only one Writ reads. GitHub's `X-Hub-Signature-256` isn't built.
- A repository added with `writ repo add` can't have a webhook.

## Next steps

- [How repositories work](repos.md#how-the-control-plane-learns-about-commits): how polls and CI jobs bring commits in.
- [API: webhook route](api.md#webhook-route): the route and its answers.
- [Read the audit log](audit-log.md).
