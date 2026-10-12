// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type {
    EngineSettingsConfig,
    EngineSettingsSnapshot,
    EngineSettingsPreview,
    EngineSettingsReceipt,
    EngineSettingsTarget
} from '@/shared/types/engine-settings'
import { engineTypeFromManagerName } from '@/shared/utils/engines'
import type { JsonObject, JsonValue } from './json-rpc-subprocess'

function object(value: JsonValue | undefined): JsonObject {
    if (!value || typeof value !== 'object' || Array.isArray(value))
        throw new Error('This device does not support engine settings.')
    return value
}
function text(value: JsonValue | undefined): string {
    return typeof value === 'string' ? value : ''
}
function number(value: JsonValue | undefined): number {
    if (typeof value !== 'number' || !Number.isSafeInteger(value) || value < 0)
        throw new Error('Invalid engine settings response.')
    return value
}
function flag(value: JsonValue | undefined): boolean {
    if (typeof value !== 'boolean') throw new Error('Invalid engine settings response.')
    return value
}
function isSettingsEngine(value: JsonValue | undefined): value is EngineSettingsTarget['engine'] {
    return typeof value === 'string' && engineTypeFromManagerName(value) !== null
}
function isSnapshotPhase(value: JsonValue | undefined): value is EngineSettingsSnapshot['phase'] {
    return isReceiptPhase(value) || value === 'idle'
}
function isReceiptPhase(value: JsonValue | undefined): value is EngineSettingsReceipt['phase'] {
    return value === 'applying' || value === 'succeeded' || value === 'failed'
}
function config(value: JsonValue | undefined): EngineSettingsConfig {
    const row = object(value)
    return {
        serverPort: number(row.serverPort),
        proxyPort: number(row.proxyPort),
        launchText: text(row.launchText)
    }
}
export function parseEngineSettings(value: JsonValue | undefined): EngineSettingsSnapshot {
    const row = object(value)
    const engine = row.engine
    const phase = row.phase
    if (!isSettingsEngine(engine) || !isSnapshotPhase(phase) || !text(row.epoch))
        throw new Error('Invalid engine settings response.')
    return {
        nodeId: text(row.nodeId),
        engine,
        phase,
        epoch: text(row.epoch),
        sequence: number(row.sequence),
        revision: number(row.revision),
        appliedRevision: number(row.appliedRevision),
        settings: config(row.settings),
        effectiveServerPort: number(row.effectiveServerPort),
        effectiveProxyPort: number(row.effectiveProxyPort),
        running: flag(row.running),
        adopted: flag(row.adopted),
        editable: flag(row.editable),
        reason: text(row.reason),
        // Preserve the target's argument format so incompatible devices stay read-only.
        format: text(row.format),
        error: text(row.error),
        requestId: text(row.requestId)
    }
}
export function parseEngineSettingsPreview(value: JsonValue | undefined): EngineSettingsPreview {
    const row = object(value)
    const errors: Record<string, string> = {}
    if (row.errors)
        for (const [key, value] of Object.entries(object(row.errors))) errors[key] = text(value)
    const conflict = row.conflict ? object(row.conflict) : undefined
    return {
        settings: config(row.settings),
        revision: number(row.revision),
        errors,
        restart: flag(row.restart),
        rebind: flag(row.rebind),
        ...(conflict
            ? {
                  conflict: {
                      serverPort: number(conflict.serverPort),
                      launchPort: number(conflict.launchPort)
                  }
              }
            : {})
    }
}
export function parseEngineSettingsReceipt(value: JsonValue | undefined): EngineSettingsReceipt {
    const row = object(value)
    const phase = row.phase
    if (!isReceiptPhase(phase)) throw new Error('Invalid operation receipt.')
    return { revision: number(row.revision), phase }
}
