# Run a control plane

You can run `writ serve` two ways. If you want to try Writ, `writ serve --dev` runs one for you alone, in one command, with no config. When your team is ready, a team's `writ serve` runs on a host with Postgres, a config file, and sign-in for people and CI jobs.

To learn what the control plane does with a plan, read [How the control plane works](serve.md).

## writ serve --dev

With `writ serve --dev`, you get a control plane of your own: the same API and the same code as a team's `writ serve`, with no config, no issuer, and its store in SQLite. Use it to try Writ, and to develop and test it.

Start it in one terminal:

```
$ writ serve --dev --config dev.hcl
writ serve --dev: http://127.0.0.1:7340, for alice only
data: /home/alice/.local/share/writ/dev (control.db, state/, keys/)
OpenTofu runs get PATH=/usr/local/bin:/usr/bin:/bin and this shell's TF_VAR_ variables, and no other variable of this shell.
control UI: http://127.0.0.1:7340/ui/login/dev?code=57CK-7N7L (works once, for 10 minutes; writ ui prints another)
ready in 400 ms
```

Then, from a second terminal, check what you can do:

```
$ writ whoami
http://127.0.0.1:7340 (from the dev control plane), dev token of alice (dev)
teams: dev
*  **  read plan apply approve override adopt admin  create update replace delete data-loss
```

Output of a real run, 2026-11-16, with a config file whose `tofu` block sets the `tofu` binary, a plugin cache, and a scratch directory. When your `tofu` block also sets `git`, the `PATH` in the banner starts with Git's directory, as each run's does.

Your remote commands find the dev control plane with no flag and no token. [How a CLI finds its control plane](sign-in.md#how-a-cli-finds-its-control-plane) gives the order. No command starts a dev control plane for you. If a command finds none, it prints the command to run.

You don't need approvals here: the dev control plane has no approval rule, so each plan needs none and is `approved` when it ends, and `writ apply --remote ID` applies it. In `writ whoami` you see the dev team's one grant: every environment, `*`, and every stack at every depth, `**`. For applies on a team's control plane, see [Approve and apply a plan](apply.md).

It starts fast. Measured on this machine on 2026-11-10, with the data directory on tmpfs, `writ serve --dev` answers `GET /v1/server` a median of 8.5 ms after it's started, over 30 starts. [bench/RESULTS.md](../bench/RESULTS.md#a-dev-control-plane-2026-11-10) has the method.

### What the dev control plane does

- **Store.** Your records go in `control.db`, in the data directory, which is `$XDG_DATA_HOME/writ/dev`, else `~/.local/share/writ/dev`, or `--data DIR`. To keep the control database in Postgres instead, pass `--database postgres://...`.
- **Listen.** By default it listens on `127.0.0.1:7340`. With `--listen` you can set another loopback address, and it refuses any other. With port 0 the kernel picks one.
- **Identity.** At start it creates a token of 32 random bytes and writes the URL and the token to `client.json` in the data directory, mode 0600, in a directory of mode 0700. Only that token works. You're the principal `dev/USER`, of kind `dev`, in the team `dev`, which can do every operation with every risk class on every `stack@env`.
- **Environments.** Any name of lowercase letters, digits, `_`, and `-` that starts with a letter, up to 63 characters, is an environment. Its key is created in `keys/ENV.key` the first time a plan opens it. If your request doesn't set an environment, it's in `dev`, so `--env` is optional.
- **Credentials.** It assumes no role. Your OpenTofu runs get `PATH=/usr/local/bin:/usr/bin:/bin`, `AWS_EC2_METADATA_DISABLED=true`, and the `TF_VAR_` variables of the shell that started it, and no other variable from that shell. Each run's `HOME` is its own directory. The start banner says so.
- **One at a time.** If you start a second `writ serve --dev` on the same data directory, it refuses to start while the first one answers. A clean stop removes `client.json`.
- **Control UI.** It serves the control UI. In the start banner you'll find a sign-in link whose code works once, for 10 minutes, and `writ ui` prints another. The link gives your browser a session of the dev user.
- **Repositories.** `writ repo add .` adds the local Git repository of the current directory, which the dev control plane reads with the `git` binary. With `--config FILE` you can add `forge` and `repo` blocks and `poll_every`, so a GitLab project works too, with a read token. That file can also have a `tofu` block and a `policy` block, as a team's config does. [Add a repository](add-repository.md) shows both.

## Run one for a team

Your team's `writ serve` needs Postgres for its records and for the state of every stack it plans, so its config's `database` must be `postgres://`. You write one config file, in HCL. The [server config](config.md) describes every setting.

### Before you begin

- A host with Postgres.
- A config file, such as `/etc/writ/serve`.
- For OpenTofu stacks, a range of `provider_uids` and the `tofu` block. [How providers are kept from the keys](serve.md#how-providers-are-kept-from-the-keys) explains why.

### Check the config, then start the server

Run the check first, then start the server with the same config:

```
$ writ serve --config /etc/writ/serve --check
$ writ serve --config /etc/writ/serve
```

Here, `--check` parses the config and prints what each team can do. With a `tofu` block, it runs `tofu version`. With `provider_uids` it also runs `writ` as a provider's uid to check what that uid can read and reach, including the EC2 instance metadata addresses. With `notify` blocks, it reads each notifier's delivery counts from Postgres, and a database it can't reach, or one `writ serve` hasn't migrated yet, doesn't fail it. It doesn't contact your Git host or the issuers, and it sends a notification only with `--notify-test`. See [Get notified](notifications.md#check-the-setup).

Here's what it printed for the NixOS module's sample config. That run was as a user who can't switch users, so the isolation check only warns:

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

Output of `just check-nixos`, 2026-11-16. To see the isolation check on a host where it runs, read [Check the isolation](serve.md#check-the-isolation).

When you start it, `writ serve` migrates its Postgres schema and fetches every issuer's discovery document and key set. If one can't be fetched, it fails. Once running, it logs JSON to stdout, one line for each decision.

On SIGTERM, it stops new requests, waits 30 seconds for running ones, and fails the plans still running. If it crashed instead, the next start marks a plan left running as `failed`.

### On NixOS

If your host runs NixOS, use the module in `deploy/nixos/writ-serve.nix`. This module runs `writ serve` as the system user `writ` with a hardened unit, and Postgres on the same host. Your `writ` user reaches its database over the Unix socket with peer authentication, so you don't need a database password.

You'll find it in the repository's flake as `nixosModules.writ-serve`, with the flake's `writ` package as the default `package`:

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

Without the flake, import `deploy/nixos/writ-serve.nix` and set `package` to any package whose `bin/writ` is the binary.

When you set `providerUids`, the unit gets `CAP_SETUID`, `CAP_SETGID`, and `CAP_KILL`. The module also creates `/var/lib/writ-tofu` for the `tofu` block's `plugin_cache` and `scratch_dir`, and loads the firewall rule from [How providers are kept from the keys](serve.md#how-providers-are-kept-from-the-keys). It sets `Delegate=yes` and leaves `ProtectControlGroups` off, so `writ serve` can give each OpenTofu step a cgroup of its own with the config's [run limits](run-isolation.md#limit-what-each-run-uses).

If your host sets `networking.nftables.enable`, the rule joins that ruleset, which flushes every other table when it loads. Otherwise a unit of the module, `writ-serve-metadata-firewall`, loads it before `writ serve` starts.

The unit lets `writ serve` create user, mount, and pid namespaces, which each `tofu` process's [filesystem view](serve.md#what-a-run-can-see) needs. On a kernel of 7.2 or later, where that view's `/proc` mounts under it, it sets `ProtectKernelTunables`, and on no older kernel. Because the unit's `PATH` has Nix's `git`, writing `git = "git"` in the config's `tofu` block gives each run that `git`. Before it starts, the unit runs `writ serve --check`.

To check the module, run `just check-nixos`. This evaluates the flake's module on the flake's pinned nixpkgs, and builds the `writ` package that its unit runs and the pinned OpenTofu. Then it checks the sample config, whose `tofu` block points to that OpenTofu, with that binary. It fails when the check doesn't find OpenTofu 1.13.9, because `writ serve` would then refuse every stack of `.tf` files.

**Limits.** No NixOS machine has run the module. For the other ways to get `writ`, see [Install](install.md).

## Back up and upgrade

- **Back up the database** with `pg_dump` or the host's Postgres backups. That database has the state of every served stack and the audit log. Under `writ serve --dev`, the data directory has all of it: `control.db` and `keys/`.
- **Back up each state key** offline and encrypted. If you lose it, every sealed secret in that environment's state is lost, and you can't check its saved plans.
- **Know what's deleted.** Once an hour `writ serve` marks live plans past `plan_ttl` as `expired`, and deletes token ids past their token's expiry. It also deletes the `none` rows of apply request keys 25 hours after each key's time, when the key is refused. Writ keeps the greatest time of a key it deleted, so the key stays refused when the clock moves back. If you set `retention.plans`, it deletes no-changes, refused, failed, expired, and superseded plans older than that, with their events, unless an approval or a run refers to them. If you set `retention.audit`, it deletes older audit rows. Without them, nothing else is deleted.
- **Upgrade** by starting the new `writ`. At start, it migrates the schema in one transaction. A database migrated by a newer `writ` is refused.

For step-by-step backup commands, retention setup, and single-node upgrade steps, see [Back up and upgrade](upgrades.md).

## Troubleshooting

- **Second `writ serve --dev` refuses to start**: Only one dev server can use a data directory at a time. Pass `--data DIR` and `--listen 127.0.0.1:0` to run a second isolated dev instance.
- **`writ serve` fails at start because an issuer is unreachable**: On start, `writ serve` fetches every configured OIDC issuer's discovery document and JWKS. Verify network connectivity, DNS, and `ca_file` (if using a private CA).
- **`writ serve --check` refuses `tofu` without `provider_uids`**: A team's server never runs OpenTofu as its own OS user. Follow [Run isolation and egress](run-isolation.md) to configure `provider_uids`.
- **Two `writ serve` processes pointed at the same Postgres database**: Stop one immediately. Each process fails in-flight runs owned by other process IDs on start and every minute after.

## Next steps

- [Configure sign-in](sign-in.md) from the CLI and the control UI.
- [AWS roles and credentials](aws-credentials.md) and [Run isolation and egress](run-isolation.md).
- [Add a repository](add-repository.md) and [Back up and upgrade](upgrades.md).
- [Server config](config.md): every setting.
