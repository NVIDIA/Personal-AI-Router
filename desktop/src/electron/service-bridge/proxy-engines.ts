// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { EngineManagerNames } from '@/shared/constants/engines'
import type { EngineType } from '@/shared/types/engines'
import { engineTypeFromManagerName } from '@/shared/utils/engines'

/**
 * Engines currently consumed through the broker's proxy plane. Keep this set
 * separate from the complete engine registry so an engine can be declared
 * before every proxy-state consumer is ready to handle it.
 */
export type ProxyEngine = Extract<EngineType, 'ollama' | 'lm-studio' | 'llama-cpp' | 'vllm'>
/** A proxy's source name: the engine-manager's name for the engine, plus -proxy. */
export type ProxyNodeSource = `${(typeof EngineManagerNames)[ProxyEngine]}-proxy`

const PROXY_SOURCE_SUFFIX = '-proxy'

export const PROXY_ENGINES: readonly ProxyEngine[] = ['ollama', 'lm-studio', 'llama-cpp', 'vllm']

export const PROXY_NODE_SOURCES: readonly ProxyNodeSource[] =
    PROXY_ENGINES.map(proxySourceForEngine)

export function isProxyEngine(engine: EngineType): engine is ProxyEngine {
    return PROXY_ENGINES.some(candidate => candidate === engine)
}

export function proxySourceForEngine(engine: ProxyEngine): ProxyNodeSource {
    return `${EngineManagerNames[engine]}${PROXY_SOURCE_SUFFIX}`
}

export function proxyEngineFromSource(source: ProxyNodeSource): ProxyEngine
export function proxyEngineFromSource(source: string): ProxyEngine | null
export function proxyEngineFromSource(source: string): ProxyEngine | null {
    if (!source.endsWith(PROXY_SOURCE_SUFFIX)) return null
    return proxyEngineFromManagerId(source.slice(0, -PROXY_SOURCE_SUFFIX.length))
}

export function proxyEngineFromManagerId(managerName: string): ProxyEngine | null {
    const engine = engineTypeFromManagerName(managerName)
    return engine !== null && isProxyEngine(engine) ? engine : null
}
