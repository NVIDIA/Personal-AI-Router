// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import fs from 'fs'
import path from 'path'
import { app, shell } from 'electron'
import { safeHandle } from '@/electron/ipc/safe-handle'
import { getStructuredLogFilePath } from '@/shared/utils/log'
import { detectLicenseType } from '@/shared/utils/detect-license'
import type { ServiceStatus, ServiceVersions, StartupState } from '@/shared/types/ipc-channels'
import {
    getConnectorStatus,
    getConnectorError,
    didWeSpawnCli,
    initializeConnector,
    destroyConnector,
    restartConnector
} from '@/electron/connector'
import {
    getModularLogLevel,
    getStartupSettings,
    setModularLogLevel,
    setStartupSettings
} from '@/electron/config/ui-config'
import { applyStartupChange, isLoginItemSupported, readLaunchAtLogin } from '@/electron/login-item'
import {
    getModularSupervisor,
    readCliBinManifest
} from '@/electron/service-bridge/modular-supervisor'
import { modularShippedBinaryBaseNames } from '@/shared/constants/modular-binaries'

const LICENSE_FILE = 'LICENSE'
const THIRD_PARTY_LICENSE_FILE = 'THIRD_PARTY_NOTICES.md'

/**
 * Resolve a file shipped via electron-builder `extraResources`. Packaged builds
 * place these next to `process.resourcesPath`; in dev they live at the repo root
 * (`app.getAppPath()`). Mirrors `getCliBinDir()` in the modular supervisor.
 */
function resolveShippedFile(name: string): string {
    const base = app.isPackaged ? process.resourcesPath : app.getAppPath()
    return path.join(base, name)
}

/**
 * The startup switches as Settings should show them, plus whether this OS has a
 * login item to write.
 *
 * `launchAtLogin` is what the OS reports, not the stored preference, so an entry
 * the user disabled outside the app does not read as on. `startHidden` has no OS
 * state of its own (macOS login items carry no arguments), so it is the stored
 * preference.
 */
function startupState(): StartupState {
    const stored = getStartupSettings()
    return {
        launchAtLogin: readLaunchAtLogin(stored),
        startHidden: stored.startHidden,
        supported: isLoginItemSupported()
    }
}

/** Open a shipped file in the OS default handler, falling back to revealing it. */
async function openShippedFile(name: string): Promise<void> {
    const target = resolveShippedFile(name)
    if (!fs.existsSync(target)) return
    const openError = await shell.openPath(target)
    if (openError) shell.showItemInFolder(target)
}

export function registerServiceIpc(): void {
    safeHandle('service:get-status', async (): Promise<ServiceStatus> => {
        const status: ServiceStatus = {
            connectorStatus: getConnectorStatus(),
            weSpawned: didWeSpawnCli()
        }
        const error = getConnectorError()
        return error ? { ...status, error } : status
    })

    safeHandle('service:stop', async () => {
        await destroyConnector({ force: true })
    })

    safeHandle('service:start', async () => {
        await initializeConnector()
    })

    safeHandle('service:restart', async () => {
        await restartConnector()
    })

    safeHandle('service:get-versions', async (): Promise<ServiceVersions> => {
        const manifest = readCliBinManifest()
        const binaries = modularShippedBinaryBaseNames()
            .map(name => ({
                name,
                version: manifest.components[name] ?? ''
            }))
            .sort((a, b) => a.name.localeCompare(b.name))
        const licensePath = resolveShippedFile(LICENSE_FILE)
        const licenseType = fs.existsSync(licensePath)
            ? detectLicenseType(fs.readFileSync(licensePath, 'utf8'))
            : ''
        return {
            appVersion: app.getVersion(),
            modularServices: manifest.services,
            binaries,
            licenseType
        }
    })

    safeHandle('service:get-log-level', async () => {
        return getModularLogLevel()
    })

    safeHandle('service:set-log-level', async (_event, payload) => {
        // Persist so the next spawn uses it, and fan out live to the running
        // broker tree (broker → scanner/node-info/proxy/workload-manager)
        // so the change applies without a restart.
        setModularLogLevel(payload.level)
        getModularSupervisor().setLogLevel(payload.level)
    })

    safeHandle('service:get-startup', async (): Promise<StartupState> => {
        return startupState()
    })

    safeHandle('service:set-startup', async (_event, payload): Promise<StartupState> => {
        // What the switches showed, read before anything changes: a request that
        // repeats it is not a change and must not touch the OS entry.
        const previous = getStartupSettings()
        const registered = readLaunchAtLogin(previous)
        // Persist first, then write the login item: the config is what the next
        // launch reconciles from, so a login item that matched a payload the
        // config never took would be undone on restart. The response is read back
        // from the OS, so a switch the OS did not take reverts in the UI.
        const next = setStartupSettings(payload)
        applyStartupChange({ requested: payload, previous, next, registered })
        return startupState()
    })

    safeHandle('service:open-log-file', async () => {
        const logPath = getStructuredLogFilePath()
        if (!logPath || !fs.existsSync(logPath)) return
        // Open in the OS default handler. `.jsonl` often has no file association,
        // in which case openPath returns an error string — fall back to revealing
        // the file in the system file manager so the action always does something.
        const openError = await shell.openPath(logPath)
        if (openError) shell.showItemInFolder(logPath)
    })

    safeHandle('service:open-log-dir', async () => {
        const logPath = getStructuredLogFilePath()
        if (!logPath) return
        const dir = path.dirname(logPath)
        if (!fs.existsSync(dir)) return
        await shell.openPath(dir)
    })

    safeHandle('service:open-license', async () => {
        await openShippedFile(LICENSE_FILE)
    })

    safeHandle('service:open-third-party-licenses', async () => {
        await openShippedFile(THIRD_PARTY_LICENSE_FILE)
    })
}
