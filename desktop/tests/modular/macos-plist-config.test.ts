// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'

/**
 * electron-builder.config.ts has module-level side effects that throw when
 * `--mac` / `--win` / `--linux` is not present on the command line, so we
 * cannot import it directly. Instead we read the source text and verify the
 * keys structurally — this is sufficient because electron-builder consumes
 * them from the same literal object.
 */
const configSource = readFileSync(
    resolve(__dirname, '../../electron-builder.config.ts'),
    'utf-8'
)

describe('macOS Info.plist configuration', () => {
    it('declares NSLocalNetworkUsageDescription with a non-empty string', () => {
        expect(configSource).toContain('NSLocalNetworkUsageDescription')

        // Verify the key is assigned a non-empty string literal (single- or
        // double-quoted), not just mentioned in a comment.
        const match = /NSLocalNetworkUsageDescription:\s*\n?\s*['"](.+?)['"]/s.exec(
            configSource
        )
        expect(match).not.toBeNull()
        expect(match?.[1].length).toBeGreaterThan(0)
    })

    it('declares NSBonjourServices containing the PAIR node scanner service type', () => {
        expect(configSource).toContain('NSBonjourServices')
        expect(configSource).toContain('_nvpair-node._tcp')
    })
})
