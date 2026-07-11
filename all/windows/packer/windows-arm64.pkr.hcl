# Packer template — build a reusable Windows 11 ARM64 build image (qcow2) for the
# URnetwork MSI, on an Apple-Silicon Mac via QEMU + Hypervisor.framework (HVF).
#
# This runs ONCE (or when the toolchain changes); build.sh then boots a
# copy-on-write overlay of the output image for each release. See README.md.
#
# FIRST-RUN TUNING (unavoidable — aarch64 Windows-on-QEMU is finicky and cannot be
# verified on the build host): the UEFI firmware path, the virtio driver ISO, and
# the exact device model lines below are the knobs most likely to need adjusting
# for your QEMU version. They are all surfaced as variables / clearly-marked
# qemuargs so you don't have to hunt.
#
# SPDX-License-Identifier: MPL-2.0

packer {
  required_plugins {
    qemu = {
      source  = "github.com/hashicorp/qemu"
      version = "~> 1.1"
    }
  }
}

# --- operator-supplied inputs ------------------------------------------------

variable "iso_path" {
  type        = string
  description = "Path to a Windows 11 ARM64 ISO (Microsoft now publishes an official ARM64 ISO)."
}
variable "iso_checksum" {
  type        = string
  default     = "none" # set to sha256:... to pin; 'none' skips verification
}
variable "virtio_iso" {
  type        = string
  description = "Path to virtio-win.iso (provides the ARM64 virtio-net driver Windows lacks inbox)."
}
variable "uefi_code" {
  type        = string
  default     = "/opt/homebrew/share/qemu/edk2-aarch64-code.fd"
  description = "aarch64 UEFI firmware (ships with the Homebrew qemu formula)."
}
variable "uefi_vars_template" {
  type        = string
  default     = "/opt/homebrew/share/qemu/edk2-arm-vars.fd"
  description = "aarch64 UEFI variable-store template (copied to a writable per-build file)."
}
variable "ssh_public_key" {
  type        = string
  description = "The STABLE public key baked into the image's authorized_keys (build.sh uses its private half)."
}
variable "output_dir" {
  type    = string
  default = "output"
}
variable "cpus" {
  type    = number
  default = 6
}
variable "memory" {
  type    = number
  default = 8192
}

# --- build image -------------------------------------------------------------

source "qemu" "win-arm64" {
  vm_name        = "windows-arm64.qcow2"
  output_directory = var.output_dir
  format         = "qcow2"
  disk_size      = "90G"
  headless       = true

  # Windows install ISOs show "Press any key to boot from CD or DVD" under UEFI.
  # With no keypress the VM drops to the UEFI shell and never enters Setup (the
  # classic "Waiting for SSH..." hang). Spam Enter across the prompt window. If
  # your firmware POSTs slower/faster, watch the VNC console Packer prints and
  # tune boot_wait — the Enters must land while the prompt is on screen.
  boot_wait      = "5s"
  boot_command   = ["<enter><wait2><enter><wait2><enter><wait2><enter><wait2><enter><wait2><enter><wait2><enter><wait2><enter><wait2><enter><wait2><enter>"]

  qemu_binary    = "qemu-system-aarch64"
  machine_type   = "virt"
  accelerator    = "hvf"
  cpus           = var.cpus
  memory         = var.memory

  # Let Packer manage the system disk + UEFI natively (avoids the QEMU
  # double-attach / duplicate -machine / uncreated-pflash pitfalls of doing it by
  # hand). virtio-blk = the viostor driver, which the unattend injects; net_device
  # virtio-net = NetKVM, also injected. UEFI: read-only code + Packer copies the
  # vars TEMPLATE to a writable per-build store.
  disk_interface    = "virtio"
  disk_cache        = "writeback"
  net_device        = "virtio-net-pci"
  efi_boot          = true
  efi_firmware_code = var.uefi_code
  efi_firmware_vars = var.uefi_vars_template

  iso_url        = var.iso_path
  iso_checksum   = var.iso_checksum

  # Unattended install: Windows scans removable media for autounattend.xml. We put
  # a templated one (with the baked ssh key) on a CD. The toolchain is installed
  # later by the powershell provisioner (uploaded over ssh), not from the CD.
  # NOTE: templatefile() resolves relative to path.root (the dir of this .pkr.hcl),
  # so the path is bare — prefixing ${path.root} would double it (packer/packer/...).
  cd_content = {
    "autounattend.xml" = templatefile("http/Autounattend.pkrtpl.xml", {
      ssh_public_key = var.ssh_public_key
    })
  }
  cd_label = "PACKER"

  # OpenSSH (enabled by the autounattend) is the communicator — same transport the
  # release build (build.ps1 over ssh) uses, so we exercise the real path.
  communicator         = "ssh"
  ssh_username         = "builder"
  ssh_private_key_file = "${path.root}/../.ssh/id_ed25519"
  ssh_timeout          = "6h" # Windows install + VS Build Tools + WDK is very slow, esp. first boot
  shutdown_command     = "shutdown /s /t 5 /f /d p:4:1"

  # Only the bits Packer's native options can't express (the aarch64 CPU model and
  # the second CD-ROM carrying the virtio-win drivers the unattend injects). This
  # is the most likely first-run tuning surface — see README.md.
  qemuargs = [
    ["-cpu", "host"],
    # aarch64 `virt` has NO default display or input device (unlike x86 pc). Without
    # a GPU the VNC console is blank; without a USB keyboard the boot_command
    # keystrokes go nowhere, so the "press any key to boot from CD" prompt is never
    # answered and the install never starts. These four are load-bearing.
    ["-device", "virtio-gpu-pci"],
    ["-device", "qemu-xhci"],
    ["-device", "usb-kbd"],
    ["-device", "usb-tablet"],
    # virtio-win drivers (viostor/NetKVM) the unattend injects during Setup.
    ["-drive", "file=${var.virtio_iso},media=cdrom,readonly=on"],
  ]
}

build {
  sources = ["source.qemu.win-arm64"]

  # Provision the toolchain over ssh (PowerShell). Kept in a separate script so it
  # can be iterated without touching the template.
  provisioner "powershell" {
    script            = "${path.root}/scripts/provision.ps1"
    execution_policy  = "bypass"
    elevated_user     = "builder"
    elevated_password = "urnetwork-build"
  }

  # Generalize is intentionally skipped — this is a private build image, not a
  # distributable one; keeping the account avoids re-running OOBE on every boot.
}
