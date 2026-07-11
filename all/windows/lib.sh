# Shared helpers for the local QEMU ARM Windows build VM. Sourced by build.sh
# (per-release MSI build) and setup.sh (one-time image build + smoke test).
#
# The image is built by driving QEMU directly with the device layout validated
# interactively (ramfb + usb-bot CD-ROM + NVMe disk + USB kbd — all inbox, no
# driver injection), NOT via Packer, whose qemu builder has no nvme disk option
# and conflicts on custom -drive/-device. build.sh then boots a CoW overlay of
# the resulting image and runs windows/app/build.ps1 over ssh.
#
# Not executable on its own. Callers set `set -euo pipefail`, then win_init.
# SPDX-License-Identifier: MPL-2.0

win_init() {
  WIN_HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  IMAGE="${IMAGE:-$WIN_HERE/output/windows-arm64.qcow2}"
  SSH_KEY="${SSH_KEY:-$WIN_HERE/.ssh/id_ed25519}"
  SSH_PORT="${SSH_PORT:-2222}"
  UEFI_CODE="${UEFI_CODE:-/opt/homebrew/share/qemu/edk2-aarch64-code.fd}"
  UEFI_VARS_TEMPLATE="${UEFI_VARS_TEMPLATE:-/opt/homebrew/share/qemu/edk2-arm-vars.fd}"
  CPUS="${CPUS:-6}"
  MEM="${MEM:-8192}"
  DISK_SIZE="${DISK_SIZE:-90G}"
  WIN_DIR="${WIN_DIR:-C:/build/urnetwork}"
  # Same dir in cwRsync's cygwin path form, for rsync's remote target.
  WIN_DIR_UNIX="${WIN_DIR_UNIX:-/cygdrive/c/build/urnetwork}"
  WIN_QEMU_PID=""
  WIN_RUN_DIR=""
}

# Mirror the build server's whole build home into the VM verbatim — the VM builds
# the exact local state run.sh set up (all repos, on their correct branches),
# exactly like the Linux container's bind mount. A pinned cwRsync + a cmd ssh shell
# are installed in the image (provision.ps1). Arg: HOST_BUILD_DIR (e.g. $BUILD_HOME).
win_sync_source() {
  local src="$1"
  [ -d "$src" ] || win_die "build dir not found: $src"
  rsync -a --delete \
    -e "ssh -i $SSH_KEY -p $SSH_PORT -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR" \
    "$src/" "builder@127.0.0.1:$WIN_DIR_UNIX/"
}

win_die() { echo "ERROR: $*" >&2; exit 1; }

win_require_tools() {
  local t missing=0
  for t in "$@"; do
    command -v "$t" >/dev/null 2>&1 || { echo "  missing: $t" >&2; missing=1; }
  done
  return $missing
}

win_ensure_ssh_key() {
  if [ ! -f "$SSH_KEY" ]; then
    mkdir -p "$(dirname "$SSH_KEY")"
    ssh-keygen -t ed25519 -N '' -f "$SSH_KEY" >/dev/null
    echo ">>> generated build key $SSH_KEY"
  fi
}

# ssh/scp to the VM (builder@127.0.0.1:$SSH_PORT). Throwaway host key.
win_ssh() {
  ssh -i "$SSH_KEY" -p "$SSH_PORT" \
    -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o ConnectTimeout=5 -o LogLevel=ERROR builder@127.0.0.1 "$@"
}
win_scp_to()   { scp -i "$SSH_KEY" -P "$SSH_PORT" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR "$1" "builder@127.0.0.1:$2"; }
win_scp_from() { scp -i "$SSH_KEY" -P "$SSH_PORT" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR "builder@127.0.0.1:$1" "$2"; }

# Build a small CD image carrying autounattend.xml (rendered with the ssh key).
# Windows scans removable media for autounattend.xml at the root.
win_make_autounattend_iso() {  # -> echoes the iso path
  local out="$WIN_RUN_DIR/autounattend.iso" stage="$WIN_RUN_DIR/unattend"
  mkdir -p "$stage"
  sed "s|\${ssh_public_key}|$(cat "$SSH_KEY.pub")|g" \
    "$WIN_HERE/packer/http/Autounattend.pkrtpl.xml" > "$stage/autounattend.xml"
  hdiutil makehybrid -quiet -o "$out" -iso -joliet -default-volume-name AUTOUNATTEND "$stage"
  echo "$out"
}

# Send Enter to the VM's QEMU monitor for a while, to answer the Windows
# "Press any key to boot from CD or DVD..." El-Torito prompt during install.
win_press_any_key() {  # win_press_any_key MON_SOCK
  local sock="$1" i
  for i in $(seq 1 25); do
    [ -S "$sock" ] && printf 'sendkey ret\n' | nc -U "$sock" >/dev/null 2>&1
    sleep 2
  done
}

# One-time: install Windows unattended into $IMAGE. Needs the two ISOs.
# Args: WINDOWS_ISO VIRTIO_ISO. Leaves the installed image at $IMAGE.
win_install_image() {
  local wiso="$1" viso="$2"
  WIN_RUN_DIR="$(mktemp -d)"
  local efivars="$WIN_RUN_DIR/efivars.fd" mon="$WIN_RUN_DIR/mon.sock"
  cp "$UEFI_VARS_TEMPLATE" "$efivars"
  mkdir -p "$(dirname "$IMAGE")"
  rm -f "$IMAGE"
  qemu-img create -f qcow2 "$IMAGE" "$DISK_SIZE" >/dev/null
  local unattend; unattend="$(win_make_autounattend_iso)"

  echo ">>> booting the unattended Windows install (headless; watch: open vnc://127.0.0.1:5901  pw 'windows')"
  # Proven layout: ramfb display, USB kbd, NVMe system disk, usb-bot install
  # CD-ROM, autounattend + virtio-driver CDs on USB, virtio-net. NOTE: no
  # bootindex on the CD — like real hardware, the empty NVMe makes edk2 boot the
  # USB installer first, and after install Windows' own boot entry wins (a forced
  # CD bootindex instead loops the installer on every reboot -> UEFI shell).
  qemu-system-aarch64 \
    -machine virt -accel hvf -cpu host -smp "$CPUS" -m "$MEM" \
    -drive if=pflash,format=raw,readonly=on,file="$UEFI_CODE" \
    -drive if=pflash,format=raw,file="$efivars" \
    -device ramfb -device qemu-xhci -device usb-kbd -device usb-tablet \
    -device nvme,drive=sysdisk,serial=urnvme \
    -drive if=none,id=sysdisk,file="$IMAGE",format=qcow2 \
    -device usb-bot,id=usbcd \
    -device scsi-cd,bus=usbcd.0,drive=installcd \
    -drive if=none,id=installcd,media=cdrom,file="$wiso" \
    -device usb-storage,drive=unattendcd \
    -drive if=none,id=unattendcd,media=cdrom,file="$unattend" \
    -device usb-storage,drive=virtiocd \
    -drive if=none,id=virtiocd,media=cdrom,file="$viso" \
    -device virtio-net-pci,netdev=net0 \
    -netdev "user,id=net0,hostfwd=tcp::${SSH_PORT}-:22" \
    -object secret,id=vncpw,data=windows -vnc 127.0.0.1:1,password-secret=vncpw \
    -monitor "unix:$mon,server,nowait" -serial null &
  WIN_QEMU_PID=$!

  # Answer the boot prompt in the background while the install runs.
  ( win_press_any_key "$mon" ) &

  echo ">>> waiting for the unattended install to finish (install + OOBE + first logon; can take a while)"
  win_wait_ssh 480 || { echo "install did not come up (watch VNC 5901)" >&2; return 1; }
  # Leave the VM running: the install writes directly to $IMAGE (the NVMe disk),
  # so the caller provisions the toolchain over ssh into the same image, then
  # shuts down (win_shutdown_vm) to finalize it.
  echo ">>> install up (leaving the VM running for provisioning)"
}

# Boot a copy-on-write overlay of the installed $IMAGE headless (release builds).
# Same device model as the install (minus the CDs), so it boots.
win_boot_vm() {
  WIN_RUN_DIR="$(mktemp -d)"
  local overlay="$WIN_RUN_DIR/overlay.qcow2" efivars="$WIN_RUN_DIR/efivars.fd"
  qemu-img create -f qcow2 -F qcow2 -b "$IMAGE" "$overlay" >/dev/null
  cp "$UEFI_VARS_TEMPLATE" "$efivars"
  qemu-system-aarch64 \
    -machine virt -accel hvf -cpu host -smp "$CPUS" -m "$MEM" \
    -drive if=pflash,format=raw,readonly=on,file="$UEFI_CODE" \
    -drive if=pflash,format=raw,file="$efivars" \
    -device ramfb -device qemu-xhci -device usb-kbd \
    -device nvme,drive=sysdisk,serial=urnvme \
    -drive if=none,id=sysdisk,file="$overlay",format=qcow2 \
    -device virtio-net-pci,netdev=net0 \
    -netdev "user,id=net0,hostfwd=tcp::${SSH_PORT}-:22" \
    -display none -serial null &
  WIN_QEMU_PID=$!
}

# Boot the installed base image READ-WRITE (no overlay) to modify it in place —
# used to re-run provisioning on an already-installed image without reinstalling
# the OS. Sets WIN_QEMU_PID + WIN_RUN_DIR. Watchable on VNC 5901 (pw 'windows').
win_boot_image_rw() {
  WIN_RUN_DIR="$(mktemp -d)"
  local efivars="$WIN_RUN_DIR/efivars.fd"
  cp "$UEFI_VARS_TEMPLATE" "$efivars"
  qemu-system-aarch64 \
    -machine virt -accel hvf -cpu host -smp "$CPUS" -m "$MEM" \
    -drive if=pflash,format=raw,readonly=on,file="$UEFI_CODE" \
    -drive if=pflash,format=raw,file="$efivars" \
    -device ramfb -device qemu-xhci -device usb-kbd \
    -device nvme,drive=sysdisk,serial=urnvme \
    -drive if=none,id=sysdisk,file="$IMAGE",format=qcow2 \
    -device virtio-net-pci,netdev=net0 \
    -netdev "user,id=net0,hostfwd=tcp::${SSH_PORT}-:22" \
    -object secret,id=vncpw,data=windows -vnc 127.0.0.1:1,password-secret=vncpw &
  WIN_QEMU_PID=$!
}

# Wait for the VM ssh service. Arg: tries (default 180, x5s). Non-zero on timeout.
win_wait_ssh() {
  local tries="${1:-180}" i
  for ((i = 0; i < tries; i++)); do
    kill -0 "$WIN_QEMU_PID" 2>/dev/null || { echo "qemu exited early" >&2; return 1; }
    if win_ssh "echo ok" >/dev/null 2>&1; then return 0; fi
    sleep 5
  done
  return 1
}

# Graceful shutdown, then hard kill; always removes the run dir. Idempotent.
win_shutdown_vm() {
  if [ -n "${WIN_QEMU_PID:-}" ] && kill -0 "$WIN_QEMU_PID" 2>/dev/null; then
    win_ssh "shutdown /s /t 0 /f" 2>/dev/null || true
    local _
    for _ in $(seq 1 60); do kill -0 "$WIN_QEMU_PID" 2>/dev/null || break; sleep 1; done
    kill -0 "$WIN_QEMU_PID" 2>/dev/null && kill -TERM "$WIN_QEMU_PID" 2>/dev/null || true
  fi
  [ -n "${WIN_RUN_DIR:-}" ] && rm -rf "$WIN_RUN_DIR"
  WIN_QEMU_PID=""
  WIN_RUN_DIR=""
}
