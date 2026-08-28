// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { safeHandle } from '@/electron/ipc/safe-handle'
import {
    checkForUpdates,
    downloadUpdate,
    getUpdateStatus,
    quitAndInstallUpdate
} from '@/electron/updater'

export function registerUpdateIpc(): void {
    safeHandle('update:get-status', async () => getUpdateStatus())

    safeHandle('update:check', async () => {
        await checkForUpdates()
    })

    safeHandle('update:download', async () => {
        await downloadUpdate()
    })

    safeHandle('update:install', async () => {
        await quitAndInstallUpdate()
    })
}
