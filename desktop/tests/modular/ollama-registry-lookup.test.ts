// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, describe, expect, it, vi } from 'vitest'
import axios, { AxiosError } from 'axios'
import { lookupOllamaModel } from '@/electron/model-hub/ollama-registry'

/**
 * Exact-name lookup against Ollama's registry, the fallback for a model the
 * committed catalog snapshot does not list. A lookup reads the manifest for the
 * download size, then the config blob it points at for the model's own
 * description of itself.
 *
 * `lookupOllamaModel` memoizes by name — including negative results — so every
 * case below uses a distinct model name and a cache hit never masks a behavior
 * under test. The one test that asserts caching does so explicitly.
 */

const CONFIG_DIGEST = 'sha256:455f34728c9b5dd9c69b6ba1dc93c0e69a1c3b2e4f5a6b7c8d9e0f1a2b3c4d5e'

/** An OCI manifest shaped the way registry.ollama.ai answers. */
const manifest = (modelLayerBytes: number, extraLayers: { mediaType: string }[] = []) => ({
    schemaVersion: 2,
    mediaType: 'application/vnd.docker.distribution.manifest.v2+json',
    config: {
        mediaType: 'application/vnd.docker.container.image.v1+json',
        digest: CONFIG_DIGEST,
        size: 487
    },
    layers: [
        {
            mediaType: 'application/vnd.ollama.image.model',
            digest: 'sha256:aa',
            size: modelLayerBytes
        },
        { mediaType: 'application/vnd.ollama.image.license', digest: 'sha256:cc', size: 11338 },
        ...extraLayers.map(l => ({ ...l, digest: 'sha256:dd', size: 1000 }))
    ]
})

/** The config blob Ollama writes, describing the model itself. */
const config = {
    model_format: 'gguf',
    model_family: 'qwen35',
    model_families: ['qwen35'],
    model_type: '27.3B',
    file_type: 'Q4_K_M'
}

/**
 * Answer by URL, the way the registry does: manifests carry sizes and the
 * config digest, blobs carry the model's description.
 */
const serve = (manifestBody: unknown, configBody: unknown = config) =>
    vi.spyOn(axios, 'get').mockImplementation(async (url: string) => {
        if (url.includes('/blobs/')) return { data: configBody }
        return { data: manifestBody }
    })

const notFound = (): AxiosError => {
    const err = new AxiosError('Request failed with status code 404')
    err.response = {
        status: 404,
        statusText: 'Not Found',
        data: 'not found',
        headers: {},
        config: { headers: new axios.AxiosHeaders() }
    }
    return err
}

afterEach(() => {
    vi.restoreAllMocks()
})

describe('lookupOllamaModel', () => {
    it('resolves a model and reports the total bytes the engine downloads', async () => {
        serve(manifest(4_920_738_944))

        const found = await lookupOllamaModel('resolve-test:8b')

        expect(found?.name).toBe('resolve-test:8b')
        // Every layer, not just the weights — that is what lands on disk.
        expect(found?.size).toBe(4_920_738_944 + 11338)
        // Ollama shows the manifest's config digest, shortened.
        expect(found?.digest).toBe('455f34728c9b')
        // Neither the manifest nor its headers carry a publication date; empty
        // means unknown so the row hides its age rather than inventing one.
        expect(found?.modified_at).toBe('')
    })

    it('describes the model from its config blob rather than from its tag', async () => {
        serve(manifest(1000))

        const found = await lookupOllamaModel('config-test:27b-coding-mxfp8')

        // A tag is a label the publisher chose; the config carries the figures.
        expect(found?.details.parameter_size).toBe('27.3B')
        expect(found?.details.quantization_level).toBe('Q4_K_M')
        expect(found?.details.format).toBe('gguf')
        expect(found?.details.families).toEqual(['qwen35'])
    })

    it('falls back to the tag when the config states no parameter size', async () => {
        // Ollama's MLX builds carry no `model_type`.
        serve(manifest(1000), { model_format: 'safetensors', file_type: 'nvfp4' })

        const found = await lookupOllamaModel('mlx-fallback-test:27b-mlx')

        expect(found?.details.parameter_size).toBe('27b-mlx')
        expect(found?.details.quantization_level).toBe('nvfp4')
    })

    it('reads vision off the projector layer, and claims no other capability', async () => {
        serve(manifest(1000, [{ mediaType: 'application/vnd.ollama.image.projector' }]))
        expect((await lookupOllamaModel('vision-test:27b'))?.details.family).toBe('vision')

        // Tools and thinking come from ollama.com's rendered page and have no
        // manifest equivalent, so a model without a projector claims nothing
        // rather than claiming something wrong.
        serve(manifest(1000, [{ mediaType: 'application/vnd.ollama.image.template' }]))
        expect((await lookupOllamaModel('no-vision-test:8b'))?.details.family).toBe('')
    })

    it('still resolves when the config blob cannot be read', async () => {
        vi.spyOn(axios, 'get').mockImplementation(async (url: string) => {
            if (url.includes('/blobs/')) throw new Error('blob unavailable')
            return { data: manifest(2048) }
        })

        // The size is the part that matters; the description is a bonus.
        const found = await lookupOllamaModel('no-config-test:8b')
        expect(found?.size).toBe(2048 + 11338)
        expect(found?.details.parameter_size).toBe('8b')
    })

    it('implies the library namespace and the latest tag, like `ollama pull`', async () => {
        const get = serve(manifest(1000))

        await lookupOllamaModel('bare-name-test')

        expect(get.mock.calls[0][0]).toBe(
            'https://registry.ollama.ai/v2/library/bare-name-test/manifests/latest'
        )
    })

    it('treats a dotted bare name as a model, not a registry hostname', async () => {
        const get = serve(manifest(1000))

        // Regression: the "dotted first segment is a hostname" rule that
        // declines `hf.co/...` was once applied to lone segments too, which
        // silently rejected most real Ollama names — `qwen3.8`, `llama3.1`,
        // `phi3.5`. A single segment is always a name under `library`.
        const found = await lookupOllamaModel('dotted.name.test')

        expect(found).not.toBeNull()
        expect(get.mock.calls[0][0]).toBe(
            'https://registry.ollama.ai/v2/library/dotted.name.test/manifests/latest'
        )
    })

    it('addresses a namespaced model without rewriting its namespace', async () => {
        const get = serve(manifest(1000))

        await lookupOllamaModel('some-user/namespaced-test:q4')

        expect(get.mock.calls[0][0]).toBe(
            'https://registry.ollama.ai/v2/some-user/namespaced-test/manifests/q4'
        )
    })

    it('resolves a missing model to null without raising', async () => {
        vi.spyOn(axios, 'get').mockRejectedValue(notFound())

        // A lookup is speculative, so "not there" is an answer, not an error.
        await expect(lookupOllamaModel('missing-model-test:1b')).resolves.toBeNull()
    })

    it('resolves to null when the registry cannot be reached', async () => {
        vi.spyOn(axios, 'get').mockRejectedValue(new Error('getaddrinfo ENOTFOUND'))

        await expect(lookupOllamaModel('offline-test:1b')).resolves.toBeNull()
    })

    it('declines a reference belonging to another registry', async () => {
        const get = vi.spyOn(axios, 'get')

        // A dot in the first of several segments is a registry hostname. This
        // registry cannot answer for it, and asking would report a real model
        // as missing.
        await expect(lookupOllamaModel('hf.co/some-user/some-repo:Q4_K_M')).resolves.toBeNull()
        expect(get).not.toHaveBeenCalled()
    })

    it('refuses a name that would steer the request URL', async () => {
        const get = vi.spyOn(axios, 'get')

        // The renderer screens queries before asking, but a lookup arrives over
        // IPC and this runs in main, so it screens its own input. Unchecked,
        // `foo/..` walks out of the namespace and `foo/bar#z` truncates the
        // path, both landing on an endpoint nobody asked for.
        for (const hostile of [
            'foo/..',
            'foo/bar?x=1',
            'foo/bar#z',
            'foo/bar:tag#z',
            '../etc',
            'foo//bar',
            'foo/bar:'
        ]) {
            await expect(lookupOllamaModel(hostile)).resolves.toBeNull()
        }
        expect(get).not.toHaveBeenCalled()
    })

    it('declines a manifest that reports no layers rather than showing a 0-byte model', async () => {
        serve({ schemaVersion: 2, layers: [] })

        await expect(lookupOllamaModel('empty-manifest-test:1b')).resolves.toBeNull()
    })

    it('caches both answers so typing past a resolved name does not refetch', async () => {
        const get = serve(manifest(2048))

        const first = await lookupOllamaModel('cache-test:8b')
        const second = await lookupOllamaModel('cache-test:8b')

        expect(second).toBe(first)
        // Manifest plus config blob for the first call, nothing for the second.
        expect(get).toHaveBeenCalledTimes(2)

        get.mockRejectedValue(notFound())
        await expect(lookupOllamaModel('cache-miss-test:8b')).resolves.toBeNull()
        await expect(lookupOllamaModel('cache-miss-test:8b')).resolves.toBeNull()
        // One more call for the first miss; the second is served from the cache.
        expect(get).toHaveBeenCalledTimes(3)
    })
})
