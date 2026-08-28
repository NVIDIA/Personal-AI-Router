// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { EngineTypes, ModelExpiries } from '@/shared/constants/engines'
import { EngineModels, EngineStatusData, EngineType, ModelExpiry } from '@/shared/types/engines'

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

/** Shallow placeholder when no model list has been pushed for this node/engine yet. */
export function emptyEngineModels(nodeId: string, engineType: EngineType): EngineModels {
    return { nodeId, engineType, models: [] }
}
