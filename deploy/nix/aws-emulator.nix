# AWS Emulator, a local stand-in of the AWS APIs, for `just
# e2e-aws-emulator` only: that recipe's nix shell is the one place it is on
# PATH. The vendor's prebuilt package, because a build from source needs
# about 100 GB. The download is 187 MB and the binary 1 GB, so the flake
# keeps it out of its checks, its dev shell, and writ's closure.
#
# Pinned by version: the vendor releases daily, and a new release can change
# the replies the test depends on. Bump the version and hash on purpose and
# rerun the test.
{ lib, stdenv, fetchurl, dpkg, autoPatchelfHook }:

stdenv.mkDerivation (finalAttrs: {
  pname = "aws-emulator";
  version = "1.0.385";

  # versions.json names this release 1.0.385.1, but only the -1 file name
  # is served.
  src = fetchurl {
    url = "https://downloads.aws.com/aws-emulator/packages/aws-emulator/LTS/aws-emulator_${finalAttrs.version}-1_amd64.deb";
    hash = "sha256-U0SznnYIljPzzC2HleVVT/Con0wmvDFvp0izkJQj0/U=";
  };

  nativeBuildInputs = [ dpkg autoPatchelfHook ];

  unpackPhase = ''
    runHook preUnpack
    dpkg-deb --extract "$src" .
    runHook postUnpack
  '';

  # The binary links only glibc, which autoPatchelfHook finds in stdenv.
  # Stripping 1 GB gains little and changes the vendor's bytes.
  dontStrip = true;

  installPhase = ''
    runHook preInstall
    install -Dm755 opt/aws-emulator/bin/aws-emulator "$out/bin/aws-emulator"
    install -Dm644 opt/aws-emulator/THIRD_PARTY_LICENSES "$out/share/doc/aws-emulator/THIRD_PARTY_LICENSES"
    runHook postInstall
  '';

  meta = {
    description = "Local emulator of the AWS APIs, for Writ's end-to-end import test";
    # The emulator's repository is CC0 and the vendor's code Apache-2.0.
    # The package itself declares no license; the notices of the crates it
    # bundles are installed beside it.
    license = with lib.licenses; [ cc0 asl20 ];
    sourceProvenance = [ lib.sourceTypes.binaryNativeCode ];
    platforms = [ "x86_64-linux" ];
    mainProgram = "aws-emulator";
  };
})
