# Run a control plane

Run `writ serve --dev` for single-user local work with no config, or `writ serve --config FILE` for a team server with Postgres and OIDC sign-in. The plan and apply lifecycle is in [How a plan becomes an apply](plan-to-apply.md).

## writ serve --dev

It runs the same API and server code as a team deployment, with SQLite and no external issuer. Start it in one terminal:

```
$ writ serve --dev --config dev.hcl
writ serve --dev: http://127.0.0.1:7340, for alice only
data: /home/alice/.local/share/writ/dev (control.db, state/, keys/)
OpenTofu runs get PATH=/usr/local/bin:/usr/bin:/bin and this shell's TF_VAR_ variables, and no other variable of this shell.
control UI: http://127.0.0.1:7340/ui/login/dev?code=57CK-7N7L (works once, for 10 minutes; writ ui prints another)
ready in 400 ms
```

Check your access in a second terminal:

```
$ writ whoami
http://127.0.0.1:7340 (from the dev control plane), dev token of alice (dev)
teams: dev
*  **  read plan apply approve override adopt admin  create update replace delete data-loss
```

Real run, 2026-11-16, with a `tofu` block setting the binary, plugin cache, and scratch directory.

- Remote CLI commands find `writ serve --dev` on their own ([How a CLI finds its control plane](how-sign-in-works.md#how-a-cli-finds-its-control-plane)). No command starts a dev server implicitly.
- Dev mode has no approval rules, so plans end `approved` and `writ apply --remote ID` applies them at once.

Its store, listen address, identity, environments, credentials, and isolation are in [writ serve --dev defaults](config.md#writ-serve---dev-defaults).

> **Warning:** dev mode runs OpenTofu as your user. On Linux, runs cannot read your other files but share your network; elsewhere, OpenTofu has your user's filesystem access. Plan only stacks you trust with that access.

## Run one for a team

A team's `writ serve` requires Postgres (`database = "postgres://..."`) and an HCL config file ([Config](config.md)).

### Before you begin

- A Linux host with Postgres.
- A server config file (for example `/etc/writ/serve`).
- A `provider_uids` range and `tofu` block for OpenTofu runs ([Run isolation and egress](run-isolation.md)).

### Check the config, then start the server

```sh
writ serve --config /etc/writ/serve --check
writ serve --config /etc/writ/serve
```

`--check` validates the config, prints team grants, checks `tofu version`, probes `provider_uids` isolation (EC2 metadata blocking included), and reads notifier delivery counts from Postgres, ignoring an unreachable or unmigrated database. It contacts no Git host or OIDC issuer, and sends test notifications only with `--notify-test` ([Get notified](notifications.md#check-the-setup)). Run as an unprivileged user, UID switching warns:

```
config /var/tmp/writ-s17/tmp.eSdEbz0YlP/serve: ok
url: https://writ.example.com
providers run as: uids 61000-61063, one for each running plan
tofu: /nix/store/60vqi4v8n11afd5ks89j1kn7pgwfrww5-opentofu-1.13.9/bin/tofu is OpenTofu 1.13.9: ok
warning[usage]
  isolation not checked: run writ as provider uid 61000: fork/exec /nix/store/a3k9grqxjk9dxgmzrvs2v57yx454ygqw-writ-f5f8f09/bin/.writ-wrapped: operation not permitted; writ serve checks its capabilities at start
```

Excerpt of a real run, 2026-11-16.

- At startup, `writ serve` runs schema migrations, fetches every OIDC issuer's discovery document and JWKS (failing if any is unreachable), and logs JSON decisions to stdout.
- On `SIGTERM`, it stops accepting requests, drains in-flight requests for up to 30 seconds, and marks unfinished plans `failed`.

### Database

Use Postgres with peer authentication or a passfile; never put a Postgres password in the `database` URL. Schema, retention of state versions, and timeouts are in [Database](config.md#database).

### On NixOS

Import `nixosModules.writ-serve` from the flake (or `deploy/nixos/writ-serve.nix`) to run `writ serve` as a hardened `writ` system user backed by local peer-authenticated Postgres:

```nix
{
  inputs.writ.url = "github:mr-karan/writ";
  outputs = { nixpkgs, writ, ... }: {
    nixosConfigurations.control = nixpkgs.lib.nixosSystem {
      system = "x86_64-linux";
      modules = [
        writ.nixosModules.writ-serve
        {
          services.writ-serve = {
            enable = true;
            configFile = ./serve;         # database: "postgres://writ@/writ?host=/run/postgresql"
            bindPrivilegedPort = true;    # listen on 443
            providerUids = "61000-61063"; # the config's provider_uids
          };
        }
      ];
    };
  };
}
```

> **Warning:** with `providerUids` set on a host that enables `networking.nftables`, the module adds its rule there, and `networking.nftables` flushes every other table when it loads.

When `providerUids` is set, the module:

- grants `CAP_SETUID`, `CAP_SETGID`, and `CAP_KILL`, and creates `/var/lib/writ-tofu`;
- installs the EC2 metadata firewall rule, into `networking.nftables` when the host enables it, or else through the `writ-serve-metadata-firewall` unit;
- sets `Delegate=yes`, leaving `ProtectControlGroups` off for per-run cgroups;
- allows user, mount, PID, and IPC namespaces, plus network namespaces for `egress_mode` `report` and `enforce`, and enables `ProtectKernelTunables` only on Linux 7.2+;
- puts Nix's `git` on `PATH`, and runs `writ serve --check` in `ExecStartPre`.

**Limits:** The NixOS module has been validated against a sample config and built packages, not yet booted on a live NixOS host. See [Install](install.md) for standalone binaries and containers.

## Back up and upgrade

Back up Postgres and every `env.*.key`, prune with `retention`, and upgrade by stopping the single node and starting the new binary (it runs migrations atomically; downgrades are refused): see [Back up and upgrade](upgrades.md).

## Troubleshooting

| Problem | Fix |
| --- | --- |
| A second `writ serve --dev` refuses to start | Pass `--data DIR` and `--listen 127.0.0.1:0` for a separate instance. |
| Startup fails fetching an issuer | Check DNS, HTTPS connectivity, and `ca_file` for every OIDC issuer. |
| `--check` refuses `tofu` without `provider_uids` | Set `provider_uids` ([Run isolation and egress](run-isolation.md)). |
| Two `writ serve` nodes share one database | Stop one at once. Every minute, each node fails in-flight runs owned by other process IDs. |

## Next steps

- [Sign in](sign-in.md): configure CLI and control UI sign-in.
- [AWS roles and credentials](aws-credentials.md) and [Run isolation and egress](run-isolation.md).
- [Add a repository](add-repository.md) and [Back up and upgrade](upgrades.md).
- [Config](config.md): full server configuration reference.
