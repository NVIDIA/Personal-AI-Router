// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { EngineType } from '@/shared/types/engines'
import { getLocalStorageJson, readOwnProperty, setLocalStorageJson } from '@/ui/utils/local-storage'
import type { SortState } from '@/ui/types/model-hub'
import { SORT_FIELDS } from '@/ui/types/model-hub'

/** Sort prefs for one engine's hub: `nvpair.modelHub.${engine}.sort`. */
function modelHubSortStorageKey(engine: EngineType): string {
    return `nvpair.modelHub.${engine}.sort`
}

const DEFAULT_ALL_DIRECTIONS: SortState['allDirections'] = {
    lastModified: false,
    name: false,
    size: false
}

function parseSortField(sortVal: string): SortState['sort'] | undefined {
    for (const f of SORT_FIELDS) {
        if (f === sortVal) return f
    }
    return undefined
}

function readOptionalBoolean(obj: object, key: string): boolean | undefined {
    const v = readOwnProperty(obj, key)
    if (typeof v !== 'boolean') return undefined
    return v
}

function parseStoredSortState(raw: unknown): SortState | undefined {
    if (raw === null || typeof raw !== 'object') return undefined
    const sortVal = readOwnProperty(raw, 'sort')
    if (typeof sortVal !== 'string') return undefined
    const sort = parseSortField(sortVal)
    if (sort === undefined) return undefined
    const sortDescending = readOwnProperty(raw, 'sortDescending')
    if (typeof sortDescending !== 'boolean') return undefined
    const adRaw = readOwnProperty(raw, 'allDirections')
    if (adRaw === null || typeof adRaw !== 'object') return undefined
    const allDirections: SortState['allDirections'] = { ...DEFAULT_ALL_DIRECTIONS }
    for (const k of SORT_FIELDS) {
        const b = readOptionalBoolean(adRaw, k)
        if (b !== undefined) {
            allDirections[k] = b
        }
    }
    return { sort, sortDescending, allDirections }
}

function readStoredSort(engine: EngineType): SortState | undefined {
    const parsed = getLocalStorageJson(modelHubSortStorageKey(engine))
    if (parsed === undefined) return undefined
    return parseStoredSortState(parsed)
}

export function writeStoredSort(engine: EngineType, sort: SortState): void {
    setLocalStorageJson(modelHubSortStorageKey(engine), sort)
}

export function resolveStoredSort(engine: EngineType, fallback: SortState): SortState {
    return readStoredSort(engine) ?? fallback
}
