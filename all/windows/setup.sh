#!/usr/bin/env bash
# One-time setup + smoke test for the local QEMU ARM Windows build environment.
# Drives an unattended Windows install with the device layout validated
# interactively (ramfb + usb-bot CD-ROM + NVMe disk, all inbox — no driver
# injection), provisions the MSI toolchain (MSVC ARM64+x64, WDK, WiX, git) into
# the image over ssh, then smoke-tests it. The repo source is rsync'd in per build
# (build.sh win_sync_source), not baked into the image. Run once on the
# Apple-Silicon build host before a release.
#
#   ./setup.sh --windows-iso ~/isos/Win11_ARM64.iso --virtio-iso ~/isos/virtio-win.iso
#
# Shares the exact boot path with build.sh (via lib.sh), so a green smoke test
# means build.sh boots the same working VM. Watch the install on VNC:
#   open vnc://127.0.0.1:5901     (password: windows)
#
# SPDX-License-Identifier: MPL-2.0
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$here/lib.sh"

usage() {
  cat <<EOF
Usage: setup.sh --windows-iso PATH --virtio-iso PATH [options]

Builds the local QEMU ARM Windows build image (unattended install + toolchain)
and smoke-tests it.

Required (unless --skip-build reuses an existing image):
  --windows-iso PATH   Windows 11 ARM64 ISO (Microsoft's official ARM64 ISO)
  --virtio-iso PATH    virtio-win.iso (only the NetKVM NIC driver is used)

Options:
  --skip-build         reuse the existing image; only run the smoke test
  --reprovision        reuse the installed image, re-run provisioning (no OS
                       reinstall) then smoke-test — for iterating on provision.ps1
  --force              rebuild even if an image already exists
  --keep-up            leave the VM running after the smoke test (to debug via ssh)
  --uefi-code PATH     override the aarch64 UEFI code firmware
  --ssh-port N         host port forwarded to the VM ssh (default 2222)
  -h, --help           show this help

One-time prep: brew install qemu
Env overrides: IMAGE, SSH_KEY, UEFI_CODE, UEFI_VARS_TEMPLATE, CPUS, MEM, DISK_SIZE.
EOF
}

WINDOWS_ISO="${WINDOWS_ISO:-}"
VIRTIO_ISO="${VIRTIO_ISO:-}"
SKIP_BUILD=""
FORCE=""
KEEP_UP=""
REPROVISION=""

while [ $# -gt 0 ]; do
  case "$1" in
    --windows-iso)   WINDOWS_ISO="$2"; shift 2 ;;
    --windows-iso=*) WINDOWS_ISO="${1#*=}"; shift ;;
    --virtio-iso)    VIRTIO_ISO="$2"; shift 2 ;;
    --virtio-iso=*)  VIRTIO_ISO="${1#*=}"; shift ;;
    --uefi-code)     UEFI_CODE="$2"; shift 2 ;;
    --uefi-code=*)   UEFI_CODE="${1#*=}"; shift ;;
    --ssh-port)      SSH_PORT="$2"; shift 2 ;;
    --ssh-port=*)    SSH_PORT="${1#*=}"; shift ;;
    --skip-build)    SKIP_BUILD=1; shift ;;
    --reprovision)   REPROVISION=1; shift ;;
    --force)         FORCE=1; shift ;;
    --keep-up)       KEEP_UP=1; shift ;;
    -h|--help)       usage; exit 0 ;;
    *)               echo "unknown argument: $1" >&2; usage; exit 2 ;;
  esac
done

win_init

# --- preflight ---------------------------------------------------------------
echo ">>> preflight: checking tools + firmware"
win_require_tools qemu-system-aarch64 qemu-img ssh scp ssh-keygen nc hdiutil \
  || win_die "missing tools above — install qemu with: brew install qemu"
[ -f "$UEFI_CODE" ] || win_die "UEFI code firmware not found: $UEFI_CODE (brew install qemu, or pass --uefi-code)"
[ -f "$UEFI_VARS_TEMPLATE" ] || win_die "UEFI vars template not found: $UEFI_VARS_TEMPLATE (override with UEFI_VARS_TEMPLATE=...)"

win_ensure_ssh_key

trap win_shutdown_vm EXIT   # tear the VM down on any exit (idempotent)

# --- build the image ---------------------------------------------------------
if [ -n "$REPROVISION" ]; then
  # Re-run provisioning on an already-installed image (no OS reinstall). The
  # provision script is idempotent, so this is safe to repeat while iterating.
  [ -f "$IMAGE" ] || win_die "no image to reprovision ($IMAGE) — run without --reprovision to build it first"
  echo ">>> re-provisioning the existing image in place (watch: open vnc://127.0.0.1:5901  pw 'windows')"
  win_boot_image_rw
  win_wait_ssh || win_die "VM ssh did not come up (watch on VNC 5901)"
  win_scp_to "$here/packer/scripts/provision.ps1" "C:/Windows/Temp/provision.ps1"
  win_ssh "powershell -ExecutionPolicy Bypass -File C:/Windows/Temp/provision.ps1" \
    || win_die "provisioning failed — see output above"
  win_shutdown_vm
  echo ">>> re-provisioned: $IMAGE"
elif [ -n "$SKIP_BUILD" ] && [ -f "$IMAGE" ]; then
  echo ">>> reusing existing image: $IMAGE (--skip-build)"
else
  [ -f "$WINDOWS_ISO" ] || win_die "Windows 11 ARM64 ISO required — pass --windows-iso PATH"
  [ -f "$VIRTIO_ISO" ]  || win_die "virtio-win.iso required — pass --virtio-iso PATH"
  if [ -f "$IMAGE" ] && [ -z "$FORCE" ]; then
    win_die "image already exists: $IMAGE — use --skip-build to reuse, or --force to rebuild"
  fi
  [ -n "$FORCE" ] && rm -f "$IMAGE"

  # 1. unattended Windows install (leaves the VM running).
  win_install_image "$WINDOWS_ISO" "$VIRTIO_ISO" \
    || win_die "windows install did not come up — watch it on vnc://127.0.0.1:5901 (pw windows)"

  # 2. provision the MSI toolchain into the image over ssh (needs the VM's network,
  #    which is up since ssh connected — NetKVM installed at first logon).
  echo ">>> provisioning the toolchain (VS Build Tools + WDK + WiX + git + rsync; slow)"
  win_scp_to "$here/packer/scripts/provision.ps1" "C:/Windows/Temp/provision.ps1"
  win_ssh "powershell -ExecutionPolicy Bypass -File C:/Windows/Temp/provision.ps1" \
    || win_die "provisioning failed — see output above"

  # 3. finalize: shut down cleanly to bake everything into $IMAGE.
  echo ">>> finalizing image"
  win_shutdown_vm
  echo ">>> image built: $IMAGE"
fi

# --- smoke test --------------------------------------------------------------
echo ">>> smoke test: booting the image (headless, HVF) on ssh port $SSH_PORT"
win_boot_vm
win_wait_ssh || win_die "VM ssh did not come up — see README.md first-run notes"

echo ">>> uploading + running smoke-test.ps1"
win_scp_to "$here/smoke-test.ps1" "C:/Windows/Temp/smoke-test.ps1"
smoke_rc=0
win_ssh "powershell -ExecutionPolicy Bypass -File C:/Windows/Temp/smoke-test.ps1" || smoke_rc=$?

# --keep-up: neuter the teardown trap so the VM survives for debugging.
if [ -n "$KEEP_UP" ]; then
  echo ">>> VM left running (--keep-up):"
  echo "      ssh -i $SSH_KEY -p $SSH_PORT builder@127.0.0.1"
  echo "      stop:  kill $WIN_QEMU_PID   (run dir: $WIN_RUN_DIR)"
  WIN_QEMU_PID=""
  WIN_RUN_DIR=""
fi

if [ "$smoke_rc" -eq 0 ]; then
  echo ">>> SMOKE TEST PASSED — the Windows build environment is ready for build.sh."
else
  win_die "SMOKE TEST FAILED (rc=$smoke_rc) — see the output above."
fi
