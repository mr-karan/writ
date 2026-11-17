# The writ binary, built from this repository with the Go of the pinned
# nixpkgs. It is static (CGO_ENABLED=0) and carries its version, the same
# build flags as release/build.sh. The flake calls this file; so can any
# nixpkgs with callPackage.
#
# writ serve --dev reads a local repository, and writ plan --remote looks
# at the work tree, with the git binary. A compiled wrapper, so no shell
# joins the closure, appends git to PATH: a git the caller has comes
# first, and the static binary itself is unchanged.
# withGit = false gives the bare static binary, for the container image,
# which runs only a team's writ serve and so never runs git.
{ lib, buildGoModule, makeBinaryWrapper, gitMinimal, version ? "dev", withGit ? true }:

buildGoModule {
  pname = "writ";
  inherit version;

  # Only what the Go build reads, so an edit to docs or the website does
  # not rebuild writ.
  src = lib.fileset.toSource {
    root = ../..;
    fileset = lib.fileset.unions [
      ../../go.mod
      ../../go.sum
      ../../cmd
      ../../internal
    ];
  };

  vendorHash = "sha256-XcDYVSThHNia3MhplwtjsIxL7udhQDii/kj5mj5Jn6U=";
  # The modules depend on go.sum alone, so their name leaves out the
  # version and a new commit reuses them instead of downloading again.
  overrideModAttrs = _: { name = "writ-go-modules"; };

  subPackages = [ "cmd/writ" ];
  env.CGO_ENABLED = 0;
  ldflags = [ "-s" "-w" "-X main.version=${version}" ];

  nativeBuildInputs = lib.optionals withGit [ makeBinaryWrapper ];
  postInstall = lib.optionalString withGit ''
    wrapProgram $out/bin/writ --suffix PATH : ${lib.makeBinPath [ gitMinimal ]}
  '';

  # just check runs the whole test suite with -race. The package build
  # compiles and runs one smoke check.
  doCheck = false;
  doInstallCheck = true;
  installCheckPhase = ''
    runHook preInstallCheck
    $out/bin/writ version | grep -qxF 'writ ${version}'
    runHook postInstallCheck
  '';

  meta = {
    description = "An open-source control plane for OpenTofu that you host yourself";
    license = lib.licenses.asl20;
    homepage = "https://github.com/mr-karan/writ";
    mainProgram = "writ";
    platforms = lib.platforms.linux ++ lib.platforms.darwin;
  };
}
