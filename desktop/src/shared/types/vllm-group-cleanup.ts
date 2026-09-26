// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { VllmGroupStatus } from '@/shared/types/vllm-group-status'

/**
 * Exact identity of the retained serving-group run a cleanup request is bound
 * to. It is derived from a freshly parsed status, never from renderer input.
 * PAIR answers a cleanup with its refreshed managed group status; unresolved
 * ranks are an error and the group stays held.
 */
export interface VllmGroupCleanupRequest {
    runId: string
    generation: number
    planDigest: string
}

/**
 * Serving-group status as the Desktop knows it for one host, including whether
 * the local platform can own a vLLM journal at all. A Windows or macOS host
 * cannot, so its unknown status must not hold remote Linux targets.
 */
export interface VllmGroupStatusView {
    known: boolean
    status: VllmGroupStatus | null
    localVllmSupported: boolean
}
