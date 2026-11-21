# Adopt a state

When a stack's state wasn't written by `writ serve`, Writ has no secret set for it. Until a secret set exists, Writ withholds plan and drift details and refuses approval for plans against that state ([States with no secret set](serve.md#states-with-no-secret-set)).

`writ state adopt` reads the state, seals a secret set for it, and populates the state index so plans can be approved again. Use it when:

- Someone applied directly to a stack's S3 bucket outside Writ.
- You imported a pre-existing S3 state before Writ ever planned the stack.
- You upgraded from state versions written before secret sets existed.

## Before you begin

- You're a signed-in person (not a CI job) with `adopt` on `stack@env`. `writ serve --check` and `POST /v1/state/adopt` refuse `adopt` for CI tokens.
- You've inspected the stack's code for `nonsensitive()`, `external` data sources, and provisioners ([The risk you take](#the-risk-you-take)).
- For an S3 stack Writ has never planned, run `writ plan --remote` once first so Writ records its current serial.

## Adopt the state

Pass the newest HTTP state `--version` or S3 `--serial` along with a 10 to 500 character `--reason`:

```sh
writ state adopt app --env prod --serial 3 --reason "a tofu apply from a laptop, OPS-311"
writ state adopt app --env prod --version 7 --reason "state from before secret sets, OPS-312"
```

Example adopting S3 serial 3 written outside Writ:

```
Adopted serial 3 of app@prod, lineage b7937cd1-a438-5711-a2f6-1ce954513d64: writ serve sealed a secret set with it, read with the providers of infra at 08d5592b.
The state index holds its 3 resources.
warning: the secret set holds each value the state marks sensitive and the environment's provider_env values; a secret the state holds where nothing marks it, as an address keyed by nonsensitive() of a variable before writ serve planned the stack, is not in it, and later plans and drift checks may show it
```

Output of `just e2e-tofu-s3`, 2026-11-15.

After adoption, plan the stack again; the new plan can be approved and applied (the earlier failed plan stays failed). The control UI also prints the exact `writ state adopt` command on affected stack and failed-plan pages.

### If the adoption is refused

`writ serve` refuses before running OpenTofu when:

- The given version or serial isn't the newest known for that `stack@env`.
- That version or serial already has a secret set.
- An apply is running on that `stack@env`.
- `writ serve` knows no state for the stack.

## What an adoption does

Adoption fetches the stack's files from the repository's default branch head, runs inside a `provider_uids` sandbox taking a plan slot, and reads state without mutating it:

- **HTTP backend:** `tofu init -backend=false` and `tofu show -json` on the stored version with no cloud credentials, no `provider_env`, and no state token.
- **S3 backend:** offline `init`, `tofu state pull` with `plan_role` (verifying the pulled serial and lineage match), then `tofu show -json` with no credentials.

The mask (seeded with `provider_env`) reads the state into state-index rows and collects every value marked sensitive in `sensitive_values` or sensitive outputs. In a single serialized transaction, `writ serve` seals that secret set under the environment's key, writes the index rows, and records a `state-adopt` audit row (person, `stack@env`, version/serial, lineage, reason). If a newer state, secret set, or apply arrived concurrently, nothing is committed.

If `pull` or `show` fails, Writ logs only the step and exit code so unmasked state never leaks. After a successful adoption, the stack's drift check becomes due. The request times out after 120 seconds.

## The risk you take

Adoption can learn only values marked sensitive in state plus `provider_env`. An unmarked secret already embedded in state (such as a resource address keyed by `nonsensitive()` of a sensitive variable before Writ ran) won't be in the secret set and may appear in later plans or drift checks. Audit the stack's code for `nonsensitive()`, `external` data sources, and provisioners before adopting.

## Troubleshooting

- **`403 denied` from CI**: Only a signed-in person (`writ login`) with `adopt` can run `writ state adopt`.
- **`409 state: state version N is not its newest`**: Run a plan to record the newest S3 serial (or check the control UI) and adopt that latest version or serial.
- **`409 state: an apply of STACK@ENV runs`**: Wait for the active apply to finish and retry.
- **Timeout after 120 seconds**: Verify S3 bucket and OpenTofu plugin cache connectivity from `writ serve`, then retry.

## Next steps

- [Secrets and the mask](secrets.md): how secret sets protect future plans and drift checks.
- [Migrate from Terraform](migrate-from-terraform.md): move existing Terraform stacks onto Writ.
- [Drift checks](drift-checks.md): catch future outside writes on S3.
