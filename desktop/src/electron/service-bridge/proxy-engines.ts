// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { EngineManagerName, EngineType } from '@/shared/types/engines'

/**
 * Engines currently consumed through the broker's proxy plane. Keep this set
 * separate from the complete engine registry so an engine can be declared
 * before every proxy-state consumer is ready to handle it.
 */
export type ProxyEngine = Extract<EngineType, 'ollama' | 'lm-studio' | 'llama-cpp'>
export type ProxyNodeSource = 'ollama-proxy' | 'lmstudio-proxy' | 'llamacpp-proxy'

interface ProxyEngineIdentity {
    managerName: EngineManagerName
    source: ProxyNodeSource
}

const PROXY_ENGINE_IDENTITIES: Record<ProxyEngine, ProxyEngineIdentity> = {
    ollama: {
        managerName: 'ollama',
        source: 'ollama-proxy'
    },
    'lm-studio': {
        managerName: 'lmstudio',
        source: 'lmstudio-proxy'
    },
    'llama-cpp': {
        managerName: 'llamacpp',
        source: 'llamacpp-proxy'
    }
}

export const PROXY_ENGINES: readonly ProxyEngine[] = ['ollama', 'lm-studio', 'llama-cpp']

export const PROXY_NODE_SOURCES: readonly ProxyNodeSource[] = PROXY_ENGINES.map(
    engine => PROXY_ENGINE_IDENTITIES[engine].source
)

export function isProxyEngine(engine: EngineType): engine is ProxyEngine {
    return PROXY_ENGINES.some(candidate => candidate === engine)
}

export function proxySourceForEngine(engine: ProxyEngine): ProxyNodeSource {
    return PROXY_ENGINE_IDENTITIES[engine].source
}

export function proxyEngineFromSource(source: ProxyNodeSource): ProxyEngine
export function proxyEngineFromSource(source: string): ProxyEngine | null
export function proxyEngineFromSource(source: string): ProxyEngine | null {
    for (const engine of PROXY_ENGINES) {
        if (PROXY_ENGINE_IDENTITIES[engine].source === source) return engine
    }
    return null
}

export function proxyEngineFromManagerId(managerName: string): ProxyEngine | null {
    for (const engine of PROXY_ENGINES) {
        if (PROXY_ENGINE_IDENTITIES[engine].managerName === managerName) return engine
    }
    return null
}
