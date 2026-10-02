// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { EngineType } from '@/shared/types/engines'
import type { Workload } from '@/shared/types/workloads'
import type { WsPushPayload } from '@/shared/types/ws-channels'
import { workloadKey } from '@/shared/utils/workloads'
import { useWorkloadsStore } from '@/ui/stores/workloads.store'

/**
 * The job list keys its cards the way the Electron catalog does: by origin,
 * engine, proxy run and job number. A removal from the broker names only the
 * origin and number and retires every engine and run sharing them. A removal
 * Electron sends after dropping one job also names the engine and run, and must
 * leave that job's siblings on screen.
 *
 * Runs in the `node` unit project with a stubbed `window` and animation frame.
 */

type Removal = WsPushPayload<'workloads:remove'>
type Snapshot = Record<string, Workload>
type FrameCallback = (time: number) => void

const ORIGIN = 'uuid-store-origin'
const RUN = 'run-a'

let upsertCb: ((workload: Workload) => void) | null = null
let removeCb: ((removal: Removal) => void) | null = null
let frames: FrameCallback[] = []

function makeFakeWindow(getInitial: () => Promise<Snapshot>) {
    return {
        pairApi: {
            workloads: {
                getInitial,
                onUpsert: (cb: (workload: Workload) => void) => {
                    upsertCb = cb
                    return () => {}
                },
                onRemove: (cb: (removal: Removal) => void) => {
                    removeCb = cb
                    return () => {}
                }
            }
        }
    }
}

function job(engine: EngineType, id: string): Workload {
    return {
        id,
        model: 'model',
        engine,
        runId: RUN,
        state: 'running',
        originatedFrom: ORIGIN,
        createdAt: 1,
        startedAt: 1,
        completedAt: null,
        error: null,
        requesterId: null
    }
}

function snapshotOf(workloads: Workload[]): Snapshot {
    return Object.fromEntries(workloads.map(workload => [workloadKey(workload), workload]))
}

function runFrames(): void {
    const pending = frames
    frames = []
    for (const frame of pending) frame(0)
}

function shownKeys(): string[] {
    return Array.from(useWorkloadsStore.getState().workloads.keys()).sort()
}

const ollamaOne = job('ollama', '1')
const lmStudioOne = job('lm-studio', '1')
const ollamaTwo = job('ollama', '2')

beforeEach(() => {
    upsertCb = null
    removeCb = null
    frames = []
    useWorkloadsStore.setState({ workloads: new Map() })
    vi.stubGlobal('requestAnimationFrame', (cb: FrameCallback) => frames.push(cb))
    vi.stubGlobal('cancelAnimationFrame', () => {})
})

afterEach(() => {
    useWorkloadsStore.getState().cleanup()
    vi.unstubAllGlobals()
})

describe('job list identity', () => {
    it('shows an Ollama job and an LM Studio job with the same number as two cards', async () => {
        vi.stubGlobal(
            'window',
            makeFakeWindow(async () => ({}))
        )
        await useWorkloadsStore.getState().initialize()

        upsertCb?.(ollamaOne)
        upsertCb?.(lmStudioOne)
        runFrames()

        expect(shownKeys()).toEqual([workloadKey(ollamaOne), workloadKey(lmStudioOne)].sort())
    })

    it("drops every engine's job with that number on a broker removal", async () => {
        vi.stubGlobal(
            'window',
            makeFakeWindow(async () => snapshotOf([ollamaOne, lmStudioOne, ollamaTwo]))
        )
        await useWorkloadsStore.getState().initialize()

        removeCb?.({ workloadId: '1', originatedFrom: ORIGIN })
        runFrames()

        expect(shownKeys()).toEqual([workloadKey(ollamaTwo)])
    })

    it('drops only the named job on an exact removal', async () => {
        vi.stubGlobal(
            'window',
            makeFakeWindow(async () => snapshotOf([ollamaOne, lmStudioOne, ollamaTwo]))
        )
        await useWorkloadsStore.getState().initialize()

        removeCb?.({ workloadId: '1', originatedFrom: ORIGIN, engine: 'ollama', runId: RUN })
        runFrames()

        expect(shownKeys()).toEqual([workloadKey(lmStudioOne), workloadKey(ollamaTwo)].sort())
    })
})

describe('removals that arrive while the job list loads', () => {
    function deferredSnapshot(): { promise: Promise<Snapshot>; resolve: (s: Snapshot) => void } {
        let resolve: (snapshot: Snapshot) => void = () => {}
        const promise = new Promise<Snapshot>(done => {
            resolve = done
        })
        return { promise, resolve }
    }

    it('subtracts a broker removal from the snapshot', async () => {
        const snapshot = deferredSnapshot()
        vi.stubGlobal(
            'window',
            makeFakeWindow(() => snapshot.promise)
        )
        const initializing = useWorkloadsStore.getState().initialize()

        removeCb?.({ workloadId: '1', originatedFrom: ORIGIN })
        runFrames()
        snapshot.resolve(snapshotOf([ollamaOne, lmStudioOne, ollamaTwo]))
        await initializing

        expect(shownKeys()).toEqual([workloadKey(ollamaTwo)])
    })

    it("subtracts an exact removal from the snapshot and keeps the job's siblings", async () => {
        const snapshot = deferredSnapshot()
        vi.stubGlobal(
            'window',
            makeFakeWindow(() => snapshot.promise)
        )
        const initializing = useWorkloadsStore.getState().initialize()

        removeCb?.({ workloadId: '1', originatedFrom: ORIGIN, engine: 'ollama', runId: RUN })
        runFrames()
        snapshot.resolve(snapshotOf([ollamaOne, lmStudioOne, ollamaTwo]))
        await initializing

        expect(shownKeys()).toEqual([workloadKey(lmStudioOne), workloadKey(ollamaTwo)].sort())
    })
})

describe('pushes that arrive while the job list refreshes', () => {
    async function startRefresh(): Promise<{
        resolve: (s: Snapshot) => void
        refreshing: Promise<void>
    }> {
        vi.stubGlobal(
            'window',
            makeFakeWindow(async () => snapshotOf([ollamaOne, lmStudioOne]))
        )
        await useWorkloadsStore.getState().initialize()

        let resolve: (snapshot: Snapshot) => void = () => {}
        const promise = new Promise<Snapshot>(done => {
            resolve = done
        })
        vi.stubGlobal(
            'window',
            makeFakeWindow(() => promise)
        )
        return { resolve, refreshing: useWorkloadsStore.getState().refresh() }
    }

    it('keeps a removal that lands during the fetch', async () => {
        const { resolve, refreshing } = await startRefresh()

        removeCb?.({ workloadId: '1', originatedFrom: ORIGIN, engine: 'ollama', runId: RUN })
        runFrames()
        resolve(snapshotOf([ollamaOne, lmStudioOne]))
        await refreshing

        expect(shownKeys()).toEqual([workloadKey(lmStudioOne)])
    })

    it('keeps an upsert that lands during the fetch', async () => {
        const { resolve, refreshing } = await startRefresh()

        upsertCb?.(ollamaTwo)
        runFrames()
        resolve(snapshotOf([ollamaOne, lmStudioOne]))
        await refreshing

        expect(shownKeys()).toEqual(
            [workloadKey(ollamaOne), workloadKey(lmStudioOne), workloadKey(ollamaTwo)].sort()
        )
    })
})
