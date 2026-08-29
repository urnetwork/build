#!/usr/bin/env bash
# Build the URnetwork Flatpak bundle for one architecture.
#
# By default this runs in Docker because the release host is macOS and Flatpak
# is Linux-only. Dockerfile.flatpak owns the complete Flatpak toolchain; this
# script installs no host packages and requires only Docker, which run.sh
# preflights before doing any release work.
#
# Bubblewrap needs nested user/mount namespaces. Only this purpose-built,
# ephemeral container gets CAP_SYS_ADMIN/CAP_NET_ADMIN plus the two Docker
# security-policy relaxations that permit those namespaces and configure the
# sandbox's loopback device. The source is mounted read-only, the artifact
# directory is the only writable host bind mount, and the daemon and AppImage
# containers keep their normal confinement.
#
# UR_FLATPAK_NATIVE=1 retains a native-Linux path for GitHub Actions, where the
# workflow explicitly installs and preflights flatpak/flatpak-builder/elfutils.
#
# WHY ONE ARCH AND NOT TWO: flatpak-builder has no cross-compile mode. Docker
# can emulate the requested architecture, but building the whole GTK4 stack a
# second time under qemu is deliberately not part of a release. ARCH defaults
# to the release machine's architecture and can be set explicitly.
#
# WHY IT IS SAFE TO RUN AFTER build-linux.sh AND NOT BEFORE: the manifest builds
# the app from `type: dir, path: ../..`, the local tree, and its meson invocation
# needs app/third_party/urnetwork-sdk/<arch>/libURnetworkSdk.so to already be
# there. build-linux.sh puts it there (app/scripts/fetch-deps.sh, on the host).
# Running this first would fail on a missing SDK, so run.sh calls it second and
# this script checks rather than assuming.
#
#   VERSION   release version, stamped into the manifest as -Dapp_version.
#             REQUIRED. make-flatpak.sh falls back to a 0.0.0 dev sentinel when
#             it is unset, and a release must never ship that.
#   LINUX_DIR the linux checkout (default: <build repo>/linux)
#   OUT_DIR   where the .flatpak lands (default: <linux>/out)
#
#   ARCH       amd64 or arm64 (default: this machine's architecture)
#   RUNTIME_VERSION GNOME runtime branch (default: 49)
#   UR_FLATPAK_NATIVE=1 use an already-installed native Linux toolchain instead
#                       of the dependency-complete Docker image
#
# Exits non-zero if it cannot produce a bundle. run.sh treats that as a release
# failure now that the toolchain is reproducibly provided by the container.
#
# SPDX-License-Identifier: MPL-2.0
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LINUX_DIR="${LINUX_DIR:-$(cd "${here}/../.." && pwd)/linux}"
: "${VERSION:?set VERSION (the release version, e.g. 2026.8.22-1026185650)}"
RUNTIME_VERSION="${RUNTIME_VERSION:-49}"
UR_FLATPAK_NATIVE="${UR_FLATPAK_NATIVE:-0}"

[ -d "${LINUX_DIR}" ] || { echo "ERROR: no linux checkout at ${LINUX_DIR}" >&2; exit 1; }
LINUX_DIR="$(cd "${LINUX_DIR}" && pwd)"
maker="${LINUX_DIR}/packaging/make-flatpak.sh"
[ -f "${maker}" ] || {
  echo "ERROR: ${maker} missing — this linux ref predates the flatpak channel." >&2
  echo "       Bump the linux submodule to a ref that has packaging/make-flatpak.sh." >&2
  exit 1
}

case "${ARCH:-}" in
  amd64|arm64) ;;
  '')
    case "$(uname -m)" in
      x86_64)        ARCH=amd64 ;;
      aarch64|arm64) ARCH=arm64 ;;
      *) echo "ERROR: unsupported machine $(uname -m); set ARCH=amd64 or ARCH=arm64" >&2; exit 1 ;;
    esac
    ;;
  *) echo "ERROR: ARCH must be amd64 or arm64 (got '${ARCH}')" >&2; exit 1 ;;
esac

case "${UR_FLATPAK_NATIVE}" in
  0|1) ;;
  *) echo "ERROR: UR_FLATPAK_NATIVE must be 0 or 1" >&2; exit 1 ;;
esac

# The vendored SDK the manifest relocates into the Flatpak's libdir. Checked
# here so the failure names the real cause instead of surfacing as a meson
# error 40 minutes into a runtime download.
sdk_so="${LINUX_DIR}/app/third_party/urnetwork-sdk/${ARCH}/libURnetworkSdk.so"
[ -f "${sdk_so}" ] || {
  echo "ERROR: ${sdk_so} missing." >&2
  echo "       Run all/build-linux.sh first — it stages the cgo SDK that this build vendors." >&2
  exit 1
}

OUT_DIR="${OUT_DIR:-${LINUX_DIR}/out}"
mkdir -p "${OUT_DIR}"
OUT_DIR="$(cd "${OUT_DIR}" && pwd)"
bundle="${OUT_DIR}/URnetwork-${VERSION}-${ARCH}.flatpak"
rm -f "${bundle}"

echo ">>> [${ARCH}/flatpak] building ${bundle##*/} (this downloads the GNOME runtime on a cold host and takes a while)"
if [ "${UR_FLATPAK_NATIVE}" = 1 ]; then
  [ "$(uname -s)" = Linux ] || {
    echo "ERROR: UR_FLATPAK_NATIVE=1 requires a Linux host; omit it to use Docker." >&2
    exit 1
  }
  command -v flatpak >/dev/null || {
    echo "ERROR: UR_FLATPAK_NATIVE=1 but flatpak is not installed." >&2
    exit 1
  }
  if command -v flatpak-builder >/dev/null && ! command -v eu-strip >/dev/null; then
    echo "ERROR: native flatpak-builder requires eu-strip (install elfutils)." >&2
    exit 1
  fi
  case "$(uname -m):${ARCH}" in
    x86_64:amd64|aarch64:arm64|arm64:arm64) ;;
    *) echo "ERROR: native flatpak-builder cannot build ARCH=${ARCH} on $(uname -m)." >&2; exit 1 ;;
  esac
  ( cd "${LINUX_DIR}" && \
      VERSION="${VERSION}" ARCH="${ARCH}" RUNTIME_VERSION="${RUNTIME_VERSION}" OUT_DIR="${OUT_DIR}" \
        bash "${maker}" --bundle )
else
  command -v docker >/dev/null || {
    echo "ERROR: docker is required for the Flatpak build." >&2
    exit 1
  }
  docker info >/dev/null 2>&1 || {
    echo "ERROR: docker is installed but its daemon is not running." >&2
    exit 1
  }

  image="urnetwork-linux-builder-flatpak:${ARCH}"
  runtime_volume="urnetwork-flatpak-runtime-${ARCH}"

  echo ">>> [${ARCH}/flatpak] building dependency-complete Linux image (layer-cached)"
  docker build --platform "linux/${ARCH}" \
    -f "${here}/Dockerfile.flatpak" \
    -t "${image}" "${here}"

  docker volume create "${runtime_volume}" >/dev/null

  # seccomp=unconfined permits clone/unshare; systempaths=unconfined removes
  # Docker's masked-/proc restriction; CAP_SYS_ADMIN permits nested mounts and
  # CAP_NET_ADMIN lets bwrap configure the sandbox's loopback device.
  docker run --rm --platform "linux/${ARCH}" \
    --cap-add SYS_ADMIN \
    --cap-add NET_ADMIN \
    --security-opt seccomp=unconfined \
    --security-opt systempaths=unconfined \
    -v "${LINUX_DIR}:/src:ro" \
    -v "${OUT_DIR}:/out" \
    -v "${runtime_volume}:/var/lib/flatpak" \
    -e VERSION="${VERSION}" \
    -e ARCH="${ARCH}" \
    -e RUNTIME_VERSION="${RUNTIME_VERSION}" \
    "${image}"
fi

# make-flatpak.sh composes this exact name; if the contract ever drifts, fail
# loudly here rather than letting run.sh's nullglob upload nothing in silence.
[ -s "${bundle}" ] || {
  echo "ERROR: make-flatpak.sh reported success but ${bundle} is missing or empty." >&2
  echo "       Bundles present in ${OUT_DIR}:" >&2
  ls -1 "${OUT_DIR}"/*.flatpak 2>/dev/null >&2 || echo "       (none)" >&2
  exit 1
}
bundle_bytes="$(wc -c < "${bundle}" | tr -d '[:space:]')"
echo ">>> [${ARCH}/flatpak] $(basename "${bundle}") — ${bundle_bytes} bytes"
