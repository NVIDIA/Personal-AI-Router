// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { EngineModels, EngineStatusData } from '@/shared/types/engines'
import type { NodeItem } from '@/shared/types/nodes'

export interface VllmServingNode {
    nodeId: string
    nodeName: string
    gpuCount: number
    processStatus: EngineStatusData['processStatus'] | 'unknown'
    loadedModels: number
    downloadedModels: number
    ready: boolean
}

export interface VllmServingReadiness {
    nodes: VllmServingNode[]
    servingReplicas: number
    reusableModels: number
    replicatedModels: number
    controlsNodeId: string | null
    distributionSupported: boolean
}

export function buildVllmServingReadiness(
    nodes: NodeItem[],
    statuses: EngineStatusData[],
    modelSets: EngineModels[]
): VllmServingReadiness {
    const statusByNode = new Map(
        statuses
            .filter(status => status.engineType === 'vllm')
            .map(status => [status.nodeId, status])
    )
    const modelsByNode = new Map(
        modelSets
            .filter(models => models.engineType === 'vllm')
            .map(models => [models.nodeId, models])
    )
    const placements = new Map<string, Set<string>>()

    const rows = nodes
        .map(node => {
            const status = statusByNode.get(node.id)
            const models = modelsByNode.get(node.id)?.models ?? []
            const downloaded = models.filter(model => model.downloaded)
            const loaded = downloaded.filter(model => model.status === 'loaded')
            for (const model of downloaded) {
                const owners = placements.get(model.name) ?? new Set<string>()
                owners.add(node.id)
                placements.set(model.name, owners)
            }
            return {
                nodeId: node.id,
                nodeName: node.name || node.id,
                gpuCount: node.topology.gpus.length,
                processStatus: status?.processStatus ?? 'unknown',
                loadedModels: loaded.length,
                downloadedModels: downloaded.length,
                ready:
                    status?.processStatus === 'running' &&
                    status.routable === true &&
                    loaded.length > 0
            } satisfies VllmServingNode
        })
        .sort((a, b) => a.nodeName.localeCompare(b.nodeName))

    const controls =
        rows.find(row => row.ready) ??
        rows.find(row => row.processStatus === 'running') ??
        rows.find(row => row.processStatus !== 'unknown' && row.processStatus !== 'not-installed')

    return {
        nodes: rows,
        servingReplicas: rows.filter(row => row.ready).length,
        reusableModels: placements.size,
        replicatedModels: Array.from(placements.values()).filter(owners => owners.size > 1).length,
        controlsNodeId: controls?.nodeId ?? null,
        distributionSupported: rows.filter(row => row.processStatus !== 'unknown').length >= 2
    }
}
