// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { app } from 'electron'
import { destroyConnector } from '@/electron/connector'
import { destroyTray } from '@/electron/tray'
import { spawnWipeScript } from '@/electron/run-wipe-script'
import { createStructuredLogger } from '@/shared/utils/log'

const log = createStructuredLogger('app')

let appDataWipeScheduled = false

/** True while a wipe-and-relaunch sequence is in progress (skips duplicate quit cleanup). */
export function isAppDataWipeScheduled(): boolean {
    return appDataWipeScheduled
}

/**
 * Packaged builds can relaunch themselves after wipe. Unpackaged (`electron-vite
 * dev`) cannot — quitting Electron also tears down the Vite renderer server —
 * so the UI must tell the developer to restart manually.
 */
export function getAppDataWipePlan(): { willRelaunch: boolean } {
    return { willRelaunch: app.isPackaged }
}

/**
 * Stop the service tree, hand the wipe to the detached repo-root script, and exit.
 * The script waits for this process to die, then wipes. Packaged builds also pass
 * `--relaunch=` so the script starts the app again after deletes finish.
 * Unpackaged builds wipe and exit only — the caller must have warned the user.
 *
 * Called from Settings. Throws before deleting anything if the script is missing,
 * leaving the app running so the caller can report it.
 */
export async function wipeAppDataAndRelaunch(): Promise<void> {
    const { willRelaunch } = getAppDataWipePlan()
    appDataWipeScheduled = true
    log.info({ sublevel: 'wipe', message: 'Stopping service before app data wipe' })
    destroyTray()
    await destroyConnector({ force: true })

    try {
        spawnWipeScript({
            waitPid: process.pid,
            relaunchExecPath: willRelaunch ? process.execPath : undefined
        })
    } catch (err) {
        appDataWipeScheduled = false
        log.error({
            sublevel: 'wipe',
            message: `Could not start the wipe script: ${err instanceof Error ? err.message : String(err)}`
        })
        throw err
    }

    if (willRelaunch) {
        log.info({ sublevel: 'wipe', message: 'Wipe script started; exiting for relaunch' })
    } else {
        log.info({
            sublevel: 'wipe',
            message: 'Wipe script started; unpackaged build will quit without relaunch'
        })
    }
    app.exit(0)
}
