# Signs ur.exe for amd64 and arm64 on the build VM (DESIGN.md §4.10; REPO-M1-03): Authenticode with
# the certificate behind URN_SIGN_THUMBPRINT, SHA-256 with an RFC 3161 timestamp, as
# windows/app/build.ps1 signs its binaries, then signtool verify /pa /all. Unlike build.ps1, which skips
# signing when no thumbprint is set, this throws: signing is the gate (owner decision 77), and ur is
# never released unsigned.
#
#   sign-ur.ps1 -Directory <dir holding amd64\ur.exe and arm64\ur.exe>
#
# SPDX-License-Identifier: MPL-2.0
param(
  [Parameter(Mandatory = $true)][string]$Directory,
  [string]$Thumbprint = $env:URN_SIGN_THUMBPRINT,
  [string]$TimestampUrl = "http://timestamp.digicert.com"
)
$ErrorActionPreference = "Stop"

if (-not $Thumbprint) {
  throw "URN_SIGN_THUMBPRINT names no signing certificate, and ur is never released unsigned (owner decision 77)"
}
foreach ($arch in @("amd64", "arm64")) {
  $exe = Join-Path $Directory "$arch\ur.exe"
  if (-not (Test-Path $exe)) { throw "no $exe to sign" }
  signtool sign /sha1 $Thumbprint /fd SHA256 /tr $TimestampUrl /td SHA256 $exe
  if ($LASTEXITCODE -ne 0) { throw "signtool sign failed for $exe" }
  signtool verify /pa /all $exe
  if ($LASTEXITCODE -ne 0) { throw "signtool verify failed for $exe" }
}
Write-Host "signed and verified ur.exe for amd64 and arm64"
