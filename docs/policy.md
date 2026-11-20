# Policy

Approval rules decide how many people must look at a plan. Policy decides what no plan may do, whoever approves it. You write those rules in Rego, and `writ serve` checks every plan of an OpenTofu stack against them twice: when the plan is created, the plan point, and when someone asks to apply it, the apply point.

Your policy reads the masked plan document with OpenTofu's own key names. So the same `.rego` file runs under `conftest` on the output of `tofu show -json`. To write one step by step, see [Write and test a policy](write-a-policy.md); for ready-to-use rules and unit tests, see the [Policy cookbook](policy-cookbook.md).

## Where policy lives

Policies come only from the server's config. You list directories of `.rego` files on the host in a `policy` block, and `writ serve` reads them at start. The [server config](config.md#policy) describes the block.

No file in a commit is read as policy. That's deliberate: a merge request can't change, weaken, or delete the policy that checks its own plan.

If your team wants checks of its own, run `conftest` on your plans in your CI job. Those checks guard your merge requests, and `writ serve` doesn't read them.

## How results become a decision

Every result is a message from one rule of one set. Your decision's outcome is `deny` when any result denies, `warn` when there are results and none denies, and `pass` otherwise.

Whether a result denies depends on the set's level:

| Result | Mandatory set | Advisory set |
| --- | --- | --- |
| A `deny` or `violation` message | denies | warns |
| A `warn` message | warns | warns |
| An error | denies | warns |

### When you plan

At the plan point, a deny refuses the plan. Its status is `refused`, and it keeps its id and its masked document but no saved plan, so no approval and no apply can use it.

Writ stores the decision with the plan. You see it as `policy` in [`writ.remote-plan/v0`](json-output.md#documents-of-writ-serve), on the plan's page in the control UI, and at the end of the plan's text. Here's `writ plan --remote` of a commit that `examples/policy` refuses, which exits 1:

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

When you ask to apply, `POST /v1/runs` checks the live plan again, after the grant check and before the run begins. This time it uses the policies the server runs now, `decision = "apply"`, the person or CI job that asks, and the approvals that count.

If a policy denies, you get 403 with a diagnostic of class `policy` for the refusal and one for each result. No run begins, your CI token isn't spent, and the plan stays approved.

To build the mask of its messages, the apply point reads the plan's commit again and opens the secret set sealed with the state the plan read. If the sources differ from the plan's, or the state has no secret set, you get 409 before any policy runs, as the apply itself would fail.

A pass or a warning lets the run begin, and the run keeps the decision as `policy` in [`writ.remote-run/v0`](json-output.md#documents-of-writ-serve).

The decision also records a digest of the approvals it checked. That's so the approvals can't change between the check and the run. The run begins only when the approvals it records, read in the transaction that starts it, have that digest. If someone withdraws or adds an approval in between, you get 409 `approvals changed since policy was evaluated; apply again`, no run starts, and no CI token is spent.

If you add or change a policy after a plan was created, it checks that plan at its apply point. Rules that only read the plan see the same document at both points.

No policy checks a [drift check](serve.md#drift-checks). Its plan is never approved or applied, and a deny would report its drift as a failed check. After `writ state adopt`, a plan of the stack is checked like any other.

### Nothing overrides a deny

Approvals don't, and neither does `writ apply --remote --override`, which skips approval rules only. To recover, push a new commit, or change the policy in the server's config and plan again.

## Read the policy the server runs

You can read the exact policy your server runs with `GET /v1/policy`. It serves the sets as [`writ.policy/v0`](json-output.md#documents-of-writ-serve), in the order the config lists them: each set's name, its level, and the text of each of its `.rego` files, named relative to the set's directory.

Files whose names end in `_test.rego` aren't part of a set, so they aren't served. The document has no path. If the server has no `policy` block, it serves no set.

You can read it when a grant gives you `read` or `admin` on any `stack@env`, the rule of `GET /v1/config`. The sets aren't narrowed to stacks, because every set checks every plan, and a plan's decision already shows the sets' messages to anyone who can read the plan.

**Warning:** Don't put a secret in a policy. Writ doesn't mask a policy's text, and every principal that can read a stack can read it.

If you use an agent, [`writ mcp`](agents.md#the-mcp-server) serves the route as its `policy` tool, which takes `set` and `file` to return less. The text of its `plan` and `get_plan` tools starts with the plan point's decision that the server stored, each result as `OUTCOME: [SET, LEVEL] RULE: MESSAGE`, denies first.

`writ mcp` checks nothing itself. Your agent reads a deny, reads the rule, and changes its code. No tool overrides a deny.

## How policy handles secrets

Your policy's input is the masked plan document, and nothing else from a run. `internal/policy` imports no other Writ package, which `internal/archtest` enforces. So if a rule compares a secret attribute with a value, it compares `(sensitive)`.

Before Writ stores, logs, or shows a message, the message passes through the mask. At the plan point that's the plan's own mask, which knows the plan's secrets.

At the apply point it's a mask of the environment's `provider_env` values, or under `writ serve --dev` of the `TF_VAR_` variables it started with, and of the secret set sealed with the state the plan read. That set lists the past secrets a state keeps where nothing marks them, as the apply's own mask learns them.

If a message isn't printable, Writ quotes it, and the mask sweeps it again after the quoting. The mask sweeps only messages. Writ decides the verdict and each result's outcome before it, so a secret that spells `deny` can't change them, and an outcome other than `pass` or `warn` denies.

**Limits.** If a policy builds a message from a secret written into the policy itself, the mask sweeps it only when the secret is one of those.

## Next steps

- [Write and test a policy](write-a-policy.md)
- [Policy cookbook](policy-cookbook.md)
- [Policy input and built-ins](policy-reference.md)
- [Approvals](approvals.md)
- [Secrets and the mask](secrets.md)
