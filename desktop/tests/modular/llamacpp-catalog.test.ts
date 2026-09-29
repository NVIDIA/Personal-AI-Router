// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, describe, expect, it, vi } from 'vitest'
import { getEngineHubModels } from '@/electron/model-hub'
import { llamaCppCatalogCache } from '@/electron/model-hub/llamacpp-catalog'
import type { EngineHubModel } from '@/shared/types/engine-api'
import { isHubEntryDownloaded } from '@/ui/utils/match-downloaded-model'
import type { ModelItem } from '@/ui/types/engine-info'
import type { ModelEntry } from '@/ui/types/model-hub'

function modelEntry(id: string): ModelEntry {
    return {
        id,
        name: id,
        author: id.slice(0, id.indexOf('/')),
        url: `https://huggingface.co/${id.slice(0, id.lastIndexOf(':'))}`,
        updatedAt: new Date(0)
    }
}

function modelItem(name: string, downloaded = true): ModelItem {
    return {
        name,
        size: 0,
        downloaded,
        status: 'idle',
        parameterSize: '',
        quantization: '',
        family: '',
        digest: '',
        sizeVram: null,
        expiresAt: null,
        expiry: '10m',
        capabilities: []
    }
}

afterEach(() => {
    vi.restoreAllMocks()
})

describe('live llama.cpp catalog', () => {
    it('serves the warmed catalog without transforming pull keys', async () => {
        const models: EngineHubModel[] = [
            {
                id: 'owner/model:Q4_K_M',
                name: 'owner/model:Q4_K_M',
                author: 'owner',
                url: 'https://huggingface.co/owner/model',
                downloads: 10,
                likes: 2,
                updatedAt: '2026-09-20T04:11:57.000Z',
                tags: ['gguf', 'Q4_K_M']
            }
        ]
        const ensureLoaded = vi
            .spyOn(llamaCppCatalogCache, 'ensureLoaded')
            .mockResolvedValue(undefined)
        vi.spyOn(llamaCppCatalogCache, 'list').mockReturnValue(models)

        await expect(getEngineHubModels('llama-cpp')).resolves.toEqual({ models })
        expect(ensureLoaded).toHaveBeenCalledOnce()
    })

    it('hides only an exact downloaded router model id', () => {
        const id = 'owner/model:Q4_K_M'
        const entry = modelEntry(id)

        expect(isHubEntryDownloaded('llama-cpp', entry, [modelItem(id)])).toBe(true)
        expect(
            isHubEntryDownloaded('llama-cpp', entry, [modelItem(id.slice(0, id.lastIndexOf(':')))])
        ).toBe(false)
        expect(isHubEntryDownloaded('llama-cpp', entry, [modelItem(id, false)])).toBe(false)
    })
})
