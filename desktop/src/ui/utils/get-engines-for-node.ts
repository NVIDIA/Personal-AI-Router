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
 * compact rows can hide them until evidence arrives. When `engineFilter` is
 * provided, only those types are included.
 */
export function getEnginesForNode(
    nodeId: string,
    statusByNode: EngineStatusByNode,
    engineFilter?: EngineType[]
): EngineStatusData[] {
    const inner = statusByNode.get(nodeId)
    const types = engineFilter ?? EnabledEngineTypes
    return types.map(type => {
        const s = inner?.get(type)
        if (s) return s
        return emptyEngineStatus(nodeId, type)
    })
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

export function engineStatusesToListRows(engineStatuses: EngineStatusData[]): EngineListRow[] {
    return engineStatuses.map(s => ({
        rowKey: `${s.nodeId}:${s.engineType}`,
        displayName: engineDisplayName(s.engineType),
        processStatus: s.processStatus,
        port: s.enginePort
    }))
}
