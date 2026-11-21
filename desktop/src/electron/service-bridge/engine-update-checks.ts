// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { createStructuredLogger } from '@/shared/utils/log'
import getErrorString from '@/shared/utils/get-error-string'
import type { EngineType } from '@/shared/types/engines'
import type { JsonValue } from './json-rpc-subprocess'
import { getModularBridgeState } from './modular-state'

const log = createStructuredLogger('service-bridge')
const CHECK_INTERVAL_MS = 6 * 60 * 60 * 1000
const updateEngines: readonly EngineType[] = ['ollama', 'lm-studio', 'llama-cpp']

/** One timer per broker. Coalesce lifecycle changes independently for each engine. */
export function watchEngineUpdates(check: (engine: EngineType) => Promise<JsonValue | undefined>): {
    refresh: (engine?: EngineType) => Promise<void>
    stop: () => void
} {
    const state = getModularBridgeState()
    let active = true
    const checking = new Set<EngineType>()
    const requested = new Set<EngineType>()

    async function checkEngine(engine: EngineType): Promise<void> {
        requested.add(engine)
        if (checking.has(engine)) return
        checking.add(engine)
        try {
            while (active && requested.delete(engine)) {
                const nodeId = state.getSelfId()
                if (!nodeId || !state.supportsEngine(engine)) return
                try {
                    const result = await check(engine)
                    if (active && !requested.has(engine)) {
                        state.applyEngineUpdateCheck(nodeId, engine, result)
                    }
                } catch (error) {
                    log.verbose({
                        sublevel: 'engine-manager',
                        message: `${engine} update check unavailable: ${getErrorString(error)}`
                    })
                }
            }
        } finally {
            checking.delete(engine)
        }
    }

    async function refresh(engine?: EngineType): Promise<void> {
        if (engine) {
            if (updateEngines.includes(engine)) await checkEngine(engine)
        } else {
            await Promise.all(updateEngines.map(checkEngine))
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
