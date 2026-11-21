# Run a control plane

Run `writ serve` in two modes: `writ serve --dev` for single-user local work with zero config, or `writ serve --config FILE` for a team server backed by Postgres and OIDC sign-in.

For the end-to-end plan and apply lifecycle, see [How a plan becomes an apply](plan-to-apply.md).

## writ serve --dev

`writ serve --dev` runs the same API and server code as a team deployment using SQLite and no external issuer.

Start it in one terminal:

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

Output of a real run, 2026-11-16, with a `tofu` block setting the binary, plugin cache, and scratch directory.

Remote CLI commands discover `writ serve --dev` automatically ([How a CLI finds its control plane](sign-in.md#how-a-cli-finds-its-control-plane)). No command starts a dev server implicitly. Dev mode defines no approval rules, so plans end `approved` and apply immediately with `writ apply --remote ID`. Startup median is 8.5 ms on tmpfs ([`bench/RESULTS.md`](../bench/RESULTS.md#a-dev-control-plane-2026-11-10)).

### What the dev control plane does

- **Store:** SQLite `control.db` under `$XDG_DATA_HOME/writ/dev` (or `~/.local/share/writ/dev`, overridden by `--data DIR`). Pass `--database postgres://...` to use Postgres instead.
- **Listen:** Defaults to `127.0.0.1:7340`. `--listen` accepts loopback addresses only (`:0` picks an ephemeral port).
- **Identity:** Generates a 32-byte random token on start and writes URL + token to `client.json` (`0600` inside `0700`). Grants full permissions (`dev` team, principal `dev/USER`).
- **Environments:** Any valid name (`[a-z][a-z0-9_-]{0,62}`) works on demand, creating `keys/ENV.key` automatically. `--env` defaults to `dev`.
- **Credentials:** No AWS role assumption. Runs receive `PATH=/usr/local/bin:/usr/bin:/bin` (prefixed with `tofu.git`'s dir if set), `AWS_EC2_METADATA_DISABLED=true`, `TF_VAR_*` from the starting shell, and a per-run `HOME`.
- **Isolation in dev mode:** Dev mode needs no `provider_uids` and runs OpenTofu as your user. On Linux, runs execute inside the read-only filesystem view so they can't read your other files (though they still share your network). On non-Linux hosts, OpenTofu runs with your user's filesystem access, and `--data` must keep `TMPDIR` short enough for macOS's 103-byte Unix socket paths. Plan only stacks you trust with that access.
- **Single instance:** Refuses to start if another dev server answers for the same data directory; clean shutdown removes `client.json`.
- **Control UI:** Serves `/ui/` with a one-time 10-minute sign-in link printed at startup and by `writ ui`.
- **Repositories:** `writ repo add .` registers a local Git repo. With `--config FILE`, dev mode also accepts `forge`, `repo`, `poll_every`, `poll_every_with_hook`, `tofu`, and `policy` ([Add a repository](add-repository.md)).

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

`--check` validates the config, prints team grants, checks `tofu version`, probes `provider_uids` isolation (including EC2 metadata blocking), and reads notifier delivery counts from Postgres (ignoring unreachable or unmigrated databases). It doesn't contact Git hosts or OIDC issuers, and sends test notifications only with `--notify-test` ([Get notified](notifications.md#check-the-setup)).

Example `--check` output (unprivileged user, so UID switching warns):

```
config /var/tmp/writ-s17/tmp.eSdEbz0YlP/serve: ok
url: https://writ.example.com
issuers: gitlab (gitlab-ci, https://gitlab.com)
envs: prod, state in postgres://writ@/writ
providers run as: uids 61000-61063, one for each running plan
tofu: /nix/store/60vqi4v8n11afd5ks89j1kn7pgwfrww5-opentofu-1.13.9/bin/tofu, plugin cache /var/lib/writ-tofu/plugins, scratch directories in /var/lib/writ-tofu/runs
aws: sts in eu-west-1 at the regional endpoint, sessions of 1h
role: prod plan arn:aws:iam::222222222222:role/writ-plan
approvals: no rule, so no plan needs an approval
TEAM      ENV   STACKS  CAN        RISKS
infra_mr  prod  *       read plan  -
tofu: /nix/store/60vqi4v8n11afd5ks89j1kn7pgwfrww5-opentofu-1.13.9/bin/tofu is OpenTofu 1.13.9: ok
warning[usage]
  isolation not checked: run writ as provider uid 61000: fork/exec /nix/store/a3k9grqxjk9dxgmzrvs2v57yx454ygqw-writ-f5f8f09/bin/.writ-wrapped: operation not permitted; writ serve checks its capabilities at start
```

Output of `just check-nixos`, 2026-11-16.

At startup, `writ serve` runs schema migrations, fetches every OIDC issuer's discovery document and JWKS (failing if any is unreachable), and logs JSON decisions to stdout. On `SIGTERM`, it stops accepting new requests, drains in-flight requests for up to 30 seconds, and marks unfinished plans `failed`.

### Database connection and timeouts

`writ serve` stores all control plane records and HTTP-backend state (newest 20 versions per `stack@env`) in Postgres schema `writ_control` (S3-backend stacks keep their state in S3). Never embed Postgres passwords in `database` URLs; use Unix-socket peer authentication, `?passfile=/path/to/pgpass`, or `PGPASSFILE`.

- **Postgres:** 5s connect timeout, 10s statement timeout, 5s lock timeout, 15s idle-in-transaction timeout.
- **SQLite (`--dev`):** 5s busy timeout.

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

When `providerUids` is set, the module grants `CAP_SETUID`, `CAP_SETGID`, and `CAP_KILL`, creates `/var/lib/writ-tofu`, installs the EC2 metadata firewall rule (into `networking.nftables` when the host enables it, which flushes every other table when it loads, or else via the `writ-serve-metadata-firewall` unit), enables `Delegate=yes` (leaving `ProtectControlGroups` off for per-run cgroups), allows user, mount, PID, and IPC namespaces, plus network namespaces for `egress_mode` `report` and `enforce` (enabling `ProtectKernelTunables` only on Linux 7.2+), puts Nix's `git` on `PATH`, and runs `writ serve --check` in `ExecStartPre`.

`just check-nixos` evaluates the module, checks that its `RestrictNamespaces` allows `net`, and validates its sample config against the built `writ` and OpenTofu 1.13.9 packages, including a run in its own network namespace.

**Limits:** The NixOS module has been validated by `just check-nixos`, not yet booted on a live NixOS host. See [Install](install.md) for standalone binaries and containers.

## Back up and upgrade

- Back up Postgres (`pg_dump`) and store encrypted offline copies of every `env.*.key` file (losing a key makes sealed states, saved plans, and secret sets unreadable).
- Hourly pruning expires plans past `plan_ttl`, removes expired CI token `jti`s, and prunes closed `apply_requests` rows after 25 hours. Set `retention.plans` and `retention.audit` to prune historical data.
- Upgrade by stopping the single node and starting the new binary (which runs migrations atomically; downgrades are refused).

See [Back up and upgrade](upgrades.md) for the full runbook.

## Troubleshooting

- **Second `writ serve --dev` refuses to start**: Pass `--data DIR` and `--listen 127.0.0.1:0` for a separate dev instance.
- **Startup fails fetching an issuer**: Verify DNS, HTTPS connectivity, and `ca_file` for every OIDC issuer.
- **`--check` refuses `tofu` without `provider_uids`**: Configure `provider_uids` ([Run isolation and egress](run-isolation.md)).
- **Two `writ serve` nodes share one database**: Stop one immediately; each node fails in-flight runs owned by other process IDs every minute.

## Next steps

- [Sign in](sign-in.md): configure CLI and control UI sign-in.
- [AWS roles and credentials](aws-credentials.md) and [Run isolation and egress](run-isolation.md).
- [Add a repository](add-repository.md) and [Back up and upgrade](upgrades.md).
- [Config](config.md): full server configuration reference.
