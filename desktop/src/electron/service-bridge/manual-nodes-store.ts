// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import fs from 'fs'
import path from 'path'
import { getPaths } from '@/electron/globals'
import type { ManualServicePorts } from '@/shared/types/manual-node'
import type { JsonObject, JsonValue } from './json-rpc-subprocess'

interface ManualNodeEntry {
    id: string
    address: string
    name: string
    /**
     * Per-service port overrides, persisted because the replay after a restart
     * is the only thing that re-creates the entry: without them a node reachable
     * on non-default ports comes back unreachable.
     */
    ports?: ManualServicePorts
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

/** A usable TCP port, or undefined for anything else. Narrowing, never a cast. */
function portValue(value: JsonValue | undefined): number | undefined {
    if (typeof value !== 'number' || !Number.isInteger(value)) return undefined
    if (value < 1 || value > 65535) return undefined
    return value
}

function portsValue(value: JsonValue | undefined): ManualServicePorts | undefined {
    const obj = objectValue(value)
    if (!obj) return undefined
    const ports: ManualServicePorts = {
        nodeInfo: portValue(obj.nodeInfo),
        cluster: portValue(obj.cluster),
        ollama: portValue(obj.ollama),
        lmstudio: portValue(obj.lmstudio),
        vllm: portValue(obj.vllm)
    }
    return definedPorts(ports)
}

/**
 * Drops every unset field, and the object itself when nothing is set. An empty
 * overrides object is not the same thing as none: it would persist and replay as
 * a value the operator never chose.
 */
function definedPorts(ports: ManualServicePorts): ManualServicePorts | undefined {
    const kept: ManualServicePorts = {}
    if (ports.nodeInfo !== undefined) kept.nodeInfo = ports.nodeInfo
    if (ports.cluster !== undefined) kept.cluster = ports.cluster
    if (ports.ollama !== undefined) kept.ollama = ports.ollama
    if (ports.lmstudio !== undefined) kept.lmstudio = ports.lmstudio
    if (ports.vllm !== undefined) kept.vllm = ports.vllm
    return Object.keys(kept).length > 0 ? kept : undefined
}

/**
 * Projects the overrides onto the snake_case field names `node/add` reads
 * (`node_info`, `cluster`, `ollama`, `lmstudio`, `vllm`). The two spellings meet
 * here and nowhere else.
 */
export function manualPortsToWire(ports: ManualServicePorts | undefined): JsonObject | undefined {
    if (!ports) return undefined
    const wire: JsonObject = {}
    if (ports.nodeInfo !== undefined) wire.node_info = ports.nodeInfo
    if (ports.cluster !== undefined) wire.cluster = ports.cluster
    if (ports.ollama !== undefined) wire.ollama = ports.ollama
    if (ports.lmstudio !== undefined) wire.lmstudio = ports.lmstudio
    if (ports.vllm !== undefined) wire.vllm = ports.vllm
    return Object.keys(wire).length > 0 ? wire : undefined
}

function entryValue(value: JsonValue | undefined): ManualNodeEntry | null {
    const obj = objectValue(value)
    if (!obj) return null

    const address = stringValue(obj.address)
    if (!address) return null

    const name = stringValue(obj.name) || address
    const id = stringValue(obj.id) || name
    const ports = portsValue(obj.ports)
    return ports ? { id, address, name, ports } : { id, address, name }
}

/**
 * Record a manually added node so it survives a restart, and hand back the entry
 * the broker should be told about.
 *
 * `nvpair-manual-nodes` keeps no durable state of its own — the list belongs to
 * the application — and the backend keys the node by `name`, which is the
 * address here so {@link resolveManualNodeKey} and the `node/remove` relay agree
 * on it. Re-adding the same address replaces its entry rather than duplicating
 * it, so changing a node's ports is just adding it again.
 */
export function addManualNodeEntry(address: string, ports?: ManualServicePorts): ManualNodeEntry {
    const trimmed = address.trim()
    const kept = ports ? definedPorts(ports) : undefined
    const entry: ManualNodeEntry = kept
        ? { id: trimmed, address: trimmed, name: trimmed, ports: kept }
        : { id: trimmed, address: trimmed, name: trimmed }
    const entries = listManualNodeEntries().filter(existing => existing.address !== trimmed)
    entries.push(entry)
    saveManualNodeEntries(entries)
    return entry
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
