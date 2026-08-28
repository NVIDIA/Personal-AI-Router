// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { app, dialog, type BrowserWindow, type SaveDialogOptions } from 'electron'
import { existsSync, readFileSync, writeFileSync } from 'fs'
import os from 'os'
import path from 'path'
import { version } from '@/../package.json'
import { APP_DISPLAY_NAME } from '@/shared/constants/app'
import { MODULAR_RUNTIME_BINARIES } from '@/shared/constants/modular-binaries'
import { MODULAR_DEFAULT_LOG_LEVEL } from '@/shared/constants/modular-runtime'
import { getStructuredLogFilePath } from '@/shared/utils/log'
import { currentPlatform } from '@/shared/utils/platform'
import { getModularBridgeState } from '@/electron/service-bridge/modular-state'
import { getCliBinDir, getModularSupervisor } from '@/electron/service-bridge/modular-supervisor'

const SESSION_STARTED_AT = new Date(Date.now() - process.uptime() * 1000)

function safeTimestamp(date: Date): string {
    return date.toISOString().replace(/[:.]/g, '-')
}

function readTextFile(filePath: string): string {
    if (!filePath || !existsSync(filePath)) return ''
    try {
        return readFileSync(filePath, 'utf8')
    } catch (err) {
        const message = err instanceof Error ? err.message : String(err)
        return `Unable to read ${filePath}: ${message}`
    }
}

function logLineTimeMs(line: string): number | null {
    const match = /"time"\s*:\s*"([^"]+)"/.exec(line)
    if (!match) return null

    const timestamp = Date.parse(match[1])
    return Number.isNaN(timestamp) ? null : timestamp
}

function readSessionTextFile(filePath: string, sessionStartedAt: Date): string {
    if (!filePath || !existsSync(filePath)) return ''
    try {
        const sessionStartedAtMs = sessionStartedAt.getTime()
        return readFileSync(filePath, 'utf8')
            .split('\n')
            .filter(line => {
                if (!line.trim()) return false
                const timestamp = logLineTimeMs(line)
                return timestamp !== null && timestamp >= sessionStartedAtMs
            })
            .join('\n')
    } catch (err) {
        const message = err instanceof Error ? err.message : String(err)
        return `Unable to read ${filePath}: ${message}`
    }
}

function section(title: string, body: string): string {
    return [`## ${title}`, body.trimEnd() || '(none)', ''].join('\n')
}

function jsonSectionBody(value: object): string {
    return JSON.stringify(value, null, 2)
}

function buildDebugLogBundle(): string {
    const generatedAt = new Date()
    const state = getModularBridgeState()
    const structuredLogPath = getStructuredLogFilePath()
    const cliBinDir = getCliBinDir()
    const cliManifestPath = path.join(cliBinDir, 'manifest.json')
    const modularLogs = state
        .getLogs()
        .map(entry => JSON.stringify(entry))
        .join('\n')
    const envLogLevel = process.env.PAIR_LOG_LEVEL ?? null

    const metadata = {
        generatedAt: generatedAt.toISOString(),
        sessionStartedAt: SESSION_STARTED_AT.toISOString(),
        appName: app.getName(),
        appVersion: app.getVersion(),
        packageVersion: version,
        isPackaged: app.isPackaged,
        platform: currentPlatform(),
        arch: process.arch,
        hostname: os.hostname(),
        userDataPath: app.getPath('userData'),
        structuredLogPath,
        cliBinDir,
        modularRuntimeBinaries: MODULAR_RUNTIME_BINARIES.map(binary => ({
            baseName: binary.baseName,
            processName: binary.processName,
            launchOwner: binary.launchOwner,
            optional: binary.optional === true
        })),
        modularLogLevel: getModularSupervisor().getLogLevel(),
        modularDefaultLogLevel: MODULAR_DEFAULT_LOG_LEVEL,
        envLogLevel,
        processVersions: process.versions
    }

    const modularState = {
        selfId: state.getSelfId(),
        nodesInitial: state.getNodesInitial(),
        availableNodes: state.getAvailableNodes(),
        engineInitialState: state.getEngineInitialState(),
        nodeInfoPollTargets: state.getNodeInfoPollTargets()
    }

    return [
        `# ${APP_DISPLAY_NAME} Logs`,
        '',
        section('Metadata', jsonSectionBody(metadata)),
        section('Current Modular State', jsonSectionBody(modularState)),
        section('CLI Binary Manifest', readTextFile(cliManifestPath)),
        section(
            'Structured App Log JSONL',
            readSessionTextFile(structuredLogPath, SESSION_STARTED_AT)
        ),
        section('Modular Subprocess Ring JSONL', modularLogs)
    ].join('\n')
}

export async function saveDebugLogs(ownerWindow: BrowserWindow | null): Promise<string | null> {
    const fileName = `nvpair-logs-${safeTimestamp(new Date())}.txt`
    const options: SaveDialogOptions = {
        title: `Save ${APP_DISPLAY_NAME} logs`,
        defaultPath: path.join(app.getPath('documents'), fileName),
        filters: [
            { name: 'Text Files', extensions: ['txt'] },
            { name: 'All Files', extensions: ['*'] }
        ]
    }

    const result =
        ownerWindow && !ownerWindow.isDestroyed()
            ? await dialog.showSaveDialog(ownerWindow, options)
            : await dialog.showSaveDialog(options)

    if (result.canceled || !result.filePath) return null

    writeFileSync(result.filePath, buildDebugLogBundle(), 'utf8')
    return result.filePath
}
