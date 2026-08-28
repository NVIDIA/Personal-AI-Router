// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { app } from 'electron'
import log from 'electron-log'
import pkg from '../../../package.json'
import type { UpdateStatus } from '@/shared/types/update'
import { UPDATE_CHECK_ERROR_MESSAGE } from '@/electron/updater/update-error-message'
import {
    clearPendingUpdate,
    writePendingUpdateVersion
} from '@/electron/updater/pending-update-store'

type StatusSetter = (patch: Partial<UpdateStatus>) => void

let timers: ReturnType<typeof setTimeout>[] = []

function delay(ms: number): Promise<void> {
    return new Promise(resolve => {
        const id = setTimeout(resolve, ms)
        timers.push(id)
    })
}

function bumpPatchVersion(version: string): string {
    const parts = version.split('.')
    const patch = Number(parts[2] ?? '0')
    if (Number.isNaN(patch)) return `${version}.1`
    parts[2] = String(patch + 1)
    return parts.join('.')
}

export function devMockUpdaterEnabled(): boolean {
    if (app.isPackaged) return false
    const mode = process.env.PAIR_MOCK_UPDATER
    return mode === '1' || mode === 'true' || mode === 'error'
}

function devMockUpdaterFailsCheck(): boolean {
    return process.env.PAIR_MOCK_UPDATER === 'error'
}

export function initializeDevMockUpdater(
    setStatus: StatusSetter,
    pendingDownloadedVersion: string | null
): void {
    if (!devMockUpdaterEnabled()) return

    log.info('[dev-mock-updater] Simulated updater enabled — no real downloads or installs')

    // Mirror the real updater's resume-on-start: if a marker names a version
    // other than the one running, jump straight to the install-ready state.
    if (pendingDownloadedVersion && pendingDownloadedVersion !== pkg.version) {
        log.info(
            `[dev-mock-updater] Restoring downloaded update ${pendingDownloadedVersion} from marker`
        )
        setStatus({
            phase: 'downloaded',
            currentVersion: pkg.version,
            latestVersion: pendingDownloadedVersion,
            downloadPercent: 100,
            error: null
        })
        return
    }

    setStatus({
        phase: 'idle',
        currentVersion: pkg.version,
        latestVersion: null,
        downloadPercent: null,
        error: null
    })
}

export async function devMockCheckForUpdates(
    setStatus: StatusSetter,
    pendingDownloadedVersion: string | null
): Promise<void> {
    clearDevMockTimers()
    setStatus({ phase: 'checking', error: null, downloadPercent: null })
    await delay(700)

    if (devMockUpdaterFailsCheck()) {
        log.error(`[dev-mock-updater] ${UPDATE_CHECK_ERROR_MESSAGE} (PAIR_MOCK_UPDATER=error)`)
        setStatus({
            phase: 'error',
            error: UPDATE_CHECK_ERROR_MESSAGE
        })
        return
    }

    const latest = bumpPatchVersion(pkg.version)
    // Mirror the real updater: a version already downloaded in a prior session
    // resumes straight to install-ready instead of prompting another download.
    if (pendingDownloadedVersion === latest) {
        setStatus({
            phase: 'downloaded',
            latestVersion: latest,
            downloadPercent: 100,
            error: null
        })
        return
    }
    setStatus({
        phase: 'available',
        latestVersion: latest,
        downloadPercent: null,
        error: null
    })
}

export async function devMockDownloadUpdate(setStatus: StatusSetter): Promise<void> {
    const latest = bumpPatchVersion(pkg.version)
    for (let percent = 0; percent <= 100; percent += 12) {
        setStatus({
            phase: 'downloading',
            latestVersion: latest,
            downloadPercent: Math.min(percent, 100)
        })
        await delay(180)
    }
    writePendingUpdateVersion(latest)
    setStatus({
        phase: 'downloaded',
        latestVersion: latest,
        downloadPercent: 100,
        error: null
    })
}

export function devMockQuitAndInstall(): void {
    // The running version never changes in dev, so consume the marker here to
    // simulate the install instead of relying on the next-start version check.
    clearPendingUpdate()
    log.info('[dev-mock-updater] Restart & install skipped in dev (not packaged)')
}

function clearDevMockTimers(): void {
    for (const id of timers) clearTimeout(id)
    timers = []
}
