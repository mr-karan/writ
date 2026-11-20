# Run isolation and egress

Isolate OpenTofu and provider processes from `writ serve`'s keys, database, and host identity, and limit the hosts a run can reach with the opt-in egress proxy.

Because a merge request chooses the OpenTofu code and providers that run on your server, a team's `writ serve` never runs OpenTofu as its own OS user. For the underlying architecture, see [How the control plane works: How providers are kept from the keys](serve.md#how-providers-are-kept-from-the-keys).

## Prerequisites

- A Linux host. (User, mount, and PID namespaces and `provider_uids` require Linux.)
- A range of unused numeric UIDs on the host, with at least as many UIDs as `limits.plans` (for example `61000-61063`). They do not need `/etc/passwd` entries.
- `CAP_SETUID`, `CAP_SETGID`, and `CAP_KILL` granted to the `writ serve` service (the [NixOS module](run.md#on-nixos) configures these automatically).
- `nftables` (or an equivalent firewall) on hosts with an instance metadata service.
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

## Next steps

- [AWS roles and credentials](aws-credentials.md): pair run isolation with short-lived per-run STS roles.
- [Egress](serve.md#egress): how a run's network, the proxy, and its record work.
- [Security model](security-model.md): complete overview of Writ's trust boundaries.
- [Run a server](run.md): systemd and NixOS deployment details.
