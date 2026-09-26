// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { Workload } from '@/shared/types/workloads'

/**
 * Stable catalog key for a workload.
 *
 * The backend's catalog is keyed by `(originatedFrom, engine, runId, id)`, but
 * clients retain all four fields so equal counters from different engines or
 * proxy runs never overwrite one another. A legacy removal without engine/run
 * identity removes the matching origin/id prefix, as the broker does. Targeted
 * removals retain exact identity.
 */
export function workloadKey(
    originatedFrom: string | null,
    id: string,
    engine?: string,
    runId?: string
): string {
    const prefix = `${originatedFrom ?? ''}\u0000${id}`
    return engine === undefined ? prefix : `${prefix}\u0000${engine}\u0000${runId ?? ''}`
}

/**
 * Which node a workload actually ran on, for UI attribution.
 *
 * `scheduledOn` is the node the scheduler routed the request to (where it
 * actually runs); `originatedFrom` is only where the request entered the
 * cluster. UI that means "which node is this job on" (per-node job badges, the
 * workload→node connection lines, the "ran on" label) attributes strictly to
 * `scheduledOn`. A `null` result means the workload has **not been scheduled
 * yet** (still queued) — it has no run-node, so it draws no connection line, is
 * counted on no node, and shows no "ran on" target. Deliberately does NOT fall
 * back to `originatedFrom`: the origin is where the request came from, not where
 * the job ran.
 */
export function workloadExecutionNodeId(workload: Pick<Workload, 'scheduledOn'>): string | null {
    return workload.scheduledOn ?? null
}
