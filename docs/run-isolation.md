# Run isolation and egress

Isolate OpenTofu and provider processes from `writ serve`'s keys, database, and host identity, and limit the hosts a run can reach with the opt-in egress proxy.

Because a merge request chooses the OpenTofu code and providers that run on your server, a team's `writ serve` never runs OpenTofu as its own OS user. For the underlying architecture, see [How the control plane works: How providers are kept from the keys](serve.md#how-providers-are-kept-from-the-keys).

## Prerequisites

- A Linux host. (User, mount, and PID namespaces and `provider_uids` require Linux.)
- A range of unused numeric UIDs on the host, with at least as many UIDs as `limits.plans` (for example `61000-61063`). They do not need `/etc/passwd` entries.
- `CAP_SETUID`, `CAP_SETGID`, and `CAP_KILL` granted to the `writ serve` service (the [NixOS module](run.md#on-nixos) configures these automatically).
- `nftables` (or an equivalent firewall) on hosts with an instance metadata service.
- For each run's memory, process, and CPU limits: cgroup v2, Linux 5.14 or later, and `writ serve` alone in a cgroup delegated to its user, as systemd's `Delegate=yes` gives it (the [NixOS module](run.md#on-nixos) sets it). See [Limit what each run uses](#limit-what-each-run-uses).
- For `egress_mode` `report` or `enforce`: the `tofu` block, and no env whose `s3_endpoints` lists an `http://` endpoint on loopback, which a run's own namespace can't reach.

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

With `provider_uids` set, `writ serve` marks itself not dumpable at start, so no process of the `writ` user can trace it or read its memory, and it writes no core dump that would hold its keys and secrets. See [How providers are kept from the keys](serve.md#how-providers-are-kept-from-the-keys).

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

On Linux, `writ serve` runs every `tofu` process (`init`, `plan`, `show`, `apply`, and plugin cache fills) inside new **user, mount, PID, and IPC namespaces** with `pivot_root` onto a read-only `tmpfs`, `no_new_privs`, an empty capability bounding set, no supplementary groups, and [writ's seccomp filter](#system-calls-a-run-cant-make):

| Mounted path | Access |
| --- | --- |
| Run scratch directory (stack files + `HOME`) | read-write |
| `/writ-tmp` (bind of run `HOME`, set as `TMPDIR` for short Unix socket paths) | read-write |
| `tofu.plugin_cache` | read-only during runs (read-write only when `writ` fills the cache) |
| `tofu` binary, dynamic loader, shared libraries (and `tofu.git` + `git-remote-http`/`https` if set) | read-only |
| `/etc/resolv.conf`, `/etc/hosts`, `/etc/ld.so.cache`, `/etc/ssl/certs/ca-certificates.crt`, minimal `/etc/nsswitch.conf` | read-only |
| `/dev/null`, `/dev/zero`, `/dev/random`, `/dev/urandom`, `/dev/fd` links, PID-scoped `/proc` | standard / read-only |
| Saved plan, for `show` and `apply`, at `/dev/fd/7` (a sealed in-memory file) | read-only |

Nothing else from the host is visible: no `/home`, no `data`, no `/tmp`, no shell (`/bin/sh`), no `ssh`, and no `hg`. Each step's System V shared memory, semaphores, and message queues live in its own IPC namespace and end with the step, so a later run can't attach to a segment an earlier run made. When a run finishes, `writ serve` kills every process belonging to that run's UID before returning the UID to the pool.

## System calls a run can't make

A process in the view has no capabilities, but it can still reach parts of the kernel that no OpenTofu run needs, and it could make a user namespace of its own and hold every capability inside it again. So the view's helper installs a seccomp filter after it makes the view and before it starts `tofu`, and every process of the run inherits it: OpenTofu, each provider, a module getter, and `git`.

These system calls fail with `EPERM`:

- `kexec_load`, `kexec_file_load`, `init_module`, `finit_module`, and `delete_module`
- `mount`, `umount2`, `pivot_root`, `mount_setattr`, `open_tree`, `move_mount`, `fsopen`, `fsconfig`, `fsmount`, `fspick`, and `open_by_handle_at`
- `unshare`, `setns`, and `clone` with a flag that makes a namespace
- `ptrace`, and `process_vm_readv`, `process_vm_writev`, and `pidfd_getfd`, which read another process's memory, write it, or copy its descriptors
- `keyctl`, `add_key`, and `request_key`
- `bpf`, `perf_event_open`, `userfaultfd`, and `io_uring_setup`

`clone3` and the x32 ABI fail with `ENOSYS`, because the filter can't read `clone3`'s flags, and glibc then falls back to `clone`. A system call of another architecture, such as i386 through `int 0x80`, kills the process. Go programs, OpenTofu and its providers among them, start processes and threads with `clone`, so the filter doesn't get in their way.

The filter needs Linux 4.14 or later on amd64 or arm64. Elsewhere, `writ serve` runs `tofu` without it, and `writ serve --check` and the start log say so. `TestSeccompFilter` proves each denial: the same calls outside a run return what the kernel makes of them.

## Limit what each run uses

A provider that forks or allocates without bound could take the memory, processes, or CPU that `writ serve` and every other run need. So each OpenTofu step (`init`, `plan`, `show`, `apply`, or a plugin cache fill) runs in a cgroup of its own, with the limits from the config's [`limits` block](config.md#settings-and-blocks):

| Limit | Default | When a step reaches it |
| --- | --- | --- |
| `run_memory` | `4GiB` | The kernel kills every process of the step. The step gets no swap where the kernel keeps a swap account for cgroups, and the page cache of the files it writes counts. |
| `run_pids` | `1024` | The step can't start another process or thread. Threads count too: an idle `hashicorp/random` 3.9.1 provider runs 9 on a 16-CPU host. |
| `run_cpus` | `2` | The step runs slower. Nothing fails. |
| `run_scratch` | `8GiB` | `writ serve` stops the step once the run's directory has more than this on disk. Each file and directory counts as at least 4KiB, the inode it costs the filesystem, so many empty files count too. It's a measure that bounds a runaway run, not a hard bound; see [How writ serve measures scratch](#how-writ-serve-measures-scratch). |

When a step reaches a limit, the plan or run fails, and its first diagnostic names the limit:

```
tofu init stopped at a limit: the run used all of limits.run_memory, 64MiB, and the kernel killed it
tofu init stopped at a limit: the run reached limits.run_pids, 64 processes and threads, and could start no more
tofu init stopped at a limit: the run's directory held more than limits.run_scratch, 16MiB, on disk
```

Output of `TestRunLimits`, 2026-11-20, with a fake `tofu` that allocates, forks, and writes past limits of 64MiB, 64, and 16MiB.

When a step ends, `writ serve` kills every process left in its cgroup with `cgroup.kill`, wherever it went in the run's PID namespace and whatever its UID, and removes the cgroup.

### How writ serve makes the cgroups

At start, `writ serve` checks the cgroup it runs in. It moves itself into a child of that cgroup, named `serve`, and enables the `memory`, `pids`, and `cpu` controllers for the cgroup's children. It makes a second child, `runs`, which holds every step's cgroup, and sets its `pids.max` and `memory.max` to the least that its cgroup and each cgroup above it allow, less 128 processes and threads and 512MiB that `writ serve` keeps for itself. Without that, steps that together used all of a unit's `TasksMax` left `writ serve` unable to start a thread, and Go killed it. Each step then gets a child cgroup of `runs`, `run-` and a random name, and `writ serve` moves the step's first process into it before that process runs anything of its own. Go would start the process in the cgroup with `clone3`, but a seccomp filter such as systemd's `RestrictNamespaces=`, which the NixOS module sets, answers `clone3` with `ENOSYS`, so `writ serve` starts it with `clone` and moves it. cgroup v2 lets a cgroup give its children controllers only while it has no process of its own, so `writ serve` must be the only process in its cgroup when it starts. It needs:

- cgroup v2, the unified hierarchy, and Linux 5.14 or later for `cgroup.kill`.
- Its cgroup's `cgroup.procs` and `cgroup.subtree_control` writable by its user. Root can write them; for another user, systemd's `Delegate=yes` makes them so.
- The `memory`, `pids`, and `cpu` controllers offered to its cgroup.
- No other process in its cgroup.
- Where its cgroup or one above it sets `pids.max` or `memory.max`, more than the 128 processes and threads, or the 512MiB, that `writ serve` keeps.

When one of these is missing, `writ serve` still starts. It logs a warning that says why, and runs each step without `run_memory`, `run_pids`, and `run_cpus`. `run_scratch` and the seccomp filter don't need a cgroup and still hold.

### Under systemd

Add these lines to the `[Service]` section of `writ serve`'s unit, whether it runs as root or as its own user:

```ini
Delegate=yes
# ProtectControlGroups=yes makes /sys/fs/cgroup read-only. Leave it unset.
```

`Delegate=yes` gives the unit's cgroup to its user, and keeps systemd from changing the controllers under it. The [NixOS module](run.md#on-nixos) sets both when you set `providerUids`. systemd runs a delegated unit's `ExecStartPre` in the unit cgroup's `.control` child, so `writ serve --check` run there reports on the unit's cgroup.

### Under Nomad's exec driver

A task of Nomad's exec driver runs in a cgroup of its own, under `nomad.slice`, with the `memory`, `pids`, and `cpu` controllers and the task's `memory_max`. The driver mounts a read-only `/sys` with no cgroup filesystem in it. A task that runs as `root` gets Nomad's legacy capability set, which has `CAP_SYS_ADMIN`. When `writ serve` runs as root and finds no cgroup v2 filesystem mounted, it mounts one at `/sys/fs/cgroup`, if that's an empty directory, and then makes each step's cgroup under the task's. Each step's limits then sit inside the task's own.

For that, run the task as `root`, and make `writ` the task's command, so that it's the only process in the task's cgroup. When the task stops, Nomad kills its cgroup and removes it, with the cgroups of the steps under it.

These facts come from the exec driver's source on Nomad's main branch, read 2026-11-20. `TestRunLimitsMountsCgroupFS` reproduces the setup in namespaces: `writ` as root, alone in its cgroup, with a read-only `/sys` and no cgroup filesystem. No Nomad agent has run it.

### Check that the limits hold

`writ serve --check` prints whether each step would get a cgroup, under which cgroup, and with which limits. It checks what `writ serve` checks at start: that the unit's cgroup is delegated to `writ`'s user, offers the `memory`, `pids`, and `cpu` controllers, and holds no other process. Then, in a cgroup of its own that it removes after, it checks `cgroup.kill` and starts a probe as a step's first process starts. It doesn't change the unit's own `cgroup.subtree_control`, which would keep systemd from starting `writ serve` in that cgroup, so it can't check that the unit's cgroup enables the controllers for its children: it checks that the cgroup offers them, and `writ serve` enables them at start. It reports the limits off, with the reason, if a check fails. Where the kernel keeps no swap account for cgroups, as `cgroup.memory=noswap` makes it, it leaves `with no swap` out and warns that a step may swap past `run_memory`; `writ serve` logs the same at start. It moves `writ` itself into no cgroup, so run it where `writ serve` runs: in the unit's `ExecStartPre`, or with `systemd-run --scope -p Delegate=yes`. In a delegated cgroup with `MemoryMax=32G`:

```
limits: each OpenTofu step runs in a cgroup of its own under /sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/app.slice/run-p595929-i597350.scope/runs: memory 2GiB with no swap, 512 processes and threads, 2 CPUs
limits: writ measures each run's directory, and stops a step past 4GiB
seccomp: each OpenTofu process runs under writ's seccomp filter
```

In a unit with no `MemoryMax`, as the NixOS module sets none, only the host's memory bounds every step together:

```
limits: each OpenTofu step runs in a cgroup of its own under /sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/app.slice/run-p595648-i600671.scope/runs: memory 2GiB with no swap, 512 processes and threads, 2 CPUs
limits: writ measures each run's directory, and stops a step past 4GiB
seccomp: each OpenTofu process runs under writ's seccomp filter
warning[usage]
  limits.run_memory, 2GiB, times 12 plans and applies at once is 24GiB, and writ's cgroup sets no memory.max, so only the host's memory bounds every step together; set the unit's MemoryMax
```

In a unit with `MemoryMax=32G` and `TasksMax=2048`, too few processes for 12 steps at once at `run_pids = 512`:

```
limits: each OpenTofu step runs in a cgroup of its own under /sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/app.slice/run-p595971-i597357.scope/runs: memory 2GiB with no swap, 512 processes and threads, 2 CPUs
limits: writ measures each run's directory, and stops a step past 4GiB
seccomp: each OpenTofu process runs under writ's seccomp filter
warning[usage]
  limits.run_pids, 512, times 12 plans and applies at once is 6144, more than the 1920 processes and threads that writ's cgroup leaves every step together, its pids.max less what writ serve keeps; raise the unit's TasksMax, or lower limits.run_pids, plans, or applies
```

Where other processes share the cgroup, here the shell that started `writ` and the `tail` it piped the output to:

```
limits: writ measures each run's directory, and stops a step past 4GiB
seccomp: each OpenTofu process runs under writ's seccomp filter
warning[usage]
  limits not enforced: writ's cgroup /sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/app.slice/run-p125069-i4334344.scope holds processes other than writ serve, [125069 125106], and a cgroup whose children have limits may hold none; run writ serve alone in its cgroup, as a systemd unit with Delegate=yes or a Nomad task does; writ serve runs each OpenTofu step without limits.run_memory, run_pids, and run_cpus
```

Output of `writ serve --check`, 2026-11-20, of a config with `run_memory = "2GiB"`, `run_pids = 512`, `run_cpus = 2`, `run_scratch = "4GiB"`, and the default 8 plans and 4 applies, run by `systemd-run --user --scope -p Delegate=yes`: with `-p MemoryMax=32G`, directly, with `-p MemoryMax=32G -p TasksMax=2048`, and from a `bash -c` that stayed in the scope.

### What run limits don't cover

- **Scratch is measured, not enforced by the kernel.** `run_scratch` bounds a runaway or careless run, not one that tries to hide what it uses on disk; the hard bound is a filesystem quota. See [How writ serve measures scratch](#how-writ-serve-measures-scratch).
- **Disk bandwidth isn't limited.** The `io` controller's limits need the scratch device's numbers, which a device-mapper or btrfs filesystem doesn't give reliably.
- **Each step has its own limits.** A run's `init` and `plan` each get `run_memory`, one after the other.
- **The limits of concurrent runs add up.** `limits.plans` runs at `run_memory` each can use more memory than the host has, so size the host or the limits for both. Every step together stays within `runs`, so they can't take what `writ serve` keeps, but a step can fail to start a process, or be killed, before it reaches its own limit. Its first diagnostic then names the limit the steps share and the unit's setting that raises it, `TasksMax` or `MemoryMax`. A step refused a process names the shared limit after its exit code, since the step may have failed for a reason of its own, and names it whenever it refused any step while the step ran, since the kernel doesn't say which. Before Linux 6.15, and from it on a cgroup filesystem mounted with `pids_localevents`, the kernel counts a refusal by the shared limit as the refused step's own, so a step names its own `run_pids` only when it reached that many processes and threads. `writ serve --check` warns when `run_pids` or `run_memory` times `limits.plans` plus `limits.applies` is more than `runs` allows, and when the unit sets no `MemoryMax`, so that nothing but the host bounds their memory together.
- **`writ serve --dev` sets no run limits.** Its config has no `limits` block.

### How writ serve measures scratch

`writ serve` walks the run's directory every half second while a step runs, or four times as long as the last walk took when that's longer, and once more after the step ends. Each walk stops once it has counted past `run_scratch`. A step can pass `run_scratch` by what it writes between two walks.

Each file and directory counts its blocks on disk, and at least 4KiB, the inode it costs the filesystem. A file with several links counts a share of its blocks at each link, so it counts once in all, and the walk keeps no list of the files it met. A link the run makes outside its directory, to a file its user owns elsewhere on the same filesystem, is a share no walk counts. A whole walk of 300,000 empty files, under the default `run_scratch`, took `writ serve` 0.3 seconds of CPU and 15MiB of allocations on a 16-CPU host.

A process can also write into a file it removed but holds open or mapped, which no directory names. So each walk also reads the descriptors and mappings of every thread in the step's cgroup from `/proc`, and counts each removed file once, on whatever filesystem it lies: a run can make a btrfs subvolume in its own directory, whose files have a device of their own. A `memfd` or System V shared memory doesn't count, because it's memory, which `run_memory` bounds, and it ends with the step: a `memfd` with the last process that holds it, and System V shared memory with the step's IPC namespace. A file the step mapped that something outside the run removes or replaces, such as a provider binary, counts too. A mapped file counts as the end of its furthest mapping, since a mapping gives no way to read the file's size on disk. Each walk reads the mappings of each process once, from one of its threads, which share them, and reads at most 16MiB of mappings across the step's processes: past that, it stops the step, as a full directory does, since a run can make each line of `/proc/PID/maps` some 60KB long with a deep path. It keeps a hash of each removed file's path, not the path, and stops once the files it found, each counted as at least 4KiB, pass `run_scratch`. It keeps at most 65,536 removed files, some 8MiB: past that, it stops the step, since at 4KiB each the default `run_scratch` alone would let a run hold two million empty removed files and make each walk take more memory than `writ serve` keeps for itself.

`writ serve` reads `/proc` as the run's user, because the kernel lets only a process's own UID list its descriptors, even for `writ serve`, which made the user namespace the process runs in. A process that made itself not dumpable gives its descriptors to root of that namespace, `writ serve`'s own UID, so `writ serve` reads those as itself. Without a cgroup, as when `writ serve` shares its cgroup, it can't list the step's processes, and a removed file escapes `run_scratch` until the step ends.

Each walk opens each directory so that a FIFO or a symbolic link swapped in for it fails at once rather than blocks the walk. A walk that takes more than 10 seconds, its read of `/proc` included, or meets a directory more than 256 levels deep, stops the step, as a full directory does. The read of `/proc` checks the time at each batch of descriptors and each line of mappings, so a process that keeps mapping files ahead of the line `writ serve` reads can't keep the read going.

The walk bounds a runaway or careless run, not a run that tries to hide what it uses on disk. Such a run can hide what it uses four ways:

- Hold a removed file only in a descriptor in flight on a Unix socket, which no thread's descriptors list.
- Hold a removed file only in a mapping, which counts as far as its furthest mapping reaches, not its blocks on disk: one mapped page can hide a large file.
- Link a file of its user's from outside its directory, which the walk counts only a share of.
- Move its files with `inotify` timing, so that each walk misses them.

The hard bound is a filesystem quota on the scratch filesystem. Put `tofu.scratch_dir` on a filesystem of its own, so a full scratch can't fill `data` or Postgres, and set ext4 or XFS project quotas on each run's directory, or per-user quotas for `provider_uids`; either also counts the removed files a run keeps. `writ serve` sets no quota itself, and a non-root `writ serve` can't mount a size-limited filesystem where every step of a run sees it.

## Outbound traffic

Two layers decide what a run can reach. The first is always on: it limits what a run fetches and what it can reach without credentials. The second, egress, is opt-in and off by default: it limits every host and port a run connects to.

### Always on

- **Instance metadata blocked**: The `nftables` rule drops `169.254.169.254` and `fd00:ec2::254` for `provider_uids`.
- **Module source allow-list**: Before `tofu` runs, `writ serve` checks every `module` `source` (after recursively decoding percent-escapes). Only local paths inside the repo (`./`, `../`), registry addresses, and remote network addresses with at most `ref` and `depth` query parameters are allowed. `file://`, absolute paths, `~`, `oci://`, fragments (`#`), and `go-getter` parameters like `checksum` or `sshkey` are refused.
- **Git over HTTPS only**: When `tofu.git` is set, Writ sets `GIT_ALLOW_PROTOCOL=https:ssh`, `GIT_CONFIG_NOSYSTEM=1`, and `GIT_CONFIG_GLOBAL=/dev/null`, and omits `ssh` from the view, so only HTTPS module clones succeed.
- **No cloud credentials during `init`**: `tofu init` fetches modules and providers without cloud credentials. Only `plan` and `apply` (and the S3 backend configuration step) receive the run's role credentials.
- **Provider binaries pinned by lock file**: `writ serve` installs providers into `tofu.plugin_cache` itself from `.terraform.lock.hcl` using a synthetic config, so stack code never runs with write access to the plugin cache. The lock file still chooses each provider's registry, and the registry the hosts the install downloads from, as `writ`'s own uid.

Under `egress_mode = "off"`, the default, that is all: a run shares the host's network, so code the commit chose can reach anything the host reaches, with the run's role credentials and the env's `provider_env` values in hand.

### Egress

Set `egress_mode` to `"report"` or `"enforce"` in `/etc/writ/serve`, and give each env an `egress` list of what its runs may reach:

```hcl
egress_mode = "enforce"

env "prod" {
  key    = "/var/lib/writ/keys/prod.key"
  egress = ["*.amazonaws.com", "registry.opentofu.org", "nomad.example.internal:80"]
}
```

Each step of a run then starts in a network namespace of its own that holds only loopback: no other interface, no route, and no DNS server. Its one way out is Writ's egress proxy, which `writ serve` serves on the run's loopback, at `127.0.0.1:3128`, set as `HTTPS_PROXY` and `HTTP_PROXY`. The credential endpoint and the state backend reach the run on its loopback too. [Egress](serve.md#egress) has the full design: the ports, how the proxy knows which run a connection is from, and the record each run keeps.

The provider install runs in a namespace of its own too, behind a proxy that allows any public address and refuses private ones, and it installs only from the registries `provider_registries` lists: `registry.opentofu.org` and `registry.terraform.io` unless you set it. See [The provider install](serve.md#the-provider-install).

An item of `egress` is `HOST`, `*.DOMAIN`, or an IP address, with an optional `:PORT`. An item with no port allows 443 only, so plain HTTP needs `HOST:80`. `*.amazonaws.com` matches `sts.amazonaws.com` and not `amazonaws.com`. See [the config's env block](config.md#env) for every rule `--check` applies to the list.

The two modes:

| `egress_mode` | A destination the list has | A destination the list lacks | Loopback, link-local (the metadata service), multicast, unspecified, `fd00:ec2::254`, and a NAT64 address of one |
| --- | --- | --- | --- |
| `off` (default) | the host's network, no proxy | the host's network, no proxy | reachable, except the metadata addresses the `nftables` rule drops |
| `report` | allowed and recorded | allowed and recorded as `unlisted` | refused |
| `enforce` | allowed and recorded | refused before any lookup | refused |

In every mode the proxy connects only to an address it resolved and checked, and a name that resolves to a private address (RFC 1918, unique local IPv6, `100.64.0.0/10`) is allowed only when the list names it exactly, not through a wildcard.

Under `report` and `enforce`, every plan and run records each `host:port` it asked for, with its verdict, reason, and count. You see it as the Network card on the plan's and run's pages in the control UI, as `network` in `GET /v1/plans/{id}` and `GET /v1/runs/{id}`, and as one `run egress` line in `writ serve`'s log. A plan that fails because the proxy refused a destination names it, with the env's list, in its diagnostics:

```
error: writ's egress proxy refused 192.0.2.11:8080 (an IP address the list does not have); env prod's egress is [192.0.2.10:8080], under egress_mode enforce: add what the stack needs to the env's egress list
```

`writ serve --check` prints the mode, each env's list, and `provider_registries` when the mode is on:

```
egress: enforce; each OpenTofu run reaches the network only through writ's egress proxy, which refuses a destination its env's egress does not list
egress: env prod allows *.amazonaws.com, registry.opentofu.org, nomad.example.internal:80
egress: providers install only from registry.opentofu.org, registry.terraform.io
```

#### What to list

Providers come from the plugin cache, which `writ serve` fills behind the install's own proxy before the run starts, so the list doesn't need the providers' registry. List what the run itself reaches:

- the cloud APIs your providers call, such as `*.amazonaws.com`, and any other API a provider talks to, such as `nomad.example.internal:4646`. With VPC interface endpoints and their private DNS, AWS names such as `sts.eu-west-1.amazonaws.com` resolve to private addresses in your VPC, which `*.amazonaws.com` doesn't allow, so list each endpoint's exact name;
- the S3 endpoint of a stack whose state is on S3;
- for remote modules, `registry.opentofu.org`, the host its download URLs point to, and your Git host.

#### Roll it out

1. Set `egress_mode = "report"` and restart `writ serve`. Runs behave as before, through the proxy.
2. Let a few days of plans, applies, and drift checks run.
3. Read the `unlisted` destinations from the Network cards, `network` in the API, or the `run egress` log lines.
4. Add each host your stacks need to its env's `egress`, as narrow as it can be: a name before a wildcard, a port when it isn't 443.
5. Set `egress_mode = "enforce"` and restart. A destination you missed fails its plan with a diagnostic that names it.

#### What egress doesn't cover

- **An allowed host can still carry data out.** `*.amazonaws.com` allows any S3 bucket, an attacker's included. The proxy decides by host and port, not by URL, account, or content, and passes a `CONNECT`'s TLS through untouched.
- **`report` isn't a boundary.** It lets a run reach what the list lacks, private addresses and the host's own addresses included, and resolves every name it's asked, so a lookup can carry data out. Only `enforce` refuses a name the list lacks before any lookup.
- **Clients that ignore proxy settings fail.** Under `report` and `enforce`, a client that ignores `HTTPS_PROXY` or connects to an IP address directly finds no route and no DNS. A client that sends the proxy only IP addresses needs each address listed.
- **A listed name is resolved by `writ serve`'s resolver**, so a run can make it look up any name the list allows.
- **Not every path runs behind it.** `writ serve --dev` has no `egress_mode`, and `writ import bulk` runs providers on the host's network.
- **A private registry can't serve the provider install.** The install's proxy refuses every private address, so a registry or download host on one fails the install under `report` and `enforce`.

If you need limits that egress doesn't give, such as per-bucket rules, add them at your VPC firewall or with `meta skuid 61000-61063` rules in `nftables`.

## Troubleshooting

- **`isolation not checked: operation not permitted` during `--check`**: You ran `--check` as an unprivileged user without `CAP_SETUID`/`CAP_SETGID`/`CAP_KILL`. Run it under the service's capabilities (or let systemd's `ExecStartPre` run it).
- **Namespace `/proc` mount fails under systemd**: If your systemd unit sets `ProtectKernelTunables=true` on a kernel older than Linux 7.2, mounting the PID-scoped `/proc` inside the view can fail. Remove `ProtectKernelTunables` on kernels before 7.2 (the NixOS module handles this automatically).
- **Plan fails because a `local-exec` provisioner or `external` data source cannot find `/bin/sh`**: The filesystem view intentionally omits shells. Replace shell provisioners with declarative resources or a static binary inside the stack.
- **`provider_uids` exhausted with withheld UIDs**: If a provider process refuses to exit after a run, Writ withholds its UID until `writ serve` restarts. Check logs for the stuck run and restart `writ serve` during a quiet window.
- **A plan fails with `writ egress refused HOST:PORT`** under `enforce`: the env's `egress` lacks that destination. Add it, or run under `report` first to collect what your stacks reach. See [Roll it out](#roll-it-out).
- **A plan fails with `which provider_registries does not list`** under `report` or `enforce`: the stack's lock file has a provider from a registry `provider_registries` lacks. Add the registry's host to `provider_registries` if you trust it.
- **A step fails with `network is unreachable` or a failed lookup** under `report` or `enforce`: a process of the run connected without the proxy, and its diagnostics say so. A run has no route but the proxy, so that client needs to honor `HTTPS_PROXY`.
- **SSH module source (`git@host:org/repo`) fails**: The view has no `ssh` binary or keys. Switch module sources to HTTPS (`git::https://...`).
- **`limits not enforced: writ's cgroup ... holds processes other than writ serve`**: Another process shares `writ serve`'s cgroup, as a shell's does. Run `writ serve` as a systemd unit with `Delegate=yes`, or as a task's only process. See [How writ serve makes the cgroups](#how-writ-serve-makes-the-cgroups).
- **`limits not enforced: writ's cgroup ... is not delegated to uid N`**: The unit lacks `Delegate=yes`, or sets `ProtectControlGroups=yes`. See [Under systemd](#under-systemd).
- **A plan fails with `stopped at a limit: the run used all of limits.run_memory`**: The step's processes together needed more memory. Raise `limits.run_memory`, or split the stack. The same goes for `run_pids` and `run_scratch`.

## Next steps

- [AWS roles and credentials](aws-credentials.md): pair run isolation with short-lived per-run STS roles.
- [Egress](serve.md#egress): how a run's network, the proxy, and its record work.
- [Security model](security-model.md): complete overview of Writ's trust boundaries.
- [Run a server](run.md): systemd and NixOS deployment details.
