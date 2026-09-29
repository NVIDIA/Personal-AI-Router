// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, describe, expect, it, vi } from 'vitest'
import {
    LlamaCppCatalogCache,
    llamaCppPublisherRequestParams
} from '@/electron/model-hub/llamacpp-catalog'
import type { JsonValue } from '@/shared/types/json'

function hubModel(id: string): JsonValue {
    return {
        id,
        gated: false,
        private: false,
        lastModified: '2026-09-20T04:11:57.000Z',
        downloads: 120,
        likes: 8,
        tags: ['gguf', 'text-generation'],
        pipeline_tag: 'text-generation',
        siblings: [{ rfilename: 'model-Q4_K_M.gguf' }]
    }
}

function deferredJson() {
    let resolvePromise: (value: JsonValue) => void = () => {
        throw new Error('deferred JSON promise was not initialized')
    }
    const promise = new Promise<JsonValue>(resolve => {
        resolvePromise = resolve
    })
    return {
        promise,
        resolve: (value: JsonValue) => resolvePromise(value)
    }
}

afterEach(() => {
    vi.useRealTimers()
})

describe('llama.cpp populated catalog cache', () => {
    it('builds a bounded Hugging Face request for one publisher', () => {
        expect(llamaCppPublisherRequestParams('bartowski')).toEqual({
            author: 'bartowski',
            filter: 'gguf',
            sort: 'downloads',
            direction: -1,
            limit: 50,
            full: true
        })
    })

    it('loads and combines every approved publisher', async () => {
        const fetchPublisher = vi.fn<(publisher: string) => Promise<JsonValue>>()
        fetchPublisher.mockImplementation(async publisher => [
            hubModel(`${publisher}/Example-GGUF`)
        ])
        const cache = new LlamaCppCatalogCache(fetchPublisher)

        await cache.ensureLoaded()

        expect(fetchPublisher.mock.calls.map(([publisher]) => publisher)).toEqual([
            'ggml-org',
            'bartowski',
            'unsloth'
        ])
        expect(cache.list().map(model => model.id)).toEqual([
            'ggml-org/Example-GGUF:Q4_K_M',
            'bartowski/Example-GGUF:Q4_K_M',
            'unsloth/Example-GGUF:Q4_K_M'
        ])
        expect(cache.isFetching).toBe(false)
        expect(cache.size).toBe(3)
    })

    it('shares one in-flight refresh between callers', async () => {
        const deferred = deferredJson()
        const fetchPublisher = vi.fn<(publisher: string) => Promise<JsonValue>>()
        fetchPublisher.mockReturnValue(deferred.promise)
        const cache = new LlamaCppCatalogCache(fetchPublisher)

        const first = cache.ensureLoaded()
        const second = cache.ensureLoaded()

        expect(cache.isFetching).toBe(true)
        expect(fetchPublisher).toHaveBeenCalledTimes(3)
        deferred.resolve([hubModel('owner/Example-GGUF')])
        await Promise.all([first, second])

        expect(fetchPublisher).toHaveBeenCalledTimes(3)
        expect(cache.size).toBe(1)
    })

    it('keeps the last good catalog when a stale refresh fails', async () => {
        vi.useFakeTimers()
        vi.setSystemTime(new Date('2026-09-20T00:00:00.000Z'))
        const fetchPublisher = vi.fn<(publisher: string) => Promise<JsonValue>>()
        fetchPublisher.mockImplementation(async publisher => [
            hubModel(`${publisher}/Example-GGUF`)
        ])
        const cache = new LlamaCppCatalogCache(fetchPublisher)
        await cache.ensureLoaded()

        fetchPublisher.mockClear()
        fetchPublisher.mockRejectedValue(new Error('offline'))
        vi.setSystemTime(new Date('2026-09-20T07:00:00.000Z'))

        await expect(cache.ensureLoaded()).resolves.toBeUndefined()
        expect(fetchPublisher).toHaveBeenCalledTimes(3)
        expect(cache.size).toBe(3)
    })

    it('surfaces a cold fetch failure', async () => {
        const fetchPublisher = vi.fn<(publisher: string) => Promise<JsonValue>>()
        fetchPublisher.mockRejectedValue(new Error('offline'))
        const cache = new LlamaCppCatalogCache(fetchPublisher)

        await expect(cache.ensureLoaded()).rejects.toThrow(
            'Unable to load llama.cpp model catalog: offline'
        )
        expect(cache.list()).toEqual([])
    })
})
