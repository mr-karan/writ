# Run isolation and egress

Because merge requests choose the OpenTofu code and providers that execute on your server, a team's `writ serve` isolates every `tofu` step from its own OS user, keys, database, and host identity, and optionally restricts all outbound network connections through an egress proxy.

For deeper internals, see [Control plane internals: How providers are kept from the keys](serve.md#how-providers-are-kept-from-the-keys).

## Prerequisites

- Linux host (user, mount, PID, IPC, and network namespaces plus `provider_uids` require Linux).
- An unused numeric UID range at least as large as `limits.plans` (for example `61000-61063`; no `/etc/passwd` entries needed).
- `CAP_SETUID`, `CAP_SETGID`, and `CAP_KILL` on the `writ serve` service (configured automatically by the [NixOS module](run.md#on-nixos)).
- `nftables` (or equivalent firewall) on hosts with a cloud metadata service.
- For per-step memory/PID/CPU limits: cgroup v2, Linux 5.14+, and `writ serve` alone in a delegated cgroup (`Delegate=yes`).
- For `egress_mode` `report` or `enforce`: a `tofu` block and no `http://` loopback URLs in `env.s3_endpoints`.

## Steps

### 1. Reserve a `provider_uids` range and separate directories

Configure `provider_uids` and keep `tofu.plugin_cache` and `tofu.scratch_dir` outside `data`:

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

- `data` (`/var/lib/writ`): `0700`, owned by `writ`.
- State keys (`keys/*.key`), TLS private key, OIDC client secrets, forge read tokens, webhook secrets, notifier secrets, and `provider_env` files: `0600` (or `0400`), owned by `writ`.
- Postgres: Unix-socket peer auth or a `0600` passfile.

With `provider_uids` set, `writ serve` marks itself non-dumpable at startup so no same-UID process can trace it or trigger core dumps containing keys.

### 2. Block the instance metadata service for `provider_uids`

Drop IPv4 and IPv6 EC2 metadata traffic from `provider_uids`:

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

Run `--check` with the service's capabilities:

```sh
writ serve --config /etc/writ/serve --check
```

`--check` spawns a probe under the first UID in `provider_uids` with an empty environment and fails if it can read `data`, state keys, TLS keys, notifier files, or `provider_env` files, or connect to `169.254.169.254:80` / `[fd00:ec2::254]:80`.

### What you should see

```
isolation: uid 61000 cannot read data, /var/lib/writ: ok
isolation: uid 61000 cannot read the state key of env prod, /var/lib/writ/keys/prod.key: ok
isolation: uid 61000 cannot read the state key of env staging, /var/lib/writ/keys/staging.key: ok
isolation: uid 61000 cannot read provider_env NOMAD_TOKEN of env prod, /var/lib/writ/secrets/nomad_token: ok
isolation: uid 61000 cannot reach 169.254.169.254:80: not checked: writ cannot reach it either, so this host has no metadata service there to protect
isolation: uid 61000 cannot reach [fd00:ec2::254]:80: not checked: writ cannot reach it either, so this host has no metadata service there to protect
```

Output of a real run, 2026-11-16 (on EC2 with the `nftables` rule loaded, both metadata checks report `ok`).

## What each run can see on disk

Every `tofu` step (`init`, `plan`, `show`, `apply`, plugin cache fill) runs in fresh **user, mount, PID, and IPC namespaces** pivoted onto a read-only `tmpfs`, with `no_new_privs`, an empty capability bounding set, no supplementary groups, and [Writ's seccomp filter](#system-calls-a-run-cant-make):

| Mounted path | Access |
| --- | --- |
| Run scratch directory (stack files + `HOME`) | read-write |
| `/writ-tmp` (bind of run `HOME`, set as `TMPDIR` for short Unix socket paths) | read-write |
| `tofu.plugin_cache` | read-only during runs (read-write only during cache fills) |
| `tofu` binary, dynamic loader, shared libraries (and `tofu.git` + `git-remote-http`/`https` if configured) | read-only |
| `/etc/resolv.conf`, `/etc/hosts`, `/etc/ld.so.cache`, `/etc/ssl/certs/ca-certificates.crt`, minimal `/etc/nsswitch.conf` | read-only |
| `/dev/null`, `/dev/zero`, `/dev/random`, `/dev/urandom`, `/dev/fd`, PID-scoped `/proc` | standard / read-only |
| Saved plan for `show` and `apply` at `/dev/fd/8` (sealed in-memory file) | read-only |

No `/home`, `data`, `/tmp`, `/bin/sh`, `ssh`, or `hg` exists in the view. Each step's System V IPC objects end with its IPC namespace, and Writ kills all processes owned by the run's UID before returning the UID to the pool.

## System calls a run can't make

A process in the view has no capabilities, but it can still reach parts of the kernel no OpenTofu run needs, and it could make a user namespace of its own and hold every capability inside it again. So before execing `tofu`, Writ installs a seccomp filter that every process of the run inherits. It needs Linux 4.14+ on amd64 or arm64; elsewhere runs proceed without it, and `--check` and the start log say so.

- **Blocked with `EPERM`:** `kexec_load`, `kexec_file_load`, `init_module`, `finit_module`, `delete_module`; `mount`, `umount2`, `pivot_root`, `mount_setattr`, `open_tree`, `move_mount`, `fsopen`, `fsconfig`, `fsmount`, `fspick`, `open_by_handle_at`; `unshare`, `setns`, namespace-creating `clone`; `ptrace`, `process_vm_readv`, `process_vm_writev`, `pidfd_getfd`; `keyctl`, `add_key`, `request_key`; `bpf`, `perf_event_open`, `userfaultfd`, `io_uring_setup`.
- **Blocked with `ENOSYS`:** `clone3` (glibc falls back to `clone`) and the x32 ABI.
- **Process killed:** Foreign-architecture syscalls (such as i386 `int 0x80`).

## Limit what each run uses

Each OpenTofu step runs in its own cgroup bounded by [`limits`](config.md#settings-and-blocks):

| Limit | Default | Effect when reached |
| --- | --- | --- |
| `run_memory` | `4GiB` | Kernel OOM-kills the step (no swap where cgroup swap accounting is enabled; file page cache counts). |
| `run_pids` | `1024` | New processes/threads fail (threads count; an idle `hashicorp/random` 3.9.1 provider uses 9 threads on 16 CPUs). |
| `run_cpus` | `2` | CPU throttled; step doesn't fail. |
| `run_scratch` | `8GiB` | Writ stops the step once measured directory + unlinked open/mapped file usage exceeds the limit (minimum 4KiB per file/directory). It bounds a runaway run, not a hostile one; see [How writ serve measures scratch](#how-writ-serve-measures-scratch). |

Diagnostics name the tripped limit:

```
tofu init stopped at a limit: the run used all of limits.run_memory, 64MiB, and the kernel killed it
tofu init stopped at a limit: the run reached limits.run_pids, 64 processes and threads, and could start no more
tofu init stopped at a limit: the run's directory held more than limits.run_scratch, 16MiB, on disk
```

Output of `TestRunLimits`, 2026-11-20. When a host-level OOM kills a step before its own or shared cgroup limit, the diagnostic reports that rather than `run_memory`. On step exit, Writ terminates remaining processes via `cgroup.kill` and deletes the cgroup.

### How writ serve makes the cgroups

At startup, `writ serve` moves itself into child cgroup `serve`, enables `memory`, `pids`, and `cpu` controllers on its parent, and creates child cgroup `runs` capped at the parent's limits minus 128 PIDs and 512MiB reserved for `writ serve`. Each step gets a `runs/run-*` cgroup before executing user code.

Requirements:

- Unified cgroup v2 and Linux 5.14+ (`cgroup.kill`).
- `cgroup.procs` and `cgroup.subtree_control` writable by `writ` (`Delegate=yes` under systemd).
- `memory`, `pids`, and `cpu` available to the cgroup.
- `writ serve` is the sole process in its cgroup at startup.
- Any ancestor `pids.max` / `memory.max` exceeds the 128 PIDs / 512MiB server reserve.

If any requirement isn't met, `writ serve` logs a warning and runs without `run_memory`, `run_pids`, and `run_cpus` (`run_scratch` and seccomp still apply).

### Under systemd

```ini
[Service]
Delegate=yes
# Leave ProtectControlGroups unset so /sys/fs/cgroup stays writable.
```

### Under Nomad's exec driver

Run the task as `root` with `writ` as the task command (sole process in the task cgroup). A root task gets Nomad's legacy capability set, which includes `CAP_SYS_ADMIN`. If Nomad mounts a read-only `/sys` without cgroupfs, `writ serve` mounts cgroup v2 at an empty `/sys/fs/cgroup` and creates per-step cgroups inside the task's limits. (Verified in namespace tests from Nomad's exec driver source, 2026-11-20; not yet run under a live Nomad agent.)

### Check that the limits hold

Run `writ serve --check` inside the target unit (`ExecStartPre` or `systemd-run --scope -p Delegate=yes`) to inspect active cgroup limits and sizing warnings. If a check fails, it reports the limits off, with the reason. Where the kernel keeps no swap account for cgroups, as `cgroup.memory=noswap` makes it, `--check` and the start log warn that a step may swap past `run_memory`:

```
limits: each OpenTofu step runs in a cgroup of its own under /sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/app.slice/run-p595929-i597350.scope/runs: memory 2GiB with no swap, 512 processes and threads, 2 CPUs
limits: writ measures each run's directory, and stops a step past 4GiB
seccomp: each OpenTofu process runs under writ's seccomp filter
```

Without unit `MemoryMax`:

```
limits: each OpenTofu step runs in a cgroup of its own under /sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/app.slice/run-p595648-i600671.scope/runs: memory 2GiB with no swap, 512 processes and threads, 2 CPUs
limits: writ measures each run's directory, and stops a step past 4GiB
seccomp: each OpenTofu process runs under writ's seccomp filter
warning[usage]
  limits.run_memory, 2GiB, times 12 plans and applies at once is 24GiB, and writ's cgroup sets no memory.max, so only the host's memory bounds every step together; set the unit's MemoryMax
```

With `TasksMax` too low for concurrent runs:

```
limits: each OpenTofu step runs in a cgroup of its own under /sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/app.slice/run-p595971-i597357.scope/runs: memory 2GiB with no swap, 512 processes and threads, 2 CPUs
limits: writ measures each run's directory, and stops a step past 4GiB
seccomp: each OpenTofu process runs under writ's seccomp filter
warning[usage]
  limits.run_pids, 512, times 12 plans and applies at once is 6144, more than the 1920 processes and threads that writ's cgroup leaves every step together, its pids.max less what writ serve keeps; raise the unit's TasksMax, or lower limits.run_pids, plans, or applies
```

With other processes sharing the cgroup:

```
limits: writ measures each run's directory, and stops a step past 4GiB
seccomp: each OpenTofu process runs under writ's seccomp filter
warning[usage]
  limits not enforced: writ's cgroup /sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/app.slice/run-p125069-i4334344.scope holds processes other than writ serve, [125069 125106], and a cgroup whose children have limits may hold none; run writ serve alone in its cgroup, as a systemd unit with Delegate=yes or a Nomad task does; writ serve runs each OpenTofu step without limits.run_memory, run_pids, and run_cpus
```

Outputs of `writ serve --check`, 2026-11-20 (`run_memory = "2GiB"`, `run_pids = 512`, `run_cpus = 2`, `run_scratch = "4GiB"`).

### What run limits don't cover

- **Scratch is polled, not kernel-enforced:** `run_scratch` bounds a runaway or careless run, not one that tries to hide what it uses on disk. The hard bound is a filesystem quota; see [How writ serve measures scratch](#how-writ-serve-measures-scratch).
- **No disk I/O bandwidth cap:** `io` cgroup limits require stable device numbers not reliably available on device-mapper/btrfs.
- **Per-step, additive concurrency:** `init` and `plan` each get `run_memory`, and concurrent runs sum up. Set systemd `MemoryMax` and `TasksMax` for worst-case `(limits.plans + limits.applies)`.
- **Steps share the `runs` limits:** A step can be refused a process, or killed, before it reaches its own limit. Its first diagnostic then names the shared limit and the unit setting that raises it, `TasksMax` or `MemoryMax`. Before Linux 6.15, and from it on a cgroup filesystem mounted with `pids_localevents`, the kernel counts a refusal by the shared limit as the step's own, so a step names its own `run_pids` only when it reached that many processes and threads.
- **No limits in `--dev`:** Dev mode doesn't enforce run limits.

### How writ serve measures scratch

Writ polls the run directory every 500ms (or 4x last walk duration if slower) and once after exit, so a step can pass `run_scratch` by what it writes between two walks. Each walk counts at least 4KiB per file/directory (prorating hard links) plus unlinked files held open or mapped by threads in the step's cgroup via `/proc` (excluding `memfd`/SysV shm/anon mappings bounded by `run_memory`). Walks abort the step if they exceed `run_scratch`, 10 seconds, 256 directory levels, 16MiB of `/proc/PID/maps`, or 65,536 unlinked files.

Without a cgroup, as when `writ serve` shares its cgroup, it can't list the step's processes, and a removed file escapes `run_scratch` until the step ends.

The walk bounds a runaway or careless run, not a hostile one. A run can hide what it uses with a removed file held only in a descriptor in flight on a Unix socket, a mapping of one page of a large removed file, a hard link to a file of its user's outside its directory, or `inotify` timing.

The hard bound is a filesystem quota. Put `tofu.scratch_dir` on a filesystem of its own, so a full scratch can't fill `data` or Postgres, and set ext4 or XFS project quotas on each run's directory, or per-user quotas for `provider_uids`. `writ serve` sets no quota itself, and a non-root `writ serve` can't mount a size-limited filesystem.

## Outbound traffic

### Always on

- **Metadata firewall:** `nftables` drops `169.254.169.254` and `fd00:ec2::254` for `provider_uids`.
- **Module source allow-list:** Only in-repo relative paths (`./`, `../`), registry addresses, and HTTPS/remote addresses with at most `ref` and `depth` params are permitted (`file://`, absolute paths, `~`, `oci://`, `#` fragments, and `go-getter` params like `checksum`/`sshkey` are refused).
- **HTTPS-only Git:** `tofu.git` runs with `GIT_ALLOW_PROTOCOL=https:ssh`, no system/global Git config, and no `ssh` binary in the view.
- **No cloud credentials in `init`:** Only `plan`, `apply`, and S3 backend init receive role credentials.
- **Trusted plugin cache fills:** `writ serve` installs lock-file providers using a synthetic config, never giving stack code write access to `tofu.plugin_cache`. The lock file still chooses each provider's registry, and the registry chooses the download hosts.

Under `egress_mode = "off"` (default), that is all. A run shares the host's network, so code the commit chose can reach anything the host reaches, with the run's role credentials and the env's `provider_env` values in hand: the internet, the network `writ serve` runs in, and services on the host's loopback, such as `writ serve`'s own Postgres. The provider install also runs on the host's network, as `writ`'s own UID, which the metadata firewall rule doesn't cover; see [The provider install](serve.md#the-provider-install).

### Egress

Set `egress_mode = "report"` or `"enforce"` and configure per-environment `egress` allow-lists:

```hcl
egress_mode = "enforce"

env "prod" {
  key    = "/var/lib/writ/keys/prod.key"
  egress = ["*.amazonaws.com", "registry.opentofu.org", "nomad.example.internal:80"]
}
```

Each step runs in a loopback-only network namespace with no external route or DNS, reaching the network solely via Writ's proxy at `127.0.0.1:3128` (`HTTPS_PROXY` / `HTTP_PROXY`), alongside the loopback credential and state endpoints ([Control plane internals: Egress](serve.md#egress)). Provider installs run in their own namespace behind a public-only proxy limited to `provider_registries` (default `registry.opentofu.org`, `registry.terraform.io`).

Entries accept `HOST`, `*.DOMAIN` (subdomains only), or IP addresses with optional `:PORT` (default `443`). Private IPs (RFC 1918, ULA, CGNAT) are permitted only when matched by an exact hostname or IP, never a wildcard.

| `egress_mode` | Listed destination | Unlisted destination | Loopback, link-local, metadata, multicast, unspecified, and a NAT64 address of one |
| --- | --- | --- | --- |
| `off` (default) | Direct host network | Direct host network | Reachable except metadata blocked by `nftables` |
| `report` | Allowed and recorded | Allowed and recorded as `unlisted` | Refused |
| `enforce` | Allowed and recorded | Refused before DNS lookup | Refused |

Under `report` and `enforce`, plans and runs record every requested `host:port` (visible on UI Network cards, `network` in API JSON, and `run egress` logs):

```
error: writ's egress proxy refused 192.0.2.11:8080 (an IP address the list does not have); env prod's egress is [192.0.2.10:8080], under egress_mode enforce: add what the stack needs to the env's egress list
```

`writ serve --check` prints active egress rules:

```
egress: enforce; each OpenTofu run reaches the network only through writ's egress proxy, which refuses a destination its env's egress does not list
egress: env prod allows *.amazonaws.com, registry.opentofu.org, nomad.example.internal:80
egress: providers install only from registry.opentofu.org, registry.terraform.io
```

#### What to list

Provider downloads use the separate install proxy, so `env.egress` only needs hosts contacted by the run itself:

- Cloud and provider APIs (`*.amazonaws.com`, internal APIs with exact names and ports; list exact names for AWS VPC interface endpoints resolving to private IPs).
- S3 state endpoints for `backend "s3"` stacks.
- Remote module registries (`registry.opentofu.org`), module download hosts, and Git hosts.

#### Roll it out

1. Set `egress_mode = "report"` and restart `writ serve`.
2. Run normal plans, applies, and drift checks for a few days.
3. Collect `unlisted` entries from UI Network cards, API `network`, or `run egress` logs.
4. Add narrow host/port entries to each `env.egress`.
5. Switch to `egress_mode = "enforce"` and restart.

#### What egress doesn't cover

- **`off` covers nothing:** Under the default, runs and the provider install use the host's network. The install runs as `writ`'s own UID, so it can reach the metadata service, and the error text of a request it sends ends up in the plan's diagnostics.
- **An allowed host can carry data out:** `*.amazonaws.com` permits any S3 bucket, an attacker's included, and any AWS API that takes the run's credentials. The proxy decides by host and port, not by the URL, the account, or what's sent, and passes a `CONNECT`'s TLS through untouched.
- **`report` isn't a security boundary:** Unlisted destinations, private addresses included, and DNS lookups still succeed. Only `enforce` refuses a name the list lacks before any lookup.
- **Proxy-unaware clients fail:** Processes ignoring `HTTPS_PROXY` have no route or DNS. A client that sends the proxy only IP addresses needs each address listed.
- **Host DNS resolution:** Listed names resolve via `writ serve`'s resolver, so a run can make it look up any name the list allows.
- **No upstream proxy:** The proxy connects directly. It doesn't use a proxy of `writ serve`'s own.
- **Out-of-scope paths:** `writ serve --dev` and `writ import bulk` don't use the egress proxy.
- **Private provider registries:** Blocked by the install proxy's private-IP refusal.

Use VPC firewalls or `nftables` `meta skuid` rules for stricter controls.

## Troubleshooting

- **`isolation not checked: operation not permitted`**: Run `--check` with `CAP_SETUID`/`CAP_SETGID`/`CAP_KILL` (or via systemd `ExecStartPre`).
- **`/proc` mount fails under systemd**: Unset `ProtectKernelTunables` on kernels older than Linux 7.2.
- **`local-exec` or `external` can't find `/bin/sh`**: Shells are intentionally absent from the view; use declarative resources or static binaries.
- **Withheld `provider_uids`**: Stuck provider processes withhold their UID until restart; restart `writ serve` during a quiet window.
- **`writ's egress proxy refused HOST:PORT`**: Add the host/port to `env.egress` or switch briefly to `report` ([Roll it out](#roll-it-out)).
- **`which provider_registries does not list`**: Add trusted provider registries to `provider_registries`.
- **`network is unreachable` under `report`/`enforce`**: Client ignored `HTTPS_PROXY`.
- **SSH module source fails**: Switch to HTTPS (`git::https://...`).
- **`limits not enforced: ... holds processes other than writ serve` or `not delegated`**: Run `writ serve` alone in a systemd unit with `Delegate=yes` and without `ProtectControlGroups`.
- **`stopped at a limit: ... limits.run_memory`**: Raise `limits.run_memory` (or `run_pids` / `run_scratch`), or split the stack.

## Next steps

- [AWS roles and credentials](aws-credentials.md): pair isolation with short-lived STS roles.
- [Control plane internals: Egress](serve.md#egress): proxy ports and protocol details.
- [Security model](security-model.md): overview of all trust boundaries.
- [Run a control plane](run.md): systemd and NixOS deployment.
