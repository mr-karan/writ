# Migrate from Terraform

Bring a stack that Terraform ran onto Writ and OpenTofu 1.13.9 without rewriting your repository first.

A stack needs a committed `.terraform.lock.hcl`, `.tf` or `.tofu` files, and a supported backend (`backend "http" {}` or `backend "s3"`), as [OpenTofu stacks](serve.md#opentofu-stacks) explains.

## Before you begin

- [OpenTofu 1.13.9](https://opentofu.org/docs/intro/install/) installed locally if you plan to refresh `.terraform.lock.hcl` now.
- Your stack's backend is either `backend "s3"` (with `use_lockfile = true` or `dynamodb_table`, and literal values only) or ready to move to `backend "http" {}`.
- A person with `adopt` on the target `stack@env` if you are keeping existing state.

## Steps

### 1. Check your provider sources

Write each `hashicorp/` provider's `source` without a registry host, as `hashicorp/aws`. If your code sets `source = "registry.terraform.io/hashicorp/aws"`, Writ refuses the stack, because OpenTofu can't init that with a read-only lock file. A provider outside `hashicorp/` can keep a `registry.terraform.io` source: OpenTofu installs it from there.

### 2. Keep or refresh `.terraform.lock.hcl`

You can keep the lock file Terraform wrote. Terraform pins each provider at `registry.terraform.io`, and OpenTofu looks up a source with no host (such as `hashicorp/aws`) at `registry.opentofu.org`. For each run, `writ serve` writes its own copy of the lock file with providers moved to OpenTofu's registry. [Lock files that Terraform wrote](serve.md#lock-files-that-terraform-wrote) has the rules, and what it means for the hashes of `hashicorp/` providers.

When you're ready to pin OpenTofu's own build hashes in Git, run `tofu init` in the stack with OpenTofu 1.13.9 and commit the updated `.terraform.lock.hcl`:

```
Initializing provider plugins...
- Reusing previous version of hashicorp/random from the dependency lock file
- Installing hashicorp/random v3.6.3...
- Installed hashicorp/random v3.6.3 (signed, key ID 0C0AF313E5FD9F80)

Providers are signed by their developers.
If you'd like to know more about provider signing, you can read about it here:
https://opentofu.org/docs/cli/plugins/signing/

OpenTofu has made some changes to the provider dependency selections recorded
in the .terraform.lock.hcl file. Review those changes and commit them to your
version control system if they represent changes you intended to make.


Warning: Dependency lock file entries automatically updated

OpenTofu automatically rewrote some entries in your dependency lock file:
  - registry.terraform.io/hashicorp/random => registry.opentofu.org/hashicorp/random

The version selections were preserved, but the hashes were not because the
OpenTofu project's provider releases are not byte-for-byte identical.

OpenTofu has been successfully initialized!
```

Output of `tofu init -backend=false` with OpenTofu 1.13.9, 2026-11-17, in a stack that requires `hashicorp/random` 3.6.3, with the lock file Terraform 1.17.1 wrote. The closing lines are cut.

### 3. Plan the stack and adopt its existing state

A state that Terraform wrote refers to its providers at `registry.terraform.io`, and OpenTofu plans against it. In a stack that requires `hashicorp/random` 3.6.3, a state Terraform 1.17.1 applied plans with OpenTofu 1.13.9 as `No changes`. Writ reads such a state for an adoption with each provider moved to the address the lock pins, and the state in your backend keeps its addresses until an apply writes it.

To keep existing state:

1. Ensure the state lives in a backend `writ serve` supports: `backend "http" {}` or your stack's `backend "s3"`.
2. Add the repository and run `writ plan --remote`. On S3, this first plan records the current serial and fails because the state has no [secret set](glossary.md).
3. Run [`writ state adopt`](adopt-state.md) with that serial or version and a reason.
4. Run `writ plan --remote` again to verify you see `No changes` (or only expected diffs) and an approvable plan.

## Troubleshooting

- **Stack refused for `source = "registry.terraform.io/hashicorp/..."`**: Remove the `registry.terraform.io/` prefix so the source is `hashicorp/NAME`.
- **Stack refused because of backend type or `cloud` block**: Writ supports only `backend "http" {}` (with an empty block) and `backend "s3"` (with literal attributes and locking). Migrate away from Terraform Cloud (`cloud {}`), `local`, or other backends before adding the stack to Writ.
- **Stack refused because of `.tf.json` or `.terraform/` directory**: Remove committed `.terraform/` directories and convert `.tf.json` files to `.tf` or `.tofu` HCL.
- **First plan fails with `no secret set for state version N`**: Expected for any pre-existing state Writ didn't write. Follow [Adopt existing state](adopt-state.md).

## Next steps

- [Adopt existing state](adopt-state.md): seal a secret set for the state you brought over.
- [AWS roles and credentials](aws-credentials.md): configure `plan_role` and `apply_role` for S3 state.
- [Control plane internals: Lock files that Terraform wrote](serve.md#lock-files-that-terraform-wrote): full rules on lock file translation and hash verification.
