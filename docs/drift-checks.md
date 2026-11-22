# Enable drift checks

Schedule drift checks to find cloud changes made outside OpenTofu and default-branch commits not yet applied. Results show in the control UI and API, and optionally in Slack or webhook notifications.

## Prerequisites

- A team's `writ serve` with a [`tofu` block](config.md#tofu) and `provider_uids`.
- At least one registered repository so Writ knows each stack's default branch.
- `limits.plans` set to `2` or higher (drift checks never take the last free plan slot).
- For S3 stacks, an environment `plan_role` with read access to the state bucket and cloud resources.

## Steps

### 1. Add a `drift` block to your server config

Add `drift` alongside `limits` in `/etc/writ/serve` (`every` must be at least `1m`, and `limits.plans` at least `2`):

```hcl
limits {
  plans = 8
}

drift {
  every = "6h"
}
```

Each check refreshes every resource in the stack with `plan_role`. Size `every` for your cloud API budget: 200 resources every 6 hours is about 800 reads a day. For alerts, add a [`notify` block](notifications.md).

### 2. Validate the config and restart `writ serve`

```sh
writ serve --config /etc/writ/serve --check
```

`--check` fails if `every` is missing or below `1m`, if no `tofu` block is configured, or if `limits.plans < 2`. Restart `writ serve` once it passes.

### 3. Read drift results

Checks start at startup and then on the schedule ([When drift checks run](config.md#when-drift-checks-run)). Results show in:

- **Control UI:** Overview (`/ui/`) shows drifted stack counts and alerts; `/ui/stack?stack=STACK&env=ENV&view=drift` lists recent checks.
- **API:** `GET /v1/drift` with `read` on the stack:

```sh
curl -sS "https://writ.example.com/v1/drift?env=prod&stack=app" \
  -H "Authorization: Bearer $TOKEN"
```

- **Notifications:** Slack or webhook alerts for `drift` and `outside-write` events ([Get notified](notifications.md)).

### What you should see

`GET /v1/drift` returns `writ.drift/v0` with the latest completed check per readable stack, separating changes into:

- **`outside`**: resources modified or deleted outside OpenTofu.
- **`code`**: changes applying the default branch head would make (unapplied merges or external S3 state writes).

Drift results never store attribute values, only resource addresses, actions, and changed top-level attribute names passed through [the mask](secrets.md):

```json
{"format":"writ.drift/v0","ok":true,"stacks":[{"stack":"app","env":"prod","backend":"http","check":{"status":"drifted","outside":0,"code":2,"changes":[{"action":"create","address":"terraform_data.keyed[\"(sensitive)\"]","kind":"code"}, ...]}}]}
```

Excerpt of a real run, 2026-11-15; every field is in [JSON output](json-output.md#documents-of-writ-serve).

A clean check clears the drifted flag. Writ keeps the newest 20 checks per `stack@env`; `GET /v1/drift?env=ENV&stack=STACK` returns them under `checks`, each with a `ref`.

### Open one drift check

`/ui/drift/REF` shows a single check (timing, `stack@env`, default-branch commit, outside vs code changes, and any error or `writ state adopt` command). `/ui/plans/PLAN` for a drift check redirects here. Both `/ui/drift/REF` and `GET /v1/drift/REF` (`writ.drift-check/v0`) require `read` on the stack.

## Troubleshooting

| Problem | Fix |
| --- | --- |
| `drift unknown: no secret set for state version N of stack@env` | The state was written outside Writ. Run [`writ state adopt`](adopt-state.md). |
| `local_file` or `local_sensitive_file` always shows as deleted outside OpenTofu | Each run uses a fresh temporary directory, so files from earlier applies are not on disk. |
| `409 state` when approving or applying a drift check plan | Drift checks keep no saved plan. Run `writ plan --remote` for an approvable plan. |
| No check starts during an apply | Writ skips stacks with active applies. If an apply starts mid-check, Writ discards the result and queues the check again. |
| Empty `GET /v1/drift` after a config edit | Restart `writ serve`, and check that your token has `read` on the stack's env. |

## Limits

- Built-in alert channels are Slack and signed HTTP webhooks ([Get notified](notifications.md)); email isn't built. Drift findings also appear in the control UI, `GET /v1/drift`, and server JSON logs (`drift found` at `WARN`).
- [Policies](write-a-policy.md) don't evaluate drift checks.
- Drift checks use the default branch head as last read by polling or webhooks.
- Drift checks don't record per-run egress network lists.

## Next steps

- [Get notified](notifications.md): send Slack or webhook alerts on drift and outside writes.
- [Adopt a state](adopt-state.md): recover a stack whose state changed outside Writ.
- [Read the audit log](audit-log.md): trace who changed a stack and when.
- [AWS credentials](aws-credentials.md): configure read-only `plan_role` permissions.
