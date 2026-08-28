// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { app } from 'electron'
import fs from 'fs'
import path from 'path'
import type { JsonObject, JsonValue } from '@/electron/service-bridge/json-rpc-subprocess'

/**
 * Persists which update version has been downloaded to disk so the
 * "Restart & install" affordance can be restored on the next app launch.
 *
 * electron-updater only tracks a downloaded update in memory for the current
 * session; after a restart it forgets, even though the installer is still
 * cached. This marker lets `initializeUpdater` re-arm the updater from that
 * cache so the user does not have to re-download.
 */
function markerFilePath(): string {
    return path.join(app.getPath('userData'), 'configs', 'pending-update.json')
}

function objectValue(value: JsonValue | undefined): JsonObject | null {
    if (!value || typeof value !== 'object' || Array.isArray(value)) return null
    return value
}

function stringValue(value: JsonValue | undefined): string {
    return typeof value === 'string' ? value : ''
}

export function readPendingUpdateVersion(): string | null {
    try {
        const filePath = markerFilePath()
        if (!fs.existsSync(filePath)) return null
        const raw = fs.readFileSync(filePath, 'utf8')
        const parsed: JsonValue = JSON.parse(raw)
        const version = stringValue(objectValue(parsed)?.version)
        return version || null
    } catch {
        return null
    }
}

export function writePendingUpdateVersion(version: string): void {
    try {
        const filePath = markerFilePath()
        fs.mkdirSync(path.dirname(filePath), { recursive: true })
        const tmp = `${filePath}.tmp`
        fs.writeFileSync(tmp, JSON.stringify({ version }, null, 2), 'utf8')
        fs.renameSync(tmp, filePath)
    } catch {
        /* best-effort */
    }
}

export function clearPendingUpdate(): void {
    try {
        const filePath = markerFilePath()
        if (fs.existsSync(filePath)) fs.rmSync(filePath, { force: true })
    } catch {
        /* best-effort */
    }
}
