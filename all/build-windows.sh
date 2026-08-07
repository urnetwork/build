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

# Optionally stage local working-tree repos over the build root so this builds
# LOCAL (possibly uncommitted) changes. No-op unless SRC_HOME / SRC_<REPO> is set
# (release builds via run.sh stage BUILD_HOME themselves and pass no SRC_*).
# shellcheck source=stage-local-repos.sh
# sdk/cgo/go.mod replaces sdk, connect, AND glog with local paths, so all three
# must be staged together or the module graph mismatches (a stale glog vs the
# staged sdk breaks resolution).
source "$here/stage-local-repos.sh"
stage_local_repos sdk connect glog goidenticons windows

# Say plainly WHICH tree is about to be built. This script builds
# $BUILD_HOME/windows — a release-staged checkout — and stage_local_repos is a
# no-op until SRC_HOME/SRC_WINDOWS is set, so a bare invocation silently
# compiles committed-only code and misses every uncommitted change. That is
# invisible in the output otherwise, and the VM build is slow enough that
# finding out afterwards is expensive.
if [ -n "${SRC_HOME:-}${SRC_WINDOWS:-}" ]; then
  echo ">>> source: LOCAL working tree (${SRC_WINDOWS:-${SRC_HOME}/windows}) staged into $BUILD_HOME/windows"
else
  echo ">>> source: $BUILD_HOME/windows (release-staged checkout — NOT your working tree)" >&2
  echo "    to build local, possibly uncommitted changes instead:" >&2
  echo "      SRC_HOME=\$HOME/urnetwork EXTERNAL_WARP_VERSION=0.0.0-0 $0" >&2
  echo "    (or just run windows/build.sh, which sets SRC_HOME for you)" >&2
fi

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

# The module graph must already be prepared: this script BUILDS and never
# modifies the sources it is handed, and the rsync into the VM is a straight
# copy — whatever go.mod/go.sum is here is what the VM compiles. `go mod tidy`
# belongs to run.sh's version staging, upstream of here, so a build can never
# silently move a dependency version.
#
# Catch it on THIS side of the rsync: inside the VM the same problem costs a
# full sync plus a Go build before surfacing as the opaque
# "go: updates to go.mod needed" / "go build failed for windows/amd64".
if [ ! -f "$BUILD_HOME/sdk/cgo/go.sum" ]; then
  {
    echo "ERROR: $BUILD_HOME/sdk/cgo/go.sum is missing."
    echo "       It is git-ignored and generated, normally by run.sh's version"
    echo "       staging. This script does not modify sources, so prepare the"
    echo "       module graph first:"
    echo ""
    echo "         (cd $BUILD_HOME/sdk/cgo && go mod tidy)"
    echo ""
    echo "       Review the go.mod diff before you keep it — a tidy here also"
    echo "       upgrades indirect deps (quic-go, gvisor, x/crypto, …)."
  } >&2
  exit 1
fi

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
