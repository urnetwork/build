#!/usr/bin/env bash
# Build the URnetwork Windows artifacts from the LOCAL working tree, entirely
# inside the local QEMU/HVF ARM Windows VM (via all/windows/build.sh): the cgo
# SDK DLLs (sdk/cgo, built natively with Go + llvm-mingw) AND the app MSIs (x64 +
# arm64). The build home is rsync'd into the VM, so it builds the exact local
# state — the mac needs no Windows cross-toolchain.
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

# Everything builds inside the QEMU ARM Windows VM (image built once by
# all/windows/setup.sh, booted here as a CoW overlay): build.sh rsyncs the build
# home in, builds the cgo SDK DLLs natively (windows/build-sdk.ps1, Go +
# llvm-mingw) and pulls the SDK zip back to sdk/cgo/build/ (so run.sh uploads
# it), then builds the app MSIs (windows/app/build.ps1). See all/windows/README.md.
echo ">>> building the windows cgo SDK + app MSIs in the VM ($EXTERNAL_WARP_VERSION)"
OUT_DIR="$OUT_DIR" \
VERSION="$EXTERNAL_WARP_VERSION" \
SDK_VERSION="$WARP_VERSION" \
    "$here/windows/build.sh"
