// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import {
    EngineManagerNames,
    EnginesListingModelsWhenStopped,
    EngineTypes,
    ModelExpiries
} from '@/shared/constants/engines'
import {
    EngineManagerName,
    EngineModels,
    EngineStatusData,
    EngineType,
    ModelExpiry
} from '@/shared/types/engines'

const ENGINE_TYPE_SET: ReadonlySet<string> = new Set(EngineTypes)
/**
 * Runtime type guard — narrows a `string` (typically arriving from service
 * bridge payloads or persisted JSON) into the closed `EngineType` union. Apply
 * at every wire/persistence boundary; everything inside the service then keeps
 * strict typing.
 */
export function isEngineType(v: string | undefined): v is EngineType {
    return typeof v === 'string' && ENGINE_TYPE_SET.has(v)
}

/** Spell an `EngineType` the way `nvpair-engine-manager` does on the wire. */
export function engineManagerName(engine: EngineType): EngineManagerName {
    return EngineManagerNames[engine]
}

const ENGINE_TYPE_BY_MANAGER_NAME: ReadonlyMap<string, EngineType> = new Map(
    EngineTypes.map(engine => [EngineManagerNames[engine], engine])
)
/**
 * Narrow an engine id arriving from `nvpair-engine-manager` into an
 * `EngineType`, or null for an engine this build does not ship.
 */
export function engineTypeFromManagerName(name: string): EngineType | null {
    return ENGINE_TYPE_BY_MANAGER_NAME.get(name) ?? null
}

const MODEL_EXPIRY_SET: ReadonlySet<string> = new Set(ModelExpiries)
/** Runtime type guard — validates a string is a known ModelExpiry. */
export function isModelExpiry(v: string | undefined): v is ModelExpiry {
    return v !== undefined && MODEL_EXPIRY_SET.has(v)
}

/** Shallow placeholder when the service has not yet reported this node/engine pair. */
export function emptyEngineStatus(nodeId: string, engineType: EngineType): EngineStatusData {
    return {
        engineType,
        nodeId,
        processStatus: 'initializing',
        enginePort: null,
        proxyPort: null
    }
}

/** An engine the node's engine manager does not list, because the node cannot run it. */
export function unsupportedEngineStatus(nodeId: string, engineType: EngineType): EngineStatusData {
    return {
        engineType,
        nodeId,
        processStatus: 'unsupported',
        enginePort: null,
        proxyPort: null
    }
}

/** The two `EngineStatusData.owner` spellings that name no tool behind the copy. */
const OWNERS_WITHOUT_A_TOOL: ReadonlySet<string> = new Set(['pair', 'other'])

/**
 * True when the copy of an engine on a node is the one PAIR installed — the only
 * copy PAIR may remove, so the only one worth offering Uninstall for.
 */
export function isPairManagedInstall(owner: string | undefined): boolean {
    return owner === 'pair'
}

/**
 * The tool to name for a copy PAIR must not remove, which is the package manager
 * that owns it. Null for a copy PAIR made itself, one that was merely found, and
 * for a node where nothing is installed: none of those have a tool to name.
 */
export function engineOwnerTool(owner: string | undefined): string | null {
    if (!owner || OWNERS_WITHOUT_A_TOOL.has(owner)) return null
    return owner
}

/**
 * Shallow placeholder when no model list has been pushed for this node/engine
 * yet. For an engine whose list is read from disk and shown while it is stopped,
 * no list yet means unknown, not "nothing downloaded".
 */
export function emptyEngineModels(nodeId: string, engineType: EngineType): EngineModels {
    return {
        nodeId,
        engineType,
        models: [],
        inventoryKnown: !EnginesListingModelsWhenStopped.includes(engineType)
    }
}
