// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { ipcRenderer } from 'electron'
import { invokeAndUnwrap } from '@/preload/api/unwrap'
import type {
    PreloadServiceTransport,
    ServiceBridgeInvokeRequest,
    ServiceBridgePushMessage
} from '@/shared/types/service-bridge'
import type {
    WsInvokeChannel,
    WsInvokeRequest,
    WsInvokeResponse,
    WsPushChannel
} from '@/shared/types/ws-channels'

const PUSH_CHANNEL = 'service-bridge:push'

type PushSubscriber = (message: ServiceBridgePushMessage) => void

const pushSubscribers = new Set<PushSubscriber>()
let pushListenerAttached = false

function isPushForChannel<C extends WsPushChannel>(
    message: ServiceBridgePushMessage,
    channel: C
): message is ServiceBridgePushMessage<C> {
    return message.channel === channel
}

function handleBridgePush(
    _event: Electron.IpcRendererEvent,
    message: ServiceBridgePushMessage
): void {
    for (const subscriber of Array.from(pushSubscribers)) {
        subscriber(message)
    }
}

function ensurePushListener(): void {
    if (pushListenerAttached) return
    ipcRenderer.on(PUSH_CHANNEL, handleBridgePush)
    pushListenerAttached = true
}

function removePushListenerIfIdle(): void {
    if (!pushListenerAttached || pushSubscribers.size > 0) return
    ipcRenderer.removeListener(PUSH_CHANNEL, handleBridgePush)
    pushListenerAttached = false
}

export const pairTransport: PreloadServiceTransport = {
    get connected() {
        return true
    },

    invoke<C extends WsInvokeChannel>(
        ...args: WsInvokeRequest<C> extends void
            ? [channel: C]
            : [channel: C, payload: WsInvokeRequest<C>]
    ): Promise<WsInvokeResponse<C>> {
        const [channel, payload] = args
        const request: ServiceBridgeInvokeRequest<C> = { channel, payload }
        return invokeAndUnwrap<WsInvokeResponse<C>>(
            ipcRenderer.invoke('service-bridge:invoke', request)
        )
    },

    subscribePush(channel, fn) {
        const subscriber: PushSubscriber = message => {
            if (!isPushForChannel(message, channel)) return
            fn(message.payload)
        }
        pushSubscribers.add(subscriber)
        ensurePushListener()
        return () => {
            pushSubscribers.delete(subscriber)
            removePushListenerIfIdle()
        }
    },

    onConnect(fn) {
        queueMicrotask(fn)
        return () => {}
    },

    onDisconnect() {
        return () => {}
    },

    onAuthFailure() {
        return () => {}
    },

    destroy() {
        pushSubscribers.clear()
        removePushListenerIfIdle()
    }
}
