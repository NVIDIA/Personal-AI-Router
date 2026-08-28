# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Wipe all PAIR-owned application data (clean-only; does not relaunch the app).
#
# Runnable from anywhere:
#   .\scripts\wipe-app-data.cmd --dry-run
#   .\scripts\wipe-app-data.ps1 --confirm
#
# No Node required. The desktop app spawns the .cmd wrapper after shutting down
# the service tree.
#
# ---------------------------------------------------------------------------
# APPEND-ONLY wipe inventory
#
# This list GROWS and is NEVER cut down. When storage moves (e.g. version 1
# writes under \a\b\c and version 2 under \a\b\d), BOTH paths stay listed so
# upgrades from any prior release remain recoverable.
#
# Keep names in sync with:
#   - desktop/src/shared/constants/app.ts
#   - services/shared/appdir/appdir.go
#   - desktop/scripts/build/{installer.nsh,linux/after-remove.sh,macos/uninstall.sh}
#   - scripts/wipe-app-data.sh (Unix twin — update both in the same change)
#
# Explicit exclusions (never add): %USERPROFILE%\.ollama, .lmstudio, external
# engine installs, and the application install tree (Program Files\PAIR).
# ---------------------------------------------------------------------------

[CmdletBinding()]
param(
    [Parameter(ValueFromRemainingArguments = $true)]
    [string[]]$ArgsList
)

$ErrorActionPreference = 'Continue'
$ConfirmPhrase = 'wipe'
$DryRun = $false
$Confirmed = $false
$ForceKill = $false
$ShowHelp = $false
$WaitPid = 0
$RelaunchExec = ''
$WaitTimeoutSeconds = 60

function Show-Usage {
    @"
Usage: scripts\wipe-app-data.cmd [options]
       scripts\wipe-app-data.ps1 [options]

Delete all Personal AI Router-owned application data (settings, logs, cluster
identity, chat history, PAIR-managed engines under the app data root).

Does NOT delete third-party model libraries (e.g. %USERPROFILE%\.ollama).
Does NOT uninstall the application binary.

Options:
  --dry-run          List paths that would be deleted; no confirmation required
  --confirm          Skip interactive prompt (required for non-interactive / app use)
  --force-kill       Stop PAIR processes before wiping
  --wait-pid=<pid>   Wait for that process to exit before wiping (app-invoked)
  --wait-timeout=<s> Give up waiting after this many seconds (default $WaitTimeoutSeconds)
  --relaunch=<path>  Start this executable after a successful wipe (app-invoked)
  --help             Show this help

Interactive mode requires typing "$ConfirmPhrase" to proceed.
Quit Personal AI Router before running unless --force-kill is set.
"@
}

foreach ($arg in @($ArgsList)) {
    switch -Regex ($arg) {
        '^--dry-run$' { $DryRun = $true }
        '^--confirm$|^--yes$' { $Confirmed = $true }
        '^--force-kill$' { $ForceKill = $true }
        '^--wait-pid=(\d+)$' { $WaitPid = [int]$matches[1] }
        '^--wait-timeout=(\d+)$' { $WaitTimeoutSeconds = [int]$matches[1] }
        '^--relaunch=(.+)$' { $RelaunchExec = $matches[1] }
        '^--help$|^-h$' { $ShowHelp = $true }
        default {
            Write-Error "Unknown option: $arg"
            Show-Usage | Write-Host
            exit 2
        }
    }
}

if ($ShowHelp) {
    Show-Usage | Write-Host
    exit 0
}

$AppId = 'com.nvidia.nvpair'
$AppOrg = 'Nvidia Corporation'
$AppDataDirName = 'Personal AI Router'
$PreviousOrg = 'NVIDIA Corporation'
$PreviousDataDirName = 'PAIR'

$LocalAppData = $env:LOCALAPPDATA
if (-not $LocalAppData) {
    Write-Error 'LOCALAPPDATA is unset'
    exit 1
}

$HomeDir = $env:USERPROFILE
if (-not $HomeDir) { $HomeDir = $HOME }

$CurrentRoot = Join-Path $LocalAppData (Join-Path $AppOrg $AppDataDirName)
$LegacyRoot = Join-Path $LocalAppData (Join-Path $PreviousOrg $PreviousDataDirName)

$Username = $env:USERNAME
if (-not $Username) { $Username = 'user' }
$sha = [System.Security.Cryptography.SHA256]::Create()
try {
    $bytes = [System.Text.Encoding]::UTF8.GetBytes("${AppId}:${Username}")
    $hash = ($sha.ComputeHash($bytes) | ForEach-Object { $_.ToString('x2') }) -join ''
    $Scope = $hash.Substring(0, 16)
} finally {
    $sha.Dispose()
}
$Tmp = $env:TEMP
if (-not $Tmp) { $Tmp = $env:TMP }
$ControlDir = Join-Path $Tmp "nvpair-$Scope"

# Append-only target list. Never remove entries — only append.
$Targets = @(
    @{ Path = $CurrentRoot; Reason = 'Current shared Electron + backend app data root' },
    @{ Path = $LegacyRoot; Reason = 'Legacy pre-rename app data root (NVIDIA Corporation/PAIR)' },
    @{ Path = (Join-Path $LocalAppData 'nvpair-updater'); Reason = 'electron-updater download cache (Windows)' },
    @{ Path = (Join-Path $CurrentRoot (Join-Path 'bin' 'nvpair.cmd')); Reason = 'Generated Windows nvpair launcher under userData' },
    @{ Path = $ControlDir; Reason = 'Removed CLI control token directory' }
)

$PairProcs = @(
    'PAIR',
    'nvpair-ui-broker',
    'ollama-proxy',
    'lmstudio-proxy',
    'nvpair-node-info',
    'nvpair-node-scanner',
    'nvpair-manual-nodes',
    'nvpair-node-settings',
    'nvpair-engine-manager',
    'nvpair-workload-manager',
    'nvpair-cluster-manager',
    'nvpair-job-scheduler',
    'nvpair-errors',
    'nvpair-tui'
)

function Test-PairProcessRunning([string]$Name) {
    $image = if ($Name -eq 'PAIR') { 'PAIR.exe' } else { "$Name.exe" }
    try {
        $out = & tasklist /FI "IMAGENAME eq $image" /NH 2>$null | Out-String
        return ($out -match [regex]::Escape($image))
    } catch {
        return $false
    }
}

if ($DryRun) {
    Write-Host "[dry-run] Would remove $($Targets.Count) path(s):"
    foreach ($t in $Targets) {
        Write-Host ("  {0}  ({1})" -f $t.Path, $t.Reason)
    }
    exit 0
}

# The app spawns this script detached and then exits, so the wipe must happen
# after its process is gone. Otherwise Chromium flushes session/cache files back
# into the directory we just deleted and the "clean" relaunch is not clean.
if ($WaitPid -gt 0) {
    $waited = 0
    while ($true) {
        $alive = $null -ne (Get-Process -Id $WaitPid -ErrorAction SilentlyContinue)
        if (-not $alive) { break }
        if ($waited -ge $WaitTimeoutSeconds) {
            Write-Error ("process {0} did not exit within {1}s; aborting wipe" -f $WaitPid, $WaitTimeoutSeconds)
            exit 1
        }
        Start-Sleep -Seconds 1
        $waited++
    }
}

$running = @()
foreach ($proc in $PairProcs) {
    if (Test-PairProcessRunning $proc) {
        $image = if ($proc -eq 'PAIR') { 'PAIR.exe' } else { "$proc.exe" }
        $running += $image
    }
}

if ($running.Count -gt 0 -and -not $ForceKill) {
    Write-Error ("Personal AI Router appears to be running ({0}). Quit the app first, or pass --force-kill." -f ($running -join ', '))
    exit 1
}

if ($running.Count -gt 0 -and $ForceKill) {
    Write-Host 'Stopping PAIR processes (--force-kill)...'
    $killScript = @'
Get-CimInstance Win32_Process | Where-Object {
  $_.ExecutablePath -and (
    $_.ExecutablePath -like "$env:LOCALAPPDATA\NVIDIA Corporation\PAIR\*" -or
    $_.ExecutablePath -like "$env:LOCALAPPDATA\Nvidia Corporation\Personal AI Router\*"
  )
} | ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }
'@
    try {
        powershell -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command $killScript | Out-Null
    } catch { }
    foreach ($proc in $PairProcs) {
        $image = if ($proc -eq 'PAIR') { 'PAIR.exe' } else { "$proc.exe" }
        try { & taskkill /F /IM $image 2>$null | Out-Null } catch { }
    }
    Start-Sleep -Seconds 1
}

if (-not $Confirmed) {
    if (-not [Environment]::UserInteractive) {
        Write-Error 'Refusing to wipe without confirmation. Pass --confirm for non-interactive use, or run interactively.'
        exit 2
    }
    Write-Host ''
    Write-Host 'WARNING: This permanently deletes all Personal AI Router app data.'
    Write-Host 'Third-party model libraries (e.g. %USERPROFILE%\.ollama) are NOT removed.'
    Write-Host ''
    Write-Host 'Paths to remove:'
    foreach ($t in $Targets) { Write-Host ("  - {0}" -f $t.Path) }
    Write-Host ''
    Write-Host ("Type `"{0}`" to confirm:" -f $ConfirmPhrase)
    $answer = Read-Host '>'
    if ($answer -ne $ConfirmPhrase) {
        Write-Host 'Aborted.'
        exit 130
    }
}

Write-Host 'Removing app data...'
$removed = 0
$failures = 0
foreach ($t in $Targets) {
    $p = $t.Path
    if (-not (Test-Path -LiteralPath $p)) { continue }
    try {
        Remove-Item -LiteralPath $p -Recurse -Force -ErrorAction Stop
        Write-Host ("  removed {0}" -f $p)
        $removed++
    } catch {
        Write-Host ("  FAILED  {0}: {1}" -f $p, $_.Exception.Message)
        $failures++
    }
}

foreach ($org in @($AppOrg, $PreviousOrg)) {
    $parent = Join-Path $LocalAppData $org
    if (Test-Path -LiteralPath $parent) {
        $kids = @(Get-ChildItem -LiteralPath $parent -Force -ErrorAction SilentlyContinue)
        if ($kids.Count -eq 0) {
            try { Remove-Item -LiteralPath $parent -Force -ErrorAction SilentlyContinue } catch { }
        }
    }
}

Write-Host ("Removed {0} path(s)." -f $removed)
if ($failures -gt 0) {
    Write-Error ("{0} path(s) could not be removed." -f $failures)
    exit 1
}

# Relaunch only after the wipe succeeded, so the new process creates the fresh
# first-run state instead of racing the deletes.
if ($RelaunchExec) {
    if (Test-Path -LiteralPath $RelaunchExec) {
        Write-Host ("Relaunching {0}" -f $RelaunchExec)
        Start-Process -FilePath $RelaunchExec | Out-Null
        exit 0
    }
    Write-Error ("relaunch target not found: {0}" -f $RelaunchExec)
    exit 1
}

Write-Host 'Done. Restart Personal AI Router to begin with a clean state.'
exit 0
