// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Vitest setupFile (loaded once per worker) that enforces test isolation.
 *
 * Goals:
 *   1. Route `PAIR_USER_DATA` to a per-worker tmpdir so unit/contract tests
 *      that inject a `PathProvider` (via `initPlatform`) never write to the
 *      dev's real userData directory.
 *   2. Block any HTTP request that escapes localhost. The contract: if a
 *      test wants real network calls, it spins up a local server (see
 *      `release-server.ts`) and connects to `127.0.0.1`. Anything else is
 *      a leak that must fail loud.
 *   3. Provide `assertIsolated()` for tests that touch the file system, so
 *      they can refuse to run unless `PAIR_USER_DATA` points at a tmpdir.
 *
 * Sandboxing of destructive subsystems is the responsibility of the test
 * runtime, not source-code branches: in-process tests use `vi.mock` at module
 * boundaries. See `../README.md`.
 *
 * This file is `import`ed for side effects via vitest.config's
 * `setupFiles` — do not export anything that's mutated at module scope.
 */

import fs from 'fs'
import os from 'os'
import path from 'path'
import axios from 'axios'
import { afterAll, expect } from 'vitest'

// ---------------------------------------------------------------------------
// Route PAIR_USER_DATA to a per-worker tmpdir. Tests build their `PathProvider`
// from this env var and register it with `initPlatform`, so it MUST be set here
// in `setupFiles` (which run before user test code). The per-worker tmpdir is
// removed unconditionally in `afterAll`.
// ---------------------------------------------------------------------------

const workerTmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'pair-test-worker-'))

if (process.env.PAIR_USER_DATA === undefined) {
    process.env.PAIR_USER_DATA = workerTmpDir
}

// ---------------------------------------------------------------------------
// Axios network block. Adds a request interceptor at module load that throws
// before the request hits the wire when the host is anything other than
// loopback. Localhost and link-local are allowed so per-test fakes work.
// ---------------------------------------------------------------------------

const ALLOWED_HOSTS: ReadonlyArray<string> = ['127.0.0.1', 'localhost', '::1', '0.0.0.0']

function isAllowedHost(input: string | undefined): boolean {
    if (!input) return false
    const lowered = input.toLowerCase()
    return ALLOWED_HOSTS.some(prefix =>
        prefix.endsWith('.') ? lowered.startsWith(prefix) : lowered === prefix
    )
}

function extractHost(url: string | undefined, baseURL: string | undefined): string | undefined {
    if (!url) return undefined
    try {
        const parsed = new URL(url, baseURL ?? 'http://127.0.0.1')
        return parsed.hostname
    } catch {
        return undefined
    }
}

axios.interceptors.request.use(config => {
    const host = extractHost(config.url, config.baseURL)
    if (!isAllowedHost(host)) {
        throw new Error(
            `[isolation] Blocked outbound axios request to "${host ?? '<unknown>'}" — ` +
                `tests must only hit loopback. Spin up a local fake server instead.`
        )
    }
    return config
})

// ---------------------------------------------------------------------------
// Per-test isolation helpers.
// ---------------------------------------------------------------------------

/**
 * Verify the test process is pointed at a per-test tmpdir, not the dev's
 * real userData. Call this from any test that constructs a service or
 * touches the file-config-store; it short-circuits with a clear error if
 * a future refactor accidentally drops the env var.
 */
export function assertIsolated(): void {
    const userData = process.env.PAIR_USER_DATA
    expect(
        userData,
        '[isolation] PAIR_USER_DATA must be set so the test does not write to the real userData dir'
    ).toBeTruthy()
    if (!userData) return
    const tmp = os.tmpdir()
    const normalized = path.resolve(userData)
    const tmpNormalized = path.resolve(tmp)
    expect(
        normalized.startsWith(tmpNormalized),
        `[isolation] PAIR_USER_DATA="${userData}" must live under the OS tmpdir ("${tmp}")`
    ).toBe(true)
}

/**
 * Track tmpdirs created by the test so a teardown can verify nothing leaked
 * to the host. Call from `tmpdir.ts` after `fs.mkdtempSync`.
 */
const trackedTmpDirs = new Set<string>()

export function trackTmpDir(dir: string): void {
    trackedTmpDirs.add(dir)
}

export function untrackTmpDir(dir: string): void {
    trackedTmpDirs.delete(dir)
}

afterAll(() => {
    // Per-worker userData tmpdir is removed unconditionally — it's the
    // sandbox shared by every test in this worker and the test process is
    // about to exit.
    try {
        fs.rmSync(workerTmpDir, { recursive: true, force: true, maxRetries: 3 })
    } catch {
        /* best effort */
    }

    if (trackedTmpDirs.size > 0) {
        const leaked = [...trackedTmpDirs]
        trackedTmpDirs.clear()
        throw new Error(
            `[isolation] ${leaked.length} tmpdir(s) survived test teardown:\n  ` +
                leaked.join('\n  ')
        )
    }
})
