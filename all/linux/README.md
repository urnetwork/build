# Linux build container

Builds the URnetwork Linux app (`linux/`, C++/GTK4 GUI + headless daemon) into
the release artifacts for **amd64 + arm64**, entirely in Docker on the macOS
build host — no snapd, no LXD, no VM. Per arch (names are **normative** —
`linux/MIGRATION.md` "Artifact filenames"):

```
urnetwork-daemon_<version>_<arch>.deb              # daemon, apt path
urnetwork-daemon-<version>-<arch>.install.tar.gz   # daemon, install.sh path
urnetwork-daemon-<version>.<rpmarch>.rpm           # daemon, dnf/zypper path
URnetwork-<version>-<arch>.AppImage                # GUI (+ .AppImage.zsync update feed)
```

The `.rpm` is the only name that does not carry `<arch>` verbatim: rpm has its
own arch spelling, so `<rpmarch>` is `x86_64`/`aarch64` while the asset arch
stays Debian-spelled everywhere else.

(This replaces the snap pipeline — Linux no longer ships as a `.snap`; see
`linux/MIGRATION.md` + `linux/APPIMAGE.md` for the why and the shape.)

## How it works

- **Two base images per arch**, because the halves cannot share one:
  - `Dockerfile.daemon` — **Ubuntu 22.04**, whose glibc **2.35 IS the floor**
    `linux/packaging/deb/nfpm.yaml` declares (`Depends: libc6 (>= 2.35)`).
    nfpm does not run `dpkg-shlibdeps`, so the build image is the only thing
    coupling that hand-written dependency to reality. Measured on arm64: a
    daemon built on 24.04 references `__isoc23_strtoll@GLIBC_2.38` (via
    nlohmann/json's number parser) and `arc4random@GLIBC_2.36`, so it would
    install on 22.04 and then fail to exec. Carries nfpm + dpkg + systemd +
    the rpm toolchain, **no GTK** (22.04 does not package libgtkmm-4.0 at
    all). 22.04 is independently the right host for the `.rpm`:
    `semodule_package` stamps the SELinux module with the *build* host's
    libsepol version and an older target refuses it, so jammy's libsepol 3.3
    → Fedora's 3.6+ is the safe direction.
  - `Dockerfile.gui` — **Ubuntu 24.04**, the oldest Ubuntu with GTK4 +
    libadwaita, plus `appimagetool`, `linuxdeploy` core, `zsyncmake`, and
    xvfb for the launch test. 24.04 is therefore the **AppImage's** ABI floor
    (glibc/libstdc++/Mesa are host-provided per the excludelist), so the GUI
    needs a 24.04+ host even though the daemon runs on 22.04.

  `linux/app`'s `gui` meson feature exists precisely to let the two halves
  build on different images. The container's arch *is* the target arch:
  - `arm64` → native on Apple Silicon (fast)
  - `amd64` → Docker qemu emulation (slower, but no second machine needed)
- **Bind mounts**: `build.sh` mounts the linux repo root **read-only** at
  `/src`, `OUT_DIR` writable at `/out`, and `build-arch.sh` at
  `/build-arch.sh`. In-container, `/src` is copied to a local `/work` before
  building. (The snapcraft-era reason for that copy — craft-parts wrote
  `user.*` xattrs that Docker's virtiofs rejects — is gone with snapcraft; the
  copy is kept so the root container can never write build junk into the macOS
  checkout, which the `:ro` mount enforces.)
- **SDK**: `build.sh` stages the cgo `URnetworkSdkLinux.zip` into
  `linux/app/third_party/urnetwork-sdk/{amd64,arm64}/` (via the app's
  `scripts/fetch-deps.sh`) before mounting, so the meson build finds
  `libURnetworkSdk.so` + `urnetwork_sdk.hpp`.
- **Packaging scripts live in the linux repo, not here.** `build-arch.sh` runs
  the meson build + `meson install --destdir` into a staging tree, then invokes
  `linux/packaging/{make-deb,make-install-tarball,make-rpm,make-appimage}.sh`
  with `VERSION`, `ARCH`, `STAGING_DIR`, `OUT_DIR`, `APP_DIR`, `SDK_DIR` in the
  environment. A missing script or a wrongly-named artifact fails the build
  loudly — nothing is produced silently. The one exception is the `.rpm`: it is
  warn-and-continue by default (`UR_REQUIRE_RPM=true` gates on it), because the
  `.deb`, the tarball, the AppImage **and** the SDK zip all upload from inside
  one `if build-linux.sh` in `run.sh` — a fatal rpm step would cost the release
  every Linux asset rather than one.
- **The daemon's three packages come out of one staging tree.** `make-deb.sh`,
  `make-install-tarball.sh` and `make-rpm.sh` all run in the same `ROLE=daemon`
  container against the same `meson install --destdir` output (via the linux
  repo's `assemble_daemon_root()`), so they cannot ship different daemons. That
  is why the `.rpm` gets no container of its own: `make-rpm.sh` is nfpm-based,
  and nfpm is already installed here for the `.deb`.
- **The packaging scripts run with the CWD set to `OUT_DIR`.** This is
  load-bearing: `appimagetool` writes its `.zsync` into the *current working
  directory*, not next to the AppImage it was told to produce. With the cwd
  anywhere else the AppImage lands in `OUT_DIR` and the `.zsync` silently does
  not — and the whole update channel depends on it.

## Verification

`verify.sh` runs at the end of each role's container and **fails the build** if
anything is wrong, so bad artifacts are never uploaded. It exists because the
hand-rolled GTK4 AppDir is the piece with a real failure precedent (Gaphor
deleted its GTK4 AppImage in 2023); the things it can get wrong — a missing
compiled GSettings schema, unregistered pixbuf loaders, a library that silently
resolves to the *build host's* copy — all pass a file-exists check and abort on
a user's machine.

Run it standalone against an existing `OUT_DIR`, without rebuilding:

```bash
docker run --rm --platform linux/arm64 \
  -v /path/to/out:/out -v "$PWD/verify.sh:/verify.sh:ro" \
  -v /path/to/linux:/src:ro \
  -e ARCH=arm64 -e VERSION=<version> -e ROLE=gui \
  urnetwork-linux-builder-gui:arm64 bash /verify.sh
```

Checks that genuinely cannot run in a container print a `[SKIP]` line naming
what and why — a real tun test needs `--cap-add NET_ADMIN --device /dev/net/tun`,
and the daemon start / control-socket handshake needs systemd as PID 1. A skip
that looks like a pass is worse than no test.

## Files

| File | Role |
|---|---|
| `Dockerfile.daemon` | `ubuntu:22.04` (the declared glibc floor) + C++ toolchain, **no GTK** + nfpm/dpkg/systemd + rpm/checkpolicy/semodule-utils |
| `Dockerfile.gui` | `ubuntu:24.04` + C++/GTK4 toolchain + appimagetool/linuxdeploy/zsyncmake + xvfb |
| `setup.sh` | **one-time smoke test** — build both containers per arch + verify each toolchain (the Linux analog of `windows/setup.sh`). Run this first. |
| `smoke-test.sh` | run inside a container by `setup.sh`; role-aware (`ROLE=daemon` checks nfpm/dpkg/systemd and asserts GTK is *absent*; `ROLE=gui` checks the GTK4 stack, the AppImage tools, and asserts webkitgtk is *absent*) |
| `build.sh` | host orchestration: stage SDK, `docker build`+`docker run` per arch **per role**, verify the artifact names |
| `build-arch.sh` | in-container per-arch/per-role step: meson build → `meson test` (incl. the glibc-floor gate) → staging tree → the linux repo's packaging scripts → artifact-name asserts → `verify.sh` |
| `verify.sh` | proves the artifacts *work*: AppImage extract + AppDir contents + dependency closure + headless launch under xvfb; `systemd-analyze verify`; `.deb` install/purge lifecycle; `install.sh` tarball round-trip; `.rpm` header metadata + arch tag. Independently runnable. |

## Smoke-test the build env first

```bash
./setup.sh                        # build + smoke-test the native (arm64) container
./setup.sh --arches "amd64 arm64" # both (amd64 is emulated, slower)
```

## Run standalone (outside the release pipeline)

The easy entry point is `../build-linux.sh`, which uses the local branches
as-is, derives the version from the `linux` repo's `v<version>` branch, builds
the cgo SDK zip, and then calls this `build.sh`. To drive `build.sh` directly
instead: Docker `-v` needs absolute paths, so resolve them (BUILD_HOME is the
repo root):

```bash
BUILD_HOME=$(cd ../../.. && pwd) \
LINUX_DIR=$(cd ../../../linux && pwd) \
SDK_ZIP=$(cd ../../../sdk/cgo/build && pwd)/URnetworkSdkLinux.zip \
OUT_DIR=/tmp/urnetwork-linux-out \
VERSION=0.0.1 \
  ./build.sh
# ARCHES="arm64" ./build.sh       # single arch
# ROLES="daemon" ./build.sh       # single half (see below)
# UR_SKIP_VERIFY=1 ./build.sh     # build + package only, skip verification
```

Env knobs: `UR_GLIBC_FLOOR` (daemon floor, default `2.35` — must match
nfpm.yaml's `Depends: libc6`), `UR_GLIBC_CEILING` (the AppImage's own gate,
default `2.39`), `UR_REQUIRE_RPM` (default `false` — make a missing or failed
`.rpm` fatal instead of a warning), `UR_SKIP_VERIFY`, `ARCHES`, `ROLES`.

`ROLES` (default `daemon gui`, the same knob `setup.sh` has always had) picks
which halves to build. It exists so the two can run as separate CI jobs — they
are separate base images and the GUI half is the long pole — and the artifact
assertions are scoped to the roles that actually ran. **A role-scoped
invocation needs its OWN `OUT_DIR`**: the stale-artifact sweep clears the whole
set, not just this role's, so `ROLES=daemon` followed by `ROLES=gui` into one
directory would leave only the AppImage.

`../build-linux.sh` additionally takes `UR_SKIP_SDK_BUILD=1`, meaning "the cgo
SDK output is already staged in `sdk/cgo/build/`; do not rebuild it". That is
how CI builds the SDK once and fans the zip out to every `(role, arch)` leg
without installing Go and zig on each of them. The per-arch `.so` and zip
assertions still run either way, so a leg handed nothing fails exactly as a
failed build would.

Both defaults leave `run.sh`'s path byte-for-byte unchanged.

### On GitHub Actions

`.github/workflows/linux-release.yml` runs exactly the above on hosted runners:
one shared SDK job, then one job per `(role, arch)` — each calling
`all/build-linux.sh` — then a release job that publishes only if every leg is
green. Each arch gets a runner of its own architecture (`ubuntu-24.04` /
`ubuntu-24.04-arm`), so unlike the macOS host nothing is qemu-emulated.

`build/all/build-linux.sh` (run.sh's linux build part) invokes this after the
macOS app build; the resulting artifacts are uploaded to the GitHub release.
**There is no store submission.** Publishing the `.deb` to the apt repo and
re-hosting the AppImage + `.zsync` on the self-hosted update endpoint (GitHub
Releases can't serve the multi-range requests zsync needs —
`linux/APPIMAGE.md` §11f) are manual follow-ups.

## Notes

- Needs Docker with buildx + qemu (Docker Desktop for Mac has both). The
  emulated amd64 pass is the slow part; the baked-in deps keep it from
  re-downloading the toolchain each run. Two images per arch means four image
  builds for a full release, all layer-cached after the first.
- `libwebkitgtk-6.0-dev` is deliberately **not** installed in the GUI image.
  meson links webkitgtk-6.0 whenever it is present (`required : false`, with no
  opt-out option), and `make-appimage.sh` then refuses to package — WebKitGTK
  hardcodes absolute paths to its multi-process helpers, so nothing can
  relocate it into an AppDir. The upgrade sheet falls back to the hosted
  checkout in the system browser, which is the documented decision.
- `appimagetool`/`linuxdeploy` are fetched from their `continuous` tags at
  image-build time (the only channel with aarch64 builds) and extracted, since
  the container has no FUSE. `docker build --no-cache` refreshes them.
- `nfpm` **is** baked into `Dockerfile.daemon` (v2.47, pinned and
  checksum-verified against the release's `checksums.txt` rather than trusting
  goreleaser's apt repo). Both `make-deb.sh` and `make-rpm.sh` are nfpm-based,
  so it is a build dependency, not a convenience. `dpkg` is still installed —
  `verify.sh` needs it for the install/purge lifecycle.
- The `.rpm` is built but **never installed** anywhere in this pipeline.
  `rpm -qp` is a pure file query; the scriptlets (`%post`'s
  `semodule -X 200 -i`, the systemd preset) are unexecuted until a Fedora host
  runs them. A green build here is not a green install.
