// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { EngineType } from '@/shared/types/engines'
import type { EngineHubModel, EngineHubSearchResponse } from '@/shared/types/engine-api'
import { loadOllamaModels, type OllamaTagsModel } from '@/electron/model-hub/ollama-library'
import {
    lmStudioCatalogCache,
    type LmStudioCatalogModel
} from '@/electron/model-hub/lmstudio-catalog'

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
 * Serve an engine's model hub. Ollama is served from the committed, locked list
 * (`ollama-models.json`), so it returns instantly with no network access. LM
 * Studio still fetches its live `lmstudio-community` catalog and awaits a cold
 * cache's initial load. Engines without a hub return empty.
 */
export async function getEngineHubModels(engineType: EngineType): Promise<EngineHubSearchResponse> {
    switch (engineType) {
        case 'ollama':
            return { models: loadOllamaModels().map(ollamaToHubModel) }
        case 'lm-studio':
            await lmStudioCatalogCache.ensureLoaded()
            return { models: lmStudioCatalogCache.list().map(lmStudioToHubModel) }
        default:
            return { models: [] }
    }
}

/**
 * Kick a background refresh of the live engine hub caches so the first modal
 * open is instant. Ollama needs no warming (it is a committed static list);
 * only LM Studio fetches from the network. Fire-and-forget; failures are logged
 * inside each cache.
 *
 * Called once the Overview renderer reports ready, deliberately not on service
 * connect: a network fetch started before the window has painted competes with
 * the renderer's own load, and a hanging one leaves an unpainted window behind.
 */
export function warmEngineHubs(): void {
    lmStudioCatalogCache.refresh()
}
