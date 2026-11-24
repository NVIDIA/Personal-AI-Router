// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, describe, expect, it, vi } from 'vitest'

vi.mock('electron', () => ({
    app: { isPackaged: false, getAppPath: () => process.cwd() },
    BrowserWindow: { getAllWindows: () => [] }
}))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import { getModularBridgeState } from '@/electron/service-bridge/modular-state'
import { getModularSupervisor } from '@/electron/service-bridge/modular-supervisor'

/**
 * A custom model started from a folder. MTPLX serves it, but it is not a pack in
 * the cache, so MTPLX's own model list cannot name it — only the resident set can.
 */
const servedModel = 'mtplx-flash-next-optimized-speed'
/** A model pack in `~/.mtplx/models`, which MTPLX's own model list does name. */
const cachedPack = 'mtplx/mtplx-lite'
let nextNode = 0

/**
 * A Mac that already runs MTPLX before PAIR starts: the engine-manager reports
 * MTPLX online and the self node is known. `discoveryResident` is what the node's
 * discovery record says MTPLX has in memory before any model sweep is read; each
 * case then decides what `engine:models` answers. Pass `null` for a node that
 * never reported residency at all — no `loadedByEngine` key, which is a third
 * answer rather than an empty one.
 */
function startMTPLX(discoveryResident: string[] | null) {
    const state = getModularBridgeState()
    const supervisor = getModularSupervisor()
    const nodeId = `mtplx-served-model-${++nextNode}`

    state.setLocalDiscoveryModelRefresher(() => {})
    state.setSelfId(nodeId)
    state.handleNotification({
        source: 'broker',
        method: 'discovery:nodes-changed',
        params: {
            nodes: [
                {
                    hostUuid: nodeId,
                    name: `${nodeId}-host`,
                    ipAddress: '192.0.2.10',
                    port: 14318,
                    models: discoveryResident ?? [],
                    modelsByEngine: { mtplx: discoveryResident ?? [] },
                    ...(discoveryResident === null
                        ? {}
                        : { loadedByEngine: { mtplx: discoveryResident } })
                }
            ]
        }
    })
    state.applyEngineManagerStatus({
        engine: 'mtplx',
        installed: true,
        running: true,
        healthy: true
    })

    vi.spyOn(supervisor, 'hasProcess').mockReturnValue(true)
    const call = vi.spyOn(supervisor, 'callProcess').mockResolvedValue({})
    const rows = () =>
        state
            .getEngineInitialState()
            .models.find(group => group.nodeId === nodeId && group.engineType === 'mtplx')

    return { state, supervisor, call, rows }
}

afterEach(() => {
    getModularBridgeState().setLocalDiscoveryModelRefresher(() => {})
    vi.restoreAllMocks()
})

describe('MTPLX model list', () => {
    it('lists the model MTPLX serves although its own model list cannot name it', async () => {
        const machine = startMTPLX([servedModel])
        machine.call.mockResolvedValue({
            models: [servedModel],
            modelsByEngine: { mtplx: [servedModel] },
            downloadedByEngine: { mtplx: [cachedPack] },
            loadedByEngine: { mtplx: [servedModel] }
        })

        machine.supervisor.refreshManagedEngineModels({ engine: 'mtplx', running: true })

        await vi.waitFor(() => expect(machine.call).toHaveBeenCalledOnce())
        expect(machine.call).toHaveBeenCalledWith('broker', 'engine:models')
        await vi.waitFor(() =>
            expect(
                machine.rows()?.models.map(model => ({ name: model.name, status: model.status }))
            ).toEqual([
                { name: cachedPack, status: 'idle' },
                { name: servedModel, status: 'loaded' }
            ])
        )
        expect(machine.rows()?.inventoryKnown).toBe(true)
    })

    it('lists only MTPLX models although the model sweep covers every engine', async () => {
        const machine = startMTPLX([])
        machine.call.mockResolvedValue({
            models: ['Qwen/Qwen3-8B', cachedPack, servedModel],
            modelsByEngine: { vllm: ['Qwen/Qwen3-8B'], mtplx: [servedModel] },
            downloadedByEngine: { vllm: ['Qwen/Qwen3-8B'], mtplx: [cachedPack] },
            loadedByEngine: { vllm: ['Qwen/Qwen3-8B'], mtplx: [servedModel] }
        })

        machine.supervisor.refreshManagedEngineModels({ engine: 'mtplx', running: true })

        await vi.waitFor(() =>
            expect(machine.rows()?.models.map(model => model.name)).toEqual([
                cachedPack,
                servedModel
            ])
        )
    })

    it('marks no model loaded once the sweep reports MTPLX serves nothing', async () => {
        const machine = startMTPLX([servedModel])
        // First read: MTPLX serves the custom model. Discovery already said so, so
        // a list that kept showing it loaded after the second read would be wrong.
        machine.call.mockResolvedValue({
            downloadedByEngine: { mtplx: [cachedPack] },
            loadedByEngine: { mtplx: [servedModel] }
        })
        machine.supervisor.refreshManagedEngineModels({ engine: 'mtplx', running: true })
        await vi.waitFor(() =>
            expect(machine.rows()?.models.find(model => model.name === servedModel)?.status).toBe(
                'loaded'
            )
        )
        machine.call.mockClear()

        // MTPLX quits: its cache is still on disk and nothing is in memory.
        machine.call.mockResolvedValue({
            downloadedByEngine: { mtplx: [cachedPack] },
            loadedByEngine: { mtplx: [] }
        })
        machine.supervisor.refreshManagedEngineModels({ engine: 'mtplx', running: false })

        await vi.waitFor(() => expect(machine.call).toHaveBeenCalledOnce())
        await vi.waitFor(() =>
            expect(
                machine.rows()?.models.map(model => ({ name: model.name, status: model.status }))
            ).toEqual([{ name: cachedPack, status: 'idle' }])
        )
    })

    it('keeps the last model list when the sweep reports none for MTPLX', async () => {
        const machine = startMTPLX([servedModel])
        machine.call.mockResolvedValue({
            downloadedByEngine: { mtplx: [cachedPack] },
            loadedByEngine: { mtplx: [servedModel] }
        })
        machine.supervisor.refreshManagedEngineModels({ engine: 'mtplx', running: true })
        await vi.waitFor(() => expect(machine.rows()?.models).toHaveLength(2))
        machine.call.mockClear()

        // A sweep that names no list for MTPLX is not a sweep that says MTPLX has
        // no models: the rows stay rather than flashing empty until the next read.
        machine.call.mockResolvedValue({
            models: ['Qwen/Qwen3-8B'],
            modelsByEngine: { vllm: ['Qwen/Qwen3-8B'] },
            loadedByEngine: { vllm: [] }
        })
        machine.supervisor.refreshManagedEngineModels({ engine: 'mtplx', running: true })

        await vi.waitFor(() => expect(machine.call).toHaveBeenCalledOnce())
        expect(machine.rows()?.models.map(model => model.name)).toEqual([cachedPack, servedModel])
    })

    it('reports residency as unknown, not as empty, when no source names what MTPLX serves', async () => {
        // MTPLX is up and its cache was read, but nothing answered what it holds in
        // memory: no `loadedByEngine` key in the discovery record and none in the
        // `engine:models` reply. That is the third answer, and rendering it as
        // "holds nothing" would tell the row a one-click Delete is safe.
        //
        // These pack names appear in no other case here. The bridge caches one
        // model list per engine for the whole file, so names the earlier cases
        // already used would let this assertion read that stale list and pass
        // without ever reading the reply.
        const packOnDisk = 'unreported/mtplx-lite'
        const packServedFromPath = 'unreported/mtplx-flash-next-optimized-speed'
        const machine = startMTPLX(null)
        machine.call.mockResolvedValue({
            downloadedByEngine: { mtplx: [packOnDisk, packServedFromPath] }
        })

        machine.supervisor.refreshManagedEngineModels({ engine: 'mtplx', running: true })

        await vi.waitFor(() =>
            expect(machine.rows()?.models.map(model => model.name)).toEqual([
                packOnDisk,
                packServedFromPath
            ])
        )
        expect(machine.rows()?.inventoryKnown).toBe(true)
        expect(machine.rows()?.residencyKnown).toBe(false)
        expect(
            machine.rows()?.models.map(model => ({ name: model.name, status: model.status }))
        ).toEqual([
            { name: packOnDisk, status: 'idle' },
            { name: packServedFromPath, status: 'idle' }
        ])
    })

    it('reports residency as known when MTPLX answers that it serves nothing', async () => {
        // Same reply plus one key. An empty `loadedByEngine` for a running MTPLX
        // is an answer — the server was queried and holds no pack — so the rows may
        // offer the plain Delete that the case above has to withhold.
        const packOnDisk = 'reported/mtplx-lite'
        const packServedFromPath = 'reported/mtplx-flash-next-optimized-speed'
        const machine = startMTPLX(null)
        machine.call.mockResolvedValue({
            downloadedByEngine: { mtplx: [packOnDisk, packServedFromPath] },
            loadedByEngine: { mtplx: [] }
        })

        machine.supervisor.refreshManagedEngineModels({ engine: 'mtplx', running: true })

        // Wait on the list, not on the call: names and `residencyKnown` arrive in
        // the same push, and the bridge still holds the previous case's list until
        // this read lands.
        await vi.waitFor(() =>
            expect(machine.rows()?.models.map(model => model.name)).toEqual([
                packOnDisk,
                packServedFromPath
            ])
        )
        expect(machine.rows()?.residencyKnown).toBe(true)
        expect(machine.rows()?.models.map(model => model.status)).toEqual(['idle', 'idle'])
    })
})
