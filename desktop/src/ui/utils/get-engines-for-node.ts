// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import {
    type EngineStatusData,
    type EngineStatusByNode,
    type EngineType
} from '@/shared/types/engines'
import { emptyEngineStatus, isEngineType } from '@/shared/utils/engines'
import { EnabledEngineTypes, EngineDisplayNames } from '@/shared/constants/engines'

interface EngineListRow {
    rowKey: string
    displayName: string
    processStatus: string
    port: number | null
}

/**
 * One row per enabled engine type for this node. Types not yet reported by the
 * service appear as `initializing` placeholders ({@link emptyEngineStatus}) so
 * compact rows can hide them until evidence arrives. Types the node cannot run
 * are left out. When `engineFilter` is provided, only those types are included.
 */
export function getEnginesForNode(
    nodeId: string,
    statusByNode: EngineStatusByNode,
    engineFilter?: EngineType[]
): EngineStatusData[] {
    const inner = statusByNode.get(nodeId)
    const types = engineFilter ?? EnabledEngineTypes
    return types
        .map(type => inner?.get(type) ?? emptyEngineStatus(nodeId, type))
        .filter(s => s.processStatus !== 'unsupported')
}

function engineDisplayName(engineType: string): string {
    if (isEngineType(engineType)) {
        return EngineDisplayNames[engineType]
    }
    return engineType
}

/** True if any node in the cluster has this engine type running. */
export function isEngineTypeRunningClusterWide(
    statusByNode: EngineStatusByNode,
    engineType: EngineType
): boolean {
    for (const inner of statusByNode.values()) {
        const s = inner.get(engineType)
        if (s?.processStatus === 'running') return true
    }
    return false
}

/**
 * The proxy port to offer for this engine on `nodeId`'s own API endpoint.
 * A node publishes its facade port for engines it can run; the facade port is
 * the same per-engine constant on every node. So when the local node cannot
 * host an engine (an engine unsupported on its OS has no status row at all),
 * the port reported by a node that runs the engine is valid evidence for
 * routing through the local proxy, which serves remote engines cluster-wide.
 * The local node's own port always wins when present.
 */
export function endpointProxyPortForNode(
    nodeId: string,
    statusByNode: EngineStatusByNode,
    engineType: EngineType
): number | null {
    const local = statusByNode.get(nodeId)?.get(engineType)
    if (local?.proxyPort) return local.proxyPort
    if (!isEngineTypeRunningClusterWide(statusByNode, engineType)) return null
    for (const inner of statusByNode.values()) {
        const s = inner.get(engineType)
        if (s?.processStatus === 'running' && s.proxyPort) return s.proxyPort
    }
    return null
}

export function engineStatusesToListRows(engineStatuses: EngineStatusData[]): EngineListRow[] {
    return engineStatuses.map(s => ({
        rowKey: `${s.nodeId}:${s.engineType}`,
        displayName: engineDisplayName(s.engineType),
        processStatus: s.processStatus,
        port: s.enginePort
    }))
}
