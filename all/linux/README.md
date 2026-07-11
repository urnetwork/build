# Linux snap build container

Builds the URnetwork Linux app (`linux/app`, C++/GTK4) into `.snap` packages for
**amd64 + arm64**, entirely in Docker on the macOS build host — no Launchpad
remote-build, no snapd, no LXD, no VM.

## How it works

- **Base image**: Canonical's official [snapcraft rock](https://github.com/canonical/snapcraft-rocks)
  `ghcr.io/canonical/snapcraft:8_core24` — the supported way to build core24
  snaps in a container. `Dockerfile` extends it only to pre-bake the C++/GTK
  build deps so Snapcraft's per-build apt installs become cache hits.
- **`--destructive-mode`**: Snapcraft builds directly inside the container (safe:
  it mutates only the container). The container's arch *is* the target arch:
  - `arm64` → native on Apple Silicon (fast)
  - `amd64` → Docker qemu emulation (slower, but no second machine needed)
- **Bind mount**: `build.sh` mounts the whole build home (`$BUILD_HOME`) at
  `/build` and builds in `/build/linux/app`, so the snap build sees the exact local
  state `run.sh` set up — all repos, on their correct branches, including any
  sibling repos the build references. (This is the Linux analog of how the Windows
  VM gets its source; there it's an rsync, since a VM can't bind-mount a host dir.)
- **SDK**: `build.sh` stages the cgo `URnetworkSdkLinux.zip` into
  `linux/app/third_party/urnetwork-sdk/{amd64,arm64}/` (via the app's
  `scripts/fetch-deps.sh`) before packing, so the meson build finds
  `libURnetworkSdk.so` + `urnetwork_sdk.hpp`.

## Files

| File | Role |
|---|---|
| `Dockerfile` | snapcraft rock + pre-baked C++/GTK build deps |
| `setup.sh` | **one-time smoke test** — build the container(s) + verify the toolchain (the Linux analog of `windows/setup.sh`). Run this first. |
| `smoke-test.sh` | run inside the container by `setup.sh`: checks snapcraft + meson/g++/pkg-config + gtkmm-4.0/libadwaita/glib/nlohmann-json |
| `build.sh` | host orchestration: stamp version, stage SDK, `docker run` per arch (build home bind-mounted), collect `.snap` |

## Smoke-test the build env first

```bash
./setup.sh                        # build + smoke-test the native (arm64) container
./setup.sh --arches "amd64 arm64" # both (amd64 is emulated, slower)
```

## Run standalone (outside the release pipeline)

Docker `-v` needs absolute paths, so resolve them (BUILD_HOME is the repo root):

```bash
BUILD_HOME=$(cd ../../.. && pwd) \
LINUX_APP_DIR=$(cd ../../../linux/app && pwd) \
SDK_ZIP=$(cd ../../../sdk/cgo/build && pwd)/URnetworkSdkLinux.zip \
OUT_DIR=/tmp/urnetwork-snaps \
VERSION=0.0.1 \
  ./build.sh
# ARCHES="arm64" ./build.sh   # to build a single arch
```

`build/all/run.sh` invokes this after the macOS app build; the resulting `.snap`
files are uploaded to the GitHub release. **Snap Store submission is manual.**

## Notes

- Needs Docker with buildx + qemu (Docker Desktop for Mac has both). The emulated
  amd64 pass is the slow part; the baked-in deps keep it from re-downloading the
  toolchain each run.
- If Snapcraft ever rejects `--build-for` alongside `--destructive-mode` in a
  future release, drop the flag — the container arch already constrains the build
  to a single target.
