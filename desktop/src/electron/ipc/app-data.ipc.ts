// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { safeHandle } from '@/electron/ipc/safe-handle'
import { getAppDataWipePlan, wipeAppDataAndRelaunch } from '@/electron/app-data-wipe-orchestrator'

export function registerAppDataIpc(): void {
    safeHandle('app:get-wipe-plan', async () => getAppDataWipePlan())
    safeHandle('app:wipe-data', async () => {
        await wipeAppDataAndRelaunch()
    })
}
