// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'

import { endpointProxyPortForNode } from '@/ui/utils/get-engines-for-node'
import { emptyEngineStatus, unsupportedEngineStatus } from '@/shared/utils/engines'
import type { EngineStatusByNode, EngineStatusData, EngineType } from '@/shared/types/engines'

function status(
    nodeId: string,
    engineType: EngineType,
    processStatus: EngineStatusData['processStatus'],
    proxyPort: number | null
): EngineStatusData {
    return {
        engineType,
        nodeId,
        processStatus,
        enginePort: null,
        proxyPort
    }
}

function statusesByNode(entries: Array<[string, EngineStatusData[]]>): EngineStatusByNode {
    return new Map(
        entries.map(([nodeId, rows]) => [
            nodeId,
            new Map<EngineType, EngineStatusData>(rows.map(r => [r.engineType, r]))
        ])
    )
}

// A client node whose OS cannot host an engine reports no usable status row
// for it, while a peer runs the engine. The local proxy facade serves that
// engine cluster-wide, so its API endpoint tile must appear with the port the
// running node published.
describe('endpointProxyPortForNode', () => {
    it('falls back to a running remote node port when the local node cannot host the engine', () => {
        const byNode = statusesByNode([
            ['laptop', [unsupportedEngineStatus('laptop', 'vllm')]],
            ['rig', [status('rig', 'vllm', 'running', 8000)]]
        ])
        expect(endpointProxyPortForNode('laptop', byNode, 'vllm')).toBe(8000)
    })

    it('treats a missing local row the same as an unsupported one', () => {
        const byNode = statusesByNode([
            ['laptop', [emptyEngineStatus('laptop', 'ollama')]],
            ['rig', [status('rig', 'ollama', 'running', 11434)]]
        ])
        expect(endpointProxyPortForNode('laptop', byNode, 'ollama')).toBe(11434)
    })

    it('prefers the local node port when the local node reports one', () => {
        const byNode = statusesByNode([
            ['laptop', [status('laptop', 'lm-studio', 'running', 1234)]],
            ['rig', [status('rig', 'lm-studio', 'stopped', 1234)]]
        ])
        expect(endpointProxyPortForNode('laptop', byNode, 'lm-studio')).toBe(1234)
    })

    it('stays silent while no node runs the engine', () => {
        const byNode = statusesByNode([
            ['laptop', [unsupportedEngineStatus('laptop', 'vllm')]],
            ['rig', [status('rig', 'vllm', 'stopped', 8000)]]
        ])
        expect(endpointProxyPortForNode('laptop', byNode, 'vllm')).toBeNull()
    })

    it('returns null when a running node has not published a proxy port yet', () => {
        const byNode = statusesByNode([
            ['laptop', []],
            ['rig', [status('rig', 'vllm', 'running', null)]]
        ])
        expect(endpointProxyPortForNode('laptop', byNode, 'vllm')).toBeNull()
    })
})
