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

The script downloads the release binary for your OS and architecture, `SHA256SUMS`, and `SHA256SUMS.minisig`. It verifies the minisign signature against the embedded release key, checks the binary's SHA-256, runs `writ version` to confirm the version matches, and atomically installs `~/.local/bin/writ` without `sudo`.

### Before you begin

Install [`minisign`](https://jedisct1.github.io/minisign/) first. Without it, the script stops before downloading anything because a checksum alone doesn't prove the Writ release key signed the file:

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

To inspect the script before running it, download it first and run `sh install.sh`.

Example output (first without `minisign`, then with it):

```
install.sh: minisign is not installed, so the signature of SHA256SUMS cannot be checked; nothing installed. Install minisign and run this again: brew install minisign (macOS), apt install minisign (Debian, Ubuntu), dnf install minisign (Fedora), nix profile install nixpkgs#minisign, or see https://jedisct1.github.io/minisign/

installed writ v9.9.9 to /home/dev/.local/bin/writ (SHA-256 checked, signature checked)
/home/dev/.local/bin is not on your PATH; add it: export PATH="/home/dev/.local/bin:$PATH"
```

Output of a real run, 2026-11-10.

If the binary's checksum fails, nothing is installed:

```
install.sh: writ-linux-amd64 has SHA-256 eafe950cb49f9769424fc2971bd176aa5c1e22b7d881ffbe94836bf80f1849cf, but SHA256SUMS says 7fb400623abfa4cac138255ff762b4c8483331aed9369c59cd4659b9720d28bd; nothing installed
```

The script also refuses to overwrite a symlink or directory at the target path, rejects `--version` values outside `[A-Za-z0-9._-]`, and installs atomically without following symlinks.

| Flag | Env var | Meaning |
| --- | --- | --- |
| `--version V` | `WRIT_VERSION` | Release tag to install (stamped into each release's script). |
| `--dir DIR` | `WRIT_INSTALL_DIR` | Install directory. Default `~/.local/bin`. |
| `--from DIR` | | Install from a local directory with no network. |
| `--insecure-skip-signature` | | Skip signature verification (still checks SHA-256 and warns). |
| | `WRIT_MINISIGN_PUBKEY` | Override the release public key. |
| | `WRIT_BASE_URL` | Override the download base URL (`https://`). |

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

By default the container runs `writ serve --config /etc/writ/serve` with `/var/lib/writ` as its work directory. Because it runs as non-root UID 65532, set `listen` to a port above 1024 (such as `"0.0.0.0:7340"`):

```sh
podman run -d --name writ \
  -v ./serve:/etc/writ/serve:ro \
  -v writ-data:/var/lib/writ \
  -p 7340:7340 \
  ghcr.io/mr-karan/writ:0.1.8
```

Validate a config with `serve --config /etc/writ/serve --check`. You can also run dev mode on the host network:

```sh
podman run --rm --network host ghcr.io/mr-karan/writ:0.1.8 serve --dev
```

```
$ curl -sS http://127.0.0.1:7340/healthz
{"format":"writ.health/v0","ok":true,"database":true,"issuers":[],"diagnostics":[]}
```

Output of a real run, 2026-11-16.

Because the image omits `git`, it can't read local repositories under `--dev` or clone `git::https://` modules. Team servers fetch GitLab repos over HTTPS without `git`.

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

## Cache location

| Env var | Purpose | Default |
| --- | --- | --- |
| `WRIT_CACHE_DIR` | Provider binaries and cached schemas for `writ import bulk` and `writ mcp` | `$XDG_CACHE_HOME/writ` (`~/.cache/writ` on Linux; `/var/lib/writ/cache` in the container image) |

## Troubleshooting

- **`install.sh: minisign is not installed`**: Install `minisign` (see [Before you begin](#before-you-begin)) and rerun the install script.
- **`writ: command not found`**: Add `~/.local/bin` to `PATH`.
- **`writ repo add .` fails in the container image**: The image omits `git`. Use the release binary or Nix package for local repositories under `--dev`.
- **Container exits with permission denied on port 443**: The container runs as UID 65532. Listen above port 1024 and terminate TLS at a reverse proxy.

## Next steps

- [Quick start](quick-start.md): run `writ serve --dev` and apply your first stack.
- [Run a server](run.md): deploy a team server with Postgres or NixOS.
