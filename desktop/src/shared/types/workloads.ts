// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { WorkloadStates } from '@/shared/constants/workloads'
import { EngineType } from '@/shared/types/engines'

export type WorkloadState = (typeof WorkloadStates)[number]

export interface WorkloadRemoval {
    workloadId: string
    originatedFrom: string | null
    engine?: EngineType
    runId?: string
}

export interface Workload {
    id: string
    runId?: string
    model: string
    engine: EngineType
    state: WorkloadState
    /**
     * Owner/origin node of the workload — the node whose proxy received the
     * request. Together with engine, runId and id, this identifies one request
     * in the global catalog; proxy counters can repeat across nodes, engines
     * and proxy runs.
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
}
