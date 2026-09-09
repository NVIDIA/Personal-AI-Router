// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { EngineType } from '@/shared/types/engines'
import type {
    EngineHubLookupResponse,
    EngineHubModel,
    EngineHubSearchResponse
} from '@/shared/types/engine-api'
import { loadOllamaModels, type OllamaTagsModel } from '@/electron/model-hub/ollama-library'
import { lookupOllamaModel } from '@/electron/model-hub/ollama-registry'
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
        // A namespaced name addresses that namespace's page; a bare one is a
        // first-party model under `library`.
        url: base.includes('/')
            ? `https://ollama.com/${base}`
            : `https://ollama.com/library/${base}`,
        size: m.size > 0 ? m.size : undefined,
        downloads: 0,
        likes: 0,
        // Empty means the source reported no date; the row omits its age.
        updatedAt: m.modified_at,
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
 * Resolve one exact model name {@link getEngineHubModels} did not return.
 * Ollama's list is a committed snapshot, so a model published since it was taken
 * is absent from the browse list even though the engine can pull it; this asks
 * Ollama's registry for that one name.
 *
 * A null model means "not available" for every cause alike — no lookup source
 * for the engine, no such model, or an unreachable registry. LM Studio has none
 * because its catalog is fetched live and never falls behind a release.
 */
export async function lookupEngineHubModel(
    engineType: EngineType,
    name: string
): Promise<EngineHubLookupResponse> {
    switch (engineType) {
        case 'ollama': {
            const found = await lookupOllamaModel(name)
            return { model: found ? ollamaToHubModel(found) : null }
        }
        default:
            return { model: null }
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
