# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Send synthetic inference traffic at an Ollama- or LM Studio-compatible
# endpoint. Pointed at a Personal AI Router proxy it behaves exactly like any
# other third-party client, so the request enters the cluster router.
#
# Runnable from anywhere:
#   .\scripts\inference-dispatcher.ps1
#   .\scripts\inference-dispatcher.ps1 --backend lmstudio --count 5 --mode parallel
#   .\scripts\inference-dispatcher.ps1 --list-models
#   .\scripts\inference-dispatcher.ps1 --help
#
# With no arguments one prompt is sent to the default Ollama-compatible port
# using an automatically selected model.
#
# Requires go on PATH — the same toolchain services\build.bat already needs. No
# Node required, so this runs on a machine that only builds the backend.
#
# The client itself lives in scripts\inference-dispatcher (Go). The app ships a
# prebuilt copy for the Inference Demo (desktop\scripts\build-inference-dispatcher.ts);
# this wrapper exists so the same tool can be driven by hand without a build step.
#
# Keep in sync with:
#   - scripts/inference-dispatcher.sh (Unix twin — update both in the same change)
#   - docs/inference-dispatcher.md (documented flags and endpoints)

[CmdletBinding()]
param(
    [Parameter(ValueFromRemainingArguments = $true)]
    [string[]]$ArgsList
)

$ErrorActionPreference = 'Stop'

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$ModuleDir = Join-Path $ScriptDir 'inference-dispatcher'

if (-not (Test-Path (Join-Path $ModuleDir 'go.mod'))) {
    Write-Error "inference-dispatcher: cannot find the client module at $ModuleDir"
    exit 2
}

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    Write-Host 'inference-dispatcher: go is required and was not found on PATH.' -ForegroundColor Red
    Write-Host '                      Install Go 1.24.3 or newer (see AGENTS.md).' -ForegroundColor Red
    exit 2
}

$BuildDir = Join-Path ([System.IO.Path]::GetTempPath()) ([System.Guid]::NewGuid().ToString())
New-Item -ItemType Directory -Path $BuildDir -Force | Out-Null

try {
    $Binary = Join-Path $BuildDir 'inference-dispatcher.exe'

    Push-Location $ModuleDir
    try {
        & go build -o $Binary .
        if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
    } finally {
        Pop-Location
    }

    # Run from the caller's directory so that a relative --result-log or
    # --debug-error-log path means what the caller expects.
    if ($null -eq $ArgsList) { $ArgsList = @() }
    & $Binary @ArgsList
    exit $LASTEXITCODE
} finally {
    Remove-Item -Recurse -Force $BuildDir -ErrorAction SilentlyContinue
}
