// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { Workload, WorkloadEngine, WorkloadEngineClass } from '@/shared/types/workloads'
import { WorkloadEngineClassLabels } from '@/shared/constants/workloads'
import { EngineDisplayNames } from '@/shared/constants/engines'

/**
 * Stable catalog key for a workload.
 *
 * The backend's catalog is keyed by `(originatedFrom, engine, runId, id)`, but
 * the `workloads:remove` push carries only `(workloadId, originatedFrom)` — and
 * the broker's own `Store.Remove` drops every record matching that pair — so
 * `(originatedFrom, id)` is the only key a subscribe client can maintain
 * consistently across upsert and remove. Each node's proxy assigns workload ids
 * from its own monotonic counter, so ids collide across nodes; `originatedFrom`
 * (the origin node) disambiguates. Mirror that here so a remote node's job never
 * overwrites a local one that happens to share an id. The `\u0000` separator
 * cannot appear in a host id or proxy counter, so the key is unambiguous.
 */
export function workloadKey(originatedFrom: string | null, id: string): string {
    return `${originatedFrom ?? ''}\u0000${id}`
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

/** Runtime type guard — validates a string is a known `WorkloadEngineClass`. */
export function isWorkloadEngineClass(value: string): value is WorkloadEngineClass {
    return Object.hasOwn(WorkloadEngineClassLabels, value)
}

/**
 * What to call whatever served a job, for display only.
 *
 * A class is not an engine: `cluster` means the cluster router answered, from
 * whichever machine held the model, and `unknown` means no engine manager in
 * this build uses the name the worker reported. Neither has a brand mark, so
 * `EngineIcon` renders a routing glyph for them and this is the text that goes
 * with it. Never send the result anywhere — the wire name is on `Workload.engine`.
 */
export function workloadEngineLabel(engine: WorkloadEngine): string {
    if (isWorkloadEngineClass(engine)) return WorkloadEngineClassLabels[engine]
    return EngineDisplayNames[engine]
}
