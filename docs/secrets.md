# Secrets and the mask

`tofu plan` and `tofu apply` print sensitive variable values in plan JSON and provider errors. `writ serve` never stores raw OpenTofu output; every document leaving a run passes through Writ's mask.

Read [What the mask can't do](#what-the-mask-cant-do) before relying on it for sensitive stacks.

## How the mask works

The mask produces every output stored or shown from a run:

- Masked plan documents (API, control UI, CLI, policy input).
- Plan text, apply text, and apply events.
- [State index](plan-lifecycle.md#the-state-index) rows.
- Failure diagnostics from plans and applies.
- Recorded hostnames under [egress](run-isolation.md#egress).

Each run gets its own mask, loaded with:

- the run's environment values (`provider_env`, dev `TF_VAR_*`);
- the state version's sealed secret set;
- every temporary AWS `SecretAccessKey` and `SessionToken` the [credential broker](aws-credentials.md#how-credentials-reach-providers-and-cloudtrail) issued, refreshed before each sweep. `AccessKeyId` stays visible for CloudTrail lookups, and temporary credentials are never saved in state secret sets.

It then applies four rules.

### Rule 1: OpenTofu's marks

Any value under a sensitive mark (before or after) becomes `(sensitive)`. The mask learns those values and every variable declared `sensitive = true` (all strings, and numbers of 6 or more characters; booleans and shorter numbers are hidden only where marked). Changed sensitive paths are listed so reviewers know a secret changed without seeing its value.

### Rule 2: a sweep by value

The mask sweeps every string, map key, and number for every learned secret in plain form as well as URL-escaped, JSON-escaped, hex, and base64 encodings. This catches values unmarked via `nonsensitive()` (for example `nonsensitive(base64encode(var.token))`). Strings shorter than 6 bytes are replaced only when they match a whole value.

Structural fields set only by HCL config (versions, variable/output names, provider/module definitions, resource types, names, actions, action reasons, and the first step of replace paths) are excluded from value sweeping so a secret spelling `delete` can't corrupt policy inputs or risk classes. Resource instance index keys are parsed and swept as decoded values (`["(sensitive)"]`), because data can populate them.

### Rule 3: names

Any attribute or key whose name contains words such as `password`, `secret`, or `token` becomes `(sensitive)` regardless of marks.

Auth-method token settings (`token` followed at the end of the name by `ttl`, `max_ttl`, `explicit_max_ttl`, `period`, `num_uses`, `locality`, `name_format`, `type`, `policies`, `bound_cidrs`, or `no_default_policy`) are exempted from name masking but still go through Rules 1 and 2. `api_token`, `token_value`, and `tokenless` remain masked by name.

### Rule 4: an allow-list of keys

Masked plan documents retain only explicitly allow-listed keys; unknown keys are dropped.

### When the mask keeps less

Two fail-closed limits drop the entire document rather than risk a leak: distinct learned secrets exceeding 4 MiB, or decoding/matching a single string costing over 32 MiB.

If a plan fails before JSON output exists, Writ keeps only masked diagnostic severity, summary, file, line, and column, never details or snippets, which `error_message` with `nonsensitive()` can populate. On S3 stacks, a refused plan saved no plan so Writ doesn't know which serial it read: its mask loads every secret set Writ keeps for that `stack@env` (failing the plan if distinct secrets exceed 4 MiB), and quoted instance keys (`["KEY"]` and `[\"KEY\"]`) in failure summaries become `["(sensitive)"]`.

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
| A non-secret field shows `(sensitive)` | Its name matched Rule 3 (`password`, `secret`, `token`, and so on). This is on purpose. |
| `no secret set for state version N` | The state was written outside Writ, before secret sets, or by an apply whose mask could not read its plan. Inspect the stack code and run [`writ state adopt`](adopt-state.md). |
| A derived secret appeared in output | See [What the mask can't do](#what-the-mask-cant-do). Remove `nonsensitive()` around hashed or multi-line base64 values, and mark source variables `sensitive = true`. |

## Next steps

- [Adopt a state](adopt-state.md): establish secret sets for externally written state.
- [Security model](security-model.md): how masking fits with credential isolation and audit logging.
- [Run isolation and egress](run-isolation.md): run execution and filesystem view.
- [Approvals](plan-lifecycle.md#apply-point-checks-and-execution): masked plan review and apply.
