# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Collect Personal AI Router logs and rewrite the identifiers in them so the
# result can be shared.
#
# Runnable from anywhere:
#   .\scripts\collect-logs.ps1
#   .\scripts\collect-logs.ps1 -in C:\path\to\nvpair-logs-2026-07-21.txt -out .\bundle
#   .\scripts\collect-logs.ps1 -raw          # local debugging, NOT shareable
#   .\scripts\collect-logs.ps1 -h
#
# With no arguments the local log directory for the current user is read and a
# sanitized bundle is written to .\pair-logs-<timestamp>.
#
# A packaged install ships a compiled collectlogs.exe beside this script, and
# that is used when present. Running from a source checkout there is no such
# binary, so the collector is built on demand and go is required — the same
# toolchain services\build.bat already needs. No Node either way.
#
# The logic lives in scripts\collectlogs (Go) rather than in this script.
# Identifiers arrive at several JSON escape depths, so replacing them correctly
# means decoding, substituting, and re-encoding; text substitution over the raw
# file would need a separate pattern per depth and would miss cases. Keeping one
# implementation also keeps this script and its Unix twin from drifting.
#
# Keep in sync with:
#   - scripts/collect-logs.sh (Unix twin — update both in the same change)
#   - desktop/src/shared/constants/app.ts (app data directory names)
#   - desktop/src/shared/utils/log.ts (log directory and file names)

[CmdletBinding()]
param(
    [Parameter(ValueFromRemainingArguments = $true)]
    [string[]]$ArgsList
)

$ErrorActionPreference = 'Stop'

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$ModuleDir = Join-Path $ScriptDir 'collectlogs'
$Prebuilt = Join-Path $ScriptDir 'collectlogs.exe'

if ($null -eq $ArgsList) { $ArgsList = @() }

if (Test-Path -PathType Leaf $Prebuilt) {
    # Packaged install: the compiled collector sits beside this script.
    & $Prebuilt @ArgsList
    exit $LASTEXITCODE
}

if (-not (Test-Path (Join-Path $ModuleDir 'go.mod'))) {
    Write-Error "collect-logs: no collector binary beside this script, and no module source at $ModuleDir to build one from."
    exit 2
}

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    Write-Host 'collect-logs: this checkout has no compiled collector, so go is' -ForegroundColor Red
    Write-Host '              required to build one, and it was not found on PATH.' -ForegroundColor Red
    Write-Host '              Install Go 1.24.3 or newer, or use the copy shipped' -ForegroundColor Red
    Write-Host '              with an installed PAIR.' -ForegroundColor Red
    exit 2
}

$BuildDir = Join-Path ([System.IO.Path]::GetTempPath()) ([System.Guid]::NewGuid().ToString())
New-Item -ItemType Directory -Path $BuildDir -Force | Out-Null

try {
    $Binary = Join-Path $BuildDir 'collectlogs.exe'

    Push-Location $ModuleDir
    try {
        & go build -o $Binary .
        if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
    } finally {
        Pop-Location
    }

    # Run from the caller's directory so that a relative -out path means what the
    # caller expects.
    & $Binary @ArgsList
    exit $LASTEXITCODE
} finally {
    Remove-Item -Recurse -Force $BuildDir -ErrorAction SilentlyContinue
}
