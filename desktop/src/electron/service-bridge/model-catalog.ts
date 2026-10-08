// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { EngineType } from '@/shared/types/engines'
import type { EngineHubModel, EngineHubSearchResponse } from '@/shared/types/engine-api'
import { getModularSupervisor } from '@/electron/service-bridge/modular-supervisor'
import type { JsonObject, JsonValue } from '@/electron/service-bridge/json-rpc-subprocess'
import { createStructuredLogger } from '@/shared/utils/log'
import { MODULAR_CATALOG_CALL_TIMEOUT_MS } from '@/shared/constants/modular-runtime'
import { engineManagerName } from '@/shared/utils/engines'
import getErrorString from '@/shared/utils/get-error-string'

const log = createStructuredLogger('model-catalog')

/**
 * `JsonObject` is what the stdio plane already resolves a reply to, so narrowing
 * happens against that rather than `unknown` — the boundary is typed, it is only
 * the shape behind it that has to be checked.
 */
function isObject(v: JsonValue | undefined): v is JsonObject {
    return typeof v === 'object' && v !== null && !Array.isArray(v)
}

function readStr(o: JsonObject, key: string): string {
    const v = o[key]
    return typeof v === 'string' ? v : ''
}

function readNum(o: JsonObject, key: string): number {
    const v = o[key]
    return typeof v === 'number' ? v : 0
}

function readStringArray(o: JsonObject, key: string): string[] {
    const v = o[key]
    if (!Array.isArray(v)) return []
    return v.filter((s): s is string => typeof s === 'string')
}

/**
 * Normalize one backend catalog row, dropping any row with no pull-ready id
 * rather than offering a model that cannot be downloaded.
 */
function toHubModel(raw: JsonValue): EngineHubModel | null {
    if (!isObject(raw)) return null
    const id = readStr(raw, 'id') || readStr(raw, 'name')
    if (!id) return null
    const size = readNum(raw, 'size')
    const family = readStr(raw, 'family')
    const parameterSize = readStr(raw, 'parameterSize')
    return {
        id,
        name: readStr(raw, 'name') || id,
        author: readStr(raw, 'author'),
        url: readStr(raw, 'url'),
        size: size > 0 ? size : undefined,
        downloads: readNum(raw, 'downloads'),
        likes: readNum(raw, 'likes'),
        updatedAt: readStr(raw, 'updatedAt'),
        tags: readStringArray(raw, 'tags'),
        family: family || undefined,
        parameterSize: parameterSize || undefined
    }
}

/**
 * Serve an engine's model hub from the backend's `engine:catalog`.
 *
 * The catalog used to be assembled here in the main process: a committed Ollama
 * list bundled into this bundle, plus a live Hugging Face fetch. Both moved into
 * `nvpair-engine-manager` so the desktop app and the terminal interface serve
 * the same models from one implementation rather than each maintaining its own.
 *
 * An engine with no curated source is an error at the backend, reported here as
 * an empty hub so the modal shows its empty state rather than a failure the user
 * can do nothing about.
 *
 * A query is passed through for the backend to search with. Only a searchable
 * source uses it; the others return their whole list, which the modal filters.
 */
export async function getEngineHubModels(
    engineType: EngineType,
    query?: string
): Promise<EngineHubSearchResponse> {
    const engine = engineManagerName(engineType)
    const search = query?.trim()
    try {
        // No target machine is named. The hub only ever installs to this
        // machine, which is also the one answering, and omitting the target is
        // how the backend is told to filter for itself — operating system and
        // CPU both, since an Intel Mac cannot install what Apple Silicon can.
        const result = await getModularSupervisor().callProcess(
            'broker',
            'engine:catalog',
            search ? { engine, query: search } : { engine },
            MODULAR_CATALOG_CALL_TIMEOUT_MS
        )
        const rows = isObject(result) && Array.isArray(result.models) ? result.models : []
        const models = rows.map(toHubModel).filter((m): m is EngineHubModel => m !== null)
        log.verbose({
            sublevel: 'catalog',
            message: `${engineType} catalog: ${models.length} models`
        })
        return { models }
    } catch (err) {
        log.warn({
            sublevel: 'catalog',
            message: `${engineType} catalog fetch failed: ${getErrorString(err)}`
        })
        return { models: [] }
    }
}

/**
 * Warm the backend's catalog cache so the first modal open is instant. Only the
 * live-fetched sources benefit; the committed one is compiled in and costs
 * nothing. Fire-and-forget: failures are logged by the call itself and the modal
 * will simply fetch again.
 *
 * Called once the Overview renderer reports ready, deliberately not on service
 * connect: a network fetch started before the window has painted competes with
 * the renderer's own load, and a hanging one leaves an unpainted window behind.
 */
export function warmEngineHubs(): void {
    void getEngineHubModels('lm-studio')
    void getEngineHubModels('llama-cpp')
}
