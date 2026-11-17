#!/usr/bin/env bash
# Keeps vendorHash in deploy/nix/package.nix in step with the Go modules.
#
# Nix keys the module download by vendorHash alone. When go.mod or go.sum
# changes and vendorHash does not, Nix reuses the old download, and the
# build fails with "inconsistent vendoring" or a hash mismatch. So
# deploy/nix/go-sum.sha256 records the SHA-256 of the inputs that
# vendorHash was computed from: go.mod, go.sum, and flake.lock, whose
# nixpkgs pins the Go that vendors the modules. Only `update` writes it.
#
#   stamp   compare the inputs with go-sum.sha256, without Nix
#   check   build .#writ; on stale inputs, compute the right vendorHash
#   update  compute vendorHash, write it and go-sum.sha256, build .#writ
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
cd "$root"
pkg=deploy/nix/package.nix
stamp=deploy/nix/go-sum.sha256
inputs=(go.mod go.sum flake.lock)

stamp_ok() {
	sha256sum --check --quiet --strict "$stamp" >/dev/null 2>&1
}

# A flake URL needs the path percent-encoded, so a checkout under a
# directory with a space or other reserved character still resolves.
url_path() {
	local LC_ALL=C s="$1" out="" c i
	for ((i = 0; i < ${#s}; i++)); do
		c="${s:i:1}"
		case "$c" in
		[A-Za-z0-9/._~-]) out+="$c" ;;
		*) printf -v c '%%%02X' "'$c"; out+="$c" ;;
		esac
	done
	printf '%s' "$out"
}

current_hash() {
	sed -n 's/^  vendorHash = "\(sha256-[A-Za-z0-9+/=]*\)";$/\1/p' "$pkg"
}

# Builds the modules with an empty vendorHash, which always downloads them,
# and prints the hash Nix reports. The flake reads the work tree, so
# uncommitted edits to tracked files count.
compute_hash() {
	local log
	log="$(mktemp)"
	# The traps outlive this function's locals, so they hold the quoted path.
	local quoted
	quoted="$(printf '%q' "$log")"
	# shellcheck disable=SC2064
	trap "rm -f -- $quoted" EXIT
	# shellcheck disable=SC2064
	trap "rm -f -- $quoted; exit 130" INT TERM
	if nix build --impure --no-link --expr "
		((builtins.getFlake \"git+file://$(url_path "$root")\").packages.\${builtins.currentSystem}.writ.overrideAttrs {
			vendorHash = \"\";
		}).goModules" 2>"$log"; then
		echo "vendor-hash: the module build passed with an empty vendorHash" >&2
		rm -f "$log"
		return 1
	fi
	local got
	got="$(sed -n 's/^ *got: *\(sha256-[A-Za-z0-9+/=]*\)$/\1/p' "$log" | tail -n 1)"
	if [[ -z "$got" ]]; then
		cat "$log" >&2
		echo "vendor-hash: the module build failed for a reason other than its hash" >&2
		rm -f "$log"
		return 1
	fi
	rm -f "$log"
	echo "$got"
}

case "${1:-}" in
stamp)
	stamp_ok && exit 0
	sha256sum --check --strict "$stamp" 2>&1 | grep -v ': OK$' >&2 || true
	cat >&2 <<-EOF
		$stamp does not match ${inputs[*]}.
		vendorHash in $pkg was computed for other module inputs, and
		nix build .#writ fails until it is updated. Run: just update-nix-hash
	EOF
	exit 1
	;;
check)
	if stamp_ok; then
		nix build --no-link .#writ
		echo "nix package builds; vendorHash matches go.mod and go.sum"
		exit 0
	fi
	want="$(compute_hash)"
	have="$(current_hash)"
	if [[ "$want" == "$have" ]]; then
		echo "vendorHash $have is right, but $stamp is stale. Run: just update-nix-hash" >&2
		exit 1
	fi
	line="$(grep -n '^  vendorHash = ' "$pkg" | cut -d: -f1)"
	cat >&2 <<-EOF
		vendorHash in $pkg is stale for ${inputs[*]}.
		  have: $have
		  want: $want
		Change $pkg line $line to:
		  vendorHash = "$want";
		or run: just update-nix-hash
	EOF
	exit 1
	;;
update)
	want="$(compute_hash)"
	have="$(current_hash)"
	[[ -n "$have" ]] || { echo "vendor-hash: no vendorHash line in $pkg" >&2; exit 1; }
	if [[ "$want" != "$have" ]]; then
		sed -i "s|^  vendorHash = \"$have\";$|  vendorHash = \"$want\";|" "$pkg"
		echo "vendorHash: $have -> $want"
	fi
	sha256sum "${inputs[@]}" >"$stamp"
	nix build --no-link .#writ
	echo "wrote $pkg and $stamp; nix build .#writ passes"
	;;
*)
	echo "usage: deploy/nix/vendor-hash.sh stamp|check|update" >&2
	exit 2
	;;
esac
