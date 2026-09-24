# Standalone ur.io setup

`setup.sh` provisions the complete desktop/mobile browser matrix used by
`mmm/ur.io/test-main.sh`. It needs the sibling `sdk`, `extension`, `localizations`,
and site checkouts under `URNETWORK_ROOT`; it does not publish anything.

The site already consumes the sibling SDK. Standalone setup now also builds
and checks that checkout's canonical npm package with
`make -C sdk/js package check-package`. This uses the existing package builder,
paired Go WASM/runtime glue, JS/types build, SDK tests, and package-consumer
checks. A failed build/check stops setup; no older package is silently reused.

`install-local-extension-sdk.mjs` requires the canonical tarball version and
dependency/peer contract to match the extension's exact locked SDK version.
It verifies the package inventory and checked-manifest SHA-256, computes the
tarball SHA-512, and records the SDK commit plus tracked/untracked non-ignored
source-content hash. Updating SDK dependencies or its version requires an
explicit corresponding extension lock update, not an automatic resolver run.

The helper copies the extension manifest and lock into a private staging
directory. Only the SDK dependency/resolution/integrity is substituted with a
snapshot of the local tarball. `npm ci --no-audit --no-fund` then validates the
complete staged lock, including every unchanged registry integrity and all
normal install scripts. Tracked manifests/locks are never rewritten, and
locally built bytes are never inserted under the old registry package hash.

After checking symlinks are relocatable and the source inputs stayed stable,
the completed `node_modules` replaces the previous install. Failed staging
preserves the previous install. The private receipt and staged lock are kept
as `node_modules/.urnetwork-local-sdk{,-lock}.json`; the SDK `file:./sdk.tgz`
reference in that audit copy names the verified staging snapshot, not a
registry URL or a new committed lockfile. SDK source/artifact, original
manifest/lock, or installer changes invalidate the receipt. The next setup
rebuilds/checks the SDK even when the dependency install can be reused.

Root extension install lifecycle hooks or additional local/link dependencies
fail closed: they would require a fuller source-aware installation strategy.
The extension's type checks, complete unit suite, and Chrome/Firefox builds
remain required by their existing acceptance runner. Browser provisioning and
mobile touch/geometry checks are unchanged.

Offline regression (real npm, loopback registry, no browsers):

```sh
node --test --test-concurrency=1 all/web/install-local-extension-sdk.test.mjs
GOMAXPROCS=2 go -C all test -run '^TestWebSetup' -count=1
```

Public registry installation and the extension's standalone registry-based
CI remain separate: this helper is deliberately scoped to sibling-checkout
ur.io acceptance and does not claim that an unpublished registry pin exists.
