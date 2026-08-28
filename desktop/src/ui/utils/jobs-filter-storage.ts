// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { JobsFilterType } from '@/ui/types/types'
import { readOwnProperty } from '@/ui/utils/local-storage'

export const JOBS_FILTER_STORAGE_KEY = 'nvpair.jobs.filter'

export const DEFAULT_JOBS_FILTER: Record<JobsFilterType, boolean> = {
    active: true,
    completed: true,
    failed: true
}

const KEYS: JobsFilterType[] = ['active', 'completed', 'failed']

/** Merge stored booleans with defaults so missing/invalid keys do not break the UI. */
export function parseJobsFilterRecord(raw: unknown): Record<JobsFilterType, boolean> {
    const next = { ...DEFAULT_JOBS_FILTER }
    if (raw === null || typeof raw !== 'object') return next
    for (const k of KEYS) {
        const v = readOwnProperty(raw, k)
        if (typeof v === 'boolean') {
            next[k] = v
        }
    }
    return next
}
