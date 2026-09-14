# Provision the Windows 11 ARM64 build image with the URnetwork MSI toolchain.
# Run by setup.sh over ssh (also re-runnable via `setup.sh --reprovision`).
#
# Installs, via direct download: Visual Studio 2022 Build Tools (native ARM64 +
# x64 cross toolset, so one ARM VM cross-builds both MSIs), the Windows Driver Kit
# (for the WFP split-tunnel driver), CMake (for the pinned zxing-cpp source build),
# WiX v5, git, rsync, and the cgo SDK toolchain (Go + llvm-mingw, so the URnetwork
# SDK DLLs build natively here instead of being cross-built on the mac). The build
# source is rsync'd in from the build server at build time (build.sh
# win_sync_source), not cloned here - no GitHub auth needed.
#
# SPDX-License-Identifier: MPL-2.0
$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

function Log($m) { Write-Host "[provision] $m" }

# QEMU user networking occasionally resets a long HTTPS transfer. Windows 11's
# inbox curl can resume the partial file across retries; publish the destination
# only after curl reports a complete transfer so installers never consume a
# truncated artifact.
$curlExe = "$env:SystemRoot\System32\curl.exe"
if (-not (Test-Path $curlExe)) { throw "missing inbox curl: $curlExe" }
function Get-RemoteFile($Uri, $OutFile) {
  $partial = "$OutFile.partial"
  $curlArguments = @(
    "--fail", "--location", "--silent", "--show-error",
    "--retry", "12", "--retry-all-errors", "--retry-delay", "2",
    "--connect-timeout", "30", "--speed-time", "60", "--speed-limit", "1024",
    "--continue-at", "-", "--output", $partial, $Uri
  )
  & $curlExe @curlArguments
  if ($LASTEXITCODE -ne 0) { throw "download failed after retries ($LASTEXITCODE): $Uri" }
  if (-not (Test-Path $partial) -or (Get-Item $partial).Length -eq 0) {
    throw "download produced no data: $Uri"
  }
  Move-Item -Force $partial $OutFile
}

# --- build-VM hygiene (FIRST, before any install) ---------------------------
# This is a hermetic, throwaway-overlay build VM: each release boots a fresh CoW
# copy, builds for ~an hour over one ssh session, and is discarded. Windows
# Update gains nothing here, and actively breaks builds: observed (System log
# 1074/7034) TrustedInstaller rebooting the guest mid `go build`, killing sshd
# and with it the release's windows artifacts. Worse, an update staged but not
# finished before the image is finalized re-applies on EVERY later overlay boot,
# so one poisoned image breaks every subsequent build.
#
# This block runs FIRST, deliberately: the installs below take ~an hour, and
# with WU live that is an hour in which a new update can be staged behind our
# backs — leaving the "hermetic" image poisoned exactly as before. Disable the
# servicing stack up front, then install into a quiet machine. OS updates are
# taken deliberately, by rebuilding the image (setup.sh) or re-provisioning it
# (setup.sh --reprovision).
Log "enforcing the shared hermetic build-guest policy"
$guestPolicy = Join-Path $PSScriptRoot "disable-auto-servicing.ps1"
if (-not (Test-Path $guestPolicy)) {
  # The vestigial Packer template uploads its PowerShell provisioner to a
  # generated path, while setup.sh places both scripts side by side.
  $guestPolicy = "C:\Windows\Temp\disable-auto-servicing.ps1"
}
if (-not (Test-Path $guestPolicy)) { throw "missing shared guest policy: $guestPolicy" }
& $guestPolicy

# Defender real-time scanning churns CPU over the rsync'd build tree, the go
# module cache, and the toolchains (it can't be fully disabled headless -
# tamper protection - but path exclusions work and are enough). Set before the
# installs below so the toolchains land in already-excluded paths.
Log "adding Defender exclusions for the build + toolchain paths"
foreach ($p in @('C:\build', 'C:\go', 'C:\llvm-mingw', "$env:USERPROFILE\go")) {
  Add-MpPreference -ExclusionPath $p -ErrorAction SilentlyContinue
}

# Never sleep mid-build.
powercfg /change standby-timeout-ac 0 | Out-Null
powercfg /change hibernate-timeout-ac 0 | Out-Null

# --- Visual Studio 2022 Build Tools -----------------------------------------
# Native ARM64 toolset + the x64 cross tools + ATL/MFC (WiX/driver need them).
Log "configuring VS 2022 Build Tools (ARM64 + x64 cross)"
$vsBootstrap = "$env:TEMP\vs_buildtools.exe"
Get-RemoteFile "https://aka.ms/vs/17/release/vs_buildtools.exe" $vsBootstrap
$vsCommonArgs = @(
  "--quiet", "--wait", "--norestart", "--nocache",
  "--add", "Microsoft.VisualStudio.Workload.VCTools",
  "--add", "Microsoft.VisualStudio.Component.VC.Tools.ARM64",
  "--add", "Microsoft.VisualStudio.Component.VC.Tools.x86.x64",
  # Required by windows/app/tools/fetch-deps.ps1 to build the pinned zxing-cpp
  # source release for every selected target architecture. This is only a
  # recommended component of the VCTools workload, so it must be explicit.
  "--add", "Microsoft.VisualStudio.Component.VC.CMake.Project",
  "--add", "Microsoft.VisualStudio.Component.Windows11SDK.22621",
  "--add", "Microsoft.VisualStudio.Component.VC.ATL",
  "--add", "Microsoft.VisualStudio.Component.VC.ATL.ARM64",
  # ".NET WinUI app development build tools": brings the AppxPackage MSBuild tasks
  # (Microsoft.Build.AppxPackage.dll / Microsoft.Build.Packaging.Pri.Tasks.dll, staged
  # under MSBuild\Microsoft\VisualStudio\v17.0\AppxPackage\) that the default MrtCore PRI
  # path (MrtCore.PriGen.targets -> ExpandPriContent / GenerateProjectPriFile) needs to
  # generate resources.pri. IMPORTANT: it must be the NON-VC group. The ".VC" variant
  # (Microsoft.VisualStudio.ComponentGroup.UWP.VC.BuildTools, "C++ v143 UWP tools") is the
  # C++ UWP *compiler* support and does NOT ship these managed (AnyCPU) tasks - only this
  # group does (per the VS Build Tools component-ID docs). Without it the App must fall
  # back to the self-contained EnableMsixTooling PRI path; with it, the standard MrtCore path.
  "--add", "Microsoft.VisualStudio.ComponentGroup.UWP.BuildTools",
  # The VCTools workload pulls in the "Vcpkg" component by default. We vendor
  # nlohmann/json and use no vcpkg packages, so keep the unused component out of
  # the image (smaller image; no dormant MSBuild auto-integration to reason about).
  "--remove", "Microsoft.VisualStudio.Component.Vcpkg"
)
$vswhere = Join-Path ${env:ProgramFiles(x86)} "Microsoft Visual Studio\Installer\vswhere.exe"
$vsInstallPath = ""
if (Test-Path $vswhere) {
  $vsInstallPath = [string]((& $vswhere -latest -products Microsoft.VisualStudio.Product.BuildTools -property installationPath | Select-Object -First 1))
}
if ([string]::IsNullOrWhiteSpace($vsInstallPath)) {
  Log "installing VS 2022 Build Tools"
  $vsArgs = $vsCommonArgs
} else {
  # The bootstrapper defaults to the install operation. On a provisioned image
  # that operation exits 1 with "already installed" before it applies any
  # component changes, so reprovisioning must explicitly modify that instance.
  Log "modifying existing VS 2022 Build Tools at $vsInstallPath"
  $quotedVsInstallPath = '"' + $vsInstallPath + '"'
  $vsArgs = @("modify", "--installPath", $quotedVsInstallPath) + $vsCommonArgs
}
$p = Start-Process -FilePath $vsBootstrap -ArgumentList $vsArgs -Wait -PassThru -NoNewWindow
if ($p.ExitCode -ne 0 -and $p.ExitCode -ne 3010) { throw "VS Build Tools install failed ($($p.ExitCode))" }

# The headless release enters the VS developer shell before fetching app
# dependencies, but the reusable VM contract must not depend on that shell
# happening to expose optional tools. Locate the component-owned executable,
# exercise it now, and persist its directory for future OpenSSH sessions.
$cmakeExe = [string]((& $vswhere -latest `
  -products Microsoft.VisualStudio.Product.BuildTools `
  -requires Microsoft.VisualStudio.Component.VC.CMake.Project `
  -find "Common7\IDE\CommonExtensions\Microsoft\CMake\CMake\bin\cmake.exe" |
  Select-Object -First 1))
if ([string]::IsNullOrWhiteSpace($cmakeExe) -or -not (Test-Path $cmakeExe)) {
  throw "VS CMake component was requested but cmake.exe was not installed"
}
$cmakeVersion = ((& $cmakeExe --version) 2>&1 | Select-Object -First 1)
if ($LASTEXITCODE -ne 0 -or $cmakeVersion -notmatch '^cmake version ') {
  throw "VS CMake install is not usable: '$cmakeVersion'"
}
$cmakeBin = Split-Path -Parent $cmakeExe
$machPath = [Environment]::GetEnvironmentVariable('Path', 'Machine')
if ($machPath -notlike "*$cmakeBin*") { $machPath = "$cmakeBin;$machPath" }
[Environment]::SetEnvironmentVariable('Path', $machPath, 'Machine')
$env:PATH = "$cmakeBin;$env:PATH"
Log "CMake ready: $cmakeVersion ($cmakeExe)"

# --- Windows Driver Kit (WFP split-tunnel callout driver) --------------------
Log "installing Windows Driver Kit"
$wdk = "$env:TEMP\wdksetup.exe"
Get-RemoteFile "https://go.microsoft.com/fwlink/?linkid=2196230" $wdk  # WDK for Win11 22H2
Start-Process -FilePath $wdk -ArgumentList @("/quiet", "/norestart") -Wait -NoNewWindow

# --- WiX v5 (MSI) ------------------------------------------------------------
Log "installing WiX v5 (dotnet global tool)"
$dotnetDir = "$env:USERPROFILE\.dotnet"
$toolsDir  = "$env:USERPROFILE\.dotnet\tools"
# WiX 5 ships as a dotnet global tool. Install the .NET SDK to a KNOWN dir
# ($dotnetDir) - dotnet-install.ps1's default is %LocalAppData%\Microsoft\dotnet,
# and the wix.exe apphost then can't resolve the runtime (hostfxr.dll not found).
# Pinning -InstallDir + exporting DOTNET_ROOT below is what lets wix.exe run.
if (-not (Test-Path "$dotnetDir\dotnet.exe")) {
  $dotnet = "$env:TEMP\dotnet-install.ps1"
  Get-RemoteFile "https://dot.net/v1/dotnet-install.ps1" $dotnet
  & $dotnet -Channel 8.0 -Architecture arm64 -InstallDir "$dotnetDir"
}
# dotnet apphosts (wix.exe) resolve the runtime via DOTNET_ROOT; PATH needs
# dotnet + the global-tools dir. Set for THIS session AND persist to the user
# environment so build.ps1 finds them in future ssh sessions.
$env:DOTNET_ROOT = $dotnetDir
$env:PATH = "$dotnetDir;$toolsDir;$env:PATH"
[Environment]::SetEnvironmentVariable('DOTNET_ROOT', $dotnetDir, 'User')
$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
foreach ($p in @($dotnetDir, $toolsDir)) {
  if ($userPath -notlike "*$p*") { $userPath = "$p;$userPath" }
}
[Environment]::SetEnvironmentVariable('Path', $userPath, 'User')

# idempotent: `dotnet tool install` errors if wix is already present.
if (-not (Test-Path "$toolsDir\wix.exe")) {
  & dotnet tool install --global wix --version 5.*
}
# Pin the UI extension to WiX v5 - unversioned pulls the v6 extension (WIX6101
# incompatibility). Invoke wix by full path (not on PATH in this process yet).
& "$toolsDir\wix.exe" extension add --global WixToolset.UI.wixext/5.0.2

# --- git + rsync (pinned cwRsync) + cmd ssh shell ---------------------------
# The build source is rsync'd in from the build server at build time (build.sh
# win_sync_source), NOT cloned here - no GitHub, no ssh key. Install git (build.ps1
# may read the synced .git), a PINNED rsync, and set OpenSSH's default shell to cmd
# so incoming `rsync --server` reaches rsync with its paths unmangled.
if (-not (Get-Command git -ErrorAction SilentlyContinue) -and -not (Test-Path "C:\Program Files\Git\cmd\git.exe")) {
  Log "installing git"
  $git = "$env:TEMP\git-arm64.exe"
  # Git for Windows ARM64 installer (adjust the asset URL when bumping versions).
  Get-RemoteFile "https://github.com/git-for-windows/git/releases/download/v2.47.1.windows.1/Git-2.47.1-arm64.exe" $git
  Start-Process -FilePath $git -ArgumentList @("/VERYSILENT", "/NORESTART") -Wait -NoNewWindow
}
$env:PATH = "C:\Program Files\Git\cmd;$env:PATH"

# rsync: pin cwRsync (Chocolatey's `rsync` package) for a reproducible build
# environment - Chocolatey keeps every version permanently. cwRsync is cygwin-
# based, so the remote target is a /cygdrive/c/... path (see WIN_DIR_UNIX in
# lib.sh). Only 64-bit is published; it runs under Windows-on-ARM x64 emulation.
$rsyncVersion = "6.4.6"   # cwRsync Free Edition - pinned; bump deliberately
if (-not (Get-Command rsync -ErrorAction SilentlyContinue)) {
  if (-not (Get-Command choco -ErrorAction SilentlyContinue) -and -not (Test-Path "$env:ProgramData\chocolatey\bin\choco.exe")) {
    Log "installing Chocolatey"
    Set-ExecutionPolicy Bypass -Scope Process -Force
    [System.Net.ServicePointManager]::SecurityProtocol = [System.Net.ServicePointManager]::SecurityProtocol -bor 3072
    $chocolateyInstall = "$env:TEMP\install-chocolatey.ps1"
    Get-RemoteFile "https://community.chocolatey.org/install.ps1" $chocolateyInstall
    & $chocolateyInstall
  }
  Log "installing rsync (cwRsync $rsyncVersion via Chocolatey)"
  & "$env:ProgramData\chocolatey\bin\choco.exe" install rsync --version=$rsyncVersion -y --no-progress
  # Put the REAL cwRsync binary (with its cygwin DLLs) on the machine PATH ahead of
  # the choco shim, so `rsync --server` runs the binary directly (transparent stdio).
  $r = Get-ChildItem "$env:ProgramData\chocolatey\lib\rsync" -Recurse -Filter rsync.exe -ErrorAction SilentlyContinue | Select-Object -First 1
  if ($r) {
    $machPath = [Environment]::GetEnvironmentVariable('Path', 'Machine')
    if ($machPath -notlike "*$($r.DirectoryName)*") {
      [Environment]::SetEnvironmentVariable('Path', "$($r.DirectoryName);$machPath", 'Machine')
    }
  }
}

# --- Go + llvm-mingw (URnetwork cgo SDK build) ------------------------------
# The cgo SDK (sdk/cgo -> URnetworkSdk.dll) builds natively in this VM now (was
# cross-built on the mac). Pin Go to the sdk module's toolchain so
# GOTOOLCHAIN=auto doesn't pull a second one at build time. llvm-mingw supplies
# x86_64-/aarch64-w64-mingw32-clang for the c-shared DLLs (no Homebrew formula
# exists; this is the upstream prebuilt, Windows-ARM64 host build, targets both).
#
# KEEP IN SYNC with the `go` directive in sdk/cgo/go.mod. When it drifts the
# build still works but silently pays a toolchain download EVERY run, on the
# slowest builder we have — "go: downloading go1.26.5 (windows/arm64)" in the
# build log is the tell. (It drifted to 1.26.4 vs the module's 1.26.5 exactly
# this way.) Provisioning has no repo checkout to read the version from, so it
# is hardcoded here on purpose.
$goVersion = "1.26.5"
$goRoot = "C:\go"
# Version-aware, not merely existence-aware: a plain Test-Path would leave an
# already-provisioned image on the OLD Go forever, so bumping $goVersion above
# would have no effect on --reprovision — the one path used to fix a stale image.
$goInstalled = ""
if (Test-Path "$goRoot\bin\go.exe") {
  # "go version go1.26.5 windows/arm64" -> "1.26.5"
  $goInstalled = (& "$goRoot\bin\go.exe" version) -replace '^go version go([^\s]+).*$', '$1'
}
if ($goInstalled -ne $goVersion) {
  $goZip = "$env:TEMP\go-$goVersion.zip"
  Get-RemoteFile "https://go.dev/dl/go$goVersion.windows-arm64.zip" $goZip
  if ($goInstalled) {
    Log "replacing Go $goInstalled with $goVersion (windows/arm64)"
  } else {
    Log "installing Go $goVersion (windows/arm64)"
  }
  if (Test-Path $goRoot) {
    # A just-executed go.exe can retain a transient image/file-system lock even
    # after the native command returns. Reprovisioning observed the first delete
    # fail while the same file was removable seconds later, so join that short
    # Windows lifecycle boundary instead of abandoning an otherwise healthy base.
    for ($removeAttempt = 1; $removeAttempt -le 30; $removeAttempt++) {
      try {
        Remove-Item -Recurse -Force $goRoot
        break
      } catch {
        if ($removeAttempt -eq 30) { throw }
        Start-Sleep -Seconds 2
      }
    }
  }
  if (Test-Path $goRoot) { throw "old Go toolchain still exists after removal retries: $goRoot" }
  Expand-Archive -Path $goZip -DestinationPath "C:\" -Force   # -> C:\go
  $check = (& "$goRoot\bin\go.exe" version)
  if ($check -notmatch [regex]::Escape("go$goVersion")) {
    throw "Go $goVersion install failed: 'go version' reports '$check'"
  }
  Log "Go pinned: $check"
} else {
  Log "Go $goVersion already installed"
}

$llvmVersion = "20260616"
$llvmDir = "C:\llvm-mingw"
if (-not (Test-Path "$llvmDir\bin\clang.exe")) {
  Log "installing llvm-mingw $llvmVersion (ucrt, windows/arm64 host)"
  $llvmZip = "$env:TEMP\llvm-mingw-$llvmVersion.zip"
  Get-RemoteFile "https://github.com/mstorsjo/llvm-mingw/releases/download/$llvmVersion/llvm-mingw-$llvmVersion-ucrt-aarch64.zip" $llvmZip
  Expand-Archive -Path $llvmZip -DestinationPath "C:\" -Force  # -> C:\llvm-mingw-<ver>-ucrt-aarch64
  if (Test-Path $llvmDir) { Remove-Item -Recurse -Force $llvmDir }
  Rename-Item "C:\llvm-mingw-$llvmVersion-ucrt-aarch64" $llvmDir
}

# Persist on the MACHINE PATH so ssh build sessions (cmd default shell then
# `powershell -File build-sdk.ps1`) resolve go + the mingw clang wrappers.
$machPath = [Environment]::GetEnvironmentVariable('Path', 'Machine')
foreach ($p in @("$goRoot\bin", "$llvmDir\bin")) {
  if ($machPath -notlike "*$p*") { $machPath = "$p;$machPath" }
}
[Environment]::SetEnvironmentVariable('Path', $machPath, 'Machine')
$env:PATH = "$goRoot\bin;$llvmDir\bin;$env:PATH"

# OpenSSH default shell -> cmd (override the autounattend's PowerShell). cmd passes
# args verbatim, so incoming `rsync --server /cygdrive/c/...` reaches the cygwin
# rsync unmangled (a msys/cygwin shell would rewrite the path). Our own ssh calls
# invoke `powershell -File ...` explicitly, so they're unaffected.
New-Item -Path "HKLM:\SOFTWARE\OpenSSH" -Force | Out-Null
New-ItemProperty -Path "HKLM:\SOFTWARE\OpenSSH" -Name DefaultShell `
  -Value "C:\Windows\System32\cmd.exe" -PropertyType String -Force | Out-Null

New-Item -ItemType Directory -Force "C:\build\urnetwork" | Out-Null

Log "provisioning complete"
