// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import fs from 'fs'
import path from 'path'
import { getPaths } from '@/electron/globals'
import type { JsonObject, JsonValue } from './json-rpc-subprocess'

interface ManualNodeEntry {
    id: string
    address: string
    name: string
}

function configFilePath(): string {
    return path.join(getPaths().getUserData(), 'configs', 'manual-nodes.json')
}

function objectValue(value: JsonValue | undefined): JsonObject | null {
    if (!value || typeof value !== 'object' || Array.isArray(value)) return null
    return value
}

function stringValue(value: JsonValue | undefined): string {
    return typeof value === 'string' ? value : ''
}

function entryValue(value: JsonValue | undefined): ManualNodeEntry | null {
    const obj = objectValue(value)
    if (!obj) return null

    const address = stringValue(obj.address)
    if (!address) return null

    const name = stringValue(obj.name) || address
    const id = stringValue(obj.id) || name
    return { id, address, name }
}

export function listManualNodeEntries(): ManualNodeEntry[] {
    try {
        const filePath = configFilePath()
        if (!fs.existsSync(filePath)) return []

        const raw = fs.readFileSync(filePath, 'utf8')
        const parsed: JsonValue = JSON.parse(raw)
        if (!Array.isArray(parsed)) return []

        const entries: ManualNodeEntry[] = []
        for (const item of parsed) {
            const entry = entryValue(item)
            if (entry) entries.push(entry)
        }
        return entries
    } catch {
        return []
    }
}

export function removeManualNodeEntry(nodeId: string): void {
    const entries = listManualNodeEntries().filter(
        entry => entry.id !== nodeId && entry.address !== nodeId && entry.name !== nodeId
    )
    saveManualNodeEntries(entries)
}

/**
 * Resolve a persisted manual entry from a set of a node's reachable addresses to
 * the key `nvpair-manual-nodes` uses for it. `replayManualNodes` re-adds entries
 * via `node/add` with `{ address, name }`, and the backend keys the resulting
 * manual node by that `name` (`nodeID(entry)`), so returning `entry.name` gives a
 * key that both {@link removeManualNodeEntry} and the broker `node/remove` relay
 * match. A node's stable UUID never matches (it is not what the entry is keyed
 * by), so callers must map it back through the node's addresses. Returns null
 * when no persisted entry owns any of `addresses` (i.e. not a manual node).
 */
export function resolveManualNodeKey(addresses: readonly string[]): string | null {
    if (addresses.length === 0) return null
    const known = new Set(addresses)
    for (const entry of listManualNodeEntries()) {
        if (known.has(entry.address)) return entry.name
    }
    return null
}

function saveManualNodeEntries(entries: ManualNodeEntry[]): void {
    try {
        const filePath = configFilePath()
        fs.mkdirSync(path.dirname(filePath), { recursive: true })
        const tmp = `${filePath}.tmp`
        fs.writeFileSync(tmp, JSON.stringify(entries, null, 2), 'utf8')
        fs.renameSync(tmp, filePath)
    } catch {
        /* best-effort */
    }
}
