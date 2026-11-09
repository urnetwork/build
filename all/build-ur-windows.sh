#!/usr/bin/env bash
# The Windows zips of ur's release (DESIGN.md §4.10; REPO-M1-03; plan-REPO-small-readings #12). ur.exe
# for amd64 and arm64 goes to the build VM (build/all/windows's helpers), where ur/windows/sign-ur.ps1
# signs each with the Authenticode certificate behind the VM's URN_SIGN_THUMBPRINT and verifies it
# (signtool verify /pa /all). Then each ur-<version>-windows-<arch>.zip holds the signed ur.exe,
# git-remote-ur.exe copied from it, LICENSE, NOTICE and README.txt.
#
# Signing is the gate (owner decision 77): unlike windows/app/build.ps1, which skips signing when no
# thumbprint is set, this fails. The copy is made only after signing, so both exes are the signed bytes,
# and SHA256SUMS comes later still, since a signature changes the file. No wintun.dll
# (plan-BRIDGE-windows-tun-driver): ur tun looks for one beside ur.exe and checks it before loading.
#
# Inputs (env), set by build-ur.sh: UR_BIN_DIR, OUT_DIR, BUILD_HOME, EXTERNAL_WARP_VERSION,
# GO_MOD_VERSION, SOURCE_DATE_EPOCH, URARCHIVE; and the VM's, as build-windows.sh takes them.
#
# SPDX-License-Identifier: MPL-2.0
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=ur/lib.sh
source "$here/ur/lib.sh"
for variable in UR_BIN_DIR OUT_DIR BUILD_HOME EXTERNAL_WARP_VERSION GO_MOD_VERSION SOURCE_DATE_EPOCH URARCHIVE; do
  [ -n "${!variable:-}" ] || ur_die "set $variable (build-ur.sh does)"
done
sidecar_module="$(ur_sidecar_module)"
for arch in amd64 arm64; do
  [ -s "$UR_BIN_DIR/windows/$arch/ur.exe" ] || ur_die "no windows/$arch ur.exe at $UR_BIN_DIR/windows/$arch/ur.exe"
done

# shellcheck source=windows/lib.sh
source "$here/windows/lib.sh"
work="$(mktemp -d)"
trap 'win_shutdown_vm; rm -rf "$work"' EXIT
win_init
echo ">>> booting the Windows VM to sign ur.exe"
win_boot_vm
win_wait_ssh || ur_die "the Windows VM's ssh did not come up"

guest="C:/Windows/Temp/ur-sign"
win_ssh "powershell -NoProfile -Command \"if (-not \$env:URN_SIGN_THUMBPRINT) { exit 3 }; New-Item -ItemType Directory -Force -Path '$guest/amd64','$guest/arm64' | Out-Null\"" ||
  ur_die "the build VM has no URN_SIGN_THUMBPRINT, and ur is never released unsigned (owner decision 77; EXT-2)"
win_scp_to "$here/ur/windows/sign-ur.ps1" "$guest/sign-ur.ps1"
for arch in amd64 arm64; do
  win_scp_to "$UR_BIN_DIR/windows/$arch/ur.exe" "$guest/$arch/ur.exe"
done
win_ssh "powershell -NoProfile -ExecutionPolicy Bypass -File $guest/sign-ur.ps1 -Directory $guest" ||
  ur_die "sign-ur.ps1 did not sign and verify both ur.exe"

for arch in amd64 arm64; do
  mkdir -p "$work/$arch"
  win_scp_from "$guest/$arch/ur.exe" "$work/$arch/ur.exe"
  [ -s "$work/$arch/ur.exe" ] || ur_die "no signed ur.exe came back for $arch"
  # an Authenticode signature is part of the file, so signed bytes differ from the built ones
  if cmp -s "$work/$arch/ur.exe" "$UR_BIN_DIR/windows/$arch/ur.exe"; then
    ur_die "the ur.exe that came back for $arch is the unsigned one"
  fi
  cp "$work/$arch/ur.exe" "$work/$arch/git-remote-ur.exe"
  name="ur-$EXTERNAL_WARP_VERSION-windows-$arch.zip"
  rm -f "$OUT_DIR/$name"
  "$URARCHIVE" zip -o "$OUT_DIR/$name" -mtime "$SOURCE_DATE_EPOCH" \
    "ur.exe=$work/$arch/ur.exe" "git-remote-ur.exe=$work/$arch/git-remote-ur.exe" \
    "LICENSE=$sidecar_module/LICENSE" "NOTICE=$sidecar_module/NOTICE" "README.txt=$here/ur/windows/README.txt"
  echo ">>> $name"
done
