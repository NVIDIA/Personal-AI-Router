// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import { redactSensitiveLogText } from '@/electron/redact-log'

/**
 * The pairing PIN reaches three log sinks through the broker's stdout: the
 * in-memory service log, the on-disk `nvpair.jsonl`, and the user-shareable
 * debug-log export. `nvpair-cluster-manager` puts it in the
 * `cluster:invite-node` result and on the `Invite` payload of
 * `cluster:invite-received` / `cluster:invite-expired`, so both the inviting and
 * the invited side can leak it.
 */
describe('redactSensitiveLogText', () => {
    it('redacts the PIN from a cluster:invite-node result', () => {
        const frame = JSON.stringify({
            jsonrpc: '2.0',
            id: 7,
            result: { inviteId: 'abc-123', state: 'pending', pin: '481920' }
        })

        const out = redactSensitiveLogText(frame)

        expect(out).not.toContain('481920')
        expect(out).toContain('[redacted]')
        // Non-secret context must survive so the log stays useful.
        expect(out).toContain('abc-123')
        expect(out).toContain('pending')
    })

    it('redacts the PIN carried on an invite notification payload', () => {
        const frame = JSON.stringify({
            jsonrpc: '2.0',
            method: 'cluster:invite-received',
            params: { invite: { inviteId: 'x', pin: '135790', fromNodeId: 'peer-1' } }
        })

        const out = redactSensitiveLogText(frame)

        expect(out).not.toContain('135790')
        expect(out).toContain('peer-1')
    })

    it('keeps a null pin distinguishable from a redacted one', () => {
        const out = redactSensitiveLogText(JSON.stringify({ invite: { pin: null } }))

        expect(out).toContain('null')
        expect(out).not.toContain('[redacted]')
    })

    it('does not touch similarly named fields such as pinnedAt', () => {
        const out = redactSensitiveLogText(
            JSON.stringify({ peer: { pinnedAt: 1750000000, pin: '246810' } })
        )

        expect(out).toContain('1750000000')
        expect(out).not.toContain('246810')
    })

    it('redacts a PIN in output that is not valid JSON', () => {
        const out = redactSensitiveLogText('cluster-manager: emitted {"pin": "998877"} <truncated')

        expect(out).not.toContain('998877')
        expect(out).toContain('[redacted]')
    })

    it('passes through unrelated output untouched', () => {
        const line = JSON.stringify({ jsonrpc: '2.0', method: 'app:ready' })

        expect(redactSensitiveLogText(line)).toBe(line)
    })

    it('is linear-time on hostile input', () => {
        const hostile = `{"pin":"${'a'.repeat(200_000)}`
        const start = performance.now()
        redactSensitiveLogText(hostile)
        const elapsed = performance.now() - start

        expect(elapsed).toBeLessThan(500)
    })
})
