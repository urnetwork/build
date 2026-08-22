#!/usr/bin/env bash
# Build the URnetwork Linux artifacts from the LOCAL working tree: the cgo SDK
# zip (sdk/cgo cross-build via zig, native on this macOS host) and, per arch
# (amd64 + arm64, Ubuntu 24.04 container via linux/build.sh):
#   urnetwork-daemon_<version>_<arch>.deb
#   urnetwork-daemon-<version>-<arch>.install.tar.gz
#   urnetwork-daemon-<version>.<rpmarch>.rpm (rpmarch = x86_64|aarch64)
#   URnetwork-<version>-<arch>.AppImage (+ .AppImage.zsync)
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
#   OUT_DIR                where the deb/tarball/rpm/AppImage artifacts land;
#                          existing ones in it are removed so the caller never
#                          picks up stale ones
#                          (default: ${BUILD_OUT:-$BUILD_HOME/out}/desktop/linux)
#   ARCHES                 forwarded to linux/build.sh (default "amd64 arm64")
#   ROLES                  forwarded to linux/build.sh: which halves to build,
#                          subset of "daemon gui" (default both). See that
#                          script's header — it is what lets a CI run the two
#                          halves as separate jobs.
#   UR_REQUIRE_RPM         forwarded: make a missing/failed .rpm fatal
#                          (default false — warn and carry on)
#   UR_SKIP_SDK_BUILD      1 = the cgo SDK output is ALREADY in
#                          sdk/cgo/build/ (linux/<arch>/libURnetworkSdk.so +
#                          URnetworkSdkLinux.zip); do not rebuild it. Default
#                          0 — the release host always builds it here.
#
#                          It exists for CI (build/.github/workflows/
#                          linux-release.yml), which builds the SDK ONCE in a
#                          shared job and hands the zip to every (role, arch)
#                          leg: four legs rebuilding a byte-identical .so would
#                          each need Go, the zig cross toolchain and the 253 MB
#                          sdk submodule, for ~2 minutes of duplicated work.
#                          The output assertions below still run either way,
#                          and THEY are what gates the packaging step — a
#                          skipped build with nothing staged fails exactly as a
#                          failed build does.
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
stage_local_repos sdk connect glog goidenticons linux

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

# Preflight the packaging scripts HERE, before the (expensive) container image
# build, and diagnose the overwhelmingly common cause: this pipeline builds
# $BUILD_HOME/linux, a release-staged checkout, NOT your working tree. Anything
# uncommitted — which packaging/ is, while the AppImage migration lands — is
# absent there unless stage_local_repos copied it in, and that is a no-op until
# SRC_HOME/SRC_LINUX is set. Without this check the failure surfaces deep inside
# the container as "the linux repo is missing packaging scripts", which reads as
# "they were never written" rather than "they were never staged".
for _p in packaging/make-deb.sh packaging/make-install-tarball.sh packaging/make-appimage.sh; do
  [ -f "$BUILD_HOME/linux/$_p" ] && continue
  {
    echo "ERROR: $BUILD_HOME/linux/$_p is missing."
    if [ -f "${SRC_HOME:-/nonexistent}/linux/$_p" ] || [ -f "${SRC_LINUX:-/nonexistent}/$_p" ]; then
      echo "       It EXISTS in the local source you pointed at, so staging did not copy it."
      echo "       Check the rsync excludes in all/stage-local-repos.sh."
    else
      echo "       This pipeline builds \$BUILD_HOME/linux (a release-staged checkout),"
      echo "       not your working tree. Uncommitted files are only present there if"
      echo "       stage_local_repos copied them in, which needs SRC_HOME or SRC_LINUX:"
      echo ""
      echo "         SRC_HOME=\$HOME/urnetwork EXTERNAL_WARP_VERSION=0.0.0-0 $0"
      echo ""
      echo "       (EXTERNAL_WARP_VERSION is required when staging local sources: the"
      echo "       auto-detect reads a v<version> branch, which a working tree is not on.)"
      echo "       Otherwise commit + push the linux repo so the release checkout has it."
    fi
  } >&2
  exit 1
done

# make-rpm.sh is preflighted here too, but SOFTLY — deliberately unlike the
# three above. It is the newest script in linux/packaging, so a checkout that
# legitimately predates it must still produce the artifacts it does have. The
# asymmetry matters because of where run.sh's uploads sit: they are all inside
# the `then` branch of one `if build-linux.sh`, so a hard exit here would cost
# the release its .deb, its tarball, its AppImage AND the SDK zip over one
# missing file. UR_REQUIRE_RPM=true restores the gate, which is what the linux
# repo's own CI runs with.
if [ ! -f "$BUILD_HOME/linux/packaging/make-rpm.sh" ]; then
  if [ "${UR_REQUIRE_RPM:-false}" = true ]; then
    {
      echo "ERROR: $BUILD_HOME/linux/packaging/make-rpm.sh is missing and UR_REQUIRE_RPM=true."
      echo "       Same cause as the block above: this pipeline builds \$BUILD_HOME/linux,"
      echo "       a release-staged checkout, NOT your working tree — see SRC_HOME/SRC_LINUX."
    } >&2
    exit 1
  fi
  {
    echo "WARNING: $BUILD_HOME/linux/packaging/make-rpm.sh is missing — this build will"
    echo "         produce no .rpm. (Same staging caveat as above: \$BUILD_HOME/linux is a"
    echo "         release-staged checkout, not your working tree.)"
    echo "         Set UR_REQUIRE_RPM=true to make this fatal instead."
  } >&2
fi

if [ ! -f "$BUILD_HOME/linux/packaging/make-arch.sh" ]; then
  if [ "${UR_REQUIRE_ARCH_PKG:-false}" = true ]; then
    {
      echo "ERROR: $BUILD_HOME/linux/packaging/make-arch.sh is missing and UR_REQUIRE_ARCH_PKG=true."
      echo "       Same cause as the block above: this pipeline builds \$BUILD_HOME/linux,"
      echo "       a release-staged checkout, NOT your working tree — see SRC_HOME/SRC_LINUX."
    } >&2
    exit 1
  fi
  {
    echo "WARNING: $BUILD_HOME/linux/packaging/make-arch.sh is missing — this build will"
    echo "         produce no .pkg.tar.zst. (Same staging caveat as above: \$BUILD_HOME/linux is a"
    echo "         release-staged checkout, not your working tree.)"
    echo "         Set UR_REQUIRE_ARCH_PKG=true to make this fatal instead."
  } >&2
fi

OUT_DIR="${OUT_DIR:-${BUILD_OUT:-$BUILD_HOME/out}/desktop/linux}"
mkdir -p "$OUT_DIR"
# Clear stale artifacts of every type this build produces (run.sh globs OUT_DIR
# to upload, so anything left here would sail into the release).
#
# Includes the .sha256/.asc SIDECARS: they carry the version in their name, so
# a version change orphans the previous set rather than overwriting it, and an
# orphaned checksum that no longer matches any artifact is worse than none.
# Also sweeps *.snap — nothing produces those since the AppImage migration, but
# a pre-migration tree still has them sitting next to the real artifacts.
rm -f "$OUT_DIR"/*.deb "$OUT_DIR"/*.install.tar.gz "$OUT_DIR"/*.rpm \
    "$OUT_DIR"/*.pkg.tar.zst \
      "$OUT_DIR"/*.AppImage "$OUT_DIR"/*.AppImage.zsync \
      "$OUT_DIR"/*.sha256 "$OUT_DIR"/*.asc \
      "$OUT_DIR"/*.snap

# SDK desktop library — cross-builds natively on this macOS host (zig cc,
# pinning the glibc floor). One-time toolchain install: (cd sdk/cgo && make init)
#
# This script BUILDS; it must never modify the sources it is handed. Module
# preparation (`go mod tidy` for the sibling-replace graph, which regenerates
# the git-ignored sdk/cgo/go.sum) belongs to run.sh's version staging, upstream
# of here — so a build can never silently move a dependency version, and the
# artifact always corresponds to the tree as given. Check and fail with the
# command to run; do not run it.
#
# UR_SKIP_SDK_BUILD=1 short-circuits the gate TOGETHER WITH the build, and that
# pairing is deliberate: with nothing to compile there is no module graph to
# prepare, and demanding go.sum anyway would force every CI leg to check out the
# 253 MB sdk submodule for a file it never opens.
if [ "${UR_SKIP_SDK_BUILD:-0}" = 1 ]; then
  echo ">>> UR_SKIP_SDK_BUILD=1 — using the sdk/cgo/build output already staged"
  echo "    (the per-arch .so and zip assertions below still run)"
elif [ ! -f "$BUILD_HOME/sdk/cgo/go.sum" ]; then
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
else
  echo ">>> building the linux cgo sdk ($WARP_VERSION)"
  (cd "$BUILD_HOME/sdk/cgo" && WARP_VERSION="$WARP_VERSION" make build_linux)
fi

# make chains recipe commands with ';', so a failed cross-compile does not stop
# the zip step — verify both .so's exist before feeding the packaging build (a
# partial zip would otherwise sail through to broken artifacts).
for a in amd64 arm64; do
  so="$BUILD_HOME/sdk/cgo/build/linux/$a/libURnetworkSdk.so"
  if [ ! -f "$so" ]; then
    echo "ERROR: $so not built." >&2
    if [ "${UR_SKIP_SDK_BUILD:-0}" = 1 ]; then
      echo "       UR_SKIP_SDK_BUILD=1, so nothing here built it: whatever staged" >&2
      echo "       sdk/cgo/build did not produce this arch. Check the job that did." >&2
    else
      echo "       Is the linux cross toolchain installed? (cd sdk/cgo && make init)" >&2
    fi
    exit 1
  fi
done

# The zip is the LAST command in the same ';'-chained recipe, so it can fail
# while every .so above succeeds and make still exits 0 — and it, not the .so
# files, is what build.sh actually hands to fetch-deps.sh. Assert it for the
# same reason the loop above exists.
SDK_ZIP="$BUILD_HOME/sdk/cgo/build/URnetworkSdkLinux.zip"
if [ ! -s "$SDK_ZIP" ]; then
  echo "ERROR: $SDK_ZIP is missing or empty — the sdk zip step did not run" >&2
  exit 1
fi

# Deb + install tarball + rpm + AppImage — built per arch in the Ubuntu
# containers (meson build + the linux repo's packaging scripts); arm64 native,
# amd64 under qemu emulation. The .deb, the tarball and the .rpm all come out
# of the same staging tree in the same ROLE=daemon container, so they cannot
# ship different daemons. See linux/README.md.
echo ">>> building the linux deb/tarball/rpm/AppImage artifacts ($EXTERNAL_WARP_VERSION)"
LINUX_DIR="$BUILD_HOME/linux" \
SDK_ZIP="$SDK_ZIP" \
OUT_DIR="$OUT_DIR" \
VERSION="$EXTERNAL_WARP_VERSION" \
    "$here/linux/build.sh"
