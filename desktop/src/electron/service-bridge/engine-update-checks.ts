// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { createStructuredLogger } from '@/shared/utils/log'
import getErrorString from '@/shared/utils/get-error-string'
import type { EngineType } from '@/shared/types/engines'
import { EngineTypes } from '@/shared/constants/engines'
import type { JsonValue } from './json-rpc-subprocess'
import { getModularBridgeState } from './modular-state'

const log = createStructuredLogger('service-bridge')
const CHECK_INTERVAL_MS = 6 * 60 * 60 * 1000

/** One timer per broker; coalesce lifecycle changes for the same node and engine. */
export function watchEngineUpdates(
    check: (nodeId: string, engine: EngineType) => Promise<JsonValue | undefined>
): {
    refresh: (engine?: EngineType, nodeId?: string) => Promise<void>
    stop: () => void
} {
    const state = getModularBridgeState()
    let active = true
    const checking = new Set<string>()
    const requested = new Set<string>()

    async function checkEngine(nodeId: string, engine: EngineType): Promise<void> {
        const key = `${nodeId}:${engine}`
        requested.add(key)
        if (checking.has(key)) return
        checking.add(key)
        try {
            while (active && requested.delete(key)) {
                if (!state.canCheckEngineUpdate(nodeId, engine)) return
                try {
                    const result = await check(nodeId, engine)
                    if (active && !requested.has(key)) {
                        state.applyEngineUpdateCheck(nodeId, engine, result)
                    }
                } catch (error) {
                    log.verbose({
                        sublevel: 'engine-manager',
                        message: `${engine} update check on ${nodeId} unavailable: ${getErrorString(error)}`
                    })
                }
            }
        } finally {
            checking.delete(key)
        }
    }

    async function refresh(engine?: EngineType, nodeId?: string): Promise<void> {
        const selfId = state.getSelfId()
        if (!selfId) return
        if (engine) await checkEngine(nodeId ?? selfId, engine)
        else {
            const nodes = Array.from(new Set([selfId, ...state.getClusterPeerIds()]))
            await Promise.all(nodes.flatMap(id => EngineTypes.map(type => checkEngine(id, type))))
        }
    }

    const timer = setInterval(() => void refresh(), CHECK_INTERVAL_MS)
    state.setEngineUpdateRefresher(refresh)
    void refresh()
    return {
        refresh,
        stop(): void {
            if (!active) return
            active = false
            clearInterval(timer)
            state.setEngineUpdateRefresher(undefined)
            state.clearEngineUpdates()
        }
    }
}
