// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import axios, { isAxiosError } from 'axios'
import type { EngineHubModel } from '@/shared/types/engine-api'
import type { JsonValue } from '@/shared/types/json'
import getErrorString from '@/shared/utils/get-error-string'
import { createStructuredLogger } from '@/shared/utils/log'
import { normalizeLlamaCppHuggingFaceModels } from './llamacpp-huggingface'

const log = createStructuredLogger('llamacpp-catalog')
const HF_MODELS_API = 'https://huggingface.co/api/models'
const APPROVED_PUBLISHERS = ['ggml-org', 'bartowski', 'unsloth']
const CATALOG_LIMIT = 50
const CACHE_TTL_MS = 6 * 60 * 60 * 1000
const HTTP_TIMEOUT_MS = 20_000
const REQUEST_HEADERS: Record<string, string> = {
    'User-Agent': 'PAIR/1.0',
    Accept: 'application/json'
}

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

export function llamaCppPublisherRequestParams(author: string) {
    return {
        author,
        filter: 'gguf',
        sort: 'downloads',
        direction: -1,
        limit: CATALOG_LIMIT,
        full: true
    }
}

async function fetchPublisherModels(publisher: string): Promise<JsonValue> {
    const { data } = await axios.get<JsonValue>(HF_MODELS_API, {
        signal: AbortSignal.timeout(HTTP_TIMEOUT_MS),
        headers: REQUEST_HEADERS,
        params: llamaCppPublisherRequestParams(publisher)
    })
    return data
}

type FetchPublisherModels = (publisher: string) => Promise<JsonValue>

function deduplicateModels(batches: EngineHubModel[][]): EngineHubModel[] {
    const models = new Map<string, EngineHubModel>()
    for (const batch of batches) {
        for (const model of batch) models.set(model.id, model)
    }
    return Array.from(models.values())
}

export class LlamaCppCatalogCache {
    private models: EngineHubModel[] = []
    private lastFetch = 0
    private fetching = false
    private inflight: Promise<void> | null = null
    private readonly fetchPublisher: FetchPublisherModels

    constructor(fetchPublisher: FetchPublisherModels = fetchPublisherModels) {
        this.fetchPublisher = fetchPublisher
    }

    get isFetching(): boolean {
        return this.fetching
    }

    get size(): number {
        return this.models.length
    }

    list(): EngineHubModel[] {
        return this.models
    }

    refresh(): void {
        void this.fetchUpstream().catch(() => {})
    }

    async ensureLoaded(): Promise<void> {
        await this.fetchUpstream()
    }

    private async fetchUpstream(): Promise<void> {
        if (this.inflight) return this.inflight
        if (Date.now() - this.lastFetch < CACHE_TTL_MS && this.models.length > 0) return

        this.fetching = true
        this.inflight = this.runFetch().finally(() => {
            this.fetching = false
            this.inflight = null
        })
        return this.inflight
    }

    private async runFetch(): Promise<void> {
        try {
            const batches = await Promise.all(
                APPROVED_PUBLISHERS.map(async publisher =>
                    normalizeLlamaCppHuggingFaceModels(await this.fetchPublisher(publisher))
                )
            )
            const models = deduplicateModels(batches)
            if (models.length === 0) throw new Error('catalog contained no eligible models')

            this.models = models
            this.lastFetch = Date.now()
            log.info({
                sublevel: 'cache',
                message: `llama.cpp catalog fetch ok: ${models.length} models`
            })
        } catch (error) {
            const status = isAxiosError(error) ? error.response?.status : undefined
            const message = getErrorString(error) || 'catalog fetch failed'
            log.warn({
                sublevel: 'http',
                message: `llama.cpp catalog fetch failed: ${status ?? ''} ${message}`.trim()
            })
            if (this.models.length === 0) {
                throw new Error(`Unable to load llama.cpp model catalog: ${message}`)
            }
        }
    }
}
