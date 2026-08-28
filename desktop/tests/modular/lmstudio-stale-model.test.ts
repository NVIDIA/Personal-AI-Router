// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest'

vi.mock('electron', () => ({
    app: { isPackaged: false, getAppPath: () => process.cwd() },
    BrowserWindow: { getAllWindows: () => [] }
}))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import { getModularBridgeState } from '@/electron/service-bridge/modular-state'
import {
    getModularSupervisor,
    parseListModelNames
} from '@/electron/service-bridge/modular-supervisor'

describe('LM Studio model reconciliation', () => {
    it('parses the native inventory and distinguishes explicit empty from unknown', () => {
        expect(
            parseListModelNames({
                models: [
                    { key: 'lmstudio-community/phi-3', loaded_instances: [] },
                    { key: 'lmstudio-community/gemma-2b', loaded_instances: [] }
                ]
            })
        ).toEqual(['lmstudio-community/phi-3', 'lmstudio-community/gemma-2b'])
        expect(parseListModelNames({ models: [] })).toEqual([])
        expect(() => parseListModelNames({ models: null })).toThrow('missing its model array')
        expect(() => parseListModelNames({ models: [{}] })).toThrow('no usable model names')
    })

    it('keeps a successful empty local inventory instead of reviving discovery data', () => {
        const state = getModularBridgeState()
        const nodeId = 'lmstudio-reconciliation-local'

        state.setSelfId(nodeId)
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: {
                nodes: [
                    {
                        hostUuid: nodeId,
                        name: 'lmstudio-reconciliation-host',
                        ipAddress: '127.0.0.1',
                        port: 14318,
                        models: ['stale-model'],
                        modelsByEngine: { lmstudio: ['stale-model'] }
                    }
                ]
            }
        })
        state.applyEngineManagerStatus({
            engine: 'lmstudio',
            installed: true,
            running: true,
            healthy: true,
            port: 1235
        })

        const localNames = (): string[] =>
            state
                .getEngineInitialState()
                .models.find(
                    models => models.nodeId === nodeId && models.engineType === 'lm-studio'
                )
                ?.models.map(model => model.name) ?? []

        expect(localNames()).toEqual(['stale-model'])

        state.setLocalEngineModels('lm-studio', [])
        expect(localNames()).toEqual([])

        state.fallbackLocalEngineModelsToDiscovery('lm-studio')
        expect(localNames()).toEqual(['stale-model'])
    })

    it('refreshes only the engine whose attribution changed when the union did not', () => {
        const state = getModularBridgeState()
        const nodeId = 'shared-model-local'
        const refresh = vi.fn()

        state.setSelfId(nodeId)
        state.setLocalDiscoveryModelRefresher(refresh)
        state.handleNotification({
            source: 'ollama-proxy',
            method: 'node/discovered',
            params: { id: nodeId, port: 11434 }
        })
        state.handleNotification({
            source: 'lmstudio-proxy',
            method: 'node/discovered',
            params: { id: nodeId, port: 1234 }
        })
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: {
                nodes: [
                    {
                        hostUuid: nodeId,
                        name: 'shared-model-host',
                        ipAddress: '127.0.0.1',
                        port: 14318,
                        models: ['shared-model'],
                        modelsByEngine: {
                            ollama: ['shared-model'],
                            lmstudio: ['shared-model']
                        }
                    }
                ]
            }
        })
        refresh.mockClear()

        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: {
                nodes: [
                    {
                        hostUuid: nodeId,
                        name: 'shared-model-host',
                        ipAddress: '127.0.0.1',
                        port: 14318,
                        models: ['shared-model'],
                        modelsByEngine: {
                            ollama: ['shared-model'],
                            lmstudio: []
                        }
                    }
                ]
            }
        })

        expect(refresh).toHaveBeenCalledOnce()
        expect(refresh).toHaveBeenCalledWith('lm-studio')
    })

    it('refreshes a deleted final model while the LM Studio proxy is unavailable', async () => {
        const state = getModularBridgeState()
        const supervisor = getModularSupervisor()
        const nodeId = 'lmstudio-proxy-down-deletion-local'
        const discoveryNode = (models: string[]) => ({
            hostUuid: nodeId,
            name: 'lmstudio-proxy-down-deletion-host',
            ipAddress: '127.0.0.1',
            port: 14318,
            models,
            modelsByEngine: { lmstudio: models }
        })

        state.setSelfId(nodeId)
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: { nodes: [discoveryNode(['deleted-model'])] }
        })
        state.applyEngineManagerStatus({
            engine: 'lmstudio',
            installed: true,
            running: true,
            healthy: true,
            port: 1235
        })
        state.setLocalEngineModels('lm-studio', ['deleted-model'])
        state.setLocalDiscoveryModelRefresher(engine =>
            supervisor.refreshDiscoveryEngineModels(engine)
        )

        const hasProcess = vi.spyOn(supervisor, 'hasProcess').mockReturnValue(true)
        const call = vi.spyOn(supervisor, 'callProcess').mockResolvedValueOnce({ models: [] })

        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: { nodes: [discoveryNode([])] }
        })

        await vi.waitFor(() => expect(call).toHaveBeenCalledOnce())
        await vi.waitFor(() =>
            expect(
                state
                    .getEngineInitialState()
                    .models.find(
                        models => models.nodeId === nodeId && models.engineType === 'lm-studio'
                    )?.models
            ).toEqual([])
        )

        call.mockRestore()
        hasProcess.mockRestore()
    })

    it('releases the stopped-state empty cache when the first restart query fails', async () => {
        const state = getModularBridgeState()
        const supervisor = getModularSupervisor()
        const nodeId = 'lmstudio-restart-local'

        state.setSelfId(nodeId)
        state.handleNotification({
            source: 'lmstudio-proxy',
            method: 'node/discovered',
            params: { id: nodeId, port: 1234 }
        })
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: {
                nodes: [
                    {
                        hostUuid: nodeId,
                        name: 'lmstudio-restart-host',
                        ipAddress: '127.0.0.1',
                        port: 14318,
                        models: ['discovery-model'],
                        modelsByEngine: { lmstudio: ['discovery-model'] }
                    }
                ]
            }
        })
        state.applyEngineManagerStatus({
            engine: 'lmstudio',
            installed: true,
            running: false,
            port: 1235
        })

        const hasProcess = vi.spyOn(supervisor, 'hasProcess').mockReturnValue(true)
        const call = vi.spyOn(supervisor, 'callProcess').mockRejectedValueOnce(new Error('offline'))

        supervisor.refreshManagedEngineModels({ engine: 'lmstudio', running: false })
        expect(
            state
                .getEngineInitialState()
                .models.find(
                    models => models.nodeId === nodeId && models.engineType === 'lm-studio'
                )?.models
        ).toEqual([])

        supervisor.refreshManagedEngineModels({ engine: 'lmstudio', running: true })
        await vi.waitFor(() =>
            expect(
                state
                    .getEngineInitialState()
                    .models.find(
                        models => models.nodeId === nodeId && models.engineType === 'lm-studio'
                    )
                    ?.models.map(model => model.name)
            ).toEqual(['discovery-model'])
        )

        call.mockRestore()
        hasProcess.mockRestore()
    })

    it('retains a successful empty inventory across a later transient query failure', async () => {
        const state = getModularBridgeState()
        const supervisor = getModularSupervisor()
        const nodeId = 'lmstudio-last-good-empty-local'

        state.setSelfId(nodeId)
        state.handleNotification({
            source: 'lmstudio-proxy',
            method: 'node/discovered',
            params: { id: nodeId, port: 1234 }
        })
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: {
                nodes: [
                    {
                        hostUuid: nodeId,
                        name: 'lmstudio-last-good-empty-host',
                        ipAddress: '127.0.0.1',
                        port: 14318,
                        models: ['stale-discovery-model'],
                        modelsByEngine: { lmstudio: ['stale-discovery-model'] }
                    }
                ]
            }
        })

        const hasProcess = vi.spyOn(supervisor, 'hasProcess').mockReturnValue(true)
        const call = vi
            .spyOn(supervisor, 'callProcess')
            .mockResolvedValueOnce({ models: [] })
            .mockRejectedValueOnce(new Error('temporary failure'))

        supervisor.refreshManagedEngineModels({ engine: 'lmstudio', running: true })
        await vi.waitFor(() =>
            expect(
                state
                    .getEngineInitialState()
                    .models.find(
                        models => models.nodeId === nodeId && models.engineType === 'lm-studio'
                    )?.models
            ).toEqual([])
        )

        supervisor.refreshManagedEngineModels({ engine: 'lmstudio', running: true })
        await vi.waitFor(() => expect(call).toHaveBeenCalledTimes(2))
        expect(
            state
                .getEngineInitialState()
                .models.find(
                    models => models.nodeId === nodeId && models.engineType === 'lm-studio'
                )?.models
        ).toEqual([])

        call.mockRestore()
        hasProcess.mockRestore()
    })

    it('releases the stopped-state empty cache when restart returns malformed inventory', async () => {
        const state = getModularBridgeState()
        const supervisor = getModularSupervisor()
        const nodeId = 'lmstudio-malformed-restart-local'

        state.setSelfId(nodeId)
        state.handleNotification({
            source: 'lmstudio-proxy',
            method: 'node/discovered',
            params: { id: nodeId, port: 1234 }
        })
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: {
                nodes: [
                    {
                        hostUuid: nodeId,
                        name: 'lmstudio-malformed-restart-host',
                        ipAddress: '127.0.0.1',
                        port: 14318,
                        models: ['discovery-after-malformed'],
                        modelsByEngine: { lmstudio: ['discovery-after-malformed'] }
                    }
                ]
            }
        })

        const hasProcess = vi.spyOn(supervisor, 'hasProcess').mockReturnValue(true)
        const call = vi.spyOn(supervisor, 'callProcess').mockResolvedValueOnce({ models: null })

        supervisor.refreshManagedEngineModels({ engine: 'lmstudio', running: false })
        supervisor.refreshManagedEngineModels({ engine: 'lmstudio', running: true })

        await vi.waitFor(() =>
            expect(
                state
                    .getEngineInitialState()
                    .models.find(
                        models => models.nodeId === nodeId && models.engineType === 'lm-studio'
                    )
                    ?.models.map(model => model.name)
            ).toEqual(['discovery-after-malformed'])
        )

        call.mockRestore()
        hasProcess.mockRestore()
    })

    it('does not let an older running refresh overwrite a later stop', async () => {
        const state = getModularBridgeState()
        const supervisor = getModularSupervisor()
        const nodeId = 'lmstudio-delayed-refresh-local'
        let resolveInventory: ((value: { models: { key: string }[] }) => void) | undefined
        const delayedInventory = new Promise<{ models: { key: string }[] }>(resolve => {
            resolveInventory = resolve
        })

        state.setSelfId(nodeId)
        const hasProcess = vi
            .spyOn(supervisor, 'hasProcess')
            .mockReturnValueOnce(true)
            .mockReturnValue(false)
        const call = vi.spyOn(supervisor, 'callProcess').mockReturnValueOnce(delayedInventory)

        supervisor.refreshManagedEngineModels({ engine: 'lmstudio', running: true })
        supervisor.refreshManagedEngineModels({ engine: 'lmstudio', running: false })
        resolveInventory?.({ models: [{ key: 'late-model' }] })
        await delayedInventory
        await Promise.resolve()

        expect(
            state
                .getEngineInitialState()
                .models.find(
                    models => models.nodeId === nodeId && models.engineType === 'lm-studio'
                )?.models
        ).toEqual([])

        call.mockRestore()
        hasProcess.mockRestore()
    })

    it('retries a failed discovery refresh without another discovery event', async () => {
        const state = getModularBridgeState()
        const supervisor = getModularSupervisor()
        const nodeId = 'lmstudio-discovery-retry-local'
        const discoveryNode = (models: string[]) => ({
            hostUuid: nodeId,
            name: 'lmstudio-discovery-retry-host',
            ipAddress: '127.0.0.1',
            port: 14318,
            models,
            modelsByEngine: { lmstudio: models }
        })

        state.setSelfId(nodeId)
        state.handleNotification({
            source: 'lmstudio-proxy',
            method: 'node/discovered',
            params: { id: nodeId, port: 1234 }
        })
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: { nodes: [discoveryNode(['deleted-model'])] }
        })
        state.setLocalDiscoveryModelRefresher(engine =>
            supervisor.refreshDiscoveryEngineModels(engine)
        )

        const hasProcess = vi.spyOn(supervisor, 'hasProcess').mockReturnValue(true)
        const call = vi
            .spyOn(supervisor, 'callProcess')
            .mockResolvedValueOnce({ models: [{ key: 'deleted-model' }] })
            .mockRejectedValueOnce(new Error('temporary discovery refresh failure'))
            .mockResolvedValueOnce({ models: [] })

        supervisor.refreshManagedEngineModels({ engine: 'lmstudio', running: true })
        await vi.waitFor(() => expect(call).toHaveBeenCalledTimes(1))

        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: { nodes: [discoveryNode([])] }
        })
        await vi.waitFor(() => expect(call).toHaveBeenCalledTimes(2))
        await vi.waitFor(() => expect(call).toHaveBeenCalledTimes(3), { timeout: 2_000 })
        await vi.waitFor(() =>
            expect(
                state
                    .getEngineInitialState()
                    .models.find(
                        models => models.nodeId === nodeId && models.engineType === 'lm-studio'
                    )?.models
            ).toEqual([])
        )

        call.mockRestore()
        hasProcess.mockRestore()
    })

    it('cancels a pending discovery retry when the engine stops', async () => {
        const supervisor = getModularSupervisor()
        const hasProcess = vi.spyOn(supervisor, 'hasProcess').mockReturnValue(true)
        const call = vi
            .spyOn(supervisor, 'callProcess')
            .mockResolvedValueOnce({ models: [{ key: 'model-before-stop' }] })
            .mockRejectedValueOnce(new Error('schedule retry'))

        supervisor.refreshManagedEngineModels({ engine: 'lmstudio', running: true })
        await vi.waitFor(() => expect(call).toHaveBeenCalledTimes(1))
        supervisor.refreshDiscoveryEngineModels('lm-studio')
        await vi.waitFor(() => expect(call).toHaveBeenCalledTimes(2))

        supervisor.refreshManagedEngineModels({ engine: 'lmstudio', running: false })
        await new Promise(resolve => setTimeout(resolve, 1_100))
        expect(call).toHaveBeenCalledTimes(2)

        call.mockRestore()
        hasProcess.mockRestore()
    })
})
