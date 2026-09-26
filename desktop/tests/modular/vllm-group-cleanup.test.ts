// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import {
    bindVllmGroupCleanup,
    bindVllmGroupCleanupStatus,
    parseVllmGroupCleanupRequest,
    vllmGroupCleanupBinding,
    vllmGroupHoldsTarget
} from '@/shared/utils/vllm-group-cleanup'
import { HELD_RUN_ID, groupRun, heldStatus, inactiveStatus } from '../fixtures/vllm-group'

const digest = 'a'.repeat(64)
const binding = () => ({ runId: HELD_RUN_ID, generation: 13, planDigest: digest })

function cleanedStatus() {
    const status = heldStatus()
    status.reserved = false
    status.reason = ''
    status.run!.state = 'failed'
    status.run!.cleanupConfirmed = true
    status.run!.ranks.forEach(rank => {
        rank.cleanupConfirmed = true
    })
    return status
}

describe('serving-group cleanup binding', () => {
    it('binds exactly to the fresh reserved run and nothing else', () => {
        expect(vllmGroupCleanupBinding(heldStatus())).toEqual(binding())
        expect(vllmGroupCleanupBinding(inactiveStatus())).toBeNull()
        expect(
            vllmGroupCleanupBinding({ ...inactiveStatus(), run: groupRun('stopped') })
        ).toBeNull()
    })

    it('rejects arbitrary renderer strings', () => {
        const good = binding()
        expect(parseVllmGroupCleanupRequest(good)).toEqual(good)
        for (const bad of [
            null,
            [],
            { ...good, runId: good.runId.toUpperCase() },
            { ...good, runId: good.runId.slice(1) },
            { ...good, generation: 0 },
            { ...good, generation: -13 },
            { ...good, generation: 13.5 },
            { ...good, generation: '13' },
            { ...good, planDigest: digest.slice(1) },
            { ...good, planDigest: digest.toUpperCase() }
        ])
            expect(() => parseVllmGroupCleanupRequest(bad)).toThrow(/cleanup request/)
    })

    it('refuses a stale or cached identity against the fresh read and sends the fresh binding', () => {
        const fresh = heldStatus()
        const sent = bindVllmGroupCleanup(fresh, binding())
        expect(sent).toEqual(binding())
        expect(sent).not.toBe(binding())
        expect(() => bindVllmGroupCleanup(fresh, { ...binding(), generation: 12 })).toThrow(
            /changed/
        )
        expect(() =>
            bindVllmGroupCleanup(fresh, { ...binding(), planDigest: 'b'.repeat(64) })
        ).toThrow(/changed/)
        expect(() => bindVllmGroupCleanup(inactiveStatus(), binding())).toThrow(
            /nothing to clean up/
        )
    })

    it('publishes only a reply that names the exact operation, whatever its hold state', () => {
        const held = heldStatus()
        expect(bindVllmGroupCleanupStatus(held, binding())).toBe(held)
        expect(bindVllmGroupCleanupStatus(cleanedStatus(), binding()).run?.cleanupConfirmed).toBe(
            true
        )
        const drifted = heldStatus()
        drifted.run!.generation = 14
        expect(() => bindVllmGroupCleanupStatus(drifted, binding())).toThrow(/different operation/)
        expect(() => bindVllmGroupCleanupStatus(inactiveStatus(), binding())).toThrow(
            /different operation/
        )
    })
})

describe('serving-group target scoping', () => {
    const self = 'node-self'

    it('does not let an unsupported local platform hold remote targets while self stays held', () => {
        const unknownLocalUnsupported = { known: false, status: null, localVllmSupported: false }
        expect(vllmGroupHoldsTarget(unknownLocalUnsupported, self, self)).toBe(true)
        expect(vllmGroupHoldsTarget(unknownLocalUnsupported, 'node-linux', self)).toBe(false)
        expect(vllmGroupHoldsTarget(unknownLocalUnsupported, 'node-linux', null)).toBe(true)
    })

    it('keeps unknown status fail-closed on a host that could own a journal', () => {
        const unknownLocalSupported = { known: false, status: null, localVllmSupported: true }
        expect(vllmGroupHoldsTarget(unknownLocalSupported, self, self)).toBe(true)
        expect(vllmGroupHoldsTarget(unknownLocalSupported, 'node-linux', self)).toBe(true)
    })

    it('scopes a held run to its members and self', () => {
        const held = { known: true, status: heldStatus(), localVllmSupported: false }
        expect(vllmGroupHoldsTarget(held, self, self)).toBe(true)
        expect(vllmGroupHoldsTarget(held, 'node-b', self)).toBe(true)
        expect(vllmGroupHoldsTarget(held, 'node-d', self)).toBe(false)
    })

    it('releases every target once the retained run is clean', () => {
        const clean = { known: true, status: cleanedStatus(), localVllmSupported: true }
        expect(vllmGroupHoldsTarget(clean, self, self)).toBe(false)
        expect(vllmGroupHoldsTarget(clean, 'node-b', self)).toBe(false)
    })
})
