#!/usr/bin/env bash
# Load a writ image archive into a scratch rootless podman store, run
# `writ serve --dev` in it, and require a healthy /healthz and the
# expected version. The image is for a team's writ serve and holds no
# git, so writ repo add of a local Git repository mounted into the
# container must fail with the message that names the missing git, and
# /healthz must still answer. Leaves no image or container behind.
#
#   release/test-image.sh IMAGE.tar.gz VERSION
#
# Run it with podman, curl, and git on PATH, as just release-test-image
# does.
# The dev control plane listens on loopback alone, so the container shares
# the host's network.
set -euo pipefail

archive="$1"
version="$2"
scratch="$(mktemp -d)"
port="$((20000 + RANDOM % 20000))"
mkdir -p "$scratch/config/containers"
# The archive is local and checked by SHA256SUMS, so the store needs no
# signature policy beyond accepting it.
echo '{"default":[{"type":"insecureAcceptAnything"}]}' >"$scratch/config/containers/policy.json"
pod() { XDG_CONFIG_HOME="$scratch/config" podman --root "$scratch/root" --runroot "$scratch/run" --storage-driver vfs "$@"; }
cleanup() {
	pod rm -f -t 0 writ-image-test >/dev/null 2>&1 || true
	pod unshare rm -rf "$scratch/root" "$scratch/run" 2>/dev/null || true
	rm -rf "$scratch"
}
trap cleanup EXIT

image="$(pod load -q -i "$archive" | sed -n 's/^Loaded image: //p')"
[[ -n "$image" ]] || { echo "test-image: podman loaded no image from $archive" >&2; exit 1; }
got="$(pod run --rm "$image" version)"
[[ "$got" == "writ $version" ]] || { echo "test-image: $image says '$got', want 'writ $version'" >&2; exit 1; }

# A real repository with one stack, made on the host, so the failure can
# only come from the missing git, not from a directory that is no work
# tree.
repo="$scratch/demo"
mkdir -p "$repo/hello"
echo '{}' >"$repo/writ.estate"
echo 'out greeting = "hello"' >"$repo/hello/main.writ"
git -C "$repo" init -q -b main
git -C "$repo" add .
git -C "$repo" -c user.name=writ -c user.email=writ@localhost commit -q -m demo
chmod -R a+rX "$repo"

pod run -d --name writ-image-test --network host -v "$repo:/demo:ro" "$image" serve --dev --listen "127.0.0.1:$port" >/dev/null
for _ in $(seq 50); do
	if health="$(curl -fsS "http://127.0.0.1:$port/healthz" 2>/dev/null)"; then
		break
	fi
	sleep 0.1
done
pod logs writ-image-test
[[ "${health:-}" == *'"ok":true'* ]] || { echo "test-image: /healthz did not answer ok: ${health:-no answer}" >&2; exit 1; }
echo "test-image: $image runs writ serve --dev; /healthz: $health"

if add="$(pod exec writ-image-test writ repo add /demo 2>&1)"; then
	echo "test-image: writ repo add succeeded, but the image should hold no git: $add" >&2
	exit 1
fi
echo "$add"
[[ "$add" == *'git is not installed'* ]] || { echo "test-image: writ repo add did not say git is not installed: $add" >&2; exit 1; }
health="$(curl -fsS "http://127.0.0.1:$port/healthz" 2>/dev/null)" || true
[[ "${health:-}" == *'"ok":true'* ]] || { echo "test-image: /healthz did not answer ok after writ repo add: ${health:-no answer}" >&2; exit 1; }
echo "test-image: $image holds no git, names it when writ repo add needs it, and still serves /healthz"
