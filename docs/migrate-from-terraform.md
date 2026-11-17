# Migrate from Terraform

`writ serve` runs OpenTofu, and it plans a stack that Terraform ran without any change to your repository. A stack needs its committed `.terraform.lock.hcl`, `.tf` or `.tofu` files, and a backend `writ serve` supports, as [OpenTofu stacks](serve.md#opentofu-stacks) explains.

## The lock file

Keep the lock file Terraform wrote. Terraform pins each provider at `registry.terraform.io`, and OpenTofu looks up a source with no host, such as `hashicorp/aws`, at `registry.opentofu.org`. So `writ serve` moves the lock file's providers to OpenTofu's registry in its own copy for each run. [Lock files that Terraform wrote](serve.md#lock-files-that-terraform-wrote) has the rules, and what it means for the hashes of `hashicorp/` providers.

When you're ready, commit a lock file that OpenTofu wrote, so your repository pins the hashes of OpenTofu's builds. Run `tofu init` in the stack with OpenTofu 1.13.9, and commit `.terraform.lock.hcl`:

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

## Provider sources

Write each `hashicorp/` provider's `source` without a host, as `hashicorp/aws`. If your code sets `source = "registry.terraform.io/hashicorp/aws"`, Writ refuses the stack, because OpenTofu can't init that with a read-only lock file. A provider outside `hashicorp/` can keep a `registry.terraform.io` source: OpenTofu installs it from there.

## The state

A state that Terraform wrote refers to its providers at `registry.terraform.io`, and OpenTofu plans against it. In a stack that requires `hashicorp/random` 3.6.3, a state Terraform 1.17.1 applied plans with OpenTofu 1.13.9 as `No changes`.

To keep the state, move it to a backend `writ serve` supports: its own `http` backend, or your stack's S3 bucket. A state that Writ didn't write has no secret set, so Writ keeps no detail of a plan against it until you [adopt the state](adopt-state.md).
