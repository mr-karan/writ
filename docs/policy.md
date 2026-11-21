# Policy

Approval rules control how many people review a plan. Policies control what no plan may do, regardless of approvals. Write rules in Rego; `writ serve` evaluates every plan twice: when created (the plan point) and again when someone requests an apply (the apply point).

Policies read the masked plan document with OpenTofu's JSON keys, so the same `.rego` files run under `conftest` on `tofu show -json`. See [Write and test a policy](write-a-policy.md) for a walkthrough and [Policy cookbook](policy-cookbook.md) for examples.

## Where policy lives

Policies load only from directories configured in `/etc/writ/serve` (`policy` block) when `writ serve` starts. See [Config: Policy](config.md#policy).

Writ never reads policy files from a stack's repository, so a merge request can't weaken or delete the rules checking it. Teams that want repo-local checks can still run `conftest` in CI independently.

## How results become a decision

Each result is a message from one rule in one set. A decision's outcome is `deny` if any result denies, `warn` if results exist without a deny, and `pass` otherwise.

| Result | Mandatory set | Advisory set |
| --- | --- | --- |
| `deny` or `violation` message | denies | warns |
| `warn` message | warns | warns |
| Evaluation error | denies | warns |

### When you plan

A mandatory deny sets the plan's status to `refused`, keeps the masked document and decision, and discards the saved plan so it can't be approved or applied.

The decision appears under `policy` in [`writ.remote-plan/v0`](json-output.md#documents-of-writ-serve), in the control UI, and at the end of `writ plan --remote` (which exits 1):

```
OpenTofu 1.13.9 plan of net@dev

  + local_file.note (create)
  + random_id.id (create)
  + tls_private_key.key (create)

Plan: 3 to add, 0 to change, 0 to destroy.

Policy: denied, so this plan cannot be approved or applied.
  deny: [examples] tls_private_key.key: an RSA key needs 4096 bits, and this one has 2048
  warn: [examples] local_file.note: file_permission is 0777; set 0600, so only its owner reads it
```

Output of `TestTofuPlanByCommit`, 2026-11-15.

### When you apply

`POST /v1/runs` re-evaluates live plans after the grant check and before starting the run, using the server's current policy sets, `decision = "apply"`, the caller, and the counting approvals.

On a deny, Writ returns `403` (`policy`), starts no run, doesn't spend a CI token, and leaves the plan `approved`. Before evaluating, Writ re-reads the commit and opens the state's secret set to mask policy messages; if sources moved or the state lacks a secret set, Writ returns `409` before policy runs.

On pass or warn, the run starts and stores the decision under `policy` in [`writ.remote-run/v0`](json-output.md#documents-of-writ-serve). The decision hashes the approvals it saw; if approvals change before the start transaction commits, Writ returns `409` (`approvals changed since policy was evaluated; apply again`) without starting a run or spending a token.

Adding or tightening a policy takes effect at the apply point even for plans created earlier. Policies don't run on [drift checks](drift-checks.md) because drift plans are never approved or applied.

### Nothing overrides a deny

Neither approvals nor `writ apply --remote --override` bypass a mandatory deny (`--override` skips approval rules only). Fix the code in a new commit, or update the server policy and plan again.

## Read the policy the server runs

`GET /v1/policy` returns the active sets as [`writ.policy/v0`](json-output.md#documents-of-writ-serve) in config order: set name, level, and every `.rego` file (excluding `*_test.rego`) with relative names. Any principal with `read` or `admin` on any `stack@env` can read it.

**Warning:** Never embed secrets in Rego files. Policy text isn't masked and is readable by anyone who can read any stack.

In [`writ mcp`](agents.md#the-mcp-server), the `policy` tool reads this route (with optional `set` and `file` filters), and `plan` / `get_plan` prepend the stored decision (`OUTCOME: [SET, LEVEL] RULE: MESSAGE`, denies first) so agents can read the failing rule and fix the code.

## How policy handles secrets

Policies receive only the masked plan document (`internal/policy` imports no other Writ package, enforced by `internal/archtest`). Sensitive attributes appear as `"(sensitive)"`.

Every policy message passes through the mask before storage, logging, or display:

- At the plan point, the plan's own mask scrubs messages.
- At the apply point, a mask built from `provider_env` (or dev `TF_VAR_*`) and the state's sealed secret set scrubs messages.
- Non-printable messages are quoted and re-swept. Outcomes are decided before masking so a secret spelling `deny` can't alter the verdict.

**Limits:** If a Rego rule hardcodes a secret in its own message text and that string isn't in the run's mask, the mask won't scrub it.

## Next steps

- [Write and test a policy](write-a-policy.md)
- [Policy cookbook](policy-cookbook.md)
- [Policy input and built-ins](policy-reference.md)
- [Approvals](approvals.md)
- [Secrets and the mask](secrets.md)
