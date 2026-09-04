@REM SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
@REM SPDX-License-Identifier: Apache-2.0

@echo off
setlocal

set ROOT=%~dp0
set BIN_OUT=%ROOT%build\bin
set VERSIONS_FILE=%ROOT%versions.json

REM jq is required to parse versions.json. PowerShell startup added ~3s of
REM dead time per build; jq is ~200x faster. Fail fast with a useful hint.
where jq >nul 2>&1
if errorlevel 1 (
    echo  ERROR: jq not found in PATH.
    echo         Install it with one of:
    echo           winget install jqlang.jq
    echo           choco install jq
    echo           scoop install jq
    echo         or grab a binary from https://jqlang.org/download/
    endlocal
    exit /b 1
)

echo ========================================
echo  Reading versions from %VERSIONS_FILE%
echo ========================================
echo.

if not exist "%VERSIONS_FILE%" (
    echo  ERROR: versions.json not found at %VERSIONS_FILE%
    endlocal
    exit /b 1
)

REM Parse versions.json with jq. We use --arg to pass each component key as
REM a string variable, which sidesteps cmd's hostility toward embedded
REM double quotes inside the jq filter (component keys contain hyphens, so
REM bare .components.nvpair-ui-broker would parse as subtraction).
for /f "delims=" %%V in ('jq -r ".product" "%VERSIONS_FILE%"')                                                  do set "V_PRODUCT=%%V"
for /f "delims=" %%V in ('jq -r --arg k "ollama-proxy"         ".components[$k]" "%VERSIONS_FILE%"')            do set "V_PROXY=%%V"
for /f "delims=" %%V in ('jq -r --arg k "lmstudio-proxy"       ".components[$k]" "%VERSIONS_FILE%"')            do set "V_LMPROXY=%%V"
for /f "delims=" %%V in ('jq -r --arg k "llamacpp-proxy"       ".components[$k]" "%VERSIONS_FILE%"')            do set "V_LLAMAPROXY=%%V"
for /f "delims=" %%V in ('jq -r --arg k "nvpair-node-info"        ".components[$k]" "%VERSIONS_FILE%"')            do set "V_NINFO=%%V"
for /f "delims=" %%V in ('jq -r --arg k "nvpair-node-scanner"     ".components[$k]" "%VERSIONS_FILE%"')            do set "V_NSCAN=%%V"
for /f "delims=" %%V in ('jq -r --arg k "nvpair-manual-nodes"     ".components[$k]" "%VERSIONS_FILE%"')            do set "V_MNODES=%%V"
for /f "delims=" %%V in ('jq -r --arg k "nvpair-workload-manager" ".components[$k]" "%VERSIONS_FILE%"')            do set "V_WLMGR=%%V"
for /f "delims=" %%V in ('jq -r --arg k "nvpair-errors"           ".components[$k]" "%VERSIONS_FILE%"')            do set "V_ERRORS=%%V"
for /f "delims=" %%V in ('jq -r --arg k "nvpair-node-settings"    ".components[$k]" "%VERSIONS_FILE%"')            do set "V_NSETTINGS=%%V"
for /f "delims=" %%V in ('jq -r --arg k "nvpair-ui-broker"        ".components[$k]" "%VERSIONS_FILE%"')            do set "V_BROKER=%%V"
for /f "delims=" %%V in ('jq -r --arg k "nvpair-engine-manager"   ".components[$k]" "%VERSIONS_FILE%"')            do set "V_ENGMGR=%%V"
for /f "delims=" %%V in ('jq -r --arg k "nvpair-cluster-manager"  ".components[$k]" "%VERSIONS_FILE%"')            do set "V_CLUMGR=%%V"
for /f "delims=" %%V in ('jq -r --arg k "nvpair-job-scheduler"    ".components[$k]" "%VERSIONS_FILE%"')            do set "V_SCHED=%%V"
for /f "delims=" %%V in ('jq -r --arg k "nvpair-tui"              ".components[$k]" "%VERSIONS_FILE%"')            do set "V_TUI=%%V"

if "%V_PRODUCT%"=="" (
    echo  ERROR: failed to parse versions.json
    endlocal
    exit /b 1
)

echo  product           = %V_PRODUCT%
echo  ollama-proxy      = %V_PROXY%
echo  lmstudio-proxy    = %V_LMPROXY%
echo  llamacpp-proxy    = %V_LLAMAPROXY%
echo  nvpair-node-info     = %V_NINFO%
echo  nvpair-node-scanner  = %V_NSCAN%
echo  nvpair-manual-nodes  = %V_MNODES%
echo  nvpair-workload-mgr  = %V_WLMGR%
echo  nvpair-errors        = %V_ERRORS%
echo  nvpair-node-settings = %V_NSETTINGS%
echo  nvpair-ui-broker     = %V_BROKER%
echo  nvpair-engine-manager= %V_ENGMGR%
echo  nvpair-cluster-mgr   = %V_CLUMGR%
echo  nvpair-job-scheduler = %V_SCHED%
echo  nvpair-tui           = %V_TUI%
echo.

echo ========================================
echo  Building all components
echo ========================================
echo.

echo [1/14] Building ollama-proxy (v%V_PROXY%)...
cd /d "%ROOT%ollama-proxy"
go build -ldflags "-X main.Version=%V_PROXY%" -o ollama-proxy.exe . || goto :fail
echo       OK

echo [2/14] Building lmstudio-proxy (v%V_LMPROXY%)...
cd /d "%ROOT%lmstudio-proxy"
go build -ldflags "-X main.Version=%V_LMPROXY%" -o lmstudio-proxy.exe . || goto :fail
echo       OK

echo [3/14] Building llamacpp-proxy (v%V_LLAMAPROXY%)...
cd /d "%ROOT%llamacpp-proxy"
go build -ldflags "-X main.Version=%V_LLAMAPROXY%" -o llamacpp-proxy.exe . || goto :fail
echo       OK

echo [4/14] Building nvpair-node-info (v%V_NINFO%)...
cd /d "%ROOT%nvpair-node-info"
go build -ldflags "-X main.Version=%V_NINFO%" -o nvpair-node-info.exe . || goto :fail
echo       OK

echo [5/14] Building nvpair-node-scanner (v%V_NSCAN%)...
cd /d "%ROOT%nvpair-node-scanner"
go build -ldflags "-X main.Version=%V_NSCAN%" -o nvpair-node-scanner.exe . || goto :fail
echo       OK

echo [6/14] Building nvpair-manual-nodes (v%V_MNODES%)...
cd /d "%ROOT%nvpair-manual-nodes"
go build -ldflags "-X main.Version=%V_MNODES%" -o nvpair-manual-nodes.exe . || goto :fail
echo       OK

echo [7/14] Building nvpair-workload-manager (v%V_WLMGR%)...
cd /d "%ROOT%nvpair-workload-manager"
go build -ldflags "-X main.Version=%V_WLMGR%" -o nvpair-workload-manager.exe . || goto :fail
echo       OK

echo [8/14] Building nvpair-errors (v%V_ERRORS%)...
cd /d "%ROOT%nvpair-errors"
go build -ldflags "-X main.Version=%V_ERRORS%" -o nvpair-errors.exe . || goto :fail
echo       OK

echo [9/14] Building nvpair-engine-manager (v%V_ENGMGR%)...
cd /d "%ROOT%nvpair-engine-manager"
go build -ldflags "-X main.Version=%V_ENGMGR%" -o nvpair-engine-manager.exe . || goto :fail
echo       OK

echo [10/14] Building nvpair-node-settings (v%V_NSETTINGS%)...
cd /d "%ROOT%nvpair-node-settings"
go build -ldflags "-X main.Version=%V_NSETTINGS%" -o nvpair-node-settings.exe . || goto :fail
echo       OK

echo [11/14] Building nvpair-ui-broker (v%V_BROKER%)...
cd /d "%ROOT%nvpair-ui-broker"
go build -ldflags "-X main.Version=%V_BROKER%" -o nvpair-ui-broker.exe . || goto :fail
echo       OK

echo [12/14] Building nvpair-cluster-manager (v%V_CLUMGR%)...
cd /d "%ROOT%nvpair-cluster-manager"
go build -ldflags "-X main.Version=%V_CLUMGR%" -o nvpair-cluster-manager.exe . || goto :fail
echo       OK

echo [13/14] Building nvpair-job-scheduler (v%V_SCHED%)...
cd /d "%ROOT%nvpair-job-scheduler"
go build -ldflags "-X main.Version=%V_SCHED%" -o nvpair-job-scheduler.exe . || goto :fail
echo       OK

echo [14/14] Building nvpair-tui (v%V_TUI%)...
cd /d "%ROOT%nvpair-tui"
go build -ldflags "-X main.Version=%V_TUI%" -o nvpair-tui.exe . || goto :fail
echo       OK

echo.
echo ========================================
echo  Copying binaries to %BIN_OUT%
echo ========================================
echo.

REM Start from an empty bin dir so a binary from an earlier build (a component
REM that doesn't exist at this commit) can't linger and ride into the installer
REM as a stale stray.
if exist "%BIN_OUT%" rmdir /s /q "%BIN_OUT%"
mkdir "%BIN_OUT%"
copy /y "%ROOT%ollama-proxy\ollama-proxy.exe" "%BIN_OUT%\ollama-proxy.exe" >nul || goto :fail
copy /y "%ROOT%lmstudio-proxy\lmstudio-proxy.exe" "%BIN_OUT%\lmstudio-proxy.exe" >nul || goto :fail
copy /y "%ROOT%llamacpp-proxy\llamacpp-proxy.exe" "%BIN_OUT%\llamacpp-proxy.exe" >nul || goto :fail
copy /y "%ROOT%nvpair-node-info\nvpair-node-info.exe" "%BIN_OUT%\nvpair-node-info.exe" >nul || goto :fail
copy /y "%ROOT%nvpair-node-scanner\nvpair-node-scanner.exe" "%BIN_OUT%\nvpair-node-scanner.exe" >nul || goto :fail
copy /y "%ROOT%nvpair-manual-nodes\nvpair-manual-nodes.exe" "%BIN_OUT%\nvpair-manual-nodes.exe" >nul || goto :fail
copy /y "%ROOT%nvpair-workload-manager\nvpair-workload-manager.exe" "%BIN_OUT%\nvpair-workload-manager.exe" >nul || goto :fail
copy /y "%ROOT%nvpair-errors\nvpair-errors.exe" "%BIN_OUT%\nvpair-errors.exe" >nul || goto :fail
copy /y "%ROOT%nvpair-engine-manager\nvpair-engine-manager.exe" "%BIN_OUT%\nvpair-engine-manager.exe" >nul || goto :fail
copy /y "%ROOT%nvpair-node-settings\nvpair-node-settings.exe" "%BIN_OUT%\nvpair-node-settings.exe" >nul || goto :fail
copy /y "%ROOT%nvpair-ui-broker\nvpair-ui-broker.exe" "%BIN_OUT%\nvpair-ui-broker.exe" >nul || goto :fail
copy /y "%ROOT%nvpair-cluster-manager\nvpair-cluster-manager.exe" "%BIN_OUT%\nvpair-cluster-manager.exe" >nul || goto :fail
copy /y "%ROOT%nvpair-job-scheduler\nvpair-job-scheduler.exe" "%BIN_OUT%\nvpair-job-scheduler.exe" >nul || goto :fail
copy /y "%ROOT%nvpair-tui\nvpair-tui.exe" "%BIN_OUT%\nvpair-tui.exe" >nul || goto :fail

echo.
echo ========================================
echo  Build complete (product v%V_PRODUCT%)
echo ========================================
echo.
echo  Proxy:            %BIN_OUT%\ollama-proxy.exe
echo  LM Studio Proxy:  %BIN_OUT%\lmstudio-proxy.exe
echo  Llama.cpp Proxy:  %BIN_OUT%\llamacpp-proxy.exe
echo  Node Info:        %BIN_OUT%\nvpair-node-info.exe
echo  Node Scanner:     %BIN_OUT%\nvpair-node-scanner.exe
echo  Manual Nodes:     %BIN_OUT%\nvpair-manual-nodes.exe
echo  Workload Mgr:     %BIN_OUT%\nvpair-workload-manager.exe
echo  Errors:           %BIN_OUT%\nvpair-errors.exe
echo  Engine Mgr:       %BIN_OUT%\nvpair-engine-manager.exe
echo  Node Settings:    %BIN_OUT%\nvpair-node-settings.exe
echo  UI Broker:        %BIN_OUT%\nvpair-ui-broker.exe
echo  Cluster Mgr:      %BIN_OUT%\nvpair-cluster-manager.exe
echo  Job Scheduler:    %BIN_OUT%\nvpair-job-scheduler.exe
echo  TUI:              %BIN_OUT%\nvpair-tui.exe
echo.

REM Surface the product version to any caller (e.g. installer_build.bat) so
REM they don't have to re-parse versions.json.
endlocal & set "NVPAIR_PRODUCT_VERSION=%V_PRODUCT%"
exit /b 0

:fail
echo.
echo  BUILD FAILED
echo.
endlocal
exit /b 1
