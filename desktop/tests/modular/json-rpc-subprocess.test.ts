// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import { JsonRpcSubprocess } from '@/electron/service-bridge/json-rpc-subprocess'

const FAKE_RPC = `
const readline = require('readline').createInterface({ input: process.stdin })
readline.on('line', line => {
  const request = JSON.parse(line)
  const response = request.method === 'reject'
    ? { jsonrpc: '2.0', id: request.id, error: { code: -32000, message: 'rejected' } }
    : { jsonrpc: '2.0', id: request.id, result: null }
  setTimeout(() => process.stdout.write(JSON.stringify(response) + '\\n'), 10)
})
`

// Exits as soon as it is asked to shut down, so `exit` has already fired by the
// time stop() starts waiting for it.
const FAKE_EARLY_EXIT = `
const readline = require('readline').createInterface({ input: process.stdin })
readline.on('line', () => process.exit(0))
`

describe('JsonRpcSubprocess.sendWithResponse', () => {
    it('observes delayed responses without imposing a short timeout', async () => {
        const rpc = new JsonRpcSubprocess('fake', process.execPath)
        rpc.start(['-e', FAKE_RPC])
        try {
            await expect(rpc.sendWithResponse('reject')).rejects.toThrow('-32000: rejected')
            await expect(rpc.sendWithResponse('accept')).resolves.toBeUndefined()
        } finally {
            await rpc.stop()
        }
    })
})

describe('JsonRpcSubprocess.stop', () => {
    // A child that dies during the shutdown call leaves no 'exit' left to observe,
    // so a listener attached afterwards never fires: the wait then burns the whole
    // grace and reports the delay as a blocked main process, blaming the wrong
    // side for a child that simply left early.
    it('returns promptly when the child exits while the shutdown call is in flight', async () => {
        const rpc = new JsonRpcSubprocess('fake', process.execPath)
        rpc.start(['-e', FAKE_EARLY_EXIT])

        const startedAt = Date.now()
        await rpc.stop(15_000)

        expect(Date.now() - startedAt).toBeLessThan(4_000)
    })

    it('returns promptly when the child is already gone', async () => {
        const rpc = new JsonRpcSubprocess('fake', process.execPath)
        rpc.start(['-e', FAKE_EARLY_EXIT])

        const exited = new Promise<void>(resolve => rpc.once('exit', () => resolve()))
        await rpc.notify('anything')
        await exited

        const startedAt = Date.now()
        await rpc.stop(15_000)

        expect(Date.now() - startedAt).toBeLessThan(2_000)
        expect(rpc.running).toBe(false)
    })
})
