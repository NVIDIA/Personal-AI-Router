// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import fs from 'fs'
import path from 'path'
import { getPaths } from '@/electron/globals'
import {
    MODULAR_DEFAULT_LOG_LEVEL,
    isModularLogLevel,
    type ModularLogLevel
} from '@/shared/constants/modular-runtime'
import type { StartupSettings } from '@/shared/types/ipc-channels'

interface UiConfig {
    /** When true, first-run onboarding has not been completed or explicitly dismissed. */
    firstRun: boolean
    /** Log level passed to every backend binary (`--log-level` at spawn + live `log/set-level`). Authoritative source for the modular log level. */
    modularLogLevel: ModularLogLevel
    /** macOS only: the one-time privileged-helper setup (register the SMAppService daemon + configure the Application Firewall) has completed. Gates the first-run admin prompt; left false until the daemon is enabled and firewall configuration succeeds, so an approval-pending launch retries next time. */
    macHelperSetupComplete: boolean
    /** Windows/macOS: register the app as an OS login item so it starts when the user signs in. Off until it is switched on, so an install or an upgrade never adds a login item on its own. */
    launchAtLogin: boolean
    /** Whether the login item carries the hidden-start argument, so a launch at sign-in goes straight to the tray with no window. Only affects a login-item launch — starting the app by hand always opens the Overview. */
    startHidden: boolean
}

const DEFAULTS: UiConfig = {
    firstRun: true,
    modularLogLevel: MODULAR_DEFAULT_LOG_LEVEL,
    macHelperSetupComplete: false,
    launchAtLogin: false,
    startHidden: true
}

let config: UiConfig = { ...DEFAULTS }
let configFilePath = ''

/** Same directory as `service-config.json` (see `initConfigStore` in file-config-store). */
function getFilePath(): string {
    if (!configFilePath) {
        configFilePath = path.join(getPaths().getUserData(), 'configs', 'ui-config.json')
    }
    return configFilePath
}

function getLegacyFilePath(): string {
    return path.join(getPaths().getUserData(), 'ui-config.json')
}

export function loadUiConfig(): void {
    try {
        const filePath = getFilePath()
        if (fs.existsSync(filePath)) {
            const raw = fs.readFileSync(filePath, 'utf8')
            const parsed = JSON.parse(raw)
            const merged = { ...DEFAULTS, ...parsed } as UiConfig
            let migrated = false
            if (!('firstRun' in parsed)) {
                merged.firstRun = false
                migrated = true
            }
            config = merged
            if (migrated) save()
        } else {
            const legacy = getLegacyFilePath()
            if (fs.existsSync(legacy)) {
                const raw = fs.readFileSync(legacy, 'utf8')
                const parsed = JSON.parse(raw)
                const merged = { ...DEFAULTS, ...parsed } as UiConfig
                if (!('firstRun' in parsed)) {
                    merged.firstRun = false
                }
                config = merged
                save()
                try {
                    fs.unlinkSync(legacy)
                } catch {
                    /* best-effort remove after migrate */
                }
            }
        }
    } catch {
        config = { ...DEFAULTS }
    }
}

function save(): void {
    try {
        const filePath = getFilePath()
        const dir = path.dirname(filePath)
        fs.mkdirSync(dir, { recursive: true })
        const tmp = filePath + '.tmp'
        fs.writeFileSync(tmp, JSON.stringify(config, null, 2), 'utf8')
        fs.renameSync(tmp, filePath)
    } catch {
        /* best-effort */
    }
}

export function isFirstRun(): boolean {
    return config.firstRun
}

/** Persist that first-run onboarding completed or was explicitly dismissed. */
export function completeFirstRun(): void {
    if (!config.firstRun) return
    config.firstRun = false
    save()
}

export function getModularLogLevel(): ModularLogLevel {
    // Guard against a hand-edited / legacy config value that isn't a valid level.
    return isModularLogLevel(config.modularLogLevel)
        ? config.modularLogLevel
        : MODULAR_DEFAULT_LOG_LEVEL
}

export function setModularLogLevel(value: ModularLogLevel): void {
    config.modularLogLevel = value
    save()
}

/**
 * The persisted startup preferences.
 *
 * Read strictly rather than passed through: a config written by an older build
 * has neither key, and one edited by hand can hold anything, so each value falls
 * back to its default unless it is the boolean it claims to be.
 */
export function getStartupSettings(): StartupSettings {
    return {
        launchAtLogin:
            typeof config.launchAtLogin === 'boolean'
                ? config.launchAtLogin
                : DEFAULTS.launchAtLogin,
        startHidden:
            typeof config.startHidden === 'boolean' ? config.startHidden : DEFAULTS.startHidden
    }
}

/**
 * Persist the given startup preferences and return the merged result. Keys that
 * are absent — or not booleans, since the payload crosses IPC from the
 * renderer — keep their stored value.
 */
export function setStartupSettings(patch: Partial<StartupSettings>): StartupSettings {
    const next = getStartupSettings()
    if (typeof patch.launchAtLogin === 'boolean') next.launchAtLogin = patch.launchAtLogin
    if (typeof patch.startHidden === 'boolean') next.startHidden = patch.startHidden
    config.launchAtLogin = next.launchAtLogin
    config.startHidden = next.startHidden
    save()
    return next
}

export function isMacHelperSetupComplete(): boolean {
    return config.macHelperSetupComplete === true
}

export function setMacHelperSetupComplete(value: boolean): void {
    config.macHelperSetupComplete = value
    save()
}
