// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Tmpdir helper for tests.
 *
 * `createTmpUserData()` returns a fresh `<tmpdir>/pair-test-<rand>/userData`
 * directory and an `[afterEach]`-bound disposer. The directory is registered
 * with `tests/fixtures/isolation.ts` so a teardown leak fails the suite.
 *
 * Use this whenever a test injects a `PathProvider` (via `initPlatform`) or
 * anything that reads `getPaths().getUserData()`.
 */

import fs from 'fs'
import os from 'os'
import path from 'path'
import { afterEach } from 'vitest'
import { trackTmpDir, untrackTmpDir } from './isolation'

export interface TmpUserData {
    /** Absolute path to the tmp userData root. */
    dir: string
    /**
     * Set `process.env.PAIR_USER_DATA` to `dir` and return a disposer that
     * restores the previous value. Use this when a test needs the env var to
     * point at this specific tmpdir before it builds a `PathProvider`.
     */
    applyEnv(): () => void
}

/**
 * Creates a fresh tmpdir, registers an `afterEach` that removes it, and
 * returns its absolute path.
 *
 * The tmpdir is *not* automatically wired to `PAIR_USER_DATA`. A test that
 * needs its `PathProvider` to resolve `getUserData()` to this dir should call
 * `applyEnv()` (or otherwise pass `dir`) before it calls `initPlatform`.
 */
export function createTmpUserData(): TmpUserData {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'pair-test-'))
    trackTmpDir(dir)

    afterEach(() => {
        try {
            fs.rmSync(dir, { recursive: true, force: true, maxRetries: 3 })
        } finally {
            untrackTmpDir(dir)
        }
    })

    return {
        dir,
        applyEnv(): () => void {
            const prev = process.env.PAIR_USER_DATA
            process.env.PAIR_USER_DATA = dir
            return () => {
                if (prev === undefined) delete process.env.PAIR_USER_DATA
                else process.env.PAIR_USER_DATA = prev
            }
        }
    }
}
