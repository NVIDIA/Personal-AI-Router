// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { WorkloadEngineClasses, WorkloadStates } from '@/shared/constants/workloads'
import { EngineType } from '@/shared/types/engines'

export type WorkloadState = (typeof WorkloadStates)[number]

/** What a job's row says when no engine on this machine reports having run it. */
export type WorkloadEngineClass = (typeof WorkloadEngineClasses)[number]

/**
 * What served a job: one of the engines this build knows, or one of the two
 * classes a job gets when it cannot be attributed to a local engine. Use
 * `workloadEngineLabel` for display text — a class is not an engine, so it has
 * no entry in `EngineDisplayNames`.
 */
export type WorkloadEngine = EngineType | WorkloadEngineClass

export interface Workload {
    id: string
    model: string
    engine: WorkloadEngine
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
}
