#!/usr/bin/env bash
# Build every release artifact of writ into one directory, and publish
# nothing: a static binary for linux and darwin on amd64 and arm64, a
# writ serve image for linux on each, install.sh stamped with the version
# and the public key, SHA256SUMS over all of them, and its minisign
# signature. release/README.md explains each step and the real release.
#
#   release/build.sh [--version V] [--out DIR] [--sign-key FILE] [--public-key FILE]
#
# --sign-key (env WRIT_MINISIGN_KEY) is the release's minisign secret key,
# and --public-key its public key, default release/minisign.pub. The key's
# password, if any, is read from stdin by the signing step alone. Without
# --sign-key it signs with a throwaway key, for a dry run. Needs the Go of
# go.mod, minisign, sha256sum, and nix; `nix develop` has them.
set -euo pipefail

# Keep stdin for the password, and give every other command none.
exec 3<&0 </dev/null

root="$(git rev-parse --show-toplevel)"
version=""
out="$root/dist"
sign_key="${WRIT_MINISIGN_KEY:-}"
public_key="$root/release/minisign.pub"

while [[ $# -gt 0 ]]; do
	case "$1" in
	--version) version="$2"; shift 2 ;;
	--out) out="$2"; shift 2 ;;
	--sign-key) sign_key="$2"; shift 2 ;;
	--public-key) public_key="$2"; shift 2 ;;
	*) echo "build.sh: unknown argument $1" >&2; exit 2 ;;
	esac
done
[[ -n "$version" ]] || version="$(git -C "$root" describe --tags --always --dirty)"
[[ "$version" =~ ^[A-Za-z0-9._-]+$ ]] || { echo "build.sh: version $version has characters outside [A-Za-z0-9._-]" >&2; exit 2; }

# Reproducible means the same Go: the release uses exactly go.mod's version
# and never downloads another toolchain.
want_go="go$(awk '$1 == "go" { print $2 }' "$root/go.mod")"
have_go="$(GOTOOLCHAIN=local go env GOVERSION)"
[[ "$have_go" == "$want_go" ]] || { echo "build.sh: go.mod needs $want_go, found $have_go; run it in nix develop" >&2; exit 1; }

# Replace only an earlier build, so a wrong --out deletes nothing else.
if [[ -e "$out" && -n "$(ls -A "$out")" && ! -f "$out/SHA256SUMS" ]]; then
	echo "build.sh: $out holds files and no SHA256SUMS; pass an empty or new --out" >&2
	exit 1
fi
rm -rf "$out"
mkdir -p "$out"
out="$(cd "$out" && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# The flags make the bytes depend only on the source, the Go version, and
# the version string: no paths, no cgo, no settings from the environment.
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
	os="${target%/*}" arch="${target#*/}"
	(cd "$root" && env -u GOFLAGS CGO_ENABLED=0 GOTOOLCHAIN=local GOOS="$os" GOARCH="$arch" GOAMD64=v1 GOARM64=v8.0 \
		go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$out/writ-$os-$arch" ./cmd/writ)
done

# Each image holds the release binary itself, so its bytes match the
# download, and an arm64 image builds on an amd64 machine. Its OpenTofu is
# the pinned nixpkgs' build for the image's architecture, which Nix
# fetches from the binary cache without running it.
tag="${version#v}"
for arch in amd64 arm64; do
	system=x86_64-linux
	[[ "$arch" == arm64 ]] && system=aarch64-linux
	nix build --impure --out-link "$work/image-$arch" --expr "
		let
		  flake = builtins.getFlake \"git+file://$root\";
		  pkgs = flake.inputs.nixpkgs.legacyPackages.\${builtins.currentSystem};
		  bin = builtins.path { path = /. + \"$out/writ-linux-$arch\"; name = \"writ-linux-$arch\"; };
		in pkgs.callPackage \"\${flake}/deploy/nix/image.nix\" {
		  architecture = \"$arch\";
		  tag = \"$tag\";
		  opentofu = flake.inputs.nixpkgs.legacyPackages.$system.opentofu;
		  writ = pkgs.runCommand \"writ-$tag\" { } \"install -Dm755 \${bin} \$out/bin/writ\";
		}"
	install -m 0644 "$work/image-$arch" "$out/writ-image-linux-$arch.tar.gz"
done

if [[ -n "$sign_key" ]]; then
	[[ -f "$public_key" ]] || { echo "build.sh: --sign-key needs its public key at $public_key" >&2; exit 1; }
	install -m 0644 "$public_key" "$out/minisign.pub"
	note="signed with $sign_key"
else
	minisign -G -W -s "$work/minisign.key" -p "$out/minisign.pub" >/dev/null
	sign_key="$work/minisign.key"
	note="signed with a throwaway key: a dry run, not for publishing"
fi
pubkey="$(tail -n 1 "$out/minisign.pub")"

sed -e "s|@WRIT_VERSION@|$version|" -e "s|@WRIT_MINISIGN_PUBKEY@|$pubkey|" "$root/release/install.sh" >"$out/install.sh"
chmod 0644 "$out/install.sh"

(cd "$out" && sha256sum writ-* install.sh minisign.pub >SHA256SUMS)
minisign -S -s "$sign_key" -m "$out/SHA256SUMS" -t "writ $version SHA256SUMS" <&3 >/dev/null
(cd "$out" && sha256sum --quiet -c SHA256SUMS)
# Checking with the published public key proves the secret key matches it.
minisign -V -q -p "$out/minisign.pub" -m "$out/SHA256SUMS"

echo "writ $version in $out, $note"
for f in "$out"/*; do
	wc -c <"$f" | awk -v f="${f##*/}" '{ printf($1 >= 1048576 ? "  %7.1f MiB  %s\n" : "  %7d B    %s\n", $1 >= 1048576 ? $1 / 1048576 : $1, f) }'
done
[[ -f "$root/LICENSE" ]] || echo "warning: the repository has no LICENSE file; a public release needs one (release/README.md)" >&2
