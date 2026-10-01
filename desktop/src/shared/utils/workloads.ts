// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { Workload } from '@/shared/types/workloads'
import type { WsPushPayload } from '@/shared/types/ws-channels'

type WorkloadIdentity = Pick<Workload, 'originatedFrom' | 'engine' | 'runId' | 'id'>

/**
 * Stable catalog key for a workload: `(originatedFrom, engine, runId, id)`, the
 * identity the broker's store keys by. Each engine facade numbers its jobs from
 * 1 and starts again in every proxy run, so an id alone collides across nodes,
 * engines and runs. The `\u0000` separator cannot appear in any part, so the
 * key is unambiguous.
 */
export function workloadKey(workload: WorkloadIdentity): string {
    return [workload.originatedFrom ?? '', workload.engine, workload.runId, workload.id].join(
        '\u0000'
    )
}

/**
 * Keys of the entries a `workloads:remove` push retires from a catalog keyed by
 * `workloadKey`. A removal from the broker names only the origin and id, and
 * retires every engine and run that shares them, as the broker's own
 * `Store.Remove` does. One Electron sends for a job it dropped from its own
 * catalog names all four parts and retires exactly that job.
 *
 * The exact form is a single lookup rather than a scan: Electron sends one per
 * entry when it empties a catalog that can hold the broker's whole history.
 */
export function workloadKeysRemovedBy(
    catalog: ReadonlyMap<string, WorkloadIdentity>,
    removal: WsPushPayload<'workloads:remove'>
): string[] {
    if ('engine' in removal) {
        const key = workloadKey({
            originatedFrom: removal.originatedFrom,
            engine: removal.engine,
            runId: removal.runId,
            id: removal.workloadId
        })
        return catalog.has(key) ? [key] : []
    }
    const origin = removal.originatedFrom ?? ''
    const keys: string[] = []
    for (const [key, workload] of catalog) {
        if (workload.id === removal.workloadId && (workload.originatedFrom ?? '') === origin) {
            keys.push(key)
        }
    }
    return keys
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
