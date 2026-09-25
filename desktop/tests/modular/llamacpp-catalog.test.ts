// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import { getEngineHubModels } from '@/electron/model-hub'
import { loadLlamaCppModels } from '@/electron/model-hub/llamacpp-catalog'
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

describe('locked llama.cpp catalog', () => {
    it('contains a small, unique, reviewed set of pull-ready models', () => {
        const models = loadLlamaCppModels()
        const publishers = new Set<string>()

        expect(models.length).toBeGreaterThanOrEqual(3)
        expect(models.length).toBeLessThanOrEqual(5)
        expect(new Set(models.map(model => model.id)).size).toBe(models.length)

        for (const model of models) {
            const repoId = model.id.slice(0, model.id.lastIndexOf(':'))
            publishers.add(model.author)
            expect(model.id).toMatch(/^(ggml-org|bartowski|unsloth)\/[^/:]+:Q4_K_M$/)
            expect(model.name).toBe(model.id)
            expect(model.author).toBe(repoId.slice(0, repoId.indexOf('/')))
            expect(model.url).toBe(`https://huggingface.co/${repoId}`)
            expect(model.tags).toContain('text-generation')
            expect(model.tags).toContain('Q4_K_M')
            expect(Number.isNaN(Date.parse(model.updatedAt))).toBe(false)
            expect(model.family).toBeTruthy()
            expect(model.parameterSize).toBeTruthy()
        }
        expect(publishers).toEqual(new Set(['ggml-org', 'bartowski', 'unsloth']))
    })

    it('serves the locked list through the engine hub without transformation', async () => {
        await expect(getEngineHubModels('llama-cpp')).resolves.toEqual({
            models: loadLlamaCppModels()
        })
    })

    it('hides only an exact downloaded router model id', () => {
        const id = loadLlamaCppModels()[0].id
        const entry = modelEntry(id)

        expect(isHubEntryDownloaded('llama-cpp', entry, [modelItem(id)])).toBe(true)
        expect(
            isHubEntryDownloaded('llama-cpp', entry, [modelItem(id.slice(0, id.lastIndexOf(':')))])
        ).toBe(false)
        expect(isHubEntryDownloaded('llama-cpp', entry, [modelItem(id, false)])).toBe(false)
    })
})
