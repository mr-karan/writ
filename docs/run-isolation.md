# Run isolation and egress

Isolate every `tofu` step on a team's `writ serve` from its OS user, keys, database, and host identity, and optionally limit where runs can connect. How each layer works is in [How run isolation works](how-run-isolation-works.md).

## Prerequisites

- A Linux host. Namespaces and `provider_uids` require Linux.
- An unused numeric UID range at least as large as `limits.plans`, for example `61000-61063`. It needs no `/etc/passwd` entries.
- `CAP_SETUID`, `CAP_SETGID`, and `CAP_KILL` on the `writ serve` service. The [NixOS module](run.md#on-nixos) sets them.
- `nftables`, or an equivalent firewall, on hosts with a cloud metadata service.
- For Writ's seccomp filter: Linux 4.14+ on amd64 or arm64. Elsewhere runs proceed without it, and `writ serve --check` and the start log say so.
- For per-step memory, PID, and CPU limits: cgroup v2, Linux 5.14+, and `writ serve` alone in a delegated cgroup (`Delegate=yes`).
- For `egress_mode` `report` or `enforce`: a `tofu` block, a unit that allows network namespaces (systemd `RestrictNamespaces` must include `net`, as the NixOS module sets), and no `http://` loopback URLs in `env.s3_endpoints`. `writ serve --check` fails when the unit refuses network namespaces.

> **Warning:** runs fail closed. If Writ cannot build a step's namespaces, filesystem view, or network namespace, the step fails with the reason and `tofu` does not run ([details](how-run-isolation-works.md#what-each-run-can-see-on-disk)).

## Steps

### 1. Reserve a `provider_uids` range and separate directories

Set `provider_uids`, and keep `tofu.plugin_cache` and `tofu.scratch_dir` outside `data`:

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

Lock down host files so no provider UID can read them:

| Path | Mode |
| --- | --- |
| `data` (`/var/lib/writ`) | `0700`, owned by `writ` |
| State keys (`keys/*.key`), TLS private key, OIDC client secrets, forge read tokens, webhook secrets, notifier secrets, `provider_env` files | `0600` or `0400`, owned by `writ` |
| Postgres | Unix-socket peer auth or a `0600` passfile |

With `provider_uids` set, `writ serve` marks itself non-dumpable at startup. No process with the same UID can trace it or read its memory, and a crash writes no core dump, only the Go traceback on stderr.

### 2. Block the instance metadata service for `provider_uids`

```nft
table inet writ {
  chain out {
    type filter hook output priority 0;
    meta skuid 61000-61063 ip daddr 169.254.169.254 drop
    meta skuid 61000-61063 ip6 daddr fd00:ec2::254 drop
  }
}
```

The [NixOS module](run.md#on-nixos) installs this rule when `providerUids` is set. Writ also sets `AWS_EC2_METADATA_DISABLED=true` in every run.

### 3. Verify isolation with `writ serve --check`

Run it with the service's capabilities:

```sh
writ serve --config /etc/writ/serve --check
```

`--check` starts a probe under the first UID in `provider_uids` with an empty environment. It fails if the probe can read `data`, state keys, TLS keys, notifier files, or `provider_env` files, or connect to `169.254.169.254:80` or `[fd00:ec2::254]:80`.

### What you should see

```
isolation: uid 61000 cannot read data, /var/lib/writ: ok
isolation: uid 61000 cannot read the state key of env prod, /var/lib/writ/keys/prod.key: ok
isolation: uid 61000 cannot reach 169.254.169.254:80: not checked: writ cannot reach it either, so this host has no metadata service there to protect
```

Excerpt of a real run, 2026-11-16. On EC2 with the `nftables` rule loaded, both metadata checks report `ok`.

## Limit what each run uses

Each OpenTofu step runs in its own cgroup, bounded by [`limits`](config.md#settings-and-blocks): `run_memory` (default `4GiB`), `run_pids` (`1024`), `run_cpus` (`2`), and `run_scratch` (`8GiB`). What each does when reached is in [What each limit does](how-run-isolation-works.md#what-each-limit-does).

1. Under systemd, delegate the cgroup:

   ```ini
   [Service]
   Delegate=yes
   # Leave ProtectControlGroups unset so /sys/fs/cgroup stays writable.
   ```

   Under Nomad's exec driver, see [Under Nomad's exec driver](how-run-isolation-works.md#under-nomads-exec-driver).

2. Set the unit's `MemoryMax` and `TasksMax` for the worst case of `limits.plans + limits.applies`, because limits are per step and add up.

### Check that the limits hold

Run `writ serve --check` inside the target unit (`ExecStartPre`, or `systemd-run --scope -p Delegate=yes`). It prints the active limits and sizing warnings. A failed check reports the limits as off, with the reason. Where the kernel keeps no cgroup swap account (as `cgroup.memory=noswap` makes it), `--check` and the start log warn that a step may swap past `run_memory`.

```
limits: each OpenTofu step runs in a cgroup of its own under /sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/app.slice/run-p595929-i597350.scope/runs: memory 2GiB with no swap, 512 processes and threads, 2 CPUs
limits: writ measures each run's directory, and stops a step past 4GiB
seccomp: each OpenTofu process runs under writ's seccomp filter
```

Each warning and its fix is in [Warnings from writ serve --check](how-run-isolation-works.md#warnings-from-writ-serve---check).

> **Warning:** `run_scratch` is polled, not enforced by the kernel, so it bounds a runaway run, not a hostile one. The hard bound is a filesystem quota on `tofu.scratch_dir` ([How writ serve measures scratch](how-run-isolation-works.md#how-writ-serve-measures-scratch)).

## Egress

> **Warning:** under the default, `egress_mode = "off"`, runs and the provider install share the host's network. Code a commit chose can reach anything the host reaches ([Always on](how-run-isolation-works.md#always-on)).

1. Set `egress_mode` and an `egress` list per env:

   ```hcl
   egress_mode = "enforce"

   env "prod" {
     key    = "/var/lib/writ/keys/prod.key"
     egress = ["*.amazonaws.com", "registry.opentofu.org", "nomad.example.internal:80"]
   }
   ```

2. List only what the run itself contacts; provider downloads use a separate install proxy limited to `provider_registries`:
   - cloud and provider APIs: `*.amazonaws.com`, internal APIs by exact name and port, and exact names for AWS VPC interface endpoints that resolve to private IPs;
   - S3 state endpoints for `backend "s3"` stacks;
   - remote module registries (`registry.opentofu.org`), module download hosts, and Git hosts.
3. Check the rules with `writ serve --check`:

   ```
   egress: enforce; each OpenTofu run reaches the network only through writ's egress proxy, which refuses a destination its env's egress does not list
   egress: env prod allows *.amazonaws.com, registry.opentofu.org, nomad.example.internal:80
   egress: providers install only from registry.opentofu.org, registry.terraform.io
   ```

A refused destination fails the step:

```
error: writ's egress proxy refused 192.0.2.11:8080 (an IP address the list does not have); env prod's egress is [192.0.2.10:8080], under egress_mode enforce: add what the stack needs to the env's egress list
```

> **Warning:** `report` is not a security boundary, and an allowed host such as `*.amazonaws.com` can still carry data out. See [What egress doesn't cover](how-run-isolation-works.md#what-egress-doesnt-cover).

> **Warning:** the egress proxy blocks private provider registries (it refuses private IPs) and connects directly, with no upstream proxy. `writ serve --dev` and `writ import bulk` don't use it.

### Roll it out

1. Set `egress_mode = "report"` and restart `writ serve`.
2. Run normal plans, applies, and drift checks for a few days.
3. Collect `unlisted` entries from UI Network cards, API `network`, or `run egress` logs.
4. Add narrow host and port entries to each `env.egress`.
5. Set `egress_mode = "enforce"` and restart.

## Troubleshooting

| Symptom | Fix |
| --- | --- |
| `isolation not checked: operation not permitted` | Run `--check` with `CAP_SETUID`, `CAP_SETGID`, `CAP_KILL`, or from systemd `ExecStartPre`. |
| `/proc` mount fails under systemd | Unset `ProtectKernelTunables` on kernels older than Linux 7.2. |
| `local-exec` or `external` can't find `/bin/sh` | Shells are absent from the view on purpose. Use declarative resources or static binaries. |
| A `provider_uids` UID is withheld | A stuck provider process keeps its UID until restart. Restart `writ serve` in a quiet window. |
| `writ's egress proxy refused HOST:PORT` | Add the host and port to `env.egress`, or switch to `report` for a while ([Roll it out](#roll-it-out)). |
| `which provider_registries does not list` | Add the trusted registry to `provider_registries`. |
| `network is unreachable` under `report` or `enforce` | The client ignored `HTTPS_PROXY`. |
| SSH module source fails | Use HTTPS (`git::https://...`). |
| `limits not enforced: ... holds processes other than writ serve`, or `not delegated` | Run `writ serve` alone in a systemd unit with `Delegate=yes` and without `ProtectControlGroups`. |
| `stopped at a limit: ... limits.run_memory` | Raise `limits.run_memory` (or `run_pids`, `run_scratch`), or split the stack. |

## Next steps

- [How run isolation works](how-run-isolation-works.md): the filesystem view, seccomp, cgroups, scratch, and the egress proxy.
- [AWS roles and credentials](aws-credentials.md): pair isolation with short-lived STS roles.
- [Run a control plane](run.md): systemd and NixOS deployment.
