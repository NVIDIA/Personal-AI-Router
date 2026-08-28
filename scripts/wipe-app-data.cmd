@REM SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
@REM SPDX-License-Identifier: Apache-2.0

@echo off
setlocal EnableExtensions
REM Thin Windows launcher for wipe-app-data.ps1 (no Node required).
REM Runnable from anywhere: scripts\wipe-app-data.cmd --confirm

set "SCRIPT_DIR=%~dp0"
powershell -NoProfile -ExecutionPolicy Bypass -File "%SCRIPT_DIR%wipe-app-data.ps1" %*
exit /b %ERRORLEVEL%
