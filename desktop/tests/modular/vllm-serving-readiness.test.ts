// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import type { EngineModels, EngineStatusData, ModelItem } from '@/shared/types/engines'
import type { NodeItem } from '@/shared/types/nodes'
import { buildVllmServingReadiness } from '@/ui/utils/vllm-serving-readiness'

function node(id: string): NodeItem {
    return {
        id,
        name: id.toUpperCase(),
        status: 'active',
        ipAddress: `192.0.2.${id === 'a' ? 1 : 2}`,
        port: 0,
        allIpAddresses: [],
        topology: {
            cpu: { model: '', cores: 0, threads: 0 },
            gpus: [{ id: 'gpu0', name: 'GPU', vramTotal: 1 }],
            ram: 1,
            storage: []
        },
        os: 'Linux'
    }
}

function status(nodeId: string, routable: boolean): EngineStatusData {
    return {
        nodeId,
        engineType: 'vllm',
        processStatus: 'running',
        enginePort: 8000,
        proxyPort: 58080,
        routable
    }
}

function model(name: string, loaded = false): ModelItem {
    return {
        name,
        size: 1,
        downloaded: true,
        status: loaded ? 'loaded' : 'idle',
        parameterSize: '',
        quantization: '',
        family: '',
        digest: '',
        sizeVram: null,
        expiresAt: null,
        expiry: '10m',
        capabilities: []
    }
}

function models(nodeId: string, items: ModelItem[]): EngineModels {
    return { nodeId, engineType: 'vllm', models: items }
}

describe('buildVllmServingReadiness', () => {
    it('derives replicas and reusable model placement from current backend facts', () => {
        const result = buildVllmServingReadiness(
            [node('a'), node('b')],
            [status('a', true), status('b', false)],
            [models('a', [model('shared', true), model('only-a')]), models('b', [model('shared')])]
        )
        expect(result.servingReplicas).toBe(1)
        expect(result.reusableModels).toBe(2)
        expect(result.replicatedModels).toBe(1)
        expect(result.controlsNodeId).toBe('a')
        expect(result.distributionSupported).toBe(true)
    })
})
