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
const SEARCH_CACHE_LIMIT = 20
const SEARCH_QUERY_LIMIT = 100
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

export function llamaCppSearchRequestParams(search: string) {
    return {
        search,
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

async function fetchSearchModels(search: string): Promise<JsonValue> {
    const { data } = await axios.get<JsonValue>(HF_MODELS_API, {
        signal: AbortSignal.timeout(HTTP_TIMEOUT_MS),
        headers: REQUEST_HEADERS,
        params: llamaCppSearchRequestParams(search)
    })
    return data
}

type FetchPublisherModels = (publisher: string) => Promise<JsonValue>
type FetchSearchModels = (search: string) => Promise<JsonValue>

interface SearchCacheEntry {
    models: EngineHubModel[]
    fetchedAt: number
}

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
    private readonly searchCache = new Map<string, SearchCacheEntry>()
    private readonly searchInflight = new Map<string, Promise<EngineHubModel[]>>()
    private readonly fetchPublisher: FetchPublisherModels
    private readonly fetchSearch: FetchSearchModels

    constructor(
        fetchPublisher: FetchPublisherModels = fetchPublisherModels,
        fetchSearch: FetchSearchModels = fetchSearchModels
    ) {
        this.fetchPublisher = fetchPublisher
        this.fetchSearch = fetchSearch
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

    async search(rawQuery: string): Promise<EngineHubModel[]> {
        const query = rawQuery.trim().slice(0, SEARCH_QUERY_LIMIT)
        if (query.length === 0) return []

        const key = query.toLowerCase()
        const cached = this.searchCache.get(key)
        if (cached && Date.now() - cached.fetchedAt < CACHE_TTL_MS) {
            this.searchCache.delete(key)
            this.searchCache.set(key, cached)
            return cached.models
        }

        const existing = this.searchInflight.get(key)
        if (existing) return existing

        const request = this.runSearch(query, key, cached).finally(() => {
            this.searchInflight.delete(key)
        })
        this.searchInflight.set(key, request)
        return request
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

    private async runSearch(
        query: string,
        key: string,
        fallback: SearchCacheEntry | undefined
    ): Promise<EngineHubModel[]> {
        try {
            const models = normalizeLlamaCppHuggingFaceModels(await this.fetchSearch(query))
            this.rememberSearch(key, models)
            log.verbose({
                sublevel: 'search',
                message: `llama.cpp catalog search ok: ${models.length} models`
            })
            return models
        } catch (error) {
            const status = isAxiosError(error) ? error.response?.status : undefined
            const message = getErrorString(error) || 'catalog search failed'
            log.warn({
                sublevel: 'search',
                message: `llama.cpp catalog search failed: ${status ?? ''} ${message}`.trim()
            })
            if (fallback) return fallback.models
            throw new Error(`Unable to search llama.cpp models: ${message}`)
        }
    }

    private rememberSearch(key: string, models: EngineHubModel[]): void {
        this.searchCache.delete(key)
        this.searchCache.set(key, { models, fetchedAt: Date.now() })
        while (this.searchCache.size > SEARCH_CACHE_LIMIT) {
            const oldestKey = this.searchCache.keys().next().value
            if (oldestKey === undefined) return
            this.searchCache.delete(oldestKey)
        }
    }
}

export const llamaCppCatalogCache = new LlamaCppCatalogCache()
