// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { safeHandle } from '@/electron/ipc/safe-handle'
import { handleServiceBridgeInvoke } from '@/electron/service-bridge/empty-handlers'

export function registerServiceBridgeIpc(): void {
    safeHandle('service-bridge:invoke', async (_event, payload) => {
        return handleServiceBridgeInvoke(payload.channel, payload.payload)
    })
}
