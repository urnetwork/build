# Provision the Windows 11 ARM64 build image with the URnetwork MSI toolchain.
# Run by setup.sh over ssh (also re-runnable via `setup.sh --reprovision`).
#
# Installs, via direct download: Visual Studio 2022 Build Tools (native ARM64 +
# x64 cross toolset, so one ARM VM cross-builds both MSIs), the Windows Driver Kit
# (for the WFP split-tunnel driver), WiX v5, git, and rsync. The build source is
# rsync'd in from the build server at build time (build.sh win_sync_source), not
# cloned here - no GitHub auth needed.
#
# SPDX-License-Identifier: MPL-2.0
$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

function Log($m) { Write-Host "[provision] $m" }

# --- Visual Studio 2022 Build Tools -----------------------------------------
# Native ARM64 toolset + the x64 cross tools + ATL/MFC (WiX/driver need them).
Log "installing VS 2022 Build Tools (ARM64 + x64 cross)"
$vsBootstrap = "$env:TEMP\vs_buildtools.exe"
Invoke-WebRequest -Uri "https://aka.ms/vs/17/release/vs_buildtools.exe" -OutFile $vsBootstrap
$vsArgs = @(
  "--quiet", "--wait", "--norestart", "--nocache",
  "--add", "Microsoft.VisualStudio.Workload.VCTools",
  "--add", "Microsoft.VisualStudio.Component.VC.Tools.ARM64",
  "--add", "Microsoft.VisualStudio.Component.VC.Tools.x86.x64",
  "--add", "Microsoft.VisualStudio.Component.Windows11SDK.22621",
  "--add", "Microsoft.VisualStudio.Component.VC.ATL",
  "--add", "Microsoft.VisualStudio.Component.VC.ATL.ARM64"
)
$p = Start-Process -FilePath $vsBootstrap -ArgumentList $vsArgs -Wait -PassThru -NoNewWindow
if ($p.ExitCode -ne 0 -and $p.ExitCode -ne 3010) { throw "VS Build Tools install failed ($($p.ExitCode))" }

# --- Windows Driver Kit (WFP split-tunnel callout driver) --------------------
Log "installing Windows Driver Kit"
$wdk = "$env:TEMP\wdksetup.exe"
Invoke-WebRequest -Uri "https://go.microsoft.com/fwlink/?linkid=2196230" -OutFile $wdk  # WDK for Win11 22H2
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
  Invoke-WebRequest -Uri "https://dot.net/v1/dotnet-install.ps1" -OutFile $dotnet
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
  Invoke-WebRequest -Uri "https://github.com/git-for-windows/git/releases/download/v2.47.1.windows.1/Git-2.47.1-arm64.exe" -OutFile $git
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
    Invoke-Expression ((New-Object System.Net.WebClient).DownloadString('https://community.chocolatey.org/install.ps1'))
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

# OpenSSH default shell -> cmd (override the autounattend's PowerShell). cmd passes
# args verbatim, so incoming `rsync --server /cygdrive/c/...` reaches the cygwin
# rsync unmangled (a msys/cygwin shell would rewrite the path). Our own ssh calls
# invoke `powershell -File ...` explicitly, so they're unaffected.
New-Item -Path "HKLM:\SOFTWARE\OpenSSH" -Force | Out-Null
New-ItemProperty -Path "HKLM:\SOFTWARE\OpenSSH" -Name DefaultShell `
  -Value "C:\Windows\System32\cmd.exe" -PropertyType String -Force | Out-Null

New-Item -ItemType Directory -Force "C:\build\urnetwork" | Out-Null

Log "provisioning complete"
