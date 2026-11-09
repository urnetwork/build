#!/usr/bin/env bash
# The Linux packages of ur's release (DESIGN.md §4.10; REPO-M1-03; plan-REPO-small-readings #9): per
# architecture, ur_<version>_<arch>.deb, ur-<version>.<rpmarch>.rpm and
# ur-<version>-<pkgarch>.pkg.tar.zst from the one template ur/nfpm.yaml, by nfpm v2.47.0 in
# build/all/linux's daemon image, which pins and checksums it and which build-linux.sh builds earlier in
# the same release. Each holds /usr/bin/ur, /usr/bin/git-remote-ur as a link to it, and LICENSE and
# NOTICE under /usr/share/doc/ur; no dependency, maintainer script or unit (Q-CORE-3). Then
# ur/distro-smoke.sh installs each on its distributions with no network, and removes it.
#
# Traps from linux/packaging: VERSION splits at its last hyphen into nfpm's version and release, or nfpm
# emits 2026.7.6~985989570, which sorts below the base; nfpm expands no environment variable in
# contents[].src, so this renders the template itself; rpm and pacman spell the arch x86_64 and aarch64
# while the deb keeps amd64 and arm64. Containers run with --network none, so a fetch cannot hide a
# dependency.
#
# Inputs (env), set by build-ur.sh: UR_BIN_DIR, OUT_DIR, BUILD_HOME, EXTERNAL_WARP_VERSION,
# VERSION_BASE, VERSION_RELEASE, GO_MOD_VERSION, SOURCE_DATE_EPOCH.
#
# SPDX-License-Identifier: MPL-2.0
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=ur/lib.sh
source "$here/ur/lib.sh"
for variable in UR_BIN_DIR OUT_DIR BUILD_HOME EXTERNAL_WARP_VERSION VERSION_BASE VERSION_RELEASE GO_MOD_VERSION SOURCE_DATE_EPOCH; do
  [ -n "${!variable:-}" ] || ur_die "set $variable (build-ur.sh does)"
done
sidecar_module="$(ur_sidecar_module)"

case "$(uname -m)" in
  arm64 | aarch64) image_arch=arm64 ;;
  x86_64) image_arch=amd64 ;;
  *) ur_die "no daemon image for $(uname -m)" ;;
esac
image="urnetwork-linux-builder-daemon:$image_arch"
docker image inspect "$image" > /dev/null 2>&1 ||
  ur_die "$image is not built here; build-linux.sh builds it from build/all/linux/Dockerfile.daemon, which pins nfpm"

# Renders ur/nfpm.yaml for one architecture, with the paths the container sees.
ur_nfpm_render() {
  local arch="$1"
  sed -e "s|@ARCH@|$arch|g" \
    -e "s|@VERSION_BASE@|$VERSION_BASE|g" \
    -e "s|@VERSION_RELEASE@|$VERSION_RELEASE|g" \
    -e "s|@BINARY@|/stage/ur|g" \
    -e "s|@LICENSE@|/stage/LICENSE|g" \
    -e "s|@NOTICE@|/stage/NOTICE|g" \
    "$here/ur/nfpm.yaml"
}

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
for arch in amd64 arm64; do
  case "$arch" in
    amd64) package_arch=x86_64 ;;
    arm64) package_arch=aarch64 ;;
  esac
  stage="$work/$arch"
  mkdir -p "$stage"
  [ -s "$UR_BIN_DIR/linux/$arch/ur" ] || ur_die "no linux/$arch ur at $UR_BIN_DIR/linux/$arch/ur"
  cp "$UR_BIN_DIR/linux/$arch/ur" "$stage/ur"
  chmod 0755 "$stage/ur"
  cp "$sidecar_module/LICENSE" "$sidecar_module/NOTICE" "$stage/"
  ur_nfpm_render "$arch" > "$stage/nfpm.yaml"
  for packager in deb rpm archlinux; do
    case "$packager" in
      deb) name="ur_${EXTERNAL_WARP_VERSION}_${arch}.deb" ;;
      rpm) name="ur-${EXTERNAL_WARP_VERSION}.${package_arch}.rpm" ;;
      archlinux) name="ur-${EXTERNAL_WARP_VERSION}-${package_arch}.pkg.tar.zst" ;;
    esac
    echo ">>> $name"
    rm -f "$OUT_DIR/$name"
    docker run --rm --network none --platform "linux/$image_arch" \
      -e SOURCE_DATE_EPOCH="$SOURCE_DATE_EPOCH" \
      -v "$stage:/stage:ro" -v "$OUT_DIR:/out" \
      "$image" nfpm package --config /stage/nfpm.yaml --packager "$packager" --target "/out/$name"
    [ -s "$OUT_DIR/$name" ] || ur_die "nfpm wrote no $name"
  done
done

"$here/ur/distro-smoke.sh" "$OUT_DIR" "$EXTERNAL_WARP_VERSION"
