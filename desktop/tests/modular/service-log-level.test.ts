// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import { serviceLogLevel } from '@/electron/service-bridge/service-log-level'

/**
 * The services log every severity to stderr, so classifying a line by the
 * stream it arrived on filed routine debug and info traffic as warnings — the
 * bulk of the debug panel — and buried the warnings that mattered among them.
 * The line already states its severity; these pin that it is what gets used.
 */
describe('serviceLogLevel', () => {
    it('takes the severity from the line, not the stream', () => {
        const cases: [string, string][] = [
            ['10:57:00.123 [nvpair-node-scanner] DEBUG browse tick ifaces=3', 'verbose'],
            ['10:57:00.123 [nvpair-node-scanner] INFO advertised addresses re-ranked', 'info'],
            ['10:57:00.123 [ollama-proxy] WARN upstream refused connection', 'warn'],
            ['10:57:00.123 [nvpair-ui-broker] ERROR worker exited code=2', 'error']
        ]
        for (const [line, want] of cases) {
            expect(serviceLogLevel('stderr', line)).toBe(want)
        }
    })

    it('does not read a severity out of the message body', () => {
        // The tag is only a claim about the line when the handler wrote it as
        // one. Anywhere else it is just a word a peer's error text contained.
        const line = '10:57:00.123 [nvpair-errors] INFO cleared ERROR entry for peer-1'
        expect(serviceLogLevel('stderr', line)).toBe('info')
    })

    it('falls back to the stream for output the services did not format', () => {
        // A runtime panic or a third-party tool states no severity, and an
        // unstructured stderr line is the shape a crash takes.
        expect(serviceLogLevel('stderr', 'panic: runtime error: index out of range')).toBe('warn')
        expect(serviceLogLevel('stdout', '{"jsonrpc":"2.0","method":"ready"}')).toBe('verbose')
    })
})
