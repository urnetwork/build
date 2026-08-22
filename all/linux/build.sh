#!/usr/bin/env bash
# Build the URnetwork Linux release artifacts for amd64 + arm64 in Docker (plain
# Ubuntu 24.04 build image). Runs on the macOS build host; arm64 builds native,
# amd64 builds under qemu emulation. Called by build/all/build-linux.sh
# (run.sh's linux build part).
#
# Per arch this produces, in OUT_DIR (names are NORMATIVE — linux/MIGRATION.md
# "Artifact filenames"; run.sh globs for exactly these):
#   urnetwork-daemon_<version>_<arch>.deb
#   urnetwork-daemon-<version>-<arch>.install.tar.gz
#   urnetwork-daemon-<version>.<rpmarch>.rpm  (rpmarch = x86_64|aarch64)
#   URnetwork-<version>-<arch>.AppImage  (+ .AppImage.zsync)
#
# The heavy lifting happens inside the container: build-arch.sh (mounted in)
# runs the meson build, stages an install tree, and invokes the packaging
# scripts the linux repo ships (linux/packaging/*, linux/app/scripts/*). Those
# scripts are owned by the linux repo (MIGRATION.md workstream B), not by this
# one — build-arch.sh fails loudly, naming the expected path, if one is missing.
#
# Inputs (env):
#   BUILD_HOME   the build server's local build dir (all repos); LINUX_DIR must
#                live under it so the build sees the exact local state run.sh
#                set up
#   LINUX_DIR    path to the linux repo root (app/ + packaging/)
#   SDK_ZIP      path to URnetworkSdkLinux.zip (cgo build output)
#   OUT_DIR      where the artifacts land; stale ones are cleared first
#   VERSION      release version embedded in the artifact names
#   ARCHES       (optional) space-separated, default "amd64 arm64"
#   ROLES        (optional) space-separated subset of "daemon gui", default
#                both. Same knob name and same default as setup.sh, which has
#                always had it. Unset, NOTHING changes: both halves build in
#                one invocation exactly as before.
#
#                It exists so a CI can run the two halves as separate jobs —
#                they use different base images (22.04 / 24.04) and the GUI
#                half is the long pole, so daemon+gui in parallel roughly
#                halves the per-arch wall clock. See
#                build/.github/workflows/linux-release.yml.
#
#                A role-scoped invocation MUST be given its OWN OUT_DIR: the
#                stale sweep below clears the WHOLE artifact set, not just this
#                role's, so `ROLES=daemon ...` followed by `ROLES=gui ...` into
#                one OUT_DIR would leave only the AppImage. The sweep is
#                deliberately not role-scoped — it is a guard against a stale
#                artifact reaching the uploader, and narrowing it would weaken
#                the case it exists for.
#
# Optional, forwarded into the container (see build-arch.sh):
#   UR_GLIBC_FLOOR    daemon glibc floor (default 2.35); must match the
#                     `Depends: libc6 (>= x)` in linux/packaging/deb/nfpm.yaml
#   UR_GLIBC_CEILING  the AppImage's own glibc gate (default: UR_GLIBC_FLOOR)
#   UR_REQUIRE_RPM    make a missing/failed .rpm fatal (default false: warn and
#                     carry on, so a bad rpm never costs the release the .deb,
#                     the tarball or the AppImage — see build-arch.sh's header)
#   UR_SKIP_VERIFY=1  build + package only, skip the verification stage
#
# SPDX-License-Identifier: MPL-2.0
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
: "${BUILD_HOME:?set BUILD_HOME}"
: "${LINUX_DIR:?set LINUX_DIR}"
: "${SDK_ZIP:?set SDK_ZIP}"
: "${OUT_DIR:?set OUT_DIR}"
: "${VERSION:?set VERSION}"
ARCHES="${ARCHES:-amd64 arm64}"

# The release stages every repo under BUILD_HOME; catch a stray path early.
case "$LINUX_DIR" in
  "$BUILD_HOME"/*) ;;
  *)
    echo "ERROR: LINUX_DIR ($LINUX_DIR) must live under BUILD_HOME ($BUILD_HOME)" >&2
    exit 1
    ;;
esac
if [ ! -d "${LINUX_DIR}/app" ]; then
  echo "ERROR: LINUX_DIR ($LINUX_DIR) does not look like the linux repo root (no app/)" >&2
  exit 1
fi

# Two build images per arch, because the two halves cannot share one (see
# Dockerfile.daemon's header): the daemon needs Ubuntu 22.04, whose glibc 2.35
# IS the floor the .deb declares; the GUI needs Ubuntu 24.04, the oldest Ubuntu
# packaging GTK4 + libadwaita.
image_base="urnetwork-linux-builder"
# Defaulted, so the release host's path (run.sh -> build-linux.sh, neither of
# which sets ROLES) is byte-for-byte what it was. Mirrors setup.sh's existing
# `roles="${ROLES:-daemon gui}"` rather than inventing a second spelling.
roles="${ROLES:-daemon gui}"
case "${roles}" in
  *[![:space:]]*) ;;
  *) echo "ERROR: ROLES is empty — it must be a non-empty subset of \"daemon gui\"" >&2; exit 1 ;;
esac
for _role in ${roles}; do
  case "${_role}" in
    daemon|gui) ;;
    *) echo "ERROR: ROLES must be a subset of \"daemon gui\" (got '${_role}' in '${roles}')" >&2; exit 1 ;;
  esac
done
# Word-boundary membership test. Used by the artifact assertions below, which
# have to know which half actually ran.
have_role() { case " ${roles} " in *" $1 "*) return 0 ;; *) return 1 ;; esac; }

mkdir -p "${OUT_DIR}"

# Stage the cgo SDK into third_party/urnetwork-sdk/{amd64,arm64}/ (both arches)
# BEFORE mounting: the linux repo is mounted read-only into the container, so
# everything it needs must be in place on the host first.
fetch_deps="${LINUX_DIR}/app/scripts/fetch-deps.sh"
if [ ! -f "${fetch_deps}" ]; then
  echo "ERROR: ${fetch_deps} missing — the linux repo's SDK vendoring script (MIGRATION.md workstream B)" >&2
  exit 1
fi
"${fetch_deps}" "${SDK_ZIP}"

# Clear any stale artifacts of the types we produce, so a partial rebuild can
# never hand old files to the uploader. (Once per run, NOT per arch — the first
# arch's output must survive the second arch's pass.)
# *.flatpak is cleared here even though this script does not build one:
# all/linux/build-flatpak.sh writes into this same OUT_DIR, it runs AFTER this,
# and run.sh's upload glob cannot tell a fresh bundle from last release's.
rm -f "${OUT_DIR}/"*.deb "${OUT_DIR}/"*.install.tar.gz "${OUT_DIR}/"*.rpm \
    "${OUT_DIR}/"*.pkg.tar.zst "${OUT_DIR}/"*.flatpak \
      "${OUT_DIR}/"*.AppImage "${OUT_DIR}/"*.AppImage.zsync

for arch in ${ARCHES}; do
  for role in ${roles}; do
    echo ">>> building linux ${role} artifacts for ${arch}"
    # Per-arch, per-role builder image (deps baked in; layer-cached across runs).
    docker build --platform "linux/${arch}" \
      -f "${here}/Dockerfile.${role}" \
      -t "${image_base}-${role}:${arch}" "${here}"

    # The linux repo is mounted READ-ONLY at /src and copied to a container-local
    # /work by build-arch.sh. The snapcraft-era reason for that copy (craft-parts
    # wrote user.* xattrs that Docker's virtiofs rejects) is gone with snapcraft;
    # the copy is kept because the container runs as root and must never write
    # build junk into the macOS checkout — the ro mount enforces it.
    docker run --rm --platform "linux/${arch}" \
      -v "${LINUX_DIR}:/src:ro" \
      -v "${OUT_DIR}:/out" \
      -v "${here}/build-arch.sh:/build-arch.sh:ro" \
      -v "${here}/verify.sh:/verify.sh:ro" \
      -e ARCH="${arch}" -e VERSION="${VERSION}" -e ROLE="${role}" \
      -e UR_GLIBC_FLOOR -e UR_GLIBC_CEILING -e UR_SKIP_VERIFY \
      -e UR_REQUIRE_RPM \
      "${image_base}-${role}:${arch}" \
      bash /build-arch.sh
  done

  # build-arch.sh already asserted these in-container; re-check on the host so
  # a mount/copy mishap can never sail through to the uploader.
  #
  # SCOPED TO THE ROLES THIS INVOCATION ACTUALLY RAN. With the default ROLES
  # both halves ran and this is the same four-name list it has always been;
  # with ROLES=daemon the AppImage was never attempted, and demanding it here
  # would fail every role-split leg for a file nothing was asked to build.
  # A space-separated string rather than an array on purpose: the release host
  # runs bash 3.2, where "${empty[@]}" under `set -u` is an unbound-variable
  # error, and these names cannot contain spaces.
  expected=''
  if have_role daemon; then
    expected="${expected} urnetwork-daemon_${VERSION}_${arch}.deb"
    expected="${expected} urnetwork-daemon-${VERSION}-${arch}.install.tar.gz"
  fi
  if have_role gui; then
    expected="${expected} URnetwork-${VERSION}-${arch}.AppImage"
    expected="${expected} URnetwork-${VERSION}-${arch}.AppImage.zsync"
  fi
  for f in ${expected}; do
    if [ ! -f "${OUT_DIR}/${f}" ]; then
      echo "ERROR: expected artifact missing after the ${arch} build: ${OUT_DIR}/${f}" >&2
      echo "       (artifact names are normative — linux/MIGRATION.md 'Artifact filenames')" >&2
      exit 1
    fi
  done

  # The .rpm is checked separately and NON-FATALLY: it is the one artifact here
  # that is not (yet) a release contract, and build-arch.sh already tolerates a
  # failed rpm per-artifact so the four names above survive it. Making it fatal
  # at this level would undo that — run.sh's uploads all live inside the `then`
  # branch of one `if build-linux.sh`, so any non-zero exit from this script
  # skips EVERY linux asset, SDK zip included. Loud, then, rather than fatal.
  case "${arch}" in
    amd64) rpm_arch=x86_64 ;;
    arm64) rpm_arch=aarch64 ;;
    *)     rpm_arch='' ;;
  esac
  rpm_name="urnetwork-daemon-${VERSION}.${rpm_arch}.rpm"
  if ! have_role daemon; then
    : # ROLE=gui only — the .rpm comes out of the daemon container, not this one
  elif [ -n "${rpm_arch}" ] && [ -f "${OUT_DIR}/${rpm_name}" ]; then
    : # present, nothing to say — the summary below lists it
  elif [ "${UR_REQUIRE_RPM:-false}" = true ]; then
    echo "ERROR: no ${rpm_name} after the ${arch} build and UR_REQUIRE_RPM=true" >&2
    exit 1
  else
    echo "WARNING: no .rpm for ${arch} (expected ${rpm_name}) — the release will" >&2
    echo "         ship without it. build-arch.sh logged the reason above;" >&2
    echo "         UR_REQUIRE_RPM=true makes this fatal instead." >&2
  fi

  # Same treatment for the Arch package, and for the same reason: it is not a
  # release contract yet, and build-arch.sh already tolerates it per-artifact.
  pkg_name="urnetwork-daemon-${VERSION}-${rpm_arch}.pkg.tar.zst"
  if [ -n "${rpm_arch}" ] && [ -f "${OUT_DIR}/${pkg_name}" ]; then
    : # present
  elif [ "${UR_REQUIRE_ARCH_PKG:-false}" = true ]; then
    echo "ERROR: no ${pkg_name} after the ${arch} build and UR_REQUIRE_ARCH_PKG=true" >&2
    exit 1
  else
    echo "WARNING: no Arch package for ${arch} (expected ${pkg_name}) — the" >&2
    echo "         release will ship without it. build-arch.sh logged the reason;" >&2
    echo "         UR_REQUIRE_ARCH_PKG=true makes this fatal instead." >&2
  fi

  echo ">>> ${arch} artifacts OK"
done

echo ">>> linux artifacts built:"
for f in "${OUT_DIR}"/*.deb "${OUT_DIR}"/*.install.tar.gz "${OUT_DIR}"/*.rpm \
         "${OUT_DIR}"/*.pkg.tar.zst \
         "${OUT_DIR}"/*.AppImage "${OUT_DIR}"/*.AppImage.zsync; do
  if [ -f "$f" ]; then echo "    $(basename "$f")"; fi
done
