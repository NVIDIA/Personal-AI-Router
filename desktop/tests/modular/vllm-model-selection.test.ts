// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { beforeEach, describe, expect, it, vi } from 'vitest'

vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import { getModularBridgeState } from '@/electron/service-bridge/modular-state'
import {
    isCanonicalVllmModelId,
    parseVllmModelSelectionRequest,
    vllmModelSelectionHold
} from '@/shared/utils/vllm-model-selection'
import { useConnectionStore } from '@/ui/stores/connection.store'
import { useEngineStatusStore } from '@/ui/stores/engine-status.store'
import { useEngineModelsStore } from '@/ui/stores/engine-models.store'
import { bindVllmGroupApi, useVllmGroupStore } from '@/ui/stores/vllm-group.store'
import {
    bindVllmModelSelectionApi,
    downloadedVllmModelIds,
    useVllmModelSelectionStore,
    vllmModelSelectionHoldFor
} from '@/ui/stores/vllm-model-selection.store'
import { emptyEngineStatus } from '@/shared/utils/engines'
import { inactiveStatus } from '../fixtures/vllm-group'
import type { ModelItem } from '@/shared/types/engines'
import { ModelExpiries } from '@/shared/constants/engines'

const MODEL = 'owner/model@' + 'a'.repeat(40)
const OTHER = 'owner/other@' + 'b'.repeat(40)

function modelItem(name: string, downloaded: boolean): ModelItem {
    return {
        name,
        size: 1,
        downloaded,
        status: downloaded ? 'idle' : 'pulling',
        parameterSize: '',
        quantization: '',
        family: '',
        digest: '',
        sizeVram: null,
        expiresAt: null,
        expiry: ModelExpiries[0],
        capabilities: []
    }
}

function seedRenderer(nodeId: string, processStatus: 'stopped' | 'running', managed = true) {
    useConnectionStore.setState({ connected: true, selfId: 'node-a', clusterId: 'cluster-a' })
    useEngineStatusStore.setState({
        statusByNode: new Map([
            [
                nodeId,
                new Map([
                    [
                        'vllm' as const,
                        {
                            ...emptyEngineStatus(nodeId, 'vllm'),
                            processStatus,
                            managed,
                            adopted: false
                        }
                    ]
                ])
            ]
        ])
    })
    useEngineModelsStore.setState({
        models: new Map([
            [
                `${nodeId}:vllm`,
                {
                    nodeId,
                    engineType: 'vllm' as const,
                    models: [modelItem(MODEL, true), modelItem(OTHER, false)]
                }
            ]
        ])
    })
    useVllmGroupStore.setState({
        known: true,
        status: inactiveStatus(),
        error: null,
        uncertainStart: false,
        owner: JSON.stringify(['node-a', 'cluster-a']),
        current: true
    })
}

describe('vLLM model selection request gate and hold', () => {
    it('accepts only canonical retained ids and exact request shapes', () => {
        expect(isCanonicalVllmModelId(MODEL)).toBe(true)
        expect(isCanonicalVllmModelId('local:' + 'c'.repeat(64))).toBe(true)
        for (const bad of [
            'owner/model',
            '/tmp/model',
            'https://hf.co/owner/model',
            'owner/model@abc',
            'rm -rf /',
            ''
        ])
            expect(isCanonicalVllmModelId(bad)).toBe(false)
        expect(parseVllmModelSelectionRequest({ nodeId: 'node-a', model: MODEL })).toEqual({
            nodeId: 'node-a',
            model: MODEL
        })
        for (const bad of [
            { nodeId: 'node-a', model: '/models/x' },
            { nodeId: 'node-a', model: MODEL, path: '/x' },
            { nodeId: 'node-a', model: MODEL, command: 'start' },
            { nodeId: '', model: MODEL },
            { model: MODEL },
            null
        ])
            expect(() => parseVllmModelSelectionRequest(bad)).toThrow()
    })

    it('holds unless the target is this stopped, managed, unheld vLLM and the model is in the retained library', () => {
        const base = {
            isSelf: true,
            processStatus: 'stopped',
            managed: true,
            adopted: false,
            groupHeld: false,
            downloadedModels: [MODEL],
            model: MODEL
        }
        expect(vllmModelSelectionHold(base)).toBeNull()
        expect(vllmModelSelectionHold({ ...base, isSelf: false })).toMatch(/this controller/)
        expect(vllmModelSelectionHold({ ...base, groupHeld: true })).toMatch(/serving group/)
        expect(vllmModelSelectionHold({ ...base, adopted: true })).toMatch(/external/)
        expect(vllmModelSelectionHold({ ...base, managed: undefined })).toMatch(/PAIR-managed/)
        expect(vllmModelSelectionHold({ ...base, processStatus: 'running' })).toMatch(/Stop vLLM/)
        expect(vllmModelSelectionHold({ ...base, model: '' })).toMatch(/Choose one/)
        expect(vllmModelSelectionHold({ ...base, model: '/tmp/model' })).toMatch(/exact retained/)
        expect(vllmModelSelectionHold({ ...base, model: OTHER })).toMatch(
            /not in the retained vLLM library/
        )
    })
})

describe('vLLM model selection renderer store', () => {
    const api = { selectVllmModel: vi.fn() }

    beforeEach(() => {
        vi.clearAllMocks()
        bindVllmGroupApi(null)
        bindVllmModelSelectionApi(api)
        useVllmModelSelectionStore.setState({ pending: null, error: null, lastResult: null })
        seedRenderer('node-a', 'stopped')
    })

    it('offers only downloaded ids and sends the request only when PAIR state admits it', async () => {
        expect(downloadedVllmModelIds('node-a')).toEqual([MODEL])
        expect(vllmModelSelectionHoldFor('node-a', MODEL, false)).toBeNull()
        api.selectVllmModel.mockResolvedValue({
            nodeId: 'node-a',
            model: MODEL,
            selectedModel: MODEL
        })
        await useVllmModelSelectionStore.getState().select('node-a', MODEL)
        expect(api.selectVllmModel).toHaveBeenCalledExactlyOnceWith('node-a', MODEL)
        expect(useVllmModelSelectionStore.getState().lastResult?.selectedModel).toBe(MODEL)
        expect(useVllmModelSelectionStore.getState().error).toBeNull()
    })

    it('refuses a model outside the retained library, a running engine, a remote node, and a held group before any request', async () => {
        await useVllmModelSelectionStore.getState().select('node-a', OTHER)
        expect(useVllmModelSelectionStore.getState().error).toMatch(
            /not in the retained vLLM library/
        )
        seedRenderer('node-a', 'running')
        await useVllmModelSelectionStore.getState().select('node-a', MODEL)
        expect(useVllmModelSelectionStore.getState().error).toMatch(/Stop vLLM/)
        seedRenderer('node-b', 'stopped')
        await useVllmModelSelectionStore.getState().select('node-b', MODEL)
        expect(useVllmModelSelectionStore.getState().error).toMatch(/this controller/)
        seedRenderer('node-a', 'stopped')
        useVllmGroupStore.setState({ known: false, status: null, error: 'journal unreadable' })
        await useVllmModelSelectionStore.getState().select('node-a', MODEL)
        expect(useVllmModelSelectionStore.getState().error).toMatch(/serving group/)
        expect(api.selectVllmModel).not.toHaveBeenCalled()
    })

    it('surfaces the backend refusal verbatim and refuses a reply for a different model', async () => {
        api.selectVllmModel.mockRejectedValueOnce(new Error('stop vLLM before selecting a model'))
        await useVllmModelSelectionStore.getState().select('node-a', MODEL)
        expect(useVllmModelSelectionStore.getState().error).toBe(
            'stop vLLM before selecting a model'
        )
        api.selectVllmModel.mockResolvedValueOnce({
            nodeId: 'node-a',
            model: MODEL,
            selectedModel: OTHER
        })
        await useVllmModelSelectionStore.getState().select('node-a', MODEL)
        expect(useVllmModelSelectionStore.getState().error).toMatch(/different model/)
        expect(useVllmModelSelectionStore.getState().lastResult).toBeNull()
    })
})

describe('selected_model plumbing through the bridge state', () => {
    it('projects Engine Manager selected_model into the local and remote engine status', () => {
        const state = getModularBridgeState()
        state.setSelfId('selection-local')
        state.applyEngineManagerStatus({
            engine: 'vllm',
            installed: true,
            running: false,
            healthy: false,
            enabled: false,
            managed: true,
            adopted: false,
            install_supported: true,
            version: '0.29.0',
            port: 8001,
            selected_model: MODEL
        })
        const local = state
            .getEngineInitialState()
            .statuses.find(
                status => status.nodeId === 'selection-local' && status.engineType === 'vllm'
            )
        expect(local?.selectedModel).toBe(MODEL)
        expect(local?.processStatus).toBe('stopped')

        state.applyEngineManagerStatus({
            engine: 'vllm',
            installed: true,
            running: false,
            healthy: false,
            managed: true,
            adopted: false,
            port: 8001
        })
        expect(
            state
                .getEngineInitialState()
                .statuses.find(
                    status => status.nodeId === 'selection-local' && status.engineType === 'vllm'
                )?.selectedModel
        ).toBeUndefined()
    })
})
