// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { create } from 'zustand'
import { useShallow } from 'zustand/react/shallow'
import type { Workload } from '@/shared/types/workloads'
import deepEqual from '@/ui/utils/deep-equal'
import { workloadExecutionNodeId, workloadKey } from '@/shared/utils/workloads'
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
type PendingOp = { kind: 'upsert'; workload: Workload } | { kind: 'remove'; key: string }
let pendingOps: PendingOp[] = []
let flushHandle = 0

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
                    const key = workloadKey(op.workload.originatedFrom, op.workload.id)
                    const existing = base.get(key)
                    if (existing && deepEqual(existing, op.workload)) continue
                    if (!updated) updated = new Map(prev)
                    updated.set(key, op.workload)
                } else {
                    if (!base.has(op.key)) continue
                    if (!updated) updated = new Map(prev)
                    updated.delete(op.key)
                }
            }

            if (updated) set({ workloads: updated })
        }

        const schedule = () => {
            if (flushHandle) return
            flushHandle = requestAnimationFrame(flush)
        }

        // A `workloads:remove` that lands (and flushes) during the baseline fetch
        // below would be silently undone by rebuilding the map from the baseline
        // snapshot, which still lists the now-retired job. Record such removes so
        // the merge can subtract them — these deltas are newer than the snapshot.
        const removedDuringInit = new Set<string>()
        let initializing = true

        // Subscribe BEFORE fetching the baseline so a `workloads:upsert` that
        // fires during the fetch is buffered rather than dropped. Any such push
        // is applied to the store map by the rAF flush; the baseline merge below
        // then overlays those (newer) entries on top so a live transition is
        // never clobbered by the older snapshot.
        if (window.pairApi) {
            unsubs.push(
                window.pairApi.workloads.onUpsert((workload: Workload) => {
                    pendingOps.push({ kind: 'upsert', workload })
                    schedule()
                }),
                window.pairApi.workloads.onRemove(({ workloadId, originatedFrom }) => {
                    const key = workloadKey(originatedFrom, workloadId)
                    if (initializing) removedDuringInit.add(key)
                    pendingOps.push({ kind: 'remove', key })
                    schedule()
                })
            )
        }

        try {
            const initial = await window.pairApi.workloads.getInitial()
            const map = new Map<string, Workload>()
            for (const [key, workload] of Object.entries(initial)) {
                map.set(key, workload)
            }
            // Subtract jobs a `workloads:remove` retired during the fetch (that
            // delta is newer than the snapshot), before overlaying upserts — so a
            // remove-then-readd of the same key still surfaces the re-add.
            for (const key of removedDuringInit) map.delete(key)
            // Overlay upserts that already landed during the fetch (also newer than
            // the baseline), so seeding never regresses a live transition.
            for (const [key, workload] of get().workloads) {
                map.set(key, workload)
            }
            set({ workloads: map })
        } catch (error) {
            console.error('Failed to initialize workloads store:', error)
        } finally {
            initializing = false
        }
    },

    refresh: async () => {
        try {
            const initial = await window.pairApi.workloads.getInitial()
            const map = new Map<string, Workload>()
            for (const [key, workload] of Object.entries(initial)) {
                map.set(key, workload)
            }
            set({ workloads: map })
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
                if (w.state === 'running' || w.state === 'queued' || w.state === 'initializing')
                    result.push(w)
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

// Number of active (running/queued/initializing) workloads scheduled on a node.
// Returns a primitive so a subscribing node card only re-renders when its own
// count changes, instead of on every workload event across the cluster.
export function useNodeActiveJobCount(nodeId: string): number {
    return useWorkloadsStore(state => {
        let count = 0
        for (const w of state.workloads.values()) {
            if (
                workloadExecutionNodeId(w) === nodeId &&
                (w.state === 'running' || w.state === 'queued' || w.state === 'initializing')
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
