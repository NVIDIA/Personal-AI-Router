// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { app, BrowserWindow } from 'electron'
import log from 'electron-log'
import electronUpdater from 'electron-updater'
import { destroyConnector } from '@/electron/connector'
import type { UpdateStatus } from '@/shared/types/update'
import pkg from '../../../package.json'
import getErrorString from '@/shared/utils/get-error-string'
import { notifyUpdateAvailable } from '@/electron/updater/update-notification'
import {
    type UpdateOperation,
    userFacingUpdateError
} from '@/electron/updater/update-error-message'
import {
    devMockCheckForUpdates,
    devMockDownloadUpdate,
    devMockQuitAndInstall,
    devMockUpdaterEnabled,
    initializeDevMockUpdater
} from '@/electron/updater/dev-mock'
import {
    clearPendingUpdate,
    readPendingUpdateVersion,
    writePendingUpdateVersion
} from '@/electron/updater/pending-update-store'

const { autoUpdater } = electronUpdater

let status: UpdateStatus = {
    phase: 'idle',
    currentVersion: pkg.version,
    latestVersion: null,
    downloadPercent: null,
    error: null
}

let activeUpdateOperation: UpdateOperation | null = null

/**
 * Version of an update already downloaded to disk in a prior session (loaded
 * from the pending-update marker). When the next check reports this same
 * version is available, we re-arm electron-updater from its cache so the
 * "Restart & install" button reappears without a fresh download.
 */
let pendingDownloadedVersion: string | null = null

function failUpdate(err: unknown, operation: UpdateOperation): void {
    log.error(`[updater] ${userFacingUpdateError(operation)}: ${getErrorString(err)}`)
    activeUpdateOperation = null
    setStatus({
        phase: 'error',
        error: userFacingUpdateError(operation)
    })
}

function setStatus(patch: Partial<UpdateStatus>): void {
    const prev = status
    status = { ...status, ...patch }
    if (
        status.phase === 'available' &&
        status.latestVersion &&
        status.latestVersion !== pendingDownloadedVersion &&
        (prev.phase !== 'available' || prev.latestVersion !== status.latestVersion)
    ) {
        notifyUpdateAvailable(status.latestVersion)
    }
    broadcastStatus()
}

function broadcastStatus(): void {
    for (const win of BrowserWindow.getAllWindows()) {
        if (!win.isDestroyed()) {
            win.webContents.send('update:status', status)
        }
    }
}

export function getUpdateStatus(): UpdateStatus {
    return status
}

const STARTUP_UPDATE_CHECK_DELAY_MS = 5000
const UPDATE_CHECK_INTERVAL_MS = 6 * 60 * 60 * 1000

function scheduleStartupUpdateCheck(): void {
    if (!app.isPackaged && !devMockUpdaterEnabled()) return

    setTimeout(() => {
        void checkForUpdates()
    }, STARTUP_UPDATE_CHECK_DELAY_MS)
}

let periodicCheckTimer: ReturnType<typeof setInterval> | null = null

function startPeriodicUpdateCheck(): void {
    if (!app.isPackaged && !devMockUpdaterEnabled()) return
    if (periodicCheckTimer) return

    periodicCheckTimer = setInterval(() => {
        // Never clobber an in-progress or ready-to-install state.
        if (
            status.phase === 'checking' ||
            status.phase === 'downloading' ||
            status.phase === 'downloaded'
        ) {
            return
        }
        void checkForUpdates()
    }, UPDATE_CHECK_INTERVAL_MS)
}

/**
 * Loads the pending-update marker. If it names the version we are already
 * running, the update was installed since it was written, so the marker is
 * stale and cleared. Otherwise it primes `pendingDownloadedVersion` so the
 * next `update-available` for that version re-arms the cached download.
 */
function loadPendingDownloadedVersion(): void {
    const marker = readPendingUpdateVersion()
    if (!marker) return
    if (marker === pkg.version) {
        clearPendingUpdate()
        return
    }
    pendingDownloadedVersion = marker
}

export function initializeUpdater(): void {
    if (devMockUpdaterEnabled()) {
        loadPendingDownloadedVersion()
        initializeDevMockUpdater(setStatus, pendingDownloadedVersion)
        scheduleStartupUpdateCheck()
        startPeriodicUpdateCheck()
        return
    }
    if (!app.isPackaged) return

    loadPendingDownloadedVersion()

    autoUpdater.logger = log
    autoUpdater.autoDownload = false
    autoUpdater.autoInstallOnAppQuit = false

    autoUpdater.on('checking-for-update', () => {
        setStatus({ phase: 'checking', error: null })
    })
    autoUpdater.on('update-available', info => {
        activeUpdateOperation = null
        setStatus({
            phase: 'available',
            latestVersion: info.version,
            downloadPercent: null,
            error: null
        })
        // A previously downloaded update is still cached on disk; re-arm
        // electron-updater so `quitAndInstall` works and the install button
        // returns without a real re-download.
        if (pendingDownloadedVersion === info.version) {
            void downloadUpdate()
        }
    })
    autoUpdater.on('update-not-available', info => {
        activeUpdateOperation = null
        setStatus({
            phase: 'not-available',
            latestVersion: info.version,
            downloadPercent: null,
            error: null
        })
    })
    autoUpdater.on('download-progress', progress => {
        setStatus({
            phase: 'downloading',
            downloadPercent: progress.percent
        })
    })
    autoUpdater.on('update-downloaded', info => {
        activeUpdateOperation = null
        pendingDownloadedVersion = info.version
        writePendingUpdateVersion(info.version)
        setStatus({
            phase: 'downloaded',
            latestVersion: info.version,
            downloadPercent: 100,
            error: null
        })
    })
    autoUpdater.on('error', err => {
        const operation = activeUpdateOperation ?? 'check'
        failUpdate(err, operation)
    })

    scheduleStartupUpdateCheck()
    startPeriodicUpdateCheck()
}

export async function checkForUpdates(): Promise<void> {
    if (devMockUpdaterEnabled()) {
        await devMockCheckForUpdates(setStatus, pendingDownloadedVersion)
        return
    }
    if (!app.isPackaged) return
    activeUpdateOperation = 'check'
    try {
        await autoUpdater.checkForUpdates()
        activeUpdateOperation = null
    } catch (err) {
        failUpdate(err, 'check')
    }
}

export async function downloadUpdate(): Promise<void> {
    if (devMockUpdaterEnabled()) {
        await devMockDownloadUpdate(setStatus)
        return
    }
    if (!app.isPackaged) return
    // Re-entrancy guard: a second click while a download is in flight is a no-op.
    if (status.phase === 'downloading') return
    activeUpdateOperation = 'download'
    // Emit the downloading state immediately so the UI gives deterministic
    // feedback before electron-updater's first download-progress event.
    setStatus({ phase: 'downloading', downloadPercent: null, error: null })
    try {
        await autoUpdater.downloadUpdate()
        activeUpdateOperation = null
    } catch (err) {
        failUpdate(err, 'download')
    }
}

export async function quitAndInstallUpdate(): Promise<void> {
    if (devMockUpdaterEnabled()) {
        devMockQuitAndInstall()
        return
    }
    if (!app.isPackaged) return
    await destroyConnector({ force: true })
    autoUpdater.quitAndInstall()
}
