# Desktop build pipeline (Windows MSI + Linux deb/rpm/Arch/AppImage/Flatpak)

How `all/run.sh` on the macOS build server produces the Windows MSI and the
Linux artifacts (daemon `.deb` + `.rpm` + Arch `.pkg.tar.zst` + `install.sh`
tarball, GUI AppImage, and a Flatpak for the build machine's architecture —
names normative in `linux/MIGRATION.md`), and the answers to the "what runs
where" questions.

## What builds natively on macOS, and what doesn't

| Artifact | Native on macOS? | Why |
|---|---|---|
| SDK Windows DLL (`URnetworkSdk.dll`, amd64+arm64) | **No** | built in the Windows VM — `sdk/cgo` via Go + llvm-mingw (both arches); see below |
| SDK Linux `.so` (amd64+arm64) | **Yes** | `sdk/cgo` cross-compiles via `zig cc` (pins the 22.04 glibc floor) |
| Linux headless core (`cmd/urnetworkd`) | **Yes** | pure Go, `CGO_ENABLED=0`, cross-compiles |
| **Windows app MSI** (WinUI 3 C++, WDK driver, WiX) | **No** | MSVC, WinUI 3, the WDK, and WiX are Windows-only |
| **Linux artifacts** (GTK4 GUI AppImage + daemon deb/tarball/rpm/Arch package) | **No (natively)** | the GTK GUI needs cgo+GTK4 for the linux target; package and AppImage assembly needs a Linux userland |

So the **Linux** SDK `.so` is cross-built on the mac (zig) and shipped **into**
the Linux container build. The **Windows** SDK DLL builds inside the Windows VM
(Go + llvm-mingw), alongside the app — the mac needs no Windows toolchain.
Either way the final *bundles* each need their own OS: a Windows host for the
MSI, Linux containers for the daemon packages and the AppImage.

## Windows: an ARM Windows 11 VM on the Apple Silicon mac

**Yes — one ARM64 Windows 11 VM builds BOTH amd64 and arm64 Windows apps.** Native
ARM64 Visual Studio 2022 ships the cross toolsets:

- `msbuild /p:Platform=ARM64` → native arm64 build.
- `msbuild /p:Platform=x64` → uses the `arm64_x64` cross compiler (host arm64,
  target x64). No x64 machine needed.
- The WDK/EWDK on ARM64 targets both arches; WiX (a .NET tool) runs on ARM
  Windows; and Windows 11 ARM has x64 emulation as a fallback for any tool
  without a native arm64 build.

The VM builds the Go SDK too: `windows/build-sdk.ps1` (Go + llvm-mingw) produces
the per-arch `URnetworkSdk.dll`, then native ARM64 VS compiles the C++/driver/MSI
for each arch against it. The SDK zip is also pulled back to the mac so run.sh
uploads it as a release artifact.

Recommended VM: Parallels or VMware Fusion (good Windows-on-ARM support + host
folder sharing) or UTM. Provision once with: VS 2022 (v143, "Desktop C++"),
Windows 11 SDK, the WDK, CMake, WiX v5, and the code-signing cert/token
(signing stays on the VM, never leaves).

### Connection: macOS build script ↔ Windows VM

Use **OpenSSH Server** (built into Windows 11) — the mac already scripts over ssh.

```
macOS run.sh                          Windows VM (arm64, ssh server)
────────────                          ─────────────────────────────
(earlier) push v<version> branch ───▶ origin  (git remote)
          on each repo
1. cross-build SDK zip (native)
2. ssh: git fetch + checkout     ───▶ $WIN_DIR/windows  (repos already cloned)
   v<version>, reset --hard,           reset --hard origin/v<version>; clean -fdx
   clean  (exact released tree)
3. scp the SDK Windows zip       ───▶ $WIN_DIR/URnetworkSdkWindows.zip
4. ssh vm  powershell build.ps1  ───▶ fetch-deps → msbuild x64+ARM64
                                       → sign PEs → WiX MSI (x64+ARM64) → sign MSI
5. scp the .msi back             ◀───  $WIN_DIR/windows/app/build/out/*.msi
6. github release (submit manually)
```

- **Source:** the VM already has the repos cloned under `$WIN_DIR`. The build
  does NOT copy source — it `git fetch`es and checks out the **version branch**
  (`v<version>`) that `run.sh` pushed to origin earlier in the run, then
  `git reset --hard origin/v<version>` + `git clean -fdx` so the tree is exactly
  the released source with no stale artifacts. (The SDK DLL is a mac build
  artifact, not a repo, so its zip is `scp`'d.)
- **Auth:** ssh key from the mac to the VM's `authorized_keys` (set
  `WINDOWS_BUILD_HOST=user@vm-ip`); the VM's git needs read access to the repo
  remotes (deploy key / credential helper).
- **Signing on the VM:** Authenticode (app) + attestation submission (driver)
  run on the VM where the EV cert/token lives (see `windows/app/SIGNING.md`).

## Linux: Ubuntu Docker containers on the mac

The Linux artifacts build in Ubuntu containers per target arch (the daemon half
on `ubuntu:22.04`, the GUI half on `ubuntu:24.04`; arm64 native on Apple
Silicon, amd64 under Docker's qemu emulation) — no Launchpad, no VM. Per arch,
`all/linux/build-arch.sh` runs the meson build, `meson install --destdir`s a
staging tree, and then invokes the packaging scripts the **linux repo** ships
(`linux/packaging/*`, `linux/app/scripts/*`) to produce the daemon `.deb`, the
`.rpm`, the Arch `.pkg.tar.zst`, the `install.sh` tarball, and the GUI
AppImage (no `.zsync`). The artifact filenames are normative
(`linux/MIGRATION.md`); the pipeline fails loudly on a missing packaging script
or a wrongly-named output rather than uploading nothing silently. Details:
`all/linux/README.md`.

The daemon's four packages (`.deb`, tarball, `.rpm`, Arch `.pkg.tar.zst`) are
all built in the **same** `ROLE=daemon` container from the **same**
`meson install --destdir` tree, so they cannot ship different daemons.
`make-rpm.sh` and `make-arch.sh` are nfpm-based — no `rpmbuild`, no `mock`, no
`makepkg` — which is why neither package needs an image of its own. The `.rpm`
needs only `rpm` (for the payload assertion) plus `checkpolicy` +
`semodule-utils` (for the SELinux policy module the Fedora path requires), and
the Arch package needs only `zstd` (for its payload assertion), all added to
`Dockerfile.daemon`.

**RPM and Arch packages are required by `run.sh`.** It unconditionally passes
`UR_REQUIRE_RPM=true` and `UR_REQUIRE_ARCH_PKG=true` to the existing Linux builder.
The standalone script's defaults remain unchanged, but a release must not continue
with a missing or failed package. Run-level output checks also require nonempty
artifacts for every selected role/architecture and the existing single Flatpak.
Either way a failed `.rpm` or Arch package never reaches the output directory:
`build-arch.sh` moves a package there only after its script built and checked
it.

## run.sh flow (added after the macOS app build)

Each platform's build lives in its own script — `all/build-windows.sh` and
`all/build-linux.sh` — which run.sh calls as required release gates. Failure stops
the release, and output completeness is checked before platform publication:

```sh
# all/build-windows.sh: cgo SDK DLLs (Go + llvm-mingw) + MSIs (x64+arm64), all
#                       built in the local QEMU/HVF ARM Windows VM
OUT_DIR="$DESKTOP_OUT/windows" "$BUILD_HOME/all/build-windows.sh"
error_trap 'windows build'
require_windows_artifacts "$DESKTOP_OUT/windows" "$EXTERNAL_WARP_VERSION"
error_trap 'required windows artifacts'
# Upload the nonempty SDK zip and requested MSIs; Store submission remains manual.

# all/build-linux.sh: cgo SDK zip (native macOS cross-build: zig)
#                     + deb/install-tarball/rpm/arch (Ubuntu 22.04 container)
#                     + AppImage (Ubuntu 24.04 container), both amd64+arm64
UR_REQUIRE_RPM=true UR_REQUIRE_ARCH_PKG=true \
  OUT_DIR="$DESKTOP_OUT/linux" "$BUILD_HOME/all/build-linux.sh"
error_trap 'linux build'
# Build the existing single-architecture Flatpak, checking its exit status.
require_linux_artifacts "$DESKTOP_OUT/linux" "$EXTERNAL_WARP_VERSION"
error_trap 'required linux artifacts'
# Upload the nonempty SDK zip and complete selected artifact matrix.
```

Both scripts use the local branches AS-IS (run.sh configures the `v<version>`
branches earlier in the run) and also run standalone — see
`all/{windows,linux}/README.md`.

### Building local (uncommitted) changes

By default the scripts build whatever is already staged under `BUILD_HOME`
(`build/{sdk,connect,glog,linux,windows}` — the release copies run.sh set up).
To instead compile a **local working tree** (e.g. to verify uncommitted SDK/app
changes before committing), point the scripts at the local repos and they
rsync them into `BUILD_HOME` first (source tree only — `.git` and build
artifacts are skipped). Set either:

- `SRC_HOME=<monorepo root>` — stages every needed repo from `$SRC_HOME/<repo>`, or
- `SRC_<REPO>=<path>` per repo (e.g. `SRC_SDK=/path/to/sdk`), which overrides `SRC_HOME`.

Each script stages the repos its cgo build's `go.mod` replaces — **`sdk`,
`connect`, and `glog`** (all three, or the module graph mismatches) — plus its
own app repo (`linux` / `windows`). A locally-staged repo usually isn't on a
`v<version>` branch, so pass `EXTERNAL_WARP_VERSION` explicitly. Example:

```bash
SRC_HOME=/Users/you/urnetwork EXTERNAL_WARP_VERSION=0.0.0-0 \
  ARCHES=arm64 OUT_DIR=/tmp/linux-out ./all/build-linux.sh
```

Note this **overwrites** the release copies under `BUILD_HOME` with the local
source; re-run run.sh's version staging before a real release. Implemented in
`all/stage-local-repos.sh`.

## Store submission / publishing: manual for now

The pipeline **builds the bundles and attaches them to the GitHub release**; a
human then publishes them:

- **Windows Store:** upload the MSI(s) to the Partner Center EXE/MSI listing.
- **Linux:** no store. The `.deb`/`.rpm`/Arch package/tarball/AppImage/Flatpak
  ship from the release page; publishing the `.deb` to the signed apt repo is a
  manual follow-up, as is copying a nightly's Linux assets (same names) to the
  stable `urnetwork/linux` release the in-app updater reads. There is no zsync
  channel (GitHub Releases can't serve the multi-range requests zsync needs —
  `linux/APPIMAGE.md` §11f). A dnf repo is a further follow-up, and needs more
  than hosting: `gpgcheck=1` verifies the signature embedded in the **rpm
  header**, which the detached `.asc` beside the artifact does not provide
  (`make-rpm.sh` emits one when `UR_RPM_SIGN_KEY_FILE` is set).

Automated submission (the `msstore` CLI on the VM; apt-repo/update-endpoint
publishing in the pipeline) is a later step — wire it in once the listings +
credentials are set up. It was intentionally left out so a release never blocks
on store APIs.

## Env vars

The desktop builds always run (the pipeline does not gate which items build).
They require:

- `WINDOWS_BUILD_HOST` — `user@host` of the ARM64 Windows build VM (ssh).
- `WINDOWS_BUILD_DIR` — (optional) path on the VM where the repos are checked
  out; default `C:/build/urnetwork`.

The Linux build needs Docker (Docker Desktop, with buildx + qemu) running on
the build host — no other credentials.
