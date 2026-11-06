// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { WorkloadStates } from '@/shared/constants/workloads'
import { EngineType } from '@/shared/types/engines'

export type WorkloadState = (typeof WorkloadStates)[number]

export interface Workload {
    id: string
    model: string
    engine: EngineType
    state: WorkloadState
    /**
     * Owner/origin node of the workload — the node whose proxy received the
     * request. This is the identity half of the backend's `(originatedFrom, id)`
     * global catalog key (workload ids are a per-node proxy counter, so they
     * collide across nodes; `originatedFrom` disambiguates).
     */
    originatedFrom: string | null
    /**
     * Node the workload was routed to / scheduled on — where it actually ran,
     * as opposed to `originatedFrom` (where it came from). Optional/additive:
     * absent until the scheduler picks a target (e.g. a still-queued workload).
     * Use `workloadExecutionNodeId` for "which node is this job on" UI.
     */
    scheduledOn?: string | null
    createdAt: number
    startedAt: number | null
    completedAt: number | null
    error: string | null
    requesterId: string | null
    /**
     * Inference statistics for a finished job. Present only on the terminal
     * (`completed` / `failed`) transition, and only when the origin proxy could
     * measure something; see {@link WorkloadStats}.
     */
    stats?: WorkloadStats
}

/**
 * Inference statistics the origin proxy measures from the response body as it
 * streams to the client (services/shared/inferstats). Every field is optional:
 * absent means "not measured", never zero.
 */
export interface WorkloadStats {
    /** Prompt size in tokens, as reported by the engine. */
    promptTokens?: number
    /**
     * Tokens generated. Reported by the engine when its response carried a usage
     * object; otherwise counted from stream chunks, in which case `estimated` is
     * set.
     */
    completionTokens?: number
    /** Decode throughput: `completionTokens` over generation time, one decimal. */
    tokensPerSecond?: number
    /** Time to first token in ms: request start to the first response body byte. */
    ttftMs?: number
    /**
     * `completionTokens` (and so `tokensPerSecond`) were counted from stream
     * chunks rather than reported by the engine, so treat them as approximate.
     */
    estimated?: boolean
}
