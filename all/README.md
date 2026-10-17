# build/all — release pipeline (`run.sh`)

`run.sh` runs on the macOS (Apple Silicon) build host. Most prerequisites are
checked by the script at startup; the two below are host setup it can't do for
you. See `build/DESKTOP_BUILD.md` for architecture and `windows/README.md` +
`linux/README.md` for the desktop builds.

## Exact-version recovery

Use `xops/builder/build.sh --resume <exact-external-version>` only after the
failed invocation reports `release checkpoint sealed`, exits, and finishes
owned cleanup. Normal invocation still starts a full new release. Preparation
(source refresh, candidate checks, versioning/module transforms and initial npm
publication) is deliberately not replayable mid-flight. After its seal, actual
SDK/CLI/app/IP-database/service-image stages retain independent, hash-checked
artifacts, timings and success evidence under the private
`$WARP_HOME/.release-checkpoints/<version>/` tree. This can consume tens of GiB;
there is no automatic deletion of recoverable releases.

Resume preserves the version and verifies the source/index/untracked-input
graph (including standalone MMM and generated SDK module sums), relevant
options/publisher plan, host tool identities/effective Go configuration,
candidate-gate outputs and exact
remote release tags. It does not pull/reset/allocate a new version. Completed
builds restore their declared output paths from verified copies; interrupted
local stages retry. Publication reads reconcile exact GitHub draft IDs, asset
digests, npm tarballs and immutable image manifests. All remaining gates,
migrations and rollout waves still execute. Code changes, missing/corrupt
cache, remote conflicts, unjournaled source mutations and ambiguous image,
Apple or multi-registry uploads fail closed; never edit JSON to mark success.
An interrupted image build/import retains publication intent: even if its tag
is currently absent, a daemon-owned BuildKit job may still push it later.
Resume never starts a second push for that intent. Only observed command
success, a matching registry receipt and a completed checkpoint permit reuse;
lock recovery does not establish BuildKit job termination or clear intent.
The host-tool seal does not make existing container/VM package installations
fully hermetic (for example F-Droid's buildserver tag/apt inputs). Eligible
retried non-publishing container stages still execute their original gates.

Private Go module prefixes and Git SSH rewrites are configured process-locally
before both fresh preparation and the first resume validation. Reapplying this
setup preserves caller settings and does not append duplicate canonical rules.
Effective Go input drift still fails validation; setup never changes a seal.

Exact GitHub reconciliation GETs wait on positively identified primary or
secondary rate limits, honoring reset/Retry-After with at most four attempts
and a 3700-second total retry budget per request. No extra metadata calls or
weaker asset checks are used. Authentication failures, ordinary 403s, invalid
JSON and transport failures stop immediately. This read-only retry does not
replay draft POSTs or finalization PATCHes, change the account's API allowance,
or replace the separate upload helper's explicit-rejection policy.

Before candidate sites, on resume and immediately before production mutations,
read-only preflight checks Main LAN database identity/head/capacity and
Grafana/Brevo authorization, then invokes Warp's exact deployment guard planner
with `--preflight`. The early check permits missing future images only; real
deployments still require them. SQL uses enforced read-only SELECT paths,
never the potentially mutating `db version` helper. Production probes have a
300-second overall deadline (the native helper compile is separately bounded
to 300 seconds). The native database/Grafana/Brevo gate runs first, then at
most two services are checked concurrently. Each service retains Warp's exact
guards, 20 sequential fresh-connection samples per observed block and clean-pass
retry rules; all 13 services must pass. A failure or deadline cancels and reaps
the owned probe groups, including registry/status child processes. Probes do
not announce, promote, initialize selectors, or send messages. Health is
checked again later, not assumed to persist.

Kernel locking covers the process group, including FD-closing descendants via
a separate reaper. Detached VMs/daemon containers require cleanup inventory:
an uncertain exit blocks a new writer until
`python3 build/all/release-state.py recover-lock "$WARP_HOME"` verifies no
recorded group/workspace-CWD/QEMU/writable-Docker-bind writer. Never remove the
lock file or kill unrelated work. The fuller commands, limitations and
Sol/medium triage versus Astra/max repair policy are in
`xops/builder/BUILD.md`. Private state/log modes do not propagate umask 077 into
Apple artifact builds; children use 022 and retained signing modes are checked.

Focused hermetic verification (no provider writes):

```sh
cd build
GO111MODULE=auto GOMAXPROCS=2 go test -p 1 ./all
cd all
python3 -B -m unittest -v test_release_recovery.py test_release_sources.py test_release_entrypoint.py test_release_preflight.py
```

The canonical-entrypoint fixture runs unchanged copies of both production
scripts against real disposable Git/state/cache boundaries and fake host/
provider commands, with genuine read-only `go env` probes: a failed mobile SDK
stage retries while the passed SDK and all three completed CLI stages are
reused. Changed effective Go inputs, adjacent source changes and corrupt
retained outputs still refuse reuse. It does not claim an end-to-end real
release or deployment.

The final MMM image build sets `UR_PRESERVE_NETWORK_STATS=1` only for that
command. Candidate checks still refresh and verify the network snapshot before
it is committed and sealed; the final rebuild consumes those same bytes. The
cross-repository regression executes the canonical command through a real
Warpctl, the real nested Makefiles and the real statistics generator, with
synthetic feed/compiler/package/image boundaries and no provider writes. Run
from the build repository root:

```sh
python3 -B all/mmm_release_inputs_test.py --warpctl /path/to/warpctl --mmm /path/to/mmm -v
```

This fixture requires those inputs explicitly (no skipped coverage) and checks
candidate refresh, frozen final inputs, and failure gates before publication.

## Required release components

Every enabled component and attempted release-input generator in `run.sh` is a
release gate. iOS/macOS build, signing/export, validation, store upload, and GitHub
upload failures stop the release; missing, empty, or stale Apple exports cannot
stand in for a new build. Windows/Linux must produce all outputs for their existing
selected architecture/role plans, including RPM, Arch packages, and the existing
single-architecture Flatpak. Every GitHub artifact must be a nonempty regular file.

Notifications and best-effort VirusTotal observations are not build components.
`BUILD_TEST`, `CONNECT_IP_UPDATE`, and `WARP_SKIP_DEPLOY` keep their explicit
opt-in/skip behavior; commented-out build targets remain disabled.

The GitBook `docs` repository is retired and is no longer a release submodule.
The current documentation and legal sources live in `$WARP_HOME/mmm/ur.io/docs`.
The mmm service's build (`$WARP_HOME/mmm/mmm`, the nginx front for the sites
whose source lives in mmm) reads that corpus for ur.io; documentation
generation and the web and mmm checks remain required. Historical release tags
retain their original `docs` submodule pins.

The mmm service builds from `$WARP_HOME/mmm` on main, with no release branch,
beside the web build. Its Go module's warp replacement is pinned to this
release's `warp` submodule through a temporary workspace. The rollout deploys
mmm first in every wave, so no load balancer routes the mmm hostnames before
mmm's blocks run.

The extension consumes the exact localizations version published by the same
release. It has no npm SDK dependency: it builds against this run's `sdk/js`
checkout. Before editing its lockfile, `run.sh` uses a new empty npm cache on
every readiness attempt to fetch and inspect the exact package tarball. This
prevents an early cached ETARGET/E404 response from hiding a version after it
reaches the registry. Expected ETARGET/exact-tarball
E404 propagation misses retry for at most 60 attempts at 10-second intervals;
authentication, transport, and other registry failures remain immediately
fatal. The bounds can be changed for controlled testing via
`NPM_PACKAGE_READY_MAX_ATTEMPTS` and
`NPM_PACKAGE_READY_RETRY_DELAY_SECONDS`. The subsequent dependency install also
uses a new empty cache on every attempt, because npm's normal cache or another
registry edge can still replay an earlier ETARGET after the independent
readiness probe succeeds. Only an ETARGET naming that exact release dependency
is retried, for at most 6 attempts at 10-second intervals; all other install
failures are immediately fatal. Its controlled-test bounds are
`NPM_INSTALL_READY_MAX_ATTEMPTS` and
`NPM_INSTALL_READY_RETRY_DELAY_SECONDS`. npm acknowledges a publish with HTTP
202 before asynchronous processing finishes, so the localizations publisher
also owns this readiness proof. If an accepted job is still absent after the
full window, it resubmits the same immutable version once and proves readiness
again. Hard failures are not resubmitted; `NPM_PUBLISH_READY_MAX_ATTEMPTS`
changes the accepted-publication bound only for controlled testing.

## Passwordless sudo (required when tests are enabled)

When `BUILD_TEST` is set, `run.sh` brings up a local test environment
(`server/local/run-local.sh`) around the test phase. That script uses `sudo` to
add a loopback alias and edit `/etc/hosts` — in an unattended build a password
prompt would hang, so the build user needs passwordless sudo:

```bash
sudo visudo -f /etc/sudoers.d/urnetwork-build
# add one line (replace <user> with the output of `whoami`):
#   <user> ALL=(ALL) NOPASSWD: ALL
```

`visudo` syntax-checks and writes the file `0440` root-owned (never edit
`/etc/sudoers` directly — a syntax error there locks you out of sudo). macOS's
`/etc/sudoers` already ends with `@includedir /private/etc/sudoers.d`, so it takes
effect immediately. Verify in a new shell: `sudo -n true && echo ok`.

> This gives that user root with no prompt — fine for a dedicated build machine,
> not a personal daily-driver. Not needed if you never set `BUILD_TEST`.

## Homebrew packages

The desktop app bundles build locally via virtualization:

```bash
brew install qemu            # Windows MSI build — local ARM Windows VM (HVF)
brew install gnu-sed         # run.sh uses BUILD_SED=gsed (GNU sed)
```

- **qemu** — the Windows build (`build/all/windows`) boots a local ARM Windows VM
  (driven directly, not Packer). Run `windows/setup.sh` once to install + smoke-test
  the image (needs a Win11 ARM64 ISO + `virtio-win.iso`).
- **Docker** (Docker Desktop) — the Linux deb/tarball/AppImage artifacts build
  in a container (`build/all/linux`), and the local test env above also needs it.
  Usually already installed; otherwise `brew install --cask docker`. Run
  `linux/setup.sh` once to build + smoke-test the Linux builder container.

The language toolchains (`go 1.27.2`, `java 21`, `node`/`npm`, the Android SDK,
`warpctl`) are validated by `run.sh` at startup — install those per the main build
docs.

## Desktop-build env vars

Full list in the header of `run.sh`. Desktop-specific:

- `WINDOWS_ISO` / `VIRTIO_ISO` — only for the **first** Windows image build
  (reused after; `windows/setup.sh` can also build it out of band).

## Rebuilding just one platform's artifacts

`build-windows.sh`, `build-linux.sh`, and `build-fdroid.sh` are the extracted
build parts of `run.sh`. They use the local branches AS-IS — no pulls, no
checkouts, no version staging — so after a release run has configured the
version branches, re-run one standalone, e.g. when a VM or container build
flaked. Uploading to the GitHub release stays in `run.sh`.

- `build-windows.sh` / `build-linux.sh` — the cgo SDK zip + the app bundles
  (MSIs / debs+install-tarballs+AppImages). The version comes off the `windows`/`linux` repo's
  `v<version>` branch (or `EXTERNAL_WARP_VERSION` / `WARP_VERSION` from the
  env); artifacts land in `${BUILD_OUT:-<build home>/out}/desktop/` (override
  with `OUT_DIR`).
- `build-fdroid.sh` — the android github/ungoogle flavor APKs, built in the
  F-Droid buildserver container. Needs `WARP_HOME` and the android repo on its
  `v<version>-ungoogle` branch; APKs land in the android gradle outputs dir.
  Its fixed 12 GiB memory cap fits the host's Docker minimum of 16 GiB. The
  previous 8 GiB cap could stall SDK compilation in reclaim with OOM kills
  disabled even when the Docker VM had spare memory.
  The script is both the host launcher and the container entrypoint (its
  `container-build` phase), and the fdroiddata recipe seds the pinned go
  version out of it — keep the recipe's prebuild path in sync if it moves.
  run.sh pushes `v<version>-fdroid` (on the `v<version>-ungoogle` commit) only
  after the github APKs in the `v<version>`, `+2` and `+3` releases are
  published. F-Droid should track `UpdateCheckMode: Tags .+-fdroid$`; the plain
  and `-ungoogle` tags are pushed before the builds and can lack APKs.
