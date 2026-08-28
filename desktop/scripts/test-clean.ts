// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Reclaim disk and detached state from interrupted test runs.
 *
 * Removes:
 *   - `pair-test-*` directories under `os.tmpdir()` — Vitest worker userdata
 *     roots (`pair-test-worker-*`, created by `tests/fixtures/isolation.ts`)
 *     and per-test roots (`pair-test-*`, created by `tests/fixtures/tmpdir.ts`)
 *     that an interrupted run left behind before its `afterEach` disposer ran.
 *   - `pair-e2e-*` directories under `os.tmpdir()` left by retired E2E runs.
 *
 * Does NOT touch the user's real PAIR data:
 *   - Real `userData` lives under platform-specific app dirs (e.g.
 *     `%AppData%/PAIR`), never under `os.tmpdir()`,
 *     and is matched by name prefix only.
 */

import fs from 'fs'
import os from 'os'
import path from 'path'

// `pair-test-` subsumes the worker roots (`pair-test-worker-`) and per-test
// roots (`pair-test-<rand>`).
const TEST_TMP_PREFIXES = ['pair-test-', 'pair-e2e-']

function removeTmpdirs(): { removed: number; bytes: number } {
    const tmp = os.tmpdir()
    let removed = 0
    let bytes = 0
    for (const entry of fs.readdirSync(tmp, { withFileTypes: true })) {
        if (!entry.isDirectory()) continue
        const matches = TEST_TMP_PREFIXES.some(p => entry.name.startsWith(p))
        if (!matches) continue
        const full = path.join(tmp, entry.name)
        try {
            bytes += dirSize(full)
            fs.rmSync(full, { recursive: true, force: true, maxRetries: 5, retryDelay: 200 })
            removed++
        } catch (err) {
            console.warn(`[test-clean] could not remove ${full}: ${String(err)}`)
        }
    }
    return { removed, bytes }
}

function dirSize(dir: string): number {
    let total = 0
    try {
        for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
            const full = path.join(dir, entry.name)
            try {
                if (entry.isDirectory()) total += dirSize(full)
                else if (entry.isFile()) total += fs.statSync(full).size
            } catch {
                /* permission denied / vanished */
            }
        }
    } catch {
        /* dir vanished mid-walk */
    }
    return total
}

function formatBytes(n: number): string {
    if (n < 1024) return `${n} B`
    if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
    if (n < 1024 * 1024 * 1024) return `${(n / 1024 / 1024).toFixed(1)} MB`
    return `${(n / 1024 / 1024 / 1024).toFixed(2)} GB`
}

function main(): void {
    console.log('[test-clean] Reclaiming test artifacts...')

    const tmp = removeTmpdirs()

    console.log(`[test-clean] Removed ${tmp.removed} tmpdir(s), ${formatBytes(tmp.bytes)} freed.`)
}

main()
