#!/usr/bin/env bash
# Smoke test the Linux snap build container: verify snapcraft + the C++/GTK4 build
# toolchain the snap needs are present (mirrors windows/smoke-test.ps1). Run inside
# the container by setup.sh with the entrypoint overridden. Exits 0 if all critical
# checks pass, else 1.
# SPDX-License-Identifier: MPL-2.0
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

# check_pc NAME pkg-config-module — a build-package the meson build links against
check_pc() {
  local name="$1" mod="$2"
  if pkg-config --exists "$mod" 2>/dev/null; then
    ok "$name" "$mod $(pkg-config --modversion "$mod" 2>/dev/null)"
  else
    bad "$name" "pkg-config module '$mod' missing"
  fi
}

# snapcraft (the rock's reason for being) + the C++ build toolchain --------------
check_cmd snapcraft  snapcraft  --version
check_cmd g++        g++        --version
check_cmd meson      meson      --version
check_cmd ninja      ninja      --version
check_cmd pkg-config pkg-config --version
check_cmd unzip      unzip      -v

# GTK4/libadwaita/glib dev libs (snapcraft.yaml build-packages) ------------------
check_pc gtkmm-4.0  gtkmm-4.0
check_pc libadwaita libadwaita-1
check_pc glib-2.0   glib-2.0

# nlohmann-json is header-only (ships a pkg-config file or just the header).
if pkg-config --exists nlohmann_json 2>/dev/null || [ -f /usr/include/nlohmann/json.hpp ]; then
  ok  'nlohmann-json' 'present'
else
  bad 'nlohmann-json' '/usr/include/nlohmann/json.hpp missing'
fi

echo
if [ "$fail" -eq 0 ]; then echo 'SMOKE TEST PASSED'; exit 0; else echo 'SMOKE TEST FAILED'; exit 1; fi
