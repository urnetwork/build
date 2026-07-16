# Windows build environment — options & decision

The Windows MSI must be built on Windows (msbuild + WDK + WiX). The build host is
an **Apple-Silicon Mac**. This doc records what does and doesn't work for running
that Windows build environment from the Mac, and how it wires into `../run.sh`.

Key fact that shrinks the problem: we only need **one ARM64 Windows VM**. It
cross-builds *both* the arm64 and x64 MSIs (WDK + MSVC target both from an ARM
host), and Apple Silicon can **hardware-accelerate an ARM Windows guest** (single
level of virtualization via Hypervisor.framework — not nested virt). So the whole
question is "how do we run/automate one accelerated ARM Windows VM on the Mac."

## Option 1 — dockur/windows (Docker) ❌ ruled out

[dockur/windows](https://github.com/dockur/windows) runs Windows in a QEMU VM
*inside* a Docker container and requires **`/dev/kvm`**. That means nested
virtualization, and **Docker Desktop for Mac does not expose `/dev/kvm`**
(verified: dockur's own docs + [issue #851](https://github.com/dockur/windows/issues/851)
— `error gathering device information while adding custom device '/dev/kvm': no such file or directory`).

The only macOS path to `/dev/kvm` is Apple's newer **`container`** runtime (not
Docker Desktop) on **M3+ / macOS 15+** with a KVM-enabled kernel
([apple/container](https://github.com/apple/container/blob/main/docs/container-machine.md)).
That's unproven for a Windows-in-QEMU-in-container stack and hardware-gated, so
it's not a foundation for the release pipeline. **dockur is out.**

## Option 2 — QEMU + Packer (local, image-as-code) ✅ recommended

QEMU with `-accel hvf` runs an accelerated Windows 11 ARM64 guest on Apple
Silicon (UTM/ACVM are just wrappers around this). [Packer's QEMU
builder](https://developer.hashicorp.com/packer/integrations/hashicorp/qemu/latest/components/builder/qemu)
does headless, `Autounattend.xml`-driven Windows installs and runs on Apple
Silicon.

- **Provision once (Packer):** boot the Win11 ARM64 ISO with an autounattend that
  installs VS Build Tools + WDK + WiX + OpenSSH Server, then export a `qcow2`.
- **Each release (`build.sh`):** boot that image headless with QEMU (HVF), wait
  for ssh, hand off to the *existing* `windows/app/build.ps1` over ssh, copy the
  MSIs back, shut the VM down.
- **Pros:** fully reproducible (image as code), scriptable, no separate physical
  machine, closest to the "container" ethos.
- **Cons:** must source a Win11 ARM64 ISO; author the autounattend/template;
  QEMU needs virtio drivers for net/storage; the known `qemu-img convert` macOS
  quirk (keep output format == input to avoid the corrupt-image bug).

## Option 3 — UTM (local, prebuilt bundle + utmctl) ✅ simpler alternative

[UTM](https://mac.getutm.app) is a Mac wrapper over QEMU/Virtualization.framework
with a CLI (`utmctl`). Build the ARM Windows VM once in the UTM GUI, then
`utmctl start/stop` it around the existing ssh `build.ps1` flow.

- **Pros:** simplest to stand up; same acceleration; `utmctl` scripts cleanly.
- **Cons:** the VM is a hand-built bundle (less reproducible than Packer); UTM is
  a GUI-oriented tool on the build host.

## Option 4 — status quo: remote ARM Windows VM over ssh

What `run.sh` does today (`WINDOWS_BUILD_HOST`): a hand-maintained ARM Windows
machine reached over ssh. Works, but not reproducible and needs a separate box.

## Decision: Option 2 (QEMU, driven directly) — implemented

We drive QEMU directly, **not** Packer — its qemu builder has no `nvme` disk
option and fights custom `-drive`/`-device`, and the aarch64 device layout needed
real interactive iteration. `windows/app/build.ps1` and the ssh handoff are
unchanged; only the VM lifecycle (boot → wait-for-ssh → shutdown) wraps around it,
replacing the remote `WINDOWS_BUILD_HOST` with a local HVF-accelerated VM.

The proven device model (validated interactively — all **inbox** drivers, no
injection): `ramfb` display + USB keyboard (aarch64 `virt` has no default
GPU/keyboard), an **NVMe** system disk, a `usb-bot` SCSI CD-ROM for the installer,
and `virtio-net` for the NIC — the one driver not inbox, so `NetKVM` is installed
from the virtio ISO at first logon by the autounattend.

### Source delivery: rsync, not clone

The VM builds the build server's **exact local state**. `build.sh` rsyncs the
whole `BUILD_HOME` (all repos, already on the version branch `run.sh` checked out)
into the VM — conceptually the same as the Linux container's bind mount, but the
VM can't mount a host dir. No clone, no GitHub, no ssh key baked into the image.
`provision.ps1` installs a **pinned cwRsync** (via Chocolatey — it keeps every
version permanently, so the environment is reproducible) and sets OpenSSH's default
shell to `cmd`, so an incoming `rsync --server /cygdrive/c/...` reaches the cygwin
rsync with its path unmangled. Our own ssh calls invoke `powershell -File …`
explicitly, so they're unaffected.

### Files

| File | Role |
|---|---|
| `setup.sh` | **one-time setup + smoke test** — installs Windows unattended, provisions the toolchain, verifies it. Run this first. |
| `lib.sh` | shared VM lifecycle (install, boot CoW overlay, boot-in-place, **rsync source in**, ssh/scp, teardown) — sourced by `setup.sh` + `build.sh` |
| `build.sh` | per-release: boot a CoW overlay, **rsync `BUILD_HOME` in**, deliver the SDK zip, run `build.ps1`, retrieve MSIs, shut down |
| `smoke-test.ps1` | run in the VM by `setup.sh`: checks MSVC (ARM64+x64), Windows SDK, WDK, WiX, git, **rsync + the `cmd` ssh shell** |
| `packer/http/Autounattend.pkrtpl.xml` | unattended install; bakes the stable ssh key, sets locale, enables OpenSSH, installs NetKVM at first logon |
| `packer/scripts/provision.ps1` | installs VS Build Tools (ARM64+x64) + WDK + WiX + git + a pinned cwRsync; sets the `cmd` ssh shell |

The `packer/` directory name is vestigial — only the autounattend template and
`provision.ps1` are used; there is no Packer build.

### One-time: set up + smoke-test the image

```bash
brew install qemu
# installs Windows unattended, provisions the toolchain, smoke-tests it
# (autogenerates the VM ssh key). Watch on  open vnc://127.0.0.1:5901  (pw windows):
./setup.sh \
  --windows-iso ~/isos/Win11_ARM64.iso \
  --virtio-iso  ~/isos/virtio-win.iso
# re-run just the smoke test on an existing image:   ./setup.sh --skip-build
# re-provision without reinstalling Windows:         ./setup.sh --reprovision
# leave the VM up to debug over ssh:                 ./setup.sh --skip-build --keep-up
```

`setup.sh` and `build.sh` share `lib.sh`, so a green smoke test means `build.sh`
boots the same working VM.

### Per release (automatic)

`build/all/build-windows.sh` (run.sh's windows build part, which also runs
standalone on the local branches as-is) calls `build.sh` with
`OUT_DIR`/`VERSION`/`SDK_VERSION` (and the exported `BUILD_HOME`). The cgo SDK
builds in the VM (`windows/build-sdk.ps1`), then the MSIs.
It boots a copy-on-write overlay of the image (base stays pristine) and rsyncs
the build home in, so releases build the exact local state. MSIs are uploaded
to the GitHub release; **Store submission is manual.**

### First-run tuning (cannot be verified on the macOS host)

aarch64 Windows-on-QEMU is finicky; the knobs most likely to need adjusting:

- **UEFI firmware** paths (`UEFI_CODE` / `UEFI_VARS_TEMPLATE`, default the Homebrew
  qemu paths) — these + the `-cpu`/`-machine` model live in `lib.sh` and must match
  between install (`win_install_image`) and boot (`win_boot_vm`).
- **NetKVM path** in the autounattend's first-logon command must match your
  `virtio-win.iso` layout (`NetKVM\w11\ARM64`) — the only non-inbox driver.
- **edition name** in the unattend (`Windows 11 Pro`) must exist in your ISO.
- **rsync in the VM** — `provision.ps1` pins cwRsync (`$rsyncVersion` via Chocolatey)
  + sets the `cmd` ssh shell; the smoke test flags either if missing. If a build-time
  sync errors on the remote path, confirm cwRsync's cygwin form (`WIN_DIR_UNIX`
  defaults to `/cygdrive/c/build/urnetwork`).

### Windows ISO

Microsoft publishes an official **Windows 11 ARM64 ISO** (Software Download site).
Activation for a private build VM is the operator's call.

**Use the 24H2 ARM64 ISO, not 25H2.** The **25H2** ARM64 build (e.g.
`Win11_25H2_English_Arm64_v2.iso`) installs its first phase fine but then
**hangs at the firmware splash on the first boot of the installed OS** — the
display freezes on the TianoCore/"Start boot option" screen with a vCPU pinned
and no further disk writes; the install never reaches OOBE or ssh, so
`setup.sh` times out at `win_wait_ssh`. The **24H2** ARM64 build
(`Win11_24H2_English_Arm64.iso`) boots through to the desktop and runs the
FirstLogonCommands (NetKVM + sshd) on the same QEMU/edk2 device layout, so the
image builds. Verified 2026-07-12 by installing both on the identical layout
(edk2 `edk2-aarch64-code.fd`, ramfb + usb-bot CD + NVMe): 25H2 hung at first
boot every time; 24H2 reached the desktop. If a future 25H2/26xx ISO is
required, this first-boot hang must be re-investigated (suspect the edk2/UEFI
handoff or a 25H2 boot-driver expectation) — it is NOT the "press any key to
boot from CD" prompt, which `lib.sh:win_press_any_key` handles.
