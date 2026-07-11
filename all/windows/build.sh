#!/usr/bin/env bash
# Build the URnetwork Windows MSI by booting a LOCAL QEMU ARM Windows VM (the
# image built by Packer / setup.sh) on the Apple-Silicon build host and running
# the existing windows/app/build.ps1 over ssh. Replaces the remote
# WINDOWS_BUILD_HOST ssh flow with a local, HVF-accelerated VM.
#
# Each run boots a copy-on-write OVERLAY of the base image, so the base stays
# pristine. The VM lifecycle helpers are shared with setup.sh via lib.sh.
# Called by build/all/run.sh.
#
# Inputs (env):
#   BUILD_HOME  the build server's local build dir (all repos, on their correct
#               branches) — rsync'd into the VM at $WIN_DIR so it builds the exact
#               local state, exactly like the Linux container's bind mount
#   SDK_ZIP     path to URnetworkSdkWindows.zip (cgo build output)
#   OUT_DIR     where to copy the resulting .msi files
#   VERSION     release version (passed to build.ps1)
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
: "${SDK_ZIP:?set SDK_ZIP}"
: "${OUT_DIR:?set OUT_DIR}"
: "${VERSION:?set VERSION}"
win_init

for f in "$UEFI_CODE" "$SDK_ZIP"; do
  [ -e "$f" ] || win_die "missing $f"
done
mkdir -p "$OUT_DIR"

win_ensure_ssh_key

# The base image is built once by setup.sh (unattended install + toolchain +
# smoke test). If it's missing, the operator must run setup.sh first.
[ -f "$IMAGE" ] || win_die "base image $IMAGE missing — run ./setup.sh --windows-iso ... --virtio-iso ... first"

trap win_shutdown_vm EXIT

echo ">>> booting Windows ARM VM (headless, HVF) on ssh port $SSH_PORT"
win_boot_vm
echo ">>> waiting for the VM ssh service"
win_wait_ssh || win_die "VM ssh did not come up"

echo ">>> syncing the build home ($BUILD_HOME) into the VM at $WIN_DIR"
# rsync the build server's whole local tree (all repos, already on the correct
# branches from run.sh) into the VM — no clone, no GitHub, no ssh key.
win_sync_source "$BUILD_HOME"

echo ">>> delivering the SDK zip"
win_scp_to "$SDK_ZIP" "$WIN_DIR/URnetworkSdkWindows.zip"

echo ">>> building the MSI (build.ps1)"
win_ssh "powershell -ExecutionPolicy Bypass -File $WIN_DIR/windows/app/build.ps1 -Version $VERSION -SdkZip $WIN_DIR/URnetworkSdkWindows.zip"

echo ">>> retrieving MSIs"
win_scp_from "$WIN_DIR/windows/app/build/out/*.msi" "$OUT_DIR/"

echo ">>> windows MSIs -> $(ls "$OUT_DIR"/*.msi | xargs -n1 basename | tr '\n' ' ')"
