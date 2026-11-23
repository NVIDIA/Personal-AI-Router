// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { readFileSync } from 'fs'
import { join } from 'path'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { createStructuredLogger, initFileLogger } from '@/shared/utils/log'
import { assertIsolated } from '../fixtures/isolation'
import { createTmpUserData } from '../fixtures/tmpdir'

/**
 * File append is already best-effort. Console write was not, so a closed
 * stdout pipe became an uncaughtException in the Electron main process
 * ("A JavaScript error occurred in the main process" / write EPIPE).
 */
describe('structured logger stdout', () => {
    const tmp = createTmpUserData()

    afterEach(() => {
        vi.restoreAllMocks()
    })

    function paths() {
        return {
            getUserData: () => tmp.dir,
            getTemp: () => tmp.dir,
            getResourcesPath: () => process.cwd(),
            getAppName: () => 'Personal AI Router'
        }
    }

    it('does not throw when stdout.write fails with EPIPE', () => {
        assertIsolated()
        initFileLogger(paths())
        vi.spyOn(process.stdout, 'write').mockImplementation(() => {
            throw new Error('write EPIPE')
        })
        const log = createStructuredLogger('app')
        expect(() => log.verbose({ sublevel: 'lifecycle', message: 'ping' })).not.toThrow()
        const onDisk = readFileSync(join(tmp.dir, 'logs', 'nvpair.jsonl'), 'utf8')
        expect(onDisk).toContain('"message":"ping"')
    })

    it('does not turn a later stdout error event into an uncaughtException', () => {
        assertIsolated()
        initFileLogger(paths())
        expect(() => process.stdout.emit('error', new Error('write EPIPE'))).not.toThrow()
    })
})
