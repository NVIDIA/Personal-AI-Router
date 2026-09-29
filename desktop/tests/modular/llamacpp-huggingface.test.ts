// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import { normalizeLlamaCppHuggingFaceModels } from '@/electron/model-hub/llamacpp-huggingface'
import type { JsonValue } from '@/shared/types/json'

interface ModelOptions {
    gated?: boolean | string
    isPrivate?: boolean
    tags?: string[]
    pipelineTag?: string
    updatedAt?: string
}

function hubModel(id: string, files: string[], options: ModelOptions = {}): JsonValue {
    return {
        id,
        gated: options.gated ?? false,
        private: options.isPrivate ?? false,
        lastModified: options.updatedAt ?? '2026-09-20T04:11:57.000Z',
        downloads: 120,
        likes: 8,
        tags: options.tags ?? ['gguf', 'text-generation'],
        pipeline_tag: options.pipelineTag ?? 'text-generation',
        siblings: files.map(rfilename => ({ rfilename }))
    }
}

describe('llama.cpp Hugging Face normalization', () => {
    it('normalizes a public generative repository into an exact pull key', () => {
        const [model] = normalizeLlamaCppHuggingFaceModels([
            hubModel('owner/Model-GGUF', ['Model-Q4_K_M.gguf'])
        ])

        expect(model).toEqual({
            id: 'owner/Model-GGUF:Q4_K_M',
            name: 'owner/Model-GGUF:Q4_K_M',
            author: 'owner',
            url: 'https://huggingface.co/owner/Model-GGUF',
            downloads: 120,
            likes: 8,
            updatedAt: '2026-09-20T04:11:57.000Z',
            tags: ['gguf', 'text-generation', 'Q4_K_M']
        })
    })

    it('accepts the first shard of a conversational Q4_K_M model', () => {
        const models = normalizeLlamaCppHuggingFaceModels([
            hubModel(
                'org/Vision-Chat-GGUF',
                [
                    'Q4_K_M/Vision-Chat-Q4_K_M-00001-of-00003.gguf',
                    'Q4_K_M/Vision-Chat-Q4_K_M-00002-of-00003.gguf'
                ],
                { tags: ['gguf', 'conversational'], pipelineTag: 'image-text-to-text' }
            )
        ])

        expect(models.map(model => model.id)).toEqual(['org/Vision-Chat-GGUF:Q4_K_M'])
    })

    it('rejects unsafe, private, and gated repositories', () => {
        const models = normalizeLlamaCppHuggingFaceModels([
            hubModel('../owner/model', ['model-Q4_K_M.gguf']),
            hubModel('owner/model/extra', ['model-Q4_K_M.gguf']),
            hubModel('owner/private-model', ['model-Q4_K_M.gguf'], { isPrivate: true }),
            hubModel('owner/gated-model', ['model-Q4_K_M.gguf'], { gated: 'manual' })
        ])

        expect(models).toEqual([])
    })

    it('rejects helper-only, wrong-quantization, and incomplete split artifacts', () => {
        const models = normalizeLlamaCppHuggingFaceModels([
            hubModel('owner/mmproj', ['mmproj-model-Q4_K_M.gguf']),
            hubModel('owner/imatrix', ['model-imatrix-Q4_K_M.gguf']),
            hubModel('owner/mtp', ['model-mtp-Q4_K_M.gguf']),
            hubModel('owner/q8', ['model-Q8_0.gguf']),
            hubModel('owner/later-shard', ['model-Q4_K_M-00002-of-00003.gguf'])
        ])

        expect(models).toEqual([])
    })

    it('rejects non-GGUF, non-generative, and invalid-date metadata', () => {
        const models = normalizeLlamaCppHuggingFaceModels([
            hubModel('owner/no-gguf-tag', ['model-Q4_K_M.gguf'], {
                tags: ['text-generation']
            }),
            hubModel('owner/embedding', ['model-Q4_K_M.gguf'], {
                tags: ['gguf'],
                pipelineTag: 'feature-extraction'
            }),
            hubModel('owner/no-date', ['model-Q4_K_M.gguf'], {
                updatedAt: 'not-a-date'
            })
        ])

        expect(models).toEqual([])
    })

    it('deduplicates repeated repository metadata', () => {
        const models = normalizeLlamaCppHuggingFaceModels([
            hubModel('owner/model', ['model-Q4_K_M.gguf']),
            hubModel('owner/model', ['model-Q4_K_M.gguf'])
        ])

        expect(models).toHaveLength(1)
        expect(models[0]?.id).toBe('owner/model:Q4_K_M')
    })
})
