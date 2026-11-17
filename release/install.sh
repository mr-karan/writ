#!/bin/sh
# Install writ, the Writ CLI and control plane, on Linux or macOS.
#
#   curl -fsSL https://github.com/mr-karan/writ/releases/download/VERSION/install.sh | sh
#
# It downloads the binary for this machine, SHA256SUMS, and the minisign
# signature of SHA256SUMS from one release. It checks the signature with the
# release key, checks the binary against SHA256SUMS, runs the binary, and
# only then puts it in place. It needs minisign, because a SHA-256 match
# shows that a download is whole, not who made it. release/build.sh stamps
# the release's version and public key into the copy it ships. Run
# `sh install.sh --help` for the options.
set -eu
# A relative --dir must not resolve through CDPATH, where cd also prints.
unset CDPATH

# build.sh stamps the release's version and key into these defaults. An
# unstamped copy, straight from the repository, still holds the
# placeholders; the comparisons split them so that the stamp skips them.
# Only the defaults are compared, so a placeholder that a user passes is
# refused below like any other value with an @.
default_version="@WRIT_VERSION@"
default_pubkey="@WRIT_MINISIGN_PUBKEY@"
[ "$default_version" != "@WRIT_""VERSION@" ] || default_version=""
[ "$default_pubkey" != "@WRIT_""MINISIGN_PUBKEY@" ] || default_pubkey=""
version="${WRIT_VERSION:-$default_version}"
pubkey="${WRIT_MINISIGN_PUBKEY:-$default_pubkey}"
base="${WRIT_BASE_URL:-}"
dir="${WRIT_INSTALL_DIR:-}"
from=""
skip_signature=0
tmp=""
stage=""

usage() {
	cat <<EOF
usage: install.sh [--version V] [--dir DIR] [--from DIR] [--insecure-skip-signature]

  --version V                the release to install, such as v0.5.0 (env WRIT_VERSION)
  --dir DIR                  where to put writ (env WRIT_INSTALL_DIR; default ~/.local/bin)
  --from DIR                 take the files from a local directory, not the network
  --insecure-skip-signature  do not check the signature of SHA256SUMS; then nothing
                             shows that the Writ release key signed what you install

It needs minisign to check the signature: https://jedisct1.github.io/minisign/
EOF
}

die() {
	printf 'install.sh: %s\n' "$*" >&2
	exit 1
}

cleanup() {
	[ -z "$tmp" ] || rm -rf "$tmp"
	[ -z "$stage" ] || rm -f "$stage"
}
trap cleanup EXIT
# Without an exit here, sh runs the trap on a signal and then carries on.
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

while [ $# -gt 0 ]; do
	case "$1" in
	--version) [ $# -ge 2 ] || die "--version needs a value"; version="$2"; shift 2 ;;
	--dir) [ $# -ge 2 ] || die "--dir needs a value"; dir="$2"; shift 2 ;;
	--from) [ $# -ge 2 ] || die "--from needs a value"; from="$2"; shift 2 ;;
	--insecure-skip-signature) skip_signature=1; shift ;;
	-h | --help) usage; exit 0 ;;
	*) usage >&2; die "unknown argument $1" ;;
	esac
done

case "$version" in *[!A-Za-z0-9._-]*) die "version $version has characters outside [A-Za-z0-9._-]" ;; esac
case "$pubkey" in *[!A-Za-z0-9+/=]*) die "the release key $pubkey is not a minisign public key" ;; esac
if [ -z "$dir" ]; then
	[ -n "${HOME:-}" ] || die "no install directory: pass --dir or set HOME"
	dir="$HOME/.local/bin"
fi

case "$(uname -s)" in
Linux) os=linux ;;
Darwin) os=darwin ;;
*) die "no writ build for $(uname -s); writ runs on Linux and macOS" ;;
esac
case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) die "no writ build for $(uname -m); writ runs on amd64 and arm64" ;;
esac
# A shell under Rosetta reports x86_64 on an Apple silicon Mac.
if [ "$os" = darwin ] && [ "$arch" = amd64 ] && [ "$(sysctl -n hw.optional.arm64 2>/dev/null || true)" = 1 ]; then
	arch=arm64
fi
asset="writ-$os-$arch"

if [ -z "$from" ] && [ -z "$base" ]; then
	[ -n "$version" ] || die "no version: pass --version, such as --version v0.5.0"
	base="https://github.com/mr-karan/writ/releases/download/$version"
fi
case "$base" in "" | https://*) ;; *) die "WRIT_BASE_URL must start with https://" ;; esac

if command -v sha256sum >/dev/null 2>&1; then
	sha256() { sha256sum "$1"; }
elif command -v shasum >/dev/null 2>&1; then
	sha256() { shasum -a 256 "$1"; }
else
	die "need sha256sum or shasum to check the download"
fi

# Fail before any download when the signature cannot be checked.
if [ "$skip_signature" = 1 ]; then
	{
		echo "install.sh: WARNING: --insecure-skip-signature: the signature of SHA256SUMS is not checked."
		echo "install.sh: WARNING: The SHA-256 check then shows only that the binary matches SHA256SUMS,"
		echo "install.sh: WARNING: not that the Writ release key signed it. Whoever can change the release"
		echo "install.sh: WARNING: files or the connection can give you a different writ."
	} >&2
else
	[ -n "$pubkey" ] || die "no release key to check the signature of SHA256SUMS with; use the install.sh of a release, or set WRIT_MINISIGN_PUBKEY to the key in docs/install.md; nothing installed"
	command -v minisign >/dev/null 2>&1 || die "minisign is not installed, so the signature of SHA256SUMS cannot be checked; nothing installed. Install minisign and run this again: brew install minisign (macOS), apt install minisign (Debian, Ubuntu), dnf install minisign (Fedora), nix profile install nixpkgs#minisign, or see https://jedisct1.github.io/minisign/"
fi

tmp="$(mktemp -d)" || die "cannot make a temporary directory"

# renames_onto_symlink OPT succeeds when `mv OPT` replaces a symlink to a
# directory with the file, rather than moving the file into the directory.
renames_onto_symlink() {
	probe="$tmp/mv$1"
	mkdir "$probe" "$probe/d" && : >"$probe/f" && ln -s d "$probe/l" || return 1
	mv -f "$1" "$probe/f" "$probe/l" 2>/dev/null &&
		[ ! -L "$probe/l" ] && [ -f "$probe/l" ] && [ ! -e "$probe/d/f" ]
}
# A plain mv follows a symlink that appears at the target after the last
# check, so the rename itself must not follow one: GNU mv -T, BSD mv -h.
if renames_onto_symlink -T; then
	mv_onto=-T
elif renames_onto_symlink -h; then
	mv_onto=-h
else
	die "this mv cannot rename onto a symlink without following it (it has neither GNU mv -T nor BSD mv -h), so writ cannot be put in place safely; nothing installed"
fi

# fetch NAME copies one file of the release into $tmp.
fetch() {
	if [ -n "$from" ]; then
		[ -f "$from/$1" ] || return 1
		cp "$from/$1" "$tmp/$1"
	elif command -v curl >/dev/null 2>&1; then
		curl -fsSL --proto '=https' --tlsv1.2 --retry 3 -o "$tmp/$1" "$base/$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -q --https-only -O "$tmp/$1" "$base/$1"
	else
		die "need curl or wget to download writ"
	fi
}

fetch SHA256SUMS || die "cannot get SHA256SUMS from ${from:-$base}"
if [ "$skip_signature" = 1 ]; then
	signed="signature NOT checked: --insecure-skip-signature"
else
	fetch SHA256SUMS.minisig || die "cannot get SHA256SUMS.minisig from ${from:-$base}; nothing installed"
	minisign -V -q -P "$pubkey" -m "$tmp/SHA256SUMS" -x "$tmp/SHA256SUMS.minisig" >/dev/null 2>&1 ||
		die "the signature of SHA256SUMS does not match the release key; nothing installed"
	signed="signature checked"
fi

want="$(awk -v f="$asset" '$2 == f || $2 == "*" f { print $1 }' "$tmp/SHA256SUMS")"
case "$want" in
"") die "SHA256SUMS has no line for $asset" ;;
*[!0-9a-f]*) die "SHA256SUMS has a malformed line for $asset" ;;
esac
[ "${#want}" -eq 64 ] || die "SHA256SUMS has a malformed line for $asset"

# checksum FILE prints the SHA-256 of FILE, or nothing when it cannot.
checksum() {
	sum="$(sha256 "$1")" || return 1
	printf '%s\n' "${sum%% *}"
}

fetch "$asset" || die "cannot get $asset from ${from:-$base}"
got="$(checksum "$tmp/$asset")" || die "cannot compute the SHA-256 of $asset"
[ "$got" = "$want" ] || die "$asset has SHA-256 $got, but SHA256SUMS says $want; nothing installed"

mkdir -p "$dir" || die "cannot make $dir"
dir="$(cd "$dir" && pwd)" || die "cannot enter $dir"
target="$dir/writ"
# mv would follow a symlink or move into a directory, so refuse both.
if [ -L "$target" ]; then
	die "$target is a symlink; remove it or pass another --dir; nothing installed"
elif [ -e "$target" ] && [ ! -f "$target" ]; then
	die "$target exists and is not a regular file; remove it or pass another --dir; nothing installed"
fi

# Write next to the target, then rename, so a running writ is never half
# written. mktemp creates a new file with a random name and fails if the
# name exists, so the write never goes through a planted symlink.
stage="$(mktemp "$dir/.writ.XXXXXXXXXX")" || die "cannot make a file in $dir"
cat "$tmp/$asset" >"$stage" || die "cannot write $stage"
chmod 0755 "$stage" || die "cannot make $stage executable"
[ "$(checksum "$stage")" = "$want" ] || die "$stage does not match SHA256SUMS after the copy; nothing installed"
reported="$("$stage" version)" || die "the downloaded writ does not run on this machine; nothing installed"
if [ -n "$version" ] && [ "$reported" != "writ $version" ]; then
	die "the downloaded writ reports \"$reported\", not writ $version; nothing installed"
fi

# Refuse a symlink someone made on purpose while the download ran. A
# symlink that appears after this check is replaced by the rename, which
# is atomic within $dir and never follows it.
if [ -L "$target" ] || { [ -e "$target" ] && [ ! -f "$target" ]; }; then
	die "$target changed into something other than a regular file; nothing installed"
fi
mv -f "$mv_onto" "$stage" "$target" || die "cannot move writ into place at $target; nothing installed"
if [ -L "$target" ] || [ ! -f "$target" ]; then
	# BSD mv -h still moves into a real directory, so clean up there.
	stage="$target/${stage##*/}"
	die "$target is not the file just installed"
fi
stage=""
installed="$("$target" version)" || die "$target does not run after the install"
[ "$installed" = "$reported" ] || die "$target reports \"$installed\", not \"$reported\""

printf 'installed %s to %s (SHA-256 checked, %s)\n' "$installed" "$target" "$signed"
# shellcheck disable=SC2016 # The user pastes the literal $PATH.
case ":${PATH:-}:" in
*":$dir:"*) ;;
*) printf '%s is not on your PATH; add it: export PATH="%s:$PATH"\n' "$dir" "$dir" ;;
esac
