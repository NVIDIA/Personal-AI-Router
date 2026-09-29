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

export const llamaCppCatalogCache = new LlamaCppCatalogCache()
