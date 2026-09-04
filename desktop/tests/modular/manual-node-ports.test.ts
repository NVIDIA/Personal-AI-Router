// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import fs from 'fs'
import os from 'os'
import path from 'path'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

let userDataDir = ''

vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/globals', () => ({
    getPaths: () => ({ getUserData: () => userDataDir })
}))

import {
    addManualNodeEntry,
    listManualNodeEntries,
    manualPortsToWire,
    removeManualNodeEntry,
    resolveManualNodeKey
} from '@/electron/service-bridge/manual-nodes-store'

// The durable manual-node list is what makes a peer on a network without
// multicast visible at all: nothing is ever discovered there, so the entry the
// application persists and replays is the node's only route into the directory.
// Its port overrides have to survive that round trip, or a node reachable on
// non-default ports comes back unreachable after a restart.
describe('manual node entries', () => {
    beforeEach(() => {
        userDataDir = fs.mkdtempSync(path.join(os.tmpdir(), 'pair-manual-nodes-'))
    })

    afterEach(() => {
        fs.rmSync(userDataDir, { recursive: true, force: true })
    })

    it('persists an address with no overrides and keys it by the address', () => {
        const entry = addManualNodeEntry('gpu-box.tail1234.ts.net')
        expect(entry).toEqual({
            id: 'gpu-box.tail1234.ts.net',
            address: 'gpu-box.tail1234.ts.net',
            name: 'gpu-box.tail1234.ts.net'
        })
        expect(listManualNodeEntries()).toEqual([entry])
    })

    it('round-trips port overrides through the persisted list', () => {
        addManualNodeEntry('gpu-box.tail1234.ts.net', { nodeInfo: 24318, ollama: 21434 })
        const [entry] = listManualNodeEntries()
        expect(entry.ports).toEqual({ nodeInfo: 24318, ollama: 21434 })
    })

    it('drops ports that are not usable, and the object when none are', () => {
        addManualNodeEntry('a.example', { nodeInfo: 0, ollama: 70000, cluster: 14321 })
        expect(listManualNodeEntries()[0].ports).toEqual({ cluster: 14321 })

        addManualNodeEntry('b.example', { nodeInfo: -1 })
        const b = listManualNodeEntries().find(entry => entry.address === 'b.example')
        expect(b?.ports).toBeUndefined()
    })

    it('replaces rather than duplicates when the same address is added again', () => {
        addManualNodeEntry('gpu-box.tail1234.ts.net', { ollama: 21434 })
        addManualNodeEntry('gpu-box.tail1234.ts.net', { ollama: 21435 })
        const entries = listManualNodeEntries()
        expect(entries).toHaveLength(1)
        expect(entries[0].ports).toEqual({ ollama: 21435 })
    })

    it('resolves a host-name entry from the node addresses the backend reports', () => {
        addManualNodeEntry('gpu-box.tail1234.ts.net')
        // A node added by name is reported by that name, since a name is a
        // dialable address like any other.
        expect(resolveManualNodeKey(['gpu-box.tail1234.ts.net'])).toBe('gpu-box.tail1234.ts.net')
        expect(resolveManualNodeKey(['192.0.2.10'])).toBeNull()
    })

    it('forgets an entry on removal', () => {
        addManualNodeEntry('gpu-box.tail1234.ts.net')
        removeManualNodeEntry('gpu-box.tail1234.ts.net')
        expect(listManualNodeEntries()).toEqual([])
    })

    it('ignores a persisted ports value that is not an object', () => {
        const file = path.join(userDataDir, 'configs', 'manual-nodes.json')
        fs.mkdirSync(path.dirname(file), { recursive: true })
        fs.writeFileSync(file, JSON.stringify([{ address: 'a.example', ports: 'nonsense' }]))
        expect(listManualNodeEntries()[0].ports).toBeUndefined()
    })
})

// The application spells the overrides in camelCase and the service reads them
// in snake_case. The two spellings meet in one projection, so a rename cannot
// silently drop a field on the way to the backend.
describe('manualPortsToWire', () => {
    it('projects every field onto the names node/add reads', () => {
        expect(
            manualPortsToWire({
                nodeInfo: 24318,
                cluster: 24321,
                ollama: 21434,
                lmstudio: 2234,
                vllm: 8001
            })
        ).toEqual({
            node_info: 24318,
            cluster: 24321,
            ollama: 21434,
            lmstudio: 2234,
            vllm: 8001
        })
    })

    it('omits unset fields, and the object itself when nothing is set', () => {
        expect(manualPortsToWire({ ollama: 21434 })).toEqual({ ollama: 21434 })
        expect(manualPortsToWire({})).toBeUndefined()
        expect(manualPortsToWire(undefined)).toBeUndefined()
    })
})
