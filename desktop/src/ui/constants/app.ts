// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { WorkloadState } from '@/shared/types/workloads'

export const CONNECTIONS_WIDTH = 60

export const MAX_HISTORY_ITEMS = 30

export const stateOrder: Record<WorkloadState, number> = {
    running: 0,
    queued: 1,
    completed: 2,
    failed: 3,
    cancelled: 4
}

export const ContentBaseClass = 'grow w-full min-h-0 min-w-0'
