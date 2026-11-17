# Releasing writ

This directory builds a release of `writ` and publishes nothing. `.github/workflows/release.yml` publishes a release when a tag `v*` is pushed. [Install](../docs/install.md) is the user's side.

## Still to do

- **The image registry.** The release workflow pushes `ghcr.io/mr-karan/writ` with the workflow's `GITHUB_TOKEN`, which creates the package on the first push. A new package is private: make it public in its settings after the first release.
- **A test on a Mac.** The darwin binaries are cross-built on Linux and have never run on a Mac. The arm64 binary carries the ad hoc signature that Go's linker adds, which Apple silicon requires, and the amd64 binary has none. They are not notarized, so a browser download gets the quarantine flag and macOS blocks it. A download by `curl` or `install.sh` gets no flag.

The release key is in [`minisign.pub`](minisign.pub), key id `D2D9A37AF1098999`, and in [Install](../docs/install.md#check-a-download-by-hand). Its secret key and password are the repository's Actions secrets `WRIT_MINISIGN_KEY` and `WRIT_MINISIGN_PASSWORD`. The repository is under the Apache 2.0 license, in `LICENSE`; `release/build.sh` warns when that file is missing.

## What a build makes

`release/build.sh` writes these files into one directory, `dist/` by default. Measured 2026-11-10, a build stamped `v0.1.0-rc0`:

| File | Size | What it is |
| --- | --- | --- |
| `writ-linux-amd64` | 36.6 MiB | Static binary |
| `writ-linux-arm64` | 33.4 MiB | Static binary |
| `writ-darwin-amd64` | 37.3 MiB | Static binary |
| `writ-darwin-arm64` | 34.2 MiB | Static binary |
| `writ-image-linux-amd64.tar.gz` | 13.7 MiB | `writ serve` image, a Docker archive |
| `writ-image-linux-arm64.tar.gz` | 12.4 MiB | `writ serve` image, a Docker archive |
| `install.sh` | 8 KiB | The install script, with the version and public key stamped in |
| `minisign.pub` | 113 B | The public key that signed `SHA256SUMS` |
| `SHA256SUMS` | 682 B | SHA-256 of every file above |
| `SHA256SUMS.minisig` | 288 B | minisign signature of `SHA256SUMS` |

The image sizes are from a later dry run, on 2026-11-10. The images are for a team's `writ serve` and hold no `git`.

The script checks `SHA256SUMS` and the signature after it writes them.

| Recipe | What it does |
| --- | --- |
| `just release-dry-run [VERSION]` | Build everything into `dist/` in the flake's dev shell, signed with a throwaway key, then install from `dist/` with `install.sh`, which checks the signature, and require the installed `writ version` to report the version. |
| `just release-test-image` | Load the amd64 image from `dist/` into a scratch rootless podman store, run `writ serve --dev` in it, require `/healthz` to answer `ok`, and require `writ repo add` of a mounted local repository to fail with `git is not installed`, since the image holds no `git`. |
| `just release-reproducible` | Build twice, the second time with an empty Go build cache, and require the same SHA-256 for every binary and image. |
| `nix build .#writ`, `nix build .#image` | The flake's package and image, built from source with the commit as the version. |

## Reproducible builds

The bytes of each binary depend only on the source, the Go version, and the version string. `build.sh` refuses any Go but the exact version in `go.mod`, and sets `GOTOOLCHAIN=local` so Go never downloads another. It builds with `CGO_ENABLED=0`, `-trimpath`, and `-ldflags "-s -w -X main.version=V"`, sets `GOAMD64=v1` and `GOARM64=v8.0`, and clears `GOFLAGS`. `-s -w` drops the symbol table and DWARF, and a panic still prints its stack trace. `just release-reproducible` gave the same SHA-256 for all six artifacts on 2026-11-10.

Each image holds the release binary itself, copied into the Nix store unchanged. So the image and the download have the same `writ` bytes, and the arm64 image builds on an amd64 machine without emulation: nothing else in the image is code. Layer times are the epoch, so the archive depends only on its inputs. `nix build .#image --rebuild` built the same archive again.

## Why minisign

The choice was minisign or cosign keyless. minisign signs offline with a key file, the same way on a laptop and in CI, and a user needs only the `minisign` binary to check a release. Cosign keyless needs an OIDC token from CI, a certificate from Fulcio, and an entry in the Rekor log at signing time, so it cannot run on a laptop without a sign-in or without contacting Sigstore. Its gain is no long-lived key. The cost of minisign is that key: whoever holds a leaked key can sign a fake release, so the owner must then publish a new key and say which releases each key signed.

`install.sh` carries the public key of the release it came from. That protects a download from a mirror or a changed asset. It does not protect against someone who can replace both `install.sh` and the files of a release. For that, check with the key in the repository, as [Install](../docs/install.md#check-a-download-by-hand) shows.

## Sign in CI

`build.sh --sign-key FILE` (or `WRIT_MINISIGN_KEY=FILE`) signs with a real key, and takes its public key from `release/minisign.pub` (or `--public-key FILE`). The signing step alone reads stdin, so pipe the key's password in:

```
printf '%s\n' "$WRIT_MINISIGN_PASSWORD" | nix develop --command release/build.sh --version "$TAG" --sign-key "$WRIT_MINISIGN_KEY_FILE"
```

A key that does not match `release/minisign.pub` fails the build at the check after signing. Both paths ran on 2026-11-10 with scratch keys: a password-protected key signed, and a key that did not match failed with `Signature key id ... but the key id in the public key is ...`.

The release workflow writes the secret `WRIT_MINISIGN_KEY` to a file readable only by the runner's user, signs as above with `WRIT_MINISIGN_PASSWORD` on stdin, and removes the file when the step ends.

## Publish a release

Tag the commit and push the tag:

```
git tag -s v0.1.0
git push origin v0.1.0
```

The tag starts `.github/workflows/release.yml`. Each job has only the permissions it needs:

1. `build`, with read access to the repository: builds `dist/` in the flake's dev shell with the real key, as in [Sign in CI](#sign-in-ci), with the tag as the version. It fails when the secret `WRIT_MINISIGN_KEY` is empty or doesn't match `release/minisign.pub`. Then it installs from `dist/` with `install.sh`, which checks the signature and the checksum, and requires `writ version` to report the tag. It keeps `dist/` as a workflow artifact for 7 days.
2. `github-release`, with write access to the repository's contents: creates the GitHub release of the tag with every file in `dist/` and generated notes. A tag with a `-`, such as `v0.1.0-rc1`, makes a prerelease. The image archives stay in the release for hosts that cannot reach the registry, and `SHA256SUMS` covers them.
3. `image`, with write access to packages: pushes each image archive with `skopeo` from the flake's nixpkgs as `ghcr.io/mr-karan/writ:0.1.0-amd64` and `ghcr.io/mr-karan/writ:0.1.0-arm64`, then joins them in one manifest list, `ghcr.io/mr-karan/writ:0.1.0`, with `docker buildx imagetools`. It pushes no `latest` tag.

`github-release` and `image` run after `build`, side by side. When one fails, rerun only that job from the workflow run's page: it takes `dist/` from the artifact again.

Then check the release from a clean machine with `minisign` installed, with the one-line install of [Install](../docs/install.md#the-install-script).

The first release, v0.1.0, was tagged on 2026-11-17.
