// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { WorkloadStates } from '@/shared/constants/workloads'
import { EngineType } from '@/shared/types/engines'

export type WorkloadState = (typeof WorkloadStates)[number]

/**
 * The engine a workload ran on. PAIR's own engine facades produce the closed
 * `EngineType` union; a third-party producer reporting through the workload
 * manager's local ingress stamps an identifier of its own (`vllm`, `comfyui`,
 * …). The service layer treats `engine` as an opaque string and the broker
 * stores those workloads, so the desktop keeps the identifier instead of
 * dropping the card.
 */
export type WorkloadEngine = EngineType | (string & {})

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
