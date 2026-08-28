// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { spawn } from 'child_process'
import fs from 'fs'
import path from 'path'
import { app } from 'electron'
import { currentPlatform } from '@/shared/utils/platform'

/**
 * Resolve the platform wipe script shipped at repo root (dev) or in
 * `resources/scripts` (packaged). The scripts own the append-only inventory;
 * Electron only shuts down around them.
 */
function resolveWipeScriptPath(): string {
    const scriptName = currentPlatform() === 'win32' ? 'wipe-app-data.cmd' : 'wipe-app-data.sh'
    if (app.isPackaged) {
        return path.join(process.resourcesPath, 'scripts', scriptName)
    }
    // Dev: electron-vite sets getAppPath() to the desktop/ package root.
    return path.join(app.getAppPath(), '..', 'scripts', scriptName)
}

interface SpawnWipeScriptOptions {
    /** Script waits for this pid to exit before deleting anything. */
    waitPid: number
    /** Executable the script starts once the wipe succeeded. Omit to wipe only. */
    relaunchExecPath?: string
}

/**
 * Start the wipe script detached so it outlives this process, then return.
 *
 * The caller must exit right after: the script waits for `waitPid` to disappear
 * before deleting, so Chromium cannot flush session and cache files back into
 * the directories being removed.
 *
 * Throws if the script file is missing (nothing has been deleted at that point).
 */
export function spawnWipeScript(options: SpawnWipeScriptOptions): void {
    const scriptPath = resolveWipeScriptPath()
    if (!fs.existsSync(scriptPath)) {
        throw new Error(`Wipe script not found: ${scriptPath}`)
    }

    const args = ['--confirm', '--force-kill', `--wait-pid=${options.waitPid}`]
    if (options.relaunchExecPath) {
        args.push(`--relaunch=${options.relaunchExecPath}`)
    }

    const child =
        currentPlatform() === 'win32'
            ? spawn(`"${scriptPath}"`, args.map(quoteWindowsArg), {
                  shell: true,
                  windowsHide: true,
                  detached: true,
                  stdio: 'ignore'
              })
            : spawn('bash', [scriptPath, ...args], {
                  detached: true,
                  stdio: 'ignore'
              })
    child.unref()
}

/** `cmd.exe` needs paths with spaces quoted; the value side of `--flag=value` included. */
function quoteWindowsArg(arg: string): string {
    const eq = arg.indexOf('=')
    if (eq === -1) {
        return arg
    }
    return `${arg.slice(0, eq)}="${arg.slice(eq + 1)}"`
}
