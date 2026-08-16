#!/usr/bin/env bash
# Smoke test a Linux build container: verify the toolchain that container's
# ROLE needs is present (mirrors windows/smoke-test.ps1). Run inside the
# container by setup.sh. Exits 0 if all critical checks pass, else 1.
#
#   ROLE=daemon  Ubuntu 22.04 image: C++ toolchain (no GTK) + nfpm/dpkg/systemd
#   ROLE=gui     Ubuntu 24.04 image: C++/GTK4 toolchain + AppImage tooling
#
# SPDX-License-Identifier: MPL-2.0
ROLE="${ROLE:-gui}"
fail=0
ok()  { printf '[ok]   %s: %s\n' "$1" "$2"; }
bad() { printf '[FAIL] %s: %s\n' "$1" "$2"; fail=1; }

# check_cmd NAME BIN [version-args...]
check_cmd() {
  local name="$1" bin="$2"; shift 2
  if command -v "$bin" >/dev/null 2>&1; then
    ok "$name" "$("$bin" "$@" 2>&1 | head -1)"
  else
    bad "$name" "$bin not found on PATH"
  fi
}

# check_present NAME BIN — presence only, for tools whose bare/`--help` output is
# a usage or error message (desktop-file-validate, gio-querymodules); running
# them for a version string would print a scary-looking line next to an [ok].
check_present() {
  local name="$1" bin="$2"
  if command -v "$bin" >/dev/null 2>&1; then
    ok "$name" "$(command -v "$bin")"
  else
    bad "$name" "$bin not found on PATH"
  fi
}

# check_pc NAME pkg-config-module — a build dep the meson build links against
check_pc() {
  local name="$1" mod="$2"
  if pkg-config --exists "$mod" 2>/dev/null; then
    ok "$name" "$mod $(pkg-config --modversion "$mod" 2>/dev/null)"
  else
    bad "$name" "pkg-config module '$mod' missing"
  fi
}

# shellcheck disable=SC1091  # /etc/os-release is provided by the image
echo "=== role: ${ROLE}   ($(. /etc/os-release 2>/dev/null && echo "${PRETTY_NAME}"))"

# Shared C++ build toolchain ------------------------------------------------
check_cmd g++        g++        --version
check_cmd meson      meson      --version
check_cmd ninja      ninja      --version
check_cmd pkg-config pkg-config --version
check_cmd unzip      unzip      -v
# msgfmt is critical: meson's i18n SILENTLY skips the catalogs without it.
check_cmd msgfmt     msgfmt     --version
# readelf backs the glibc-floor gate and the AppImage's webkit/ABI assertions.
check_cmd readelf    readelf    --version
check_pc  glib-2.0   glib-2.0

# nlohmann-json is header-only (ships a pkg-config file or just the header).
if pkg-config --exists nlohmann_json 2>/dev/null || [ -f /usr/include/nlohmann/json.hpp ]; then
  ok  'nlohmann-json' 'present'
else
  bad 'nlohmann-json' '/usr/include/nlohmann/json.hpp missing'
fi

case "${ROLE}" in
daemon)
  # The daemon image must NOT carry GTK: it is Ubuntu 22.04 precisely because
  # its glibc is the declared floor, and 22.04 has no libgtkmm-4.0 at all.
  if pkg-config --exists gtkmm-4.0 2>/dev/null; then
    bad 'no GTK in the daemon image' 'gtkmm-4.0 present — wrong base image?'
  else
    ok  'no GTK in the daemon image' 'gtkmm-4.0 absent, as expected'
  fi
  # glibc must be the floor the .deb declares.
  ok 'glibc' "$(ldd --version | head -1)"

  # .deb + install-tarball packaging and its verification.
  check_cmd nfpm     nfpm     --version
  check_cmd dpkg     dpkg     --version
  check_cmd dpkg-deb dpkg-deb --version
  check_cmd fakeroot fakeroot --version
  check_cmd tar      tar      --version
  check_cmd systemd-analyze systemd-analyze --version
  # The .deb Depends on libfuse2; without it dpkg -i cannot configure and the
  # lifecycle test fails misleadingly.
  if ldconfig -p 2>/dev/null | grep -q 'libfuse\.so\.2'; then
    ok  'libfuse2 (deb dependency)' 'present'
  else
    bad 'libfuse2 (deb dependency)' 'missing — dpkg -i will not configure'
  fi
  ;;
gui)
  # GTK4/libadwaita dev libs.
  check_pc gtkmm-4.0  gtkmm-4.0
  check_pc libadwaita libadwaita-1
  check_pc libsecret   libsecret-1

  # webkitgtk-6.0 must be ABSENT: meson links it whenever present (no opt-out
  # option) and make-appimage.sh then refuses to package, because WebKitGTK's
  # absolute helper paths cannot be relocated into an AppDir.
  if pkg-config --exists webkitgtk-6.0 2>/dev/null; then
    bad 'webkitgtk-6.0 absent' 'present — the AppImage build will refuse to package'
  else
    ok  'webkitgtk-6.0 absent' 'as required for the AppImage build'
  fi

  # AppImage packaging + AppDir assembly.
  check_cmd appimagetool appimagetool --version
  check_cmd linuxdeploy  linuxdeploy  --version
  check_cmd zsyncmake    zsyncmake    -V
  check_cmd mksquashfs   mksquashfs   -version
  check_cmd patchelf     patchelf     --version
  check_present desktop-file-validate desktop-file-validate
  # make-appimage.sh hard-requires these.
  check_cmd glib-compile-schemas glib-compile-schemas --version
  check_present gio-querymodules gio-querymodules
  if command -v gdk-pixbuf-query-loaders >/dev/null 2>&1 \
     || ls /usr/lib/*/gdk-pixbuf-2.0/gdk-pixbuf-query-loaders >/dev/null 2>&1; then
    ok  'gdk-pixbuf-query-loaders' 'present'
  else
    bad 'gdk-pixbuf-query-loaders' 'missing — AppRun cannot regenerate the loader cache'
  fi
  if [ -d /usr/share/icons/Adwaita ]; then
    ok  'Adwaita icon theme' '/usr/share/icons/Adwaita'
  else
    bad 'Adwaita icon theme' 'missing — make-appimage.sh hard-fails'
  fi

  # The headless launch smoke test.
  check_present xvfb-run    xvfb-run
  check_present dbus-launch dbus-launch
  ;;
esac

echo
if [ "$fail" -eq 0 ]; then echo 'SMOKE TEST PASSED'; exit 0; else echo 'SMOKE TEST FAILED'; exit 1; fi
