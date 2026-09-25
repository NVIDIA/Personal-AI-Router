// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { EngineHubModel } from '@/shared/types/engine-api'

interface LockedLlamaCppModel {
    id: string
    updatedAt: string
    family: string
    parameterSize: string
}

/**
 * Small, reviewed set of text-generation GGUF repositories. Each id is the
 * exact llama.cpp router pull key for the repository's Q4_K_M artifact.
 */
const LOCKED_LLAMA_CPP_MODELS: readonly LockedLlamaCppModel[] = [
    {
        id: 'ggml-org/gemma-3-1b-it-GGUF:Q4_K_M',
        updatedAt: '2025-03-12T10:30:05.000Z',
        family: 'Gemma 3',
        parameterSize: '1B'
    },
    {
        id: 'bartowski/Qwen2.5-3B-Instruct-GGUF:Q4_K_M',
        updatedAt: '2024-09-19T15:03:30.000Z',
        family: 'Qwen 2.5',
        parameterSize: '3B'
    },
    {
        id: 'bartowski/Llama-3.2-3B-Instruct-GGUF:Q4_K_M',
        updatedAt: '2024-10-08T14:01:10.000Z',
        family: 'Llama 3.2',
        parameterSize: '3B'
    },
    {
        id: 'unsloth/Qwen3-8B-GGUF:Q4_K_M',
        updatedAt: '2025-06-08T08:09:00.000Z',
        family: 'Qwen 3',
        parameterSize: '8B'
    }
]

const LLAMA_CPP_MODELS: EngineHubModel[] = LOCKED_LLAMA_CPP_MODELS.map(model => {
    const quantifierAt = model.id.lastIndexOf(':')
    const repoId = quantifierAt > 0 ? model.id.slice(0, quantifierAt) : model.id
    const authorAt = repoId.indexOf('/')
    return {
        ...model,
        name: model.id,
        author: authorAt > 0 ? repoId.slice(0, authorAt) : '',
        url: `https://huggingface.co/${repoId}`,
        downloads: 0,
        likes: 0,
        tags: ['gguf', 'text-generation', 'Q4_K_M']
    }
})

/** Return the locked llama.cpp starter catalog without network access. */
export function loadLlamaCppModels(): EngineHubModel[] {
    return LLAMA_CPP_MODELS
}
