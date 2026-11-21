# Write and test a policy

Write a Rego policy, test it locally with `conftest`, and load it into `writ serve`, which evaluates it on each plan's masked document at plan time and again before apply. How verdicts work is in [How policy decisions work](policy-reference.md#how-policy-decisions-work).

> **Warning:** a mandatory deny cannot be approved or overridden, not even with `writ apply --remote --override`. Policies load only from server config directories, never from a stack's repository, so a merge request cannot weaken them.

> **Warning:** never put secrets in Rego files. Policy text is not masked, and anyone who can read any stack can read it through `GET /v1/policy`.

## Before you begin

- [`conftest`](https://www.conftest.dev/) installed locally for `conftest verify` and `conftest test`.
- OpenTofu 1.13.9 and a stack you can plan locally or on `writ serve --dev`.

## 1. Write a rule

Create a policy set directory (for example `policy/baseline`) with rules in `package main`. `deny*` and `violation*` rules block when the set is `mandatory`; `warn*` rules warn. Naming and refusal rules are in [Rules a policy set must follow](policy-reference.md#rules-a-policy-set-must-follow).

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

For rules that read non-secret fields the mask keeps, `conftest` and `writ serve` return the same verdicts and messages:

```
writ serve: deny local_file.key: file_permission is 0777; set 0600, so only its owner reads it | tls_private_key.key: an RSA key needs 4096 bits, and this one has 2048
conftest:   deny local_file.key: file_permission is 0777; set 0600, so only its owner reads it | tls_private_key.key: an RSA key needs 4096 bits, and this one has 2048
```

Output of a real run, 2026-11-15.

`writ serve` masks sensitive values, adds `input.writ`, and is stricter about built-in errors, so a rule can pass in `conftest` and deny in `writ serve` ([writ serve and conftest](policy-reference.md#writ-serve-and-conftest)).

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

`examples/policy` holds a full policy set and unit tests. Its files are listed in the [Policy cookbook](policy-cookbook.md#files-in-examplespolicy).

## Troubleshooting

| Problem | Fix |
| --- | --- |
| `rego_type_error: undefined ref: input.planned_values` | Writ omits `planned_values` and `prior_state`. Read `input.resource_changes`, `input.resource_drift`, and `input.output_changes` ([Policy input](policy-reference.md#input)). |
| `undefined function ...` in `writ serve --check` | The rule calls a disallowed built-in, such as `http.send`, `time.now_ns`, or `print`. Use `input.writ.now` for time; see [Built-ins](policy-reference.md#built-ins). |
| Passes in `conftest`, denies in `writ serve` | The rule reads a sensitive value (`"(sensitive)"`), hits a strict built-in error, or reads `input.writ`. |
| Rule never fires | Check exact strings (`input.writ.environment == "prod"`, not `"production"`) and test with `conftest verify`. |

## Next steps

- [Policy cookbook](policy-cookbook.md): 12 copy-pasteable Rego rules and tests.
- [Policy input and built-ins](policy-reference.md): input schema, allowed built-ins, and process limits.
- [Server config](config.md#policy): full `policy` block reference.
