# Product acceptance guest executed inside the Windows ARM64 QEMU VM.
[CmdletBinding()]
param(
  [Parameter(Mandatory = $true)][string]$Msi,
  [Parameter(Mandatory = $true)][string]$ExpectedMsiSha256,
  [Parameter(Mandatory = $true)][string]$AppVersion,
  [Parameter(Mandatory = $true)][string]$SdkVersion,
  [Parameter(Mandatory = $true)][int]$Repeat,
  [Parameter(Mandatory = $true)][string]$Agent,
  [Parameter(Mandatory = $true)][string]$Credentials,
  [Parameter(Mandatory = $true)][string]$Fixture,
  [Parameter(Mandatory = $true)][string]$WorkDir
)

$ErrorActionPreference = "Stop"
$installLog = Join-Path $WorkDir "install.log"
$uninstallLog = Join-Path $WorkDir "uninstall.log"
$resultPath = Join-Path $WorkDir "result.json"
$agentLog = Join-Path $WorkDir "agent.log"
$stateDir = Join-Path $WorkDir "state"
$installed = $false
$exitCode = 1

New-Item -ItemType Directory -Force -Path $WorkDir, $stateDir | Out-Null

try {
  $copiedHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $Msi).Hash
  if ($copiedHash -ne $ExpectedMsiSha256) {
    throw "copied MSI hash does not match the locally built artifact"
  }

  & msiexec.exe /i $Msi /qn /norestart /l*v $installLog
  if ($LASTEXITCODE -ne 0) {
    throw "MSI install failed with exit code $LASTEXITCODE"
  }
  $installed = $true

  $service = $null
  for ($i = 0; $i -lt 60; $i++) {
    $service = Get-Service -Name urnetworkd -ErrorAction SilentlyContinue
    if ($service -and $service.Status -eq "Running") { break }
    Start-Sleep -Seconds 1
  }
  if (-not $service -or $service.Status -ne "Running") {
    throw "urnetworkd did not reach Running after installation"
  }

  $serviceConfig = Get-CimInstance Win32_Service -Filter "Name='urnetworkd'"
  if (-not $serviceConfig -or $serviceConfig.PathName -notmatch "URnetwork.+urnetworkd.exe") {
    throw "urnetworkd is not running from the installed URnetwork directory"
  }
  $serviceExecutable = $serviceConfig.PathName.Trim('"')
  $appExecutable = Join-Path (Split-Path -Parent $serviceExecutable) "URnetwork.exe"
  if (-not (Test-Path -LiteralPath $appExecutable -PathType Leaf)) {
    throw "the locally packaged URnetwork.exe was not installed beside urnetworkd.exe"
  }

  & $Agent `
    -credentials $Credentials `
    -fixture $Fixture `
    -active-client (Join-Path $WorkDir "active-client-id") `
    -state-dir $stateDir `
    -sdk-version $SdkVersion `
    -app-version $AppVersion `
    -service-version $SdkVersion `
    -repeat $Repeat `
    1> $resultPath 2> $agentLog
  if ($LASTEXITCODE -ne 0) {
    throw "acceptance agent failed with exit code $LASTEXITCODE"
  }
  $result = Get-Content -Raw -LiteralPath $resultPath | ConvertFrom-Json
  if (-not $result.ok -or $result.platform -ne "windows" -or $result.repetitions -ne $Repeat) {
    throw "acceptance agent emitted an invalid result"
  }

  # The Wintun adapter identity persists by design, but tunnel routes and
  # addresses must be gone after stop_tunnel.
  $remainingRoutes = @(Get-NetRoute -InterfaceAlias URnetwork -AddressFamily IPv4 `
      -ErrorAction SilentlyContinue | Where-Object { $_.DestinationPrefix -ne "255.255.255.255/32" })
  if ($remainingRoutes.Count -ne 0) {
    throw "URnetwork routes remained after disconnect"
  }

  $exitCode = 0
}
catch {
  $_ | Out-String | Set-Content -LiteralPath (Join-Path $WorkDir "failure.txt")
  Write-Error $_
}
finally {
  $serviceLog = "C:\ProgramData\URnetwork\service\logs\urnetworkd.log"
  if (Test-Path -LiteralPath $serviceLog) {
    Copy-Item -LiteralPath $serviceLog -Destination (Join-Path $WorkDir "urnetworkd.log") -Force
  }
  if ($installed) {
    & msiexec.exe /x $Msi /qn /norestart /l*v $uninstallLog
    if ($LASTEXITCODE -ne 0 -and $exitCode -eq 0) {
      $exitCode = 1
    }
    if (Get-Service -Name urnetworkd -ErrorAction SilentlyContinue) {
      "urnetworkd remained installed after MSI removal" | Set-Content -LiteralPath (Join-Path $WorkDir "uninstall-failure.txt")
      $exitCode = 1
    }
  }
}

exit $exitCode
