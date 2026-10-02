// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { create } from 'zustand'
import { useShallow } from 'zustand/react/shallow'
import type { Workload } from '@/shared/types/workloads'
import type { WsPushPayload } from '@/shared/types/ws-channels'
import deepEqual from '@/ui/utils/deep-equal'
import {
    workloadExecutionNodeId,
    workloadKey,
    workloadKeysRemovedBy
} from '@/shared/utils/workloads'
import { stateOrder, MAX_HISTORY_ITEMS } from '@/ui/constants/app'

interface WorkloadsStore {
    workloads: Map<string, Workload>
    initialize: () => Promise<void>
    /** Re-fetch workloads from service (e.g. after state reset / leave cluster). */
    refresh: () => Promise<void>
    cleanup: () => void
}

let unsubs: Array<() => void> = []

// A single proxy request can emit a burst of upsert/remove pushes (e.g. a
// backend restart removes every catalog entry one by one, and concurrent
// inferences each fire their own lifecycle events). Applying each push as its
// own store commit clones the Map and re-runs every selector per event, which
// fans out into the job list, the connector lines, and node cards. We instead
// buffer pushes and flush them in a single commit on the next animation frame.
type PendingOp =
    | { kind: 'upsert'; workload: Workload }
    | { kind: 'remove'; removal: WsPushPayload<'workloads:remove'> }
let pendingOps: PendingOp[] = []
let flushHandle = 0

// Pushes that land during a snapshot fetch are newer than the snapshot, but the
// flush applies them to the map the snapshot then replaces. Each in-flight
// fetch records them here and replays them onto its snapshot, in arrival order,
// so a remove-then-readd of one job still surfaces the re-add.
const fetchLogs = new Set<PendingOp[]>()

function recordOp(op: PendingOp): void {
    for (const log of fetchLogs) log.push(op)
    pendingOps.push(op)
}

async function loadSnapshot(): Promise<Map<string, Workload>> {
    const log: PendingOp[] = []
    fetchLogs.add(log)
    try {
        const initial = await window.pairApi.workloads.getInitial()
        const map = new Map<string, Workload>(Object.entries(initial))
        for (const op of log) {
            if (op.kind === 'upsert') {
                map.set(workloadKey(op.workload), op.workload)
            } else {
                for (const key of workloadKeysRemovedBy(map, op.removal)) map.delete(key)
            }
        }
        return map
    } finally {
        fetchLogs.delete(log)
    }
}

export const useWorkloadsStore = create<WorkloadsStore>((set, get) => ({
    workloads: new Map(),

    initialize: async () => {
        const flush = () => {
            flushHandle = 0
            const ops = pendingOps
            pendingOps = []
            if (ops.length === 0) return

            const prev = get().workloads
            // Build the next Map lazily so a burst that nets no change (e.g. a
            // deep-equal upsert) never triggers a re-render.
            let updated: Map<string, Workload> | null = null
            for (const op of ops) {
                const base = updated ?? prev
                if (op.kind === 'upsert') {
                    const key = workloadKey(op.workload)
                    const existing = base.get(key)
                    if (existing && deepEqual(existing, op.workload)) continue
                    if (!updated) updated = new Map(prev)
                    updated.set(key, op.workload)
                } else {
                    const keys = workloadKeysRemovedBy(base, op.removal)
                    if (keys.length === 0) continue
                    if (!updated) updated = new Map(prev)
                    for (const key of keys) updated.delete(key)
                }
            }

            if (updated) set({ workloads: updated })
        }

        const schedule = () => {
            if (flushHandle) return
            flushHandle = requestAnimationFrame(flush)
        }

        // Subscribe BEFORE fetching the baseline so a push that fires during the
        // fetch is recorded for replay rather than dropped.
        if (window.pairApi) {
            unsubs.push(
                window.pairApi.workloads.onUpsert((workload: Workload) => {
                    recordOp({ kind: 'upsert', workload })
                    schedule()
                }),
                window.pairApi.workloads.onRemove(removal => {
                    recordOp({ kind: 'remove', removal })
                    schedule()
                })
            )
        }

        try {
            set({ workloads: await loadSnapshot() })
        } catch (error) {
            console.error('Failed to initialize workloads store:', error)
        }
    },

    refresh: async () => {
        try {
            set({ workloads: await loadSnapshot() })
        } catch (error) {
            console.error('Failed to refresh workloads store:', error)
        }
    },

    cleanup: () => {
        unsubs.forEach(u => u())
        unsubs = []
        if (flushHandle) {
            cancelAnimationFrame(flushHandle)
            flushHandle = 0
        }
        pendingOps = []
    }
}))

export function useActiveWorkloads(): Workload[] {
    return useWorkloadsStore(
        useShallow(state => {
            const result: Workload[] = []
            for (const w of state.workloads.values()) {
                if (w.state === 'running' || w.state === 'queued') result.push(w)
            }
            return result.sort((a, b) => {
                const stateDiff = stateOrder[a.state] - stateOrder[b.state]
                if (stateDiff !== 0) return stateDiff
                if (a.state === 'running' && b.state === 'running')
                    return (b.startedAt ?? 0) - (a.startedAt ?? 0)
                return b.createdAt - a.createdAt
            })
        })
    )
}

export function useAssignedWorkloads(): Workload[] {
    return useWorkloadsStore(
        useShallow(state => {
            const result: Workload[] = []
            for (const w of state.workloads.values()) {
                if (
                    (workloadExecutionNodeId(w) !== null && w.state === 'running') ||
                    w.state === 'queued'
                )
                    result.push(w)
            }
            return result.sort((a, b) => {
                const stateDiff = stateOrder[a.state] - stateOrder[b.state]
                if (stateDiff !== 0) return stateDiff
                return (a.startedAt ?? 0) - (b.startedAt ?? 0)
            })
        })
    )
}

// Number of active (running/queued) workloads scheduled on a node. Returns a
// primitive so a subscribing node card only re-renders when its own count
// changes, instead of on every workload event across the cluster.
export function useNodeActiveJobCount(nodeId: string): number {
    return useWorkloadsStore(state => {
        let count = 0
        for (const w of state.workloads.values()) {
            if (
                workloadExecutionNodeId(w) === nodeId &&
                (w.state === 'running' || w.state === 'queued')
            )
                count++
        }
        return count
    })
}

export function useCompletedWorkloads(): Workload[] {
    return useWorkloadsStore(
        useShallow(state => {
            const result: Workload[] = []
            for (const w of state.workloads.values()) {
                if (w.state === 'completed') result.push(w)
            }
            return result
                .sort((a, b) => (b.completedAt ?? 0) - (a.completedAt ?? 0))
                .slice(0, MAX_HISTORY_ITEMS)
        })
    )
}

export function useFailedWorkloads(): Workload[] {
    return useWorkloadsStore(
        useShallow(state => {
            const result: Workload[] = []
            for (const w of state.workloads.values()) {
                if (w.state === 'failed') result.push(w)
            }
            return result
                .sort((a, b) => (b.completedAt ?? 0) - (a.completedAt ?? 0))
                .slice(0, MAX_HISTORY_ITEMS)
        })
    )
}

// Jobs the requester stopped waiting for. Kept out of useFailedWorkloads so
// the failed bucket only holds outcomes a user might act on, rather than also
// collecting every time somebody pressed stop.
export function useCancelledWorkloads(): Workload[] {
    return useWorkloadsStore(
        useShallow(state => {
            const result: Workload[] = []
            for (const w of state.workloads.values()) {
                if (w.state === 'cancelled') result.push(w)
            }
            return result
                .sort((a, b) => (b.completedAt ?? 0) - (a.completedAt ?? 0))
                .slice(0, MAX_HISTORY_ITEMS)
        })
    )
}
