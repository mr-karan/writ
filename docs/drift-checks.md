# Enable drift checks

Run scheduled drift checks so Writ alerts you in the control UI and API when real infrastructure changes outside OpenTofu or when your default branch has unapplied code.

For how drift checks fit into the control plane, see [How the control plane works](serve.md#drift-checks) and [Glossary](glossary.md).

## Prerequisites

- A team's `writ serve` with a [`tofu` block](config.md#tofu) and `provider_uids`.
- At least one repository added so Writ knows each stack's default branch.
- `limits.plans` set to `2` or higher. Drift checks never take the last free plan slot, leaving it for people and CI jobs.
- For stacks on S3, an environment `plan_role` with read access to the state bucket and your cloud resources.

## Steps

### 1. Add a `drift` block to your server config

Open your `writ serve` config file (for example `/etc/writ/serve`) and add a `drift` block next to `limits`. Set `every` to at least `1m`, and make sure `limits.plans` is at least `2`:

```hcl
limits {
  plans = 8
}

drift {
  every = "6h"
}
```

Each check reads every resource in a stack through its providers using the environment's `plan_role`. Choose `every` to match your cloud API budget: a stack of 200 resources checked every 6 hours makes about 800 cloud API reads a day.

### 2. Validate the config and restart `writ serve`

Check the config before you restart:

```sh
writ serve --config /etc/writ/serve --check
```

`writ serve --check` fails if `drift` has no `every`, if `every` is under `1m`, if there is no `tofu` block, or if `limits.plans` is below `2`.

Once `--check` passes, restart `writ serve`.

### 3. Read drift results

Writ checks every OpenTofu `stack@env` that has state on the `http` backend, in the state index, or read from S3. Checks run at server start and then every quarter of `every` (at most every 30 seconds). A stack is also checked after an apply finishes, after Writ sees a state written outside Writ on S3, or after someone [adopts](adopt-state.md) its state.

You can read the latest results in two places:

- **Control UI**: Open the Overview (`/ui/`) to see the count of drifted stacks and drift alerts, or open a stack's Drift tab (`/ui/stack?stack=STACK&env=ENV&view=drift`).
- **API**: Query `GET /v1/drift` with a token that has `read` on the stack:

```sh
curl -sS "https://writ.example.com/v1/drift?env=prod&stack=app" \
  -H "Authorization: Bearer $TOKEN"
```

### What you should see

`GET /v1/drift` returns a `writ.drift/v0` JSON document with the newest completed check for each readable stack. Drift checks separate changes into two kinds:

- **`outside`**: resources that changed outside OpenTofu (for example, edited in a cloud console or deleted).
- **`code`**: changes that applying the default branch head would make (for example, a merged commit not yet applied, or an S3 state modified outside Writ).

Values are never stored in drift results, only resource addresses, actions, and changed top-level attribute names, swept by the [mask](secrets.md):

```json
{
    "diagnostics": [],
    "format": "writ.drift/v0",
    "more": false,
    "ok": true,
    "stacks": [
        {
            "backend": "http",
            "check": {
                "changes": [
                    {
                        "action": "create",
                        "address": "terraform_data.keyed[\"(sensitive)\"]",
                        "kind": "code"
                    },
                    {
                        "action": "delete",
                        "address": "terraform_data.keyed[\"(sensitive)\"]",
                        "kind": "code"
                    }
                ],
                "checked_at": "2026-11-15T06:22:51Z",
                "code": 2,
                "commit": "dda65cefe05a83f1433a335c880fdfb4a1d7a51d",
                "drifted_since": "2026-11-15T06:22:51Z",
                "error": "",
                "outside": 0,
                "plan": "703a6a51ed7d286e9bdbe4f62eeaac6d",
                "repo": "infra",
                "started_at": "2026-11-15T06:22:51Z",
                "status": "drifted"
            },
            "checking": false,
            "env": "prod",
            "indexed_at": "2026-11-15T06:22:50Z",
            "indexed_version": 3,
            "outside_write": null,
            "resources": 5,
            "stack": "app"
        }
    ]
}
```

Output of `just e2e`, 2026-11-15, indented with `python3 -m json.tool`.

When a later check finds no changes, Writ clears the drifted mark. Writ keeps the newest 20 checks per `stack@env`.

## Troubleshooting

- **Check fails with `drift unknown: no secret set for state version N of stack@env`**: The state was written outside Writ (or before Writ sealed secret sets). Adopt the state with [`writ state adopt`](adopt-state.md) so drift checks and plans can read it again.
- **`local_file` or `local_sensitive_file` always reports as deleted outside OpenTofu**: Each run uses a fresh temporary directory, so files written by an earlier apply are not present on disk.
- **Trying to approve or apply a drift check plan fails with `409 state`**: Drift check plans keep no [saved plan](glossary.md) and cannot be approved or applied. Run `writ plan --remote` on the stack to create an approvable plan.
- **No check starts while an apply runs**: If an apply is running on a `stack@env`, Writ skips that stack until the apply finishes. If an apply runs during a check, Writ discards the check's result and re-queues the stack.
- **No `drift` results after editing config**: Make sure you restarted `writ serve` and that your token has `read` on the stack's environment.

## Limits

- Writ does not send alerts or notifications (Slack, email, webhooks) when drift is found. Read drift in the control UI, `GET /v1/drift`, or the server's JSON log (`drift found` at `WARN` level).
- No [policy](policy.md) evaluates drift checks.
- Drift checks run against the head of the default branch as Writ last polled it (`poll_every`).

## Next steps

- [Adopt existing state](adopt-state.md): recover a stack whose state was modified outside Writ.
- [Read the audit log](audit-log.md): trace who changed a stack and when.
- [AWS roles and credentials](aws-credentials.md): configure read-only `plan_role` permissions.
