# Install

The current release is **v0.1.8**. Releases include static binaries for Linux and macOS (`amd64` and `arm64`), a multi-arch `writ serve` container image, an install script, `SHA256SUMS`, and its minisign signature. There is no Windows build.

| Method | Best for |
| --- | --- |
| [The install script](#the-install-script) | Local CLI on a laptop |
| [The container image](#the-container-image) | Team `writ serve` and CI jobs |
| [Nix](#nix) | Nix users and NixOS servers |
| [Build from source](#build-from-source) | Contributors |

**Limits.** macOS binaries are cross-built on Linux and haven't been tested on macOS. The `arm64` binary carries Go's ad hoc signature; `amd64` has none. Neither is notarized, so browser downloads trigger macOS quarantine (downloading with `curl` or the install script does not).

## The install script

The script downloads the binary for your OS and architecture, `SHA256SUMS`, and `SHA256SUMS.minisig`. It checks the minisign signature against the embedded release key and the binary's SHA-256, runs `writ version` to confirm the version, and installs `~/.local/bin/writ` atomically, without `sudo`.

### Before you begin

Install [`minisign`](https://jedisct1.github.io/minisign/). Without it, the script stops before downloading anything, because a checksum alone does not prove the Writ release key signed the file:

| OS / Manager | Command |
| --- | --- |
| macOS | `brew install minisign` |
| Debian, Ubuntu | `sudo apt install minisign` |
| Fedora | `sudo dnf install minisign` |
| Nix | `nix profile install nixpkgs#minisign` |

### Run it

```sh
curl -fsSL https://github.com/mr-karan/writ/releases/download/v0.1.8/install.sh | sh
```

To read the script first, download it and run `sh install.sh`. A successful run ends with:

```
installed writ v9.9.9 to /home/dev/.local/bin/writ (SHA-256 checked, signature checked)
```

Real run, 2026-11-10. If `~/.local/bin` is not on `PATH`, the script says so and prints the `export PATH` line. Without `minisign` it installs nothing and prints how to install it. If the checksum fails, it installs nothing and prints both hashes.

Flags such as `--version`, `--dir`, and `--from`, and the script's safety rules, are in [install.sh flags](commands.md#installsh-flags).

Pass flags through a pipe with `sh -s --`:

```sh
curl -fsSL https://github.com/mr-karan/writ/releases/download/v0.1.8/install.sh | sh -s -- --dir /opt/writ/bin
```

### Check a download by hand

Verify `SHA256SUMS` against the release public key from this page (or [`release/minisign.pub`](../release/minisign.pub), key ID `D2D9A37AF1098999`), not the copy shipped alongside the download:

```sh
minisign -Vm SHA256SUMS -P RWSZiQnxeqPZ0uwAJ6eSbsg1B0acHb5UvdJMfAWvUM+dya+188xWcOHf
sha256sum -c --ignore-missing SHA256SUMS
```

On macOS, replace `sha256sum` with `shasum -a 256`.

## The container image

`ghcr.io/mr-karan/writ:0.1.8` is a multi-architecture (`amd64`, `arm64`) image containing `writ`, OpenTofu 1.13.9 at `/bin/tofu`, CA certificates, tzdata, and user `writ` (UID 65532). It has no shell and no `git`.

```sh
podman pull ghcr.io/mr-karan/writ:0.1.8
```

To build and load the image locally with Nix:

```sh
nix build .#image && podman load -i result
```

The container runs `writ serve --config /etc/writ/serve` in `/var/lib/writ`. It runs as non-root UID 65532, so set `listen` to a port above 1024, such as `"0.0.0.0:7340"`:

```sh
podman run -d --name writ \
  -v ./serve:/etc/writ/serve:ro \
  -v writ-data:/var/lib/writ \
  -p 7340:7340 \
  ghcr.io/mr-karan/writ:0.1.8
```

Validate a config with `serve --config /etc/writ/serve --check`. Dev mode runs on the host network:

```sh
podman run --rm --network host ghcr.io/mr-karan/writ:0.1.8 serve --dev
```

```
$ curl -sS http://127.0.0.1:7340/healthz
{"format":"writ.health/v0","ok":true,"database":true,"issuers":[],"diagnostics":[]}
```

Without `git`, the image cannot read local repositories under `--dev` or clone `git::https://` modules. Team servers fetch GitLab repositories over HTTPS and need no `git`.

**Limits.** Running a team server with `provider_uids` inside a container hasn't been tested. Doing so requires `CAP_SETUID`, `CAP_SETGID`, `CAP_KILL`, and the host `nftables` metadata rule; see [Run isolation and egress](run-isolation.md).

## Nix

Run or install from the repository flake:

```sh
nix run github:mr-karan/writ -- version
nix profile install github:mr-karan/writ
```

The Nix package wraps the static binary with `gitMinimal` appended to `PATH`, so your own `git` takes precedence and `git` is always available if missing. Supported flake systems: `x86_64-linux`, `aarch64-linux`, and `aarch64-darwin` (Intel Macs use the release binary).

In your own flake, add `inputs.writ.url = "github:mr-karan/writ";` and reference `writ.packages.${system}.writ`. For NixOS servers, the flake exports `nixosModules.writ-serve`; see [Run a server: On NixOS](run.md#on-nixos).

## Build from source

Requirements: Go 1.27.3+ (matching `go.mod`) and [`just`](https://github.com/casey/just).

```sh
just build
export PATH="$PWD/bin:$PATH"
writ version
```

`just build` compiles a static binary (`CGO_ENABLED=0`) to `bin/writ`. `go install github.com/mr-karan/writ/cmd/writ@COMMIT` also works and embeds the Go module version.

## Git

`git` on `PATH` is required only for:

- `writ serve --dev` reading local Git repositories,
- `writ repo add .` finding the local work tree,
- `writ plan --remote` reading the current commit from a clean work tree (not needed when passing `--repo`, `--commit`, and `--stack`), and
- `tofu.git` on a team server if stacks clone remote `git::https://` modules.

Neither the install script nor the container image bundles `git`; the Nix package does. Without `git`, `writ repo add .` exits with:

```
writ: git is not installed: writ reads local Git repositories with the git binary, and none is on PATH; install git, such as apt install git, dnf install git, brew install git, or nix profile install nixpkgs#gitMinimal
```

Output of a real run, 2026-11-10.

## Troubleshooting

| Problem | Fix |
| --- | --- |
| `install.sh: minisign is not installed` | Install `minisign` ([Before you begin](#before-you-begin)) and rerun the script. |
| `writ: command not found` | Add `~/.local/bin` to `PATH`. |
| `writ repo add .` fails in the container image | The image has no `git`. Use the release binary or the Nix package for local repositories under `--dev`. |
| Container exits with permission denied on port 443 | It runs as UID 65532. Listen above port 1024 and terminate TLS at a reverse proxy. |

## Next steps

- [Quick start](quick-start.md): run `writ serve --dev` and apply your first stack.
- [Run a server](run.md): deploy a team server with Postgres or NixOS.
