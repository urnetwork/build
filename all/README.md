# build/all — release pipeline (`run.sh`)

`run.sh` runs on the macOS (Apple Silicon) build host. Most prerequisites are
checked by the script at startup; the two below are host setup it can't do for
you. See `build/DESKTOP_BUILD.md` for architecture and `windows/README.md` +
`linux/README.md` for the desktop builds.

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

The extension consumes the exact localizations and JavaScript SDK versions
published by the same release. Before editing its lockfile, `run.sh` uses a new
empty npm cache on every readiness attempt to fetch and inspect each exact
package tarball. This prevents an early cached ETARGET/E404 response from
hiding a version after it reaches the registry. Expected ETARGET/exact-tarball
E404 propagation misses retry for at most 60 attempts at 10-second intervals;
authentication, transport, and other registry failures remain immediately
fatal. The bounds can be changed for controlled testing via
`NPM_PACKAGE_READY_MAX_ATTEMPTS` and
`NPM_PACKAGE_READY_RETRY_DELAY_SECONDS`. The subsequent dependency install also
uses a new empty cache on every attempt, because npm's normal cache or another
registry edge can still replay an earlier ETARGET after the independent
readiness probe succeeds. Only an ETARGET naming one of these exact release
dependencies is retried, for at most 6 attempts at 10-second intervals; all
other install failures are immediately fatal. Its controlled-test bounds are
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

The language toolchains (`go 1.26.7`, `java 21`, `node`/`npm`, the Android SDK,
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
  The script is both the host launcher and the container entrypoint (its
  `container-build` phase), and the fdroiddata recipe seds the pinned go
  version out of it — keep the recipe's prebuild path in sync if it moves.
