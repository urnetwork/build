# Smoke test: verify the build image has everything windows/app/build.ps1 needs.
# Uploaded + run over ssh by setup.sh. Exits 0 if all critical checks pass, else 1.
# SPDX-License-Identifier: MPL-2.0
$ErrorActionPreference = 'Continue'
$script:fail = 0

function Ok($name, $detail)  { Write-Host ("[ok]   {0}: {1}" -f $name, $detail) }
function Bad($name, $detail) { Write-Host ("[FAIL] {0}: {1}" -f $name, $detail); $script:fail = 1 }

# git ------------------------------------------------------------------------
try { $v = (git --version) 2>&1 | Select-Object -First 1; Ok 'git' $v } catch { Bad 'git' $_ }

# WiX v5 (dotnet global tool) ------------------------------------------------
try { $v = (wix --version) 2>&1 | Select-Object -First 1; Ok 'wix' $v } catch { Bad 'wix' 'wix not on PATH (dotnet tool)' }

# Visual Studio + MSVC cross toolsets (ARM64 native + x64 cross) via vswhere --
$vswhere = Join-Path ${env:ProgramFiles(x86)} 'Microsoft Visual Studio\Installer\vswhere.exe'
if (Test-Path $vswhere) {
  $vsPath = (& $vswhere -latest -products * -property installationPath) 2>&1 | Select-Object -First 1
  if ($vsPath -and (Test-Path $vsPath)) {
    Ok 'visual-studio' $vsPath
    $msvc = Join-Path $vsPath 'VC\Tools\MSVC'
    $cl = Get-ChildItem -Path $msvc -Recurse -Filter cl.exe -ErrorAction SilentlyContinue
    $arm64 = $cl | Where-Object { $_.FullName -match '\\Host(arm64|x64|x86)\\arm64\\cl.exe$' } | Select-Object -First 1
    $x64   = $cl | Where-Object { $_.FullName -match '\\Host(arm64|x64|x86)\\x64\\cl.exe$' }   | Select-Object -First 1
    if ($arm64) { Ok 'msvc-arm64' $arm64.FullName } else { Bad 'msvc-arm64' "no arm64 cl.exe under $msvc" }
    if ($x64)   { Ok 'msvc-x64-cross' $x64.FullName } else { Bad 'msvc-x64-cross' "no x64 cl.exe under $msvc" }
  } else { Bad 'visual-studio' 'vswhere found no installation' }
} else { Bad 'visual-studio' 'vswhere.exe not found (VS Build Tools not installed)' }

# Windows SDK ----------------------------------------------------------------
$sdkBin = 'C:\Program Files (x86)\Windows Kits\10\bin'
$signtool = Get-ChildItem $sdkBin -Recurse -Filter signtool.exe -ErrorAction SilentlyContinue |
  Where-Object { $_.FullName -match '\\(arm64|x64)\\signtool.exe$' } | Select-Object -First 1
if ($signtool) { Ok 'windows-sdk' $signtool.FullName } else { Bad 'windows-sdk' "no signtool.exe under $sdkBin" }

# Windows Driver Kit (for the WFP split-tunnel driver) -----------------------
$inf2cat = Get-ChildItem $sdkBin -Recurse -Filter inf2cat.exe -ErrorAction SilentlyContinue | Select-Object -First 1
if ($inf2cat) { Ok 'wdk' $inf2cat.FullName } else { Bad 'wdk' 'inf2cat.exe not found (WDK missing)' }

# rsync + cmd ssh shell (build.sh mirrors the source in via rsync-over-ssh) ------
# build.ps1 itself is delivered per build (not baked into the image), so it is
# intentionally absent here - we verify the transport instead. provision.ps1 pins
# cwRsync (Chocolatey) and puts its real binary on the machine PATH.
$rsync = Get-Command rsync -ErrorAction SilentlyContinue
$rsyncPath = if ($rsync) { $rsync.Source } else {
  (Get-ChildItem "$env:ProgramData\chocolatey\lib\rsync" -Recurse -Filter rsync.exe -ErrorAction SilentlyContinue | Select-Object -First 1).FullName
}
if ($rsyncPath) { Ok 'rsync' $rsyncPath } else { Bad 'rsync' 'rsync not found (build source sync will fail)' }

$defShell = (Get-ItemProperty -Path 'HKLM:\SOFTWARE\OpenSSH' -Name DefaultShell -ErrorAction SilentlyContinue).DefaultShell
if ($defShell -match 'cmd\.exe') { Ok 'ssh-default-shell' $defShell }
else { Bad 'ssh-default-shell' "expected cmd.exe, got '$defShell' (cwRsync needs an unmangled path)" }

# build.ps1 runs over ssh via `powershell -File` explicitly (independent of the
# default shell). Confirm PowerShell is available.
Ok 'powershell' $PSVersionTable.PSVersion.ToString()

Write-Host ''
if ($script:fail -eq 0) { Write-Host 'SMOKE TEST PASSED'; exit 0 } else { Write-Host 'SMOKE TEST FAILED'; exit 1 }
