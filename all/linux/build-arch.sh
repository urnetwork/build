#!/usr/bin/env bash
# Per-arch build + package + verify step for the URnetwork Linux app. Runs
# INSIDE the urnetwork-linux-builder container (Ubuntu 24.04, root), invoked by
# build.sh:
#
#   /src     the linux repo root (app/ + packaging/), mounted READ-ONLY
#   /out     OUT_DIR, writable — the release artifacts land here
#   ARCH     amd64 | arm64 (equals the container arch)
#   VERSION  release version, e.g. 2026.7.6-985989570
#   ROLE     daemon | gui — which half to build and package
#
# TWO IMAGES, ONE SCRIPT. The halves cannot share a build image:
#   ROLE=daemon runs on Ubuntu 22.04 (Dockerfile.daemon), whose glibc 2.35 IS
#     the floor nfpm.yaml declares. -Dgui=disabled; produces the .deb and the
#     install tarball. 22.04 has no libgtkmm-4.0 at all.
#   ROLE=gui runs on Ubuntu 24.04 (Dockerfile.gui), the oldest Ubuntu with
#     GTK4 + libadwaita. -Dgui=enabled; produces the AppImage + .zsync.
# Measured 2026-08-05 (arm64): a daemon built on 24.04 references GLIBC_2.38,
# so a single-image build cannot honestly declare the 2.35 floor — the app's
# `glibc-floor` meson test fails it, which is the gate working.
#
# Flow: copy /src to a writable /work (the container runs as root and must
# never write into the macOS checkout — the ro mount enforces it), meson build
# + test + install into a staging tree, invoke the linux repo's packaging
# scripts, then run verify.sh against the artifacts.
#
# The packaging scripts are owned by the linux repo (linux/MIGRATION.md
# workstream B); this script only calls them, with this env contract:
#
#   VERSION      release version (the artifact names embed it)
#   ARCH         amd64 | arm64
#   STAGING_DIR  the `meson install --destdir` tree (usr/... layout)
#   OUT_DIR      where the artifact must be written (= /out)
#   APP_DIR      the app source dir (/work/app) — packaging/lib/common.sh
#                resolves APP_PACKAGING_DIR from it
#   SDK_DIR      the vendored cgo SDK slice for ARCH
#
# Each script produces its NORMATIVE artifact name in OUT_DIR
# (linux/MIGRATION.md "Artifact filenames" — the pipeline greps for these):
#   make-deb.sh             -> urnetwork-daemon_<version>_<arch>.deb          (ROLE=daemon)
#   make-install-tarball.sh -> urnetwork-daemon-<version>-<arch>.install.tar.gz (ROLE=daemon)
#   make-rpm.sh             -> urnetwork-daemon-<version>.<rpmarch>.rpm       (ROLE=daemon)
#   make-appimage.sh        -> URnetwork-<version>-<arch>.AppImage + .zsync   (ROLE=gui)
#
# NOTE the .rpm is the one name that does not carry ${ARCH} verbatim: rpm has
# its own arch spelling, so <rpmarch> is x86_64/aarch64 while the ASSET arch
# stays Debian-spelled (amd64/arm64) everywhere else. The name is still exact,
# not a glob — make-rpm.sh only emits the mangled canonical NVR under
# UR_RPM_CANONICAL_NAME=1, which this pipeline never sets.
#
# Env knobs:
#   UR_GLIBC_FLOOR    daemon glibc floor asserted by meson's `glibc-floor` test
#                     (default 2.35). MUST equal the `Depends: libc6 (>= x)` in
#                     linux/packaging/deb/nfpm.yaml — nothing else couples that
#                     hand-written dependency to reality.
#   UR_GLIBC_CEILING  the AppImage's own glibc gate (default 2.39, the GUI
#                     image's glibc). Higher than the daemon's floor ON PURPOSE
#                     and unavoidably: the GUI needs GTK4, which the floor
#                     distro does not package, and the AppImage host-provides
#                     glibc per the excludelist. So the GUI requires a 24.04+
#                     host while the daemon runs on 22.04.
#   UR_REQUIRE_RPM    make a missing or failed .rpm fatal (default false: warn
#                     and carry on). Default is deliberate. run.sh's own
#                     warn-and-continue does NOT provide this tolerance: its
#                     uploads live INSIDE the `then` branch of a single
#                     `if build-linux.sh`, so one non-zero exit anywhere in the
#                     linux leg skips EVERY linux asset — the .deb, the
#                     tarball, the AppImage and the SDK zip included. A fatal
#                     rpm step would therefore turn "the new package broke"
#                     into "the release shipped no linux artifacts at all",
#                     which is strictly worse than the status quo. So the
#                     tolerance lives here, per artifact, and the .rpm runs
#                     only after the contract artifacts are already on disk.
#                     Set true to gate the release on it, which is what the
#                     linux repo's own CI does (beta-build.yml UR_REQUIRE_RPM).
#   UR_SKIP_VERIFY=1  build + package only, skip verify.sh
#
# SPDX-License-Identifier: MPL-2.0
set -euo pipefail

: "${ARCH:?set ARCH (amd64|arm64)}"
: "${VERSION:?set VERSION}"
: "${ROLE:?set ROLE (daemon|gui)}"
case "${ROLE}" in daemon|gui) ;; *) echo "ERROR: ROLE must be daemon or gui (got '${ROLE}')" >&2; exit 1 ;; esac
[ -d /src/app ] || { echo "ERROR: /src is not the linux repo root (no app/) — check build.sh's -v mount" >&2; exit 1; }
[ -d /out ] || { echo "ERROR: /out not mounted — check build.sh's -v mount" >&2; exit 1; }

work="/work"
app="${work}/app"
UR_GLIBC_FLOOR="${UR_GLIBC_FLOOR:-2.35}"
UR_GLIBC_CEILING="${UR_GLIBC_CEILING:-2.39}"

echo ">>> [${ARCH}] copying /src -> ${work} (the /src mount stays read-only)"
mkdir -p "${work}"
cp -a /src/. "${work}/"
rm -rf "${work}/.git"

# Resolve the linux repo's packaging entry points BEFORE the (slow, possibly
# qemu-emulated) meson build, so a missing script fails in seconds rather than
# after minutes of compiling. These paths are pinned by MIGRATION.md
# (workstream B owns linux/packaging/**).
deb_script="${work}/packaging/make-deb.sh"
tarball_script="${work}/packaging/make-install-tarball.sh"
rpm_script="${work}/packaging/make-rpm.sh"
appimage_script="${work}/packaging/make-appimage.sh"
if [ "${ROLE}" = daemon ]; then
  need_scripts=("${deb_script}" "${tarball_script}")
  # rpm's own arch spelling — see the note in the header. Mapped here rather
  # than at the call site so an unusable ARCH fails before the meson build.
  case "${ARCH}" in
    amd64) rpm_arch=x86_64 ;;
    arm64) rpm_arch=aarch64 ;;
    *) echo "ERROR: ARCH must be amd64 or arm64 (got '${ARCH}')" >&2; exit 1 ;;
  esac
else
  need_scripts=("${appimage_script}")
fi
missing=()
for s in "${need_scripts[@]}"; do
  [ -f "${s}" ] || missing+=("${s#"${work}"/}")
done
if [ "${#missing[@]}" -gt 0 ]; then
  {
    echo "ERROR: the linux repo is missing packaging scripts this pipeline invokes:"
    printf '         %s\n' "${missing[@]}"
    echo "       They are owned by the linux repo (linux/MIGRATION.md workstream B)."
    echo "       build/all/linux/build-arch.sh invokes them with VERSION, ARCH,"
    echo "       STAGING_DIR, OUT_DIR, APP_DIR, SDK_DIR in the environment and requires"
    echo "       the normative artifact names in OUT_DIR."
  } >&2
  exit 1
fi

# make-rpm.sh is preflighted too — knowing in seconds beats finding out after a
# qemu-emulated meson build — but SOFTLY, unlike the three above. It is the
# newest script in linux/packaging, so a checkout that legitimately predates it
# must still be able to produce the artifacts it does have; see UR_REQUIRE_RPM
# in the header for why one missing script must not cost the release its .deb.
build_rpm=1
if [ "${ROLE}" = daemon ] && [ ! -f "${rpm_script}" ]; then
  if [ "${UR_REQUIRE_RPM:-false}" = true ]; then
    {
      echo "ERROR: ${rpm_script#"${work}"/} is missing and UR_REQUIRE_RPM=true."
      echo "       It is owned by the linux repo (linux/MIGRATION.md workstream B),"
      echo "       same as the scripts checked above."
    } >&2
    exit 1
  fi
  build_rpm=0
  echo "WARN: [${ARCH}] packaging/make-rpm.sh is absent — this build produces no .rpm." >&2
  echo "      Set UR_REQUIRE_RPM=true to make that fatal instead." >&2
fi

# The vendored cgo SDK slice for this arch (staged on the host by
# app/scripts/fetch-deps.sh before the ro mount).
sdk_dir="${app}/third_party/urnetwork-sdk/${ARCH}"
if [ ! -f "${sdk_dir}/libURnetworkSdk.so" ]; then
  echo "ERROR: ${sdk_dir}/libURnetworkSdk.so missing — build.sh runs app/scripts/fetch-deps.sh on the host first" >&2
  exit 1
fi

builddir="${work}/build-${ARCH}"
staging="${work}/staging-${ARCH}"

# -Dgui: the feature defaults to `auto`, which SILENTLY skips urnetwork-gui
#   when gtkmm/libadwaita are absent — producing no AppImage with no error.
#   `enabled` on the GUI image makes a missing toolkit a hard failure;
#   `disabled` on the daemon image (22.04, no GTK4 packaged) is explicit
#   rather than accidental.
# -Dapp_version: compiled in as UR_APP_VERSION — what urnetworkd reports as
#   daemon_version in the control hello (the value the GUI's "daemon out of
#   date" state names). Without it every release would report the 0.0.0
#   sentinel and the skew check would be useless.
# Each image asserts the floor of the artifact IT ships. The GUI image also
# links a urnetworkd as a side effect (both targets are in one meson project),
# but that binary is discarded — only the 22.04 daemon image's copy is
# packaged — so gating the GUI image at the daemon's 2.35 floor would fail on a
# throwaway. Gate it at the AppImage's own ceiling instead.
if [ "${ROLE}" = gui ]; then
  gui_opt=enabled
  glibc_gate="${UR_GLIBC_CEILING}"
else
  gui_opt=disabled
  glibc_gate="${UR_GLIBC_FLOOR}"
fi
echo ">>> [${ARCH}/${ROLE}] meson build (gui=${gui_opt}, app_version=${VERSION}, glibc_floor=${glibc_gate})"
meson setup "${builddir}" "${app}" \
  --prefix=/usr --buildtype=release \
  -Dsdk_arch="${ARCH}" \
  -Dgui="${gui_opt}" \
  -Dapp_version="${VERSION}" \
  -Dglibc_floor="${glibc_gate}"
meson compile -C "${builddir}"

# The unit tests plus the `glibc-floor` gate, which asserts the built daemon
# references no glibc symbol above the floor the .deb declares. A failure here
# is real: the package would install on an older release and then fail to exec.
echo ">>> [${ARCH}/${ROLE}] meson test"
if ! meson test -C "${builddir}" --print-errorlogs; then
  {
    echo "ERROR: meson test failed for ${ARCH}/${ROLE} — see the log above."
    echo "       If it is the glibc-floor gate on ROLE=daemon: this image's glibc is newer"
    echo "       than the declared floor (${glibc_gate}), so the .deb's 'Depends: libc6 (>= …)'"
    echo "       would be a lie. Keep the daemon on the image whose glibc IS that floor"
    echo "       (Dockerfile.daemon = Ubuntu 22.04), or raise the floor in BOTH the gate"
    echo "       and linux/packaging/deb/nfpm.yaml."
  } >&2
  exit 1
fi

DESTDIR="${staging}" meson install -C "${builddir}"

# The daemon package ships the SDK .so at /usr/lib/urnetwork/libURnetworkSdk.so
# (normative — MIGRATION.md installed paths). Stage it if meson did not.
if [ -z "$(find "${staging}" -name 'libURnetworkSdk.so' -print -quit)" ]; then
  echo ">>> [${ARCH}] staging the SDK .so into the install tree (meson did not)"
  install -Dm755 "${sdk_dir}/libURnetworkSdk.so" "${staging}/usr/lib/urnetwork/libURnetworkSdk.so"
fi

# meson's i18n module SILENTLY skips the gettext catalogs when msgfmt is
# missing. The image bakes gettext in, but keep the tripwire — an english-only
# release would otherwise ship without any error to notice.
if [ -z "$(find "${staging}" -name '*.mo' -print -quit)" ]; then
  echo "WARN: [${ARCH}] no compiled locale catalogs (*.mo) in the staging tree — english-only build?" >&2
fi

# --- invoke the linux repo's packaging scripts (env contract in the header) ---
export VERSION ARCH
export STAGING_DIR="${staging}"
export OUT_DIR="/out"
export APP_DIR="${app}"
export SDK_DIR="${sdk_dir}"
export UR_GLIBC_CEILING

expect_artifact() {
  local name="$1" label="$2"
  if [ ! -f "/out/${name}" ]; then
    echo "ERROR: the ${label} script ran but did not produce ${name} in OUT_DIR (/out)." >&2
    echo "       The artifact names are normative — linux/MIGRATION.md 'Artifact filenames'." >&2
    exit 1
  fi
  echo ">>> [${ARCH}] ${name}"
}

# Run the packaging scripts with the CWD set to OUT_DIR. This is load-bearing
# for the AppImage: appimagetool writes its .zsync into the CURRENT WORKING
# DIRECTORY, not next to the output AppImage it was told to produce (verified
# against appimagetool continuous build 295 — with cwd elsewhere, the AppImage
# lands in OUT_DIR and the .zsync silently lands in the cwd). The .zsync is a
# contract artifact and the whole update channel depends on it, so make the cwd
# agree with the destination rather than hope. make-deb.sh and
# make-install-tarball.sh are cwd-independent (they cd in subshells), so this
# is safe for all three.
cd /out

if [ "${ROLE}" = daemon ]; then
  echo ">>> [${ARCH}] daemon .deb: ${deb_script}"
  bash "${deb_script}"
  expect_artifact "urnetwork-daemon_${VERSION}_${ARCH}.deb" "daemon .deb"

  echo ">>> [${ARCH}] daemon install tarball: ${tarball_script}"
  bash "${tarball_script}"
  expect_artifact "urnetwork-daemon-${VERSION}-${ARCH}.install.tar.gz" "daemon install tarball"

  # The .rpm goes LAST, and the order is load-bearing rather than cosmetic: the
  # .deb and the tarball are release contracts and are already on disk by the
  # time the tolerated step runs, so a bad rpm can never cost them. All three
  # come out of ONE assemble_daemon_root() call on ONE staging tree, which is
  # also why the .rpm belongs in this container and not in one of its own —
  # three packages built from one install tree cannot ship different daemons.
  if [ "${build_rpm}" = 0 ]; then
    echo "WARN: [${ARCH}] skipping the daemon .rpm — make-rpm.sh is absent (see the preflight)" >&2
  else
    echo ">>> [${ARCH}] daemon .rpm: ${rpm_script}"
    rpm_name="urnetwork-daemon-${VERSION}.${rpm_arch}.rpm"
    if ! bash "${rpm_script}"; then
      rpm_problem="make-rpm.sh failed"
    elif [ ! -f "/out/${rpm_name}" ]; then
      rpm_problem="make-rpm.sh reported success but wrote no ${rpm_name} to OUT_DIR (/out)"
    else
      rpm_problem=''
      echo ">>> [${ARCH}] ${rpm_name}"
    fi
    if [ -n "${rpm_problem}" ]; then
      if [ "${UR_REQUIRE_RPM:-false}" = true ]; then
        echo "ERROR: [${ARCH}] ${rpm_problem} — UR_REQUIRE_RPM=true" >&2
        exit 1
      fi
      echo "WARN: [${ARCH}] ${rpm_problem}" >&2
      echo "      The other daemon artifacts are already built; continuing without the .rpm." >&2
      echo "      Set UR_REQUIRE_RPM=true to gate the release on it instead." >&2
    fi
  fi
else
  echo ">>> [${ARCH}] GUI AppImage: ${appimage_script}"
  bash "${appimage_script}"
  expect_artifact "URnetwork-${VERSION}-${ARCH}.AppImage" "GUI AppImage"
  # appimagetool emits the .zsync into the CWD (set to /out above). If it is
  # still absent the update channel would ship broken, so make it fatal here
  # rather than inheriting make-appimage.sh's warning.
  expect_artifact "URnetwork-${VERSION}-${ARCH}.AppImage.zsync" "GUI AppImage (.zsync update feed)"
fi

# --- verification: prove the artifacts work, not just that they exist --------
if [ "${UR_SKIP_VERIFY:-0}" = 1 ]; then
  echo ">>> [${ARCH}/${ROLE}] UR_SKIP_VERIFY=1 — skipping verify.sh"
elif [ -f /verify.sh ]; then
  echo ">>> [${ARCH}/${ROLE}] verifying artifacts"
  ROLE="${ROLE}" bash /verify.sh
else
  echo "ERROR: /verify.sh not mounted — build.sh must mount it (or set UR_SKIP_VERIFY=1)" >&2
  exit 1
fi

echo ">>> [${ARCH}/${ROLE}] done"
