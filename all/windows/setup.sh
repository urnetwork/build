#!/usr/bin/env bash
# One-time setup + smoke test for the local QEMU ARM Windows build environment.
# Drives an unattended Windows install with the device layout validated
# interactively (ramfb + usb-bot CD-ROM + NVMe disk, all inbox — no driver
# injection), provisions the MSI toolchain (MSVC ARM64+x64, WDK, WiX, git) into
# the image over ssh, then smoke-tests it. The repo source is rsync'd in per build
# (build.sh win_sync_source), not baked into the image. Run once on the
# Apple-Silicon build host before a release.
#
#   ./setup.sh --windows-iso ~/isos/Win11_24H2_English_Arm64.iso \
#              --virtio-iso ~/isos/virtio-win.iso
#
# REQUIRES THE WINDOWS 11 *24H2* ARM64 ISO (build 26100). 25H2 (build 26200)
# installs its first phase fine and then hangs FOREVER at the first boot of the
# installed OS on this QEMU/edk2 device layout — the install never reaches OOBE
# or ssh (see README "Windows ISO"). That failure surfaces ~40 min in, as a
# silent wait-for-ssh timeout, so the ISO is verified up front instead: the
# preflight reads the real build number out of the ISO (not its filename) and
# refuses anything but 26100. Get 24H2 from Microsoft's Software Download site.
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
  --windows-iso PATH   Windows 11 *24H2* ARM64 ISO, Pro edition (build 26100).
                       REQUIRED: 25H2 (26200) installs but then hangs forever at
                       the first boot of the installed OS on this QEMU/edk2
                       layout. The build number is read from the ISO itself and
                       anything but 24H2 is refused up front — see README.
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
ISO gate overrides (deliberate use only): WIN_REQUIRED_BUILD / WIN_REQUIRED_EDITION
to target a different build, or WINDOWS_ISO_SKIP_CHECK=1 to bypass the check.
EOF
}

# Refuse a Windows ISO that isn't the one this VM stack is known to work with,
# BEFORE burning ~40 min on an install that can only fail. The wrong-ISO failure
# is invisible and expensive: 25H2 installs its first phase, then hangs at the
# first boot of the installed OS, and all the operator sees is a wait-for-ssh
# timeout (and, historically, a poisoned image that breaks every later build).
# The edition matters too: the unattend selects it by /IMAGE/NAME, and an ISO
# without it fails in Setup, headless, with no useful signal.
# Join the newline-separated edition list for display. NOTE: `paste -sd', '`
# looks right and is not — -d takes a LIST of delimiters used cyclically, so it
# emits "A,B C". Join on one char, then expand.
win_iso_editions_str() { printf '%s' "$WIN_ISO_EDITIONS" | paste -sd'|' - | sed 's/|/, /g'; }

check_windows_iso() {
  local iso="$1" found_release
  if [ -n "${WINDOWS_ISO_SKIP_CHECK:-}" ]; then
    echo ">>> WARNING: WINDOWS_ISO_SKIP_CHECK set — not verifying $(basename "$iso")."
    echo "    If this is not build $WIN_REQUIRED_BUILD ($(win_build_release "$WIN_REQUIRED_BUILD")), expect the install to hang at first boot."
    return 0
  fi
  echo ">>> checking the Windows ISO ($(basename "$iso"))"
  if ! win_iso_probe "$iso"; then
    win_die "could not identify $iso (no readable sources/install.wim metadata).
  This gate exists because the wrong ISO wastes ~40 min and yields an unbootable image.
  Verify it is the Windows 11 $(win_build_release "$WIN_REQUIRED_BUILD") ARM64 ISO, then re-run with WINDOWS_ISO_SKIP_CHECK=1 to bypass."
  fi
  found_release="$(win_build_release "$WIN_ISO_BUILD")"
  echo "    build $WIN_ISO_BUILD ($found_release); editions: $(win_iso_editions_str)"
  if [ "$WIN_ISO_BUILD" != "$WIN_REQUIRED_BUILD" ]; then
    win_die "wrong Windows ISO: $(basename "$iso") is build $WIN_ISO_BUILD ($found_release), but this VM stack requires build $WIN_REQUIRED_BUILD ($(win_build_release "$WIN_REQUIRED_BUILD")).
  25H2/26xx installs its first phase and then hangs FOREVER at the first boot of the installed OS
  on this QEMU/edk2 device layout (verified; see README.md \"Windows ISO\"). Use the 24H2 ARM64 ISO.
  To re-test a newer build deliberately: WIN_REQUIRED_BUILD=$WIN_ISO_BUILD ./setup.sh ..."
  fi
  if ! printf '%s\n' "$WIN_ISO_EDITIONS" | grep -qxF "$WIN_REQUIRED_EDITION"; then
    win_die "$(basename "$iso") does not contain the '$WIN_REQUIRED_EDITION' edition the unattend installs
  (it has: $(win_iso_editions_str)).
  Setup would fail headless with no useful signal. Use an ISO with that edition, or set
  WIN_REQUIRED_EDITION='<one of the above>' and match /IMAGE/NAME in packer/http/Autounattend.pkrtpl.xml."
  fi
  echo "    ok: $found_release + '$WIN_REQUIRED_EDITION' present"
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
  # 90 min: this boots the BASE image, which — unlike a hermetic post-provision
  # image — may still have a pending Windows update to apply before sshd serves
  # (an image built before the WU-disable below, or one whose provisioning
  # session staged one). That boot is slow but legitimate, and timing out on it
  # hard-kills the guest mid-servicing and damages the base image.
  win_wait_ssh 1080 || win_die "VM ssh did not come up (watch on VNC 5901)"
  win_scp_to "$here/packer/scripts/provision.ps1" "C:/Windows/Temp/provision.ps1"
  win_ssh "powershell -ExecutionPolicy Bypass -File C:/Windows/Temp/provision.ps1" \
    || win_die "provisioning failed — see output above"
  win_shutdown_vm
  echo ">>> re-provisioned: $IMAGE"
elif [ -n "$SKIP_BUILD" ] && [ -f "$IMAGE" ]; then
  echo ">>> reusing existing image: $IMAGE (--skip-build)"
else
  [ -f "$WINDOWS_ISO" ] || win_die "Windows 11 $(win_build_release "$WIN_REQUIRED_BUILD") ARM64 ISO required — pass --windows-iso PATH"
  [ -f "$VIRTIO_ISO" ]  || win_die "virtio-win.iso required — pass --virtio-iso PATH"
  if [ -f "$IMAGE" ] && [ -z "$FORCE" ]; then
    win_die "image already exists: $IMAGE — use --skip-build to reuse, or --force to rebuild"
  fi
  # Gate the ISO BEFORE --force deletes the existing image: a wrong ISO must not
  # cost the operator the image they already had.
  check_windows_iso "$WINDOWS_ISO"
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
