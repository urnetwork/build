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
  # The Windows ISO this VM stack REQUIRES: 11 24H2 == build 26100, Pro edition.
  # 25H2 (26200) installs fine and then hangs forever at the first boot of the
  # installed OS on this exact edk2/device layout (README "Windows ISO"), and the
  # edition must exist in the ISO or Setup fails — the unattend selects it by
  # /IMAGE/NAME. setup.sh gates on both via win_iso_probe. Overridable for a
  # future deliberate re-test of a newer build.
  WIN_REQUIRED_BUILD="${WIN_REQUIRED_BUILD:-26100}"
  WIN_REQUIRED_EDITION="${WIN_REQUIRED_EDITION:-Windows 11 Pro}"
  WIN_DIR="${WIN_DIR:-C:/build/urnetwork}"
  # Same dir in cwRsync's cygwin path form, for rsync's remote target.
  WIN_DIR_UNIX="${WIN_DIR_UNIX:-/cygdrive/c/build/urnetwork}"
  # Every ssh/scp/rsync to the VM is strictly non-interactive: publickey only,
  # with exactly the build key (IdentitiesOnly — an ssh-agent's keys are
  # otherwise offered first and can exhaust the server's MaxAuthTries before
  # the -i key is tried), and BatchMode so a rejected key FAILS the call
  # instead of falling back to Windows OpenSSH's password prompt — that prompt
  # goes to /dev/tty, survives >/dev/null redirects, and blocks forever, which
  # turned win_wait_ssh's bounded loop into an unbounded silent hang.
  # ServerAlive*: without keepalives an ESTABLISHED session whose guest dies
  # underneath it (observed: Windows servicing rebooting the VM mid-build)
  # hangs the client forever on the dead TCP — the build wedges silently
  # instead of failing. 15s x 4 = dead sessions fail within ~60s.
  WIN_SSH_OPTS=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null
    -o LogLevel=ERROR -o ConnectTimeout=5 -o BatchMode=yes -o IdentitiesOnly=yes
    -o PreferredAuthentications=publickey -o NumberOfPasswordPrompts=0
    -o ServerAliveInterval=15 -o ServerAliveCountMax=4)
  WIN_QEMU_PID=""
  WIN_RUN_DIR=""
  WIN_MON_SOCK=""
}

# Mirror the build server's whole build home into the VM verbatim — the VM builds
# the exact local state run.sh set up (all repos, on their correct branches),
# exactly like the Linux container's bind mount. A pinned cwRsync + a cmd ssh shell
# are installed in the image (provision.ps1). Arg: HOST_BUILD_DIR (e.g. $BUILD_HOME).
win_sync_source() {
  local src="$1"
  [ -d "$src" ] || win_die "build dir not found: $src"
  # Allowlist: the VM build needs only four repos — the app (windows/) and the
  # cgo SDK with its local module deps (sdk/ + connect/ + glog/, wired by
  # sdk/cgo's replace directives). Sync just those, not the whole build home:
  # BUILD_HOME also holds the VM's OWN ~24GB disk image (all/windows/output/
  # *.qcow2 — copying the VM into itself), ~4GB of .git, node_modules, and every
  # other platform's repo, which made openrsync-over-cwRsync crawl (the "stuck at
  # syncing the build home" hang). Within the four, drop the usual .git
  # (build-sdk.ps1 passes -buildvcs=false, so Go doesn't need it) + node_modules.
  # --delete only ever touches the VM, never $src; --progress shows per-file
  # activity so the sync isn't a silent wait. To add a repo, list it here.
  rsync -a --delete --progress \
    --exclude=.git \
    --exclude=node_modules \
    -e "ssh -i $SSH_KEY -p $SSH_PORT ${WIN_SSH_OPTS[*]}" \
    "$src/windows" "$src/sdk" "$src/connect" "$src/glog" \
    "builder@127.0.0.1:$WIN_DIR_UNIX/"
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

# ssh/scp to the VM (builder@127.0.0.1:$SSH_PORT). Throwaway host key;
# non-interactive by construction (WIN_SSH_OPTS, see win_init).
win_ssh() {
  ssh -i "$SSH_KEY" -p "$SSH_PORT" "${WIN_SSH_OPTS[@]}" builder@127.0.0.1 "$@"
}
win_scp_to()   { scp -i "$SSH_KEY" -P "$SSH_PORT" "${WIN_SSH_OPTS[@]}" "$1" "builder@127.0.0.1:$2"; }
win_scp_from() { scp -i "$SSH_KEY" -P "$SSH_PORT" "${WIN_SSH_OPTS[@]}" "builder@127.0.0.1:$1" "$2"; }

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

# Send one command to the VM's QEMU monitor and echo its reply. Pinned to
# /usr/bin/nc: a PATH netcat variant (ncat, gnu) can sit forever reading the
# open monitor connection after stdin EOF, which would wedge the caller's loop
# on its first send; Apple's nc with an idle timeout always returns.
win_mon_out() {  # win_mon_out MON_SOCK CMD  -> monitor reply on stdout
  local sock="$1" cmd="$2"
  [ -S "$sock" ] || return 0
  printf '%s\n' "$cmd" | /usr/bin/nc -w 1 -U "$sock" 2>/dev/null || true
}

# Send a monitor command, discarding the reply.
win_mon() {  # win_mon MON_SOCK CMD
  win_mon_out "$1" "$2" >/dev/null
}

# Read one drive's cumulative byte counter from `info blockstats`. Args:
# MON_SOCK DRIVE_ID FIELD(rd_bytes|wr_bytes). Echoes an integer, empty if not
# yet readable. Device lines are "installcd: rd_bytes=N wr_bytes=N ...".
win_mon_blockbytes() {  # -> integer (maybe empty)
  win_mon_out "$1" "info blockstats" | tr -d '\r' \
    | grep "$2:" | grep -oE "$3=[0-9]+" | head -1 | cut -d= -f2
}

# Answer the one-time edk2 "Press any key to boot from CD or DVD..." El-Torito
# prompt, then STOP. The prompt appears only on the FIRST boot (empty NVMe),
# opens ~9s after power-on, and stays up only ~4-5s; miss it and edk2 falls
# through to the UEFI shell ("Shell>") and hangs forever — the install never
# starts. But the prompt's wall-clock position varies with host speed, and once
# Windows Setup's GUI paints, a stray Enter lands on its focused Cancel button
# and opens a "quit setup?" dialog (seen on a fast build host where the GUI
# appeared within 30s). A fixed tap window is fragile at both ends, so instead
# tap Enter once a second but stop the instant the installer is demonstrably
# booting — the CD streams boot.wim (installcd rd_bytes jumps past ~50MB) or
# Setup starts writing the NVMe (sysdisk wr_bytes climbs). Both cross well
# before any interactive screen, so no tap ever reaches the Cancel button. The
# iteration cap only backstops a monitor/parse failure.
win_press_any_key() {  # win_press_any_key MON_SOCK
  local sock="$1" i rd wr
  for i in $(seq 1 60); do
    rd="$(win_mon_blockbytes "$sock" installcd rd_bytes)"
    wr="$(win_mon_blockbytes "$sock" sysdisk wr_bytes)"
    if [ "${rd:-0}" -gt 52428800 ] || [ "${wr:-0}" -gt 10485760 ]; then
      return 0   # installer is booting/installing — the prompt was answered
    fi
    win_mon "$sock" "sendkey ret"
    sleep 1
  done
}

# Map a Windows build number to its marketing release name, for readable errors.
win_build_release() {
  case "$1" in
    26200) echo "25H2" ;;
    26100) echo "24H2" ;;
    22631) echo "23H2" ;;
    22621) echo "22H2" ;;
    22000) echo "21H2" ;;
    *)     echo "unknown release" ;;
  esac
}

# Read a little-endian uint64 out of a file at a byte offset. `od -tu8` decodes
# in HOST byte order, which is little-endian on the Apple-Silicon build host —
# the same order WIM stores these fields in, so no swapping is needed.
win_le64() { dd if="$1" bs=1 skip="$2" count=8 2>/dev/null | od -An -tu8 | tr -d ' \n'; }

# Identify a Windows install ISO: which build it carries, and which editions.
# Sets WIN_ISO_BUILD (e.g. 26100) + WIN_ISO_EDITIONS (newline-separated /IMAGE/NAME
# values, what the unattend selects on). Non-zero if the ISO can't be identified.
#
# The build number is NOT in any text file on the media — sources/idwbinfo.txt and
# cversion.ini are byte-identical between the 24H2 and 25H2 ARM64 ISOs (verified),
# and filenames are operator-chosen, so neither can be trusted. The authoritative
# source is the XML metadata resource of sources/install.wim, whose location is in
# the WIM header: rhXmlData at 0x48 is {size:56 bits, flags:8 bits} then offset at
# 0x50. The blob is UTF-16LE. It sits ~5GB into the file, so seek by 512-byte
# blocks (a bs=1 skip would read gigabytes byte-by-byte) and trim the remainder.
win_iso_probe() {  # win_iso_probe ISO
  local iso="$1" mp wim raw off size flags blk rem xml
  WIN_ISO_BUILD=""
  WIN_ISO_EDITIONS=""
  mp="$(mktemp -d)" || return 1
  if ! hdiutil attach -readonly -nobrowse -mountpoint "$mp" "$iso" >/dev/null 2>&1; then
    rmdir "$mp" 2>/dev/null
    return 1
  fi
  wim="$mp/sources/install.wim"
  [ -f "$wim" ] || wim="$mp/sources/install.esd"
  if [ -f "$wim" ]; then
    raw="$(win_le64 "$wim" 72)"   # 0x48: size + flags
    off="$(win_le64 "$wim" 80)"   # 0x50: offset
    if [ -n "$raw" ] && [ -n "$off" ] && [ "$off" -gt 0 ] 2>/dev/null; then
      size=$(( raw & 0x00FFFFFFFFFFFFFF ))
      flags=$(( (raw >> 56) & 0xFF ))
      # bit 0x04 = compressed. The XML resource is stored raw in practice; if a
      # future image compresses it we cannot inflate it here (no wimlib on macOS)
      # and the caller falls back to its skip/override path rather than guessing.
      if [ $(( flags & 0x04 )) -eq 0 ] && [ "$size" -gt 0 ] && [ "$size" -le 10485760 ]; then
        blk=$(( off / 512 ))
        rem=$(( off % 512 ))
        xml="$(dd if="$wim" bs=512 skip="$blk" count=$(( (rem + size + 511) / 512 )) 2>/dev/null \
               | tail -c +$(( rem + 1 )) | head -c "$size" \
               | iconv -f UTF-16LE -t UTF-8 2>/dev/null | tr -d '\r')"
        WIN_ISO_BUILD="$(printf '%s' "$xml" | grep -oE '<BUILD>[0-9]+</BUILD>' | head -1 | tr -dc '0-9')"
        WIN_ISO_EDITIONS="$(printf '%s' "$xml" | grep -oE '<NAME>[^<]*</NAME>' | sed -E 's|</?NAME>||g' | sort -u)"
      fi
    fi
  fi
  hdiutil detach "$mp" >/dev/null 2>&1 || hdiutil detach -force "$mp" >/dev/null 2>&1
  rmdir "$mp" 2>/dev/null
  [ -n "$WIN_ISO_BUILD" ]
}

# One-time: install Windows unattended into $IMAGE. Needs the two ISOs.
# Args: WINDOWS_ISO VIRTIO_ISO. Leaves the installed image at $IMAGE.
win_install_image() {
  local wiso="$1" viso="$2"
  WIN_RUN_DIR="$(mktemp -d)"
  local efivars="$WIN_RUN_DIR/efivars.fd"
  WIN_MON_SOCK="$WIN_RUN_DIR/mon.sock"
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
    -monitor "unix:$WIN_MON_SOCK,server,nowait" -serial null &
  WIN_QEMU_PID=$!

  # Answer the boot prompt in the background while the install runs.
  ( win_press_any_key "$WIN_MON_SOCK" ) &

  echo ">>> waiting for the unattended install to finish (install + OOBE + first logon; can take a while)"
  # 3h budget (nominal ~5s/try). The install legitimately runs long: phase 1
  # streams the ~5GB ISO through the emulated usb-bot CD, and phase 2 + OOBE +
  # first logon vary a lot with host speed/load. A 480-try (~40 min) budget
  # killed a REAL mid-phase-2 install on the production build host — leaving a
  # half-installed image that every later build.sh boot then hung on — so err
  # far on the side of patience; a wedged install is cheap to abort by hand.
  if ! win_wait_ssh 2160; then
    # Grab what the VM was showing so a headless failure is diagnosable later.
    mkdir -p "$WIN_HERE/output"
    win_mon "$WIN_MON_SOCK" "screendump $WIN_HERE/output/install-fail.ppm"
    echo "install did not come up; last screen saved to $WIN_HERE/output/install-fail.ppm" >&2
    return 1
  fi
  # Leave the VM running: the install writes directly to $IMAGE (the NVMe disk),
  # so the caller provisions the toolchain over ssh into the same image, then
  # shuts down (win_shutdown_vm) to finalize it.
  echo ">>> install up (leaving the VM running for provisioning)"
}

# Boot a copy-on-write overlay of the installed $IMAGE headless (release builds).
# Same device model as the install (minus the CDs), so it boots — including the
# monitor socket + password VNC (5901, pw 'windows'): a boot that never reaches
# ssh is undiagnosable on a headless host without them, so build.sh screendumps
# over the monitor when win_wait_ssh times out, and an operator can watch a live
# build VM with  open vnc://127.0.0.1:5901  at any time.
win_boot_vm() {
  WIN_RUN_DIR="$(mktemp -d)"
  local overlay="$WIN_RUN_DIR/overlay.qcow2" efivars="$WIN_RUN_DIR/efivars.fd"
  WIN_MON_SOCK="$WIN_RUN_DIR/mon.sock"
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
    -object secret,id=vncpw,data=windows -vnc 127.0.0.1:1,password-secret=vncpw \
    -monitor "unix:$WIN_MON_SOCK,server,nowait" -serial null &
  WIN_QEMU_PID=$!
}

# Boot the installed base image READ-WRITE (no overlay) to modify it in place —
# used to re-run provisioning on an already-installed image without reinstalling
# the OS. Sets WIN_QEMU_PID + WIN_RUN_DIR. Watchable on VNC 5901 (pw 'windows').
win_boot_image_rw() {
  WIN_RUN_DIR="$(mktemp -d)"
  local efivars="$WIN_RUN_DIR/efivars.fd"
  WIN_MON_SOCK="$WIN_RUN_DIR/mon.sock"
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
    -object secret,id=vncpw,data=windows -vnc 127.0.0.1:1,password-secret=vncpw \
    -monitor "unix:$WIN_MON_SOCK,server,nowait" &
  WIN_QEMU_PID=$!
}

# Run one win_ssh command under a hard deadline (seconds). ConnectTimeout only
# bounds connect + banner; a stalled kex/auth (half-up sshd in a still-booting
# guest) otherwise blocks the client forever. Returns 124 on timeout, else the
# ssh exit code. stdin is detached so a backgrounded probe never reads the tty.
win_ssh_probe() {  # win_ssh_probe DEADLINE CMD...
  local deadline="$1" pid t=0
  shift
  win_ssh "$@" </dev/null & pid=$!
  while kill -0 "$pid" 2>/dev/null; do
    if [ "$t" -ge "$deadline" ]; then
      kill -9 "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
      return 124
    fi
    sleep 1
    t=$((t + 1))
  done
  wait "$pid"
}

# Wait for the VM ssh service. Arg: tries (default 360, x5s = 30 min). Non-zero
# on timeout. Every probe is hard-bounded (win_ssh_probe), so the loop itself can
# never wedge; on timeout the last probe reruns unsuppressed so the log names
# the cause (Connection refused/timeout = guest or sshd never came up;
# "timed out during banner exchange" = the guest is up but sshd isn't serving
# yet, classically a boot still applying Windows updates; Permission denied =
# the local key doesn't match the image's baked key).
#
# The default is deliberately generous. A hermetic image boots in well under a
# minute, so a long ceiling costs nothing on the happy path — but a boot that
# has to chew through a pending update can take 30+ min, and the old 15-min
# ceiling turned that into a teardown that killed the guest MID-UPDATE.
win_wait_ssh() {
  local tries="${1:-360}" i
  for ((i = 0; i < tries; i++)); do
    kill -0 "$WIN_QEMU_PID" 2>/dev/null || { echo "qemu exited early" >&2; return 1; }
    if win_ssh_probe 15 "echo ok" >/dev/null 2>&1; then return 0; fi
    sleep 5
  done
  echo "VM ssh still failing after $tries tries; last probe error:" >&2
  win_ssh_probe 15 "echo ok" >/dev/null || true
  return 1
}

# Graceful shutdown, then hard kill; always removes the run dir. Idempotent.
# The graceful window is 5 min, not 60s: a hard kill is a yanked power cord to
# the guest, and win_install_image/win_boot_image_rw write to the BASE image, so
# a kill during a Windows servicing pass can leave it half-updated (recoverable
# only by a slow "undoing changes" boot, or not at all). A guest that is merely
# slow to shut down must not be killed; 5 min of patience is far cheaper than
# reinstalling the image. The loop exits as soon as qemu does, so a healthy
# shutdown still returns in seconds.
win_shutdown_vm() {
  if [ -n "${WIN_QEMU_PID:-}" ] && kill -0 "$WIN_QEMU_PID" 2>/dev/null; then
    win_ssh "shutdown /s /t 0 /f" 2>/dev/null || true
    local _
    for _ in $(seq 1 300); do kill -0 "$WIN_QEMU_PID" 2>/dev/null || break; sleep 1; done
    kill -0 "$WIN_QEMU_PID" 2>/dev/null && kill -TERM "$WIN_QEMU_PID" 2>/dev/null || true
  fi
  [ -n "${WIN_RUN_DIR:-}" ] && rm -rf "$WIN_RUN_DIR"
  WIN_QEMU_PID=""
  WIN_RUN_DIR=""
  WIN_MON_SOCK=""
}
