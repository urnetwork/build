#!/usr/bin/env bash
# Build the URnetwork Windows artifacts from the LOCAL working tree: the cgo SDK
# zip (sdk/cgo cross-build, native on this macOS host) and the app MSIs (x64 +
# arm64, built in the local QEMU/HVF ARM Windows VM via windows/build.sh).
#
# This is the windows build part of run.sh, extracted so it can also run
# standalone. It uses the local branches AS-IS — no pulls, no checkouts, no
# version staging — and assumes run.sh (or the operator) already configured
# every repo on the correct version branch. Standalone, run it to (re)build the
# windows artifacts without a release, e.g. after a flaky VM build.
#
# Inputs (env, all optional):
#   BUILD_HOME             build home (default: this script's parent dir)
#   EXTERNAL_WARP_VERSION  release version, e.g. 2026.7.6-985989570 (default:
#                          from the v<version> branch of $BUILD_HOME/windows)
#   WARP_VERSION           internal version, e.g. 2026.7.6+985989570 (default:
#                          EXTERNAL_WARP_VERSION with the last '-' as a '+')
#   OUT_DIR                where the .msi files land; existing .msi files in it
#                          are removed so the caller never picks up stale ones
#                          (default: ${BUILD_OUT:-$BUILD_HOME/out}/desktop/windows)
#
# SPDX-License-Identifier: MPL-2.0
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export BUILD_HOME="${BUILD_HOME:-$(dirname "$here")}"

# The local branches are the source of truth: when the caller doesn't pass the
# version (run.sh exports it), read it off the windows repo's v<version> branch.
if [ -z "${EXTERNAL_WARP_VERSION:-}" ]; then
  branch="$(git -C "$BUILD_HOME/windows" branch --show-current)"
  case "$branch" in
    v?*) EXTERNAL_WARP_VERSION="${branch#v}" ;;
    *)
      echo "ERROR: set EXTERNAL_WARP_VERSION or put $BUILD_HOME/windows on its v<version> branch (currently: ${branch:-detached})" >&2
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

OUT_DIR="${OUT_DIR:-${BUILD_OUT:-$BUILD_HOME/out}/desktop/windows}"
mkdir -p "$OUT_DIR"
rm -f "$OUT_DIR"/*.msi

# SDK desktop library — cross-builds natively on this macOS host (mingw-w64 +
# llvm-mingw). One-time toolchain install: (cd sdk/cgo && make init)
echo ">>> building the windows cgo sdk ($WARP_VERSION)"
(cd "$BUILD_HOME/sdk/cgo" && WARP_VERSION="$WARP_VERSION" make build_windows)

# App MSIs — built in the local QEMU ARM Windows VM (image built once by
# windows/setup.sh, booted here as a CoW overlay). See windows/README.md.
echo ">>> building the windows app MSIs ($EXTERNAL_WARP_VERSION)"
SDK_ZIP="$BUILD_HOME/sdk/cgo/build/URnetworkSdkWindows.zip" \
OUT_DIR="$OUT_DIR" \
VERSION="$EXTERNAL_WARP_VERSION" \
    "$here/windows/build.sh"
