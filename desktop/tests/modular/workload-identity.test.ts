// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import {
    getModularBridgeState,
    parseWorkloadsInitial
} from '@/electron/service-bridge/modular-state'
import { subscribePush } from '@/electron/service-bridge/push-bus'
import type { EngineType } from '@/shared/types/engines'
import type { Workload, WorkloadState } from '@/shared/types/workloads'
import type { WsPushPayload } from '@/shared/types/ws-channels'
import { engineManagerName } from '@/shared/utils/engines'
import { workloadKey, workloadKeysRemovedBy } from '@/shared/utils/workloads'

/**
 * Each engine facade numbers its jobs from 1 and starts again in every proxy
 * run, so a job number alone is not an identity. While the catalog keyed jobs
 * by origin and number only, an Ollama job and an LM Studio job with the same
 * number shared one entry, and a new job overwrote one from before a restart
 * that reused its number.
 */

type Removal = WsPushPayload<'workloads:remove'>

const ORIGIN = 'uuid-identity-origin'
const PEER = 'uuid-identity-peer'
const RUN = 'run-a'

interface UpsertFields {
    engine: EngineType
    runId: string
    id: string
    state?: WorkloadState
    scheduledOn?: string
}

function brokerUpsert({
    engine,
    runId,
    id,
    state = 'running',
    scheduledOn = ORIGIN
}: UpsertFields): void {
    getModularBridgeState().upsertWorkloadFromInfo({
        workloadInfo: {
            id,
            engine: engineManagerName(engine),
            runId,
            state,
            model: 'model',
            originatedFrom: ORIGIN,
            scheduledOn,
            createdAt: 1
        }
    })
}

function keyOf(engine: EngineType, runId: string, id: string): string {
    return workloadKey({ originatedFrom: ORIGIN, engine, runId, id })
}

function catalogKeys(): string[] {
    return Object.keys(getModularBridgeState().getWorkloads()).sort()
}

let removals: Removal[] = []
let unsubscribe: () => void = () => {}

beforeEach(() => {
    getModularBridgeState().clearWorkloads()
    removals = []
    unsubscribe = subscribePush(event => {
        if (event.channel === 'workloads:remove') removals.push(event.payload)
    })
})

afterEach(() => {
    unsubscribe()
})

describe('workload identity in the Electron catalog', () => {
    it('keeps jobs with the same number on different engines apart', () => {
        brokerUpsert({ engine: 'ollama', runId: RUN, id: '1' })
        brokerUpsert({ engine: 'lm-studio', runId: RUN, id: '1' })

        expect(catalogKeys()).toEqual(
            [keyOf('ollama', RUN, '1'), keyOf('lm-studio', RUN, '1')].sort()
        )
    })

    it('keeps a job from an earlier proxy run apart from a new job reusing its number', () => {
        brokerUpsert({ engine: 'ollama', runId: 'run-earlier', id: '1', state: 'completed' })
        brokerUpsert({ engine: 'ollama', runId: RUN, id: '1', state: 'queued' })

        const catalog = getModularBridgeState().getWorkloads()
        expect(catalog[keyOf('ollama', 'run-earlier', '1')]).toMatchObject({ state: 'completed' })
        expect(catalog[keyOf('ollama', RUN, '1')]).toMatchObject({ state: 'queued' })
    })

    it('retires every engine and run sharing the origin and number on a broker removal', () => {
        brokerUpsert({ engine: 'ollama', runId: RUN, id: '1' })
        brokerUpsert({ engine: 'lm-studio', runId: RUN, id: '1' })
        brokerUpsert({ engine: 'ollama', runId: 'run-earlier', id: '1' })
        brokerUpsert({ engine: 'ollama', runId: RUN, id: '2' })

        getModularBridgeState().removeWorkloadFromParams({
            workloadId: '1',
            originatedFrom: ORIGIN
        })

        expect(catalogKeys()).toEqual([keyOf('ollama', RUN, '2')])
        expect(removals).toEqual([{ workloadId: '1', originatedFrom: ORIGIN }])
    })

    it('forwards a broker removal for a job it has not seen', () => {
        // The renderer may hold this job from a baseline Electron never seeded.
        getModularBridgeState().removeWorkloadFromParams({
            workloadId: '9',
            originatedFrom: ORIGIN
        })

        expect(removals).toEqual([{ workloadId: '9', originatedFrom: ORIGIN }])
    })

    it('evicts only the job that ran on a departed peer when leaving a cluster', () => {
        const state = getModularBridgeState()
        state.setSelfId(ORIGIN)
        brokerUpsert({ engine: 'lm-studio', runId: RUN, id: '7' })
        brokerUpsert({ engine: 'ollama', runId: RUN, id: '7', scheduledOn: PEER })

        state.dropRemoteWorkloads()

        expect(catalogKeys()).toEqual([keyOf('lm-studio', RUN, '7')])
        expect(removals).toEqual([
            { workloadId: '7', originatedFrom: ORIGIN, engine: 'ollama', runId: RUN }
        ])
    })
})

describe('finding the jobs a workloads:remove push retires', () => {
    function job(engine: EngineType, runId: string, id: string): Workload {
        return {
            id,
            model: 'model',
            engine,
            runId,
            state: 'running',
            originatedFrom: ORIGIN,
            createdAt: 1,
            startedAt: 1,
            completedAt: null,
            error: null,
            requesterId: null
        }
    }

    function catalogOf(workloads: Workload[]): Map<string, Workload> {
        return new Map(workloads.map(workload => [workloadKey(workload), workload]))
    }

    const ollamaJob = job('ollama', RUN, '1')
    const lmStudioJob = job('lm-studio', RUN, '1')
    const earlierRunJob = job('ollama', 'run-earlier', '1')
    const otherJob = job('ollama', RUN, '2')
    const catalog = catalogOf([ollamaJob, lmStudioJob, earlierRunJob, otherJob])

    it('finds every engine and run for a broker removal', () => {
        const removal: Removal = { workloadId: '1', originatedFrom: ORIGIN }

        expect(workloadKeysRemovedBy(catalog, removal).sort()).toEqual(
            [ollamaJob, lmStudioJob, earlierRunJob].map(workloadKey).sort()
        )
    })

    it('finds only the named engine and run for an exact removal', () => {
        const removal: Removal = {
            workloadId: '1',
            originatedFrom: ORIGIN,
            engine: 'ollama',
            runId: RUN
        }

        expect(workloadKeysRemovedBy(catalog, removal)).toEqual([workloadKey(ollamaJob)])
    })

    it('finds nothing for another job number or origin', () => {
        const otherNumber: Removal = { workloadId: '3', originatedFrom: ORIGIN }
        const otherOrigin: Removal = { workloadId: '1', originatedFrom: PEER }

        expect(workloadKeysRemovedBy(catalog, otherNumber)).toEqual([])
        expect(workloadKeysRemovedBy(catalog, otherOrigin)).toEqual([])
    })

    it('treats an unreported origin and an empty one as the same node', () => {
        const unowned: Workload = { ...ollamaJob, originatedFrom: null }
        const unownedCatalog = catalogOf([unowned])
        const broker: Removal = { workloadId: '1', originatedFrom: '' }
        const exact: Removal = { workloadId: '1', originatedFrom: '', engine: 'ollama', runId: RUN }

        expect(workloadKeysRemovedBy(unownedCatalog, broker)).toEqual([workloadKey(unowned)])
        expect(workloadKeysRemovedBy(unownedCatalog, exact)).toEqual([workloadKey(unowned)])
    })
})

describe("reading a workload's proxy run", () => {
    it('keeps the run the origin proxy reports', () => {
        const [workload] = parseWorkloadsInitial({
            workloads: [
                {
                    id: '1',
                    engine: 'ollama',
                    runId: RUN,
                    state: 'queued',
                    model: 'model',
                    originatedFrom: ORIGIN,
                    createdAt: 1
                }
            ]
        })

        expect(workload?.runId).toBe(RUN)
    })

    it('leaves the run empty when the origin does not report one', () => {
        const [workload] = parseWorkloadsInitial({
            workloads: [
                {
                    id: '1',
                    engine: 'ollama',
                    state: 'queued',
                    model: 'model',
                    originatedFrom: ORIGIN,
                    createdAt: 1
                }
            ]
        })

        expect(workload?.runId).toBe('')
    })
})
