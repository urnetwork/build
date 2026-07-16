#!/usr/bin/env bash
# Build the URnetwork Linux artifacts from the LOCAL working tree: the cgo SDK
# zip (sdk/cgo cross-build via zig, native on this macOS host) and the snaps
# (amd64 + arm64, Canonical snapcraft rock container via linux/build.sh).
#
# This is the linux build part of run.sh, extracted so it can also run
# standalone. It uses the local branches AS-IS — no pulls, no checkouts, no
# version staging — and assumes run.sh (or the operator) already configured
# every repo on the correct version branch. Standalone, run it to (re)build the
# linux artifacts without a release, e.g. after a flaky container build.
#
# Inputs (env, all optional):
#   BUILD_HOME             build home (default: this script's parent dir)
#   EXTERNAL_WARP_VERSION  release version, e.g. 2026.7.6-985989570 (default:
#                          from the v<version> branch of $BUILD_HOME/linux)
#   WARP_VERSION           internal version, e.g. 2026.7.6+985989570 (default:
#                          EXTERNAL_WARP_VERSION with the last '-' as a '+')
#   OUT_DIR                where the .snap files land; existing .snap files in it
#                          are removed so the caller never picks up stale ones
#                          (default: ${BUILD_OUT:-$BUILD_HOME/out}/desktop/linux)
#   ARCHES                 forwarded to linux/build.sh (default "amd64 arm64")
#
# SPDX-License-Identifier: MPL-2.0
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export BUILD_HOME="${BUILD_HOME:-$(dirname "$here")}"

# Optionally stage local working-tree repos over the build root so this builds
# LOCAL (possibly uncommitted) changes. No-op unless SRC_HOME / SRC_<REPO> is set
# (release builds via run.sh stage BUILD_HOME themselves and pass no SRC_*).
# shellcheck source=stage-local-repos.sh
# sdk/cgo/go.mod replaces sdk, connect, AND glog with local paths, so all three
# must be staged together or the module graph mismatches (a stale glog vs the
# staged sdk breaks resolution).
source "$here/stage-local-repos.sh"
stage_local_repos sdk connect glog linux

# The local branches are the source of truth: when the caller doesn't pass the
# version (run.sh exports it), read it off the linux repo's v<version> branch.
if [ -z "${EXTERNAL_WARP_VERSION:-}" ]; then
  branch="$(git -C "$BUILD_HOME/linux" branch --show-current)"
  case "$branch" in
    v?*) EXTERNAL_WARP_VERSION="${branch#v}" ;;
    *)
      echo "ERROR: set EXTERNAL_WARP_VERSION or put $BUILD_HOME/linux on its v<version> branch (currently: ${branch:-detached})" >&2
      exit 1
      ;;
  esac
fi
if [ -z "${WARP_VERSION:-}" ]; then
  case "$EXTERNAL_WARP_VERSION" in
    # <base>-<version_code> -> <base>+<version_code>
    *-*) WARP_VERSION="${EXTERNAL_WARP_VERSION%-*}+${EXTERNAL_WARP_VERSION##*-}" ;;
    *) WARP_VERSION="$EXTERNAL_WARP_VERSION" ;;
  esac
fi
export EXTERNAL_WARP_VERSION WARP_VERSION

OUT_DIR="${OUT_DIR:-${BUILD_OUT:-$BUILD_HOME/out}/desktop/linux}"
mkdir -p "$OUT_DIR"
rm -f "$OUT_DIR"/*.snap

# SDK desktop library — cross-builds natively on this macOS host (zig cc,
# pinning the glibc floor). One-time toolchain install: (cd sdk/cgo && make init)
#
# sdk/cgo/go.sum is git-ignored and generated (run.sh regenerates it at version
# staging via `go mod tidy`). A standalone build from main skips staging, so it
# can be absent — regenerate it here or the cgo `go build` fails with "missing
# go.sum entry". `go mod download all` is build-complete and leaves the tracked
# go.mod untouched.
if [ ! -f "$BUILD_HOME/sdk/cgo/go.sum" ]; then
  echo ">>> sdk/cgo/go.sum missing — generating it (go mod download all)"
  (cd "$BUILD_HOME/sdk/cgo" && PATH="$PATH:/usr/local/go/bin:$HOME/go/bin" go mod download all)
fi

echo ">>> building the linux cgo sdk ($WARP_VERSION)"
(cd "$BUILD_HOME/sdk/cgo" && WARP_VERSION="$WARP_VERSION" make build_linux)

# make chains recipe commands with ';', so a failed cross-compile does not stop
# the zip step — verify both .so's exist before feeding the snap build (a partial
# zip would otherwise sail through to a broken snap).
for a in amd64 arm64; do
  so="$BUILD_HOME/sdk/cgo/build/linux/$a/libURnetworkSdk.so"
  [ -f "$so" ] || { echo "ERROR: $so not built — is the linux cross toolchain installed? (cd sdk/cgo && make init)" >&2; exit 1; }
done

# Snaps — built per arch in the snapcraft rock container (--destructive-mode);
# arm64 native, amd64 under qemu emulation. See linux/README.md.
echo ">>> building the linux snaps ($EXTERNAL_WARP_VERSION)"
LINUX_APP_DIR="$BUILD_HOME/linux/app" \
SDK_ZIP="$BUILD_HOME/sdk/cgo/build/URnetworkSdkLinux.zip" \
OUT_DIR="$OUT_DIR" \
VERSION="$EXTERNAL_WARP_VERSION" \
    "$here/linux/build.sh"
