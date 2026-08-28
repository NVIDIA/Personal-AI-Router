// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import ollamaModelsData from './ollama-models.json'

/**
 * Ollama-tags–shaped model entry. This is the wire shape the committed list
 * (`ollama-models.json`) stores and that `getEngineHubModels` maps to
 * `EngineHubModel`.
 *
 * PAIR no longer scrapes ollama.com at runtime — the live scrape was fragile
 * and broke whenever Ollama changed their markup. The list is now a **locked**
 * committed file regenerated on demand by `npm run scrape:ollama-models`
 * (`scripts/scrape-ollama-models.ts`); a dev reviews the diff and commits it.
 */
export interface OllamaTagsModel {
    name: string
    model: string
    modified_at: string
    size: number
    digest: string
    details: {
        parent_model: string
        format: string
        family: string
        families: string[] | null
        parameter_size: string
        quantization_level: string
    }
}

function isRecord(v: unknown): v is Record<string, unknown> {
    return v !== null && typeof v === 'object'
}

function readStr(o: Record<string, unknown>, key: string): string {
    const v = o[key]
    return typeof v === 'string' ? v : ''
}

function readStringArrayOrNull(o: Record<string, unknown>, key: string): string[] | null {
    const v = o[key]
    if (!Array.isArray(v)) return null
    const arr: string[] = []
    for (const item of v) {
        if (typeof item === 'string') arr.push(item)
    }
    return arr
}

/**
 * Normalize one committed entry into a fully-typed `OllamaTagsModel`, dropping
 * anything malformed. Reading through `unknown` keeps the loader type-safe
 * (no casts) and guards against a bad hand-edit ever reaching the renderer.
 */
function normalizeEntry(raw: unknown): OllamaTagsModel | null {
    if (!isRecord(raw)) return null
    const o = raw
    const rawName = readStr(o, 'name') || readStr(o, 'model')
    const name = rawName.trim()
    if (!name) return null
    const model = readStr(o, 'model') || name
    const sizeVal = o.size
    const size = typeof sizeVal === 'number' && sizeVal >= 0 ? sizeVal : 0
    const d: Record<string, unknown> = isRecord(o.details) ? o.details : {}
    return {
        name,
        model,
        modified_at: readStr(o, 'modified_at'),
        size,
        digest: readStr(o, 'digest'),
        details: {
            parent_model: readStr(d, 'parent_model'),
            format: readStr(d, 'format'),
            family: readStr(d, 'family'),
            families: readStringArrayOrNull(d, 'families'),
            parameter_size: readStr(d, 'parameter_size'),
            quantization_level: readStr(d, 'quantization_level')
        }
    }
}

function dedupeByName(models: OllamaTagsModel[]): OllamaTagsModel[] {
    const byName = new Map<string, OllamaTagsModel>()
    for (const m of models) byName.set(m.name, m)
    return Array.from(byName.values())
}

/**
 * The committed model list, normalized once at module load. `ollama-models.json`
 * is bundled into the main process (`resolveJsonModule` + inlined by
 * electron-vite), so there is no filesystem read or path resolution at runtime.
 */
const OLLAMA_MODELS: OllamaTagsModel[] = dedupeByName(
    (Array.isArray(ollamaModelsData.models) ? ollamaModelsData.models : [])
        .map(normalizeEntry)
        .filter((m): m is OllamaTagsModel => m != null)
)

/** The locked Ollama model list served to the model hub. */
export function loadOllamaModels(): OllamaTagsModel[] {
    return OLLAMA_MODELS
}
