// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { BrowserWindow } from 'electron'
import type { ServiceBridgePushMessage } from '@/shared/types/service-bridge'
import type { WsPushChannel, WsPushPayload } from '@/shared/types/ws-channels'
import { publishPush } from './push-bus'

const PUSH_CHANNEL = 'service-bridge:push'

export function emitBridgePush<C extends WsPushChannel>(
    channel: C,
    payload: WsPushPayload<C>
): void {
    const message: ServiceBridgePushMessage<C> = { channel, payload }
    for (const window of BrowserWindow.getAllWindows()) {
        if (window.isDestroyed() || window.webContents.isDestroyed()) continue
        window.webContents.send(PUSH_CHANNEL, message)
    }
    // Mirror to in-process subscribers so non-window consumers see the same
    // reactive stream as the UI.
    publishPush(channel, payload)
}
