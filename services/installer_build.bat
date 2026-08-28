@REM SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
@REM SPDX-License-Identifier: Apache-2.0

@REM REFERENCE SCRIPT. This shows how NVIDIA assembles the services bundle; it is
@REM not a supported way to produce a distributable build. Anything it emits is
@REM unsigned, because signing lives outside this repository. To build and run the
@REM services locally, use build.bat and run the binaries from build\bin.

@echo off
setlocal

set ROOT=%~dp0
set VERSIONS_FILE=%ROOT%versions.json

echo  NOTE: installer_build.bat is a reference script. Its output is unsigned and
echo  is not a supported distributable build. For local use, run build.bat.
echo.

echo ========================================
echo  Step 1: Building all components
echo ========================================
echo.

call "%ROOT%build.bat"
if %ERRORLEVEL% neq 0 (
    echo.
    echo  Build failed -- aborting installer creation.
    endlocal
    exit /b 1
)

echo.
echo ========================================
echo  Step 2: Creating installer
echo ========================================
echo.

REM Look for makensis on PATH first, then common install locations
set MAKENSIS=
where makensis >nul 2>nul
if %ERRORLEVEL% equ 0 (
    set "MAKENSIS=makensis"
    goto :found_nsis
)
if exist "C:\Program Files (x86)\NSIS\makensis.exe" (
    set "MAKENSIS=C:\Program Files (x86)\NSIS\makensis.exe"
    goto :found_nsis
)
if exist "C:\Program Files\NSIS\makensis.exe" (
    set "MAKENSIS=C:\Program Files\NSIS\makensis.exe"
    goto :found_nsis
)

echo  ERROR: makensis.exe not found.
echo  Install NSIS from https://nsis.sourceforge.io/Download
echo  and add it to PATH, or install to the default location.
endlocal
exit /b 1

:found_nsis
echo  Using: %MAKENSIS%
echo.

REM Create dist output directory
if not exist "%ROOT%dist" mkdir "%ROOT%dist"

REM Resolve installer version. Precedence:
REM   1. Explicit CLI arg:  installer_build.bat 1.2.3
REM   2. versions.json "installer" field
REM   3. versions.json "product" field (fallback)
REM
REM build.bat (called above) already verified jq is on PATH, so we don't
REM re-check here. Using a :get_version subroutine keeps the for /f out of
REM any enclosing IF block, which avoids cmd.exe's parens-in-IF parser bugs.
set "VERSION=%~1"
if "%VERSION%"=="" call :get_version

if "%VERSION%"=="" (
    echo  ERROR: could not determine installer version -- no CLI arg and no versions.json.
    endlocal
    exit /b 1
)

echo  Installer version: %VERSION%
echo.

"%MAKENSIS%" /V3 /DPRODUCT_VERSION=%VERSION% "%ROOT%installer\nvpair-setup.nsi"
if %ERRORLEVEL% neq 0 (
    echo.
    echo  INSTALLER BUILD FAILED
    endlocal
    exit /b 1
)

echo.
echo ========================================
echo  Installer ready
echo ========================================
echo.
echo  %ROOT%dist\NVIDIA-Personal-AI-Router-%VERSION%-Setup.exe
echo.

endlocal
exit /b 0

:get_version
if not exist "%VERSIONS_FILE%" exit /b 0
REM jq's `//` operator returns the right-hand side when the left is null or
REM false, giving us the installer-then-product fallback in one filter.
for /f "delims=" %%V in ('jq -r ".installer // .product" "%VERSIONS_FILE%"') do set "VERSION=%%V"
if /i "%VERSION%"=="null" set "VERSION="
exit /b 0
