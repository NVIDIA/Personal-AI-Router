// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest'

vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import type { EngineStatusByNode, EngineStatusData } from '@/shared/types/engines'
import { getModularBridgeState } from '@/electron/service-bridge/modular-state'
import { getEnginesForNode } from '@/ui/utils/get-engines-for-node'

// An engine manager lists only the engines its host can run. On Windows the
// listing has no vLLM, the way these do.
const windowsListing = {
    engines: [
        { engine: 'ollama', installed: true, running: true, healthy: true, port: 11434 },
        { engine: 'lmstudio', installed: false, running: false, healthy: false, port: 0 },
        { engine: 'llamacpp', installed: false, running: false, healthy: false, port: 0 }
    ]
}

function statusOf(nodeId: string, engineType: string): EngineStatusData | undefined {
    return getModularBridgeState()
        .getEngineInitialState()
        .statuses.find(status => status.nodeId === nodeId && status.engineType === engineType)
}

function byNode(statuses: EngineStatusData[]): EngineStatusByNode {
    const out: EngineStatusByNode = new Map()
    for (const status of statuses) {
        const inner = out.get(status.nodeId) ?? new Map()
        inner.set(status.engineType, status)
        out.set(status.nodeId, inner)
    }
    return out
}

describe('engines a node cannot run', () => {
    it('marks an engine the local engine manager leaves out as unsupported', () => {
        const state = getModularBridgeState()
        state.setSelfId('windows-self')

        state.applyEngineManagerList(windowsListing)

        expect(statusOf('windows-self', 'vllm')).toMatchObject({ processStatus: 'unsupported' })
        expect(statusOf('windows-self', 'lm-studio')).toMatchObject({
            processStatus: 'not-installed'
        })
    })

    it('marks an engine a peer leaves out as unsupported', () => {
        const state = getModularBridgeState()
        const peerId = 'windows-peer'
        state.setSelfId('linux-self')
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: {
                nodes: [
                    {
                        hostUuid: peerId,
                        name: 'windows-peer',
                        ipAddress: '192.0.2.70',
                        port: 14318,
                        trusted: true,
                        clustered: true
                    }
                ]
            }
        })

        state.applyRemoteEngineFacts(peerId, windowsListing)

        expect(statusOf(peerId, 'vllm')).toMatchObject({ processStatus: 'unsupported' })
        expect(statusOf(peerId, 'lm-studio')).toMatchObject({ processStatus: 'not-installed' })
    })

    it('keeps an engine a peer has not listed yet as unknown, not unsupported', () => {
        const state = getModularBridgeState()
        const peerId = 'unpolled-peer'
        state.setSelfId('linux-self')
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: {
                nodes: [
                    {
                        hostUuid: peerId,
                        name: 'unpolled-peer',
                        ipAddress: '192.0.2.71',
                        port: 14318,
                        trusted: true,
                        clustered: true
                    }
                ]
            }
        })

        state.applyRemoteEngineFacts(peerId, { engines: [] })

        expect(statusOf(peerId, 'vllm')).toBeUndefined()
    })

    it('gives an unsupported engine no row, and keeps one for an unreported engine', () => {
        const nodeId = 'windows-node'
        const statuses = byNode([
            {
                engineType: 'ollama',
                nodeId,
                processStatus: 'running',
                enginePort: 11434,
                proxyPort: null
            },
            {
                engineType: 'vllm',
                nodeId,
                processStatus: 'unsupported',
                enginePort: null,
                proxyPort: null
            }
        ])

        const rows = getEnginesForNode(nodeId, statuses).map(s => [s.engineType, s.processStatus])

        expect(rows).toEqual([
            ['ollama', 'running'],
            ['lm-studio', 'initializing'],
            ['llama-cpp', 'initializing']
        ])
    })
})
