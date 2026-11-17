# Adopt a state

Sometimes Writ can't vouch for a state it didn't write. When a state has no secret set, Writ keeps no detail of its plans and drift checks, and no one can approve a plan against it. [States with no secret set](serve.md#states-with-no-secret-set) explains why.

`writ state adopt` fixes that. It seals a secret set with such a state, so plans and drift checks of the stack keep their detail again and you can approve a plan.

You need it in three cases:

- for a state that an apply outside Writ wrote to a stack's bucket,
- for the state a bucket had before `writ serve` first planned the stack,
- or for a version on the http backend written before `writ serve` kept secret sets.

## Before you begin

- You're a person, not a CI job, with `adopt` on the `stack@env`. `writ serve --check` refuses a grant of `adopt` to a team of a CI issuer, and `POST /v1/state/adopt` refuses a CI job's token, as it does for `override`.
- You've read the stack's code for `nonsensitive()`, `external` data sources, and provisioners. See [The risk you take](#the-risk-you-take).
- If your stack is on S3 and `writ serve` never planned it, it has no read serial yet. Plan it first, and the failed plan shows you the serial.

## Adopt the state

Run `writ state adopt` with the state version on the http backend, or the serial on S3, and a reason of 10 to 500 characters:

```
writ state adopt app --env prod --serial 3 --reason "a tofu apply from a laptop, OPS-311"
writ state adopt app --env prod --version 7 --reason "state from before secret sets, OPS-312"
```

Here's bob adopting serial 3, which a `tofu apply` with the bucket owner's keys wrote outside Writ:

```
Adopted serial 3 of app@prod, lineage b7937cd1-a438-5711-a2f6-1ce954513d64: writ serve sealed a secret set with it, read with the providers of infra at 08d5592b.
The state index holds its 3 resources.
warning: the secret set holds each value the state marks sensitive and the environment's provider_env values; a secret the state holds where nothing marks it, as an address keyed by nonsensitive() of a variable before writ serve planned the stack, is not in it, and later plans and drift checks may show it
```

Output of `just e2e-tofu-s3`, 2026-11-15.

Once you've adopted it, plan the stack again. That new plan is approvable, and its apply writes serial 4. The plan that failed stays failed.

You don't have to remember the command. In the [control UI](sign-in.md#the-control-ui), the page of a stack whose drift check found no secret set, and the page of a plan that failed for that reason, show the command that adopts the state.

### If the adoption is refused

`writ serve` refuses the request before anything runs when:

- The state isn't the newest it knows: the newest stored version on the http backend, or the serial of its newest read on S3.
- The state already has a secret set.
- An apply of the stack is running.
- The stack has no state that `writ serve` knows.

## What an adoption does

An adoption gathers its code the way a [drift check](serve.md#drift-checks) does, with the stack's files at the head of its repository's default branch. Then it reads the state the way the [state index](serve.md#the-state-index) does:

- **On the http backend**, `tofu init -backend=false` and `tofu show -json` of the stored version as a file, with no cloud credential, no `provider_env`, and no state token.
- **On S3**, the two `init` steps and `tofu state pull` with the plan role, and `tofu show -json` of the pulled file with no credential. The pulled serial must be the one you gave, of the lineage `writ serve` read it in. The run records its read of the serial, as a refresh does.

Both run as one of `provider_uids` in the filesystem view, and take one of your plan slots.

Next, the mask, seeded with `provider_env`'s values, reads the state into rows of the index. It learns each value the state marks sensitive, in a resource's `sensitive_values` or a sensitive output. Writ seals that set under the environment's key with the version or serial, as an apply seals its own.

Nothing else of the state is kept. In one transaction, `writ serve` writes the set, the index rows, and one audit row of kind `state-adopt`. That audit row has the person, the `stack@env`, the version or serial, its lineage, and your reason.

That transaction first takes the `stack@env`'s turn, which each apply also takes as it begins, so an apply that begins meanwhile waits for it to commit. Writ refuses the adoption when a newer state or a set arrived while the run read the state, or when an apply of the stack is running. If it fails, `writ serve` keeps none of the three, and you can run the adoption again.

If `show` or `pull` fails, Writ keeps only the step and its exit code, because its output can contain a value of the state that the mask hasn't learned.

After an adoption, the stack's drift check is due. Your whole request must end within 120 seconds, and if it runs longer, Writ keeps nothing.

## The risk you take

Adoption doesn't learn a secret that the state contains where nothing marks it. For example, a resource keyed by `nonsensitive()` of a sensitive variable before `writ serve` planned the stack keeps that key in its address, and no mark covers it. Later plans and drift checks of the stack may show such a value.

So adopt only a state whose code you've read for `nonsensitive()`, `external` data sources, and provisioners. `writ state adopt` prints this limit as a warning.
