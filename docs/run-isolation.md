# Run isolation and egress

Isolate OpenTofu and provider processes from `writ serve`'s keys, database, and host identity, and understand what outbound traffic Writ restricts.

Because a merge request chooses the OpenTofu code and providers that run on your server, a team's `writ serve` never runs OpenTofu as its own OS user. For the underlying architecture, see [How the control plane works: How providers are kept from the keys](serve.md#how-providers-are-kept-from-the-keys).

## Prerequisites

- A Linux host. (User, mount, and PID namespaces and `provider_uids` require Linux.)
- A range of unused numeric UIDs on the host, with at least as many UIDs as `limits.plans` (for example `61000-61063`). They do not need `/etc/passwd` entries.
- `CAP_SETUID`, `CAP_SETGID`, and `CAP_KILL` granted to the `writ serve` service (the [NixOS module](run.md#on-nixos) configures these automatically).
- `nftables` (or an equivalent firewall) on hosts with an instance metadata service.

## Steps

### 1. Reserve a `provider_uids` range and separate directories

In `/etc/writ/serve`, set `provider_uids` and place `tofu.plugin_cache` and `tofu.scratch_dir` outside `data`:

```hcl
data          = "/var/lib/writ"
provider_uids = "61000-61063"

tofu {
  binary       = "tofu"
  plugin_cache = "/var/lib/writ-tofu/plugins"
  scratch_dir  = "/var/lib/writ-tofu/runs"
  git          = "git" # optional; needed only for git::https:// or github.com/ modules
}
```

Set tight permissions on everything `writ serve` owns so no UID in `provider_uids` can read it:

- `data` (`/var/lib/writ`): mode `0700`, owned by the `writ` user.
- State key files (`keys/*.key`), TLS private key, OIDC client secret, GitLab read token, and `provider_env` secret files: mode `0600`, owned by `writ`.
- Postgres: use Unix-socket peer authentication (as the NixOS module does) or a `0600` passfile so provider UIDs have no database access.

### 2. Block the instance metadata service for `provider_uids`

On cloud VMs (such as AWS EC2), `writ serve`'s host identity can assume roles for every environment. Add an `nftables` rule so processes running under `provider_uids` cannot reach the IPv4 or IPv6 metadata endpoints:

```nft
table inet writ {
  chain out {
    type filter hook output priority 0;
    meta skuid 61000-61063 ip daddr 169.254.169.254 drop
    meta skuid 61000-61063 ip6 daddr fd00:ec2::254 drop
  }
}
```

The [NixOS module](run.md#on-nixos) installs this rule automatically when `services.writ-serve.providerUids` is set. Writ also sets `AWS_EC2_METADATA_DISABLED=true` in every run's environment as defense in depth.

### 3. Verify isolation with `writ serve --check`

Run `--check` as the `writ` service user (with `CAP_SETUID`, `CAP_SETGID`, and `CAP_KILL`) before starting the server:

```sh
writ serve --config /etc/writ/serve --check
```

`writ serve --check` spawns a child process as the first UID in `provider_uids` with an empty environment and verifies that it cannot read `data`, any state key, the TLS key, or any `provider_env` file, and cannot connect to `169.254.169.254:80` or `[fd00:ec2::254]:80`. If any check fails, `--check` exits non-zero.

### What you should see

```
isolation: uid 61000 cannot read data, /var/lib/writ: ok
isolation: uid 61000 cannot read the state key of env prod, /var/lib/writ/keys/prod.key: ok
isolation: uid 61000 cannot read the state key of env staging, /var/lib/writ/keys/staging.key: ok
isolation: uid 61000 cannot read provider_env NOMAD_TOKEN of env prod, /var/lib/writ/secrets/nomad_token: ok
isolation: uid 61000 cannot reach 169.254.169.254:80: not checked: writ cannot reach it either, so this host has no metadata service there to protect
isolation: uid 61000 cannot reach [fd00:ec2::254]:80: not checked: writ cannot reach it either, so this host has no metadata service there to protect
```

Output of a real run, 2026-11-16, on a host without a metadata service. On an EC2 instance with the `nftables` rule loaded, both metadata lines report `ok`.

## What each run can see on disk

On Linux, `writ serve` runs every `tofu` process (`init`, `plan`, `show`, `apply`, and plugin cache fills) inside new **user, mount, and PID namespaces** with `pivot_root` onto a read-only `tmpfs`, `no_new_privs`, an empty capability bounding set, and no supplementary groups:

| Mounted path | Access |
| --- | --- |
| Run scratch directory (stack files + `HOME`) | read-write |
| `/writ-tmp` (bind of run `HOME`, set as `TMPDIR` for short Unix socket paths) | read-write |
| `tofu.plugin_cache` | read-only during runs (read-write only when `writ` fills the cache) |
| `tofu` binary, dynamic loader, shared libraries (and `tofu.git` + `git-remote-http`/`https` if set) | read-only |
| `/etc/resolv.conf`, `/etc/hosts`, `/etc/ld.so.cache`, `/etc/ssl/certs/ca-certificates.crt`, minimal `/etc/nsswitch.conf` | read-only |
| `/dev/null`, `/dev/zero`, `/dev/random`, `/dev/urandom`, `/dev/fd` links, PID-scoped `/proc` | standard / read-only |

Nothing else from the host is visible: no `/home`, no `data`, no `/tmp`, no shell (`/bin/sh`), no `ssh`, and no `hg`. When a run finishes, `writ serve` kills every process belonging to that run's UID before returning the UID to the pool.

## Outbound traffic and egress controls

Writ restricts what code and tools a run can use to make outbound connections:

- **Instance metadata blocked**: The `nftables` rule drops `169.254.169.254` and `fd00:ec2::254` for `provider_uids`.
- **Module source allow-list**: Before `tofu` runs, `writ serve` checks every `module` `source` (after recursively decoding percent-escapes). Only local paths inside the repo (`./`, `../`), registry addresses, and remote network addresses with at most `ref` and `depth` query parameters are allowed. `file://`, absolute paths, `~`, `oci://`, fragments (`#`), and `go-getter` parameters like `checksum` or `sshkey` are refused.
- **Git over HTTPS only**: When `tofu.git` is set, Writ sets `GIT_ALLOW_PROTOCOL=https:ssh`, `GIT_CONFIG_NOSYSTEM=1`, and `GIT_CONFIG_GLOBAL=/dev/null`, and omits `ssh` from the view, so only HTTPS module clones succeed.
- **No cloud credentials during `init`**: `tofu init` fetches modules and providers without cloud credentials. Only `plan` and `apply` (and the S3 backend configuration step) receive the run's role credentials.
- **Provider binaries pinned by lock file**: `writ serve` installs providers into `tofu.plugin_cache` itself from `.terraform.lock.hcl` using a synthetic config, so stack code never runs with write access to the plugin cache.

### Host network and egress limits

`writ serve` leaves the rest of the network namespace unchanged so `tofu init` can reach `registry.opentofu.org` and module hosts, and providers can reach cloud APIs. Writ does not yet include a built-in egress proxy or host allow-list for provider traffic. If you need strict outbound allow-lists (for example, allowing only your cloud APIs and OpenTofu registry), add `meta skuid 61000-61063` egress rules in `nftables` or enforce them at your VPC firewall.

## Troubleshooting

- **`isolation not checked: operation not permitted` during `--check`**: You ran `--check` as an unprivileged user without `CAP_SETUID`/`CAP_SETGID`/`CAP_KILL`. Run it under the service's capabilities (or let systemd's `ExecStartPre` run it).
- **Namespace `/proc` mount fails under systemd**: If your systemd unit sets `ProtectKernelTunables=true` on a kernel older than Linux 7.2, mounting the PID-scoped `/proc` inside the view can fail. Remove `ProtectKernelTunables` on kernels before 7.2 (the NixOS module handles this automatically).
- **Plan fails because a `local-exec` provisioner or `external` data source cannot find `/bin/sh`**: The filesystem view intentionally omits shells. Replace shell provisioners with declarative resources or a static binary inside the stack.
- **`provider_uids` exhausted with withheld UIDs**: If a provider process refuses to exit after a run, Writ withholds its UID until `writ serve` restarts. Check logs for the stuck run and restart `writ serve` during a quiet window.
- **SSH module source (`git@host:org/repo`) fails**: The view has no `ssh` binary or keys. Switch module sources to HTTPS (`git::https://...`).

## Next steps

- [AWS roles and credentials](aws-credentials.md): pair run isolation with short-lived per-run STS roles.
- [Security model](security-model.md): complete overview of Writ's trust boundaries.
- [Run a server](run.md): systemd and NixOS deployment details.
