# Run isolation and egress

Merge requests choose the OpenTofu code and providers that run on your server. A team's `writ serve` therefore isolates every `tofu` step from its own OS user, keys, database, and host identity, and can restrict outbound connections through an egress proxy.

## Prerequisites

- A Linux host. Namespaces and `provider_uids` require Linux.
- An unused numeric UID range at least as large as `limits.plans`, for example `61000-61063`. It needs no `/etc/passwd` entries.
- `CAP_SETUID`, `CAP_SETGID`, and `CAP_KILL` on the `writ serve` service. The [NixOS module](run.md#on-nixos) sets them.
- `nftables`, or an equivalent firewall, on hosts with a cloud metadata service.
- For per-step memory, PID, and CPU limits: cgroup v2, Linux 5.14+, and `writ serve` alone in a delegated cgroup (`Delegate=yes`).
- For `egress_mode` `report` or `enforce`: a `tofu` block, a unit that allows network namespaces (systemd `RestrictNamespaces` must include `net`, as the NixOS module sets), and no `http://` loopback URLs in `env.s3_endpoints`. `writ serve --check` fails when the unit refuses network namespaces.

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
isolation: uid 61000 cannot read the state key of env staging, /var/lib/writ/keys/staging.key: ok
isolation: uid 61000 cannot read provider_env NOMAD_TOKEN of env prod, /var/lib/writ/secrets/nomad_token: ok
isolation: uid 61000 cannot reach 169.254.169.254:80: not checked: writ cannot reach it either, so this host has no metadata service there to protect
isolation: uid 61000 cannot reach [fd00:ec2::254]:80: not checked: writ cannot reach it either, so this host has no metadata service there to protect
```

Real run, 2026-11-16. On EC2 with the `nftables` rule loaded, both metadata checks report `ok`.

## What each run can see on disk

Every `tofu` step (`init`, `plan`, `show`, `apply`, plugin cache fill) runs in fresh user, mount, PID, and IPC namespaces, pivoted onto a read-only `tmpfs`, with `no_new_privs`, an empty capability bounding set, no supplementary groups, and [Writ's seccomp filter](#system-calls-a-run-cant-make).

| Mounted path | Access |
| --- | --- |
| Run scratch directory (stack files and `HOME`) | read-write |
| `/writ-tmp` (bind of the run's `HOME`, set as `TMPDIR` for short Unix socket paths) | read-write |
| `tofu.plugin_cache` | read-only during runs, read-write only during cache fills |
| `tofu` binary, dynamic loader, shared libraries, and `tofu.git` with `git-remote-http`/`https` if configured | read-only |
| `/etc/resolv.conf`, `/etc/hosts`, `/etc/ld.so.cache`, `/etc/ssl/certs/ca-certificates.crt`, a minimal `/etc/nsswitch.conf` | read-only |
| `/dev/null`, `/dev/zero`, `/dev/random`, `/dev/urandom`, `/dev/fd`, a PID-scoped `/proc` | standard, read-only |
| Saved plan for `show` and `apply` (a sealed in-memory file) | read-only |

- The view has no `/home`, `data`, `/tmp`, `/bin/sh`, `ssh`, or `hg`.
- A step's System V IPC objects end with its IPC namespace. Writ kills every process owned by the run's UID before it returns the UID to the pool.
- Runs fail closed. The namespaces, the filesystem view, and the run's network namespace have no fallback: if Writ cannot build them, the step fails with the reason and `tofu` does not run.

## System calls a run can't make

A process without capabilities can still reach kernel features no OpenTofu run needs, and could create its own user namespace to hold every capability inside it again. Before it execs `tofu`, Writ installs a seccomp filter that every process of the run inherits. The filter needs Linux 4.14+ on amd64 or arm64. Elsewhere runs proceed without it, and `--check` and the start log say so.

| Result | System calls |
| --- | --- |
| `EPERM` | `kexec_load`, `kexec_file_load`, `init_module`, `finit_module`, `delete_module`; `mount`, `umount2`, `pivot_root`, `mount_setattr`, `open_tree`, `move_mount`, `fsopen`, `fsconfig`, `fsmount`, `fspick`, `open_by_handle_at`; `unshare`, `setns`, namespace-creating `clone`; `ptrace`, `process_vm_readv`, `process_vm_writev`, `pidfd_getfd`; `keyctl`, `add_key`, `request_key`; `bpf`, `perf_event_open`, `userfaultfd`, `io_uring_setup` |
| `ENOSYS` | `clone3` (glibc falls back to `clone`), the x32 ABI |
| Process killed | Foreign-architecture system calls, such as i386 `int 0x80` |

## Limit what each run uses

Each OpenTofu step runs in its own cgroup, bounded by [`limits`](config.md#settings-and-blocks):

| Limit | Default | When reached |
| --- | --- | --- |
| `run_memory` | `4GiB` | The kernel OOM-kills the step. No swap where cgroup swap accounting is on. File page cache counts. |
| `run_pids` | `1024` | New processes and threads fail. Threads count: an idle `hashicorp/random` 3.9.1 provider uses 9 threads on 16 CPUs. |
| `run_cpus` | `2` | The step is throttled, not failed. |
| `run_scratch` | `8GiB` | Writ stops the step once the measured directory plus unlinked open or mapped files exceed the limit, counting at least 4KiB per file or directory. It bounds a runaway run, not a hostile one: see [How writ serve measures scratch](#how-writ-serve-measures-scratch). |

The diagnostic names the limit:

```
tofu init stopped at a limit: the run used all of limits.run_memory, 64MiB, and the kernel killed it
tofu init stopped at a limit: the run reached limits.run_pids, 64 processes and threads, and could start no more
tofu init stopped at a limit: the run's directory held more than limits.run_scratch, 16MiB, on disk
```

Real run, 2026-11-20. If a host-level OOM kills a step first, the diagnostic says so instead of naming `run_memory`. When a step exits, Writ ends its remaining processes with `cgroup.kill` and deletes the cgroup.

### How writ serve makes the cgroups

At startup, `writ serve` moves itself into a child cgroup `serve`, enables the `memory`, `pids`, and `cpu` controllers on its parent, and creates a child cgroup `runs` capped at the parent's limits minus 128 PIDs and 512MiB kept for `writ serve`. Each step gets a `runs/run-*` cgroup before any user code runs.

Requirements:

- Unified cgroup v2 and Linux 5.14+ (`cgroup.kill`).
- `cgroup.procs` and `cgroup.subtree_control` writable by `writ` (`Delegate=yes` under systemd).
- The `memory`, `pids`, and `cpu` controllers available to the cgroup.
- `writ serve` the only process in its cgroup at startup.
- Every ancestor `pids.max` and `memory.max` above the 128 PIDs and 512MiB reserve.

If one is missing, `writ serve` logs a warning and runs without `run_memory`, `run_pids`, and `run_cpus`. `run_scratch` and seccomp still apply.

### Under systemd

```ini
[Service]
Delegate=yes
# Leave ProtectControlGroups unset so /sys/fs/cgroup stays writable.
```

### Under Nomad's exec driver

Run the task as `root` with `writ` as the task command, so it is the only process in the task cgroup. A root task gets Nomad's legacy capability set, which includes `CAP_SYS_ADMIN`. If Nomad mounts a read-only `/sys` without cgroupfs, `writ serve` mounts cgroup v2 at an empty `/sys/fs/cgroup` and creates per-step cgroups inside the task's limits. Not yet verified under a live Nomad agent.

### Check that the limits hold

Run `writ serve --check` inside the target unit (`ExecStartPre`, or `systemd-run --scope -p Delegate=yes`). It prints the active limits and sizing warnings. A failed check reports the limits as off, with the reason. Where the kernel keeps no cgroup swap account (as `cgroup.memory=noswap` makes it), `--check` and the start log warn that a step may swap past `run_memory`.

Healthy output:

```
limits: each OpenTofu step runs in a cgroup of its own under /sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/app.slice/run-p595929-i597350.scope/runs: memory 2GiB with no swap, 512 processes and threads, 2 CPUs
limits: writ measures each run's directory, and stops a step past 4GiB
seccomp: each OpenTofu process runs under writ's seccomp filter
```

Warnings it adds, each under `warning[usage]`:

| Cause | Warning starts with | Fix |
| --- | --- | --- |
| Unit sets no `MemoryMax` | `limits.run_memory, 2GiB, times 12 plans and applies at once is 24GiB, and writ's cgroup sets no memory.max` | Set the unit's `MemoryMax`. |
| `TasksMax` too low | `limits.run_pids, 512, times 12 plans and applies at once is 6144, more than the 1920 processes and threads that writ's cgroup leaves every step together` | Raise the unit's `TasksMax`, or lower `limits.run_pids`, `plans`, or `applies`. |
| Other processes share the cgroup | `limits not enforced: writ's cgroup ... holds processes other than writ serve` (the `limits:` line is then missing, and steps run without `run_memory`, `run_pids`, and `run_cpus`) | Run `writ serve` alone in its cgroup, as a systemd unit with `Delegate=yes` or a Nomad task does. |

Outputs of `writ serve --check`, 2026-11-20, with `run_memory = "2GiB"`, `run_pids = 512`, `run_cpus = 2`, `run_scratch = "4GiB"`.

### What run limits don't cover

- **Scratch is polled, not enforced by the kernel.** See [How writ serve measures scratch](#how-writ-serve-measures-scratch).
- **No disk I/O bandwidth cap.** `io` cgroup limits need stable device numbers, which device-mapper and btrfs do not reliably give.
- **Limits are per step and add up.** `init` and `plan` each get `run_memory`, and concurrent runs sum. Set systemd `MemoryMax` and `TasksMax` for the worst case of `limits.plans + limits.applies`.
- **Steps share the `runs` limits.** A step can be refused a process, or killed, before it reaches its own limit. Its first diagnostic then names the shared limit and the unit setting that raises it, `TasksMax` or `MemoryMax`. Before Linux 6.15, and from 6.15 on a cgroup filesystem mounted with `pids_localevents`, the kernel counts a refusal by the shared limit as the step's own, so a step names its own `run_pids` only when it reached that many processes and threads.
- **No limits in `--dev`.**

### How writ serve measures scratch

- Writ walks the run directory every 500ms (or every 4x the last walk's duration, if longer) and once after exit. A step can pass `run_scratch` by what it writes between two walks.
- A walk counts at least 4KiB per file or directory, prorates hard links, and adds unlinked files that threads in the step's cgroup hold open or mapped, read through `/proc`. It excludes `memfd`, System V shared memory, and anonymous mappings, which `run_memory` bounds.
- A walk stops the step past `run_scratch`, 10 seconds, 256 directory levels, 16MiB of `/proc/PID/maps`, or 65,536 unlinked files.
- Without a cgroup (as when `writ serve` shares its cgroup), Writ cannot list the step's processes, and a removed file escapes `run_scratch` until the step ends.
- It bounds a runaway or careless run, not a hostile one. A run can hide usage with a removed file held only in a descriptor in flight on a Unix socket, a one-page mapping of a large removed file, a hard link to a file of its user's outside its directory, or `inotify` timing.

The hard bound is a filesystem quota. Put `tofu.scratch_dir` on its own filesystem so a full scratch cannot fill `data` or Postgres, and set ext4 or XFS project quotas on each run's directory, or per-user quotas for `provider_uids`. `writ serve` sets no quota itself, and a non-root `writ serve` cannot mount a size-limited filesystem.

## Outbound traffic

### Always on

- **Metadata firewall:** the `nftables` rule from step 2.
- **Module source allow-list:** in-repo relative paths (`./`, `../`), registry addresses, and HTTPS or remote addresses with at most `ref` and `depth` parameters. Refused: `file://`, absolute paths, `~`, `oci://`, `#` fragments, and `go-getter` parameters such as `checksum` and `sshkey`.
- **HTTPS-only Git:** `tofu.git` runs with `GIT_ALLOW_PROTOCOL=https:ssh`, no system or global Git config, and no `ssh` binary in the view. HTTPS submodules work without `/bin/sh`.
- **No cloud credentials in `init`:** only `plan`, `apply`, and S3 backend init receive role credentials.
- **Trusted plugin cache fills:** `writ serve` installs lock-file providers from a synthetic config, so stack code never gets write access to `tofu.plugin_cache`. The lock file still chooses each provider's registry, and the registry chooses the download hosts.

Under `egress_mode = "off"` (the default), that is all:

- A run shares the host's network. Code the commit chose can reach anything the host reaches, holding the run's role credentials and the env's `provider_env` values: the internet, the network `writ serve` runs in, and services on the host's loopback, such as `writ serve`'s own Postgres.
- The provider install also uses the host's network, as `writ`'s own UID, which the metadata firewall does not cover. A lock file's registry can send the install to any host `writ serve` reaches, the metadata service included, and the error text ends up in plan diagnostics.

### Egress

Set `egress_mode = "report"` or `"enforce"`, and an `egress` list per env:

```hcl
egress_mode = "enforce"

env "prod" {
  key    = "/var/lib/writ/keys/prod.key"
  egress = ["*.amazonaws.com", "registry.opentofu.org", "nomad.example.internal:80"]
}
```

Each step then runs in a loopback-only network namespace with no external route or DNS. It reaches the network only through Writ's loopback endpoints, bound before `tofu` starts, with `NO_PROXY=127.0.0.1,localhost,::1`:

| Run loopback port | Service | Environment variable |
| --- | --- | --- |
| `127.0.0.1:3128` | Egress proxy (`CONNECT` and HTTP) | `HTTP_PROXY`, `HTTPS_PROXY`, `http_proxy`, `https_proxy` |
| `127.0.0.1:3129` | Loopback STS credential endpoint | `AWS_CONTAINER_CREDENTIALS_FULL_URI` |
| `127.0.0.1:3130` | HTTP state backend (`/v1/tfstate/` only, per-run rate bucket) | `TF_HTTP_ADDRESS` |

- Each port caps at 256 connections with a 10s header timeout.
- The egress proxy caps at 128 connections per run (10m idle, 1h max), closes every socket when the step exits, and records only `host:port`, never paths, headers, or bodies.
- An entry is `HOST`, `*.DOMAIN` (subdomains only), or an IP address, with an optional `:PORT` (default `443`).
- The proxy resolves listed names on the host and dials only the checked IP.
- NAT64 addresses count as their embedded IPv4 address: `64:ff9b::a9fe:a9fe` is blocked as metadata, and `64:ff9b:1::/48` is private.
- Private IPs (RFC 1918, ULA, CGNAT) are allowed only by an exact host or IP entry, never a wildcard.
- Provider installs run in their own namespace behind a public-only proxy limited to `provider_registries` (default `registry.opentofu.org`, `registry.terraform.io`). Install destinations appear first in `network` with `step: "install"`.

| `egress_mode` | Listed destination | Unlisted destination | Loopback, link-local, metadata, multicast, unspecified, or a NAT64 address of one |
| --- | --- | --- | --- |
| `off` (default) | Direct host network | Direct host network | Reachable, except metadata blocked by `nftables` |
| `report` | Allowed and recorded | Allowed and recorded as `unlisted` | Refused |
| `enforce` | Allowed and recorded | Refused before DNS lookup | Refused |

Under `report` and `enforce`, plans and runs record every requested `host:port`, shown on UI Network cards, in `network` in API JSON, and in `run egress` logs. A refusal looks like this:

```
error: writ's egress proxy refused 192.0.2.11:8080 (an IP address the list does not have); env prod's egress is [192.0.2.10:8080], under egress_mode enforce: add what the stack needs to the env's egress list
```

`writ serve --check` prints the active rules:

```
egress: enforce; each OpenTofu run reaches the network only through writ's egress proxy, which refuses a destination its env's egress does not list
egress: env prod allows *.amazonaws.com, registry.opentofu.org, nomad.example.internal:80
egress: providers install only from registry.opentofu.org, registry.terraform.io
```

#### What to list

Provider downloads use the separate install proxy, so `env.egress` needs only the hosts the run itself contacts:

- Cloud and provider APIs: `*.amazonaws.com`, internal APIs by exact name and port, and exact names for AWS VPC interface endpoints that resolve to private IPs.
- S3 state endpoints for `backend "s3"` stacks.
- Remote module registries (`registry.opentofu.org`), module download hosts, and Git hosts.

#### Roll it out

1. Set `egress_mode = "report"` and restart `writ serve`.
2. Run normal plans, applies, and drift checks for a few days.
3. Collect `unlisted` entries from UI Network cards, API `network`, or `run egress` logs.
4. Add narrow host and port entries to each `env.egress`.
5. Set `egress_mode = "enforce"` and restart.

#### What egress doesn't cover

- **`off` covers nothing.** See [Always on](#always-on) for what runs and the provider install can reach under the default.
- **An allowed host can carry data out.** `*.amazonaws.com` permits any S3 bucket, an attacker's included, and any AWS API that takes the run's credentials. The proxy decides by host and port, not by URL, account, or content, and passes a `CONNECT`'s TLS through untouched.
- **`report` is not a security boundary.** Unlisted destinations, private addresses included, and DNS lookups still succeed. Only `enforce` refuses a name the list lacks, before any lookup.
- **Proxy-unaware clients fail.** A process that ignores `HTTPS_PROXY` has no route or DNS. A client that sends the proxy only IP addresses needs each address listed.
- **DNS runs on the host.** Listed names resolve through `writ serve`'s resolver, so a run can make it look up any name the list allows.
- **No upstream proxy.** The proxy connects directly and does not use a proxy of `writ serve`'s own.
- **Not every path uses it.** `writ serve --dev` and `writ import bulk` do not use the egress proxy.
- **Private provider registries are blocked** by the install proxy's private-IP refusal.

For stricter control, use VPC firewalls or `nftables` `meta skuid` rules.

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

- [AWS roles and credentials](aws-credentials.md): pair isolation with short-lived STS roles.
- [Security model](security-model.md): all trust boundaries.
- [Run a control plane](run.md): systemd and NixOS deployment.
