# Desktop build pipeline (Windows Store + Snap Store)

How `all/run.sh` on the macOS build server produces the Windows MSI and the Linux
snap, and the answers to the "what runs where" questions.

## What builds natively on macOS, and what doesn't

| Artifact | Native on macOS? | Why |
|---|---|---|
| SDK Windows DLL (`URnetworkSdk.dll`, amd64+arm64) | **Yes** | `sdk/cgo` cross-compiles via mingw-w64 (amd64) + llvm-mingw (arm64); Go + cgo, no Windows needed |
| SDK Linux `.so` (amd64+arm64) | **Yes** | `sdk/cgo` cross-compiles via `zig cc` (pins the 22.04 glibc floor) |
| Linux headless core (`cmd/urnetworkd`) | **Yes** | pure Go, `CGO_ENABLED=0`, cross-compiles |
| **Windows app MSI** (WinUI 3 C++, WDK driver, WiX) | **No** | MSVC, WinUI 3, the WDK, and WiX are Windows-only |
| **Linux snap** (GTK4 GUI + snapcraft) | **No (natively)** | the GTK GUI needs cgo+GTK4 for the linux target; snapcraft needs Linux |

So the SDK libraries are cross-built on the mac and shipped **into** the app
builds as prebuilt per-arch binaries. The two final *bundles* each need their
own OS: a Windows host for the MSI, a Linux environment (or Launchpad) for the
snap.

## Windows: an ARM Windows 11 VM on the Apple Silicon mac

**Yes — one ARM64 Windows 11 VM builds BOTH amd64 and arm64 Windows apps.** Native
ARM64 Visual Studio 2022 ships the cross toolsets:

- `msbuild /p:Platform=ARM64` → native arm64 build.
- `msbuild /p:Platform=x64` → uses the `arm64_x64` cross compiler (host arm64,
  target x64). No x64 machine needed.
- The WDK/EWDK on ARM64 targets both arches; WiX (a .NET tool) runs on ARM
  Windows; and Windows 11 ARM has x64 emulation as a fallback for any tool
  without a native arm64 build.

The VM does **not** build the Go SDK — it consumes the per-arch `URnetworkSdk.dll`
that the mac cross-built. So the VM only compiles the C++/driver/MSI for each
arch, which native ARM64 VS does directly.

Recommended VM: Parallels or VMware Fusion (good Windows-on-ARM support + host
folder sharing) or UTM. Provision once with: VS 2022 (v143, "Desktop C++"),
Windows 11 SDK, the WDK, WiX v5, vcpkg, and the code-signing cert/token
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
- Alternative to a local VM: a remote Windows runner (GitHub Actions
  `windows-latest`, Azure). Same `build.ps1`; the tradeoff is signing secrets
  live in CI instead of a local VM.

## Linux: `snapcraft remote-build` (no local Linux) or multipass

Two options for the mac build server:

1. **`snapcraft remote-build`** (recommended for a mac host) — submits the source
   to Canonical's Launchpad build farm, which builds amd64 **and** arm64 snaps and
   returns them. No local Linux VM. Needs a Launchpad account and the project on a
   git branch Launchpad can fetch; builds are public (fine for an app we ship).
2. **multipass Ubuntu VM** on the mac + `snapcraft` inside it (`snapcraft` drives
   an LXD/multipass build). Self-contained, private, but arm64+amd64 means either
   an arm64 multipass VM (native arm64 snap; amd64 via emulation, slow) or two VMs.

`remote-build` is the cleaner fit. Because snapcraft/remote-build only sees the
`linux/app` tree (not the sibling `sdk`/`connect`/`glog` the `replace` directives
point to), `run.sh` runs `go mod vendor` first — the replaces resolve against the
alongside checkouts, vendoring the deps into `linux/app/vendor/` so the snap
build is fully self-contained (no network module fetches on Launchpad).

## run.sh flow (added after the macOS app build)

Each platform's build lives in its own script — `all/build-windows.sh` and
`all/build-linux.sh` — which run.sh calls non-blocking (a flaky desktop build
warns and skips that platform's artifacts instead of sinking the release):

```sh
# all/build-windows.sh: cgo SDK zip (native macOS cross-build: mingw/llvm-mingw)
#                       + MSIs (x64+arm64) in the local QEMU/HVF ARM Windows VM
if OUT_DIR="$DESKTOP_OUT/windows" "$BUILD_HOME/all/build-windows.sh"; then
    github_release_upload "URnetworkSdkWindows-${V}.zip" ...
    github_release_upload URnetwork-*.msi ...   # (then submit to the Store manually)
fi

# all/build-linux.sh: cgo SDK zip (native macOS cross-build: zig)
#                     + snaps (amd64+arm64) in the snapcraft rock container
if OUT_DIR="$DESKTOP_OUT/linux" "$BUILD_HOME/all/build-linux.sh"; then
    github_release_upload "URnetworkSdkLinux-${V}.zip" ...
    github_release_upload urnetwork_*.snap ...  # (then submit to the Snap Store manually)
fi
```

Both scripts use the local branches AS-IS (run.sh configures the `v<version>`
branches earlier in the run) and also run standalone — see
`all/{windows,linux}/README.md`.

## Store submission: manual for now

The pipeline **builds the bundles and attaches them to the GitHub release**; a
human then submits them:

- **Windows Store:** upload the MSI(s) to the Partner Center EXE/MSI listing.
- **Snap Store:** `snapcraft upload --release=stable <snap>` (or the Snap Store
  web dashboard).

Automated submission (the `msstore` CLI on the VM; `snapcraft upload` in the
pipeline) is a later step — wire it in once the listings + credentials are set
up. It was intentionally left out so a release never blocks on store APIs.

## Env vars

The desktop builds always run (the pipeline does not gate which items build).
They require:

- `WINDOWS_BUILD_HOST` — `user@host` of the ARM64 Windows build VM (ssh).
- `WINDOWS_BUILD_DIR` — (optional) path on the VM where the repos are checked
  out; default `C:/build/urnetwork`.

The snap builds via `snapcraft remote-build` (needs a configured Launchpad
account on the build host).
