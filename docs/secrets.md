# Secrets and the mask

`tofu plan` and `tofu apply` print sensitive variable values in plan JSON and provider errors. `writ serve` never stores raw OpenTofu output; every document leaving a run passes through a single masking package (`internal/mask`).

Read [What the mask can't do](#what-the-mask-cant-do) before relying on it for sensitive stacks.

## How the mask works

`internal/mask` produces every output stored or shown from a run:

- Masked plan documents (API, control UI, CLI, policy input).
- Plan text, apply text, and apply events.
- [State index](serve.md#the-state-index) rows.
- Failure diagnostics from plans and applies.
- Recorded hostnames under [egress](serve.md#egress).

Each run gets its own mask initialized with the run's environment values (`provider_env`, dev `TF_VAR_*`) and every temporary AWS `SecretAccessKey` and `SessionToken` issued by the [credential broker](serve.md#roles-and-credentials) (refreshed before each sweep; `AccessKeyId` stays visible for CloudTrail lookup and temporary credentials are never saved in state secret sets). The mask then applies four rules.

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

If a plan fails before JSON output exists, Writ keeps only masked diagnostic severity, summary, file, line, and column, never details or snippets, which `error_message` with `nonsensitive()` can populate. On S3 stacks, quoted instance keys in failure summaries also become `["(sensitive)"]` ([What a run can see](serve.md#what-a-run-can-see)).

`internal/archtest` enforces that no code outside `internal/tofu` reads raw run state except as input to `mask.Index`.

## What the mask can't do

The mask is an output filter. It catches a secret only if OpenTofu marks it, the mask learned its value, or its attribute name matches Rule 3. Seven cases remain outside its reach:

- **Unmarked secret in an unmarked attribute:** A non-sensitive variable written to a non-sensitive attribute looks like plain text.
- **Transformed secret passed to `nonsensitive()`:** `nonsensitive(sha256(var.token))` produces a derived value the mask can't invert.
- **Line-wrapped base64:** Base64 decoding runs per line; secrets >= 6 bytes whose base64 wraps across lines (like a 64-column PEM body) aren't matched unless marked.
- **Reformatted numbers:** Numbers match by literal text (`150000` matches `150000.0`, not `1.5e5` or `150,000`).
- **Prior value of an unmarked `nonsensitive()` attribute:** On update, `before` is swept only against known secrets (the state's secret set, values marked in the plan, and run env).
- **Unmarked provider return value after apply:** Sealed in state, but visible in the state index if the provider puts it in an indexed attribute such as `id`.
- **Refused S3 plan against a state with no secret set:** A value from that state quoted outside a quoted instance key, or a key quoted three times or more, shows in the summary ([What a run can see](serve.md#what-a-run-can-see)).

Declare all secret variables `sensitive = true` and avoid `nonsensitive()` on derived values.

## How it's tested

`just verify-mask` runs a differential test against real `writ serve --dev` and OpenTofu 1.13.9 inside isolated Linux namespaces (loopback only):

```sh
just verify-mask            # 6 generated stacks, then 30 s of fuzzing
just verify-mask 20 2m      # 20 stacks, 2 minutes of fuzzing
```

1. Generates stacks over `hashicorp/random`, `local`, and `tls` passing a sensitive variable through templates, `base64encode`, `urlencode`, `jsonencode`, `sha256`, nested blocks, `for_each`, modules, outputs, and `nonsensitive()`.
2. Plans and applies each stack in two control planes differing only by canary value.
3. Checks:
   - **Check 1:** All API responses and unsealed database rows match identically after normalizing IDs, hashes, and timestamps.
   - **Check 2:** Neither canary (plain, base64, hex, or URL-encoded) appears in the database, data directory, scratch directory, CLI output, or server logs.
   - **Check 3:** Re-indexing state after restart leaks no sensitive variable defaults into index rows, output, or logs.
4. Runs `FuzzPlan` with arbitrary JSON and sensitive marks to verify no marked value or canary survives.

**Limits:** `just verify-mask` doesn't configure policies, notifications, or render control UI HTML. Control UI pages read only masked documents, and [notifications](notifications.md) send event metadata rather than raw plan diffs or secrets. The mask is tested, not proven.

### Where to look

| Claim | Where |
| --- | --- |
| Single package produces all run output | `internal/mask/` |
| Raw state access restricted to `internal/tofu` | `internal/archtest/state_test.go` |
| Differential test (Checks 1 to 3) | `e2e/verify_mask_test.go`, `justfile` (`verify-mask`) |
| Fuzz test | `internal/mask/fuzz_test.go` |

## Troubleshooting

- **Plan or run stores no document:** The run's distinct learned secrets exceeded 4 MiB, or one string cost more than 32 MiB to decode, and failed closed. Move large sensitive blobs out of inline variables.
- **Non-secret field masked as `(sensitive)`:** Attribute name matched Rule 3 (`password`, `secret`, `token`, etc.). Intended to prevent leaks.
- **`no secret set for state version N`:** State was written outside Writ, before secret sets, or by an apply whose mask couldn't read its plan; inspect the stack code and run [`writ state adopt`](adopt-state.md).
- **Derived secret appeared in output:** Review [What the mask can't do](#what-the-mask-cant-do), remove `nonsensitive()` around hashed or multi-line base64 values, and mark source variables `sensitive = true`.

## Next steps

- [Adopt a state](adopt-state.md): establish secret sets for externally written state.
- [Security model](security-model.md): how masking fits with credential isolation and audit logging.
- [OpenTofu stacks](serve.md#opentofu-stacks): run execution and filesystem view.
- [Approvals](approvals.md#how-an-opentofu-plan-is-approved-and-applied): masked plan review and apply.
