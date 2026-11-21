# Evaluates a NixOS system with this repository's flake module, which
# imports writ-serve.nix and defaults its package to the flake's writ, on
# the flake's pinned nixpkgs. It returns the unit file, the Postgres setup,
# the config text, the writ package, the OpenTofu of the config's tofu
# block, and the git on the unit's PATH. `just check-nixos` prints them,
# builds that package and that OpenTofu, checks the config with them, and
# checks that the unit's PATH has that git.
let
  tofu = nixpkgs.legacyPackages.${builtins.currentSystem}.opentofu;
  serveConfig = ''
    url           = "https://writ.example.com"
    listen        = "0.0.0.0:443"
    database      = "postgres://writ@/writ?host=/run/postgresql"
    provider_uids = "61000-61063"
    egress_mode   = "report"

    tls {
      cert = "/var/lib/writ/tls/fullchain.pem"
      key  = "/var/lib/writ/tls/key.pem"
    }

    tofu {
      binary       = "${tofu}/bin/tofu"
      plugin_cache = "/var/lib/writ-tofu/plugins"
      scratch_dir  = "/var/lib/writ-tofu/runs"
    }

    aws {
      region = "eu-west-1"
    }

    issuer "gitlab" {
      kind     = "gitlab-ci"
      identity = "gitlab"
      url      = "https://gitlab.com"
    }

    team "infra_mr" {
      issuer = "gitlab"
      match  = { project_id = ["4242"], pipeline_source = ["merge_request_event"] }
    }

    grant {
      teams  = ["infra_mr"]
      stacks = ["*"]
      envs   = ["prod"]
      can    = ["read", "plan"]
    }

    env "prod" {
      key       = "/var/lib/writ/keys/prod.key"
      plan_role = "arn:aws:iam::222222222222:role/writ-plan"
    }
  '';
  flake = builtins.getFlake "git+file://${toString ../..}";
  nixpkgs = flake.inputs.nixpkgs;
  system = nixpkgs.lib.nixosSystem {
    system = builtins.currentSystem;
    modules = [
      flake.nixosModules.writ-serve
      ({ pkgs, ... }: {
        boot.loader.grub.enable = false;
        fileSystems."/" = { device = "/dev/null"; fsType = "ext4"; };
        system.stateVersion = "26.05";
        services.writ-serve = {
          enable = true;
          configFile = pkgs.writeText "serve" serveConfig;
          bindPrivilegedPort = true;
          providerUids = "61000-61063";
        };
      })
    ];
  };
  c = system.config;
in
{
  config = serveConfig;
  writ = c.services.writ-serve.package;
  inherit tofu;
  git = system.pkgs.git.outPath;
  unit = c.systemd.units."writ-serve.service".text;
  firewall = c.systemd.units."writ-serve-metadata-firewall.service".text;
  tmpfiles = builtins.filter (r: nixpkgs.lib.hasInfix "writ" r) c.systemd.tmpfiles.rules;
  postgres = {
    enable = c.services.postgresql.enable;
    databases = c.services.postgresql.ensureDatabases;
    users = map (u: u.name) c.services.postgresql.ensureUsers;
  };
}
