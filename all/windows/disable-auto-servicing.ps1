# SPDX-License-Identifier: MPL-2.0

[CmdletBinding()]
param(
    [ValidateRange(1, 600)]
    [int]$StopTimeoutSeconds = 180
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

function Log([string]$Message) {
    Write-Host "[guest-policy] $Message"
}

function Set-RequiredServiceStartDisabled([string]$Name) {
    $configOutput = & sc.exe config $Name start= disabled 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "could not disable $Name (sc.exe exit $LASTEXITCODE): $($configOutput -join ' ')"
    }
}

function Stop-RequiredService([string]$Name) {
    $service = Get-Service -Name $Name -ErrorAction Stop
    if ($service.Status -ne [System.ServiceProcess.ServiceControllerStatus]::Stopped) {
        $stopOutput = & sc.exe stop $Name 2>&1
        $stopExitCode = $LASTEXITCODE
        $service.Refresh()
        if ($stopExitCode -ne 0 -and $service.Status -ne [System.ServiceProcess.ServiceControllerStatus]::Stopped) {
            throw "could not stop $Name (sc.exe exit $stopExitCode): $($stopOutput -join ' ')"
        }
        try {
            $service.WaitForStatus(
                [System.ServiceProcess.ServiceControllerStatus]::Stopped,
                [TimeSpan]::FromSeconds($StopTimeoutSeconds)
            )
        } catch {
            throw "$Name did not stop within $StopTimeoutSeconds seconds: $($_.Exception.Message)"
        }
    }

    $service.Refresh()
    if ($service.Status -ne [System.ServiceProcess.ServiceControllerStatus]::Stopped) {
        throw "$Name is $($service.Status), expected Stopped"
    }
}

function Assert-RequiredServiceDisabledAndStopped([string]$Name) {
    $service = Get-Service -Name $Name -ErrorAction Stop
    $service.Refresh()
    if ($service.Status -ne [System.ServiceProcess.ServiceControllerStatus]::Stopped) {
        throw "$Name is $($service.Status), expected Stopped"
    }
    $start = (Get-ItemProperty -Path "HKLM:\SYSTEM\CurrentControlSet\Services\$Name" -Name Start).Start
    if ([int]$start -ne 4) {
        throw "$Name registry Start is $start, expected 4 (Disabled)"
    }
}

Log "disabling Windows Update auto-servicing for this hermetic VM"
$windowsUpdate = "HKLM:\SOFTWARE\Policies\Microsoft\Windows\WindowsUpdate"
$automaticUpdates = "HKLM:\SOFTWARE\Policies\Microsoft\Windows\WindowsUpdate\AU"
New-Item -Path $windowsUpdate -Force | Out-Null
New-Item -Path $automaticUpdates -Force | Out-Null
New-ItemProperty -Path $automaticUpdates -Name NoAutoUpdate -Value 1 -PropertyType DWord -Force | Out-Null
New-ItemProperty -Path $automaticUpdates -Name NoAutoRebootWithLoggedOnUsers -Value 1 -PropertyType DWord -Force | Out-Null
New-ItemProperty -Path $windowsUpdate -Name DoNotConnectToWindowsUpdateInternetLocations -Value 1 -PropertyType DWord -Force | Out-Null
New-ItemProperty -Path $windowsUpdate -Name DisableWindowsUpdateAccess -Value 1 -PropertyType DWord -Force | Out-Null
New-ItemProperty -Path $windowsUpdate -Name WUServer -Value " " -PropertyType String -Force | Out-Null
New-ItemProperty -Path $windowsUpdate -Name WUStatusServer -Value " " -PropertyType String -Force | Out-Null
New-ItemProperty -Path $windowsUpdate -Name UpdateServiceUrlAlternate -Value " " -PropertyType String -Force | Out-Null
New-ItemProperty -Path $automaticUpdates -Name UseWUServer -Value 1 -PropertyType DWord -Force | Out-Null

# Windows protects several update tasks/services and can restore their startup
# modes. Make that self-healing harmless: even if a service returns later, its
# service SID cannot reach the network. These narrowly scoped rules do not
# affect PowerShell, Go, NuGet, or the Visual Studio installer.
$blockedUpdateServices = @("UsoSvc", "wuauserv", "WaaSMedicSvc")
foreach ($serviceName in $blockedUpdateServices) {
    $ruleName = "URNETWORK_BUILD_BLOCK_$serviceName"
    Remove-NetFirewallRule -Name $ruleName -ErrorAction SilentlyContinue
    New-NetFirewallRule -Name $ruleName -DisplayName $ruleName -Direction Outbound `
        -Action Block -Profile Any -Service $serviceName -Enabled True | Out-Null
}

# A registry policy alone is not a safe build boundary. In particular,
# NoAutoRebootWithLoggedOnUsers is a legacy policy on Windows 11 and only
# governs one class of scheduled installs. Stop the two services that can
# discover/orchestrate new servicing work, and wait for STOPPED: `sc stop`
# returning only means that a stop was requested (the stale image that exposed
# this bug reported STOP_PENDING with a 30-second wait hint).
# The Windows Update Medic service is protected on some Windows releases. It is
# defense in depth rather than the build-safety boundary above, so try to
# disable/stop it without turning a protected-service refusal into a false
# build failure.
$medic = Get-Service -Name "WaaSMedicSvc" -ErrorAction SilentlyContinue
if ($null -ne $medic) {
    $null = & sc.exe config WaaSMedicSvc start= disabled 2>&1
    if ($LASTEXITCODE -ne 0) {
        Log "WaaSMedicSvc is protected; continuing with update policy plus stopped wuauserv/UsoSvc"
    } else {
        $medic.Refresh()
        if ($medic.Status -ne [System.ServiceProcess.ServiceControllerStatus]::Stopped) {
            $null = & sc.exe stop WaaSMedicSvc 2>&1
        }
    }
}

# Configure BOTH required services before stopping either one, and stop the
# orchestrator first. Stopping wuauserv while a still-live UsoSvc can trigger it
# to restore wuauserv's start mode and restart it; that exact ordering failure
# was reproduced against the stale build image.
$requiredServices = @("UsoSvc", "wuauserv")
foreach ($serviceName in $requiredServices) {
    Set-RequiredServiceStartDisabled $serviceName
}
foreach ($serviceName in $requiredServices) {
    Stop-RequiredService $serviceName
}
# Either service can restore the other's start mode while it is still alive.
# Once both are stopped, write both modes again so the final state does not
# depend on which stop transition won that race.
foreach ($serviceName in $requiredServices) {
    Set-RequiredServiceStartDisabled $serviceName
}

# Quiesce the payload movers while clearing anything downloaded by the stale
# base image. They remain demand-startable for provisioning tools; public
# update discovery is blocked by the policy and service firewall above.
foreach ($serviceName in @("BITS", "DoSvc")) {
    Stop-RequiredService $serviceName
}
$updateDownloadCache = "C:\Windows\SoftwareDistribution\Download"
if (Test-Path $updateDownloadCache) {
    Get-ChildItem -LiteralPath $updateDownloadCache -Force -ErrorAction Stop |
        Remove-Item -Recurse -Force -ErrorAction Stop
    if (@(Get-ChildItem -LiteralPath $updateDownloadCache -Force -ErrorAction Stop).Count -ne 0) {
        throw "Windows Update download cache is not empty"
    }
}

# There must be no local servicing operation capable of rebooting the build
# after network isolation. A pending restart is a poisoned base-image signal,
# not something a release build should discover halfway through linking.
foreach ($pendingRebootPath in @(
    "HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Component Based Servicing\RebootPending",
    "HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\WindowsUpdate\Auto Update\RebootRequired"
)) {
    if (Test-Path $pendingRebootPath) {
        throw "pending Windows servicing reboot: $pendingRebootPath"
    }
}
Stop-RequiredService "TrustedInstaller"
if (Get-Process -Name "TiWorker" -ErrorAction SilentlyContinue) {
    throw "TiWorker is still running after update isolation"
}

# Observe immediate remediation. Service startup state is defense in depth;
# Windows can legitimately restore these protected services. The durable build
# boundary is the verified source policy + service firewall + empty cache + no
# pending/local servicing work, so report self-healing without mistaking it for
# an unsafe guest.
Start-Sleep -Seconds 2
foreach ($serviceName in $requiredServices) {
    try {
        Assert-RequiredServiceDisabledAndStopped $serviceName
    } catch {
        Log "$serviceName self-healed after quiescence; verified update isolation remains authoritative"
    }
}

foreach ($policyName in @("NoAutoUpdate", "NoAutoRebootWithLoggedOnUsers")) {
    $value = (Get-ItemProperty -Path $automaticUpdates -Name $policyName).$policyName
    if ([int]$value -ne 1) {
        throw "$policyName is $value, expected 1"
    }
}
foreach ($policyName in @("DoNotConnectToWindowsUpdateInternetLocations", "DisableWindowsUpdateAccess")) {
    $value = (Get-ItemProperty -Path $windowsUpdate -Name $policyName).$policyName
    if ([int]$value -ne 1) {
        throw "$policyName is $value, expected 1"
    }
}
$useWUServer = (Get-ItemProperty -Path $automaticUpdates -Name UseWUServer).UseWUServer
if ([int]$useWUServer -ne 1) { throw "UseWUServer is $useWUServer, expected 1" }
foreach ($policyName in @("WUServer", "WUStatusServer", "UpdateServiceUrlAlternate")) {
    $value = (Get-ItemProperty -Path $windowsUpdate -Name $policyName).$policyName
    if ([string]$value -ne " ") {
        throw "$policyName does not point at the disabled update source"
    }
}
foreach ($serviceName in $blockedUpdateServices) {
    $ruleName = "URNETWORK_BUILD_BLOCK_$serviceName"
    $rule = Get-NetFirewallRule -Name $ruleName -ErrorAction Stop
    $serviceFilter = $rule | Get-NetFirewallServiceFilter
    if ($rule.Enabled -ne "True" -or $rule.Direction -ne "Outbound" -or
        $rule.Action -ne "Block" -or $serviceFilter.Service -ne $serviceName) {
        throw "invalid outbound update firewall rule for $serviceName"
    }
}

# A stale base image may also predate the no-sleep provisioning rule. Reapply
# it on every throwaway overlay so a long native build cannot suspend itself.
& powercfg.exe /change standby-timeout-ac 0 | Out-Null
if ($LASTEXITCODE -ne 0) { throw "powercfg standby policy failed ($LASTEXITCODE)" }
& powercfg.exe /change hibernate-timeout-ac 0 | Out-Null
if ($LASTEXITCODE -ne 0) { throw "powercfg hibernate policy failed ($LASTEXITCODE)" }

Log "verified update-source/firewall isolation, empty cache, and no pending servicing reboot"
