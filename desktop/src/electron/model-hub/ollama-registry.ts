// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import axios, { isAxiosError } from 'axios'
import { createStructuredLogger } from '@/shared/utils/log'
import getErrorString from '@/shared/utils/get-error-string'
import type { OllamaTagsModel } from '@/electron/model-hub/ollama-library'

const log = createStructuredLogger('ollama-registry')

/**
 * Resolve one Ollama model by exact name, for names the committed catalog
 * snapshot (`ollama-models.json`) does not carry.
 *
 * `registry.ollama.ai` speaks the OCI distribution API that `ollama pull` uses,
 * so this asks the same source the engine would. It only reads manifests: the
 * registry answers 404 for `/tags/list`, so enumerating a catalog still needs
 * the snapshot and a lookup is an exact-name check rather than a search.
 */

const REGISTRY_BASE = 'https://registry.ollama.ai/v2'
const USER_AGENT = 'PAIR/1.0'

/**
 * Bounds the whole request rather than just the response. `axios`'s `timeout` is
 * a socket timeout and does not start until a socket exists, so it misses a
 * wedged DNS resolver; an `AbortSignal` covers every phase. `lmstudio-catalog.ts`
 * carries the same reasoning at more length.
 */
const HTTP_TIMEOUT_MS = 8_000

/** What `ollama pull <name>` resolves a bare name to. */
const DEFAULT_TAG = 'latest'

/** Ollama's namespace for first-party models, implied by a bare name. */
const DEFAULT_NAMESPACE = 'library'

/**
 * Characters a repository segment or tag may contain. This is the only guard on
 * what reaches the request URL, so it has to hold on its own: the renderer runs
 * its own check before asking, but a lookup arrives over IPC and main-process
 * code does not treat the renderer as trusted. Without it, `foo/..` walks out of
 * the namespace and `foo/bar#z` truncates the path.
 */
const SAFE_SEGMENT = /^[a-zA-Z0-9][a-zA-Z0-9._-]*$/

/**
 * Positive and negative results both cached, because the hub looks names up as
 * the user types and would otherwise re-request on every keystroke past a
 * resolved name. Bounded so a long session cannot grow it without limit.
 */
const CACHE_LIMIT = 256
const cache = new Map<string, OllamaTagsModel | null>()

function remember(key: string, value: OllamaTagsModel | null): OllamaTagsModel | null {
    if (cache.size >= CACHE_LIMIT) {
        // Map iteration is insertion-ordered, so the first key is the oldest.
        const oldest = cache.keys().next()
        if (!oldest.done) cache.delete(oldest.value)
    }
    cache.set(key, value)
    return value
}

/**
 * Split `[namespace/]name[:tag]` into a registry path and tag, or null for a
 * reference this registry cannot answer.
 *
 * OCI clients read a dot in the first of *several* segments as a registry
 * hostname, which is how `hf.co/<user>/<repo>` is recognized as someone else's
 * registry and declined rather than reported as missing. That rule must not
 * reach a lone segment: bare model names legitimately carry dots — `qwen3.8`,
 * `llama3.1`, `phi3.5` — and are always first-party names under `library`.
 */
function parseReference(name: string): { path: string; tag: string } | null {
    const colon = name.lastIndexOf(':')
    const slash = name.lastIndexOf('/')
    // A colon ahead of the last slash belongs to a host:port, not a tag.
    const hasTag = colon > slash
    const repository = hasTag ? name.slice(0, colon) : name
    const tag = hasTag ? name.slice(colon + 1) : DEFAULT_TAG
    if (!SAFE_SEGMENT.test(tag)) return null

    const segments = repository.split('/')
    if (!segments.every(segment => SAFE_SEGMENT.test(segment))) return null
    if (segments.length === 1) return { path: `${DEFAULT_NAMESPACE}/${segments[0]}`, tag }
    if (segments.length > 2 || segments[0].includes('.')) return null
    return { path: repository, tag }
}

function isRecord(v: unknown): v is Record<string, unknown> {
    return v !== null && typeof v === 'object'
}

function manifestLayers(raw: unknown): Record<string, unknown>[] {
    if (!isRecord(raw) || !Array.isArray(raw.layers)) return []
    return raw.layers.filter(isRecord)
}

/**
 * Total bytes the engine downloads: every layer, not just the weights. The
 * others (template, params, license) are a few kilobytes each, but the sum is
 * what lands on disk.
 */
function totalLayerBytes(raw: unknown): number {
    let total = 0
    for (const layer of manifestLayers(raw)) {
        if (typeof layer.size === 'number' && layer.size > 0) total += layer.size
    }
    return total
}

/**
 * Capability chip for the row. `details.family` carries a capability rather than
 * a model family here, because that is what the catalog scraper puts there and
 * what the row renders.
 *
 * Vision is the only one a manifest proves: a multimodal model carries an extra
 * `projector` layer for its vision encoder and nothing else does. Tools,
 * thinking and embedding exist only on ollama.com's rendered page, so a model
 * with those and no projector gets no chip instead of a wrong one.
 */
function capabilityFromLayers(raw: unknown): string {
    const vision = manifestLayers(raw).some(
        layer => layer.mediaType === 'application/vnd.ollama.image.projector'
    )
    return vision ? 'vision' : ''
}

function configDigest(raw: unknown): string {
    if (!isRecord(raw) || !isRecord(raw.config)) return ''
    return typeof raw.config.digest === 'string' ? raw.config.digest : ''
}

/** The 12-character form Ollama shows in its own model lists. */
function shortenDigest(digest: string): string {
    return digest.replace(/^sha256:/, '').slice(0, 12)
}

/**
 * What the model says about itself, read from the config blob the manifest
 * points at. Worth the extra request because a tag like `27b-coding-mxfp8` is a
 * label the publisher chose, while these are the real figures.
 */
interface OllamaModelConfig {
    format: string
    families: string[] | null
    parameterSize: string
    quantizationLevel: string
}

/** Resolves to null on any failure; the row still renders, with less on it. */
async function fetchConfig(path: string, digest: string): Promise<OllamaModelConfig | null> {
    if (!digest) return null
    try {
        const { data } = await axios.get<unknown>(`${REGISTRY_BASE}/${path}/blobs/${digest}`, {
            headers: { 'User-Agent': USER_AGENT, Accept: 'application/json' },
            signal: AbortSignal.timeout(HTTP_TIMEOUT_MS)
        })
        if (!isRecord(data)) return null
        const families = Array.isArray(data.model_families)
            ? data.model_families.filter((f): f is string => typeof f === 'string')
            : []
        return {
            format: typeof data.model_format === 'string' ? data.model_format : '',
            families: families.length > 0 ? families : null,
            parameterSize: typeof data.model_type === 'string' ? data.model_type : '',
            quantizationLevel: typeof data.file_type === 'string' ? data.file_type : ''
        }
    } catch {
        return null
    }
}

/**
 * Look one exact model name up in Ollama's registry.
 *
 * Null covers every "not available" case alike — a reference this registry does
 * not serve, a model or tag that does not exist, a timeout, an offline machine.
 * None of them surface as an error: the lookup is speculative, and the user
 * asked to search rather than to make this request.
 */
export async function lookupOllamaModel(name: string): Promise<OllamaTagsModel | null> {
    const cached = cache.get(name)
    if (cached !== undefined) return cached

    const reference = parseReference(name)
    if (!reference) return remember(name, null)

    try {
        const { data } = await axios.get<unknown>(
            `${REGISTRY_BASE}/${reference.path}/manifests/${reference.tag}`,
            {
                headers: {
                    'User-Agent': USER_AGENT,
                    Accept: 'application/vnd.docker.distribution.manifest.v2+json'
                },
                signal: AbortSignal.timeout(HTTP_TIMEOUT_MS)
            }
        )
        const size = totalLayerBytes(data)
        if (size === 0) return remember(name, null)

        const digest = configDigest(data)
        const config = await fetchConfig(reference.path, digest)

        log.info({ sublevel: 'lookup', message: `resolved ${name} outside the catalog snapshot` })
        return remember(name, {
            name,
            model: name,
            // Neither the manifest nor its headers carry a publication date.
            // Empty means unknown, and the row omits its age rather than
            // claiming the model was updated just now.
            modified_at: '',
            size,
            digest: shortenDigest(digest),
            details: {
                parent_model: '',
                format: config?.format ?? '',
                family: capabilityFromLayers(data),
                families: config?.families ?? null,
                // Falls back to the tag, which is what the catalog shows here.
                parameter_size:
                    config?.parameterSize || (reference.tag === DEFAULT_TAG ? '' : reference.tag),
                quantization_level: config?.quantizationLevel ?? ''
            }
        })
    } catch (err) {
        const status = isAxiosError(err) ? err.response?.status : undefined
        // 404 is the ordinary answer for a name that does not exist.
        if (status !== 404) {
            log.warn({
                sublevel: 'http',
                message: `lookup for ${name} failed: ${status ?? ''} ${getErrorString(err)}`.trim()
            })
        }
        return remember(name, null)
    }
}
