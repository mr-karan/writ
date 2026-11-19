# How the control plane works

`writ serve` is the control plane: one process with a REST API that plans and applies your OpenTofu stacks. Your CI jobs ask it for a plan or an apply, and they never hold a cloud credential or a state key.

Your jobs prove who they are with the OIDC token their platform issues, and the teams and grants in your config decide what each token can do. Your plan runs on the server with OpenTofu, writes nothing, and stays there. Its id is a hash of what OpenTofu planned.

Every request you send to `writ serve` leaves a row in an audit log in the control database before you get its answer. That covers the API, the control UI, and the state backend. Only `GET /healthz`, `GET /v1/server`, and requests that the [address rate limit](api.md#rate-limits) refuses leave no row, as [Every answer waits for its audit row](api.md#every-answer-waits-for-its-audit-row) explains. `GET /v1/audit` and `writ audit --remote` read the rows your grants let you see, as [Read the audit log](api.md#read-the-audit-log) explains. For a team the control database is Postgres. Under [`writ serve --dev`](run.md#writ-serve---dev), which runs the same code for you alone, it's SQLite.

Read this page to understand what happens to your plan and what protects it. To set one up, see [Run a control plane](run.md) and the [server config](config.md). [Repositories](repos.md) and [Approvals](approvals.md) cover how a plan refers to a commit and how it's approved and applied. [Not built yet](status.md#not-built-yet) lists what's missing.

## How a remote plan runs

Here's what happens when your GitLab CI job asks for a plan:

1. Your job asks GitLab for a token whose audience is the URL of `writ serve`. Then it runs `writ plan --remote --env prod --stack network --commit "$CI_COMMIT_SHA"`.
2. From there, `writ` sends the commit and no files. Outside CI, `writ plan --remote` sends the commit too, when you run it in a clean work tree of a [repository](repos.md) the control plane reads. If you have edits, it tells you why no commit can be planned.
3. Before it reads the request body, `writ serve` checks the token. Next it maps the token's claims to teams and checks for a `plan` grant on `network@prod`. For a commit, it takes the repository from the token's project id and checks that the Git host has the commit. Then it fetches the stack's files that it doesn't have yet, and checks each one against its Git blob id.
4. With STS, it assumes the environment's plan role. The role session name and the source identity identify your job, as [Roles and credentials](#roles-and-credentials) explains.
5. OpenTofu plans, with `-lock=false`, against the stack's state, and writes nothing. Your state sits behind the `http` backend that `writ serve` keeps, or in the stack's S3 backend. Your providers run under their own uid, with the run's credentials and nothing from the server's environment. See [How providers are kept from the keys](#how-providers-are-kept-from-the-keys).
6. Writ keeps the masked plan and the saved plan, [sealed](glossary.md) under the environment's key, and returns the plan's id. That id is the first 32 hex digits of a SHA-256 over the masked plan, the saved plan, the commit, the stack, and the environment.

At no point does your job see the state key, the saved plan, or a cloud credential. Your providers never see the state key or the host's own identity.

## OpenTofu stacks

To Writ, an OpenTofu stack is a directory with a committed `.terraform.lock.hcl`, `.tf` or `.tofu` files, and no `.writ` file, a file type Writ reserves. You commit it, and `writ serve` plans it by commit with OpenTofu. If a commit contains `.writ` files, Writ refuses it. To learn how such a plan is approved and applied, see [Approvals](approvals.md#how-an-opentofu-plan-is-approved-and-applied).

When `writ serve` plans your OpenTofu stack, it:

1. Gathers, at the commit, every file in the stack's directory and in the local modules it calls, whose `source` starts with `./` or `../`. [What a stack can contain](#what-a-stack-can-contain) lists the checks on those files.
2. Takes the stack's backend from its `terraform` block. If you write `backend "http" {}`, `writ serve` keeps the state. That means it serves the http backend under `/v1/tfstate/`, and gives each run the address and a token that reads that stack's state and nothing else. That token ends when the run ends. If you write `backend "s3"`, the state stays in the stack's own bucket, and the run reaches it with the environment's role, as [Stacks on S3](#stacks-on-s3) explains. Writ refuses a stack with no backend, an http block with anything in it, a `cloud` block, or any other backend, and says which backend it found.
3. Installs the providers that the lock file pins into `tofu.plugin_cache`. If Terraform wrote your lock file, the run uses a copy moved to OpenTofu's registry, as [Lock files that Terraform wrote](#lock-files-that-terraform-wrote) explains. This runs as `writ serve`'s own user, from a configuration it writes itself, so none of your stack's code runs as that user. This init sees the [filesystem view](#what-a-run-can-see) below, with the cache writable. A provider uid can read the cache but can't write it.
4. Runs `tofu init -lockfile=readonly`, `tofu plan -lock=false`, and `tofu show -json` as one of `provider_uids`. Because `init` fetches every module and provider the commit refers to, it gets the backend's address and no cloud credential. Both `plan` and `show` also get the environment's `provider_env` and its plan role, and no apply role. If your stack is on S3, `init` runs twice. First `tofu init -backend=false` fetches the modules and providers with no credential. Then `tofu init -get=false`, which fetches nothing, configures the backend with `provider_env` and the plan role, because it needs them to list the bucket.
5. Masks the plan's JSON and its events. Writ keeps only the masked plan document, the text made from it, and the saved plan, sealed under the environment's key. You can't fetch the saved plan from any API route. tofu's raw output never leaves the run's directory, which is deleted.

### What a stack can contain

Before you commit a stack, check it against the rules in this section, because Writ refuses a stack that breaks them.

Your stack needs its lock file. Writ also refuses a committed `.terraform` directory, and a `.tf.json` or `.tofu.json` file in the stack's directory or a module's. That's because `writ serve` reads module calls only from `.tf` and `.tofu` files, override files among them.

Write each module call's `source` as a quoted string in one of three forms. If the `source` isn't a quoted string, such as `var.module_path`, or isn't one of these, Writ refuses the call with its file and line:

- Local paths that start with `./` or `../` and stay inside the repository's path.
- A registry address, `NAMESPACE/NAME/SYSTEM`, with an optional host before it and `//SUBDIR` after it.
- A remote address that points to a network host. After an optional `git::`, `hg::`, `s3::`, `gcs::`, `http::`, or `https::`, it's a URL with scheme `https`, `http`, `ssh`, `git`, `s3`, or `gcs` and a host, `user@host:path`, or a `github.com/` or `bitbucket.org/` address.

On a remote address, you can use only the query parameters `ref` and `depth`, as in `git::https://git.example.com/infra.git?ref=v1.2.0&depth=1`. Writ refuses any other parameter and says which one. That includes `checksum`, `archive`, `filename`, `sshkey`, `rev`, the S3 parameters such as `aws_profile` and `aws_access_key_id`, and any unknown one. That's because go-getter reads them as orders, and `checksum=file:URL` reads a checksum file from any URL, including a path on the server, before any download.

Writ compares a parameter name after it decodes it, and refuses a `ref` or `depth` written with percent escapes. Writ also refuses a remote address with a `#` fragment.

Writ refuses every other source before tofu runs, because `tofu init` would copy or clone it from the server's own files. That covers an absolute path, a `file:` anywhere in the source, a forced getter before a path, as in `git::/srv/x` or `git::file:///srv/x`, a `~` path, a Windows drive or UNC path, a backslash, an `oci://` address, and any other getter.

To catch a disguised source, `writ serve` decodes percent escapes in the source until it stops changing. Then it looks for `file:` and a backslash both as written and decoded, so `%66ile%3A` is refused like `file:`. So is a malformed percent escape.

Your stacks have size limits as well. Writ refuses a stack, and says which file, when one of its `.tf`, `.tofu`, `.tfvars`, or lock files is over 1 MiB, any other file is over 16 MiB, or its files and its modules' files are over 2,000 files or 64 MiB. Writ counts every file, whether an earlier plan of the commit read it or not.

These limits leave room for a function's archive or another file your code reads, and they keep what one plan holds in memory and writes to its directory small. If a file is oversized, Writ refuses only the stacks that contain it, so discovery and the other stacks go on. OpenTofu code counts against these limits for each stack that contains it, and against no limit for the commit. So code elsewhere in your repository doesn't refuse a stack.

### Lock files that Terraform wrote

You can plan a stack whose `.terraform.lock.hcl` Terraform wrote, and you don't have to change your repository. Terraform pins each provider at `registry.terraform.io`. OpenTofu resolves a source with no host, such as `hashicorp/aws`, to `registry.opentofu.org`, so `tofu init -lockfile=readonly` refuses Terraform's lock file.

So for each run, `writ serve` writes its own copy of the lock file. In that copy, each `registry.terraform.io` provider moves to `registry.opentofu.org` at the same version. The commit's lock file stays as it is, and so does the stack's source digest. Writ never writes to your repository.

The hashes depend on the provider's namespace:

- A provider outside `hashicorp/`, such as `carlpett/sops`, keeps its hashes. OpenTofu's registry serves its author's release archives, the same ones Terraform's registry serves, and `tofu init` refuses an archive that doesn't match a hash.
- A `hashicorp/` provider, such as `hashicorp/aws`, loses its hashes. OpenTofu's registry serves OpenTofu's own builds of these, which aren't byte-for-byte the same, so Terraform's hashes can't match. `writ serve` installs OpenTofu's build, `tofu init` checks the signature that OpenTofu's registry serves with it, and `writ serve` records that build's hashes. Every run of that version checks the cache against them, and a plan and its apply use the same ones.

For a `hashicorp/` provider, your trust moves from the hashes you committed to the signature of OpenTofu's registry. To pin OpenTofu's hashes in your repository, run `tofu init` in the stack and commit the lock file it writes, as [Migrate from Terraform](migrate-from-terraform.md) explains.

A state that Terraform wrote names its providers at `registry.terraform.io` too. `tofu plan` maps those addresses itself, but `tofu show -json` of a state file loads the schema of each provider the file names, and fails when one isn't installed. So when [an adoption](adopt-state.md) or a [state index](#the-state-index) refresh reads a state file, Writ hands `show` a copy in which each resource's provider is the address the run's lock file pins: `registry.terraform.io/hashicorp/aws` becomes `registry.opentofu.org/hashicorp/aws` when the lock pins only the second. Nothing else in the copy changes, the backend's state isn't written, and the copy is deleted with the run's directory.

The plan's text says which providers moved, with a line such as `Warning: .terraform.lock.hcl pins registry.terraform.io/hashicorp/random, as Terraform writes it; the run used registry.opentofu.org/hashicorp/random at the same version, checked against the hashes of OpenTofu's build, which OpenTofu's registry signs`.

Limits:

- If your code sets `source = "registry.terraform.io/hashicorp/NAME"`, Writ refuses the stack, because OpenTofu refuses such a lock entry with the lock file read-only. Write `source = "hashicorp/NAME"` instead. A provider outside `hashicorp/` that your code points to at `registry.terraform.io` stays there. If other code in the stack names the same provider with no host, the copy pins it at both addresses, with the same version and hashes.
- Writ reads `source` in the stack's code and its local modules. A remote module that sets a `registry.terraform.io` source isn't read.
- `writ serve` keeps the recorded hashes in memory. After a restart, the first run of each `hashicorp/` provider version downloads it again from OpenTofu's registry.

### Policy and the mask

If your config has a `policy` block, its policy sets evaluate the masked plan document of every plan that OpenTofu created, and Writ refuses a plan that a policy denies. When someone asks to apply the plan, the same policies check it again. [Policy](policy.md) has the details.

Before the mask reads any of tofu's output, it starts from `provider_env`'s values and the secret set sealed with the state it reads. That's true for each plan, [drift check](#drift-checks), and apply.

Writ starts there because a state can keep a past secret where nothing marks it. For example, a resource keyed by `nonsensitive(var.key)` keeps the old key in its address after the key changes. Your next plan, which deletes that resource, shows the old key in no input and under no mark. So each apply seals what its mask knew with each state version or serial it wrote, as [The state index](#the-state-index) explains, and a set carries forward every secret an earlier apply knew.

That's why Writ keeps nothing of a plan's output until `writ serve` knows the state version or S3 serial the plan read and has given the mask that state's set. On the http backend, that's the newest version, which must be the same when the plan starts and when it ends. If an apply wrote a newer version during your plan, the plan keeps nothing, not even a refused plan's diagnostics, and tells you to plan again. On S3 it's the serial in the saved plan. A plan that tofu refused saved none, so its mask gets every set Writ keeps for the stack instead, as [What a run can see](#what-a-run-can-see) explains.

### States with no secret set

Some state versions and S3 serials have no secret set. They were written before `writ serve` kept secret sets, by an apply whose mask couldn't read its plan, or outside `writ serve`. Such a state can contain a secret that no set lists.

You can still plan against such a state, and on S3 the plan still records the serial it read, but it keeps no detail. You'll see its status as `failed`, and no one can approve or apply it. Its one diagnostic has class `state` and starts `no secret set for state version N of stack@env, so writ serve keeps no detail of a plan against it and never approves one`. On S3, N is the serial. Drift checks against it fail with only `drift unknown: no secret set for state version N of stack@env`.

On S3, this covers every stack whose bucket had a state before `writ serve` first planned it, and every stack after a write outside Writ. You can't approve the stack again until one of two things happens:

- someone [adopts the state](adopt-state.md), which seals a set with it, or
- an apply of `writ serve` writes a newer version from a plan against a version that has one.

On S3, a plan that tofu refuses before it saves a plan, as when the plan role lacks a permission, still shows each diagnostic's summary, swept. [What a run can see](#what-a-run-can-see) explains how.

### How an apply runs

When you apply an approved OpenTofu plan, Writ writes the commit's files into a new directory and runs `tofu init -lockfile=readonly` with no cloud credential. Then it runs `tofu apply` of the sealed saved plan as one of `provider_uids`, with the environment's `provider_env` and its apply role.

On the http backend, the run's token locks and writes the stack's state, and ends with the run. If your stack is on S3, its second `init` and the apply run with the apply role, and `tofu` takes and releases the bucket's lock itself. [Approvals](approvals.md#how-an-opentofu-plan-is-approved-and-applied) has the details.

### What a run can see

Your stack's code runs on the server, so Writ keeps it away from the host's files. Writ runs every `tofu` process in a private view of the filesystem. That covers `init`, `plan`, `show`, `apply`, Fill's `init`, and `writ serve --check`'s `tofu version`. So the code can't read the host's files with a `local_file` data source or `file()` of an absolute path, through a module host whose `X-Terraform-Get` or download location points to `file:///`, or through a link in a fetched module.

To build the view, `writ serve` starts a copy of its own binary in new user, mount, and pid namespaces. That copy mounts a tmpfs as the new root and binds into it the paths below, at the paths they have on the host, all but `/writ-tmp`. Next it mounts a read-only `/proc` of the new pid namespace, drops the old root with `pivot_root`, and makes the root read-only. Then it takes the provider uid, empties its capability bounding set, sets `no_new_privs`, and execs `tofu`.

Here's what the view contains:

| Path | Access |
| --- | --- |
| The run's directory: the stack's files, and `HOME` | read-write |
| `/writ-tmp`: the run's `HOME` bound again, and `TMPDIR`. Each provider creates its socket there, because a Unix socket's path must fit in 108 bytes, and the run's directory under a long data directory doesn't. If another path in the view, such as a `scratch_dir`, is at, under, or above `/writ-tmp`, the view uses the first free one of `/writ-tmp-1`, `/writ-tmp-2`, and so on | read-write |
| `tofu.plugin_cache` | read-only, and read-write for Fill's `init` |
| The `tofu` binary, its dynamic loader, and the libraries it loads, or a script's interpreter | read-only |
| With `tofu.git`: that `git`, and `git`, `git-remote-http`, and `git-remote-https` from its exec path, which a clone over https runs, with their dynamic loader and libraries | read-only |
| `/etc/resolv.conf`, `/etc/hosts`, `/etc/ld.so.cache`, when the host has them, and the host's CA bundle at `/etc/ssl/certs/ca-certificates.crt` | read-only |
| `/etc/nsswitch.conf`, written as `hosts: files dns`, so names resolve through `/etc/hosts` and DNS without the host's NSS services | read-only |
| `/dev/null`, `/dev/zero`, `/dev/random`, `/dev/urandom`, and `/dev/fd`, `/dev/stdin`, `/dev/stdout`, and `/dev/stderr` links into `/proc/self` | as on the host |
| `/proc`, of the run's own processes | read-only |

Nothing else from the host is in the view: no `/home`, no `data`, no `/tmp`, no shell, no `hg`, no `ssh`, and none of git's other programs. So if your stack needs `hg` for a module source, a shell for a `local-exec` provisioner, or an `external` data source, it fails. Your provisioner can still run a static program that's in the stack's files.

In each step's environment, `git` stays on https. Setting `GIT_ALLOW_PROTOCOL=https:ssh` overrides any `protocol.allow` setting, so `git` refuses `file://`, a local path, and `ext::`, even after a `url.*.insteadOf` rewrite. With `GIT_CONFIG_NOSYSTEM=1` and `GIT_CONFIG_GLOBAL=/dev/null`, the host's and the user's git config stay out.

If you use an `ssh://` or `git@host:path` source, it fails, because the view has no `ssh`. Even with it, `ssh` would need a key, known hosts, and a passwd entry for the provider uid, which the view doesn't have.

After each clone, `tofu init` runs `git submodule update --init --recursive`, and git 2.56's `git submodule` is a shell script. So the environment aliases `submodule` to `submodule--helper`, the builtin the script runs. Git reads that alias only when it finds no `git-submodule` program, and the view never has one. Writ fetches a module's submodules over https.

Writ leaves the network unchanged in the view, so `init` reaches the registry and module hosts. Because the run's processes live in their own pid namespace, they all die when `tofu` exits. If Writ can't create the view, the step fails with the reason, and `tofu` doesn't run.

When a step fails, it ends before the mask has read a plan. At that point the mask knows the environment's values, the token, and the secret set of the state the plan read, but none of the stack's other sensitive values. Yet a diagnostic's detail can show one of those, as `nonsensitive()` in a validation's `error_message` does.

So the refused plan keeps only each diagnostic's severity, summary, file, line, and column, swept by the mask. If a step wrote no diagnostics in JSON, as `tofu init` doesn't, it leaves only its name and exit code.

On S3, Writ doesn't know which serial a refused plan read, because tofu saved no plan. So the mask starts from the environment's values and every secret set `writ serve` keeps for the `stack@env`: those of its serials in `state_serials`, of every lineage, and those of its state versions from any time it was on the http backend. The mask sweeps every secret those sets list out of each summary. If the distinct secrets of those sets pass the mask's 4 MiB, Writ stops reading them and the plan fails with that reason.

The serial the plan read can still have no set, as one written outside Writ, or the state of a stack that Writ never applied or [adopted](adopt-state.md). Such a state can keep a past secret in an address keyed by `nonsensitive()`, so each quoted instance key in a summary becomes `["(sensitive)"]`, both as OpenTofu writes it, `["KEY"]`, and as a summary that quotes the address once more writes it, `[\"KEY\"]`. An integer key such as `[0]` stays. The rest of the summary shows, swept by the mask. So a provider's error shows, such as an AWS `AccessDenied` that names the action your plan role lacks.

**Limits.** The mask knows nothing of a state with no set. If a refused plan's summary quotes a value from such a state outside a quoted instance key, as an attribute set through `nonsensitive()`, an attribute the state marks sensitive, or a name built from `each.key`, the value shows, because no set lists it and the mask hasn't read the plan. So does a key quoted three times or more. Only those quoted keys and the values the mask starts from are swept: the sets, `provider_env`, and the run's own credentials.

Your stack's variables come from its committed `.tfvars` files and from the `TF_VAR_` names in `provider_env`.

**Under `writ serve --dev`**, OpenTofu comes from your `PATH`, and its directories are under the data directory. A run gets `PATH=/usr/local/bin:/usr/bin:/bin`, `AWS_EC2_METADATA_DISABLED=true`, and the `TF_VAR_` variables of the shell that started `writ serve --dev`, and no other variable from that shell. Its `HOME` is the run's own directory. If you add a `tofu` block in `writ serve --dev --config`, it replaces those directories and the binary, and when it sets `git`, Git's directory goes first on the run's `PATH`.

Dev mode needs no `provider_uids`: tofu and its providers run as the user who started `writ serve --dev`. On Linux they run in the filesystem view above, so a stack's code reads none of that user's files but the run's own, and still reaches the network as that user. Elsewhere tofu runs with that user's files, and `TMPDIR` is the run's `HOME` under the data directory. So a run refuses to start when a provider's socket there would pass the 103 bytes macOS allows. If you hit that, set `--data` to a shorter path. Plan only stacks you trust with that.

**Limits.**

- You need Linux for the namespaces. Elsewhere, only `writ serve --dev` runs tofu, and it runs without the view.
- The view's `/proc` mounts on any kernel when nothing covers part of `writ serve`'s own `/proc`. When something does, as systemd's `ProtectKernelTunables=` does, the view needs Linux 7.2 or later, which mounts the pid-only `/proc` the view asks for first. If you use [the NixOS module](run.md#on-nixos), it sets that option only on such a kernel.

## The state index

Use the state index to see what each OpenTofu stack's state manages. Writ keeps it in the control database, with one row for each managed resource of a `stack@env`, in the root module and in child modules. In each row you get the resource's address, type, and provider, and a few identifying attributes. You can read the rows of one `stack@env` from `GET /v1/index`, or see them in [the Resources view](#the-resources-view).

Those attributes come from an allow-list, and the table has a column for each one and for nothing else. They're `id`, `arn`, `identifier`, and `region`, and `namespace` and `modify_index` for a `nomad_job`. Writ leaves an attribute empty when the state marks it sensitive, when the mask sweeps its value, or when its name says it's a secret, as the mask's rule 4 decides.

Only the mask writes rows. Inside a run, it reads `tofu show -json` of a state. To keep it that way, `internal/archtest` refuses any package that reads that raw JSON other than by passing it to the mask's `Index`. That test finds `Index` by the callee's type, so a method called `Index` on another type doesn't pass.

Writ replaces a `stack@env`'s rows together, in one transaction, and never with the rows of a state version older than the one the index has.

### When the index is filled

**After an apply.** When `tofu apply` succeeds, the run runs `tofu show -json` of the state it left, in the same directory, with the backend's token and no cloud credential. By then the mask has learned the plan's secrets and the run's environment. Then it reads the state into rows when that state is the stack's newest version: the apply wrote it, or nothing wrote since the version the plan read. If this step fails, the apply doesn't. Writ logs the failure, and the next refresh reads the state.

**When a stack is first seen, and every five minutes.** On the http backend, a stack changes state only through an apply of `writ serve`. So its index is behind only when an apply wrote state and then failed, when the show after an apply failed, or when the state predates the index.

At start, and then every five minutes, `writ serve` looks for each `stack@env` whose newest state version is newer than its index, up to 50 at a time, and refreshes it with a run. That run gets the files of the commit applied by the apply that wrote that version. Next it runs `tofu init -backend=false`, then `tofu show -json` of the stored state, which it gets as a file. It gets no cloud credential, no `provider_env`, and no token for the state backend, and it runs as one of `provider_uids` in the filesystem view.

Here the refresh has a problem to solve. Your state can contain a secret that nothing in it marks, as an address keyed by `nonsensitive()` of a sensitive variable does, and the refresh has no plan to learn the variable's value from. So each apply seals what its mask knew under the environment's key with each state version it wrote, meaning `provider_env`'s values, the secret set of the version its plan read, and the plan's secrets. The refresh's mask starts from `provider_env`'s values and those of the version it reads. When a version goes, its sealed secrets go with it, and no API returns them.

If a version doesn't have them, such as one written before `writ serve` kept them, Writ doesn't index it. Instead, the refresh logs why, and the index stays at its older version until an apply writes a newer one or someone [adopts](adopt-state.md) the version. If your stack has no state, there's nothing to index. If a refresh fails, Writ logs it and tries again five minutes later.

### The Resources view

On an OpenTofu stack's page, the Resources view shows you the rows of its state index from `GET /v1/index`. You see each resource's address, type, provider, and the attributes in the index's allow-list, and the state version or S3 serial the rows came from. You never see any other attribute of the state there.

Grouped by service or by module, a row shows the resource's name and key, such as `app["arn:aws:iam::aws:policy/ReadOnlyAccess"]`, with its type in words under it. The flat view, and the row when you open it, show the whole address. When a resource's ID is its ARN, as for a load balancer, the row shows the name the ARN ends with, and the opened row shows the ARN once. The opened row also shows the provider without its registry host, such as `hashicorp/aws`, and the region from the index or the ARN. A row shows its module as a chip when the resource isn't in the root module.

## Stacks on S3

If your stacks keep their state in S3, they can stay there. Your stack keeps its state in its own bucket with a block such as:

```hcl
terraform {
  backend "s3" {
    bucket       = "acme-tofu-states"
    key          = "network/terraform.tfstate"
    region       = "eu-west-1"
    use_lockfile = true
  }
}
```

Writ runs such a stack with the environment's roles, and gives `tofu` no token for the http backend. Writ refuses your stack on S3 in these cases:

- Without `use_lockfile = true` or a `dynamodb_table` that isn't empty. With the lock, an apply of `writ serve` and a write from elsewhere can't change the state at once.
- With any attribute or block but these, read from the backend's schema in OpenTofu 1.13.9: `bucket`, `key`, `region`, `workspace_key_prefix`, `use_lockfile`, `dynamodb_table`, `encrypt`, `kms_key_id`, `use_path_style`, `force_path_style`, `use_dualstack_endpoint`, `use_fips_endpoint`, `skip_credentials_validation`, `skip_region_validation`, `skip_requesting_account_id`, `skip_s3_checksum`, `max_retries`, `retry_mode`, and `endpoints`. Any other attribute sets credentials, a role, a profile, a file, a proxy, a CA bundle or a custom endpoint, turns off the TLS check, or sets the state object's ACL. Writ gives `tofu` the credentials of the environment's role, and your stack mustn't choose others or send the role's signed requests elsewhere.
- With a value set by an expression, such as a variable, because `writ serve` checks what the block says. Write each value as a literal.
- With `endpoints` that list a URL that the environment's `s3_endpoints` doesn't list exactly. Since a custom endpoint gets every request the role signs, only the control plane's config can choose one. Instead of the deprecated `endpoint`, `dynamodb_endpoint`, `iam_endpoint`, and `sts_endpoint`, which Writ refuses, write `endpoints = { s3 = "URL" }`. In `s3_endpoints` you can write `https://HOST`, or `http://` on a loopback address, as `aws.sts_endpoint` does.
- In an environment with no `plan_role`, for a plan, or no `apply_role`, for an apply.
- Under `writ serve --dev`, which has no credential broker.

Give your roles these permissions on the bucket. Writ checks none of them for you, and the local test proves only that a plan succeeds with a role that can't write:

| Role | Needs | Must not have |
| --- | --- | --- |
| `plan_role` | `s3:ListBucket` on the bucket, and `s3:GetObject` on the state's key | `s3:PutObject` and `s3:DeleteObject` on the key and on `KEY.tflock` |
| `apply_role` | `s3:ListBucket`, `s3:GetObject` and `s3:PutObject` on the key and on `KEY.tflock`, and `s3:DeleteObject` on `KEY.tflock` | |

Plans write nothing to the bucket: they run with `-lock=false`, and `just e2e-tofu-s3` lists the bucket before and after each plan.

**Warning:** Your plan and apply roles, not Writ's grants, are the boundary between the states of an environment's stacks. Your stack chooses its bucket and key, and the environment's roles reach whatever their policies allow. Grants, as in [Teams and grants](#teams-and-grants), don't keep one stack's state key from another's. If a stack's backend points to another stack's key, it runs with the same roles, and reads or writes that state. When stacks mustn't read each other's state, scope the roles' S3 permissions to one environment's keys, and give each such stack its own environment, with roles scoped to its keys.

### How Writ reads the serial

Because `writ serve` doesn't see writes to the bucket, it reads the state's serial and lineage at each run and on a timer. Writ keeps each serial it reads in `state_serials`: up to 20 for each `stack@env` from those an apply wrote, and 20 from the others.

**A plan** reads the serial of the state it planned from its saved plan. That's the serial `tofu apply` checks before it applies.

**An apply** runs `tofu state pull` with the apply role after `tofu apply` ends, however it ends. Then it runs `tofu show -json` of the pulled file with no credential. Writ counts the pulled serial as the apply's when both of these hold:

- the apply succeeded, or failed after it began to change a resource, which it does only while it holds the lock.
- the serial is of the lineage its plan read, and above the serial its plan read.

Any serial above the one its plan read is the apply's too, because OpenTofu saves the state during a long apply. With that serial, the apply seals what its mask knew: the environment's secrets, the secret set of the serial its plan read, and the secrets it read from the plan, as it does for a version on the http backend. Then it fills the index too, so a plan after a partial apply masks the state as one after a whole apply does.

If an apply failed before it changed anything, it wrote nothing and claims no serial. That's the case when OpenTofu refused it because the state changed after the plan, or when it couldn't take the lock. Neither does an apply that left another lineage. On the next read, Writ treats such a serial as any other: as drift, when no apply of `writ serve` wrote it.

**At start and every five minutes,** a run reads each `stack@env` on S3 that an apply of `writ serve` wrote, and that no apply is running on. That run gets the files of the commit that apply applied, runs the two `init` steps and `tofu state pull` with the plan role, and runs `tofu show -json` of the pulled file with no credential, as one of `provider_uids` in the filesystem view. Its role session's name starts with `writ-index-`. On each look it reads up to 50 stacks: first those it's never read, then those it read longest ago, so every stack gets a turn however many there are.

### Writes outside Writ

When someone writes the state from outside Writ, as a `tofu apply` from a laptop does, Writ notices at its next read.

Writ starts from the first serial `writ serve` reads of a `stack@env`. Unless an apply of `writ serve` wrote it to an empty bucket, no secret set was sealed with it. So if your bucket already had a state, a plan of the stack keeps no detail. You can't approve it until someone [adopts](adopt-state.md) that serial, as [States with no secret set](#states-with-no-secret-set) explains.

After that, any serial that `writ serve` hasn't read and no apply of it wrote is drift. Writ marks it on the stack's record in `state_index_stacks`, as the serial and the time it read it. Then it logs `state written outside writ` at level `WARN` with the stack, the serial, the lineage, and the bucket and key. Beyond that, it records no change and sends no alert.

You see the mark as `outside_write` in `GET /v1/drift` and `GET /v1/index`, and on the stack's page. That mark stays until a later drift replaces it. Writ can't tell you who wrote the state, because S3 doesn't say. If Writ read a serial while an apply of the stack ran, it checks that serial at the next read.

Writ doesn't index a serial that no apply of `writ serve` wrote. Your state can contain a secret that nothing in it marks, as an address keyed by `nonsensitive()` of a sensitive variable does, and only the mask of the apply that wrote a serial knew its plan's secrets. Indexing such a serial with only the secrets of `provider_env` could store such a secret.

So the refresh logs why, and the index stays at the serial Writ wrote until an apply of `writ serve` writes a newer one or someone [adopts](adopt-state.md) the serial. For the same reason, a plan of such a serial keeps no detail and can't be approved. Writes between two reads show as one drift, at the serial the second read finds.

Reads are ordered. Whether a plan, an apply, or a refresh reads a serial, the read takes the next number among the reads of its `stack@env`. In the same transaction, a plan's or a refresh's read checks for an apply of the stack, after it locks the `stack@env`'s row, which an apply's record of its serial locks too. So an apply that ends while a read is recorded is seen, or its own read takes the later number.

Writ stores an index only from the newest read of its `stack@env`, of whatever lineage and serial, checked in the transaction that stores it. When a later read follows, even a plan's, or a refresh's that found the index already has its serial, the older read's index is dropped and logged at level `DEBUG`, and the next refresh indexes the state again.

OpenTofu can encrypt a state or a saved plan, and Writ can't read the serial of either. So if your stack uses that encryption, Writ refuses its plan.

Here's what `writ serve` logged at its refresh after a `tofu apply` with the bucket owner's keys and a restart:

```
{"time":"2026-11-14T20:13:52.088355974+05:30","level":"WARN","msg":"state written outside writ","stack":"app","env":"prod","serial":2,"lineage":"6f8c0549-9d28-f161-88b8-188caefb612d","object":"s3://writ-states/app/terraform.tfstate"}
{"time":"2026-11-14T20:13:52.089272316+05:30","level":"ERROR","msg":"refresh the state index of a stack on S3; the next look tries again","stack":"app","env":"prod","err":"serial 2 of the state is not indexed: no apply of writ serve wrote it, so no mask knew its plan's secrets; the index stays at its older serial until an apply of writ serve writes a newer one"}
```

Output of `just e2e-tofu-s3`, 2026-11-14.

### Limits

There's a short window in which Writ can claim someone else's write. That's because `tofu apply` lets go of the lock before `writ serve` pulls the state, and OpenTofu 1.13.9 reports no serial it wrote in its `-json` output. If a write from elsewhere, of the same lineage, lands between the two, Writ claims it as the apply's, and fills the index from it with the apply's secrets and the environment's. That window is the time one `tofu state pull` takes after the apply.

## Drift checks

You want to know when your real infrastructure no longer matches what you applied. Writ finds that with drift checks: plans that `writ serve` creates on a timer to find what changed since an apply. Drift checks are off until you add a block next to `limits`:

```hcl
drift {
  every = "6h"
}
```

To do that, a check reads every resource of a stack through its providers, with the plan role. That has a cost, and you choose it: a stack of 200 resources checked every six hours costs about 800 reads of the cloud's API a day.

### What a check plans

- **Which stacks.** Any OpenTofu `stack@env` whose state `writ serve` keeps on the http backend, indexed, or read from S3, in an environment it serves.
- **Which code.** The head of the default branch of the stack's repository, as `writ serve` last read it, which `poll_every` sets. When several repositories have the stack at their heads, a check uses the one whose code `writ serve` applied last to the `stack@env`. If no head has the stack, the check fails and says why.
- **How.** Like a commit's plan: the plan role, `tofu plan -lock=false`, the filesystem view, a state token that reads the stack's state on the http backend, and the mask. Because a plan of a stack on S3 reads the serial, a check also finds a write outside Writ, as [Stacks on S3](#stacks-on-s3) explains. Writ keeps only the mask's output. Writ drops the saved plan, and no [policy](policy.md) checks the plan.
- **When.** A check is due when no check of the `stack@env` started within `every`. Also, it's due when one of these happened after its last check started: an apply of `writ serve` on it ended, `writ serve` read a serial on S3 that no apply of it wrote, or someone [adopted](adopt-state.md) its state. Writ looks for due checks at start and then every quarter of `every`, at most every 30 seconds. Those checked longest ago start first, so every stack gets a turn. If a check is planning a `stack@env`, or an apply is applying it, right now, it isn't due.
- **How many at once.** Checks take the plan slots of `limits.plans`, which every plan shares, and at most `limits.plans_per_principal` of them. Checks never take the last free slot, which stays free for people's plans. So if you add a `drift` block, set `limits.plans` to at least 2. If a due check finds no slot, it waits for the next look.

### What a check reports

Writ runs a normal plan for a check, not `tofu plan -refresh-only`. OpenTofu 1.13.9 writes both kinds of change into one plan's JSON, so a check reports them separately:

- **`outside`**: a resource that OpenTofu read changed outside OpenTofu, from the plan's `resource_drift`, such as a setting changed in the console or a file deleted. With a refresh-only plan, you'd see only these.
- **`code`**: a change an apply of the code at the head would make, from `resource_changes`. This kind also finds a state that someone wrote outside Writ, as a `tofu apply` from a laptop to a bucket does, and a merged commit that no apply has applied yet.

If the check's plan changes something, Writ marks the `stack@env` drifted. In the mark you get each changed address, its kind, its action, and the names of the top-level attributes that change, from the masked plan. Values are never kept. Read `drifted_since` as when the first of an unbroken run of drifted checks ended. When a check's plan changes nothing, it clears the mark.

If a check fails, as when OpenTofu refuses the plan, it keeps the reason, swept by the mask, and leaves no mark. A check whose plan runs against a state with no secret set fails with only `drift unknown: no secret set for state version N of stack@env`, as [States with no secret set](#states-with-no-secret-set) explains. A check that OpenTofu refuses on S3 keeps its summaries, swept as a refused plan's are.

If an apply of the same `stack@env` ran during a check, the check keeps nothing, because it may have read a state the apply was changing. Then the stack is due again at the next look. When it finds drift, `writ serve` logs `drift found` at level `WARN`, with the counts and the plan id, and sends no alert.

You can't approve or apply a drift check's plan. Checks are kept in their own table, apart from plans, and keep no saved plan. If you send the id of a check's plan to `POST /v1/plans/{id}/approvals` or `POST /v1/runs`, a principal that can read the stack gets `409` with class `state`, saying it's a drift check. Anyone else gets `no plan`.

Expect a `local_file` or `local_sensitive_file` resource to show as deleted outside OpenTofu in every check. That's because each run starts in a new directory, so the file its apply wrote isn't there.

### Where you see the result

Writ keeps the newest 20 checks of each `stack@env`. From `GET /v1/drift` you get the newest one that ended, and you see it on the overview and on each stack's page in the [control UI](sign-in.md#the-control-ui).

In this example, `writ serve` restarted, and its check at start planned app after its key changed. Its state keeps the old key in the address of `terraform_data.keyed`, and the mask swept it with the secret set of state version 3. Here's the answer to `GET /v1/drift?env=prod&stack=app`:

```json
{
    "diagnostics": [],
    "format": "writ.drift/v0",
    "more": false,
    "ok": true,
    "stacks": [
        {
            "backend": "http",
            "check": {
                "changes": [
                    {
                        "action": "create",
                        "address": "terraform_data.keyed[\"(sensitive)\"]",
                        "kind": "code"
                    },
                    {
                        "action": "delete",
                        "address": "terraform_data.keyed[\"(sensitive)\"]",
                        "kind": "code"
                    }
                ],
                "checked_at": "2026-11-15T06:22:51Z",
                "code": 2,
                "commit": "dda65cefe05a83f1433a335c880fdfb4a1d7a51d",
                "drifted_since": "2026-11-15T06:22:51Z",
                "error": "",
                "outside": 0,
                "plan": "703a6a51ed7d286e9bdbe4f62eeaac6d",
                "repo": "infra",
                "started_at": "2026-11-15T06:22:51Z",
                "status": "drifted"
            },
            "checking": false,
            "env": "prod",
            "indexed_at": "2026-11-15T06:22:50Z",
            "indexed_version": 3,
            "outside_write": null,
            "resources": 5,
            "stack": "app"
        }
    ]
}
```

Output of `just e2e-tofu`, whose test `just e2e` now runs, 2026-11-15. That test printed it on one line, and it's indented here with `python3 -m json.tool`.

And here's the check of app after a `tofu apply` with the bucket owner's keys wrote serial 3 outside Writ. Since no secret set was sealed with serial 3, the check keeps only that:

```json
{
    "diagnostics": [],
    "format": "writ.drift/v0",
    "more": false,
    "ok": true,
    "stacks": [
        {
            "backend": "s3",
            "check": {
                "changes": [],
                "checked_at": "2026-11-15T06:25:55Z",
                "code": 0,
                "commit": "c74e301354ad2a469009acb0b979b455c3966b8e",
                "drifted_since": "",
                "error": "drift unknown: no secret set for state version 3 of app@prod",
                "outside": 0,
                "plan": "",
                "repo": "infra",
                "started_at": "2026-11-15T06:25:54Z",
                "status": "failed"
            },
            "checking": false,
            "env": "prod",
            "indexed_at": "2026-11-15T06:24:57Z",
            "indexed_version": 2,
            "outside_write": {
                "seen_at": "2026-11-15T06:25:25Z",
                "serial": 3
            },
            "resources": 3,
            "stack": "app"
        }
    ]
}
```

Output of `just e2e-tofu-s3`, 2026-11-15, printed and indented the same way.

## Teams and grants

You decide who can do what with teams and grants. Teams say who a token belongs to, and grants say what a team can do.

Think of a team as a rule over the claims of one issuer's tokens, and a token is a member of every team whose rule it matches. Here's how a rule matches your tokens:

- All keys in `match` must match. One matches when the claim's value, or any element of a list claim, equals one of the listed strings. Numbers and booleans compare as their JSON text.
- A listed value that ends in `/*` matches one more path segment, and one that ends in `/**` matches any number.
- A missing claim never matches. If a team has no rule, it has no members.

With a grant, you give teams operations on stacks in environments. Grants only add.

- In `can`, you list `read`, `plan`, `apply`, `approve`, `override`, `adopt`, and `admin`. [Approvals](approvals.md) says what `apply`, `approve`, and `override` allow, and [Adopt a state](adopt-state.md) what `adopt` allows.
- `stacks` lists stack names or patterns, as [Stack patterns](#stack-patterns) describes. For `envs`, list names or `*`.
- `risks` lists the risk classes a team can apply, by the names of the [risk classes](glossary.md), which are `create`, `update`, `replace`, `delete`, and `data-loss`. On a live plan, `teams_that_may_apply` lists the teams whose grants cover its risk classes.

Grants match a stack by its name. That's its path from the Git work tree that contains it, or its directory's name outside one, as `writ plan --remote` reads it. You can set it with `--stack NAME`.

When your job is refused, run `writ whoami` first. That command prints what a token can do: the control plane and where it was found, who the token speaks for and where the token came from, the teams, and one line for each grant with its environments, stacks, operations, and risk classes. [writ serve --dev](run.md#writ-serve---dev) shows it for the dev user.

### Stack patterns

In a grant or an approval rule, you can match stacks with a pattern. Split at `/`, a pattern's parts match the parts of a stack name. In a part, `*`, `?`, and `[...]` work as in Go's `path.Match` and never cross a `/`, so `*` matches `network` and not `apps/api`.

When a part is `**` alone, it matches any number of parts, including none. So `data/**` matches `data`, `data/pg`, and `data/pg/replica`, `**/pg` matches `pg` and `eu/data/pg`, and `**` matches every stack.

When you run `writ serve --check`, it refuses a pattern with `**` inside a part, such as `apps**`, and one with `**` twice, such as `a/**/b/**`. You never need a second one: one `**` already matches any number of parts, and a pattern with one takes a single pass over the name.

**If your config is from before 2026-11-12,** check your patterns. Before that date, `**` in a grant or approval rule matched one part, like `*`. Now it matches any number, so a config with `stacks = ["**"]` or `apps/**` covers deeper stacks than before. For example, `**` with `admin` on every environment now also allows adding a repository. Write `*` where a grant meant one part.

### Who a person is

Approvals depend on knowing who a person is: a plan's author can't approve it, and each person counts once. So Writ gives each person an id that approvals compare.

In your config, every issuer has `identity`, the name of the Git host or identity provider whose user ids its tokens carry. Writ writes a person as `IDENTITY/user/ID`, with the user id from `user_id` of a GitLab CI token, `actor_id` of a GitHub Actions token, or `sub` of a people issuer's ID token.

Give the issuers of one Git host one identity, such as the name of its `forge` block. In the [sample config](config.md#sample-config), the CI issuer `gitlab` and the people issuer `sso` both take `identity = "gitlab"`. So a job that alice started and alice's session are one person, `gitlab/user/1001`.

Issuers of two Git hosts take two identities, because user 1001 of one GitLab isn't user 1001 of another. GitHub.com's Actions issuer and a GitHub Enterprise Server's are two Git hosts too.

Identities are names and not the issuers' URLs, because a URL can spell one host many ways. Person ids once contained the host. Then two issuers of one GitLab at `https://GITLAB.example.com` and `https://gitlab.example.com:0443`, or at an internationalized name and its `xn--` form, or at an IPv6 address written two ways, gave alice two ids, and alice approved their own plan with the second. Now no spelling of a URL changes an identity. Writ still checks each token against its issuer's exact URL.

When you run `writ serve --check`, it refuses an issuer without `identity`, and an identity that isn't a lowercase name or is `dev`, the namespace of the one user of `writ serve --dev`. You also get a warning when issuers of two identities, or a `forge` block named like one of them, have URLs that look like one host once case, IDNA, a trailing dot, the form of an IP address, and the default port are set aside. That check is best effort and decides nothing: the config's identities do.

If you rename an identity, you rename every person in it. Writ refuses an approval from a session, or for a plan's author, whose id has an identity the config doesn't define. You sign in again, and create the plan again.

Approvals kept under an identity the config no longer defines count nowhere: not toward a plan's approvals, not in its status, and not when an apply after a merge picks a plan or a run begins. The control database keeps them, so the plan's approvers stay on record. If you approved before a rename, approve again under the new identity, and you count once. Rolling back the rename makes the kept approval count again.

## Roles and credentials

When an environment has `plan_role`, Writ plans with that role's credentials. Before any provider of a plan starts, `writ serve` calls `sts:AssumeRole` as its own AWS identity, which the AWS SDK's default chain finds: environment variables, shared files, or the instance's role. Here's what the request sets:

| Field | Value |
| --- | --- |
| `RoleArn` | The environment's `plan_role` |
| `RoleSessionName` | `writ-WHO-ID`. `WHO` is `ci` for a CI job, the person's display name for a `writ login` session, and `drift` for a drift check. `ID` is the plan's ref for a plan, the run id for an apply (`writ runs --remote` lists it), `adopt-` and an id for an adoption, and `index-` and an id for a read of the state index. AWS allows 64 characters, so a long `WHO` is cut and `ID` is kept. |
| `SourceIdentity` | `gitlab-PROJECTID-PIPELINEID` for a GitLab CI job, `github-REPOID-RUNID` for a GitHub Actions run, the person, such as `gitlab-user-2002`, for a session, and `writ-serve-drift` or `writ-serve-index` for a drift check or a read of the state index. In both fields, a character STS refuses becomes `-`. |
| `DurationSeconds` | `aws.session`, one hour by default |

So you can trace each session in CloudTrail. CloudTrail records the session name and the source identity with the call, and AWS keeps the source identity on every session created from this one.

Your role's trust policy must allow `sts:AssumeRole` and `sts:SetSourceIdentity` to the control plane's identity. If an environment has no `plan_role`, its providers get no AWS credential.

Writ doesn't hand the credentials to the providers directly. Instead, `writ serve` serves them on a loopback endpoint in the AWS SDKs' container credentials protocol. Your providers get `AWS_CONTAINER_CREDENTIALS_FULL_URI` and `AWS_CONTAINER_AUTHORIZATION_TOKEN`, a random token for the run. That endpoint assumes the role again 15 minutes before the credentials expire, and refuses the token once the plan ends. Ending the token doesn't revoke credentials a provider already fetched, so the run's mask knows the secret key and session token of each set it served, and sweeps them from everything the run keeps, as [Secrets and the mask](secrets.md#how-the-mask-works) explains. If STS refuses the role, the plan fails before any provider starts, with a `denied` diagnostic that names the role.

In a provider's environment you'll find the endpoint and its token, the environment's `provider_env` files, `PATH=/usr/local/bin:/usr/bin:/bin` (with Git's directory first when the `tofu` block sets `git`), `AWS_EC2_METADATA_DISABLED=true`, `HOME` and `TMPDIR` set to the provider's own directory, and the plugin settings. Nothing comes from the environment of `writ serve`.

In `provider_env`, Writ refuses names that start with `AWS_`, `TF_`, `PLUGIN_`, or `WRIT_`, and `HOME` and `TMPDIR`. You can use names that start with `TF_VAR_`, which set the variables of an [OpenTofu stack](#opentofu-stacks).

### How it's tested

To test this, `just test-serve` plans a stack on the real `hashicorp/aws` 6.70.0 through `writ serve` against an STS stand-in. Here's the AssumeRole request the stand-in received, signed with the control plane's key from the environment of `writ serve`:

```
signed by AKIACONTROLPLANE0001 for us-east-1/sts, Action=AssumeRole&DurationSeconds=3600&RoleArn=arn%3Aaws%3Aiam%3A%3A222222222222%3Arole%2Fwrit-plan&RoleSessionName=writ-ci-OH7MRPZTDKWIQBRXFHJDKV3GAD&SourceIdentity=gitlab-4242-1849213&Version=2011-06-15
```

And here's the line `writ serve` logged for it:

```
{"time":"2026-11-10T19:21:56.87338551+05:30","level":"INFO","msg":"assumed role","component":"credentials","role":"arn:aws:iam::222222222222:role/writ-plan","session":"writ-ci-OH7MRPZTDKWIQBRXFHJDKV3GAD","source_identity":"gitlab-4242-1849213","why":"start","access_key_id":"ASIATEST000000000001","expires":"2026-11-10T14:51:56Z"}
```

Output of `just test-serve`, 2026-11-10.

In that test, the provider checked its credentials with `GetCallerIdentity`, signed with `ASIATEST000000000001`, the key STS issued to that session. Even though the control plane's own key was in the environment of `writ serve`, and the AWS SDKs prefer it to the endpoint, it never signed a provider's call.

Another test gives the real provider credentials that last five seconds. One provider process signed its first call with `ASIATEST000000000001` and, after they expired, its second with `ASIATEST000000000002`, which the endpoint had assumed anew. So the AWS provider fetches again from the endpoint, and a long run outlives its first credentials.

## How providers are kept from the keys

Your plan runs code that the merge request chose: the providers, with the settings the code gives them. So Writ keeps that code away from the keys.

When you set `provider_uids`, `writ serve` runs each plan's providers under their own uid from that range, with the group of the same number and no supplementary group. Two plans that run at once get two uids, so a provider of one can't read the other's environment, which has that run's token. You don't need a user on the host for the uids.

- Inside its own directory, a provider has its plugin socket, and that directory is its `HOME` and `TMPDIR`. You'll find it in `/tmp`, owned by the provider's uid, with mode 0700. Writ creates it and connects to the socket with the provider's filesystem ids, on one thread.
- `writ serve` needs `CAP_SETUID`, `CAP_SETGID`, and `CAP_KILL`, and refuses to start without them. At start it clears its inheritable and ambient capabilities, which systemd's `AmbientCapabilities` sets, because a process keeps ambient capabilities across exec. If a provider starts with any capability, Writ stops it, and the plan fails.
- When a plan ends, `writ serve` kills every process left running as its uid before another plan gets the uid. At start it does the same for every uid in the range.
- Make sure the uids can't read `data`, the state keys, the `provider_env` files, or the TLS key. With peer authentication, as the NixOS module sets up, a provider's uid has no role in Postgres.
- A firewall rule drops the uids' traffic to the instance metadata service, so a provider can't reach the host's identity, which may assume every role:

```
table inet writ {
  chain out {
    type filter hook output priority 0;
    meta skuid 61000-61063 ip daddr 169.254.169.254 drop
    meta skuid 61000-61063 ip6 daddr fd00:ec2::254 drop
  }
}
```

For an OpenTofu stack, `tofu` and its providers run as the provider uid inside the [private filesystem view](#what-a-run-can-see), so they can't read a world-readable file of the host either. For the view, Writ needs user, mount, and pid namespaces, which an unprivileged user can create. Writ's `CAP_SETUID` and `CAP_SETGID` let it map the provider uid into the view's user namespace.

If an environment in your config sets a role, or the config has a `tofu` block, you must set `provider_uids`. Your OpenTofu stack runs arbitrary code from the commit, such as a `local_file` data source that reads a path on the host, so a team's `writ serve` never runs tofu as its own user.

Both `--check` and start refuse a `tofu` block without `provider_uids`, and a plan that would run tofu with no provider uid fails before tofu runs. Without `provider_uids`, your config plans no OpenTofu stack, and `--check` warns you about it.

### Check the isolation

Before you trust the isolation, check it. Run `writ serve --check`, and it runs `writ` as the first uid of the range, with nothing from its own environment, and prints what the kernel let that uid do. If any line fails, `--check` fails, so a unit that runs it first doesn't start. You'll see `not checked` on a metadata line where `writ serve` itself reaches no metadata service.

```
$ writ serve --config /srv/writ/serve --check
config /srv/writ/serve: ok
url: https://writ.example.com
issuers: gitlab (gitlab-ci, https://gitlab.com), sso (people, https://gitlab.com, client 8f1c0b7e4d2a)
control UI: https://writ.example.com/ui/, sign-in redirects to https://writ.example.com/ui/callback, sessions last 12h0m0s
envs: prod, staging, state in postgres://writ@/writ
trusted proxies: 127.0.0.1/32; X-Forwarded-For names the client only when one of them sends it
providers run as: uids 61000-61063, one for each running plan
tofu: /nix/store/60vqi4v8n11afd5ks89j1kn7pgwfrww5-opentofu-1.13.9/bin/tofu, plugin cache /var/lib/writ-tofu/plugins, scratch directories in /var/lib/writ-tofu/runs
aws: sts in eu-west-1 at the regional endpoint, sessions of 1h
role: prod plan arn:aws:iam::222222222222:role/writ-plan
role: staging plan arn:aws:iam::111111111111:role/writ-plan
approvals: no rule, so no plan needs an approval
TEAM      ENV           STACKS  CAN           RISKS
infra_mr  staging,prod  *       read plan     -
platform  staging,prod  *       read approve  -
tofu: /nix/store/60vqi4v8n11afd5ks89j1kn7pgwfrww5-opentofu-1.13.9/bin/tofu is OpenTofu 1.13.9: ok
isolation: uid 61000 cannot read data, /var/lib/writ: ok
isolation: uid 61000 cannot read the state key of env prod, /var/lib/writ/keys/prod.key: ok
isolation: uid 61000 cannot read the state key of env staging, /var/lib/writ/keys/staging.key: ok
isolation: uid 61000 cannot read provider_env NOMAD_TOKEN of env prod, /var/lib/writ/secrets/nomad_token: ok
isolation: uid 61000 cannot reach 169.254.169.254:80: not checked: writ cannot reach it either, so this host has no metadata service there to protect
isolation: uid 61000 cannot reach [fd00:ec2::254]:80: not checked: writ cannot reach it either, so this host has no metadata service there to protect
```

Output of a real run, 2026-11-16, as root of a user namespace with subordinate uids and with only loopback, so neither metadata address answers. Its config has a `tofu` block and a people issuer, its key files and `provider_env` file are mode 0600, and its data directory is mode 0700.

Another test in `just test-serve` runs a provider the way a test runs `writ serve` under systemd: as a user other than root, with `CAP_SETUID`, `CAP_SETGID`, and `CAP_KILL` ambient and `no_new_privs` set, in a network namespace that answers on both metadata addresses. Before the capabilities are cleared, the provider is refused. After, the go-plugin handshake and a call to the provider work across the two users, and the provider's own report says what it couldn't do:

```
the provider saw: key "open /tmp/TestProviderIsolation3679868081/001/keys/prod.key: permission denied", environ "open /proc/2405915/environ: permission denied", "Get \"http://169.254.169.254:80/latest/meta-data/iam/security-credentials/\": context deadline exceeded (Client.Timeout exceeded while awaiting headers)", "Get \"http://[fd00:ec2::254]:80/latest/meta-data/iam/security-credentials/\": context deadline exceeded (Client.Timeout exceeded while awaiting headers)"
```

Outside the provider, the test process reached both addresses, and with the rule moved to other uids the provider reached `169.254.169.254` too.

## Next steps

- [Run a control plane](run.md) and the [server config](config.md): set one up.
- [API](api.md) and [JSON output](json-output.md): the routes, and the documents the API carries.
- [Architecture](architecture.md): the package map, and the decisions behind it.
