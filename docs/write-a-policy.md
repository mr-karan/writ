# Write and test a policy

Say you want to block weak TLS keys and world-readable files across every stack, whoever approves the plan. You write that as a policy: a directory of Rego files that `writ serve` evaluates against each plan's masked document at the plan point and the apply point.

Writ reads the same rules `conftest` does. You write a policy once, test it locally with `conftest verify` and `conftest test`, and load it into `writ serve` through its config. For ready-to-use rules covering tags, S3 public access, encryption, security groups, instance sizes, regions, change windows, and apply approvals, see the [Policy cookbook](policy-cookbook.md). To learn how policy sets and decision points work, read [Policy](policy.md).

## Before you begin

- [`conftest`](https://www.conftest.dev/) installed locally to run unit tests (`conftest verify`) and test policies against `tofu show -json` before deploying them.
- OpenTofu 1.13.9 and a stack you can plan locally or on `writ serve --dev`.

## 1. Write a rule

Create a directory for your policy set (for example, `policy/baseline`) and put your rules in `package main`, the package `conftest` reads by default. Writ reads these rule names:

- `deny`, `violation`, and any rule whose name starts with `deny_` or `violation_` (such as `deny_rsa`) deny the plan or apply when the set's `level` is `mandatory`.
- `warn` and any rule whose name starts with `warn_` (such as `warn_file_permission`) attach a warning without blocking approval or apply.
- Each rule is a set of messages (`deny_rsa contains msg if { ... }`). Every message is a string, or an object whose `msg` field is a string, as `conftest` accepts.

Write `policy/baseline/tls.rego` to block RSA keys shorter than 4096 bits, and `policy/baseline/files.rego` to warn on permissive file modes:

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

You can put helpers in other packages and import them from `package main`. Writ compiles and evaluates each policy set separately, so a rule in one set cannot add to or replace a rule or helper in another set. Writ refuses:

- A set that defines no `deny*`, `violation*`, or `warn*` rule in `package main`.
- An `exception` rule: write any exception directly into the condition of the rule it excepts.
- Any call to `print` or any built-in outside the [allowed built-ins list](policy-reference.md#built-ins).

## 2. Unit-test it with `conftest verify`

Add `policy/baseline/policy_test.rego` alongside your rules. Files ending in `_test.rego` are run by `conftest verify` and ignored by `writ serve`:

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

Run the unit tests with `conftest verify` against the repository's bundled `examples/policy` directory:

```
$ conftest verify --policy examples/policy
19 tests, 19 passed, 0 warnings, 0 failures, 0 exceptions, 0 skipped
```

## 3. Test it with conftest on an OpenTofu plan

Plan a stack locally with `tofu`, export the plan as JSON, and run `conftest test`:

```
tofu show -json plan.out > plan.json
conftest test --policy policy/baseline plan.json
```

You get the same verdict and messages from `conftest` and `writ serve` for any rule that reads keys the masked document keeps and values that aren't secrets. `just policy-parity` proves it for `examples/policy` across three stacks, each planned by `writ serve --dev` and by `tofu` alone:

```
writ serve: deny local_file.key: file_permission is 0777; set 0600, so only its owner reads it | tls_private_key.key: an RSA key needs 4096 bits, and this one has 2048
conftest:   deny local_file.key: file_permission is 0777; set 0600, so only its owner reads it | tls_private_key.key: an RSA key needs 4096 bits, and this one has 2048
```

Output of `just policy-parity`, 2026-11-15.

Expect `conftest` and `writ serve` to differ in these cases:

- A value under a sensitive mark is `"(sensitive)"` in `writ serve`, and the cleartext value under `conftest`.
- `input.writ` (`decision`, `now`, `environment`, `stack`, `actor`, `approvals`) exists only in `writ serve`. Use `object.get(input, ["writ", "environment"], "")` so rules still evaluate cleanly under `conftest`.
- A built-in runtime error produces no message under `conftest`, and an error that denies in `writ serve`.
- A rule that reads a key the mask drops (`planned_values` or `prior_state`), or calls a built-in outside the allow-list, runs under `conftest` and is refused by `writ serve` at start.
- `writ serve` reads no `exception` rules and no external data documents (`conftest --data`).

## 4. Give it to writ serve

1. Copy the policy directory onto the `writ serve` host (for example, `/etc/writ/policy/baseline`).
2. Add a `policy` block to the [server config](config.md#policy):
   ```hcl
   policy {
     set "baseline" {
       dir   = "policy/baseline"
       level = "mandatory"
     }
   }
   ```
3. Validate the config and compile every policy set against Writ's input schema before restarting:
   ```
   writ serve --config /etc/writ/serve --check
   ```
4. Restart `writ serve` (or start `writ serve --dev --policy ./examples/policy` locally) and verify the loaded sets with `GET /v1/policy`.

## 5. See a plan denied and a warning shown

When you run `writ plan --remote` on a commit that creates a 2048-bit RSA key and a `0777` local file, `writ serve` evaluates the policy set at the plan point, prints both the `deny` and the `warn`, and refuses the plan (exit code `1`):

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

Because the plan was denied by a `mandatory` set, `writ serve` stores its masked document and policy decision with `status = "refused"` and discards the saved plan bytes. Neither `writ approve --remote` nor `writ apply --remote --override` can apply a refused plan. Once you change `rsa_bits = 4096` and plan again, only the `warn` remains, the plan's status is `planned`, and it can be approved and applied.

## Start from the examples

`examples/policy` contains a complete policy set and unit tests (`policy_test.rego` and `cookbook_test.rego`) that work under both `writ serve` and `conftest`. See the [Policy cookbook](policy-cookbook.md) for the full source and test of each recipe:

| File | Rule | Verdict |
| --- | --- | --- |
| `tls.rego` | `deny_rsa` | Denies an RSA key of fewer than 4096 bits |
| `files.rego` | `warn_file_permission` | Warns of a `local_file` or `local_sensitive_file` created with a `file_permission` other than `0600` |
| `replace.rego` | `deny_replace`, `warn_replace` | Denies the replace of a `tls_private_key` or a `nomad_job`, and warns of any other replace |
| `deletes.rego` | `deny_delete`, `warn_delete` | Denies a delete in `prod`, and warns of a delete elsewhere or under `conftest` |
| `window.rego` | `deny_window` | Denies an apply to `prod` from 09:00 to 16:00 Asia/Kolkata, Monday to Friday |
| `cookbook.rego` | `deny_missing_tags`, `deny_public_s3`, `deny_unencrypted_storage`, `deny_open_ingress`, `deny_instance_size`, `deny_region`, `warn_iam_wildcard`, `deny_iam_prod_apply` | AWS tag, S3, encryption, security group, instance size, region, and IAM/approval rules |
| `writ.rego` | | `environment` and `decision` from `input.writ`, `""` under `conftest` |

## Troubleshooting

- **`rego_type_error: undefined ref: input.planned_values`**: Writ's masked plan input omits `planned_values` and `prior_state`. Read `input.resource_changes`, `input.resource_drift`, and `input.output_changes` instead; see [Policy input and built-ins](policy-reference.md#input).
- **`undefined function ...` during `writ serve --check`**: Your rule calls a Rego built-in outside Writ's allow-list (such as `http.send`, `time.now_ns`, or `print`). Replace `time.now_ns` with `input.writ.now` and check [Built-ins](policy-reference.md#built-ins).
- **Rule passes under `conftest` but denies in `writ serve`**: Check whether the rule inspects a sensitive value (which becomes `"(sensitive)"` in `writ serve`), triggers a built-in error (strict in Writ), or depends on `input.writ`.
- **Policy silently never fires**: Double-check string comparisons (for example `input.writ.environment == "prod"`, not `"production"`), and write a `*_test.rego` file run with `conftest verify`.

## Next steps

- [Policy cookbook](policy-cookbook.md): 12 copy-pasteable Rego rules and tests.
- [Policy input and built-ins](policy-reference.md): exact input schema, allowed built-ins, and memory/timeout bounds.
- [Policy](policy.md): plan-point vs apply-point decisions and mandatory vs advisory sets.
- [Server config](config.md#policy): full `policy` block reference.
