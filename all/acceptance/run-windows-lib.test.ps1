[CmdletBinding()]
param(
  [Parameter(Mandatory = $true)][string]$Fixture
)

$ErrorActionPreference = "Stop"
. (Join-Path $PSScriptRoot "run-windows-lib.ps1")

$native = Resolve-AcceptanceNativePath -LiteralPath $Fixture
if ($native -notmatch '^[A-Za-z]:\\') {
  throw "resolved fixture is not an absolute native Windows path: $native"
}
if ($native.Contains('/')) {
  throw "resolved fixture retained forward slashes: $native"
}
if (-not (Test-Path -LiteralPath $native -PathType Leaf)) {
  throw "resolved fixture does not identify the source file: $native"
}

$missingFailed = $false
try {
  Resolve-AcceptanceNativePath -LiteralPath "$Fixture.missing" | Out-Null
}
catch {
  $missingFailed = $true
}
if (-not $missingFailed) {
  throw "missing acceptance input was treated as a native file path"
}

$probe = Join-Path $PSScriptRoot "native-stderr-probe.cmd"
$probeOut = Join-Path $PSScriptRoot "native-stderr-probe.out"
$probeErr = Join-Path $PSScriptRoot "native-stderr-probe.err"
try {
  @(
    "@echo off",
    "echo expected-progress 1>&2",
    "exit /b 0"
  ) | Set-Content -LiteralPath $probe -Encoding Ascii
  $probeExitCode = Invoke-AcceptanceProcess `
    -FilePath $probe `
    -ArgumentList @() `
    -StandardOutput $probeOut `
    -StandardError $probeErr
  if ($probeExitCode -ne 0) {
    throw "zero-exit native stderr probe returned $probeExitCode"
  }
  if ((Get-Content -Raw -LiteralPath $probeErr).Trim() -ne "expected-progress") {
    throw "native stderr probe did not preserve its progress output"
  }
}
finally {
  Remove-Item -LiteralPath $probe, $probeOut, $probeErr -Force -ErrorAction SilentlyContinue
}

# SSH readiness does not imply QEMU's slirp DNS proxy is ready. Exercise the
# bounded retry without sleeping so the first non-idempotent signup POST never
# becomes the network-readiness probe.
$dnsState = [pscustomobject]@{
  ApiAttempts = 0
  ConnectAttempts = 0
  Sleeps = 0
}
$dnsResolver = {
  param([string]$HostName)
  if ($HostName -eq "api.bringyour.com") {
    $dnsState.ApiAttempts++
    if ($dnsState.ApiAttempts -eq 1) {
      throw "simulated startup miss"
    }
  }
  elseif ($HostName -eq "connect.bringyour.com") {
    $dnsState.ConnectAttempts++
  }
  return [System.Net.IPAddress]::Parse("192.0.2.1")
}
$dnsSleeper = {
  param([int]$Milliseconds)
  if ($Milliseconds -ne 17) {
    throw "unexpected DNS retry delay: $Milliseconds"
  }
  $dnsState.Sleeps++
}
Wait-AcceptanceControlPlaneDns `
  -HostNames @("api.bringyour.com", "connect.bringyour.com") `
  -Attempts 2 `
  -RetryDelayMilliseconds 17 `
  -Resolver $dnsResolver `
  -Sleeper $dnsSleeper
if ($dnsState.ApiAttempts -ne 2 -or $dnsState.ConnectAttempts -ne 1 -or $dnsState.Sleeps -ne 1) {
  throw "control-plane DNS retry did not re-check the complete host set after one startup miss"
}

$terminalDnsState = [pscustomobject]@{ Sleeps = 0 }
$terminalDnsFailed = $false
try {
  Wait-AcceptanceControlPlaneDns `
    -HostNames @("api.bringyour.com") `
    -Attempts 2 `
    -RetryDelayMilliseconds 0 `
    -Resolver { param([string]$HostName) throw "still unavailable" } `
    -Sleeper { param([int]$Milliseconds) $terminalDnsState.Sleeps++ }
}
catch {
  $terminalDnsFailed = $_.Exception.Message -match "api\.bringyour\.com" -and `
    $_.Exception.Message -match "after 2 attempts" -and `
    $_.Exception.Message -match "still unavailable"
}
if (-not $terminalDnsFailed -or $terminalDnsState.Sleeps -ne 1) {
  throw "control-plane DNS terminal failure was not bounded or did not preserve its cause"
}

# The long-running provider can exit cleanly while Windows PowerShell 5.1 keeps
# ExitCode blank. Its atomic result file is the success contract; a blank shell
# property must not turn verified bidirectional traffic into a false failure.
$deferredExitProcess = [pscustomobject]@{
  ExitCode = $null
  TimedWaitCount = 0
  CompletionWaitCount = 0
  RefreshCount = 0
}
$deferredExitProcess | Add-Member -MemberType ScriptMethod -Name WaitForExit -Value {
  if ($args.Count -eq 1) {
    $this.TimedWaitCount++
    return $true
  }
  $this.CompletionWaitCount++
}
$deferredExitProcess | Add-Member -MemberType ScriptMethod -Name Refresh -Value {
  $this.RefreshCount++
}
$providerCompletionDir = Join-Path $env:TEMP "urnetwork-provider-completion-$PID"
$providerCompletion = Join-Path $providerCompletionDir "result.json"
try {
  New-Item -ItemType Directory -Force -Path $providerCompletionDir | Out-Null
  '{"ok":true}' | Set-Content -LiteralPath $providerCompletion -Encoding Ascii
  $completion = Wait-AcceptanceProvider `
    -Process $deferredExitProcess `
    -TimeoutMilliseconds 1000 `
    -ResultPath $providerCompletion
  if (-not $completion.ok) {
    throw "atomic provider success result was not accepted"
  }
  if ($deferredExitProcess.TimedWaitCount -ne 1 -or `
      $deferredExitProcess.CompletionWaitCount -ne 1 -or `
      $deferredExitProcess.RefreshCount -ne 1 -or `
      $null -ne $deferredExitProcess.ExitCode) {
    throw "blank-exit provider completion was not joined and refreshed exactly once"
  }
  Remove-Item -LiteralPath $providerCompletion -Force
  $missingResultFailed = $false
  try {
    Wait-AcceptanceProvider `
      -Process $deferredExitProcess `
      -TimeoutMilliseconds 1000 `
      -ResultPath $providerCompletion | Out-Null
  }
  catch {
    $missingResultFailed = $true
  }
  if (-not $missingResultFailed) {
    throw "blank exit code without an atomic provider result was accepted"
  }
}
finally {
  Remove-Item -LiteralPath $providerCompletionDir -Recurse -Force -ErrorAction SilentlyContinue
}

# Connected WFP permits the installed UI image path, not an arbitrary test
# binary path. Prove that the SDK stand-in is installed at that exact sibling
# path and that the packaged UI is restored byte-for-byte afterward.
$appScopeDir = Join-Path $env:TEMP "urnetwork-acceptance-app-scope-$PID"
$appScopeInstallDir = Join-Path $appScopeDir "Program Files\URnetwork"
$fakeService = Join-Path $appScopeInstallDir "urnetworkd.exe"
$fakeApp = Join-Path $appScopeInstallDir "URnetwork.exe"
$fakeAgent = Join-Path $appScopeDir "acceptance\agent.exe"
$fakeBackup = Join-Path $appScopeDir "packaged-app.backup"
try {
  New-Item -ItemType Directory -Force -Path $appScopeInstallDir, (Split-Path -Parent $fakeAgent) | Out-Null
  "service" | Set-Content -LiteralPath $fakeService -Encoding Ascii
  "packaged-app" | Set-Content -LiteralPath $fakeApp -Encoding Ascii
  "acceptance-agent" | Set-Content -LiteralPath $fakeAgent -Encoding Ascii

  $appState = Install-AcceptanceAppScopedAgent `
    -ServiceExecutable $fakeService `
    -Agent $fakeAgent `
    -BackupPath $fakeBackup
  if ($appState.Executable -ne (Get-Item -LiteralPath $fakeApp).FullName) {
    throw "acceptance agent did not use the exact installed UI image path"
  }
  if ((Get-Content -Raw -LiteralPath $fakeApp) -ne (Get-Content -Raw -LiteralPath $fakeAgent)) {
    throw "installed UI path does not contain the acceptance agent"
  }
  if ((Get-Content -Raw -LiteralPath $fakeBackup).Trim() -ne "packaged-app") {
    throw "packaged UI was not preserved before acceptance substitution"
  }

  Restore-AcceptancePackagedApp -State $appState
  if ((Get-Content -Raw -LiteralPath $fakeApp).Trim() -ne "packaged-app") {
    throw "packaged UI was not restored after acceptance substitution"
  }
  if (Test-Path -LiteralPath $fakeBackup) {
    throw "packaged UI backup remained after restoration"
  }
}
finally {
  Remove-Item -LiteralPath $appScopeDir -Recurse -Force -ErrorAction SilentlyContinue
}

Write-Output "windows acceptance helper tests passed"
