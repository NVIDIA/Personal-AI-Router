// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import {
    type EngineStatusData,
    type EngineStatusByNode,
    type EngineType
} from '@/shared/types/engines'
import { emptyEngineStatus, isEngineType } from '@/shared/utils/engines'
import {
    EnabledEngineTypes,
    MANIFEST_ENGINE_TYPES,
    EngineDisplayNames
} from '@/shared/constants/engines'
import type { CustomEndpointInfo } from '@/shared/types/custom-endpoints'

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

export function engineStatusesToListRows(engineStatuses: EngineStatusData[]): EngineListRow[] {
    return engineStatuses.map(s => ({
        rowKey: `${s.nodeId}:${s.engineType}`,
        displayName: engineDisplayName(s.engineType),
        processStatus: s.processStatus,
        port: s.enginePort
    }))
}

/**
 * The tray's compact `Engines (n/m)` count for one node.
 *
 * `online` counts engines accepting work right now: `running` manifest
 * engines plus custom endpoints whose latest probe answered healthy.
 *
 * `total` counts engines the node has: manifest entries the node reported
 * with an installed process state, plus configured custom endpoints. The
 * explicit `not-installed` report proves absence and `initializing` is only
 * the desktop's placeholder for a pair the service never reported — counting
 * either (as the padded manifest row list once did) made a node with nothing
 * installed read `0/3` instead of `0/0`.
 *
 * Custom is counted per endpoint from the node's `custom/list` snapshot, not
 * from the engine-status map: the Custom facade fronts every endpoint on the
 * node behind one status, so the map cannot say how many endpoints are
 * configured or individually healthy. A `custom` entry in `reported`, when
 * one arrives, is therefore ignored here.
 */
export function countNodeEngineServices(
    reported: ReadonlyMap<EngineType, EngineStatusData> | undefined,
    customEntries: readonly CustomEndpointInfo[] | undefined
): { online: number; total: number } {
    let online = 0
    let total = 0
    if (reported) {
        for (const type of MANIFEST_ENGINE_TYPES) {
            const status = reported.get(type)?.processStatus
            if (!status || status === 'not-installed' || status === 'initializing') continue
            total += 1
            if (status === 'running') online += 1
        }
    }
    for (const entry of customEntries ?? []) {
        total += 1
        if (entry.healthy === true) online += 1
    }
    return { online, total }
}
