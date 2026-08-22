#!/usr/bin/env bash
# Build the URnetwork Flatpak bundle for THIS machine's architecture.
#
# WHY THIS RUNS ON THE HOST, not in a container like every other linux artifact:
# flatpak-builder needs bubblewrap, which needs unprivileged user namespaces.
# all/linux/build.sh runs `docker run` with no --privileged and no
# --security-opt, so bwrap cannot set up its sandbox in there. Granting the
# desktop build container CAP_SYS_ADMIN to produce one artifact is a much worse
# trade than running this one step outside it.
#
# WHY ONE ARCH AND NOT TWO: flatpak-builder builds for the machine it runs on;
# it has no cross-compile mode. The other artifacts get an arm64 variant because
# `docker run --platform linux/arm64` emulates one. Running an entire GTK4 stack
# build under qemu would take hours, so the arm64 Flatpak is deliberately not
# built here. That is a gap, and it is a stated one: arm64 users have the
# AppImage and the native packages. See linux/APPIMAGE.md for the channel matrix.
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
# Exits non-zero if it cannot produce a bundle. run.sh wraps the call so a
# missing flatpak toolchain costs the release its Flatpak and nothing else.
#
# SPDX-License-Identifier: MPL-2.0
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LINUX_DIR="${LINUX_DIR:-$(cd "${here}/../.." && pwd)/linux}"
: "${VERSION:?set VERSION (the release version, e.g. 2026.8.22-1026185650)}"

[ -d "${LINUX_DIR}" ] || { echo "ERROR: no linux checkout at ${LINUX_DIR}" >&2; exit 1; }
maker="${LINUX_DIR}/packaging/make-flatpak.sh"
[ -f "${maker}" ] || {
  echo "ERROR: ${maker} missing — this linux ref predates the flatpak channel." >&2
  echo "       Bump the linux submodule to a ref that has packaging/make-flatpak.sh." >&2
  exit 1
}

case "$(uname -m)" in
  x86_64)        ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "ERROR: unsupported machine $(uname -m) for the flatpak build" >&2; exit 1 ;;
esac

command -v flatpak >/dev/null || {
  echo "ERROR: flatpak is not installed on this host, so no Flatpak can be built." >&2
  echo "       Debian/Ubuntu: apt-get install -y flatpak" >&2
  echo "       make-flatpak.sh installs org.flatpak.Builder and the GNOME runtime itself." >&2
  exit 1
}

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
bundle="${OUT_DIR}/URnetwork-${VERSION}-${ARCH}.flatpak"
rm -f "${bundle}"

echo ">>> [${ARCH}/flatpak] building ${bundle##*/} (this downloads the GNOME runtime on a cold host and takes a while)"
( cd "${LINUX_DIR}" && VERSION="${VERSION}" ARCH="${ARCH}" OUT_DIR="${OUT_DIR}" \
    bash "${maker}" --bundle )

# make-flatpak.sh composes this exact name; if the contract ever drifts, fail
# loudly here rather than letting run.sh's nullglob upload nothing in silence.
[ -s "${bundle}" ] || {
  echo "ERROR: make-flatpak.sh reported success but ${bundle} is missing or empty." >&2
  echo "       Bundles present in ${OUT_DIR}:" >&2
  ls -1 "${OUT_DIR}"/*.flatpak 2>/dev/null >&2 || echo "       (none)" >&2
  exit 1
}
echo ">>> [${ARCH}/flatpak] $(basename "${bundle}") — $(stat -c %s "${bundle}") bytes"
