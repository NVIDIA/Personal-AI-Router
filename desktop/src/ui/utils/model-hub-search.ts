// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { ModelEntry, SORT_FIELD } from '@/ui/types/model-hub'
import { EngineType } from '@/shared/types/engines'
import type { EngineHubModel } from '@/shared/types/engine-api'
import { EngineCapabilities } from '@/ui/constants/engine-capabilities'
import { EngineHubConfig } from '@/ui/types/engine-manifest'

export const sortFieldLabel = (sort: SORT_FIELD) => {
    switch (sort) {
        case 'lastModified':
            return 'Updated'
        case 'name':
            return 'Name'
        case 'size':
            return 'Size'
        default:
            return sort
    }
}

function getEngineHub(engine: EngineType): EngineHubConfig | undefined {
    return EngineCapabilities[engine]?.engineHub
}

/** Map a normalized hub row from the Electron-main module into a display entry. */
function hubModelToEntry(m: EngineHubModel): ModelEntry {
    return {
        id: m.id,
        name: m.name,
        author: m.author,
        url: m.url,
        size: m.size,
        updatedAt: m.updatedAt ? new Date(m.updatedAt) : new Date(),
        family: m.family,
        parameterSize: m.parameterSize
    }
}

/**
 * Fetch an engine's full model hub catalog from the service. The Electron-main
 * module owns the upstream source (Ollama library scrape, LM Studio community
 * catalog); the renderer filters/sorts the returned rows locally.
 */
export async function searchEngineHub(engine: EngineType): Promise<ModelEntry[]> {
    if (!getEngineHub(engine)) return []
    const { models } = await window.pairApi.engines.searchHub(engine)
    return models.map(hubModelToEntry)
}
