// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import axios, { isAxiosError } from 'axios'
import { createStructuredLogger } from '@/shared/utils/log'
import getErrorString from '@/shared/utils/get-error-string'
import { currentPlatform } from '@/shared/utils/platform'

const log = createStructuredLogger('lmstudio-catalog')

/**
 * LM Studio's "Discover" catalog is the `lmstudio-community` Hugging Face org:
 * curated GGUF quantizations whose repo ids (e.g.
 * `lmstudio-community/Qwen3-8B-GGUF`) are exactly the strings `lms get`
 * accepts on the pull path. There is no separate public LM Studio catalog
 * JSON API — the lmstudio.ai/models page is client-rendered and ultimately
 * resolves to these same Hugging Face repos.
 */
const HF_MODELS_API = 'https://huggingface.co/api/models'
const CATALOG_AUTHOR = 'lmstudio-community'
const CATALOG_LIMIT = 500
const CACHE_TTL_MS = 6 * 60 * 60 * 1000

/**
 * Bounds the whole request, not just the response. `axios`'s own `timeout`
 * maps to a socket timeout, which does not start until a socket exists — so it
 * does not cover name resolution. A machine whose DNS resolver is wedged has
 * been observed holding this request open for 74s against a 20s timeout, long
 * enough to matter to the rest of the main process. An `AbortSignal` covers
 * every phase, so this is the single bound on the call.
 */
const HTTP_TIMEOUT_MS = 20_000
const REQUEST_HEADERS: Record<string, string> = {
    'User-Agent': 'PAIR/1.0',
    Accept: 'application/json'
}

/**
 * One normalized catalog row. `id`/`name` carry the pull-ready repo id; the
 * renderer's `mapLmStudioHub` turns this into a display `ModelEntry`.
 */
export interface LmStudioCatalogModel {
    id: string
    name: string
    author: string
    downloads: number
    likes: number
    updatedAt: string
    tags: string[]
    url: string
}

function isRecord(v: unknown): v is Record<string, unknown> {
    return v !== null && typeof v === 'object'
}

function readStr(o: Record<string, unknown>, key: string): string {
    const v = o[key]
    return typeof v === 'string' ? v : ''
}

function readNum(o: Record<string, unknown>, key: string): number {
    const v = o[key]
    return typeof v === 'number' ? v : 0
}

function readStringArray(o: Record<string, unknown>, key: string): string[] {
    const v = o[key]
    if (!Array.isArray(v)) return []
    return v.filter((s): s is string => typeof s === 'string')
}

function normalizeEntry(raw: unknown): LmStudioCatalogModel | null {
    if (!isRecord(raw)) return null
    const id = (readStr(raw, 'id') || readStr(raw, 'modelId')).trim()
    if (!id) return null
    const author = id.includes('/') ? id.slice(0, id.indexOf('/')) : CATALOG_AUTHOR
    const updatedAt =
        readStr(raw, 'lastModified') || readStr(raw, 'createdAt') || new Date().toISOString()
    return {
        id,
        name: id,
        author,
        downloads: readNum(raw, 'downloads'),
        likes: readNum(raw, 'likes'),
        updatedAt,
        tags: readStringArray(raw, 'tags'),
        url: `https://huggingface.co/${id}`
    }
}

function dedupeById(models: LmStudioCatalogModel[]): LmStudioCatalogModel[] {
    const byId = new Map<string, LmStudioCatalogModel>()
    for (const m of models) byId.set(m.id, m)
    return Array.from(byId.values())
}

/**
 * MLX is Apple's framework: those quantizations only run on Apple Silicon.
 * `lms get` rejects them on Windows/Linux with "No download options available",
 * so listing them off-Mac only offers models that can never install. Detect via
 * the HF `mlx` tag or an `mlx` token in the repo id (e.g. `…-MLX-8bit`).
 */
function isMlxModel(m: LmStudioCatalogModel): boolean {
    if (m.tags.some(t => t.toLowerCase() === 'mlx')) return true
    return /(?:^|[-_/])mlx(?:[-_/]|$)/i.test(m.id)
}

/**
 * Drop Mac-only MLX repos on non-Mac platforms. On macOS the catalog keeps both
 * GGUF and MLX since either can run there.
 */
function filterByPlatform(models: LmStudioCatalogModel[]): LmStudioCatalogModel[] {
    if (currentPlatform() === 'darwin') return models
    return models.filter(m => !isMlxModel(m))
}

class LmStudioCatalogCache {
    private models: LmStudioCatalogModel[] = []
    private lastFetch = 0
    private fetching = false
    private inflight: Promise<void> | null = null

    get isFetching(): boolean {
        return this.fetching
    }

    get size(): number {
        return this.models.length
    }

    list(): LmStudioCatalogModel[] {
        return this.models
    }

    refresh(): void {
        log.info({
            sublevel: 'cache',
            message: `LM Studio catalog refresh requested (current=${this.models.length} fetching=${this.fetching})`
        })
        void this.fetchUpstream()
    }

    /** Ensure the catalog has been populated at least once. */
    async ensureLoaded(): Promise<void> {
        if (this.models.length > 0) return
        await this.fetchUpstream()
    }

    private async fetchUpstream(): Promise<void> {
        if (this.inflight) return this.inflight
        if (Date.now() - this.lastFetch < CACHE_TTL_MS && this.models.length > 0) {
            log.verbose({
                sublevel: 'cache',
                message: `cache fresh, skipping fetch (${this.models.length} models)`
            })
            return
        }
        this.fetching = true
        this.inflight = this.runFetch().finally(() => {
            this.fetching = false
            this.inflight = null
        })
        return this.inflight
    }

    private async runFetch(): Promise<void> {
        try {
            log.verbose({
                sublevel: 'http',
                message: `GET ${HF_MODELS_API}?author=${CATALOG_AUTHOR}`
            })
            const { data } = await axios.get<unknown>(HF_MODELS_API, {
                signal: AbortSignal.timeout(HTTP_TIMEOUT_MS),
                headers: REQUEST_HEADERS,
                params: {
                    author: CATALOG_AUTHOR,
                    sort: 'downloads',
                    direction: -1,
                    limit: CATALOG_LIMIT
                }
            })
            const raw = Array.isArray(data) ? data : []
            const normalized = dedupeById(
                raw.map(normalizeEntry).filter((m): m is LmStudioCatalogModel => m != null)
            )
            const platformModels = filterByPlatform(normalized)
            if (platformModels.length > 0) {
                this.models = platformModels
                this.lastFetch = Date.now()
                log.info({
                    sublevel: 'cache',
                    message: `LM Studio catalog fetch ok: ${platformModels.length} models (filtered ${
                        normalized.length - platformModels.length
                    } MLX for ${currentPlatform()})`
                })
                return
            }
            log.warn({
                sublevel: 'cache',
                message: 'LM Studio catalog fetch returned 0 models; keeping prior cache'
            })
        } catch (err) {
            const status = isAxiosError(err) ? err.response?.status : undefined
            const msg = getErrorString(err) || 'catalog fetch failed'
            log.warn({
                sublevel: 'http',
                message: `LM Studio catalog fetch failed: ${status ?? ''} ${msg}`.trim()
            })
        }
    }
}

export const lmStudioCatalogCache = new LmStudioCatalogCache()
