## What this changes

<!-- What changes for a person who runs Writ, and why. Link the issue it closes. -->

## How it was tested

<!-- The gates you ran and their result, such as `just check` and `just e2e`. Name any gate you could not run. -->

## Checklist

- [ ] `just check` passes.
- [ ] Tests cover the new behavior, and no test reaches a real cloud host.
- [ ] Docs under `docs/` say what changed, with real output, and `just website` passes if docs changed.
- [ ] A new dependency is pinned to an exact version, with its reason in the commit body, and `just update-nix-hash` was run.
