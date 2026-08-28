// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { Dispatch, SetStateAction } from 'react'
import { useLocalStorageState } from '@/ui/hooks/useLocalStorageState'
import {
    DEFAULT_JOBS_FILTER,
    JOBS_FILTER_STORAGE_KEY,
    parseJobsFilterRecord
} from '@/ui/utils/jobs-filter-storage'
import type { JobsFilterType } from '@/ui/types/types'

/**
 * Jobs filter toggles persisted in `localStorage` (`nvpair.jobs.filter`).
 */
export function useJobsFilterState(): [
    Record<JobsFilterType, boolean>,
    Dispatch<SetStateAction<Record<JobsFilterType, boolean>>>
] {
    return useLocalStorageState(JOBS_FILTER_STORAGE_KEY, DEFAULT_JOBS_FILTER, {
        parse: parseJobsFilterRecord
    })
}
