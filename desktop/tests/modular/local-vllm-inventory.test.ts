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
import type { JsonValue } from '@/electron/service-bridge/json-rpc-subprocess'

const activeModel = 'org/active-model'
const downloadedModel = 'org/downloaded-model'
let nextNode = 0

async function startOwner(running: boolean, downloaded: string[]) {
    const state = getModularBridgeState()
    const supervisor = getModularSupervisor()
    const nodeId = `local-vllm-inventory-${++nextNode}`
    state.setLocalDiscoveryModelRefresher(() => {})
    state.setSelfId(nodeId)

    const node = (inventory: string[] | undefined, id = nodeId) => ({
        hostUuid: id,
        name: `${id}-host`,
        ipAddress: '192.0.2.10',
        port: 14318,
        models: running ? ['api-alias', 'other-model'] : ['other-model'],
        modelsByEngine: {
            vllm: running ? ['api-alias'] : [],
            ollama: ['other-model'],
            lmstudio: ['other-model'],
            llamacpp: ['other-model']
        },
        loadedByEngine: { vllm: running ? [activeModel] : [] },
        ...(inventory === undefined ? {} : { downloadedByEngine: { vllm: inventory } })
    })
    const publish = (inventory: string[] | undefined) =>
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: { nodes: [node(inventory)] }
        })
    publish(downloaded)
    for (const engine of ['ollama', 'lmstudio', 'llamacpp']) {
        state.applyEngineManagerStatus({ engine, installed: true, running: true, healthy: true })
    }
    state.applyEngineManagerStatus({ engine: 'vllm', installed: true, running, healthy: running })
    const hasProcess = vi.spyOn(supervisor, 'hasProcess').mockReturnValue(true)
    const call = vi
        .spyOn(supervisor, 'callProcess')
        .mockResolvedValue({ data: downloaded.map(id => ({ id })) })
    supervisor.refreshManagedEngineModels({ engine: 'vllm', running })
    const rows = () =>
        state
            .getEngineInitialState()
            .models.find(group => group.nodeId === nodeId && group.engineType === 'vllm')
    await vi.waitFor(() => expect(rows()?.models.map(model => model.name)).toEqual(downloaded))
    call.mockClear()
    state.setLocalDiscoveryModelRefresher(engine => supervisor.refreshDiscoveryEngineModels(engine))
    return { state, supervisor, nodeId, node, publish, rows, call, hasProcess }
}

afterEach(() => {
    getModularBridgeState().setLocalDiscoveryModelRefresher(() => {})
    vi.restoreAllMocks()
})

describe('local vLLM downloaded inventory', () => {
    it.each([
        {
            name: 'adds a peer-downloaded model while the served alias stays unchanged',
            running: true,
            before: [activeModel],
            after: [activeModel, downloadedModel]
        },
        {
            name: 'removes a peer-deleted idle model while the served alias stays unchanged',
            running: true,
            before: [activeModel, downloadedModel],
            after: [activeModel]
        },
        {
            name: 'adds a peer-downloaded model while the engine is stopped',
            running: false,
            before: [],
            after: [downloadedModel]
        },
        {
            name: 'removes the final peer-deleted model while the engine is stopped',
            running: false,
            before: [downloadedModel],
            after: []
        }
    ])('$name', async ({ running, before, after }) => {
        const owner = await startOwner(running, before)
        owner.call.mockResolvedValue({ data: after.map(id => ({ id })) })

        owner.publish(after)

        await vi.waitFor(() => expect(owner.call).toHaveBeenCalledOnce())
        expect(owner.call).toHaveBeenCalledWith('broker', 'engine:action', {
            engine: 'vllm',
            action: 'list_models'
        })
        await vi.waitFor(() =>
            expect(
                owner.rows()?.models.map(model => ({ name: model.name, status: model.status }))
            ).toEqual(
                after.map(name => ({
                    name,
                    status: running && name === activeModel ? 'loaded' : 'idle'
                }))
            )
        )
        expect(owner.rows()?.inventoryKnown).toBe(true)
    })

    it('does not refresh an unchanged downloaded inventory', async () => {
        const owner = await startOwner(true, [activeModel, downloadedModel])

        owner.publish([activeModel, downloadedModel])

        expect(owner.call).not.toHaveBeenCalled()
        expect(owner.rows()?.models.map(model => model.name)).toEqual([
            activeModel,
            downloadedModel
        ])
    })

    it('keeps a stopped owner cache when its downloaded inventory is temporarily unknown', async () => {
        const owner = await startOwner(false, [downloadedModel])

        owner.publish(undefined)

        expect(owner.call).not.toHaveBeenCalled()
        expect(owner.rows()?.models.map(model => model.name)).toEqual([downloadedModel])
    })

    it('clears a stopped owner cache when an unknown inventory becomes known empty', async () => {
        const owner = await startOwner(false, [downloadedModel])
        owner.publish(undefined)
        owner.call.mockResolvedValue({ data: [] })

        owner.publish([])

        await vi.waitFor(() => expect(owner.call).toHaveBeenCalledOnce())
        await vi.waitFor(() => expect(owner.rows()?.models).toEqual([]))
        expect(owner.rows()?.inventoryKnown).toBe(true)
    })

    it('updates peer rows without refreshing the local owner inventory', async () => {
        const owner = await startOwner(true, [activeModel])
        const peerId = 'vllm-inventory-remote-peer'

        owner.state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: {
                nodes: [owner.node([activeModel]), owner.node([downloadedModel], peerId)]
            }
        })

        expect(owner.call).not.toHaveBeenCalled()
        const peer = owner.state
            .getEngineInitialState()
            .models.find(group => group.nodeId === peerId && group.engineType === 'vllm')
        expect(peer?.models.map(model => model.name)).toContain(downloadedModel)
        expect(owner.rows()?.models.map(model => model.name)).toEqual([activeModel])
    })

    it('retries a failed stopped-engine refresh without another inventory change', async () => {
        const owner = await startOwner(false, [downloadedModel])
        owner.call
            .mockRejectedValueOnce(new Error('temporary list failure'))
            .mockResolvedValue({ data: [] })

        owner.publish([])

        await vi.waitFor(() => expect(owner.call).toHaveBeenCalledTimes(2), { timeout: 2_000 })
        await vi.waitFor(() => expect(owner.rows()?.models).toEqual([]))
    })

    it('does not restore a deleted model from an older in-flight inventory response', async () => {
        const owner = await startOwner(false, [])
        let finishDownload: ((result: JsonValue) => void) | undefined
        const pendingDownload = new Promise<JsonValue>(resolve => {
            finishDownload = resolve
        })
        owner.call.mockReturnValueOnce(pendingDownload).mockResolvedValue({ data: [] })

        owner.publish([downloadedModel])
        owner.publish([])

        await vi.waitFor(() => expect(owner.call).toHaveBeenCalledTimes(2))
        await vi.waitFor(() => expect(owner.rows()?.models).toEqual([]))
        if (!finishDownload) throw new Error('Download response resolver was not initialized')
        finishDownload({ data: [{ id: downloadedModel }] })
        await pendingDownload
        expect(owner.rows()?.models).toEqual([])
    })
})
