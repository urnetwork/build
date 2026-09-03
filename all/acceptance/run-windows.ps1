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
  [Parameter(Mandatory = $true)][string]$Tests,
  [Parameter(Mandatory = $true)][string]$Fixture,
  [Parameter(Mandatory = $true)][string]$WorkDir
)

$ErrorActionPreference = "Stop"
$installLog = Join-Path $WorkDir "install.log"
$uninstallLog = Join-Path $WorkDir "uninstall.log"
$resultPath = Join-Path $WorkDir "result.json"
$agentLog = Join-Path $WorkDir "agent.log"
$stateDir = Join-Path $WorkDir "state"
$providerDir = Join-Path $WorkDir "peer-provider"
$providerReady = Join-Path $providerDir "provider-client-id"
$providerStop = Join-Path $providerDir "stop"
$providerResult = Join-Path $providerDir "result.json"
$providerActiveClient = Join-Path $providerDir "active-client-id"
$providerState = Join-Path $providerDir "state"
$provider = $null
$appAgentState = $null
$installed = $false
$exitCode = 1

. (Join-Path $PSScriptRoot "run-windows-lib.ps1")

New-Item -ItemType Directory -Force -Path $WorkDir, $stateDir, $providerDir | Out-Null

try {
  $nativeMsi = Resolve-AcceptanceNativePath -LiteralPath $Msi
  $copiedHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $nativeMsi).Hash
  if ($copiedHash -ne $ExpectedMsiSha256) {
    throw "copied MSI hash does not match the locally built artifact"
  }

  $installExitCode = Invoke-AcceptanceMsi -Action Install -Msi $nativeMsi -Log $installLog
  if ($installExitCode -ne 0) {
    throw "MSI install failed with exit code $installExitCode"
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

  # Run the service's deterministic native test suite from the exact binary in
  # the MSI before any live account/tunnel traffic. This catches Windows-only
  # policy regressions (including invalid MIB_IPINTERFACE_ROW preparation) in
  # the same architecture and package the acceptance case is about.
  $selfTestExitCode = Invoke-AcceptanceProcess `
    -FilePath $serviceExecutable `
    -ArgumentList @("selftest") `
    -StandardOutput (Join-Path $WorkDir "selftest.log") `
    -StandardError (Join-Path $WorkDir "selftest.err.log")
  if ($selfTestExitCode -ne 0) {
    throw "urnetworkd selftest failed with exit code $selfTestExitCode"
  }

  # QEMU's slirp DNS proxy can lag the SSH service during a fresh guest boot.
  # The signup lifecycle below deliberately uses the ordinary OS resolver, so
  # wait for that exact name path before its first non-idempotent POST. The
  # provider's SDK uses in-process DoH and therefore cannot prove this path is
  # ready merely by reaching its own ready marker.
  Wait-AcceptanceControlPlaneDns -HostNames @(
    "api.bringyour.com",
    "connect.bringyour.com"
  )

  $appAgentState = Install-AcceptanceAppScopedAgent `
    -ServiceExecutable $serviceExecutable `
    -Agent $Agent `
    -BackupPath (Join-Path $WorkDir "URnetwork.exe.packaged")
  $appAgent = $appAgentState.Executable

  $physicalRoute = Get-NetRoute -AddressFamily IPv4 -DestinationPrefix "0.0.0.0/0" |
    Where-Object { $_.InterfaceAlias -ne "URnetwork" } |
    Sort-Object RouteMetric |
    Select-Object -First 1
  if (-not $physicalRoute -or $physicalRoute.InterfaceIndex -le 0) {
    throw "no physical IPv4 default route is available for the peer provider"
  }
  Remove-Item -LiteralPath $providerReady, $providerStop, $providerResult -Force -ErrorAction SilentlyContinue
  $provider = Start-Process -FilePath $appAgent -PassThru `
    -RedirectStandardOutput (Join-Path $WorkDir "peer-provider.stdout.log") `
    -RedirectStandardError (Join-Path $WorkDir "peer-provider.log") `
    -ArgumentList @(
      "-peer-provider",
      "-credentials", $Credentials,
      "-active-client", $providerActiveClient,
      "-state-dir", $providerState,
      "-sdk-version", $SdkVersion,
      "-app-version", $AppVersion,
      "-peer-provider-ready", $providerReady,
      "-peer-provider-stop", $providerStop,
      "-peer-provider-result", $providerResult,
      "-peer-provider-egress-index", $physicalRoute.InterfaceIndex
    )
  for ($i = 0; $i -lt 180; $i++) {
    if (Test-Path -LiteralPath $providerReady -PathType Leaf) { break }
    if ($provider.HasExited) {
      throw "peer provider exited before becoming ready with code $($provider.ExitCode)"
    }
    Start-Sleep -Seconds 1
  }
  if (-not (Test-Path -LiteralPath $providerReady -PathType Leaf)) {
    throw "timed out waiting for the peer provider"
  }

  $agentArguments = @(
    "-credentials", $Credentials,
    "-tests", $Tests,
    "-fixture", $Fixture,
    "-active-client", (Join-Path $WorkDir "active-client-id"),
    "-peer-provider-client", $providerReady,
    "-state-dir", $stateDir,
    "-sdk-version", $SdkVersion,
    "-app-version", $AppVersion,
    "-service-version", $SdkVersion,
    "-repeat", $Repeat
  )
  $agentExitCode = Invoke-AcceptanceProcess `
    -FilePath $appAgent `
    -ArgumentList $agentArguments `
    -StandardOutput $resultPath `
    -StandardError $agentLog
  if ($agentExitCode -ne 0) {
    throw "acceptance agent failed with exit code $agentExitCode"
  }
  $result = Get-Content -Raw -LiteralPath $resultPath | ConvertFrom-Json
  if (-not $result.ok -or $result.platform -ne "windows" -or $result.repetitions -ne $Repeat) {
    throw "acceptance agent emitted an invalid result"
  }
  if (@($result.peer_to_peer).Count -ne $Repeat) {
    throw "acceptance agent did not emit one peer-to-peer proof per repetition"
  }
  foreach ($proof in @($result.peer_to_peer)) {
    if (-not $proof.provider_selected -or $proof.remote_egress_packets -le 0 -or `
        $proof.remote_egress_bytes -le 0 -or $proof.remote_ingress_packets -le 0 -or `
        $proof.remote_ingress_bytes -le 0) {
      throw "acceptance agent emitted an incomplete peer-to-peer proof"
    }
  }

  New-Item -ItemType File -Force -Path $providerStop | Out-Null
  $providerProof = Wait-AcceptanceProvider `
    -Process $provider `
    -TimeoutMilliseconds 60000 `
    -ResultPath $providerResult
  if (-not $providerProof.ok -or $providerProof.remote_egress_packets -le 0 -or `
      $providerProof.remote_egress_bytes -le 0 -or $providerProof.remote_ingress_packets -le 0 -or `
      $providerProof.remote_ingress_bytes -le 0) {
    throw "peer provider emitted an incomplete bidirectional traffic proof"
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
  if ($provider -and -not $provider.HasExited) {
    New-Item -ItemType File -Force -Path $providerStop | Out-Null
    $providerWait = Wait-AcceptanceProcessExit -Process $provider -TimeoutMilliseconds 30000
    if (-not $providerWait.Exited) {
      $provider.Kill()
      $provider.WaitForExit()
    }
  }
  $serviceLog = "C:\ProgramData\URnetwork\service\logs\urnetworkd.log"
  if (Test-Path -LiteralPath $serviceLog) {
    Copy-Item -LiteralPath $serviceLog -Destination (Join-Path $WorkDir "urnetworkd.log") -Force
  }
  if ($appAgentState) {
    try {
      Restore-AcceptancePackagedApp -State $appAgentState
      $appAgentState = $null
    }
    catch {
      $_ | Out-String | Set-Content -LiteralPath (Join-Path $WorkDir "app-restore-failure.txt")
      $exitCode = 1
    }
  }
  if ($installed) {
    $uninstallExitCode = Invoke-AcceptanceMsi -Action Uninstall -Msi $nativeMsi -Log $uninstallLog
    if ($uninstallExitCode -ne 0 -and $exitCode -eq 0) {
      $exitCode = 1
    }
    if (Get-Service -Name urnetworkd -ErrorAction SilentlyContinue) {
      "urnetworkd remained installed after MSI removal" | Set-Content -LiteralPath (Join-Path $WorkDir "uninstall-failure.txt")
      $exitCode = 1
    }
  }

  $privateInputCleanupFailed = $false
  foreach ($privateInput in @($Credentials, $Tests)) {
    try {
      if (Test-Path -LiteralPath $privateInput) {
        Remove-Item -LiteralPath $privateInput -Force
      }
      if (Test-Path -LiteralPath $privateInput) {
        $privateInputCleanupFailed = $true
      }
    }
    catch {
      $privateInputCleanupFailed = $true
    }
  }
  if ($privateInputCleanupFailed) {
    "private acceptance inputs could not be removed from the guest" |
      Set-Content -LiteralPath (Join-Path $WorkDir "private-input-cleanup-failure.txt")
    $exitCode = 1
  }
}

exit $exitCode
