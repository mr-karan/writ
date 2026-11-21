# Write and test a policy

Policies are directories of Rego files that `writ serve` evaluates against each plan's masked document at plan time and again before apply. Unlike approval rules, a mandatory policy deny can't be approved or overridden.

Writ uses the same rule conventions as `conftest`: write rules once, test them locally with `conftest verify` and `conftest test`, and load them into `writ serve` via server config. Policies load only from server config directories, never from a stack's repository, so a merge request can't weaken or delete the rules checking it. For ready-to-use AWS and workflow rules, see the [Policy cookbook](policy-cookbook.md).

## How policy decisions work

| Result | Mandatory set | Advisory set |
| --- | --- | --- |
| `deny` or `violation` message | denies | warns |
| `warn` message | warns | warns |
| Evaluation error | denies | warns |

- **At plan time:** A mandatory deny sets the plan's status to `refused`, keeps the masked document and decision, and discards the saved plan so it can't be approved or applied.
- **At apply time:** `POST /v1/runs` re-evaluates live plans against the server's current policy sets with `decision = "apply"`, the caller, and counting approvals. On a deny, Writ returns `403` (`policy`), starts no run, doesn't spend a CI token, and leaves the plan `approved`. If sources moved, state lacks a secret set, or approvals changed before start commits, Writ returns `409`. Adding or tightening a policy takes effect at apply time even for plans created earlier. Policies don't run on [drift checks](drift-checks.md).
- **Nothing overrides a deny:** Neither approvals nor `writ apply --remote --override` bypass a mandatory deny (`--override` skips approval rules only).
- **Read active policies:** `GET /v1/policy` (and the `policy` tool in [`writ mcp`](agents.md#the-mcp-server)) returns active sets as [`writ.policy/v0`](json-output.md#documents-of-writ-serve) to any principal with `read` or `admin` on any `stack@env`. **Warning:** Never embed secrets in Rego files; policy text isn't masked and is readable by anyone who can read any stack.
- **Secrets in policy messages:** Policies receive only the masked plan document, and output messages pass through the mask before storage or display. **Limit:** If a Rego rule hardcodes a secret in its own message text and that string isn't in the run's mask, the mask won't scrub it. Outcomes are decided before masking, so a secret that spells `deny` can't change the verdict.

## Before you begin

- [`conftest`](https://www.conftest.dev/) installed locally for `conftest verify` and `conftest test`.
- OpenTofu 1.13.9 and a stack you can plan locally or on `writ serve --dev`.

## 1. Write a rule

Create a policy set directory (for example `policy/baseline`) and put rules in `package main`:

- `deny`, `violation`, `deny_*`, and `violation_*` block the plan or apply when the set's `level` is `mandatory`.
- `warn` and `warn_*` attach warnings without blocking approval or apply.
- Each rule returns a set of strings or objects with a string `msg` field.

Write `policy/baseline/tls.rego` and `policy/baseline/files.rego`:

```rego
package main

import rego.v1

deny_rsa contains msg if {
	some rc in input.resource_changes
	rc.type == "tls_private_key"
	"create" in rc.change.actions
	rc.change.after.algorithm == "RSA"
	rc.change.after.rsa_bits < 4096
	msg := sprintf("%s: an RSA key needs 4096 bits, and this one has %d", [rc.address, rc.change.after.rsa_bits])
}

warn_file_permission contains msg if {
	some rc in input.resource_changes
	rc.type in {"local_file", "local_sensitive_file"}
	"create" in rc.change.actions
	rc.change.after.file_permission != "0600"
	msg := sprintf("%s: file_permission is %s; set 0600, so only its owner reads it", [rc.address, rc.change.after.file_permission])
}
```

Helper packages can be imported from `package main`. Each policy set compiles and evaluates in isolation, so one set can't alter rules in another. Writ refuses:

- A set with no `deny*`, `violation*`, or `warn*` rule in `package main`.
- Any `exception` rule (encode exceptions directly in the rule condition).
- `print` or any built-in outside the [allowed built-ins list](policy-reference.md#built-ins).

## 2. Unit-test it with `conftest verify`

Put unit tests in `*_test.rego` files beside your rules. `conftest verify` runs them; `writ serve` ignores them:

```rego
package main

import rego.v1

rsa_key(bits) := {"resource_changes": [{
	"address": "tls_private_key.k",
	"type": "tls_private_key",
	"change": {"actions": ["create"], "after": {"algorithm": "RSA", "rsa_bits": bits}},
}]}

test_rsa_2048_denied if {
	deny_rsa == {"tls_private_key.k: an RSA key needs 4096 bits, and this one has 2048"} with input as rsa_key(2048)
}

test_rsa_4096_allowed if {
	count(deny_rsa) == 0 with input as rsa_key(4096)
}
```

Run the bundled examples with `conftest verify`:

```
$ conftest verify --policy examples/policy
19 tests, 19 passed, 0 warnings, 0 failures, 0 exceptions, 0 skipped
```

## 3. Test it with conftest on an OpenTofu plan

Export a local plan as JSON and run `conftest test`:

```sh
tofu show -json plan.out > plan.json
conftest test --policy policy/baseline plan.json
```

For rules reading non-secret fields kept by Writ's mask, `conftest` and `writ serve` return identical verdicts and messages:

```
writ serve: deny local_file.key: file_permission is 0777; set 0600, so only its owner reads it | tls_private_key.key: an RSA key needs 4096 bits, and this one has 2048
conftest:   deny local_file.key: file_permission is 0777; set 0600, so only its owner reads it | tls_private_key.key: an RSA key needs 4096 bits, and this one has 2048
```

Output of a real run, 2026-11-15.

Expected differences between `conftest` and `writ serve`:

- Sensitive values become `"(sensitive)"` in `writ serve`, while `conftest` sees cleartext.
- `input.writ` (`decision`, `now`, `environment`, `stack`, `actor`, `approvals`) exists only in `writ serve`. Read it with `object.get(input, ["writ", "environment"], "")` so rules work in both.
- Built-in runtime errors are strict in `writ serve` (triggering a deny) and silent in `conftest`.
- Rules referencing dropped keys (`planned_values`, `prior_state`) or disallowed built-ins fail `writ serve` startup.
- `writ serve` doesn't load `exception` rules or external `--data` documents.

## 4. Give it to writ serve

1. Place the policy directory on the `writ serve` host (for example `/etc/writ/policy/baseline`).
2. Add a `policy` block to your [server config](config.md#policy):
   ```hcl
   policy {
     set "baseline" {
       dir   = "policy/baseline"
       level = "mandatory"
     }
   }
   ```
3. Validate the config and compile every policy set against Writ's input schema:
   ```sh
   writ serve --config /etc/writ/serve --check
   ```
4. Restart `writ serve` (or pass the config to `writ serve --dev --config FILE` locally) and verify loaded sets with `GET /v1/policy`.

## 5. See a plan denied and a warning shown

Planning a commit with a 2048-bit RSA key and a `0777` file triggers both `deny` and `warn` and exits 1:

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

Output of a real run, 2026-11-15.

A mandatory deny stores the masked document and decision with `status = "refused"` and discards the saved plan. Neither `writ approve --remote` nor `writ apply --remote --override` can apply it. Once you raise `rsa_bits` to `4096` and plan again, only the warning remains and the plan becomes `pending` (or `approved` if no approval rule matches).

## Start from the examples

`examples/policy` provides a full policy set and unit tests (`policy_test.rego` and `cookbook_test.rego`). See the [Policy cookbook](policy-cookbook.md) for full rules and tests:

| File | Rule | Verdict |
| --- | --- | --- |
| `tls.rego` | `deny_rsa` | Denies RSA keys shorter than 4096 bits |
| `files.rego` | `warn_file_permission` | Warns on `local_file` / `local_sensitive_file` with `file_permission != "0600"` |
| `replace.rego` | `deny_replace`, `warn_replace` | Denies replacement of `tls_private_key` or `nomad_job`; warns on other replacements |
| `deletes.rego` | `deny_delete`, `warn_delete` | Denies deletes in `prod`; warns elsewhere and under `conftest` |
| `window.rego` | `deny_window` | Denies `prod` applies 09:00 to 16:00 Asia/Kolkata, Monday to Friday |
| `cookbook.rego` | `deny_missing_tags`, `deny_public_s3`, `deny_unencrypted_storage`, `deny_open_ingress`, `deny_instance_size`, `deny_region`, `warn_iam_wildcard`, `deny_iam_prod_apply` | AWS tags, S3 public access, encryption, security groups, instance sizes, regions, and IAM/approval rules |
| `writ.rego` | | Helpers reading `environment` and `decision` from `input.writ` (default `""` under `conftest`) |

## Troubleshooting

- **`rego_type_error: undefined ref: input.planned_values`**: Writ omits `planned_values` and `prior_state`. Read `input.resource_changes`, `input.resource_drift`, and `input.output_changes` instead ([Policy input and built-ins](policy-reference.md#input)).
- **`undefined function ...` during `writ serve --check`**: Your rule calls a disallowed built-in (such as `http.send`, `time.now_ns`, or `print`). Use `input.writ.now` for time and check [Built-ins](policy-reference.md#built-ins).
- **Passes in `conftest`, denies in `writ serve`**: Check whether the rule inspects a sensitive value (`"(sensitive)"`), hits a strict built-in error, or reads `input.writ`.
- **Rule never fires**: Verify exact string values (for example `input.writ.environment == "prod"`, not `"production"`) and test with `conftest verify`.

## Next steps

- [Policy cookbook](policy-cookbook.md): 12 copy-pasteable Rego rules and tests.
- [Policy input and built-ins](policy-reference.md): input schema, allowed built-ins, and process limits.
- [Server config](config.md#policy): full `policy` block reference.
