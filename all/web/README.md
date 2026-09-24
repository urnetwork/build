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
mobile touch/geometry checks remain mandatory.

`firefox-smoke.mjs` must create and delete a real geckodriver session. It reuses
the site's `tests/firefox-process.mjs` exact-profile owner, including a bounded
15-second late-registration window after failed startup or a detected browser
replacement. This covers a pending macOS Firefox update that exits its original
process with status 0 and later relaunches the same temporary profile under
another parent. Cleanup never targets ordinary Firefox profiles or similarly
prefixed temporary profiles. Missing ownership, surviving owned processes, or
session deletion failures stop setup.

The smoke snapshots executable identity before launch and after cleanup. An
application update during the check is an explicit failure, not a retry or a
passed browser gate. Once the host update and owned-process cleanup have
finished, run setup again under its normal suite lock to test the stable new
browser. Setup does not disable host updates or change Firefox preferences.

Offline regression (real npm, loopback registry, no browsers):

```sh
node --test --test-concurrency=1 all/web/install-local-extension-sdk.test.mjs
node --test --test-concurrency=1 all/web/firefox-smoke.test.mjs
GOMAXPROCS=2 go -C all test -run '^TestWebSetup' -count=1
```

The Firefox regression uses virtual WebDriver/process I/O and a source-pinned
copy of the existing profile owner, so it never starts a browser. To reproduce
the pre-fix failure against the exact inline source in an ancestor commit:

```sh
UR_FIREFOX_SMOKE_BASELINE_COMMIT=cfa1b727893e1ed08754e27a0117b97c4aaee356 \
  node --test --test-concurrency=1 all/web/firefox-smoke.test.mjs
```

Public registry installation and the extension's standalone registry-based
CI remain separate: this helper is deliberately scoped to sibling-checkout
ur.io acceptance and does not claim that an unpublished registry pin exists.
