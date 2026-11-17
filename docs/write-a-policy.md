# Write and test a policy

Say you want to block weak TLS keys in every stack, whoever approves the plan. You write that as a policy: a set of Rego rules that `writ serve` evaluates against each plan's masked document.

Writ reads the same rules `conftest` does. So you can write a policy once, test it with `conftest` on your laptop or in CI, and give it to `writ serve` through its config. To learn when policies run and what a deny does, read [Policy](policy.md).

## Write a rule

Put your rules in `package main`, the package `conftest` reads by default. Writ reads these:

- `deny`, `violation`, and any rule whose name starts `deny_` or `violation_`, such as `deny_rsa`, deny.
- `warn`, and any rule whose name starts `warn_`, warn.
- Each rule is a set of messages. A message is a string, or an object whose `msg` is a string, as `conftest` accepts.

Here's `examples/policy/tls.rego`:

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
```

You can put helpers in other packages, and `package main` can import them. Writ refuses:

- A set that defines no rule Writ reads.
- An `exception` rule: write the exception into the condition of the rule it excepts.
- A call to `print`, because Writ prints no input.

Writ compiles and evaluates each set separately. So a rule in one set can't add to, or replace, a rule or a helper in another set. For what your rules can read and call, see [Policy input and built-ins](policy-reference.md).

## Test it with conftest

Plan with `tofu`, then run `conftest` on the plan's JSON:

```
tofu show -json plan.out > plan.json
conftest test --policy policy/baseline plan.json
```

You get the same verdict and messages from `conftest` and `writ serve` for a rule that reads keys the masked document keeps, and values that aren't secrets. `just policy-parity` proves it for `examples/policy` on three stacks, each planned by `writ serve --dev` and by `tofu` alone:

```
writ serve: deny local_file.key: file_permission is 0777; set 0600, so only its owner reads it | tls_private_key.key: an RSA key needs 4096 bits, and this one has 2048
conftest:   deny local_file.key: file_permission is 0777; set 0600, so only its owner reads it | tls_private_key.key: an RSA key needs 4096 bits, and this one has 2048
```

Output of `just policy-parity`, 2026-11-15.

Expect them to differ in these cases:

- A value under a sensitive mark is `(sensitive)` in `writ serve`, and the value itself under `conftest`.
- `input.writ` exists only in `writ serve`.
- A built-in error gives no message under `conftest`, and an error that denies in `writ serve`.
- A rule that reads a key the mask drops, or calls a built-in that isn't allowed, runs under `conftest` and is refused by `writ serve` at start.
- `writ serve` reads no `exception` rules and no data documents, such as those of `conftest --data`.

## Start from the examples

You don't have to start from nothing. `examples/policy` has one set that works under both, and `policy_test.rego` has its tests, which `conftest verify --policy examples/policy` runs.

| File | Rule | Verdict |
| --- | --- | --- |
| `tls.rego` | `deny_rsa` | Denies an RSA key of fewer than 4096 bits |
| `files.rego` | `warn_file_permission` | Warns of a `local_file` or `local_sensitive_file` created with a `file_permission` other than `0600` |
| `replace.rego` | `deny_replace`, `warn_replace` | Denies the replace of a `tls_private_key` or a `nomad_job`, and warns of any other replace |
| `deletes.rego` | `deny_delete`, `warn_delete` | Denies a delete in `prod`, and warns of a delete elsewhere or under `conftest` |
| `window.rego` | `deny_window` | Denies an apply to `prod` from 09:00 to 16:00 Asia/Kolkata, Monday to Friday. It reads `input.writ.now`, so it never denies under `conftest` |
| `writ.rego` | | `environment` and `decision` from `input.writ`, `""` under `conftest` |

## Give it to writ serve

1. Put the set's directory on the host.
2. List it in the `policy` block of the [server config](config.md#policy).
3. Run `writ serve --check`. It fails on a set that doesn't compile, and `writ serve` compiles every set at start.
