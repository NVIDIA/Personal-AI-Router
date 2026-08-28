// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import log from 'electron-log'
import { showOverviewMessage } from '@/electron/window'
import { APP_DISPLAY_NAME } from '@/shared/constants/app'

let lastNotifiedVersion: string | null = null

export function notifyUpdateAvailable(version: string): void {
    if (lastNotifiedVersion === version) return
    lastNotifiedVersion = version

    showOverviewMessage({
        id: `update-available:${version}`,
        kind: 'update',
        title: 'Update available',
        body: `${APP_DISPLAY_NAME} ${version} is available. Open Settings to download.`,
        actionLabel: 'Show',
        action: 'open-update'
    })
    log.info(`[updater] Surfaced update-available message for ${version}`)
}
