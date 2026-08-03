[CmdletBinding()]
param(
    [string]$RecoveryStatePath = (Join-Path $env:LOCALAPPDATA 'Moon Bridge\recovery\recovery-state-v2.json'),
    [string]$CodexConfigPath = '',
    [int]$GatewayPid = 0,
    [int]$GatewayPort = 38440,
    [int]$CapturePort = 38441,
    [string]$ExpectedSidecarPath = '',
    [switch]$ForceKillGateway
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

if ([string]::IsNullOrWhiteSpace($CodexConfigPath)) {
    $codexHome = if ([string]::IsNullOrWhiteSpace($env:CODEX_HOME)) {
        Join-Path $env:USERPROFILE '.codex'
    } else {
        $env:CODEX_HOME
    }
    $CodexConfigPath = Join-Path $codexHome 'config.toml'
}

function Test-ListeningPort([int]$Port) {
    return $null -ne (Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue)
}

function Get-ListeningPids([int]$Port) {
    return @(Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue | Select-Object -ExpandProperty OwningProcess -Unique)
}

function Get-SafeProcessInfo([int]$Pid) {
    $process = Get-Process -Id $Pid -ErrorAction Stop
    $path = $null
    try { $path = $process.Path } catch { }
    [pscustomobject]@{
        Id = $process.Id
        Name = $process.ProcessName
        Path = $path
    }
}

Write-Output "Recovery state: $RecoveryStatePath"
$recovery = $null
if (Test-Path -LiteralPath $RecoveryStatePath -PathType Leaf) {
    $recovery = Get-Content -LiteralPath $RecoveryStatePath -Raw | ConvertFrom-Json
    $required = @('schemaVersion', 'phase', 'integrationActive', 'captureStateLastKnown', 'relayActiveLastKnown')
    $propertyNames = @($recovery.PSObject.Properties.Name)
    foreach ($field in $required) {
        if ($propertyNames -notcontains $field) {
            throw "Recovery state is missing required field: $field"
        }
    }
    Write-Output "  schemaVersion=$($recovery.schemaVersion) phase=$($recovery.phase)"
    Write-Output "  integrationActive=$($recovery.integrationActive) relayActiveLastKnown=$($recovery.relayActiveLastKnown)"
    Write-Output "  captureStateLastKnown=$($recovery.captureStateLastKnown)"
    if ($recovery.PSObject.Properties.Name -contains 'reconciliationStatus') {
        Write-Output "  reconciliationStatus=$($recovery.reconciliationStatus)"
    }
} else {
    Write-Output "  not present (no persisted recovery incident)"
}

if (Test-Path -LiteralPath $CodexConfigPath -PathType Leaf) {
    $configText = Get-Content -LiteralPath $CodexConfigPath -Raw
    $hasCaptureUrl = $configText -match ('127\.0\.0\.1:' + $CapturePort)
    $configHash = (Get-FileHash -LiteralPath $CodexConfigPath -Algorithm SHA256).Hash
    Write-Output "Codex config exists: yes"
    Write-Output "  captureUrlPresent=$hasCaptureUrl sha256=$configHash"
    if ($recovery -and $recovery.integrationActive -and -not $hasCaptureUrl) {
        Write-Warning 'Recovery state says integrationActive but the effective Codex config has no Capture URL. Do not auto-edit the config; inspect the reconciliation status.'
    }
} else {
    Write-Output "Codex config exists: no"
}

Write-Output "Gateway port $GatewayPort listening: $(Test-ListeningPort $GatewayPort)"
Write-Output "Capture port $CapturePort listening: $(Test-ListeningPort $CapturePort)"

if ($ForceKillGateway) {
    if ($GatewayPid -le 0) {
        throw '-ForceKillGateway requires an explicit -GatewayPid.'
    }
    $processInfo = Get-SafeProcessInfo $GatewayPid
    $gatewayOwners = @(Get-ListeningPids $GatewayPort)
    $isMoonBridge = $processInfo.Name -match '^moonbridge$' -or ($processInfo.Path -and [IO.Path]::GetFileNameWithoutExtension($processInfo.Path) -eq 'moonbridge')
    if (-not $isMoonBridge) {
        throw "Refusing to terminate PID $GatewayPid because it is not identified as moonbridge."
    }
    if ($gatewayOwners -notcontains $GatewayPid) {
        throw "Refusing to terminate PID $GatewayPid because it does not own Gateway port $GatewayPort."
    }
    if (-not [string]::IsNullOrWhiteSpace($ExpectedSidecarPath)) {
        if ([string]::IsNullOrWhiteSpace($processInfo.Path) -or [IO.Path]::GetFullPath($processInfo.Path) -ne [IO.Path]::GetFullPath($ExpectedSidecarPath)) {
            throw "Refusing to terminate PID $GatewayPid because its executable path does not match the expected sidecar path."
        }
    }
    $gatewayOwnersBeforeKill = @(Get-ListeningPids $GatewayPort)
    if ($gatewayOwnersBeforeKill -notcontains $GatewayPid) {
        throw "Refusing to terminate PID $GatewayPid because Gateway port ownership changed before termination."
    }
    Write-Output "Stopping verified Moon Bridge process PID $GatewayPid."
    Stop-Process -Id $GatewayPid -Force
    Start-Sleep -Milliseconds 500
    Write-Output "Gateway port $GatewayPort listening after kill: $(Test-ListeningPort $GatewayPort)"
}

Write-Output 'No secrets, config contents, cookies, or payloads were printed.'
