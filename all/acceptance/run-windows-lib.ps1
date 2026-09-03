# Shared, side-effect-free helpers for the Windows acceptance guest.

function Resolve-AcceptanceNativePath {
  [CmdletBinding()]
  param(
    [Parameter(Mandatory = $true)][string]$LiteralPath
  )

  # PowerShell file APIs accept C:/path, but the Windows Installer service can
  # reinterpret that spelling as /path and lose the drive. Resolve through the
  # filesystem provider so native executables always receive C:\path.
  $resolved = (Get-Item -LiteralPath $LiteralPath -ErrorAction Stop).FullName
  if ($resolved -notmatch '^[A-Za-z]:\\') {
    throw "acceptance input did not resolve to an absolute native Windows path: $resolved"
  }
  return $resolved
}

function Invoke-AcceptanceMsi {
  [CmdletBinding()]
  param(
    [Parameter(Mandatory = $true)][ValidateSet("Install", "Uninstall")][string]$Action,
    [Parameter(Mandatory = $true)][string]$Msi,
    [Parameter(Mandatory = $true)][string]$Log
  )

  $switch = if ($Action -eq "Install") { "/i" } else { "/x" }
  $process = Start-Process -FilePath "$env:SystemRoot\System32\msiexec.exe" `
    -ArgumentList @($switch, $Msi, "/qn", "/norestart", "/l*v", $Log) `
    -Wait -PassThru
  return $process.ExitCode
}

function Invoke-AcceptanceProcess {
  [CmdletBinding()]
  param(
    [Parameter(Mandatory = $true)][string]$FilePath,
    [Parameter(Mandatory = $true)][AllowEmptyCollection()][string[]]$ArgumentList,
    [Parameter(Mandatory = $true)][string]$StandardOutput,
    [Parameter(Mandatory = $true)][string]$StandardError
  )

  # With $ErrorActionPreference=Stop, Windows PowerShell promotes any native
  # stderr line into a terminating NativeCommandError even when the process is
  # healthy and stderr is redirected. Start-Process keeps the streams as files
  # and makes the real process exit code the only success criterion.
  $start = @{
    FilePath = $FilePath
    RedirectStandardOutput = $StandardOutput
    RedirectStandardError = $StandardError
    Wait = $true
    PassThru = $true
  }
  if ($ArgumentList.Count -gt 0) {
    $start.ArgumentList = $ArgumentList
  }
  $process = Start-Process @start
  return $process.ExitCode
}

function Wait-AcceptanceControlPlaneDns {
  [CmdletBinding()]
  param(
    [Parameter(Mandatory = $true)][string[]]$HostNames,
    [int]$Attempts = 60,
    [int]$RetryDelayMilliseconds = 1000,
    [scriptblock]$Resolver = {
      param([string]$HostName)
      [System.Net.Dns]::GetHostAddresses($HostName)
    },
    [scriptblock]$Sleeper = {
      param([int]$Milliseconds)
      Start-Sleep -Milliseconds $Milliseconds
    }
  )

  if ($HostNames.Count -eq 0) {
    throw "at least one control-plane DNS host is required"
  }
  if ($Attempts -lt 1) {
    throw "control-plane DNS attempts must be positive"
  }
  if ($RetryDelayMilliseconds -lt 0) {
    throw "control-plane DNS retry delay cannot be negative"
  }

  $lastFailure = "no lookup attempted"
  for ($attempt = 1; $attempt -le $Attempts; $attempt++) {
    $ready = $true
    foreach ($hostName in $HostNames) {
      try {
        $addresses = @(& $Resolver $hostName)
        if ($addresses.Count -eq 0) {
          throw "lookup returned no addresses"
        }
      }
      catch {
        $ready = $false
        $lastFailure = "$hostName`: $($_.Exception.Message)"
        break
      }
    }
    if ($ready) {
      return
    }
    if ($attempt -lt $Attempts) {
      & $Sleeper $RetryDelayMilliseconds
    }
  }

  throw "acceptance control-plane DNS did not become ready after $Attempts attempts ($lastFailure)"
}

function Wait-AcceptanceProcessExit {
  [CmdletBinding()]
  param(
    [Parameter(Mandatory = $true)]$Process,
    [Parameter(Mandatory = $true)][int]$TimeoutMilliseconds
  )

  if (-not $Process.WaitForExit($TimeoutMilliseconds)) {
    return [pscustomobject]@{ Exited = $false; ExitCode = $null }
  }

  # Join redirected-stream completion and refresh the process snapshot. Windows
  # PowerShell 5.1 can still leave ExitCode blank for this long-lived process,
  # so callers use the provider's atomic success marker as the authority.
  $Process.WaitForExit() | Out-Null
  $Process.Refresh()
  $exitCode = if ($null -eq $Process.ExitCode) { $null } else { [int]$Process.ExitCode }
  return [pscustomobject]@{ Exited = $true; ExitCode = $exitCode }
}

function Wait-AcceptanceProvider {
  [CmdletBinding()]
  param(
    [Parameter(Mandatory = $true)]$Process,
    [Parameter(Mandatory = $true)][int]$TimeoutMilliseconds,
    [Parameter(Mandatory = $true)][string]$ResultPath
  )

  $wait = Wait-AcceptanceProcessExit -Process $Process -TimeoutMilliseconds $TimeoutMilliseconds
  if (-not $wait.Exited) {
    throw "peer provider did not stop after the client completed"
  }
  if ($null -ne $wait.ExitCode -and $wait.ExitCode -ne 0) {
    throw "peer provider failed with exit code $($wait.ExitCode)"
  }
  if (-not (Test-Path -LiteralPath $ResultPath -PathType Leaf)) {
    $description = if ($null -eq $wait.ExitCode) { "unavailable" } else { $wait.ExitCode }
    throw "peer provider exited without its atomic success result (exit code $description)"
  }
  return Get-Content -Raw -LiteralPath $ResultPath | ConvertFrom-Json
}

function Install-AcceptanceAppScopedAgent {
  [CmdletBinding()]
  param(
    [Parameter(Mandatory = $true)][string]$ServiceExecutable,
    [Parameter(Mandatory = $true)][string]$Agent,
    [Parameter(Mandatory = $true)][string]$BackupPath
  )

  $nativeService = Resolve-AcceptanceNativePath -LiteralPath $ServiceExecutable
  $nativeAgent = Resolve-AcceptanceNativePath -LiteralPath $Agent
  $appExecutable = Join-Path (Split-Path -Parent $nativeService) "URnetwork.exe"
  $nativeApp = Resolve-AcceptanceNativePath -LiteralPath $appExecutable
  if (Test-Path -LiteralPath $BackupPath) {
    throw "acceptance app backup already exists: $BackupPath"
  }

  # Connected-mode WFP policy grants physical-network access to the installed
  # UI image path. The acceptance agent is the UI's SDK stand-in, so execute it
  # from that exact path instead of widening production policy to an arbitrary
  # acceptance directory. Preserve the packaged UI for restoration before MSI
  # removal.
  $originalHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $nativeApp).Hash
  $agentHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $nativeAgent).Hash
  Copy-Item -LiteralPath $nativeApp -Destination $BackupPath -ErrorAction Stop
  try {
    Copy-Item -LiteralPath $nativeAgent -Destination $nativeApp -Force -ErrorAction Stop
    $installedHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $nativeApp).Hash
    if ($installedHash -ne $agentHash) {
      throw "acceptance agent at the installed app path does not match its source"
    }
  }
  catch {
    Copy-Item -LiteralPath $BackupPath -Destination $nativeApp -Force -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath $BackupPath -Force -ErrorAction SilentlyContinue
    throw
  }

  return [pscustomobject]@{
    Executable = $nativeApp
    Backup = $BackupPath
    OriginalSha256 = $originalHash
  }
}

function Restore-AcceptancePackagedApp {
  [CmdletBinding()]
  param(
    [Parameter(Mandatory = $true)]$State
  )

  if (-not (Test-Path -LiteralPath $State.Backup -PathType Leaf)) {
    throw "acceptance app backup is missing: $($State.Backup)"
  }
  Copy-Item -LiteralPath $State.Backup -Destination $State.Executable -Force -ErrorAction Stop
  $restoredHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $State.Executable).Hash
  if ($restoredHash -ne $State.OriginalSha256) {
    throw "restored packaged app does not match the installed original"
  }
  Remove-Item -LiteralPath $State.Backup -Force -ErrorAction Stop
}
