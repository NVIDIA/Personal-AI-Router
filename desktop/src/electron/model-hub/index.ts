// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { EngineType } from '@/shared/types/engines'
import type { EngineHubModel, EngineHubSearchResponse } from '@/shared/types/engine-api'
import { loadOllamaModels, type OllamaTagsModel } from '@/electron/model-hub/ollama-library'
import {
    lmStudioCatalogCache,
    type LmStudioCatalogModel
} from '@/electron/model-hub/lmstudio-catalog'
import { llamaCppCatalogCache } from './llamacpp-catalog'

function ollamaToHubModel(m: OllamaTagsModel): EngineHubModel {
    const base = m.name.includes(':') ? m.name.slice(0, m.name.indexOf(':')) : m.name
    return {
        id: m.name,
        name: m.name,
        author: '',
        url: `https://ollama.com/library/${base}`,
        size: m.size > 0 ? m.size : undefined,
        downloads: 0,
        likes: 0,
        updatedAt: m.modified_at || new Date().toISOString(),
        tags: [],
        family: m.details.family || undefined,
        parameterSize: m.details.parameter_size || undefined
    }
}

function lmStudioToHubModel(m: LmStudioCatalogModel): EngineHubModel {
    return {
        id: m.id,
        name: m.name,
        author: m.author,
        url: m.url,
        downloads: m.downloads,
        likes: m.likes,
        updatedAt: m.updatedAt,
        tags: m.tags
    }
}

/**
 * Serve an engine's model hub. Ollama uses a committed locked list. LM Studio
 * and llama.cpp await their cached live Hugging Face catalogs on a cold load.
 */
export async function getEngineHubModels(engineType: EngineType): Promise<EngineHubSearchResponse> {
    switch (engineType) {
        case 'ollama':
            return { models: loadOllamaModels().map(ollamaToHubModel) }
        case 'lm-studio':
            await lmStudioCatalogCache.ensureLoaded()
            return { models: lmStudioCatalogCache.list().map(lmStudioToHubModel) }
        case 'llama-cpp':
            await llamaCppCatalogCache.ensureLoaded()
            return { models: llamaCppCatalogCache.list() }
        default:
            return { models: [] }
    }
}

/**
 * Kick a background refresh of the live engine hub caches so the first modal
 * open is instant. The committed Ollama list needs no warming. Fire-and-forget;
 * failures are logged inside each cache.
 *
 * Called once the Overview renderer reports ready, deliberately not on service
 * connect: a network fetch started before the window has painted competes with
 * the renderer's own load, and a hanging one leaves an unpainted window behind.
 */
export function warmEngineHubs(): void {
    lmStudioCatalogCache.refresh()
    llamaCppCatalogCache.refresh()
}
