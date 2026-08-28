// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { app } from 'electron'
import { platform } from 'os'
import path from 'path'
import { currentPlatform } from '@/shared/utils/platform'
import {
    APP_DATA_DIR_NAME,
    APP_ORG,
    APP_PREVIOUS_DATA_DIR_NAME,
    APP_PREVIOUS_ORG
} from '@/shared/constants/app'
import { migrateAppDataDirectory } from '@/electron/app-data-migration'

export interface PathProvider {
    getUserData(): string
    getTemp(): string
    getResourcesPath(): string
    getAppName(): string
}

const BASE_ROOT =
    platform() === 'win32'
        ? (process.env.LOCALAPPDATA ?? app.getPath('appData').replace('Roaming', 'Local'))
        : currentPlatform() === 'linux'
          ? (process.env.XDG_CONFIG_HOME ?? app.getPath('appData'))
          : path.join(app.getPath('home'), 'Library', 'Application Support')
const ROOT = path.join(BASE_ROOT, APP_ORG)
const PREVIOUS_ROOT = path.join(BASE_ROOT, APP_PREVIOUS_ORG)
const APP_DIR = path.join(ROOT, APP_DATA_DIR_NAME)

/**
 * The generated `nvpair` launcher directory (see `src/electron/nvpair-command.ts`)
 * lives under userData on Windows and is referenced by absolute path from the
 * user's PATH. Leave it in the previous directory so `nvpair` keeps resolving
 * in already-open terminals; the app regenerates it in the new location on the
 * next launch.
 */
const MIGRATION_SKIP_ENTRIES = ['bin'] as const

/**
 * One-time merge of the pre-rename Electron app data into the shared directory.
 * Must run for a single instance only — callers invoke it after acquiring the
 * single-instance lock so two launches cannot migrate concurrently.
 */
export const migrateAppData = (): void => {
    migrateAppDataDirectory(
        PREVIOUS_ROOT,
        APP_PREVIOUS_DATA_DIR_NAME,
        ROOT,
        APP_DATA_DIR_NAME,
        MIGRATION_SKIP_ENTRIES
    )
}

export const setPaths = async (): Promise<void> => {
    // make it stick even in dev
    app.commandLine.appendSwitch('user-data-dir', APP_DIR)
    app.commandLine.appendSwitch('disk-cache-dir', path.join(APP_DIR, 'cache'))

    // override Electron paths BEFORE any other imports
    app.setPath('appData', ROOT)
    app.setPath('userData', APP_DIR)
    app.setPath('sessionData', path.join(APP_DIR, 'session'))
    app.setPath('logs', path.join(APP_DIR, 'logs'))
    app.setAppLogsPath(path.join(APP_DIR, 'logs'))
    app.setPath('crashDumps', path.join(APP_DIR, 'crash'))
    app.setPath('temp', path.join(APP_DIR, 'tmp'))
    app.setPath('userCache', path.join(APP_DIR, 'cache'))

    return
}

/**
 * PathProvider backed by Electron's app.getPath(). This is the sole provider in
 * production; tests inject their own PathProvider via `initPlatform` so they
 * resolve `getUserData()` to a per-worker tmpdir (see tests/fixtures).
 */
export class ElectronPathProvider implements PathProvider {
    private app: { getPath(name: string): string; getName(): string }

    constructor(electronApp: { getPath(name: string): string; getName(): string }) {
        this.app = electronApp
    }

    getUserData(): string {
        return this.app.getPath('userData')
    }

    getTemp(): string {
        return this.app.getPath('temp')
    }

    getResourcesPath(): string {
        return process.resourcesPath ?? this.app.getPath('userData')
    }

    getAppName(): string {
        return this.app.getName()
    }
}
