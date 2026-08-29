#!/usr/bin/env bash
# In-container half of build-flatpak.sh. The source is deliberately mounted
# read-only at /src; copy it to ephemeral /work before flatpak-builder writes
# its build tree and stamped manifest.
# SPDX-License-Identifier: MPL-2.0
set -euo pipefail

: "${VERSION:?set VERSION}"
: "${ARCH:?set ARCH}"
RUNTIME_VERSION="${RUNTIME_VERSION:-49}"

case "${ARCH}" in
  amd64|arm64) ;;
  *) echo "ERROR: ARCH must be amd64 or arm64 (got '${ARCH}')" >&2; exit 1 ;;
esac

for path in \
  /src/packaging/make-flatpak.sh \
  /src/packaging/flatpak/com.bringyour.network.yml \
  "/src/app/third_party/urnetwork-sdk/${ARCH}/libURnetworkSdk.so"; do
  [ -f "${path}" ] || { echo "ERROR: required Flatpak input missing: ${path}" >&2; exit 1; }
done

command -v flatpak >/dev/null
command -v flatpak-builder >/dev/null
command -v eu-strip >/dev/null

echo ">>> [${ARCH}/flatpak] toolchain: $(flatpak --version); $(flatpak-builder --version)"
echo ">>> [${ARCH}/flatpak] ensuring GNOME runtime + SDK ${RUNTIME_VERSION} (cached in a Docker volume)"
flatpak install -y --noninteractive --system flathub \
  "org.gnome.Platform//${RUNTIME_VERSION}" \
  "org.gnome.Sdk//${RUNTIME_VERSION}"

# Keep the flatpak-builder state and target on this same ephemeral filesystem.
# flatpak-builder hard-links between them and rejects a separately mounted
# cache volume. The expensive GNOME runtime remains cached in /var/lib/flatpak;
# everything under /work is deliberately thrown away with the container.
mkdir -p /work/linux /out
cp -a /src/. /work/linux/
# A release checkout is a git submodule, whose .git file points outside /src.
# The target is mounted read-only and that pointer is meaningless after the
# copy, so remove only the copied metadata file to avoid noisy git probes.
[ ! -f /work/linux/.git ] || rm -f /work/linux/.git

cd /work/linux
VERSION="${VERSION}" \
ARCH="${ARCH}" \
RUNTIME_VERSION="${RUNTIME_VERSION}" \
OUT_DIR=/out \
  bash packaging/make-flatpak.sh --bundle
