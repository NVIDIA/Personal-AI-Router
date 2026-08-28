// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { ipcMain } from 'electron'
import { getGatewayAddress } from '@/electron/connector'
import type { BootstrapPayload } from '@/shared/types/ipc-channels'

/**
 * Synchronous bootstrap IPC channel retained for local HTTP helper callers.
 * The modular preload service transport does not need connection details, so
 * the connector returns `null`.
 */
export function registerBootstrapIpc(): void {
    ipcMain.on('bootstrap:get', event => {
        const payload: BootstrapPayload = getGatewayAddress()
        event.returnValue = payload
    })
}
