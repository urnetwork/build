#!/usr/bin/env bash
# Build the URnetwork Windows cgo SDK + MSI by booting a LOCAL QEMU ARM Windows
# VM (the image built by Packer / setup.sh) on the Apple-Silicon build host and
# running windows/build-sdk.ps1 (native cgo SDK) then windows/app/build.ps1 over
# ssh. Replaces the remote WINDOWS_BUILD_HOST ssh flow with a local, HVF VM.
#
# Each run boots a copy-on-write OVERLAY of the base image, so the base stays
# pristine. The VM lifecycle helpers are shared with setup.sh via lib.sh.
# Called by build/all/build-windows.sh (run.sh's windows build part).
#
# Inputs (env):
#   BUILD_HOME  the build server's local build dir (all repos, on their correct
#               branches) — rsync'd into the VM at $WIN_DIR so it builds the exact
#               local state, exactly like the Linux container's bind mount
#   OUT_DIR     where to copy the resulting .msi files
#   VERSION     release version, EXTERNAL_WARP_VERSION (passed to build.ps1)
#   SDK_VERSION WARP_VERSION, baked into the SDK DLL (passed to build-sdk.ps1)
#   WIN_DIR     (optional) build root inside the VM (default C:/build/urnetwork)
#   IMAGE       (optional) base qcow2 (default output/windows-arm64.qcow2)
#   SSH_KEY     (optional) private key matching the image's authorized key
#   SSH_PORT    (optional) host port forwarded to the VM's :22 (default 2222)
#
# SPDX-License-Identifier: MPL-2.0
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$here/lib.sh"

: "${BUILD_HOME:?set BUILD_HOME}"
: "${OUT_DIR:?set OUT_DIR}"
: "${VERSION:?set VERSION}"           # EXTERNAL_WARP_VERSION, for the app/MSI
: "${SDK_VERSION:?set SDK_VERSION}"   # WARP_VERSION, baked into the SDK DLL
win_init

[ -e "$UEFI_CODE" ] || win_die "missing $UEFI_CODE"
mkdir -p "$OUT_DIR"

win_ensure_ssh_key

# The base image is built once by setup.sh (unattended install + toolchain +
# smoke test). If it's missing, the operator must run setup.sh first.
[ -f "$IMAGE" ] || win_die "base image $IMAGE missing — run ./setup.sh --windows-iso ... --virtio-iso ... first"

trap win_shutdown_vm EXIT

echo ">>> booting Windows ARM VM (headless, HVF) on ssh port $SSH_PORT — watch: open vnc://127.0.0.1:5901 (pw 'windows')"
win_boot_vm
echo ">>> waiting for the VM ssh service"
if ! win_wait_ssh; then
  # Grab what the VM was showing before the EXIT trap tears it down, so a
  # headless failure is diagnosable later (same as the install path).
  mkdir -p "$here/output"
  win_mon "$WIN_MON_SOCK" "screendump $here/output/build-fail.ppm"
  win_die "VM ssh did not come up — last screen saved to $here/output/build-fail.ppm"
fi

echo ">>> syncing the build home ($BUILD_HOME) into the VM at $WIN_DIR"
# rsync the build server's whole local tree (all repos, already on the correct
# branches from run.sh) into the VM — no clone, no GitHub, no ssh key.
win_sync_source "$BUILD_HOME"

# The cgo SDK builds natively in the VM now (Go + llvm-mingw, provisioned into
# the image), replacing the old macOS cross-build. build-sdk.ps1 writes the zip
# to sdk/cgo/build/ inside the VM; pull it back so run.sh uploads it as the
# URnetworkSdkWindows artifact (the app build below consumes it in place).
sdk_zip_vm="$WIN_DIR/sdk/cgo/build/URnetworkSdkWindows.zip"
echo ">>> building the cgo SDK in the VM (build-sdk.ps1)"
win_ssh "powershell -ExecutionPolicy Bypass -File $WIN_DIR/windows/build-sdk.ps1 -Version $SDK_VERSION -SdkDir $WIN_DIR/sdk/cgo"

echo ">>> retrieving the SDK zip -> sdk/cgo/build/"
mkdir -p "$BUILD_HOME/sdk/cgo/build"
win_scp_from "$sdk_zip_vm" "$BUILD_HOME/sdk/cgo/build/URnetworkSdkWindows.zip"

echo ">>> building the MSI (build.ps1, with split-tunnel driver)"
# -IncludeDriver builds driver/SplitTunnel.vcxproj (SplitTunnel.sys) and harvests it
# into the MSI. build.ps1 installs the WDK MSBuild toolset (from the WDK.vsix) on
# demand and copies the .sys into $bin for WiX. See windows/app/build.ps1.
win_ssh "powershell -ExecutionPolicy Bypass -File $WIN_DIR/windows/app/build.ps1 -Version $VERSION -SdkZip $sdk_zip_vm -IncludeDriver"

echo ">>> retrieving MSIs"
win_scp_from "$WIN_DIR/windows/app/build/out/*.msi" "$OUT_DIR/"

echo ">>> windows MSIs -> $(ls "$OUT_DIR"/*.msi | xargs -n1 basename | tr '\n' ' ')"
