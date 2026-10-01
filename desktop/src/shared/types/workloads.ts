// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { WorkloadStates } from '@/shared/constants/workloads'
import { EngineType } from '@/shared/types/engines'

export type WorkloadState = (typeof WorkloadStates)[number]

export interface Workload {
    id: string
    model: string
    engine: EngineType
    /**
     * The origin proxy process's run nonce. Job ids restart from 1 in every
     * run, so an id from an earlier run can repeat in a later one. Empty when
     * the origin does not report one.
     */
    runId: string
    state: WorkloadState
    /**
     * Owner/origin node of the workload — the node whose proxy received the
     * request. Part of the `(originatedFrom, engine, runId, id)` identity that
     * `workloadKey` and the broker's store share: each engine facade numbers
     * its jobs from 1, so ids collide across nodes, engines and runs.
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
