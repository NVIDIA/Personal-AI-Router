// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { beforeEach, describe, expect, it, vi } from 'vitest'

vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import { getModularBridgeState } from '@/electron/service-bridge/modular-state'

/**
 * The OpenAI-compatible proxy fronts more than one engine, so a node it reports
 * is no longer necessarily LM Studio. The node's own per-engine model
 * attribution says which engine it runs; reading the proxy's name instead would
 * label every vLLM node as LM Studio.
 */
function proxyNode(id: string, modelsByEngine: Record<string, string[]>) {
    return {
        id,
        host: id,
        port: 1234,
        addresses: ['192.0.2.60'],
        ip: '192.0.2.60',
        modelsByEngine
    }
}

describe('OpenAI proxy engine attribution', () => {
    // The bridge state is a process singleton, so each case uses its own node
    // ids rather than trying to reset it.
    const state = getModularBridgeState()

    beforeEach(() => {
        state.setSelfId('openai-proxy-engines-local')
    })

    it('labels a vLLM-only peer as vLLM, not LM Studio', () => {
        state.handleNotification({
            source: 'lmstudio-proxy',
            method: 'node/discovered',
            params: proxyNode('vllm-peer', { vllm: ['Qwen/Qwen3-8B'] })
        })
        expect(state.isRemoteEngineRunning('vllm-peer', 'vllm')).toBe(true)
        expect(state.isRemoteEngineRunning('vllm-peer', 'lm-studio')).toBe(false)
    })

    it('reports both engines for a peer running LM Studio and vLLM', () => {
        state.handleNotification({
            source: 'lmstudio-proxy',
            method: 'node/discovered',
            params: proxyNode('dual-peer', { lmstudio: ['qwen2.5-7b'], vllm: ['Qwen/Qwen3-8B'] })
        })
        expect(state.isRemoteEngineRunning('dual-peer', 'lm-studio')).toBe(true)
        expect(state.isRemoteEngineRunning('dual-peer', 'vllm')).toBe(true)
    })

    it('counts an engine that is running with no models as present', () => {
        // The proxy reports a key with an empty (JSON null) list for an engine
        // that is up but holds nothing, which is not the same as absent.
        state.handleNotification({
            source: 'lmstudio-proxy',
            method: 'node/discovered',
            params: proxyNode('empty-peer', { vllm: [] })
        })
        expect(state.isRemoteEngineRunning('empty-peer', 'vllm')).toBe(true)
    })

    it('drops an engine the node stopped advertising', () => {
        state.handleNotification({
            source: 'lmstudio-proxy',
            method: 'node/discovered',
            params: proxyNode('shrinking-peer', { lmstudio: ['a'], vllm: ['b'] })
        })
        state.handleNotification({
            source: 'lmstudio-proxy',
            method: 'node/updated',
            params: proxyNode('shrinking-peer', { lmstudio: ['a'] })
        })
        expect(state.isRemoteEngineRunning('shrinking-peer', 'lm-studio')).toBe(true)
        expect(state.isRemoteEngineRunning('shrinking-peer', 'vllm')).toBe(false)
    })

    it('clears every engine the proxy fronts when the node is removed', () => {
        state.handleNotification({
            source: 'lmstudio-proxy',
            method: 'node/discovered',
            params: proxyNode('leaving-peer', { lmstudio: ['a'], vllm: ['b'] })
        })
        // A removal payload carries the node id alone; the proxy sends it only
        // once its last engine entry for that node is gone.
        state.handleNotification({
            source: 'lmstudio-proxy',
            method: 'node/removed',
            params: { id: 'leaving-peer' }
        })
        expect(state.isRemoteEngineRunning('leaving-peer', 'lm-studio')).toBe(false)
        expect(state.isRemoteEngineRunning('leaving-peer', 'vllm')).toBe(false)
    })

    it('needs no attribution from a proxy that fronts one engine', () => {
        // ollama-proxy routes Ollama and nothing else, so its source names the
        // engine and its payload carries no per-engine map.
        state.handleNotification({
            source: 'proxy',
            method: 'node/discovered',
            params: { id: 'ollama-peer', host: 'ollama-peer', port: 11434, ip: '192.0.2.61' }
        })
        expect(state.isRemoteEngineRunning('ollama-peer', 'ollama')).toBe(true)
    })
})
