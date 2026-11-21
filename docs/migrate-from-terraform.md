# Migrate from Terraform

Move an existing Terraform stack onto Writ and OpenTofu 1.13.9 without rewriting the repository first. Each stack needs `.tf` or `.tofu` files, a committed `.terraform.lock.hcl`, and either `backend "http" {}` or `backend "s3"` ([OpenTofu stacks](serve.md#opentofu-stacks)).

## Before you begin

- [OpenTofu 1.13.9](https://opentofu.org/docs/intro/install/) installed locally if you want to refresh `.terraform.lock.hcl` now.
- Stack backend configured as `backend "s3"` (literal attributes only, with `use_lockfile = true` or `dynamodb_table`) or ready to switch to `backend "http" {}`.
- A signed-in person with `adopt` on `stack@env` if keeping existing state.

## Steps

### 1. Check your provider sources

Omit the registry host on `hashicorp/` providers (write `source = "hashicorp/aws"`, not `"registry.terraform.io/hashicorp/aws"`). Writ refuses explicit `registry.terraform.io/hashicorp/...` sources because OpenTofu can't initialize them with `-lockfile=readonly`. Third-party providers outside `hashicorp/` may keep `registry.terraform.io` sources.

### 2. Keep or refresh `.terraform.lock.hcl`

You can keep Terraform's existing lock file while migrating: `writ serve` translates `registry.terraform.io` entries to OpenTofu's registry per run ([Lock files that Terraform wrote](serve.md#lock-files-that-terraform-wrote)).

When you're ready to pin OpenTofu's own build hashes in Git, run `tofu init` with OpenTofu 1.13.9 and commit `.terraform.lock.hcl`:

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

Output of `tofu init -backend=false` with OpenTofu 1.13.9, 2026-11-17, in a stack requiring `hashicorp/random` 3.6.3 with a lock file written by Terraform 1.17.1 (closing lines omitted).

### 3. Plan the stack and adopt its existing state

OpenTofu 1.13.9 plans cleanly against state written by Terraform (providers at `registry.terraform.io` stay unchanged in backend state until the first OpenTofu apply).

1. Confirm the stack uses `backend "http" {}` or `backend "s3"`.
2. Register the repository and run `writ plan --remote`. On S3, this records the current serial and fails because no [secret set](glossary.md) exists yet.
3. Run [`writ state adopt`](adopt-state.md) with that serial or version and a reason.
4. Run `writ plan --remote` again to confirm `No changes` (or expected diffs) and an approvable plan.

## Troubleshooting

- **Refused for `source = "registry.terraform.io/hashicorp/..."`**: Remove the `registry.terraform.io/` prefix so the source is `hashicorp/NAME`.
- **Refused for unsupported backend or `cloud` block**: Migrate from `cloud {}`, `local`, or other backends to `backend "http" {}` or `backend "s3"` with literal attributes and locking.
- **Refused for `.tf.json` or `.terraform/`**: Remove committed `.terraform/` directories and convert `.tf.json` to HCL `.tf` or `.tofu`.
- **First plan fails with `no secret set for state version N`**: Expected for pre-existing state Writ didn't write; follow [Adopt a state](adopt-state.md).

## Next steps

- [Adopt a state](adopt-state.md): seal a secret set for existing state.
- [AWS credentials](aws-credentials.md): configure `plan_role` and `apply_role` for S3 state.
- [Control plane internals: Lock files that Terraform wrote](serve.md#lock-files-that-terraform-wrote): lock-file translation and hash rules.
