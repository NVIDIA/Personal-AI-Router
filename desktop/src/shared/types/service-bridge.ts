// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type {
    WsInvokeChannel,
    WsInvokeRequest,
    WsPushChannel,
    WsPushPayload,
    WsInvokeResponse
} from '@/shared/types/ws-channels'

export interface ServiceBridgeInvokeRequest<C extends WsInvokeChannel = WsInvokeChannel> {
    channel: C
    payload?: WsInvokeRequest<C>
}

export interface ServiceBridgePushMessage<C extends WsPushChannel = WsPushChannel> {
    channel: C
    payload: WsPushPayload<C>
}

export type ServiceBridgePushHandler<C extends WsPushChannel> = (payload: WsPushPayload<C>) => void
export type ServiceBridgeLifecycleHandler = () => void

export interface PreloadServiceTransport {
    readonly connected: boolean
    invoke<C extends WsInvokeChannel>(
        ...args: WsInvokeRequest<C> extends void
            ? [channel: C]
            : [channel: C, payload: WsInvokeRequest<C>]
    ): Promise<WsInvokeResponse<C>>
    subscribePush<C extends WsPushChannel>(channel: C, fn: ServiceBridgePushHandler<C>): () => void
    onConnect(fn: ServiceBridgeLifecycleHandler): () => void
    onDisconnect(fn: ServiceBridgeLifecycleHandler): () => void
    onAuthFailure(fn: ServiceBridgeLifecycleHandler): () => void
    destroy(): void
}
