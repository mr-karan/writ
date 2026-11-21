# How the mask works

The rules Writ's mask applies to every output of an OpenTofu run, and when it drops a document instead. What it cannot catch, and how to keep secrets safe, is in [Secrets and the mask](secrets.md).

The mask produces every output stored or shown from a run:

- Masked plan documents (API, control UI, CLI, policy input).
- Plan text, apply text, and apply events.
- [State index](plan-lifecycle.md#the-state-index) rows.
- Failure diagnostics from plans and applies.
- Recorded hostnames under [egress](run-isolation.md#egress).

Each run gets its own mask, loaded with:

- the run's environment values (`provider_env`, dev `TF_VAR_*`);
- the state version's sealed secret set;
- every temporary AWS `SecretAccessKey` and `SessionToken` the [credential broker](how-aws-credentials-work.md#how-credentials-reach-providers-and-cloudtrail) issued, refreshed before each sweep. `AccessKeyId` stays visible for CloudTrail lookups, and temporary credentials are never saved in state secret sets.

It then applies four rules.

## Rule 1: OpenTofu's marks

Any value under a sensitive mark (before or after) becomes `(sensitive)`. The mask learns those values and every variable declared `sensitive = true` (all strings, and numbers of 6 or more characters; booleans and shorter numbers are hidden only where marked). Changed sensitive paths are listed so reviewers know a secret changed without seeing its value.

## Rule 2: a sweep by value

The mask sweeps every string, map key, and number for every learned secret in plain form as well as URL-escaped, JSON-escaped, hex, and base64 encodings. This catches values unmarked via `nonsensitive()` (for example `nonsensitive(base64encode(var.token))`). Strings shorter than 6 bytes are replaced only when they match a whole value.

Structural fields set only by HCL config (versions, variable/output names, provider/module definitions, resource types, names, actions, action reasons, and the first step of replace paths) are excluded from value sweeping so a secret spelling `delete` can't corrupt policy inputs or risk classes. Resource instance index keys are parsed and swept as decoded values (`["(sensitive)"]`), because data can populate them.

## Rule 3: names

Any attribute or key whose name contains words such as `password`, `secret`, or `token` becomes `(sensitive)` regardless of marks.

Auth-method token settings (`token` followed at the end of the name by `ttl`, `max_ttl`, `explicit_max_ttl`, `period`, `num_uses`, `locality`, `name_format`, `type`, `policies`, `bound_cidrs`, or `no_default_policy`) are exempted from name masking but still go through Rules 1 and 2. `api_token`, `token_value`, and `tokenless` remain masked by name.

## Rule 4: an allow-list of keys

Masked plan documents retain only explicitly allow-listed keys; unknown keys are dropped.

## When the mask keeps less

Two fail-closed limits drop the entire document rather than risk a leak: distinct learned secrets exceeding 4 MiB, or decoding/matching a single string costing over 32 MiB.

If a plan fails before JSON output exists, Writ keeps only masked diagnostic severity, summary, file, line, and column, never details or snippets, which `error_message` with `nonsensitive()` can populate. On S3 stacks, a refused plan saved no plan so Writ doesn't know which serial it read: its mask loads every secret set Writ keeps for that `stack@env` (failing the plan if distinct secrets exceed 4 MiB), and quoted instance keys (`["KEY"]` and `[\"KEY\"]`) in failure summaries become `["(sensitive)"]`.
