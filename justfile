staticcheck := "honnef.co/go/tools/cmd/staticcheck@v0.8.2"

# Build the writ binary into ./bin.
build:
	CGO_ENABLED=0 go build -ldflags "-X main.version=$(git describe --always --dirty)" -o bin/writ ./cmd/writ

# Format, vet, lint, and run the unit tests. Fail, without running Nix,
# when go.mod, go.sum, or flake.lock changed since vendorHash in
# deploy/nix/package.nix was last computed.
check:
	deploy/nix/vendor-hash.sh stamp
	test -z "$(gofmt -l $(git ls-files '*.go'))" || (gofmt -l $(git ls-files '*.go'); exit 1)
	go vet ./...
	go run {{staticcheck}} ./...
	go test -race -timeout 20m ./...

# Generate Go code for both plugin protocols into internal/tfplugin5 and internal/tfplugin6.
# The Go plugins are built at pinned versions; protoc comes from nixpkgs.
proto:
	#!/usr/bin/env bash
	set -euo pipefail
	bin="$(mktemp -d)"
	trap 'rm -rf "$bin"' EXIT
	GOBIN="$bin" go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12
	GOBIN="$bin" go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.2
	for v in 5 6; do
		mkdir -p "internal/tfplugin$v"
		nix shell nixpkgs#protobuf -c protoc \
			--plugin=protoc-gen-go="$bin/protoc-gen-go" \
			--plugin=protoc-gen-go-grpc="$bin/protoc-gen-go-grpc" \
			--proto_path=proto \
			--go_out="internal/tfplugin$v" --go_opt=paths=source_relative \
			--go_opt="Mtfplugin$v.proto=github.com/mr-karan/writ/internal/tfplugin$v" \
			--go-grpc_out="internal/tfplugin$v" --go-grpc_opt=paths=source_relative \
			--go-grpc_opt="Mtfplugin$v.proto=github.com/mr-karan/writ/internal/tfplugin$v" \
			"tfplugin$v.proto"
	done
	gofmt -w internal/tfplugin5 internal/tfplugin6

# The end-to-end test.
# TestTofuPlanByCommit plans OpenTofu stacks of a local Git repository by
# commit through writ serve --dev, with SQLite on loopback and the policy
# of examples/policy, checks each masked plan, and requires the policy's
# refusal of a commit that makes an RSA key of 2048 bits. TestTofuApproveApply runs a team's writ
# serve, with Postgres, a test issuer, a stand-in of GitLab, and provider
# uids: a second person approves a plan, the job on main applies it, the
# same files plan no changes, an edit plans one update, a stale plan is
# refused, and after a key changes, a drift check and a plan mask the old
# key, which the state keeps in an address keyed by nonsensitive(). Each
# then scans the control database and the data directory for the canary
# the stacks take through a sensitive variable, and for both keys.
# TestTofuMCP drives writ mcp against writ serve --dev. TestTofuEgress
# runs a team's writ serve under egress_mode enforce: a module from the
# listed host comes through writ's egress proxy, one from an unlisted
# host is refused with a diagnostic that names it. OpenTofu, which
# must be 1.13.9, and Postgres come from the flake's nixpkgs. Fills
# TF_PLUGIN_CACHE_DIR with hashicorp/random, local, and tls on first run,
# then runs in user, network, mount, and pid namespaces with only
# loopback. Needs unshare with subordinate uids, and ip.
e2e:
	nix shell --inputs-from . nixpkgs#opentofu nixpkgs#postgresql_18 -c go test -tags e2e,e2e_tofu -count=1 -v -timeout 20m -run '^(TestTofuPlanByCommit|TestTofuApproveApply|TestTofuMCP|TestTofuEgress)$' ./e2e

# The test of stacks on S3:
# TestTofuS3 runs a team's writ serve on an OpenTofu stack whose state is
# in a bucket on a local S3 stand-in, versitygw, with use_lockfile. STS is
# a stand-in that answers the plan and apply roles with the keys of the
# stand-in's users: the plan role's may read the state and write nothing.
# A plan writes nothing to the bucket, an approved apply writes the state
# and the index, the same files plan no changes, the refresh marks a tofu
# apply outside Writ as drift on the stack's index record, after an edit
# applied from Writ's serial. Drift checks, which an apply or a write
# outside Writ makes due, find nothing after each apply of Writ, and drift
# unknown after the write outside Writ, which has no secret set, as a plan
# of it keeps no detail. Then it scans the control database and the data
# directory for the canary. OpenTofu, Postgres, and versitygw come from
# the flake's nixpkgs, and it runs in namespaces as just e2e does.
e2e-tofu-s3:
	nix shell --inputs-from . nixpkgs#opentofu nixpkgs#postgresql_18 nixpkgs#versitygw -c go test -tags e2e,e2e_tofu -count=1 -v -timeout 20m -run '^TestTofuS3$' ./e2e

# The differential test of the mask, docs/secrets.md.
# TestVerifyMask generates `stacks` OpenTofu stacks over random, local, and
# tls that pass a sensitive variable through templates, encodings, nested
# blocks, for_each, a module, outputs, and nonsensitive(), and plans and
# applies each through writ serve --dev twice, with two canaries. Check 1:
# what the API serves and the control database hold are the same in both
# runs once ids, hashes, and times are normalized. Check 2: no canary,
# plain, base64, hex, or URL-encoded, is in the database, the data and
# scratch directories, an output, or the log. Each control plane indexes
# its stacks again by a refresh at a restart with the index emptied. Check
# 3: the default of a sensitive variable, which only the files and the
# plan hold, is in no index row, output, or log. Then FuzzPlan fuzzes the
# mask for `fuzztime`. Runs in namespaces as just e2e does.
verify-mask stacks="6" fuzztime="30s":
	nix shell --inputs-from . nixpkgs#opentofu -c go test -tags e2e,e2e_tofu -count=1 -v -timeout 20m -run '^TestVerifyMask$' ./e2e -args -verify-mask.stacks={{stacks}}
	go test -run '^$' -fuzz '^FuzzPlan$' -fuzztime {{fuzztime}} ./internal/mask

# The policy parity test, docs/policy.md: one
# .rego file gives the same verdict in writ serve and under conftest.
# conftest verify runs the tests of examples/policy. Then TestPolicyParity
# plans three OpenTofu stacks through writ serve --dev, which judges them
# with examples/policy, and again with tofu alone, whose `tofu show -json`
# conftest judges with the same directory, and requires the same verdicts
# and messages. OpenTofu and conftest come from the flake's nixpkgs. It
# runs in namespaces as just e2e does.
policy-parity:
	nix shell --inputs-from . nixpkgs#conftest -c conftest verify --no-color --policy examples/policy
	nix shell --inputs-from . nixpkgs#opentofu nixpkgs#conftest -c go test -tags e2e,e2e_tofu -count=1 -v -timeout 20m -run '^TestPolicyParity$' ./e2e

# Plan the imports of AWS objects through the importer's provider client
# against hashicorp/aws 6.70.0 with no network: the provider runs in a
# network namespace with only loopback. Downloads the provider into
# WRIT_CACHE_DIR on first run. Needs unshare.
e2e-import:
	WRIT_OFFLINE_AWS_GATE=1 go test -count=1 -v -run '^TestOfflineAWSImportGate$' ./internal/provider

# Take over existing objects into OpenTofu stacks with writ import bulk and
# require resource and import blocks that tofu fmt leaves alone, a tofu
# plan in an empty state that only imports, and writ import accept passing
# on it: random objects that tofu applied, a job that tofu registered on a
# Nomad dev agent on loopback, and an ELB listener rule and a launch
# template that a stand-in of the AWS API serves to hashicorp/aws 6.70.0.
# The Nomad job plans an update of the provider's own settings, which
# writ import accept must allow from the saved plan and print. OpenTofu, which must be 1.13.9, comes from the flake's
# nixpkgs; nomad comes from PATH. Fills TF_PLUGIN_CACHE_DIR, and the AWS
# provider in it, on first run, then runs the random and AWS steps in a
# network namespace with only loopback. Needs unshare and ip.
e2e-import-hcl:
	nix shell --inputs-from . nixpkgs#opentofu -c go test -tags e2e,e2e_import -count=1 -v -timeout 15m -run '^(TestImportHCL|TestImportHCLAWS)$' ./e2e

# Adopt AWS objects that OpenTofu created on AWS Emulator, through the real
# hashicorp/aws 6.70.0: a launch template, a security group, a target
# group, an ALB, its listener, and a listener rule. writ import bulk must
# write code whose tofu plan in an empty state shows exactly 6 imports,
# with readable names and no doubt about that plan, and writ import
# accept must pass that plan. The emulator, tofu, and writ run only in a
# user, mount, and network namespace with only loopback, where the usual
# credential files read as empty, the emulator with its telemetry off.
# tofu installs providers only from TF_PLUGIN_CACHE_DIR, so fill it first
# with just e2e-aws-emulator-providers. The emulator comes from the flake
# (deploy/nix/aws-emulator.nix, a 187 MB download and 1 GB on disk) and
# OpenTofu, which must be 1.13.9, from its nixpkgs. The emulator is built
# only for x86_64-linux; elsewhere the recipe says it skips and passes.
# Needs unshare and ip.
e2e-aws-emulator:
	#!/usr/bin/env bash
	set -euo pipefail
	if [[ "$(uname -sm)" != "Linux x86_64" ]]; then
		echo "e2e-aws-emulator: skipped: AWS Emulator is built only for x86_64-linux, not $(uname -sm)"
		exit 0
	fi
	nix shell --inputs-from . nixpkgs#opentofu .#aws-emulator -c go test -tags e2e,e2e_import,e2e_awsemu -count=1 -v -timeout 15m -run '^(TestImportAWSEmulator|TestAWSEmulatorApplyRetry)$' ./e2e

# Fill OpenTofu's plugin cache with hashicorp/aws 6.70.0 from the OpenTofu
# registry for just e2e-aws-emulator, which installs providers from nowhere
# else. It runs apart from the test, with every AWS_ variable unset. The
# cache is TF_PLUGIN_CACHE_DIR, or writ-e2e/tofu-plugins in the user cache
# directory, as the e2e tests use.
e2e-aws-emulator-providers:
	#!/usr/bin/env bash
	set -euo pipefail
	unset "${!AWS_@}"
	export TF_PLUGIN_CACHE_DIR="${TF_PLUGIN_CACHE_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/writ-e2e/tofu-plugins}"
	mkdir -p "$TF_PLUGIN_CACHE_DIR"
	dir=$(mktemp -d)
	trap 'rm -rf "$dir"' EXIT
	cp e2e/testdata/import-hcl/aws.lock.hcl "$dir/.terraform.lock.hcl"
	printf '%s\n' 'terraform {' '  required_providers {' '    aws = { source = "hashicorp/aws", version = "6.70.0" }' '  }' '}' > "$dir/main.tf"
	: > "$dir/tofurc"
	TF_CLI_CONFIG_FILE="$dir/tofurc" CHECKPOINT_DISABLE=1 nix shell --inputs-from . nixpkgs#opentofu -c tofu -chdir="$dir" init -input=false -lockfile=readonly -backend=false -no-color

# Build the landing page and docs site from docs/ into website/out. A docs
# edit that breaks a page selector fails here. Bun and Node come from nixpkgs.
website:
	cd website && nix shell nixpkgs#bun nixpkgs#nodejs -c bash -c 'bun install --frozen-lockfile && bun run typecheck && bun run build'

# Shoot the control UI for the landing page from the showcase test of
# internal/ui at a Git ref: overview, a plan, drift, and runs at 1280 and
# 390 px, light and dark, with demo names alice, bob, and infra. Writes
# AVIF and WebP to website/public/ui and the date to
# website/lib/ui-shots.json. Run it again after the control UI changes.
# Chromium, avifenc, and cwebp come from nixpkgs; it reaches only loopback.
ui-shots ref="HEAD":
	cd website && nix shell nixpkgs#bun nixpkgs#chromium nixpkgs#libavif nixpkgs#libwebp -c bun scripts/ui-shots.ts {{ quote(ref) }}

# Fuzz the OIDC token and key set parsers, and the refusals of every flow
# against an identity provider whose one answer is the fuzzer's, for a
# while each. Pass a duration: `just fuzz-oidc 5m`. New inputs that fail
# land in internal/oidc/testdata/fuzz.
fuzz-oidc time="2m":
	go test -run '^$' -fuzz '^FuzzVerify$' -fuzztime {{time}} ./internal/oidc
	go test -run '^$' -fuzz '^FuzzParseKeySet$' -fuzztime {{time}} ./internal/oidc
	go test -run '^$' -fuzz '^FuzzRefusal$' -fuzztime {{time}} ./internal/oidc

# Run the control plane's tests with a scratch Postgres: the control
# database, the API, and the control UI, with a plan role against an STS
# stand-in, and the showcase behind the uishowcase tag, which must serve
# every page it lists. Then prove provider isolation in user and network namespaces:
# providers as other uids behind the metadata firewall rule, the AWS
# provider refreshing its credentials, plans as a run's uid from a writ
# that is not root and holds only the capabilities its unit gives it,
# each OpenTofu step held to its memory, process, CPU, and scratch limits
# in a cgroup of its own, from a writ that is not root in a cgroup
# delegated as Delegate=yes delegates it and from a writ that is root
# with no cgroup filesystem mounted, as under Nomad's exec driver, every
# tofu process under writ's seccomp filter, and a run under egress_mode
# enforce, in its own network namespace, reaching only what writ's
# egress proxy allows and still getting its credentials. Needs unshare
# with subordinate uids, and systemd-run --user for a delegated cgroup.
# Postgres and nftables come from nixpkgs. The isolation tests run
# without cgo, as writ ships, so no -race there. just e2e runs writ
# serve end to end.
test-serve:
	nix shell nixpkgs#postgresql_18 -c go test -race -count=1 -timeout 20m ./internal/control/... ./internal/tfstate/... ./internal/server/... ./internal/ui/... ./internal/awsrole/...
	go test -race -count=1 -tags uishowcase -run '^TestShowcaseServes$' ./internal/ui
	CGO_ENABLED=0 nix shell nixpkgs#nftables -c go test -count=1 -run 'TestProviderIsolation|TestAWSProviderRefreshesCredentials|TestPlanAsRunUserWithoutRoot|TestRunLimits|TestSeccompFilter' ./internal/provider ./internal/tofu
	CGO_ENABLED=0 nix shell nixpkgs#nftables -c go test -count=1 -run '^TestRunEgressIsolation$' ./internal/tofu

# Run the flake's OpenTofu, which must be 1.13.9, against the http state
# backend of internal/tfstate: plan, apply, and plan again, a plan's token
# refused on writes, a second apply refused while one holds the lock, and
# a stale write refused. Fills TF_PLUGIN_CACHE_DIR with hashicorp/random
# and local on first run, then runs in a user and network namespace with
# only loopback. Needs unshare and ip.
test-tfstate:
	nix shell --inputs-from . nixpkgs#opentofu -c go test -tags tofu -count=1 -v -run '^TestGate$' ./internal/tfstate

# Run internal/tofu against the flake's OpenTofu, which must be 1.13.9,
# with hashicorp/random, local, and tls. Fills TF_PLUGIN_CACHE_DIR on first
# run, then runs every case in user, network, mount, and pid namespaces
# with only loopback, under a root directory of the namespace's own, as
# writ and as a run uid. Needs unshare with subordinate uids.
test-tofu:
	nix shell --inputs-from . nixpkgs#opentofu -c go test -tags tofu -count=1 -v ./internal/tofu

# Lint the GitLab CI component and its example with yamllint. GitLab's own
# CI lint needs a GitLab instance, so this checks YAML, not GitLab's schema.
lint-gitlab-ci:
	nix shell nixpkgs#yamllint -c yamllint --strict -c ci/gitlab/.yamllint.yml ci/gitlab

# Build the flake's writ package. When go.mod, go.sum, or flake.lock
# changed since deploy/nix/go-sum.sha256 was written, compute the right
# vendorHash instead, print it and the line to change, and fail. Nix
# caches the build, so a second run with no change takes seconds.
check-nix-package:
	deploy/nix/vendor-hash.sh check

# Compute vendorHash for the current go.mod, go.sum, and flake.lock, write
# it into deploy/nix/package.nix and their checksums into
# deploy/nix/go-sum.sha256, and build the package. Run it after any change
# to the Go modules, and commit both files.
update-nix-hash:
	deploy/nix/vendor-hash.sh update

# Evaluate the NixOS module of the flake against its pinned nixpkgs, print
# its unit, check that the unit's PATH has Nix's git, build the writ
# package the module uses and the OpenTofu of the sample config's tofu
# block, and check the sample config with that writ serve --check, which
# must find OpenTofu 1.13.9 there and so plan stacks of .tf files.
check-nixos:
	#!/usr/bin/env bash
	set -euo pipefail
	dir="$(mktemp -d)"
	trap 'rm -rf "$dir"' EXIT
	nix eval --impure --raw --file deploy/nixos/check.nix unit
	nix eval --impure --raw --file deploy/nixos/check.nix firewall
	nix eval --impure --json --file deploy/nixos/check.nix tmpfiles
	echo
	nix eval --impure --json --file deploy/nixos/check.nix postgres
	echo
	nix eval --impure --raw --file deploy/nixos/check.nix config > "$dir/serve"
	writ="$(nix build --impure --no-link --print-out-paths --file deploy/nixos/check.nix writ)"
	grep -qF "ExecStart=$writ/bin/writ serve" <(nix eval --impure --raw --file deploy/nixos/check.nix unit)
	grep -qF "$(nix eval --impure --raw --file deploy/nixos/check.nix git)/bin" <(nix eval --impure --raw --file deploy/nixos/check.nix unit)
	# Each OpenTofu step's cgroup needs the unit's cgroup delegated and
	# writable.
	grep -qxF "Delegate=true" <(nix eval --impure --raw --file deploy/nixos/check.nix unit)
	! grep -qxF "ProtectControlGroups=true" <(nix eval --impure --raw --file deploy/nixos/check.nix unit)
	tofu="$(nix build --impure --no-link --print-out-paths --file deploy/nixos/check.nix tofu)"
	"$writ/bin/writ" serve --config "$dir/serve" --check | tee "$dir/check"
	if ! grep -qxF "tofu: $tofu/bin/tofu is OpenTofu 1.13.9: ok" "$dir/check"; then
		echo "check-nixos: the sample config runs no OpenTofu 1.13.9, so writ serve would refuse every stack of .tf files" >&2
		exit 1
	fi

# Run every local gate in order, stop at the first that fails, and print
# each gate's time. TMPDIR is a new directory under /var/tmp unless it is
# already set; ci removes a directory it made when every gate passes.
# Left out: lint-gitlab-ci, the fuzz recipes, and the
# release recipes run on their own when a change touches them.
[doc('Run every local gate in order and stop at the first that fails.')]
ci:
	#!/usr/bin/env bash
	set -euo pipefail
	made=""
	if [[ -z "${TMPDIR:-}" ]]; then
		made="$(mktemp -d /var/tmp/writ-ci.XXXXXX)"
		export TMPDIR="$made"
	fi
	echo "ci: TMPDIR=$TMPDIR"
	summary=()
	for gate in check e2e e2e-import e2e-import-hcl e2e-aws-emulator e2e-tofu-s3 verify-mask policy-parity test-serve test-tfstate test-tofu website check-nix-package check-nixos; do
		start=$SECONDS
		if ! {{ quote(just_executable()) }} --justfile {{ quote(justfile()) }} "$gate"; then
			summary+=("ci: $gate failed after $((SECONDS - start))s")
			printf '%s\n' "${summary[@]}" >&2
			echo "ci: TMPDIR $TMPDIR is kept" >&2
			exit 1
		fi
		summary+=("ci: $gate passed in $((SECONDS - start))s")
	done
	printf '%s\n' "${summary[@]}"
	if [[ -n "$made" ]]; then
		rm -rf "$made"
	fi

# Build every release artifact into dist/ and publish nothing: static
# binaries for linux and darwin on amd64 and arm64, the writ serve images,
# install.sh, SHA256SUMS, and its minisign signature by a throwaway key.
# Then install from dist/ with install.sh, which checks the signature and
# the checksum, and require the installed writ to report the version. Go
# and minisign come from the flake's dev shell. Pass a version to stamp:
# `just release-dry-run v0.5.0`. See release/README.md.
release-dry-run version="":
	#!/usr/bin/env bash
	set -euo pipefail
	version={{ quote(version) }}
	[[ -n "$version" ]] || version="$(git describe --tags --always --dirty)"
	nix develop --command release/build.sh --version "$version" --out dist
	bin="$(mktemp -d)"
	trap 'rm -rf "$bin"' EXIT
	nix develop --command sh dist/install.sh --from dist --dir "$bin"
	got="$("$bin/writ" version)"
	echo "$got"
	[[ "$got" == "writ $version" ]] || { echo "release-dry-run: the installed writ reports '$got', want 'writ $version'" >&2; exit 1; }
	# Compare with the parameter itself too, so a lost --version fails here.
	[[ -z {{ quote(version) }} || "$got" == "writ "{{ quote(version) }} ]] || { echo "release-dry-run: the installed writ reports '$got', not the requested version" >&2; exit 1; }

# Run writ serve --dev inside the amd64 image in dist/ with rootless
# podman from the flake's nixpkgs, require /healthz to answer, and require
# writ repo add of a mounted repository to fail because the image has no
# git. git on the host makes that repository. Run release-dry-run first.
release-test-image:
	nix shell --inputs-from . nixpkgs#podman nixpkgs#curl nixpkgs#gitMinimal -c release/test-image.sh dist/writ-image-linux-amd64.tar.gz "$(dist/writ-linux-amd64 version | cut -d ' ' -f 2)"

# Build the release twice, the second time with an empty Go build cache,
# and require the same bytes for every binary and image.
release-reproducible:
	#!/usr/bin/env bash
	set -euo pipefail
	a="$(mktemp -d)" b="$(mktemp -d)" cache="$(mktemp -d)"
	trap 'rm -rf "$a" "$b" "$cache"' EXIT
	nix develop --command release/build.sh --out "$a/dist" >/dev/null
	GOCACHE="$cache" nix develop --command release/build.sh --out "$b/dist" >/dev/null
	diff <(grep ' writ-' "$a/dist/SHA256SUMS") <(grep ' writ-' "$b/dist/SHA256SUMS")
	grep ' writ-' "$a/dist/SHA256SUMS"
	echo "two builds gave the same bytes"
