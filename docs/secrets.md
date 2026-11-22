# Secrets and the mask

`tofu plan` and `tofu apply` print sensitive variable values in plan JSON and provider errors. `writ serve` never stores raw OpenTofu output; every document leaving a run passes through Writ's mask.

> **Warning:** read [What the mask can't do](#what-the-mask-cant-do) before relying on it for sensitive stacks.

## Keep secrets out of output

1. Declare every secret variable `sensitive = true`. The mask learns every string value, and every number of 6 or more characters, and scrubs it in plain and encoded forms. Shorter numbers and booleans are hidden only where marked, and strings under 6 bytes are replaced only on a whole-value match ([Rule 1](how-the-mask-works.md#rule-1-opentofus-marks), [Rule 2](how-the-mask-works.md#rule-2-a-sweep-by-value)).
2. Pass provider credentials and other secrets through `provider_env` files, not literals. The mask learns those values too ([AWS roles and credentials](aws-credentials.md)).
3. Do not wrap hashed, encoded, or derived secret values in `nonsensitive()`. The mask cannot invert them.
4. Review a plan: a secret the mask knows shows as `(sensitive)`, and changed sensitive paths are listed so reviewers see that a secret changed.

The mask produces every masked plan, plan and apply text, state index row, diagnostic, and recorded egress host. Its four rules, its inputs, and when it drops a whole document are in [How the mask works](how-the-mask-works.md).

## What the mask can't do

The mask is an output filter. It catches a secret only if OpenTofu marks it, the mask learned its value, or its attribute name matches Rule 3. Seven cases remain outside its reach:

- **Unmarked secret in an unmarked attribute:** A non-sensitive variable written to a non-sensitive attribute looks like plain text.
- **Transformed secret passed to `nonsensitive()`:** `nonsensitive(sha256(var.token))` produces a derived value the mask can't invert.
- **Line-wrapped base64:** Base64 decoding runs per line; secrets >= 6 bytes whose base64 wraps across lines (like a 64-column PEM body) aren't matched unless marked.
- **Reformatted numbers:** Numbers match by literal text (`150000` matches `150000.0`, not `1.5e5` or `150,000`).
- **Prior value of an unmarked `nonsensitive()` attribute:** On update, `before` is swept only against known secrets (the state's secret set, values marked in the plan, and run env).
- **Unmarked provider return value after apply:** Sealed in state, but visible in the state index if the provider puts it in an indexed attribute such as `id`.
- **Refused S3 plan against a state with no secret set:** The mask knows nothing of a state with no secret set. If a refused plan's summary quotes a value from such a state outside a quoted instance key (for example an attribute set through `nonsensitive()`, an attribute marked sensitive in state, or a name built from `each.key`), or quotes a key three times or more, that value shows.

Declare every secret variable `sensitive = true`, and avoid `nonsensitive()` on derived values. The mask is checked by differential and fuzz tests, not formally proven, and those tests do not cover policy messages, notifications, or control UI HTML.

## Troubleshooting

| Problem | Fix |
| --- | --- |
| A plan or run stores no document | The run's distinct learned secrets passed 4 MiB, or one string cost more than 32 MiB to decode, so the mask failed closed. Move large sensitive blobs out of inline variables. |
| A non-secret field shows `(sensitive)` | Its name matched [Rule 3](how-the-mask-works.md#rule-3-names) (`password`, `secret`, `token`, and so on). This is on purpose. |
| `no secret set for state version N` | The state was written outside Writ, before secret sets, or by an apply whose mask could not read its plan. Inspect the stack code and run [`writ state adopt`](adopt-state.md). |
| A derived secret appeared in output | See [What the mask can't do](#what-the-mask-cant-do). Remove `nonsensitive()` around hashed or multi-line base64 values, and mark source variables `sensitive = true`. |

## Next steps

- [Adopt a state](adopt-state.md): establish secret sets for externally written state.
- [Security model](security-model.md): how masking fits with credential isolation and audit logging.
- [Run isolation and egress](run-isolation.md): run execution and filesystem view.
- [Apply-point checks](plan-lifecycle.md#apply-point-checks-and-execution): masked plan review and apply.
