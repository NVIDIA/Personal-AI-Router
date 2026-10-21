// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { EngineManagerNames, EngineTypes, ModelExpiries } from '@/shared/constants/engines'
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

const IDLE_SECONDS: Partial<Record<ModelExpiry, number>> = {
    '1m': 60,
    '5m': 300,
    '10m': 600,
    '30m': 1800,
    '1h': 3600
}

/** The idle limit in seconds an expiry value stands for; 0 is never. Undefined for a value that is not an idle limit. */
export function expiryToIdleSeconds(expiry: ModelExpiry): number | undefined {
    return expiry === '-1' ? 0 : IDLE_SECONDS[expiry]
}

/**
 * The offered expiry value closest to an idle limit in seconds, for display. A
 * limit the settings never offer, such as one set by editing a file, shows as the
 * nearest one. 0 is never.
 */
export function idleSecondsToExpiry(seconds: number): ModelExpiry {
    if (seconds <= 0) return '-1'
    let best: ModelExpiry = '10m'
    let distance = Infinity
    for (const [expiry, value] of Object.entries(IDLE_SECONDS)) {
        if (isModelExpiry(expiry) && Math.abs(value - seconds) < distance) {
            best = expiry
            distance = Math.abs(value - seconds)
        }
    }
    return best
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

/** Shallow placeholder when no model list has been pushed for this node/engine yet. */
export function emptyEngineModels(nodeId: string, engineType: EngineType): EngineModels {
    return { nodeId, engineType, models: [] }
}
