# Install

Writ v0.1.0 is the first release. For Linux and macOS, on amd64 and arm64, it has a static `writ` binary, a container image of `writ serve`, an install script, `SHA256SUMS`, and its minisign signature. You can also use Nix or build from source.

| Way | For |
| --- | --- |
| [The install script](#the-install-script) | One person on a laptop |
| [The container image](#the-container-image) | A team's `writ serve`, and CI jobs |
| [Nix](#nix) | Nix users, and NixOS hosts with the module |
| [Build from source](#build-from-source) | People who change Writ |

Writ runs on Linux and macOS, on amd64 and arm64. There's no Windows build.

**Limits.** The macOS binaries are cross-built on Linux, and nobody has run them on a Mac yet. The arm64 binary carries the ad hoc signature that Go's linker adds, which Apple silicon requires, and the amd64 binary has none. They aren't notarized, so macOS blocks a binary that a browser downloaded. The install script downloads with `curl`, so macOS doesn't block what it installs.

## The install script

From one release, the script downloads the binary for your machine, `SHA256SUMS`, and the minisign signature of `SHA256SUMS`. Then it checks the signature against the release key that the script carries, checks the binary's SHA-256 against `SHA256SUMS`, and runs the binary. Only after all three pass does it put `writ` in `~/.local/bin`, and you don't need `sudo`.

### Before you begin

You need [`minisign`](https://jedisct1.github.io/minisign/); without it, the script stops before any download. That's because a SHA-256 match shows only that a file is whole, while the signature shows that the Writ release key signed the list of checksums. Install minisign first:

| System | Command |
| --- | --- |
| macOS | `brew install minisign` |
| Debian, Ubuntu | `sudo apt install minisign` |
| Fedora | `sudo dnf install minisign` |
| Nix | `nix profile install nixpkgs#minisign` |

### Run it

Run:

```
curl -fsSL https://github.com/mr-karan/writ/releases/download/v0.1.0/install.sh | sh
```

Replace `v0.1.0` with the release you want. To try the script on a local build instead, with no download:

```
just release-dry-run
sh dist/install.sh --from dist
```

Here's a dry run stamped `v9.9.9`, for a user whose home is `/home/dev`. On the first run, `minisign` wasn't on `PATH`. On the second, it was.

```
install.sh: minisign is not installed, so the signature of SHA256SUMS cannot be checked; nothing installed. Install minisign and run this again: brew install minisign (macOS), apt install minisign (Debian, Ubuntu), dnf install minisign (Fedora), nix profile install nixpkgs#minisign, or see https://jedisct1.github.io/minisign/

installed writ v9.9.9 to /home/dev/.local/bin/writ (SHA-256 checked, signature checked)
/home/dev/.local/bin is not on your PATH; add it: export PATH="/home/dev/.local/bin:$PATH"
```

Output of a real run, 2026-11-10.

If a binary doesn't match `SHA256SUMS`, the install stops, and nothing changes:

```
install.sh: writ-linux-amd64 has SHA-256 eafe950cb49f9769424fc2971bd176aa5c1e22b7d881ffbe94836bf80f1849cf, but SHA256SUMS says 7fb400623abfa4cac138255ff762b4c8483331aed9369c59cd4659b9720d28bd; nothing installed
```

In these cases too, the script refuses and changes nothing: when `writ` in the install directory is a symlink or a directory, when the new binary doesn't run, or when it reports a version other than the release's. To install, it writes the new binary to a new file with a random name next to `writ` and renames it over `writ`, so a running `writ` is never half written. For the rename it uses `mv -T` (GNU) or `mv -h` (macOS and BSD), so a symlink that appears at `writ` during the install is replaced, not followed, and nothing lands outside the install directory. With neither, the script stops before any download. If you pass a `--version` with a character outside `A-Z a-z 0-9 . _ -`, the script refuses it.

| Option | Variable | What it does |
| --- | --- | --- |
| `--version V` | `WRIT_VERSION` | The release to install. The script from a release sets its own version. |
| `--dir DIR` | `WRIT_INSTALL_DIR` | Where to put `writ`. Default `~/.local/bin`. |
| `--from DIR` | | Takes the files from a local directory, with no network. |
| `--insecure-skip-signature` | | Skips the signature check. The script still checks the SHA-256 and prints a warning, but nothing then shows that the Writ release key signed what you install. |
| | `WRIT_MINISIGN_PUBKEY` | Checks the signature with this public key in place of the one the script carries. |
| | `WRIT_BASE_URL` | Downloads from this `https://` URL in place of the GitHub release. |

To pass an option through a pipe, use `sh -s --`, as in `curl -fsSL .../install.sh | sh -s -- --dir /opt/writ/bin`.

**Warning:** When you pipe to `sh`, you run whatever the server sends. To read the script first, download it, read it, and run it with `sh install.sh`.

### Check a download by hand

With each release you get `SHA256SUMS`, `SHA256SUMS.minisig`, and `minisign.pub`. Check the signature with the release key from this page, not with the `minisign.pub` that came with the download, then check the files:

```
minisign -Vm SHA256SUMS -P RWSZiQnxeqPZ0uwAJ6eSbsg1B0acHb5UvdJMfAWvUM+dya+188xWcOHf
sha256sum -c --ignore-missing SHA256SUMS
```

On macOS, use `shasum -a 256 -c --ignore-missing SHA256SUMS`. The release key is also in [`release/minisign.pub`](../release/minisign.pub), with the key id `D2D9A37AF1098999`. [release/README.md](../release/README.md) explains why the signature uses minisign.

## The container image

Use the image for a team's `writ serve`. Inside you'll find `writ`, OpenTofu 1.13.9 at `/bin/tofu`, CA certificates, the time zone database, and a passwd file with the user `writ` (uid 65532), and no shell and no `git`. Its `PATH` is `/bin`, so `binary = "tofu"` in the config's [`tofu` block](config.md#tofu) finds that OpenTofu.

On a team's `writ serve`, repositories come from a Git host and local ones are refused, so you need `git` only for a module source that `tofu init` clones, such as `git::https://...`, through the `tofu` block's `git`. Since the image has none, such a module source fails in it.

Nix's `dockerTools` builds the image, so no Docker daemon takes part. For each release there's one image archive for amd64 and one for arm64, and each contains the same `writ` bytes as the release binary for that architecture. The release workflow publishes them as one multi-architecture image, `ghcr.io/mr-karan/writ:0.1.0`:

```
podman pull ghcr.io/mr-karan/writ:0.1.0
```

To use an image without the registry, load an archive from the release or from `dist/`, or build one with Nix:

```
just release-dry-run
podman load -i dist/writ-image-linux-amd64.tar.gz

nix build .#image && podman load -i result
```

Mount your config at `/etc/writ/serve` and a volume at `/var/lib/writ`, then run the image. By default it starts a team's `writ serve` with its config at `/etc/writ/serve`. That volume is its home and work directory. Because the container's user isn't root, set `listen` in your config to a port above 1024, such as `"0.0.0.0:7340"`:

```
podman run -d --name writ \
  -v ./serve:/etc/writ/serve:ro \
  -v writ-data:/var/lib/writ \
  -p 7340:7340 \
  ghcr.io/mr-karan/writ:0.1.0
```

[Run a control plane](run.md) and the [server config](config.md) explain the config and its Postgres database. To check a config, run the same image with `serve --config /etc/writ/serve --check`.

You can also run `writ serve --dev` in the image. Because it listens on loopback only, the container shares the host's network:

```
podman run --rm --network host ghcr.io/mr-karan/writ:0.1.0 serve --dev
```

Without `git`, the image can't read a local repository. To try `writ serve --dev` with a local repository, use the release binary or the Nix package on a machine with `git`, as [Git](#git) explains.

From `/healthz` you get the same answer as from `writ serve --dev` outside the image:

```
$ curl -sS http://127.0.0.1:7340/healthz
{"format":"writ.health/v0","ok":true,"database":true,"issuers":[],"diagnostics":[]}
```

Output of a real run, 2026-11-16.

To test the image, `just release-test-image` runs `writ serve --dev` with rootless podman and requires `/healthz` to answer. Next it mounts a repository created on the host, and requires `writ repo add` inside the container to fail with the message about the missing `git`, and `/healthz` to answer again. Then it prints the image, the `/healthz` document, that message, and a line that says the image has no `git`. Its last recorded output, of 2026-11-10, predates the removal of the change watchers from `/healthz`, so it isn't shown.

**Limits.** Nobody has tested a team's `writ serve` with `provider_uids` inside a container. If you try it, it needs `CAP_SETUID`, `CAP_SETGID`, and `CAP_KILL`, and the metadata firewall rule on the host, as [How providers are kept from the keys](serve.md#how-providers-are-kept-from-the-keys) explains.

## Nix

With Nix, you can use the repository as a flake. Its package builds `writ` with the Go of one pinned nixpkgs, static, with the commit as its version. These commands use the flake on the repository's main branch:

```
nix run github:mr-karan/writ -- version
nix profile install github:mr-karan/writ
```

Inside, the package wraps `writ` so that `gitMinimal` from the same nixpkgs is at the end of its `PATH`. If you have your own `git`, it comes first, and the commands in [Git](#git) work when you have none. That wrapper is a compiled program, not a shell script, and the static binary is unchanged inside the package, as `bin/.writ-wrapped`. With git, the package's closure is 194 MiB.

In a flake of your own, add `inputs.writ.url = "github:mr-karan/writ";` and use `writ.packages.${system}.writ`. You can build the flake for `x86_64-linux`, `aarch64-linux`, and `aarch64-darwin`. Its nixpkgs dropped `x86_64-darwin`, so on an Intel Mac you take the release binary.

For a server, the flake also exports `nixosModules.writ-serve`, the NixOS module that runs a team's `writ serve` with Postgres on the same host. Its `package` defaults to the flake's `writ`. [writ serve on NixOS](run.md#on-nixos) has the configuration.

## Build from source

You need:

- Go 1.27.3 or later, the version in `go.mod`.
- [`just`](https://github.com/casey/just), to run the build recipe.

From the root of the repository, build the binary into `bin/writ` and put it on your `PATH`:

```
just build
export PATH="$PWD/bin:$PATH"
writ version
```

You get one static binary, built with `CGO_ENABLED=0`. If you use `go install github.com/mr-karan/writ/cmd/writ@COMMIT` instead, it builds with no version flag, and `writ version` then prints the module version that Go records in the binary.

## Git

Some commands run the `git` binary on `PATH`:

- To read the commits of a local repository, `writ serve --dev` uses it.
- `writ repo add .` finds the work tree with it.
- `writ plan --remote` finds the commit of a clean work tree with it. Without `git`, set the commit with `--repo`, `--commit`, and `--stack`.
- A team's `writ serve` gives each OpenTofu run the `git` that its config's `tofu.git` points to, a path or a name on `PATH`, for module sources that `tofu init` clones.

Neither the install script nor the container image gives you `git`, but the Nix package brings its own. Other commands run no `git`. Install it with `apt install git`, `dnf install git`, `brew install git`, or `nix profile install nixpkgs#gitMinimal`. Without it, the first two commands stop, and a module source that needs `git` fails. Here's `writ repo add .` from the release binary with no `git` on `PATH`:

```
writ: git is not installed: writ reads local Git repositories with the git binary, and none is on PATH; install git, such as apt install git, dnf install git, brew install git, or nix profile install nixpkgs#gitMinimal
```

Output of a real run, 2026-11-10.

## Where Writ keeps files

| Path | What it contains |
| --- | --- |
| `WRIT_CACHE_DIR` | Provider binaries and cached provider schemas. It defaults to `writ` inside your user cache directory. On Linux that's `~/.cache/writ`, unless `XDG_CACHE_HOME` is set. In the container image it's `/var/lib/writ/cache`. |
