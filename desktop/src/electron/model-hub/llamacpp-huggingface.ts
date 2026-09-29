// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { EngineHubModel } from '@/shared/types/engine-api'
import type { JsonValue } from '@/shared/types/json'

const LLAMA_CPP_QUANTIZATION = 'Q4_K_M'
const SAFE_REPOSITORY_ID = /^[A-Za-z0-9][A-Za-z0-9._-]*\/[A-Za-z0-9][A-Za-z0-9._-]*$/
const SPLIT_GGUF_SUFFIX = /-([0-9]{5})-of-([0-9]{5})\.gguf$/i

interface JsonObject {
    [key: string]: JsonValue
}

function objectValue(value: JsonValue | undefined): JsonObject | null {
    if (
        value === null ||
        value === undefined ||
        Array.isArray(value) ||
        typeof value !== 'object'
    ) {
        return null
    }
    return value
}

function stringValue(object: JsonObject, key: string): string {
    const value = object[key]
    return typeof value === 'string' ? value : ''
}

function numberValue(object: JsonObject, key: string): number {
    const value = object[key]
    return typeof value === 'number' && Number.isFinite(value) && value >= 0 ? value : 0
}

function stringArrayValue(object: JsonObject, key: string): string[] {
    const value = object[key]
    if (!Array.isArray(value)) return []
    return value.filter((entry): entry is string => typeof entry === 'string')
}

function isGenerative(object: JsonObject, tags: string[]): boolean {
    const pipeline = stringValue(object, 'pipeline_tag').toLowerCase()
    if (
        pipeline === 'text-generation' ||
        pipeline === 'image-text-to-text' ||
        pipeline === 'any-to-any'
    ) {
        return true
    }
    return tags.some(tag => {
        const normalized = tag.toLowerCase()
        return normalized === 'text-generation' || normalized === 'conversational'
    })
}

function isPrimaryQuantizedGguf(path: string): boolean {
    if (!path.toLowerCase().endsWith('.gguf')) return false
    const fileName = path.slice(path.lastIndexOf('/') + 1).toLowerCase()
    if (fileName.includes('mmproj') || fileName.includes('imatrix') || fileName.includes('mtp-')) {
        return false
    }
    if (!/Q4_K_M[.-]/i.test(path)) return false
    const split = path.match(SPLIT_GGUF_SUFFIX)
    return split === null || split[1] === '00001'
}

function hasPullReadyArtifact(object: JsonObject): boolean {
    const siblings = object.siblings
    if (!Array.isArray(siblings)) return false
    return siblings.some(sibling => {
        const siblingObject = objectValue(sibling)
        return (
            siblingObject !== null &&
            isPrimaryQuantizedGguf(stringValue(siblingObject, 'rfilename'))
        )
    })
}

function normalizeModel(value: JsonValue): EngineHubModel | null {
    const object = objectValue(value)
    if (object === null) return null

    const repositoryId = (stringValue(object, 'id') || stringValue(object, 'modelId')).trim()
    if (!SAFE_REPOSITORY_ID.test(repositoryId)) return null
    if (object.private === true || object.gated !== false) return null

    const tags = stringArrayValue(object, 'tags')
    if (!tags.some(tag => tag.toLowerCase() === 'gguf')) return null
    if (!isGenerative(object, tags) || !hasPullReadyArtifact(object)) return null

    const updatedAt = stringValue(object, 'lastModified') || stringValue(object, 'createdAt')
    if (updatedAt.length === 0 || Number.isNaN(Date.parse(updatedAt))) return null

    const authorEnd = repositoryId.indexOf('/')
    const pullId = `${repositoryId}:${LLAMA_CPP_QUANTIZATION}`
    return {
        id: pullId,
        name: pullId,
        author: repositoryId.slice(0, authorEnd),
        url: `https://huggingface.co/${repositoryId}`,
        downloads: numberValue(object, 'downloads'),
        likes: numberValue(object, 'likes'),
        updatedAt,
        tags: Array.from(new Set([...tags, LLAMA_CPP_QUANTIZATION]))
    }
}

export function normalizeLlamaCppHuggingFaceModels(value: JsonValue): EngineHubModel[] {
    if (!Array.isArray(value)) return []
    const models = new Map<string, EngineHubModel>()
    for (const entry of value) {
        const model = normalizeModel(entry)
        if (model !== null) models.set(model.id, model)
    }
    return Array.from(models.values())
}
