// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * In-process fan-out of service push events. `emitBridgePush` already sends every
 * push to the renderer windows; it also republishes here so non-window consumers
 * can observe the exact same event stream without competing with or blocking the
 * UI.
 */
import { EventEmitter } from 'events'
import type { WsPushChannel, WsPushPayload } from '@/shared/types/ws-channels'

/** Discriminated union of every push event, keyed by `channel`. */
type BridgePushEvent = {
    [C in WsPushChannel]: { channel: C; payload: WsPushPayload<C> }
}[WsPushChannel]

const EVENT = 'push'
const emitter = new EventEmitter()
// Subscribers are unbounded; lift the default 10-listener warning cap.
emitter.setMaxListeners(0)

export function publishPush<C extends WsPushChannel>(channel: C, payload: WsPushPayload<C>): void {
    const event = { channel, payload }
    emitter.emit(EVENT, event)
}

export function subscribePush(listener: (event: BridgePushEvent) => void): () => void {
    emitter.on(EVENT, listener)
    return () => {
        emitter.off(EVENT, listener)
    }
}
