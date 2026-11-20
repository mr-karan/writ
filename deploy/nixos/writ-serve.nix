# NixOS module for writ serve, the Writ control plane, with its Postgres on
# the same host. Import it, set package and configFile, and enable it.
# The flake's nixosModules.writ-serve imports this file with package set
# to the flake's writ. docs/serve.md explains each setting.
{ config, lib, pkgs, ... }:

let
  cfg = config.services.writ-serve;
  writ = lib.getExe' cfg.package "writ";
  isolated = cfg.providerUids != null;
  capabilities = lib.optional cfg.bindPrivilegedPort "CAP_NET_BIND_SERVICE"
    ++ lib.optionals isolated [ "CAP_SETUID" "CAP_SETGID" "CAP_KILL" ];
  # Drops the provider uids' traffic to the instance metadata service, so
  # a provider cannot reach the host's identity.
  metadataChain = ''
    chain out {
      type filter hook output priority 0;
      meta skuid ${cfg.providerUids} ip daddr 169.254.169.254 drop
      meta skuid ${cfg.providerUids} ip6 daddr fd00:ec2::254 drop
    }
  '';
  # A host that manages nftables itself flushes the whole ruleset when it
  # loads its own, so the rule joins that ruleset. Otherwise a unit of
  # this module loads it. The first two lines make loading it again
  # replace the table instead of adding to it.
  ownRuleset = config.networking.nftables.enable;
  firewallUnit = if ownRuleset then "nftables.service" else "writ-serve-metadata-firewall.service";
  metadataRules = pkgs.writeText "writ-metadata.nft" ''
    table inet writ
    delete table inet writ
    table inet writ {
    ${metadataChain}
    }
  '';
in
{
  options.services.writ-serve = {
    enable = lib.mkEnableOption "writ serve, the Writ control plane";

    package = lib.mkOption {
      type = lib.types.package;
      description = ''
        A package whose bin/writ is the writ binary. The flake's
        nixosModules.writ-serve sets it to the flake's writ.
      '';
    };

    configFile = lib.mkOption {
      type = lib.types.path;
      description = ''
        The writ serve config, in HCL. It holds no secret. Point
        database at "postgres://writ@/writ?host=/run/postgresql" to use the
        local Postgres this module sets up.
      '';
    };

    createDatabase = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = ''
        Run Postgres on this host with a database and a role named writ,
        which the writ system user reaches over the Unix socket with peer
        authentication, so no password exists.
      '';
    };

    bindPrivilegedPort = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = "Let writ serve listen on a port below 1024, such as 443.";
    };

    providerUids = lib.mkOption {
      type = lib.types.nullOr (lib.types.strMatching "[0-9]+-[0-9]+");
      default = null;
      example = "61000-61063";
      description = ''
        The provider_uids of configFile. writ serve runs each plan's
        providers as one of these uids, which no user on the host may
        have. Setting it gives writ serve CAP_SETUID, CAP_SETGID, and
        CAP_KILL, delegates the unit's cgroup to the writ user so that
        each OpenTofu step gets a cgroup of its own with the config's
        run limits, makes /var/lib/writ-tofu for the tofu block's
        plugin_cache and scratch_dir, which must be outside data, and
        loads an nftables rule that drops these uids' traffic to the
        instance metadata addresses. writ serve --check, which runs
        before each start, checks the rule from one of these uids.
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    users.users.writ = {
      isSystemUser = true;
      group = "writ";
      home = "/var/lib/writ";
    };
    users.groups.writ = { };

    services.postgresql = lib.mkIf cfg.createDatabase {
      enable = true;
      ensureDatabases = [ "writ" ];
      ensureUsers = [
        {
          name = "writ";
          ensureDBOwnership = true;
        }
      ];
    };

    systemd.tmpfiles.rules = lib.optionals isolated [
      "d /var/lib/writ-tofu 0755 writ writ -"
    ];

    networking.nftables.tables.writ = lib.mkIf (isolated && ownRuleset) {
      family = "inet";
      content = metadataChain;
    };

    systemd.services.writ-serve-metadata-firewall = lib.mkIf (isolated && !ownRuleset) {
      description = "Keep writ serve's providers from the instance metadata service";
      wantedBy = [ "multi-user.target" ];
      before = [ "writ-serve.service" ];
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        ExecStart = "${pkgs.nftables}/bin/nft -f ${metadataRules}";
        ExecStop = "${pkgs.nftables}/bin/nft delete table inet writ";
      };
    };

    systemd.services.writ-serve = {
      description = "writ serve, the Writ control plane";
      wantedBy = [ "multi-user.target" ];
      # git = "git" in the config's tofu block finds this git, which each
      # tofu process's filesystem view holds for git module sources.
      path = [ pkgs.git ];
      wants = [ "network-online.target" ];
      after = [ "network-online.target" ]
        ++ lib.optional cfg.createDatabase "postgresql.service"
        ++ lib.optional isolated firewallUnit;
      requires = lib.optional cfg.createDatabase "postgresql.service"
        ++ lib.optional isolated firewallUnit;
      serviceConfig = {
        User = "writ";
        Group = "writ";
        StateDirectory = "writ";
        StateDirectoryMode = "0700";
        WorkingDirectory = "/var/lib/writ";
        ExecStartPre = "${writ} serve --config ${cfg.configFile} --check";
        ExecStart = "${writ} serve --config ${cfg.configFile}";
        # SIGTERM stops new requests, waits 30 seconds for running ones,
        # and fails the plans still running.
        KillSignal = "SIGTERM";
        TimeoutStopSec = "60s";
        Restart = "on-failure";
        RestartSec = "5s";
        NoNewPrivileges = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        PrivateTmp = true;
        PrivateDevices = true;
        # Each tofu process's filesystem view mounts a /proc of its own
        # pid namespace. Before Linux 7.2 the kernel refuses that mount in
        # a user namespace while a read-only bind covers part of writ's
        # /proc, as this option makes.
        ProtectKernelTunables = lib.versionAtLeast config.boot.kernelPackages.kernel.version "7.2";
        ProtectKernelModules = true;
        # With provider uids, writ serve gives each OpenTofu step a cgroup
        # of its own in runs, under the unit's, which it moves itself out
        # of into the leaf serve, so the step's limits.run_memory,
        # run_pids, and run_cpus hold, and runs leaves writ serve 128
        # processes and threads and 512MiB of the unit's limits. That needs the unit's cgroup delegated to the
        # writ user and /sys/fs/cgroup writable, which
        # ProtectControlGroups would make read-only. Without provider
        # uids, no OpenTofu runs.
        Delegate = isolated;
        ProtectControlGroups = !isolated;
        RestrictAddressFamilies = [ "AF_UNIX" "AF_INET" "AF_INET6" ];
        # The user, mount, pid, and IPC namespaces of each tofu process's
        # private filesystem view.
        RestrictNamespaces = "user mnt pid ipc";
        LockPersonality = true;
        ReadWritePaths = [ "/var/lib/writ" ] ++ lib.optionals isolated [ "/var/lib/writ-tofu" ];
        UMask = "0077";
        # writ serve clears its inheritable and ambient capabilities at
        # start, so its providers get none of these.
        AmbientCapabilities = capabilities;
        CapabilityBoundingSet = capabilities;
      };
    };
  };
}
