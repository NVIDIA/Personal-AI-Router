// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Display row for a model hub result. The Electron-main model-hub module
 * (`src/electron/model-hub/`) returns normalized `EngineHubModel` rows over the
 * `engine:search-hub` channel; `model-hub-search.ts` maps those into this
 * renderer-only display shape. `name`/`id` carry the pull-ready identifier the
 * engine's `pull_model` action expects.
 */
export interface ModelEntry {
    id: string
    name: string
    author: string
    url: string
    size?: number
    /**
     * Publication date, when the source reports one. Undefined for a model
     * resolved by exact-name lookup, since an OCI manifest carries no date and
     * the current time would give the row an age it never had.
     */
    updatedAt?: Date
    family?: string
    parameterSize?: string
}

/** Sort fields for the engine hub list (all sorting is client-side). */
export const SORT_FIELDS = ['lastModified', 'name', 'size'] as const

export type SORT_FIELD = (typeof SORT_FIELDS)[number]

export interface SortState {
    sort: SORT_FIELD
    sortDescending: boolean
    allDirections: {
        [key in SORT_FIELD]: boolean
    }
}
