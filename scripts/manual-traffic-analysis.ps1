[CmdletBinding()]
param(
    [ValidateSet('M1A', 'M1B', 'M2', 'M3', 'M4A', 'M4B', 'M5')]
    [string]$Case,
    [string]$CodexConfigPath = '',
    [string]$RecoveryStatePath = '',
    [int]$GatewayPid = 0,
    [int]$GatewayPort = 38440,
    [int]$CapturePort = 38441,
    [int]$DisplayScalePercent = 0,
    [string]$ExpectedSidecarPath = '',
    [switch]$ForceKillGateway,
    [switch]$SelfTest,
    [string]$ErrorCode = ''
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Get-PropertyValue($Object, [string]$Name, $Default = $null) {
    if ($null -ne $Object -and $Object.PSObject.Properties.Name -contains $Name) {
        return $Object.$Name
    }
    return $Default
}

function Get-StringSha256([string]$Value) {
    $sha = [System.Security.Cryptography.SHA256]::Create()
    try {
        $bytes = [System.Text.Encoding]::UTF8.GetBytes($Value)
        return ([BitConverter]::ToString($sha.ComputeHash($bytes))).Replace('-', '')
    } finally {
        $sha.Dispose()
    }
}

function Remove-TomlComment([string]$Line) {
    $quote = [char]0
    $escaped = $false
    for ($i = 0; $i -lt $Line.Length; $i++) {
        $character = $Line[$i]
        if ($quote -eq [char]0) {
            if ($character -eq [char]34 -or $character -eq [char]39) {
                $quote = $character
            } elseif ($character -eq [char]35) {
                return $Line.Substring(0, $i)
            }
        } elseif ($quote -eq [char]34) {
            if ($character -eq [char]92 -and -not $escaped) {
                $escaped = $true
                continue
            }
            if ($character -eq [char]34 -and -not $escaped) {
                $quote = [char]0
            }
            $escaped = $false
        } elseif ($character -eq [char]39 -and $character -eq $quote) {
            $quote = [char]0
        }
    }
    return $Line
}

# Manual diagnostic decoder for single-line simple basic/literal strings only.
# Not a full TOML implementation: unknown escapes keep the backslash+character.
function Convert-TomlBasicString([string]$Quote, [string]$Value) {
    if ($Quote -eq "'") {
        return $Value
    }
    $builder = [System.Text.StringBuilder]::new()
    $i = 0
    $length = $Value.Length
    while ($i -lt $length) {
        $character = $Value[$i]
        if ($character -ne [char]92) {
            [void]$builder.Append($character)
            $i++
            continue
        }
        if ($i + 1 -ge $length) {
            [void]$builder.Append($character)
            $i++
            continue
        }
        $escaped = $Value[$i + 1]
        switch ($escaped) {
            '"' { [void]$builder.Append('"'); break }
            '\' { [void]$builder.Append('\'); break }
            'n' { [void]$builder.Append("`n"); break }
            'r' { [void]$builder.Append("`r"); break }
            't' { [void]$builder.Append("`t"); break }
            default { [void]$builder.Append($character); [void]$builder.Append($escaped); break }
        }
        $i += 2
    }
    return $builder.ToString()
}

function Get-TomlTopLevelString([string]$Text, [string]$Key) {
    $table = $null
    foreach ($line in ($Text -split "`r?`n")) {
        $content = (Remove-TomlComment $line).Trim()
        if ([string]::IsNullOrWhiteSpace($content)) { continue }
        if ($content -match '^\[\[\s*([^\]]+)\s*\]\]$') {
            $table = $Matches[1].Trim()
            continue
        }
        if ($content -match '^\[\s*([^\]]+)\s*\]$') {
            $table = $Matches[1].Trim()
            continue
        }
        if ($null -ne $table) { continue }
        $pattern = '^' + [regex]::Escape($Key) + '\s*=\s*(["''])(.*?)\1\s*$'
        if ($content -match $pattern) {
            return [pscustomobject]@{
                Present = $true
                Value = Convert-TomlBasicString $Matches[1] $Matches[2]
            }
        }
    }
    return [pscustomobject]@{ Present = $false; Value = $null }
}

function Get-TomlTableString([string]$Text, [string]$TableName, [string]$Key) {
    $table = $null
    foreach ($line in ($Text -split "`r?`n")) {
        $content = (Remove-TomlComment $line).Trim()
        if ([string]::IsNullOrWhiteSpace($content)) { continue }
        if ($content -match '^\[\[\s*([^\]]+)\s*\]\]$') {
            $table = $Matches[1].Trim()
            continue
        }
        if ($content -match '^\[\s*([^\]]+)\s*\]$') {
            $table = $Matches[1].Trim()
            continue
        }
        if ($table -ne $TableName) { continue }
        $pattern = '^' + [regex]::Escape($Key) + '\s*=\s*(["''])(.*?)\1\s*$'
        if ($content -match $pattern) {
            return [pscustomobject]@{
                Present = $true
                Value = Convert-TomlBasicString $Matches[1] $Matches[2]
            }
        }
    }
    return [pscustomobject]@{ Present = $false; Value = $null }
}

function Get-SafeConfigState([string]$Path, [int]$CapturePort) {
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
        return [pscustomobject]@{
            Exists = $false
            ConfigFileFingerprint = $null
            BaseUrlPresent = $false
            BaseUrlFingerprint = $null
            MatchesCaptureUrl = $false
        }
    }
    $text = Get-Content -LiteralPath $Path -Raw
    $base = Get-TomlTopLevelString $text 'openai_base_url'
    $expected = "http://127.0.0.1:$CapturePort"
    return [pscustomobject]@{
        Exists = $true
        ConfigFileFingerprint = (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash
        BaseUrlPresent = [bool]$base.Present
        BaseUrlFingerprint = if ($base.Present) { Get-StringSha256 ([string]$base.Value) } else { $null }
        MatchesCaptureUrl = [bool]($base.Present -and $base.Value -eq $expected)
    }
}

function Get-SafeRecoveryState([string]$Path) {
    if ([string]::IsNullOrWhiteSpace($Path) -or -not (Test-Path -LiteralPath $Path -PathType Leaf)) { return $null }
    $value = Get-Content -LiteralPath $Path -Raw | ConvertFrom-Json
    [pscustomobject]@{
        SchemaVersion = Get-PropertyValue $value 'schemaVersion'
        Phase = Get-PropertyValue $value 'phase'
        IntegrationActive = [bool](Get-PropertyValue $value 'integrationActive' $false)
        CaptureState = Get-PropertyValue $value 'captureStateLastKnown'
        RelayActive = [bool](Get-PropertyValue $value 'relayActiveLastKnown' $false)
        ReconciliationStatus = Get-PropertyValue $value 'reconciliationStatus'
        AutoLogStatus = Get-PropertyValue $value 'autoLogStatus'
        AutoLogSessionId = [string](Get-PropertyValue (Get-PropertyValue $value 'autoLog') 'sessionId')
        UnsavedMayRemain = [bool](Get-PropertyValue $value 'unsavedObservationsMayRemain' $false)
        DiscardConfirmed = [bool](Get-PropertyValue $value 'unsavedDiscardConfirmed' $false)
    }
}

# --- Command journal (session-limited, read-only, non-sensitive) ---
# Reads the Rust command journal and aggregates only the adopted session's
# stop / finish_relay event counts. Never returns raw journal text; the
# record carries counts, a non-secret session id, and a safe blocked reason.

function Get-CommandJournalFiles {
    $files = [System.Collections.Generic.List[string]]::new()
    $debugJournal = Join-Path $PSScriptRoot '..\logs\command-journal.jsonl'
    $releaseJournal = Join-Path $env:LOCALAPPDATA 'Moon Bridge\logs\command-journal.jsonl'
    foreach ($base in @($debugJournal, $releaseJournal)) {
        foreach ($variant in @($base, ($base -replace '\.jsonl$', '.1.jsonl'))) {
            if (Test-Path -LiteralPath $variant -PathType Leaf) {
                $files.Add([IO.Path]::GetFullPath($variant))
            }
        }
    }
    return @($files)
}

function Read-JournalRecords([string[]]$Paths) {
    $records = [System.Collections.Generic.List[object]]::new()
    $blockedReason = $null
    foreach ($path in $Paths) {
        $lines = @(Get-Content -LiteralPath $path -Raw -ErrorAction SilentlyContinue | ForEach-Object { $_ -split "`r?`n" })
        for ($i = 0; $i -lt $lines.Count; $i++) {
            $line = $lines[$i]
            if ([string]::IsNullOrWhiteSpace($line)) { continue }
            $record = $null
            try { $record = $line | ConvertFrom-Json } catch { $record = $null }
            if ($null -ne $record) {
                $records.Add($record)
                continue
            }
            $isLast = $i -eq $lines.Count - 1 -or ($i -eq $lines.Count - 2 -and [string]::IsNullOrWhiteSpace($lines[$lines.Count - 1]))
            if ($isLast) {
                # Desktop may be appending this line; re-read the final line once.
                Start-Sleep -Milliseconds 300
                $retried = $null
                try {
                    $retried = (Get-Content -LiteralPath $path -Raw -ErrorAction SilentlyContinue |
                        ForEach-Object { $_ -split "`r?`n" } |
                        Where-Object { -not [string]::IsNullOrWhiteSpace($_) } |
                        Select-Object -Last 1) | ConvertFrom-Json
                } catch { $retried = $null }
                if ($null -eq $retried) {
                    if ($null -eq $blockedReason) { $blockedReason = 'journal_incomplete_tail' }
                } else {
                    $records.Add($retried)
                }
                continue
            }
            # A partial or corrupted line in the middle: immediate BLOCKED.
            if ($null -eq $blockedReason) { $blockedReason = 'journal_middle_line_unreadable' }
        }
    }
    return [pscustomobject]@{ Records = @($records); BlockedReason = $blockedReason }
}

# Counts how many invocation_id groups hold a complete invoke/acquired/end
# triple (end.result=ok) and how many hold an incomplete one. Ties the three
# events of one command call together the same way the Rust wrappers do.
function Get-InvocationTripleSummary($Records, [string]$Command) {
    $groups = @{}
    foreach ($record in @($Records)) {
        if ([string]$record.command -ne $Command) { continue }
        $id = [string]$record.invocation_id
        if (-not $groups.ContainsKey($id)) { $groups[$id] = @{ Invoke = 0; Acquired = 0; End = 0; EndOk = 0 } }
        switch ($record.event) {
            'invoke' { $groups[$id].Invoke++ }
            'acquired' { $groups[$id].Acquired++ }
            'end' {
                $groups[$id].End++
                if ($record.result -eq 'ok') { $groups[$id].EndOk++ }
            }
        }
    }
    $complete = 0
    $partial = 0
    foreach ($id in @($groups.Keys)) {
        $group = $groups[$id]
        if ($group.Invoke -eq 1 -and $group.Acquired -eq 1 -and $group.End -eq 1 -and $group.EndOk -eq 1) {
            $complete++
        } elseif ($group.Invoke -gt 0 -or $group.Acquired -gt 0 -or $group.End -gt 0) {
            $partial++
        }
    }
    return [pscustomobject]@{ CompleteTriples = $complete; PartialTriples = $partial }
}

function Get-CurrentSessionJournal([datetime]$StartedAt, [string]$RecoveryPath, [string[]]$Paths) {
    if ($null -eq $Paths -or $Paths.Count -eq 0) { $Paths = @(Get-CommandJournalFiles) }
    $empty = [pscustomobject]@{
        SessionId = $null
        StopInvoke = 0; StopAcquired = 0; StopEnd = 0; StopEndOk = 0
        FinishInvoke = 0; FinishAcquired = 0; FinishEnd = 0; FinishEndOk = 0
        StopCompleteTriples = 0; StopPartialTriples = 0
        FinishCompleteTriples = 0; FinishPartialTriples = 0
        Adopted = $false
        BlockedReason = $null
        RecoverySessionId = $null
    }
    $existingPaths = @($Paths | Where-Object { Test-Path -LiteralPath $_ -PathType Leaf })
    if ($existingPaths.Count -eq 0) {
        $empty.BlockedReason = 'journal_missing'
        return $empty
    }
    $read = Read-JournalRecords $existingPaths
    $records = $read.Records
    $startedUtc = $StartedAt.ToUniversalTime()
    # Adopt the latest start/end/ok session recorded at or after the script start.
    $startEnds = @($records | Where-Object {
        $_.kind -eq 'command' -and
        $_.command -eq 'traffic_analysis_start' -and
        $_.event -eq 'end' -and
        $_.result -eq 'ok' -and
        -not [string]::IsNullOrWhiteSpace([string]$_.session_id) -and
        ([datetimeoffset]::Parse([string]$_.t).UtcDateTime) -ge $startedUtc
    })
    if ($startEnds.Count -eq 0) {
        $empty.BlockedReason = if ($read.BlockedReason) { $read.BlockedReason } else { 'journal_no_start_ok' }
        return $empty
    }
    $latest = $startEnds | Sort-Object { [datetimeoffset]::Parse([string]$_.t).UtcDateTime } | Select-Object -Last 1
    $sessionId = [string]$latest.session_id
    $recoverySessionId = $null
    $recovery = Get-SafeRecoveryState $RecoveryPath
    if ($null -ne $recovery) { $recoverySessionId = [string]$recovery.AutoLogSessionId }
    $blockedReason = $read.BlockedReason
    if (-not [string]::IsNullOrWhiteSpace($recoverySessionId) -and $recoverySessionId -ne $sessionId) {
        $blockedReason = 'journal_session_mismatch'
    }
    $sessionRecords = @($records | Where-Object { $_.kind -eq 'command' -and [string]$_.session_id -eq $sessionId })
    $stop = @($sessionRecords | Where-Object { $_.command -eq 'traffic_analysis_stop' })
    $finish = @($sessionRecords | Where-Object { $_.command -eq 'traffic_analysis_finish_relay' })
    $stopTriples = Get-InvocationTripleSummary $stop 'traffic_analysis_stop'
    $finishTriples = Get-InvocationTripleSummary $finish 'traffic_analysis_finish_relay'
    return [pscustomobject]@{
        SessionId = $sessionId
        StopInvoke = @($stop | Where-Object { $_.event -eq 'invoke' }).Count
        StopAcquired = @($stop | Where-Object { $_.event -eq 'acquired' }).Count
        StopEnd = @($stop | Where-Object { $_.event -eq 'end' }).Count
        StopEndOk = @($stop | Where-Object { $_.event -eq 'end' -and $_.result -eq 'ok' }).Count
        FinishInvoke = @($finish | Where-Object { $_.event -eq 'invoke' }).Count
        FinishAcquired = @($finish | Where-Object { $_.event -eq 'acquired' }).Count
        FinishEnd = @($finish | Where-Object { $_.event -eq 'end' }).Count
        FinishEndOk = @($finish | Where-Object { $_.event -eq 'end' -and $_.result -eq 'ok' }).Count
        StopCompleteTriples = $stopTriples.CompleteTriples
        StopPartialTriples = $stopTriples.PartialTriples
        FinishCompleteTriples = $finishTriples.CompleteTriples
        FinishPartialTriples = $finishTriples.PartialTriples
        Adopted = $true
        BlockedReason = $blockedReason
        RecoverySessionId = $recoverySessionId
    }
}

function Test-StopJournal($Journal) {
    if ($Journal.BlockedReason) { return 'BLOCKED' }
    if ($Journal.StopEnd -ge 1 -and $Journal.StopEndOk -lt $Journal.StopEnd) { return 'FAIL' }
    if ($Journal.StopInvoke -eq 1 -and $Journal.StopAcquired -eq 1 -and $Journal.StopEnd -eq 1 -and $Journal.StopEndOk -eq 1 -and $Journal.StopCompleteTriples -eq 1 -and $Journal.StopPartialTriples -eq 0) { return 'PASS' }
    return 'BLOCKED'
}

function Test-FinishRelayAbsent($Journal) {
    if ($Journal.BlockedReason) { return 'BLOCKED' }
    if ($Journal.FinishInvoke -eq 0 -and $Journal.FinishAcquired -eq 0 -and $Journal.FinishEnd -eq 0) { return 'PASS' }
    return 'FAIL'
}

function Test-FinishRelayComplete($Journal) {
    if ($Journal.BlockedReason) { return 'BLOCKED' }
    if ($Journal.FinishInvoke -eq 0) { return 'BLOCKED' }
    if ($Journal.FinishEnd -ge 1 -and $Journal.FinishEndOk -lt $Journal.FinishEnd) { return 'FAIL' }
    if ($Journal.FinishInvoke -eq 1 -and $Journal.FinishAcquired -eq 1 -and $Journal.FinishEnd -eq 1 -and $Journal.FinishEndOk -eq 1 -and $Journal.FinishCompleteTriples -eq 1 -and $Journal.FinishPartialTriples -eq 0) { return 'PASS' }
    return 'BLOCKED'
}

function Test-ListeningPort([int]$Port) {
    return $null -ne (Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue)
}

function Get-ListeningPids([int]$Port) {
    return @(Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue | Select-Object -ExpandProperty OwningProcess -Unique)
}

function Get-SafeProcessInfo([int]$ProcessId) {
    $process = Get-Process -Id $ProcessId -ErrorAction Stop
    $path = $null
    try { $path = $process.Path } catch { }
    return [pscustomobject]@{
        Id = $process.Id
        Name = $process.ProcessName
        Path = $path
    }
}

# Builds a Metadata object for a check. Only pid/port/sha256-style keys are
# allowed; values must be integers or SHA-256 hex strings. Unknown keys and
# free-form values are dropped so the result record stays non-sensitive.
function Get-SafeMetadata([hashtable]$Values) {
    if ($null -eq $Values) { return $null }
    $allowedKeys = @('pid','port','gatewayPort','capturePort','sha256','configFileFingerprint','baseUrlFingerprint')
    $result = [ordered]@{}
    foreach ($key in $allowedKeys) {
        if (-not $Values.ContainsKey($key)) { continue }
        $value = $Values[$key]
        $isInteger = $value -is [int16] -or $value -is [int32] -or $value -is [int64] -or $value -is [uint16] -or $value -is [uint32] -or $value -is [uint64] -or $value -is [byte]
        $isSha256 = $value -is [string] -and $value -match '^[0-9A-Fa-f]{64}$'
        if (-not ($isInteger -or $isSha256)) { continue }
        $result[$key] = $value
    }
    if ($result.Count -eq 0) { return $null }
    return $result
}

function Test-ProcessExists([int]$ProcessId) {
    return $null -ne (Get-Process -Id $ProcessId -ErrorAction SilentlyContinue)
}

function Add-Check([string]$Id, [string]$Kind, [bool]$Passed, [string]$Message, [string]$Code = '', [hashtable]$SafeMetadata = $null) {
    $result = if ($Passed) { 'PASS' } else { 'FAIL' }
    if ($Passed) {
        Write-Host "PASS  $Message" -ForegroundColor Green
    } else {
        Write-Host "FAIL  $Message" -ForegroundColor Red
    }
    $check = [pscustomobject]@{
        Id = $Id
        Kind = $Kind
        Result = $result
        ErrorCode = if ($Passed) { $null } elseif ($Code) { $Code } else { 'manual_check_failed' }
        CheckedAt = (Get-Date).ToUniversalTime().ToString('o')
    }
    $metadata = Get-SafeMetadata $SafeMetadata
    if ($null -ne $metadata) {
        $check | Add-Member -NotePropertyName Metadata -NotePropertyValue $metadata
    }
    [void]$script:Checks.Add($check)
    return $Passed
}

function Add-ManualCheck([string]$Id, [string]$Prompt, [string]$Code = 'manual_check_failed') {
    Write-Host "`n$Prompt" -ForegroundColor Cyan
    do {
        $answer = (Read-Host '結果を入力してください: P=PASS / F=FAIL / B=BLOCKED').Trim().ToUpperInvariant()
    } while ($answer -notin @('P', 'F', 'B'))
    $result = switch ($answer) { 'P' { 'PASS' } 'F' { 'FAIL' } default { 'BLOCKED' } }
    $message = "$Id => $result"
    Write-Host $message -ForegroundColor $(if ($result -eq 'PASS') { 'Green' } elseif ($result -eq 'FAIL') { 'Red' } else { 'Yellow' })
    [void]$script:Checks.Add([pscustomobject]@{
        Id = $Id
        Kind = 'manual'
        Result = $result
        ErrorCode = if ($result -eq 'PASS') { $null } else { $Code }
        CheckedAt = (Get-Date).ToUniversalTime().ToString('o')
    })
}

function Add-ResultCheck([string]$Id, [string]$Result, [string]$Message, [string]$Code = 'manual_check_failed') {
    Write-Host "$Result  $Message" -ForegroundColor $(if ($Result -eq 'PASS') { 'Green' } elseif ($Result -eq 'FAIL') { 'Red' } else { 'Yellow' })
    [void]$script:Checks.Add([pscustomobject]@{
        Id = $Id
        Kind = 'automatic'
        Result = $Result
        ErrorCode = if ($Result -eq 'PASS') { $null } elseif ($Code) { $Code } else { 'manual_check_failed' }
        CheckedAt = (Get-Date).ToUniversalTime().ToString('o')
    })
    return $Result
}

function Resolve-CodexConfigPath {
    if (-not [string]::IsNullOrWhiteSpace($CodexConfigPath)) { return [IO.Path]::GetFullPath($CodexConfigPath) }
    $codexHomePath = if ([string]::IsNullOrWhiteSpace($env:CODEX_HOME)) { Join-Path $env:USERPROFILE '.codex' } else { $env:CODEX_HOME }
    return [IO.Path]::GetFullPath((Join-Path $codexHomePath 'config.toml'))
}

function Resolve-RecoveryPath {
    if (-not [string]::IsNullOrWhiteSpace($RecoveryStatePath)) { return [IO.Path]::GetFullPath($RecoveryStatePath) }
    return [IO.Path]::GetFullPath((Join-Path $env:LOCALAPPDATA 'Moon Bridge\recovery\recovery-state-v2.json'))
}

function Get-FinalResult {
    $failed = @($script:Checks | Where-Object Result -eq 'FAIL').Count
    $blocked = @($script:Checks | Where-Object Result -eq 'BLOCKED').Count
    if ($failed -gt 0) { return 'FAIL' }
    if ($blocked -gt 0) { return 'BLOCKED' }
    return 'PASS'
}

function Write-SafeResultRecord([string]$CaseId, [datetime]$StartedAt, [string]$ConfigPath, [string]$RecordDirectory = (Join-Path $PSScriptRoot '..\logs\manual')) {
    New-Item -ItemType Directory -Force -Path $RecordDirectory | Out-Null
    $recordPath = Join-Path $RecordDirectory ("traffic-analysis-$CaseId-" + (Get-Date -Format 'yyyyMMdd-HHmmss') + '.json')
    $record = [pscustomobject]@{
        caseId = $CaseId
        result = Get-FinalResult
        errorCode = if ($ErrorCode) { $ErrorCode } else { $null }
        startedAt = $StartedAt.ToUniversalTime().ToString('o')
        finishedAt = (Get-Date).ToUniversalTime().ToString('o')
        os = [System.Environment]::OSVersion.VersionString
        displayScale = if ($DisplayScalePercent -gt 0) { $DisplayScalePercent } else { $null }
        gatewayPort = $GatewayPort
        capturePort = $CapturePort
        checks = @($script:Checks)
        configPathProvided = -not [string]::IsNullOrWhiteSpace($CodexConfigPath)
        recoveryPathProvided = -not [string]::IsNullOrWhiteSpace($RecoveryStatePath)
    }
    $record | ConvertTo-Json -Depth 6 | Set-Content -LiteralPath $recordPath -Encoding utf8
    Write-Host "`nResult: $($record.result)" -ForegroundColor $(if ($record.result -eq 'PASS') { 'Green' } elseif ($record.result -eq 'FAIL') { 'Red' } else { 'Yellow' })
    Write-Host "Safe result record: $recordPath"
    return $record.result
}

function Write-SampleConfig([string]$Directory, [string]$Name, [string]$Content) {
    $path = Join-Path $Directory $Name
    Set-Content -LiteralPath $path -Value $Content -Encoding utf8
    return $path
}

function Invoke-SelfTest {
    $root = Join-Path ([IO.Path]::GetTempPath()) ("moon-bridge-manual-selftest-" + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Force -Path $root | Out-Null
    try {
        # 1: URL only inside a comment must not be detected as the top-level key.
        $commentOnly = Write-SampleConfig $root 'comment-only.toml' @'
# openai_base_url = "http://127.0.0.1:38441"
note = "not a key"
'@
        $state = Get-SafeConfigState $commentOnly 38441
        if ($state.BaseUrlPresent) { throw 'SelfTest: comment-only URL was detected as top-level.' }

        # 2: URL only under a table must not be detected as the top-level key.
        $tableOnly = Write-SampleConfig $root 'table-only.toml' @'
[example]
openai_base_url = "http://127.0.0.1:38441"
'@
        $state = Get-SafeConfigState $tableOnly 38441
        if ($state.BaseUrlPresent) { throw 'SelfTest: table-only URL was detected as top-level.' }

        # 3: A pseudo key inside a string value must not be detected.
        $stringOnly = Write-SampleConfig $root 'string-only.toml' @'
note = "openai_base_url = \"http://127.0.0.1:38441\""
'@
        $state = Get-SafeConfigState $stringOnly 38441
        if ($state.BaseUrlPresent) { throw 'SelfTest: pseudo key inside a string was detected.' }

        # 4: Top-level URL is detected and matches the requested Capture port.
        $topOnly = Write-SampleConfig $root 'top-only.toml' 'openai_base_url = "http://127.0.0.1:38441"'
        $state = Get-SafeConfigState $topOnly 38441
        if (-not $state.MatchesCaptureUrl -or -not $state.BaseUrlPresent) { throw 'SelfTest: top-level URL was not detected.' }

        # 5: Single-quoted and double-quoted strings produce the same fingerprint.
        $singleQuote = Write-SampleConfig $root 'single-quote.toml' "openai_base_url = 'http://127.0.0.1:38441'"
        $stateSingle = Get-SafeConfigState $singleQuote 38441
        if ($stateSingle.BaseUrlFingerprint -ne $state.BaseUrlFingerprint) { throw 'SelfTest: quote styles produced different fingerprints.' }
        $sameValue = Get-StringSha256 'http://127.0.0.1:38441'
        if ($sameValue -ne $state.BaseUrlFingerprint) { throw 'SelfTest: URL value fingerprint was not normalized.' }

        # 6: Escape decode in one string: \" \\ \n \r \t decode, unknown \x is kept.
        $decoded = Convert-TomlBasicString '"' 'a\"b\\c\nd\re\tf\xg'
        $expected = 'a"' + 'b\' + 'c' + "`n" + 'd' + "`r" + 'e' + "`t" + 'f' + '\x' + 'g'
        if ($decoded -ne $expected) { throw 'SelfTest: basic-string escape decode failed.' }
        $literalDecoded = Convert-TomlBasicString "'" 'a\"b\\c'
        if ($literalDecoded -ne 'a\"b\\c') { throw 'SelfTest: literal string should not decode escapes.' }
        $escapedConfig = Write-SampleConfig $root 'escaped-url.toml' 'openai_base_url = "http://127.0.0.1:38441\tpath"'
        $escapedState = Get-SafeConfigState $escapedConfig 38441
        $expectedFingerprint = Get-StringSha256 ("http://127.0.0.1:38441" + "`t" + "path")
        if ($escapedState.BaseUrlFingerprint -ne $expectedFingerprint) { throw 'SelfTest: config-level escape decode failed.' }

        # 7: Result JSON is written to the temp directory and stays non-sensitive.
        $sentinelUrl = 'http://secret-host.invalid/oauth/token=abc123xyz'
        $sentinelConfig = Write-SampleConfig $root 'sentinel.toml' ('openai_base_url = "' + $sentinelUrl + '"')
        $sentinelState = Get-SafeConfigState $sentinelConfig 38441
        if ($sentinelState.BaseUrlFingerprint -notmatch '^[0-9A-Fa-f]{64}$') { throw 'SelfTest: sentinel fingerprint is not SHA-256.' }
        $recordDir = Join-Path $root 'records'
        $script:Checks.Clear()
        # Sentinel values go in the config body/URL only; ErrorCode stays a safe fixed code.
        [void](Add-Check 'selftest-secret' 'automatic' $true 'safe message' 'safe_fixed_code' @{ pid = 424242; gatewayPort = 38440; sha256 = $sentinelState.BaseUrlFingerprint })
        [void](Add-Check 'selftest-secret-fail' 'automatic' $false 'safe message' 'safe_fixed_code')
        $null = Write-SafeResultRecord 'selftest' (Get-Date) $sentinelConfig $recordDir
        $recordFile = Get-ChildItem -LiteralPath $recordDir -Filter 'traffic-analysis-selftest-*.json' | Sort-Object LastWriteTime -Descending | Select-Object -First 1
        if ($null -eq $recordFile) { throw 'SelfTest: result record was not written.' }
        $json = Get-Content -LiteralPath $recordFile.FullName -Raw
        foreach ($forbidden in @('secret-host', 'abc123xyz', 'http://', 'token')) {
            if ($json -match [regex]::Escape($forbidden)) { throw "SelfTest: result JSON leaked '$forbidden'." }
        }
        $parsed = $json | ConvertFrom-Json
        $checks = @($parsed.checks)
        if ([string]::IsNullOrWhiteSpace($checks[0].CheckedAt)) { throw 'SelfTest: CheckedAt is missing.' }
        if ($null -eq $checks[0].Metadata) { throw 'SelfTest: safe Metadata is missing.' }
        if ($checks[0].Metadata.gatewayPort -ne 38440) { throw 'SelfTest: Metadata gatewayPort was not preserved.' }
        if ($checks[0].Metadata.pid -ne 424242) { throw 'SelfTest: Metadata pid was not preserved.' }
        if ($checks[1].ErrorCode -ne 'safe_fixed_code') { throw 'SelfTest: safe ErrorCode was not preserved.' }
        $metaKeys = @($checks[0].Metadata.PSObject.Properties.Name)
        $disallowed = @($metaKeys | Where-Object { $_ -notin @('pid','port','gatewayPort','capturePort','sha256','configFileFingerprint','baseUrlFingerprint') })
        if ($disallowed.Count -gt 0) { throw 'SelfTest: unexpected Metadata key.' }

        # M4A marker decode and arbitrary-port matching still hold.
        $m4aConfig = Write-SampleConfig $root 'm4a.toml' @'
openai_base_url = "http://127.0.0.1:38441"

[moon_bridge_manual_test]
m4a_marker = "preserve-me"
'@
        $marker = Get-TomlTableString (Get-Content -LiteralPath $m4aConfig -Raw) 'moon_bridge_manual_test' 'm4a_marker'
        if (-not $marker.Present -or $marker.Value -ne 'preserve-me') { throw 'SelfTest: M4A marker was not decoded.' }
        $wrongPort = Get-SafeConfigState $m4aConfig 38442
        if ($wrongPort.MatchesCaptureUrl) { throw 'SelfTest: port parameter was ignored.' }

        # Command journal aggregation: session-limited, read-race aware, non-sensitive.
        $journalDir = Join-Path $root 'journal'
        New-Item -ItemType Directory -Force -Path $journalDir | Out-Null
        $journalPath = Join-Path $journalDir 'command-journal.jsonl'
        $startedUtc = [datetime]::UtcNow
        function New-JournalLine([string]$Session, [int]$Id, [string]$Command, [string]$Event, [string]$Result, [datetime]$When) {
            $sessionJson = if ($Session) { '"' + $Session + '"' } else { 'null' }
            $resultJson = if ($Result) { '"' + $Result + '"' } else { 'null' }
            return '{"schema":1,"t":"' + $When.ToUniversalTime().ToString('o') + '","session_id":' + $sessionJson + ',"invocation_id":' + $Id + ',"kind":"command","command":"' + $Command + '","event":"' + $Event + '","result":' + $resultJson + '}'
        }
        $sentinel = 'secret-host.invalid-abc123xyz'
        $pastSession = 'past-session'
        $session = 'current-session'
        $lines = @(
            (New-JournalLine $pastSession 1 'traffic_analysis_start' 'end' 'ok' $startedUtc.AddMinutes(-5)),
            ('{"schema":1,"t":"' + $startedUtc.AddMinutes(-10).ToString('o') + '","session_id":"older","invocation_id":0,"kind":"command","command":"traffic_analysis_start","event":"end","result":"ok","note":"' + $sentinel + '"}'),
            (New-JournalLine $null 2 'traffic_analysis_start' 'invoke' $null $startedUtc.AddSeconds(1)),
            (New-JournalLine $null 2 'traffic_analysis_start' 'acquired' $null $startedUtc.AddSeconds(1)),
            (New-JournalLine $session 2 'traffic_analysis_start' 'end' 'ok' $startedUtc.AddSeconds(1)),
            (New-JournalLine $session 3 'traffic_analysis_stop' 'invoke' $null $startedUtc.AddSeconds(2)),
            (New-JournalLine $session 3 'traffic_analysis_stop' 'acquired' $null $startedUtc.AddSeconds(2)),
            (New-JournalLine $session 3 'traffic_analysis_stop' 'end' 'ok' $startedUtc.AddSeconds(3))
        )
        Set-Content -LiteralPath $journalPath -Value $lines -Encoding utf8
        $journal = Get-CurrentSessionJournal $startedUtc $null @($journalPath)
        if ($journal.BlockedReason) { throw "SelfTest: journal blocked unexpectedly: $($journal.BlockedReason)" }
        if ($journal.SessionId -ne $session) { throw 'SelfTest: past session was adopted instead of the current one.' }
        if ($journal.StopInvoke -ne 1 -or $journal.StopAcquired -ne 1 -or $journal.StopEnd -ne 1 -or $journal.StopEndOk -ne 1) { throw 'SelfTest: stop journal counts are wrong.' }
        if ($journal.StopCompleteTriples -ne 1 -or $journal.StopPartialTriples -ne 0) { throw 'SelfTest: stop invocation triples are wrong.' }
        if ($journal.FinishInvoke -ne 0 -or $journal.FinishAcquired -ne 0 -or $journal.FinishEnd -ne 0) { throw 'SelfTest: finish_relay records leaked into the stop-only session.' }
        if ((Test-StopJournal $journal) -ne 'PASS') { throw 'SelfTest: stop-only session was not judged PASS.' }
        if ((Test-FinishRelayAbsent $journal) -ne 'PASS') { throw 'SelfTest: absent finish_relay was not judged PASS.' }
        if ((Test-FinishRelayComplete $journal) -ne 'BLOCKED') { throw 'SelfTest: missing finish_relay should be BLOCKED.' }
        $journalJson = $journal | ConvertTo-Json -Compress
        foreach ($forbidden in @('secret-host', 'abc123xyz')) {
            if ($journalJson -match [regex]::Escape($forbidden)) { throw "SelfTest: journal aggregation leaked '$forbidden'." }
        }
        # Early finish_relay invoke must be FAIL and counted as a partial triple.
        Add-Content -LiteralPath $journalPath -Value (New-JournalLine $session 8 'traffic_analysis_finish_relay' 'invoke' $null $startedUtc.AddSeconds(4)) -Encoding utf8
        $early = Get-CurrentSessionJournal $startedUtc $null @($journalPath)
        if ((Test-FinishRelayAbsent $early) -ne 'FAIL') { throw 'SelfTest: early finish_relay was not judged FAIL.' }
        if ($early.FinishPartialTriples -ne 1 -or $early.FinishCompleteTriples -ne 0) { throw 'SelfTest: early finish_relay partial triple was not counted.' }
        # Complete finish_relay must be PASS; all three events share id 8.
        Add-Content -LiteralPath $journalPath -Value (New-JournalLine $session 8 'traffic_analysis_finish_relay' 'acquired' $null $startedUtc.AddSeconds(5)) -Encoding utf8
        Add-Content -LiteralPath $journalPath -Value (New-JournalLine $session 8 'traffic_analysis_finish_relay' 'end' 'ok' $startedUtc.AddSeconds(5)) -Encoding utf8
        $complete = Get-CurrentSessionJournal $startedUtc $null @($journalPath)
        if ((Test-FinishRelayComplete $complete) -ne 'PASS') { throw 'SelfTest: complete finish_relay was not judged PASS.' }
        if ($complete.FinishCompleteTriples -ne 1 -or $complete.FinishPartialTriples -ne 0) { throw 'SelfTest: complete finish_relay triple counts are wrong.' }
        # Two incomplete stop calls (double press) must be BLOCKED, one partial
        # triple per call instead of a single ambiguous count.
        $doublePath = Join-Path $journalDir 'double.jsonl'
        $doubleLines = @(
            (New-JournalLine $session 4 'traffic_analysis_start' 'end' 'ok' $startedUtc.AddSeconds(1)),
            (New-JournalLine $session 5 'traffic_analysis_stop' 'invoke' $null $startedUtc.AddSeconds(2)),
            (New-JournalLine $session 6 'traffic_analysis_stop' 'invoke' $null $startedUtc.AddSeconds(2))
        )
        Set-Content -LiteralPath $doublePath -Value $doubleLines -Encoding utf8
        $double = Get-CurrentSessionJournal $startedUtc $null @($doublePath)
        if ((Test-StopJournal $double) -ne 'BLOCKED') { throw 'SelfTest: two incomplete stop calls were not BLOCKED.' }
        if ($double.StopCompleteTriples -ne 0 -or $double.StopPartialTriples -ne 2) { throw 'SelfTest: double-press partial triples were not counted.' }
        # Recovery session mismatch must be BLOCKED, matching id must not be.
        $recoveryDir = Join-Path $root 'recovery'
        New-Item -ItemType Directory -Force -Path $recoveryDir | Out-Null
        $recoveryPath = Join-Path $recoveryDir 'recovery-state-v2.json'
        Set-Content -LiteralPath $recoveryPath -Value '{"schemaVersion":2,"autoLog":{"sessionId":"different-session"}}' -Encoding utf8
        $mismatch = Get-CurrentSessionJournal $startedUtc $recoveryPath @($journalPath)
        if ($mismatch.BlockedReason -ne 'journal_session_mismatch') { throw 'SelfTest: recovery session mismatch was not BLOCKED.' }
        Set-Content -LiteralPath $recoveryPath -Value ('{"schemaVersion":2,"autoLog":{"sessionId":"' + $session + '"}}') -Encoding utf8
        $match = Get-CurrentSessionJournal $startedUtc $recoveryPath @($journalPath)
        if ($match.BlockedReason) { throw 'SelfTest: matching recovery session should not be blocked.' }
        # Missing journal must be BLOCKED.
        $missing = Get-CurrentSessionJournal $startedUtc $null @((Join-Path $root 'no-journal.jsonl'))
        if ($missing.BlockedReason -ne 'journal_missing') { throw 'SelfTest: missing journal was not BLOCKED.' }
        # A partial final line retried and still unreadable must be BLOCKED.
        $partialPath = Join-Path $journalDir 'partial.jsonl'
        Set-Content -LiteralPath $partialPath -Value ($lines[0]) -Encoding utf8
        Add-Content -LiteralPath $partialPath -Value '{"schema":1,"t":"2026-08-04T00:00:00Z","session_id":"partial","invocation_id":99,"kind":"command","command":"traffic_analysis_start","event":"end","result":"ok","unterminated' -Encoding utf8
        $partial = Get-CurrentSessionJournal $startedUtc $null @($partialPath)
        if ($partial.BlockedReason -ne 'journal_incomplete_tail') { throw 'SelfTest: unterminated final line was not BLOCKED.' }

        $script:Checks.Clear()
        [void]$script:Checks.Add([pscustomobject]@{ Id = 'selftest-fail'; Kind = 'automatic'; Result = 'FAIL'; ErrorCode = 'test_failure' })
        [void]$script:Checks.Add([pscustomobject]@{ Id = 'selftest-blocked'; Kind = 'manual'; Result = 'BLOCKED'; ErrorCode = 'test_blocked' })
        if ((Get-FinalResult) -ne 'FAIL') { throw 'SelfTest: FAIL did not take precedence.' }
        $script:Checks.Clear()
        [void]$script:Checks.Add([pscustomobject]@{ Id = 'selftest-blocked'; Kind = 'manual'; Result = 'BLOCKED'; ErrorCode = 'test_blocked' })
        [void]$script:Checks.Add([pscustomobject]@{ Id = 'selftest-pass'; Kind = 'automatic'; Result = 'PASS'; ErrorCode = $null })
        if ((Get-FinalResult) -ne 'BLOCKED') { throw 'SelfTest: BLOCKED did not take precedence over PASS.' }
        $script:Checks.Clear()
        [void]$script:Checks.Add([pscustomobject]@{ Id = 'selftest'; Kind = 'automatic'; Result = 'PASS'; ErrorCode = $null })
        Write-Host 'PASS  SelfTest completed' -ForegroundColor Green
    } finally {
        if (Test-Path -LiteralPath $root) { Remove-Item -LiteralPath $root -Recurse -Force }
    }
}

$script:Checks = [System.Collections.Generic.List[object]]::new()

if ($SelfTest) {
    Invoke-SelfTest
    exit 0
}
if ([string]::IsNullOrWhiteSpace($Case)) {
    throw '-Case is required unless -SelfTest is specified.'
}

$configPath = Resolve-CodexConfigPath
$recoveryPath = Resolve-RecoveryPath
$startedAt = Get-Date
$before = Get-SafeConfigState $configPath $CapturePort

Write-Host "Moon Bridge Desktop manual test $Case"
Write-Host 'Sensitive values, cookies, prompts, and payloads are not displayed or saved.'

if ($Case -eq 'M1A') {
        Add-ManualCheck 'm1a_preflight' "Confirm Codex Desktop, Gateway, and external editor autosave are stopped."
        Add-ManualCheck 'm1a_started' "Start Traffic Analysis with Codex Desktop closed and zero observations."
        [void](Add-Check 'gateway_listening_after_start' 'automatic' (Test-ListeningPort $GatewayPort) "Gateway port $GatewayPort is listening")
        [void](Add-Check 'capture_listening_after_start' 'automatic' (Test-ListeningPort $CapturePort) "Capture port $CapturePort is listening")
        [void](Add-Check 'capture_url_after_start' 'automatic' ((Get-SafeConfigState $configPath $CapturePort).MatchesCaptureUrl) 'Top-level openai_base_url points to the requested Capture port')
        Add-ManualCheck 'm1a_zero_observations' "Confirm the UI shows capturing with zero connections and observations."
        Add-ManualCheck 'm1a_pause' "Press Stop Analysis and confirm the UI shows passthrough."
        [void](Add-Check 'capture_listening_after_pause' 'automatic' (Test-ListeningPort $CapturePort) "Capture port $CapturePort remains listening after pause")
        $afterStop = Get-SafeConfigState $configPath $CapturePort
        [void](Add-Check 'config_full_fingerprint_restored' 'automatic' ($afterStop.ConfigFileFingerprint -eq $before.ConfigFileFingerprint) 'Config file fingerprint exactly matches the pre-analysis state')
        Add-ManualCheck 'm1a_passthrough' "Confirm passthrough remains active and new observations are not recorded."
        Add-ManualCheck 'm1a_finish_relay' "Finish the relay and confirm the Capture port closes."
        [void](Add-Check 'capture_closed_after_finish' 'automatic' (-not (Test-ListeningPort $CapturePort)) "Capture port $CapturePort is closed after relay finish")
        [void](Add-Check 'gateway_retained_after_relay_finish' 'automatic' (Test-ListeningPort $GatewayPort) "Gateway port $GatewayPort remains listening after relay finish")
} elseif ($Case -eq 'M1B') {
        Add-ManualCheck 'm1b_started' "Start Traffic Analysis and send a short non-sensitive Codex request."
        Add-ManualCheck 'm1b_observation' "Confirm a sanitized observation is visible in the UI."
        Add-ManualCheck 'm1b_pause' @'
Press Stop Analysis once.
After it succeeds, the primary button changes to Finish Relay.
Do not press it again until the explicit Finish Relay step.
'@
        [void](Add-Check 'capture_listening_after_pause' 'automatic' (Test-ListeningPort $CapturePort) "Capture port $CapturePort remains listening after pause")
        $afterStop = Get-SafeConfigState $configPath $CapturePort
        [void](Add-Check 'managed_url_restored' 'automatic' ($afterStop.BaseUrlPresent -eq $before.BaseUrlPresent -and $afterStop.BaseUrlFingerprint -eq $before.BaseUrlFingerprint) 'Managed openai_base_url value returned to its previous value')
        $journal = Get-CurrentSessionJournal $startedAt $recoveryPath
        $stopResult = Test-StopJournal $journal
        $finishEarlyResult = Test-FinishRelayAbsent $journal
        [void](Add-ResultCheck 'journal_session_adopted' $(if ($journal.Adopted -and -not $journal.BlockedReason) { 'PASS' } else { 'BLOCKED' }) "journal session adopted (id matches recovery autosave)" $(if ($journal.BlockedReason) { $journal.BlockedReason } else { 'manual_check_failed' }))
        [void](Add-ResultCheck 'stop_journal' $stopResult ("stop journal: invoke=$($journal.StopInvoke)/acquired=$($journal.StopAcquired)/end=$($journal.StopEnd)(ok=$($journal.StopEndOk)) complete-triples=$($journal.StopCompleteTriples) partial-triples=$($journal.StopPartialTriples)") $(if ($stopResult -eq 'FAIL') { 'stop_ended_with_error' } elseif ($journal.BlockedReason) { $journal.BlockedReason } else { 'manual_check_failed' }))
        [void](Add-ResultCheck 'finish_relay_not_early' $finishEarlyResult ("finish_relay invoked before passthrough confirmation: $($journal.FinishInvoke)") 'journal_finish_relay_early')
        Add-ManualCheck 'm1b_passthrough_request' "After stopping analysis, send a new short Codex turn and confirm no stream disconnect or reconnect failure."
        Add-ManualCheck 'm1b_finish_relay' "Finish the relay and confirm the Capture port closes."
        [void](Add-Check 'capture_closed_after_finish' 'automatic' (-not (Test-ListeningPort $CapturePort)) "Capture port $CapturePort is closed after relay finish")
        $journalAfterFinish = Get-CurrentSessionJournal $startedAt $recoveryPath
        $finishResult = Test-FinishRelayComplete $journalAfterFinish
        [void](Add-ResultCheck 'finish_relay_journal' $finishResult ("finish_relay journal: invoke=$($journalAfterFinish.FinishInvoke)/acquired=$($journalAfterFinish.FinishAcquired)/end=$($journalAfterFinish.FinishEnd)(ok=$($journalAfterFinish.FinishEndOk)) complete-triples=$($journalAfterFinish.FinishCompleteTriples) partial-triples=$($journalAfterFinish.FinishPartialTriples)") $(if ($finishResult -eq 'FAIL') { 'finish_relay_ended_with_error' } elseif ($journalAfterFinish.BlockedReason) { $journalAfterFinish.BlockedReason } else { 'manual_check_failed' }))
        Add-ManualCheck 'm1b_restart_request' "Fully restart Codex Desktop and confirm a normal connection succeeds."
} elseif ($Case -eq 'M2') {
        if ($GatewayPid -le 0) {
            [void](Add-Check 'gateway_pid_argument' 'automatic' $false 'M2 requires -GatewayPid' 'missing_gateway_pid')
        } else {
        $info = Get-SafeProcessInfo $GatewayPid
        $owners = @(Get-ListeningPids $GatewayPort)
        $nameOk = $info.Name -match '^moonbridge$' -or ($info.Path -and [IO.Path]::GetFileNameWithoutExtension($info.Path) -eq 'moonbridge')
        $pathOk = [string]::IsNullOrWhiteSpace($ExpectedSidecarPath) -or ($info.Path -and [IO.Path]::GetFullPath($info.Path) -eq [IO.Path]::GetFullPath($ExpectedSidecarPath))
        [void](Add-Check 'verified_gateway_process' 'automatic' $nameOk "PID $GatewayPid is moonbridge" '' @{ pid = $GatewayPid; gatewayPort = $GatewayPort })
        [void](Add-Check 'gateway_port_owner' 'automatic' ($owners -contains $GatewayPid) "PID $GatewayPid owns Gateway port $GatewayPort")
        [void](Add-Check 'expected_sidecar_path' 'automatic' $pathOk 'Gateway executable path matches the expected sidecar path')
        Add-ManualCheck 'm2_ready_to_kill' "Recheck the PID and Gateway-port ownership before force termination."
        $ownersBeforeKill = @(Get-ListeningPids $GatewayPort)
        $infoBeforeKill = Get-SafeProcessInfo $GatewayPid
        $nameStillMatches = $infoBeforeKill.Name -match '^moonbridge$' -or ($infoBeforeKill.Path -and [IO.Path]::GetFileNameWithoutExtension($infoBeforeKill.Path) -eq 'moonbridge')
        $ownershipStillMatches = $ownersBeforeKill -contains $GatewayPid
        $pathStillMatches = [string]::IsNullOrWhiteSpace($ExpectedSidecarPath) -or ($infoBeforeKill.Path -and [IO.Path]::GetFullPath($infoBeforeKill.Path) -eq [IO.Path]::GetFullPath($ExpectedSidecarPath))
        $killOk = $ForceKillGateway -and $nameStillMatches -and $ownershipStillMatches -and $pathStillMatches
        if ($killOk) {
            Stop-Process -Id $GatewayPid -Force
            Start-Sleep -Seconds 2
            [void](Add-Check 'gateway_stopped' 'automatic' (-not (Test-ListeningPort $GatewayPort)) "Gateway port $GatewayPort is closed after the verified kill")
        } elseif ($ForceKillGateway) {
            [void](Add-Check 'm2_ownership_changed' 'automatic' $false 'Refusing to kill: name, path, or port ownership changed before termination' 'ownership_changed_before_kill')
        } else {
            Add-ManualCheck 'm2_force_kill_skipped' "Choose BLOCKED if the force kill was skipped."
        }
        Add-ManualCheck 'm2_desktop_error' "Confirm Desktop shows a safe error and does not auto-restart Gateway."
        Add-ManualCheck 'm2_recovery' "Restart Desktop and confirm recovery is shown without automatic config reapply."
        }
} elseif ($Case -eq 'M3') {
        Add-ManualCheck 'm3_capture_active' 'Start Traffic Analysis and confirm the Capture URL is applied.'
        $beforeCrash = Get-SafeConfigState $configPath $CapturePort
        [void](Add-Check 'm3_capture_url_applied_before_crash' 'automatic' $beforeCrash.MatchesCaptureUrl 'Capture URL is applied before terminating Desktop')
        Add-ManualCheck 'm3_desktop_terminated' 'Terminate only Moon Bridge Desktop, then restart it.'
        Add-ManualCheck 'm3_restarted' "Restart Desktop and confirm the recovery card is shown."
        $afterRestart = Get-SafeConfigState $configPath $CapturePort
        [void](Add-Check 'm3_config_unchanged' 'automatic' ($afterRestart.ConfigFileFingerprint -eq $beforeCrash.ConfigFileFingerprint) 'Config was not silently rewritten after Desktop restart')
        [void](Add-Check 'm3_capture_not_auto_started' 'automatic' (-not (Test-ListeningPort $CapturePort)) 'Capture did not start automatically')
        Add-ManualCheck 'm3_config_not_rewritten' "Confirm Desktop restart did not rewrite or change the config file; recovery requires explicit user action."
        Add-ManualCheck 'm3_explicit_restore' "Explicitly restore the config and confirm normal connectivity returns."
} elseif ($Case -eq 'M4A') {
        Add-ManualCheck 'm4a_preflight' 'Run this case before starting Traffic Analysis, then start analysis and add the marker.'
        Add-ManualCheck 'm4a_external_marker' "Without changing openai_base_url, add the M4A marker table and restart Desktop."
        $recovery = Get-SafeRecoveryState $recoveryPath
        [void](Add-Check 'm4a_conflict_detected' 'automatic' ($null -ne $recovery -and $recovery.ReconciliationStatus -eq 'config_conflict') 'reconciliationStatus=config_conflict')
        Add-ManualCheck 'm4a_no_change_before_confirmation' "Confirm config_conflict is shown and the file is unchanged before confirmation."
        Add-ManualCheck 'm4a_confirm_restore' "Confirm restore and verify that only openai_base_url is restored."
        $text = ''
        if (Test-Path -LiteralPath $configPath -PathType Leaf) {
            $text = Get-Content -LiteralPath $configPath -Raw
        }
        $marker = Get-TomlTableString $text 'moon_bridge_manual_test' 'm4a_marker'
        [void](Add-Check 'm4a_marker_preserved' 'automatic' ($marker.Present -and $marker.Value -eq 'preserve-me') 'M4A marker value remains after confirmed restore')
        $afterRestore = Get-SafeConfigState $configPath $CapturePort
        $urlRestored = $afterRestore.BaseUrlPresent -eq $before.BaseUrlPresent -and $afterRestore.BaseUrlFingerprint -eq $before.BaseUrlFingerprint
        [void](Add-Check 'managed_url_restored' 'automatic' $urlRestored 'Managed openai_base_url returned to the pre-analysis state')
        Add-ManualCheck 'm4a_toml_valid' "Confirm TOML reloads and normal connectivity returns after restore."
        Add-ManualCheck 'm4a_cleanup' "After recording PASS, manually remove the M4A test table."
} elseif ($Case -eq 'M4B') {
        [void](Add-Check 'm4b_test_port_unused' 'automatic' (-not (Test-ListeningPort 65534)) 'The local M4B test port is not listening')
        Add-ManualCheck 'm4b_external_url' "With Codex Desktop closed, set openai_base_url to http://127.0.0.1:65534 and restart Desktop."
        $recovery = Get-SafeRecoveryState $recoveryPath
        [void](Add-Check 'm4b_conflict_detected' 'automatic' ($null -ne $recovery -and $recovery.ReconciliationStatus -eq 'config_conflict') 'reconciliationStatus=config_conflict')
        Add-ManualCheck 'm4b_no_overwrite_before_confirmation' "Confirm the external URL is not overwritten before confirmation."
        Add-ManualCheck 'm4b_confirm_restore' "Explicitly restore and confirm the pre-analysis openai_base_url returns."
        $afterRestore = Get-SafeConfigState $configPath $CapturePort
        $urlRestored = $afterRestore.BaseUrlPresent -eq $before.BaseUrlPresent -and $afterRestore.BaseUrlFingerprint -eq $before.BaseUrlFingerprint
        [void](Add-Check 'managed_url_restored' 'automatic' $urlRestored 'Managed openai_base_url returned to the pre-analysis state')
} elseif ($Case -eq 'M5') {
        $ownersBeforeExit = @(Get-ListeningPids $GatewayPort)
        [void](Add-Check 'm5_gateway_owner_before_exit' 'automatic' ($ownersBeforeExit.Count -eq 1) "Exactly one PID owns Gateway port $GatewayPort before exit")
        Add-ManualCheck 'm5_normal_exit' "Exit Desktop normally while Gateway is running."
        Start-Sleep -Seconds 1
        [void](Add-Check 'old_gateway_exited' 'automatic' (-not (Test-ListeningPort $GatewayPort)) "Gateway port $GatewayPort is closed after Desktop exit")
        $oldPid = if ($ownersBeforeExit.Count -eq 1) { $ownersBeforeExit[0] } else { $null }
        $oldPidGone = $null -eq $oldPid -or (-not (Test-ProcessExists $oldPid))
        [void](Add-Check 'old_gateway_pid_gone' 'automatic' $oldPidGone "Previous gateway PID $oldPid is gone after Desktop exit")
        Add-ManualCheck 'm5_no_old_sidecar' "Confirm no old sidecar is orphaned and no legacy launcher or dedicated CODEX_HOME is used."
        Add-ManualCheck 'm5_restart' "Restart Desktop, start Gateway, and confirm a new instance ID and Desktop mode in UI or logs."
        Start-Sleep -Seconds 2
        $newOwners = @(Get-ListeningPids $GatewayPort)
        [void](Add-Check 'new_gateway_listening' 'automatic' (Test-ListeningPort $GatewayPort) "Gateway port $GatewayPort is listening after restart")
        [void](Add-Check 'new_gateway_owner_count' 'automatic' ($newOwners.Count -eq 1) "Exactly one PID owns Gateway port $GatewayPort after restart")
        if ($newOwners.Count -eq 1) {
            $newPid = $newOwners[0]
            $newPidDiffers = $newPid -ne $oldPid
            [void](Add-Check 'new_gateway_pid_differs' 'automatic' $newPidDiffers "New gateway PID $newPid differs from pre-exit PID $oldPid")
        }
}
$null = Write-SafeResultRecord $Case $startedAt $configPath
