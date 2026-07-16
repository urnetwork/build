<!-- SPDX-License-Identifier: MPL-2.0 -->
# Building local versions on the target platforms

The `build/all/build-*.sh` scripts build the Linux, Windows, and Android
(F-Droid) apps. They normally build whatever is staged under `BUILD_HOME`
(the release copies `run.sh` checks out). This doc covers using them to build a
**local version from your working tree** — including uncommitted changes — so you
can verify a build on each platform before committing.

> **Host:** the build repo is intended to run on a macOS host with an **Apple
> M1** chip. **M3** is a next target. (Apple M4 currently hits an "invalid
> instruction" error under Docker's Apple virtualization framework — see the
> `build-fdroid.sh` FIXME and https://github.com/golang/go/issues/71434.)

| Platform | Script | Toolchain (macOS host) | Output |
|---|---|---|---|
| Linux | `build-linux.sh` | zig (cgo cross) + Docker (snapcraft) | `*.snap` (amd64, arm64) |
| Windows | `build-windows.sh` | QEMU/HVF Windows VM | `*.msi` (x64, arm64) |
| Android (F-Droid) | `build-fdroid.sh` | Docker (F-Droid buildserver) | github/ungoogle `*.apk` |

For the iOS/macOS xcframework and the Play-store Android aar (the gomobile
bindings, not covered by a `build-*.sh`), see `sdk/build/` — `make build_apple`
and `make build_android` build them from the working tree directly (their
`sdk/build/go.mod` already `replace`s sdk/connect/glog with the local repos).

For the pipeline internals (VM/container plumbing, store submission), see
`build/all/DESKTOP_BUILD.md`.

## Local-repo staging (the shared mechanism)

Each `build-*.sh` builds from `$BUILD_HOME/{sdk,connect,glog,<app>}`. To build
your local working tree instead, point the script at your repos and it rsyncs
them into `BUILD_HOME` first (via `build/all/stage-local-repos.sh`). Set either:

- **`SRC_HOME=<monorepo root>`** — stages every needed repo from `$SRC_HOME/<repo>`
  (e.g. `SRC_HOME=/Users/you/urnetwork`), **or**
- **`SRC_<REPO>=<path>`** per repo (e.g. `SRC_SDK=/path/to/sdk`), which overrides
  `SRC_HOME` for that repo.

Each script stages the repos its cgo/aar build's `go.mod` replaces — **`sdk`,
`connect`, and `glog` (all three, or the module graph mismatches)** — plus its
app repo (`linux` / `windows` / `android`). Only the source tree is copied;
`.git`, `build/`, `out/`, `node_modules/`, and test binaries are skipped, and the
destination's own build outputs and `.git` are preserved.

Because a locally-staged repo usually isn't on a `v<version>` branch, pass
**`EXTERNAL_WARP_VERSION`** for the Linux/Windows builds (they read the version
off a `v<version>` branch otherwise). The F-Droid build takes its version from
`android/app/local.properties` instead (see below).

> Staging **overwrites** the release copies under `BUILD_HOME` with your local
> source. Re-run `run.sh`'s version staging before a real release.

## Prerequisites

- **All:** Docker Desktop running (Linux + F-Droid), Go + gomobile on `PATH`
  (`/usr/local/go/bin`, `~/go/bin`).
- **Linux:** `zig` (`brew install zig`); one-time cgo cross toolchain via
  `(cd sdk/cgo && make init)`.
- **Windows:** QEMU (`brew install qemu`) and the Windows ARM VM image at
  `build/all/windows/output/windows-arm64.qcow2` — built once with
  `build/all/windows/setup.sh` (see `build/all/windows/README.md`).
- **Android (F-Droid):** a signing config at `$WARP_HOME/release`, and the
  `android` repo already configured for the ungoogle flavor (its
  `v<version>-ungoogle` gradle settings + `app/local.properties` with
  `warp.version`/`warp.version_code`). See the `build-fdroid.sh` FIXME re: Apple
  M4 + Docker.

## Linux — snap

Cross-builds the cgo SDK `.so`s natively (zig), then builds the snaps in the
Canonical snapcraft rock Docker container (arm64 native, amd64 under qemu
emulation).

```bash
SRC_HOME=/Users/you/urnetwork \
EXTERNAL_WARP_VERSION=0.0.0-0 \
ARCHES=arm64 \
OUT_DIR=/tmp/linux-out \
  build/all/build-linux.sh
```

- `ARCHES` (default `"amd64 arm64"`) — set `arm64` for a faster native-only
  build; the cgo SDK still cross-builds both arches (only the snap step is
  limited).
- Output: `.snap` files in `OUT_DIR` (default `$BUILD_HOME/out/desktop/linux`).

## Windows — MSI

Everything builds inside the local QEMU/HVF ARM Windows VM: it boots headless,
the build home is rsync'd in, the cgo SDK DLLs build (Go + llvm-mingw), then the
app MSIs (x64 + arm64).

```bash
SRC_HOME=/Users/you/urnetwork \
EXTERNAL_WARP_VERSION=0.0.0-0 \
OUT_DIR=/tmp/windows-out \
  build/all/build-windows.sh
```

- Output: `.msi` files in `OUT_DIR` (default `$BUILD_HOME/out/desktop/windows`).
- This is the heaviest build (VM boot + in-VM Go/MSVC compile). Watch the VM at
  `vnc://127.0.0.1:5901` (password `windows`).

## Android — F-Droid (github/ungoogle APK)

Builds the github/ungoogle APKs in the F-Droid buildserver container (gradle
`buildSdk assembleGithub`), the context F-Droid's reproducible-build check needs.

```bash
WARP_HOME=/Users/you/urnetwork \
SRC_HOME=/Users/you/urnetwork \
  build/all/build-fdroid.sh
```

- `WARP_HOME` is required (the release signing config at `$WARP_HOME/release` is
  mounted into the container).
- The version comes from `android/app/local.properties` (not
  `EXTERNAL_WARP_VERSION`); the staged `android` working tree must carry the
  ungoogle gradle settings + `local.properties` (staging copies it AS-IS).
- Output: APKs in `$BUILD_HOME/android/app/app/build/outputs/apk/github/release/`.

## Common env vars

| Var | Applies to | Meaning |
|---|---|---|
| `SRC_HOME` | all | local monorepo root; stages each needed repo from `$SRC_HOME/<repo>` |
| `SRC_<REPO>` | all | per-repo local source override (e.g. `SRC_SDK`), beats `SRC_HOME` |
| `EXTERNAL_WARP_VERSION` | linux, windows | release version stamp (e.g. `0.0.0-0`); required when staging local |
| `BUILD_HOME` | all | build root (default: `build/`); staging destination |
| `OUT_DIR` | linux, windows | where artifacts land (existing ones are cleared) |
| `ARCHES` | linux | snap arches (default `"amd64 arm64"`) |
| `WARP_HOME` | fdroid | release signing config root |

## Caveats

- Staging overwrites `BUILD_HOME/{sdk,connect,glog,<app>}`; re-stage via `run.sh`
  before a release.
- These are full app builds (snap/MSI/APK), not just compiles — they take
  minutes (Windows the longest). A failure at the app-compile step means your
  code doesn't compile against the SDK; a failure later (packaging/linter) does
  not.
- No `SRC_*` set ⇒ the script builds whatever is already staged in `BUILD_HOME`
  (the normal release path); nothing changes.
