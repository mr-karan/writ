{
  description = "Writ: a typed infrastructure-as-code language and its control plane";

  # One pinned nixpkgs, locked in flake.lock, builds the package, the
  # image, and the NixOS module check. Its Go is the version in go.mod, and
  # its OpenTofu is 1.13.9, the one version Writ runs.
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/d4f25a3e562008648d45bcf2d70bcc7422cad84e";

  outputs = { self, nixpkgs }:
    let
      lib = nixpkgs.lib;
      linux = [ "x86_64-linux" "aarch64-linux" ];
      # The pinned nixpkgs (26.11) dropped x86_64-darwin. Intel Macs take
      # the release binary, which release/build.sh still builds.
      systems = linux ++ [ "aarch64-darwin" ];
      forAll = names: f: lib.genAttrs names (system: f nixpkgs.legacyPackages.${system});
      # A build of a commit says its short hash; a build of a dirty tree
      # says so too.
      version = self.shortRev or self.dirtyShortRev or "dev";
      writFor = pkgs: args: pkgs.callPackage ./deploy/nix/package.nix ({ inherit version; } // args);
    in
    {
      packages = lib.foldl' lib.recursiveUpdate { } [
        (forAll systems (pkgs: rec {
          writ = writFor pkgs { };
          default = writ;
        }))
        (forAll linux (pkgs: {
          image = pkgs.callPackage ./deploy/nix/image.nix { writ = writFor pkgs { withGit = false; }; };
        }))
        # Test-only, for just e2e-aws-emulator; the vendor ships no other
        # system.
        (forAll [ "x86_64-linux" ] (pkgs: {
          aws-emulator = pkgs.callPackage ./deploy/nix/aws-emulator.nix { };
        }))
      ];

      # The NixOS module of deploy/nixos, with this flake's writ as its
      # default package.
      nixosModules = rec {
        writ-serve = { pkgs, lib, ... }: {
          imports = [ ./deploy/nixos/writ-serve.nix ];
          services.writ-serve.package = lib.mkDefault self.packages.${pkgs.stdenv.hostPlatform.system}.writ;
        };
        default = writ-serve;
      };

      # Leaves out aws-emulator, a 187 MB download that only its e2e recipe
      # needs.
      checks = forAll systems (pkgs: removeAttrs self.packages.${pkgs.stdenv.hostPlatform.system} [ "aws-emulator" ]);

      devShells = forAll systems (pkgs: {
        default = pkgs.mkShell {
          packages = [ pkgs.go pkgs.just pkgs.minisign pkgs.opentofu ];
        };
      });

      formatter = forAll systems (pkgs: pkgs.nixfmt);
    };
}
