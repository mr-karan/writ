# The container image of writ serve, built by Nix's dockerTools, so no
# Docker daemon takes part. It holds writ, OpenTofu at /bin/tofu, CA
# certificates, time zones, and a passwd file with an unprivileged writ
# user, and nothing else: no shell. OpenTofu is nixpkgs' opentofu, 1.13.9
# in the flake's pin, the one version writ serve runs. It runs as uid
# 65532 with /var/lib/writ as home and work directory. The default
# command is a team's writ serve with its config at /etc/writ/serve,
# which the operator mounts.
#
# writ is any package whose bin/writ is the binary. The flake passes its
# own build. release/build.sh passes each release binary, so the image
# holds the same bytes as the download, and sets architecture to build an
# arm64 image on an amd64 machine, with opentofu of that architecture.
{ lib, dockerTools, fakeNss, cacert, tzdata, opentofu, writ, architecture ? null, tag ? writ.version or "dev" }:

let
  uid = 65532;
  home = "/var/lib/writ";
  nss = fakeNss.override {
    extraPasswdLines = [ "writ:x:${toString uid}:${toString uid}:writ:${home}:/noshell" ];
    extraGroupLines = [ "writ:x:${toString uid}:" ];
  };
in
dockerTools.buildLayeredImage ({
  # The name it will have when published, so a loaded archive runs with
  # the same commands as a pulled image.
  name = "ghcr.io/mr-karan/writ";
  inherit tag;
  contents = [ writ opentofu cacert tzdata nss ];
  # The image's own directories, owned by the writ user: its home with the
  # dev data, the provider cache, and /tmp for providers.
  fakeRootCommands = ''
    mkdir -p .${home} tmp etc/writ
    chown ${toString uid}:${toString uid} .${home}
    chmod 0700 .${home}
    chmod 1777 tmp
  '';
  enableFakechroot = false;
  config = {
    User = "${toString uid}:${toString uid}";
    WorkingDir = home;
    Entrypoint = [ "${lib.getExe' writ "writ"}" ];
    Cmd = [ "serve" "--config" "/etc/writ/serve" ];
    Env = [
      "HOME=${home}"
      "PATH=/bin"
      "USER=writ"
      "SSL_CERT_FILE=${cacert}/etc/ssl/certs/ca-bundle.crt"
      "TZDIR=${tzdata}/share/zoneinfo"
      "WRIT_CACHE_DIR=${home}/cache"
    ];
    Volumes = { ${home} = { }; };
    Labels = {
      "org.opencontainers.image.title" = "writ";
      "org.opencontainers.image.source" = "https://github.com/mr-karan/writ";
      "org.opencontainers.image.version" = tag;
    };
  };
} // lib.optionalAttrs (architecture != null) { inherit architecture; })
