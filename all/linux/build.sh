#!/usr/bin/env bash
# Build the URnetwork Linux snap for amd64 + arm64 in Docker (Canonical snapcraft
# rock, --destructive-mode). Runs on the macOS build host; arm64 builds native,
# amd64 builds under qemu emulation. Called by build/all/run.sh.
#
# Inputs (env):
#   BUILD_HOME      the build server's local build dir (all repos) — bind-mounted
#                   into the container at /build so the snap build sees the exact
#                   local state run.sh set up, including any sibling repos it
#                   references (../../sdk, etc.)
#   LINUX_APP_DIR   path to linux/app (meson project + snap/snapcraft.yaml); must
#                   live under BUILD_HOME
#   SDK_ZIP         path to URnetworkSdkLinux.zip (cgo build output)
#   OUT_DIR         where to copy the resulting .snap files
#   VERSION         release version to stamp into snapcraft.yaml
#   ARCHES          (optional) space-separated, default "amd64 arm64"
#
# SPDX-License-Identifier: MPL-2.0
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
: "${BUILD_HOME:?set BUILD_HOME}"
: "${LINUX_APP_DIR:?set LINUX_APP_DIR}"
: "${SDK_ZIP:?set SDK_ZIP}"
: "${OUT_DIR:?set OUT_DIR}"
: "${VERSION:?set VERSION}"
ARCHES="${ARCHES:-amd64 arm64}"

# The container bind-mounts the whole build home at /build; the snap project is
# LINUX_APP_DIR relative to it.
proj_rel="${LINUX_APP_DIR#"$BUILD_HOME"/}"
if [ "$proj_rel" = "$LINUX_APP_DIR" ]; then
  echo "ERROR: LINUX_APP_DIR ($LINUX_APP_DIR) must live under BUILD_HOME ($BUILD_HOME)" >&2
  exit 1
fi

image_base="urnetwork-snap-builder"

mkdir -p "${OUT_DIR}"

# Stamp the release version into the snap metadata.
sed_i() { if sed --version >/dev/null 2>&1; then sed -i "$@"; else sed -i '' "$@"; fi; }
sed_i "s/^version: .*/version: \"${VERSION}\"/" "${LINUX_APP_DIR}/snap/snapcraft.yaml"

# Stage the cgo SDK into third_party/urnetwork-sdk/{amd64,arm64}/ (both arches).
"${LINUX_APP_DIR}/scripts/fetch-deps.sh" "${SDK_ZIP}"

# Clean any stale snap output in the mounted project dir.
rm -f "${LINUX_APP_DIR}/"*.snap

for arch in ${ARCHES}; do
  echo ">>> building linux snap for ${arch}"
  # Build the per-arch builder image (deps baked in; layer-cached across runs).
  docker build --platform "linux/${arch}" -t "${image_base}:${arch}" "${here}"

  # snapcraft runs as the rock's entrypoint; args after the image go to snapcraft.
  # --destructive-mode builds directly in the container (arch == container arch).
  docker run --rm --platform "linux/${arch}" -u root \
    -v "${BUILD_HOME}:/build" -w "/build/${proj_rel}" \
    "${image_base}:${arch}" \
    pack --destructive-mode --build-for "${arch}" --verbosity verbose

  # Collect the arch's snap (snapcraft names it urnetwork_<version>_<arch>.snap).
  snap_file="$(ls -t "${LINUX_APP_DIR}/"*"_${arch}.snap" 2>/dev/null | head -1 || true)"
  if [ -z "${snap_file}" ]; then
    # fall back to any freshly built snap
    snap_file="$(ls -t "${LINUX_APP_DIR}/"*.snap 2>/dev/null | head -1 || true)"
  fi
  if [ -z "${snap_file}" ]; then
    echo "ERROR: no .snap produced for ${arch}" >&2
    exit 1
  fi
  cp "${snap_file}" "${OUT_DIR}/"
  echo ">>> ${arch} snap -> ${OUT_DIR}/$(basename "${snap_file}")"
  rm -f "${LINUX_APP_DIR}/"*.snap
done

echo ">>> linux snaps built: $(ls "${OUT_DIR}/"*.snap | xargs -n1 basename | tr '\n' ' ')"
