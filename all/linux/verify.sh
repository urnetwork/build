#!/usr/bin/env bash
# Verify the URnetwork Linux release artifacts actually WORK — not merely that
# files with the right names exist. Runs INSIDE the builder container (root,
# target arch), against /out.
#
# Why this exists: the GTK4 AppImage is the piece with a real failure
# precedent. Gaphor — GTK4 + libadwaita, the closest analogue to this app —
# deleted its AppImage in 2023 because it broke constantly and the breakage
# went unnoticed for releases. `linuxdeploy-plugin-gtk` is an unmaintained stub
# for GTK4, so the AppDir here is hand-rolled, which means the things it can
# get wrong (missing GSettings schemas, unregistered pixbuf loaders, a library
# that silently resolves to the build host's copy) are exactly the things that
# pass on the build box and abort on a user's machine. Every check below is
# aimed at that class.
#
# Ordered cheapest-first so a structural break fails in seconds.
#
#   ARCH     amd64 | arm64
#   VERSION  release version
#   ROLE     daemon | gui | all — which artifacts to check. Each build image
#            only carries its own half's runtime (the daemon image has no GTK,
#            the GUI image has no dpkg/systemd), so the role scopes the checks
#            to what the container can honestly run. `all` is for a manual run
#            in an image that has both.
#   OUT_DIR  where the artifacts are (default /out)
#
# Independently runnable — iterate on the checks without a rebuild:
#   docker run --rm --platform linux/arm64 \
#     -v /path/to/out:/out -v "$PWD/verify.sh:/verify.sh:ro" \
#     -v /path/to/linux:/src:ro \
#     -e ARCH=arm64 -e VERSION=0.0.0-0 -e ROLE=gui \
#     urnetwork-linux-builder-gui:arm64 bash /verify.sh
#
# Exit 0 = all checks passed (skips are reported but do not fail).
# SPDX-License-Identifier: MPL-2.0
set -uo pipefail

: "${ARCH:?set ARCH (amd64|arm64)}"
: "${VERSION:?set VERSION}"
ROLE="${ROLE:-all}"
case "${ROLE}" in daemon|gui|all) ;; *) echo "ERROR: ROLE must be daemon|gui|all (got '${ROLE}')" >&2; exit 1 ;; esac
do_gui=0; do_daemon=0
case "${ROLE}" in
  gui)    do_gui=1 ;;
  daemon) do_daemon=1 ;;
  all)    do_gui=1; do_daemon=1 ;;
esac
OUT_DIR="${OUT_DIR:-/out}"
APP_ID="com.bringyour.network"

APPIMAGE="${OUT_DIR}/URnetwork-${VERSION}-${ARCH}.AppImage"
DEB="${OUT_DIR}/urnetwork-daemon_${VERSION}_${ARCH}.deb"
TARBALL="${OUT_DIR}/urnetwork-daemon-${VERSION}-${ARCH}.install.tar.gz"
# The .rpm is the one artifact name that does not carry ${ARCH} verbatim — rpm
# has its own arch spelling. Mirrors make-rpm.sh's map; an unknown ARCH leaves
# RPM_ARCH empty and the rpm section reports a skip rather than a bogus path.
case "${ARCH}" in
  amd64) RPM_ARCH='x86_64' ;;
  arm64) RPM_ARCH='aarch64' ;;
  *)     RPM_ARCH='' ;;
esac
RPM="${OUT_DIR}/urnetwork-daemon-${VERSION}.${RPM_ARCH}.rpm"

pass_n=0; fail_n=0; skip_n=0
pass() { printf '[PASS] %s\n' "$1"; pass_n=$((pass_n + 1)); }
fail() { printf '[FAIL] %s\n' "$1" >&2; fail_n=$((fail_n + 1)); }
# A skip that looks like a pass is worse than no test: always say what and why.
skip() { printf '[SKIP] %s — %s\n' "$1" "$2"; skip_n=$((skip_n + 1)); }
sec()  { printf '\n=== %s\n' "$1"; }

# check <label> <cmd...> — pass when the command exits 0
check() {
  local label="$1"; shift
  if "$@" >/dev/null 2>&1; then pass "${label}"; else fail "${label}"; fi
}

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

# ===========================================================================
if [ "${do_gui}" = 0 ]; then
  sec "1-3. AppImage checks"
  skip "AppImage structure / closure / launch" "ROLE=${ROLE}; the daemon image has no GTK runtime to test against"
else
sec "1. AppImage structure"
# ===========================================================================
if [ ! -f "${APPIMAGE}" ]; then
  fail "AppImage present at ${APPIMAGE}"
else
  pass "AppImage present ($(du -h "${APPIMAGE}" | cut -f1))"
  check "AppImage is executable" test -x "${APPIMAGE}"

  # Unpack WITHOUT executing the AppImage, and record whether the runtime is
  # executable here at all (RUNTIME_EXECUTABLE, used by the launch test below).
  #
  # `--appimage-extract` would be the obvious call, but it runs the outer
  # AppImage runtime — a *static-pie* ELF — and Docker Desktop's amd64
  # emulation on an Apple Silicon host cannot exec those ("Exec format error"),
  # even though ordinary x86-64 binaries emulate fine. Extracting by offset is
  # emulator-independent, so the amd64 artifact gets the same structural
  # verification the native arm64 one does instead of a spurious failure.
  #
  # An AppImage type 2 is an ELF with a squashfs appended; the squashfs starts
  # at the end of the section-header table (the sum appimage's own
  # appimage_get_elf_size() uses). Split readelf on ':' and take field 2 — NOT
  # $NF, since those lines end in "(bytes into file)".
  APPDIR="${WORK}/squashfs-root"
  ai_off=$(readelf -h "${APPIMAGE}" | awk -F: '
    /Start of section headers/ {gsub(/[^0-9]/,"",$2); o=$2}
    /Size of section headers/  {gsub(/[^0-9]/,"",$2); s=$2}
    /Number of section headers/{gsub(/[^0-9]/,"",$2); n=$2}
    END {print o + s * n}')
  if [ "${ai_off:-0}" -gt 0 ] && unsquashfs -q -o "${ai_off}" -d "${APPDIR}" "${APPIMAGE}" >/dev/null 2>&1; then
    pass "AppImage squashfs extracts (offset ${ai_off}, no exec required)"
  else
    fail "AppImage squashfs extracts (offset ${ai_off:-unreadable})"
  fi

  # Can the runtime actually exec on THIS host? Native arm64: yes. amd64 under
  # emulation: no. Not a defect in the artifact — it is a limit of the builder.
  if "${APPIMAGE}" --appimage-version >/dev/null 2>&1; then
    RUNTIME_EXECUTABLE=1
  else
    RUNTIME_EXECUTABLE=0
  fi

  if [ -d "${APPDIR}" ]; then
    # The hand-rolled AppDir owes all of this by hand (APPIMAGE.md §11e).
    # A missing GSettings schema is a HARD ABORT in GTK4, not a degradation.
    check "AppDir: AppRun"                    test -x "${APPDIR}/AppRun"
    check "AppDir: usr/bin/urnetwork-gui"     test -x "${APPDIR}/usr/bin/urnetwork-gui"
    check "AppDir: libURnetworkSdk.so"        test -f "${APPDIR}/usr/lib/urnetwork/libURnetworkSdk.so"
    check "AppDir: desktop entry"             test -f "${APPDIR}/${APP_ID}.desktop"
    check "AppDir: .DirIcon"                  test -f "${APPDIR}/.DirIcon"
    check "AppDir: compiled GSettings schemas (gschemas.compiled)" \
          test -s "${APPDIR}/usr/share/glib-2.0/schemas/gschemas.compiled"
    check "AppDir: Adwaita icon theme"        test -d "${APPDIR}/usr/share/icons/Adwaita"
    check "AppDir: world-110m.json (globe land layer)" \
          test -s "${APPDIR}/usr/share/urnetwork/world-110m.json"
    check "AppDir: gdk-pixbuf-query-loaders (AppRun regenerates the cache)" \
          test -x "${APPDIR}/usr/bin/gdk-pixbuf-query-loaders"

    # GTK4 + libadwaita + libsecret + the GLib family must all be bundled.
    # libsecret is not optional: RPC client credentials must never fall back
    # to a plaintext file when the host does not provide the library.
    # Bundling the GLib family WITHOUT its GIO modules is the documented
    # invariant break.
    for lib in libgtk-4.so libadwaita-1.so libglib-2.0.so libgio-2.0.so \
               libgobject-2.0.so libgdk_pixbuf-2.0.so libsecret-1.so; do
      if compgen -G "${APPDIR}/usr/lib/${lib}*" >/dev/null; then
        pass "AppDir bundles ${lib}"
      else
        fail "AppDir bundles ${lib}"
      fi
    done
    if compgen -G "${APPDIR}/usr/lib/gdk-pixbuf-2.0/*/loaders/*.so" >/dev/null; then
      pass "AppDir: gdk-pixbuf loaders"
    else
      fail "AppDir: gdk-pixbuf loaders"
    fi
    if compgen -G "${APPDIR}/usr/lib/gio/modules/*.so" >/dev/null; then
      pass "AppDir: GIO modules (bundle the GLib family WITH its modules)"
    else
      fail "AppDir: GIO modules (bundle the GLib family WITH its modules)"
    fi
    if compgen -G "${APPDIR}/usr/share/locale/*/LC_MESSAGES/urnetwork.mo" >/dev/null; then
      pass "AppDir: gettext catalogs ($(find "${APPDIR}/usr/share/locale" -name 'urnetwork.mo' | wc -l | tr -d ' ') locales)"
    else
      fail "AppDir: gettext catalogs"
    fi

    # WebKitGTK must NOT be linked: it hardcodes absolute paths to its
    # multi-process helpers, so no tool can relocate it into an AppDir.
    if readelf -d "${APPDIR}/usr/bin/urnetwork-gui" 2>/dev/null | grep -qi webkit; then
      fail "GUI does not link webkitgtk (absolute helper paths cannot be relocated)"
    else
      pass "GUI does not link webkitgtk"
    fi

    # =====================================================================
    sec "2. AppImage dependency closure"
    # =====================================================================
    # THE classic AppImage failure: a bundled stack that silently falls
    # through to the build host's copy. It passes here and dies everywhere
    # else, because the host paths exist on the build box and nowhere else.
    LDD_OUT="${WORK}/ldd.txt"
    LD_LIBRARY_PATH="${APPDIR}/usr/lib:${APPDIR}/usr/lib/urnetwork" \
      ldd "${APPDIR}/usr/bin/urnetwork-gui" > "${LDD_OUT}" 2>&1

    if grep -q 'not found' "${LDD_OUT}"; then
      fail "no unresolved libraries in the AppDir closure"
      grep 'not found' "${LDD_OUT}" | sed 's/^/       /' >&2
    else
      pass "no unresolved libraries in the AppDir closure"
    fi

    # Each of these must resolve INSIDE the AppDir. libc/libstdc++/Mesa are
    # deliberately host-provided (excludelist), so they are not listed.
    for lib in libgtk-4.so libadwaita-1.so libglib-2.0.so libgio-2.0.so \
               libgobject-2.0.so libgdk_pixbuf-2.0.so libsecret-1.so; do
      resolved="$(grep -oE "${lib}[^ ]* => [^ ]+" "${LDD_OUT}" | awk '{print $3}' | head -1)"
      if [ -z "${resolved}" ]; then
        skip "${lib} resolves inside the AppDir" "not in the GUI's closure"
      elif case "${resolved}" in "${APPDIR}"/*) true ;; *) false ;; esac then
        pass "${lib} resolves inside the AppDir"
      else
        fail "${lib} resolves to the HOST (${resolved}) — bundling silently fell through"
      fi
    done

    # =====================================================================
    sec "3. AppImage headless launch"
    # =====================================================================
    # A GTK/schema/pixbuf misconfiguration aborts during startup, which is
    # precisely what this catches. xvfb gives GTK a display; a private dbus
    # session keeps it off the (absent) system bus.
    if ! command -v xvfb-run >/dev/null 2>&1; then
      skip "headless launch" "xvfb-run not installed in the builder image"
    else
      LAUNCH_LOG="${WORK}/launch.log"
      # Prefer the AppImage itself (it exercises the runtime's mount + AppRun's
      # env setup). Where the runtime cannot exec — amd64 under emulation on an
      # Apple Silicon host — fall back to the extracted AppRun, which still
      # exercises everything we actually bundle: AppRun's env setup, the GTK4
      # stack, GSettings schemas and the pixbuf loaders. Only the upstream
      # runtime's self-mount goes untested, and that is not our code.
      export HOME="${WORK}/home"; mkdir -p "${HOME}"
      if [ "${RUNTIME_EXECUTABLE}" = 1 ]; then
        LAUNCH_TARGET="${APPIMAGE}"
        LAUNCH_WHAT="AppImage"
      else
        LAUNCH_TARGET="${APPDIR}/AppRun"
        LAUNCH_WHAT="extracted AppRun"
        export APPDIR
        echo "       note: the ${ARCH} AppImage runtime cannot exec on this builder" >&2
        echo "       (static-pie under emulation) — launching the extracted AppRun instead" >&2
      fi
      set +e
      timeout 25 xvfb-run -a --server-args='-screen 0 1024x768x24' \
        dbus-run-session -- "${LAUNCH_TARGET}" >"${LAUNCH_LOG}" 2>&1 &
      launch_pid=$!
      sleep 12
      if kill -0 "${launch_pid}" 2>/dev/null; then
        pass "${LAUNCH_WHAT} launched and stayed alive 12s under xvfb (no startup abort)"
        # SIGTERM the whole tree; the AppImage runtime forks a mount helper.
        pkill -TERM -P "${launch_pid}" 2>/dev/null
        kill -TERM "${launch_pid}" 2>/dev/null
        wait "${launch_pid}" 2>/dev/null
      else
        wait "${launch_pid}"; rc=$?
        fail "${LAUNCH_WHAT} launch (exited ${rc} within 12s)"
        echo "       --- last 30 lines of launch output ---" >&2
        tail -30 "${LAUNCH_LOG}" | sed 's/^/       /' >&2
      fi
      set -e
      # A GTK app that starts but spews these is broken in a way a liveness
      # check alone would miss.
      for pat in 'Settings schema .* is not installed' \
                 'GLib-GIO-ERROR' \
                 'Unable to load image-loading module' \
                 'symbol lookup error'; do
        if grep -qE "${pat}" "${LAUNCH_LOG}" 2>/dev/null; then
          fail "launch output clean of: ${pat}"
          grep -E "${pat}" "${LAUNCH_LOG}" | head -3 | sed 's/^/       /' >&2
        fi
      done
    fi
  fi
fi
fi   # do_gui

# ===========================================================================
sec "4. systemd unit (static check)"
# ===========================================================================
# No live systemd in a container, but `systemd-analyze verify` is a real
# static check — and this unit is Type=notify with RuntimeDirectory/
# StateDirectory/LogsDirectory, all of which it validates.
if [ "${do_daemon}" = 0 ]; then
  skip "systemd-analyze verify" "ROLE=${ROLE}; systemd is only in the daemon image"
elif ! command -v systemd-analyze >/dev/null 2>&1; then
  skip "systemd-analyze verify" "systemd not installed in the builder image"
elif [ ! -f /src/app/packaging/urnetworkd.service ]; then
  skip "systemd-analyze verify" "/src not mounted (unit source unavailable)"
else
  cp /src/app/packaging/urnetworkd.service "${WORK}/urnetworkd.service"
  # systemd-analyze resolves ExecStart against the filesystem; the daemon is
  # not installed at this point, so stub the path it names to avoid a spurious
  # "command not found" that is not what we are testing.
  install -d /usr/lib/urnetwork
  [ -e /usr/lib/urnetwork/urnetworkd ] || { : > /usr/lib/urnetwork/urnetworkd; chmod +x /usr/lib/urnetwork/urnetworkd; }
  VERIFY_OUT="${WORK}/systemd-verify.txt"
  systemd-analyze verify "${WORK}/urnetworkd.service" >"${VERIFY_OUT}" 2>&1
  # Exit status is unreliable across systemd versions; assert on real output.
  if grep -qiE 'unknown|invalid|failed|ignoring' "${VERIFY_OUT}"; then
    fail "systemd-analyze verify urnetworkd.service"
    sed 's/^/       /' "${VERIFY_OUT}" >&2
  else
    pass "systemd-analyze verify urnetworkd.service"
  fi
fi

# ===========================================================================
if [ "${do_daemon}" = 0 ]; then
  sec "5-6. daemon package lifecycle"
  skip ".deb + .rpm + install.sh tarball lifecycle" "ROLE=${ROLE}; the dpkg/nfpm/rpm tooling lives in the daemon image"
else
sec "5. .deb install lifecycle"
# ===========================================================================
# policy-rc.d: the postinst starts the unit via deb-systemd-invoke, which
# honours this veto. Containers are exactly what it is for.
printf '#!/bin/sh\nexit 101\n' > /usr/sbin/policy-rc.d && chmod +x /usr/sbin/policy-rc.d

if [ ! -f "${DEB}" ]; then
  fail ".deb present at ${DEB}"
else
  pass ".deb present ($(du -h "${DEB}" | cut -f1))"
  check ".deb metadata readable (dpkg-deb --info)" dpkg-deb --info "${DEB}"

  if dpkg -i "${DEB}" >"${WORK}/dpkg-i.log" 2>&1; then
    pass "dpkg -i installs the daemon package"
  else
    fail "dpkg -i installs the daemon package"
    tail -20 "${WORK}/dpkg-i.log" | sed 's/^/       /' >&2
  fi

  check "installed: /usr/lib/urnetwork/urnetworkd"          test -x /usr/lib/urnetwork/urnetworkd
  check "installed: /usr/lib/urnetwork/libURnetworkSdk.so"  test -f /usr/lib/urnetwork/libURnetworkSdk.so
  check "installed: /usr/bin/urnetwork (launcher)"          test -x /usr/bin/urnetwork
  check "installed: unit at /lib/systemd/system"            test -f /lib/systemd/system/urnetworkd.service
  check "installed: desktop entry (app-id filename)" \
        test -f "/usr/share/applications/${APP_ID}.desktop"
  check "installed: NetworkManager unmanaged marking"       test -f /etc/NetworkManager/conf.d/95-urnetwork.conf
  check "installed: udev unmanaged rule"                    test -f /etc/udev/rules.d/85-urnetwork-unmanaged.rules
  check "installed: inert autostart template"               test -f "/etc/urnetwork/autostart/${APP_ID}.desktop"
  check "postinst created the 'urnetwork' system group"     getent group urnetwork

  # The AppImage is NEVER packaged: the launcher must fail with a hint, not a
  # confusing stack trace, when no GUI AppImage is installed in the user's home.
  launcher_out="$(HOME="${WORK}/nohome" /usr/bin/urnetwork 2>&1)"; launcher_rc=$?
  if [ "${launcher_rc}" = 127 ]; then
    pass "launcher exits 127 when no AppImage is present"
  else
    fail "launcher exits 127 when no AppImage is present (got ${launcher_rc})"
  fi
  if [ -n "${launcher_out}" ]; then
    pass "launcher prints an install hint ($(printf '%s' "${launcher_out}" | head -1 | cut -c1-60)…)"
  else
    fail "launcher prints an install hint"
  fi

  # The daemon must actually exec — this is what the glibc floor protects.
  if /usr/lib/urnetwork/urnetworkd --version >"${WORK}/daemon-version.txt" 2>&1; then
    pass "urnetworkd --version execs ($(head -1 "${WORK}/daemon-version.txt"))"
  else
    rc=$?
    # No --version flag is not a failure; a loader/ABI error is.
    if grep -qiE 'not found|symbol|GLIBC|cannot open shared object' "${WORK}/daemon-version.txt"; then
      fail "urnetworkd execs (loader/ABI error)"
      sed 's/^/       /' "${WORK}/daemon-version.txt" >&2
    else
      skip "urnetworkd --version" "exited ${rc}; no --version flag (not a loader error)"
    fi
  fi

  if dpkg -P urnetwork-daemon >"${WORK}/dpkg-P.log" 2>&1; then
    pass "dpkg -P purges the package"
  else
    fail "dpkg -P purges the package"
    tail -20 "${WORK}/dpkg-P.log" | sed 's/^/       /' >&2
  fi
  check "purge removed /usr/lib/urnetwork/urnetworkd"  test ! -e /usr/lib/urnetwork/urnetworkd
  check "purge removed /usr/bin/urnetwork"             test ! -e /usr/bin/urnetwork
fi

# ===========================================================================
sec "5b. .rpm (metadata only)"
# ===========================================================================
# Deliberately THIN, and both halves of that are on purpose.
#
# What is NOT repeated here: make-rpm.sh already asserts the payload against
# `rpm -qp` before it returns — the six required installed paths, the policy
# module, that nothing ships under /lib, that the unit is not %config, and that
# all four scriptlets exist and name their units. Re-running those would test
# the same tool twice. (That is also why Dockerfile.daemon installs `rpm`:
# without it make-rpm.sh silently downgrades the whole assertion to a warning.)
#
# What IS checked here is the thing only the CALLER can know: the staging tree
# is arch-specific, so an aarch64 rpm falling out of the amd64 leg is the real
# failure mode at this level, and neither the packaging script nor a
# file-exists check can see it.
#
# There is NO install test, on purpose. `rpm -i` on Ubuntu would create an
# rpmdb on a dpkg-owned filesystem and STILL not exercise what matters: %post's
# `semodule -X 200 -i` and the systemd preset only mean something on a Fedora
# host. Nothing in this pipeline executes an rpm scriptlet — do not read a
# green build here as a green install.
if [ -z "${RPM_ARCH}" ]; then
  skip ".rpm checks" "ARCH='${ARCH}' has no rpm arch spelling (expected amd64|arm64)"
elif [ ! -f "${RPM}" ]; then
  # A skip, not a fail: the .rpm is warn-and-continue in build-arch.sh
  # (UR_REQUIRE_RPM), which has already reported the reason with more detail
  # than this script has. Failing here would just double the noise and turn a
  # tolerated miss into a build failure by the back door.
  skip ".rpm present" "no $(basename "${RPM}") in ${OUT_DIR} — see build-arch.sh's UR_REQUIRE_RPM warning above"
elif ! command -v rpm >/dev/null 2>&1; then
  skip ".rpm metadata" "rpm is not installed in this image (Dockerfile.daemon installs it)"
else
  pass ".rpm present ($(du -h "${RPM}" | cut -f1))"
  check "rpm metadata readable (rpm -qp --info)" rpm -qp --info "${RPM}"

  # The arch tag in the HEADER, not merely in the filename: a mis-plumbed
  # staging tree renames nothing.
  rpm_pkg_arch="$(rpm -qp --qf '%{ARCH}' "${RPM}" 2>/dev/null || true)"
  if [ "${rpm_pkg_arch}" = "${RPM_ARCH}" ]; then
    pass "rpm arch tag is ${RPM_ARCH} (this leg is ARCH=${ARCH})"
  else
    fail "rpm arch tag is ${RPM_ARCH} (got '${rpm_pkg_arch:-unreadable}') — an rpm from the other arch's leg?"
  fi
fi

# ===========================================================================
sec "6. install.sh tarball lifecycle"
# ===========================================================================
if [ ! -f "${TARBALL}" ]; then
  fail "install tarball present at ${TARBALL}"
else
  pass "install tarball present ($(du -h "${TARBALL}" | cut -f1))"

  EXTRACT="${WORK}/tar"; mkdir -p "${EXTRACT}"
  tar xzf "${TARBALL}" -C "${EXTRACT}"
  # Never a tarbomb: exactly one top-level directory, named urnetwork-daemon/.
  tops="$(find "${EXTRACT}" -mindepth 1 -maxdepth 1 | wc -l | tr -d ' ')"
  if [ "${tops}" = 1 ] && [ -d "${EXTRACT}/urnetwork-daemon" ]; then
    pass "tarball has exactly one top-level dir: urnetwork-daemon/"
  else
    fail "tarball has exactly one top-level dir: urnetwork-daemon/ (found ${tops})"
  fi
  ROOT="${EXTRACT}/urnetwork-daemon"
  check "tarball: install.sh"   test -x "${ROOT}/install.sh"
  check "tarball: uninstall.sh" test -x "${ROOT}/uninstall.sh"
  check "tarball: VERSION"      test -f "${ROOT}/VERSION"
  check "tarball: payload/ tree" test -d "${ROOT}/payload"

  # install.sh's preflight hard-requires a live systemd (/run/systemd/system),
  # which a container does not have. --force downgrades preflight refusals to
  # warnings, and every systemctl call in the script is already guarded on that
  # same directory — so with --force the FILE-PLACEMENT lifecycle runs for real
  # here while the unit enable/start path is simply not reached. That path is
  # reported as its own skip below rather than hidden inside these passes.
  INSTALL_ARGS=()
  if [ ! -d /run/systemd/system ]; then
    INSTALL_ARGS+=(--force)
    skip "install.sh systemd unit enable/start path" \
         "no live systemd in the container; running install.sh --force, which exercises file placement only"
  fi

  # Fresh install.
  if "${ROOT}/install.sh" "${INSTALL_ARGS[@]}" >"${WORK}/install.log" 2>&1; then
    pass "install.sh fresh install"
  else
    fail "install.sh fresh install"
    tail -25 "${WORK}/install.log" | sed 's/^/       /' >&2
  fi
  check "install.sh placed the daemon"   test -x /usr/lib/urnetwork/urnetworkd
  check "install.sh placed the launcher" test -x /usr/bin/urnetwork
  check "install.sh created the group"   getent group urnetwork

  # Upgrade over itself must preserve /etc state (the same command installs
  # and upgrades — there is no separate --upgrade flag).
  mkdir -p /etc/urnetwork
  echo 'verify-state-marker' > /etc/urnetwork/.verify-state
  if "${ROOT}/install.sh" "${INSTALL_ARGS[@]}" >"${WORK}/upgrade.log" 2>&1; then
    pass "install.sh is idempotent (re-run upgrades in place)"
    upgrade_ran=1
  else
    fail "install.sh is idempotent (re-run upgrades in place)"
    upgrade_ran=0
    tail -25 "${WORK}/upgrade.log" | sed 's/^/       /' >&2
  fi
  # Only meaningful if the upgrade actually ran — otherwise this asserts
  # nothing but that we can read back a file we just wrote.
  if [ "${upgrade_ran}" = 1 ]; then
    if [ "$(cat /etc/urnetwork/.verify-state 2>/dev/null)" = 'verify-state-marker' ]; then
      pass "upgrade preserved /etc/urnetwork state"
    else
      fail "upgrade preserved /etc/urnetwork state"
    fi
  else
    skip "upgrade preserved /etc/urnetwork state" "the upgrade run did not complete, so there is nothing to assert"
  fi

  # Refuse to fight dpkg: two package managers owning the same paths is the
  # worst failure mode available here, and it is silent until an upgrade.
  if [ -f "${DEB}" ]; then
    dpkg -i "${DEB}" >/dev/null 2>&1
    if "${ROOT}/install.sh" >"${WORK}/conflict.log" 2>&1; then
      fail "install.sh aborts when dpkg owns the install"
    else
      if grep -qiE 'dpkg|apt' "${WORK}/conflict.log"; then
        pass "install.sh aborts against a dpkg-owned install, pointing at apt"
      else
        fail "install.sh aborts against a dpkg-owned install with an apt-pointing message"
        tail -10 "${WORK}/conflict.log" | sed 's/^/       /' >&2
      fi
    fi
    dpkg -P urnetwork-daemon >/dev/null 2>&1
    # dpkg -P removed the shared paths; reinstall from the tarball so the
    # uninstall test below exercises a real install.
    "${ROOT}/install.sh" "${INSTALL_ARGS[@]}" >/dev/null 2>&1
  else
    skip "install.sh aborts against a dpkg-owned install" "no .deb was produced"
  fi

  if "${ROOT}/uninstall.sh" --purge >"${WORK}/uninstall.log" 2>&1; then
    pass "uninstall.sh --purge"
  else
    fail "uninstall.sh --purge"
    tail -25 "${WORK}/uninstall.log" | sed 's/^/       /' >&2
  fi
  check "purge removed the daemon"   test ! -e /usr/lib/urnetwork/urnetworkd
  check "purge removed the launcher" test ! -e /usr/bin/urnetwork
fi
fi   # do_daemon

# ===========================================================================
sec "7. Checks that cannot run in this container"
# ===========================================================================
# A skip that looks like a pass is worse than no test — name each one.
if [ -c /dev/net/tun ]; then
  if command -v ip >/dev/null 2>&1 && ip tuntap add mode tun name urnetverify0 >/dev/null 2>&1; then
    ip link del urnetverify0 >/dev/null 2>&1
    pass "tun device creation (NET_ADMIN present)"
  else
    skip "tun device creation" "/dev/net/tun present but NET_ADMIN missing — needs --cap-add NET_ADMIN"
  fi
else
  skip "tun data-plane test" "no /dev/net/tun — needs 'docker run --cap-add NET_ADMIN --device /dev/net/tun'"
fi
skip "daemon start + control-socket handshake" "needs live systemd (PID 1); the container has none"
skip "GUI<->daemon device RPC over loopback mTLS" "needs both halves running under a real session"
skip "AppImage self-update (appimageupdatetool -O)" "needs the self-hosted zsync endpoint; GitHub Releases returns 501 on multi-range"
skip "desktop integration (update-desktop-database / gtk-update-icon-cache triggers)" "needs a real desktop session to observe the urnetwork:// handler"
skip "GSK GL renderer path" "xvfb gives no GPU; the launch test exercises the Cairo/software path only"

# ===========================================================================
printf '\n===========================================================\n'
printf 'verify [%s/%s]: %d passed, %d failed, %d skipped\n' "${ARCH}" "${ROLE}" "${pass_n}" "${fail_n}" "${skip_n}"
printf '===========================================================\n'
[ "${fail_n}" -eq 0 ] || exit 1
