// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { app } from 'electron'
import { destroyConnector } from '@/electron/connector'
import { destroyTray } from '@/electron/tray'
import { spawnWipeScript } from '@/electron/run-wipe-script'
import { getModularSupervisor } from '@/electron/service-bridge/modular-supervisor'
import { createStructuredLogger } from '@/shared/utils/log'
import type { ManagedEngineUninstall } from '@/shared/types/engine-api'

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
 * Remove the engines PAIR installed, before the wipe script deletes the app
 * data folder.
 *
 * Deleting that folder only reaches the engines that install inside it. A
 * vendor installer picks its own location — LM Studio lands in the user's home
 * — so without this, resetting app data removed Ollama and llama.cpp and left
 * LM Studio behind.
 *
 * One backend call, not a loop over engines: engine-manager owns which installs
 * are PAIR's and what removing one safely involves, including preserving each
 * engine's model store. Deciding that here would be a second implementation of
 * a rule `services/` owns, and the TUI and the platform uninstallers go through
 * the same method.
 *
 * Best-effort. A reset the user asked for still has to clear their data, so
 * every failure is logged and the sequence continues.
 */
async function removeManagedEngines(): Promise<void> {
    const supervisor = getModularSupervisor()
    if (!supervisor.hasProcess('broker')) {
        log.info({
            sublevel: 'wipe',
            message: 'Service is not running; PAIR-installed engines are left in place'
        })
        return
    }
    let outcomes: ManagedEngineUninstall[] = []
    try {
        outcomes = await supervisor.uninstallManagedEngines()
    } catch (err) {
        log.warn({
            sublevel: 'wipe',
            message: `Could not remove PAIR-installed engines: ${err instanceof Error ? err.message : String(err)}`
        })
        return
    }
    for (const outcome of outcomes) {
        if (outcome.removed) {
            log.info({
                sublevel: 'wipe',
                message: `Removed PAIR-installed engine ${outcome.engine}`
            })
        } else if (outcome.error !== '') {
            // Files are still on disk, and the record of who owns them is about
            // to be deleted with the data folder.
            log.error({
                sublevel: 'wipe',
                message: `Engine ${outcome.engine} not removed: ${outcome.error}`
            })
        } else {
            log.info({
                sublevel: 'wipe',
                message: `Engine ${outcome.engine} left in place; PAIR did not install it`
            })
        }
    }
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
    // Engines first: this needs the broker, which the teardown below stops, and
    // the records of which installs were ours live in the folder being deleted.
    await removeManagedEngines()
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
