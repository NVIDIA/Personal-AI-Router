// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
    MODULAR_NODE_INFO_POLL_BACKOFF_MAX_MS,
    MODULAR_NODE_INFO_POLL_INTERVAL_MS,
    MODULAR_NODE_INFO_POLL_TIMEOUT_MS,
    MODULAR_NODE_INFO_WALK_COOLDOWN_MS
} from '@/shared/constants/modular-runtime'
import type { getModularBridgeState } from '@/electron/service-bridge/modular-state'

type BridgeState = ReturnType<typeof getModularBridgeState>

const mocks = vi.hoisted(() => ({
    state: {
        getNodeInfoPollTargets: vi.fn<BridgeState['getNodeInfoPollTargets']>(),
        mergeNodeInfoResponse: vi.fn<BridgeState['mergeNodeInfoResponse']>()
    },
    logged: [] as {
        level: string
        sublevel?: string
        message: string
        data?: { nodeId?: string; reason?: string; url?: string }
    }[]
}))

vi.mock('@/electron/service-bridge/modular-state', () => ({
    getModularBridgeState: () => mocks.state
}))

vi.mock('@/shared/utils/log', () => {
    const record =
        (level: string) =>
        (payload: {
            sublevel?: string
            message: string
            data?: { nodeId?: string; reason?: string; url?: string }
        }): void => {
            mocks.logged.push({
                level,
                sublevel: payload.sublevel,
                message: payload.message,
                data: payload.data
            })
        }
    return {
        createStructuredLogger: () => ({
            info: record('info'),
            warn: record('warn'),
            error: record('error'),
            verbose: record('verbose')
        })
    }
})

import { startNodeInfoPoller, stopNodeInfoPoller } from '@/electron/service-bridge/node-info-poller'

const fetchMock = vi.fn<typeof fetch>()

/** The once-per-node outage reports, the only user-visible poll verdict. */
function outageWarnings(): string[] {
    return mocks.logged
        .filter(entry => entry.level === 'warn' && entry.message.includes('not answering'))
        .map(entry => entry.message)
}

/** The reason each outage report gave, keyed by node. */
function outageReasons(): Record<string, string> {
    const reasons: Record<string, string> = {}
    for (const entry of mocks.logged) {
        if (entry.level !== 'warn' || !entry.message.includes('not answering')) continue
        if (entry.data?.nodeId) reasons[entry.data.nodeId] = entry.data.reason ?? ''
    }
    return reasons
}

/** The endpoint each outage report named, keyed by node. */
function outageUrls(): Record<string, string> {
    const urls: Record<string, string> = {}
    for (const entry of mocks.logged) {
        if (entry.level !== 'warn' || !entry.message.includes('not answering')) continue
        if (entry.data?.nodeId) urls[entry.data.nodeId] = entry.data.url ?? ''
    }
    return urls
}

/** What `fetch` throws for a refused connection: a generic message over the real one. */
function refused(address: string): Error {
    return Object.assign(new Error('fetch failed'), {
        cause: Object.assign(new Error(`connect ECONNREFUSED ${address}`), {
            code: 'ECONNREFUSED'
        })
    })
}

function fetchCallsFor(host: string): number {
    return fetchMock.mock.calls.filter(([input]) => String(input).includes(host)).length
}

/** Answer with this node's own telemetry, as a reachable address would. */
function answersFor(nodeId: string): Response {
    return new Response(JSON.stringify({ hostUuid: nodeId }), { status: 200 })
}

/** Never answer and never refuse, the way a dropped SYN behaves. */
function blackhole(_input: RequestInfo | URL, init?: RequestInit): Promise<Response> {
    return new Promise((_resolve, reject) => {
        init?.signal?.addEventListener('abort', () => reject(new Error('aborted')))
    })
}

// The poller must walk a node's published addresses only when there is a reason
// to: it remembers where a node answered and asks that address alone, so the
// steady state costs one connect per node per tick however many addresses the node
// published. It must also survive an address that drops SYNs rather than refusing,
// keep one slow node from delaying the others, and cancel in flight on stop.
describe('node info polling', () => {
    beforeEach(() => {
        vi.useFakeTimers()
        vi.stubGlobal('fetch', fetchMock)
        fetchMock.mockReset()
        mocks.state.getNodeInfoPollTargets.mockReturnValue([])
        mocks.logged.length = 0
    })

    afterEach(() => {
        stopNodeInfoPoller()
        vi.unstubAllGlobals()
        vi.useRealTimers()
    })

    it('reaches a working address past ones that drop connections', async () => {
        mocks.state.getNodeInfoPollTargets.mockReturnValue([
            {
                id: 'uuid-multihomed',
                hosts: ['192.0.2.1', '192.0.2.2', '192.0.2.3', '10.0.0.5'],
                port: 14318
            }
        ])
        fetchMock.mockImplementation((input, init) =>
            String(input).includes('10.0.0.5')
                ? Promise.resolve(answersFor('uuid-multihomed'))
                : blackhole(input, init)
        )

        startNodeInfoPoller()
        await vi.advanceTimersByTimeAsync(0)
        // Every address is asked straight away. Asking one at a time meant the three
        // that drop connections each cost a full timeout, and the walk ran out of
        // budget before the address that works was ever tried.
        expect(fetchMock).toHaveBeenCalledTimes(4)

        await vi.advanceTimersByTimeAsync(MODULAR_NODE_INFO_POLL_TIMEOUT_MS)
        expect(mocks.state.mergeNodeInfoResponse).toHaveBeenCalledWith(
            'uuid-multihomed',
            expect.objectContaining({ hostUuid: 'uuid-multihomed' })
        )
        expect(outageWarnings()).toHaveLength(0)
    })

    it('keeps the address that answers even when a better-ranked one starts working', async () => {
        mocks.state.getNodeInfoPollTargets.mockReturnValue([
            { id: 'uuid-stable', hosts: ['192.0.2.1', '10.0.0.5'], port: 14318 }
        ])
        fetchMock.mockImplementation(input =>
            String(input).includes('10.0.0.5')
                ? Promise.resolve(answersFor('uuid-stable'))
                : Promise.reject(new Error('ECONNREFUSED'))
        )

        startNodeInfoPoller()
        await vi.advanceTimersByTimeAsync(MODULAR_NODE_INFO_POLL_INTERVAL_MS)

        // The top-ranked address comes up. Nothing is wrong with the one in use, so
        // it is not asked and the node does not change addresses: a swap would cost
        // a reconnect to learn nothing.
        fetchMock.mockImplementation(() => Promise.resolve(answersFor('uuid-stable')))
        fetchMock.mockClear()
        for (let tick = 0; tick < 3; tick += 1) {
            await vi.advanceTimersByTimeAsync(MODULAR_NODE_INFO_POLL_INTERVAL_MS)
        }

        expect(fetchMock).toHaveBeenCalledTimes(3)
        for (const [input] of fetchMock.mock.calls) {
            expect(String(input)).toContain('10.0.0.5')
        }
    })

    it('asks only the address that answered, however many the node published', async () => {
        mocks.state.getNodeInfoPollTargets.mockReturnValue([
            {
                id: 'uuid-settled',
                hosts: ['192.0.2.1', '192.0.2.2', '10.0.0.5', '192.0.2.3'],
                port: 14318
            }
        ])
        fetchMock.mockImplementation(input =>
            String(input).includes('10.0.0.5')
                ? Promise.resolve(answersFor('uuid-settled'))
                : Promise.reject(new Error('ECONNREFUSED'))
        )

        startNodeInfoPoller()
        await vi.advanceTimersByTimeAsync(0)
        // The one walk that had a reason to happen: every address asked at once,
        // and the best-ranked answer taken.
        expect(fetchMock).toHaveBeenCalledTimes(4)

        fetchMock.mockClear()
        for (let tick = 0; tick < 5; tick += 1) {
            await vi.advanceTimersByTimeAsync(MODULAR_NODE_INFO_POLL_INTERVAL_MS)
        }
        // One connect per tick, to the remembered address only.
        expect(fetchMock).toHaveBeenCalledTimes(5)
        for (const [input] of fetchMock.mock.calls) {
            expect(String(input)).toContain('10.0.0.5')
        }
    })

    it('walks again when the remembered address stops answering', async () => {
        mocks.state.getNodeInfoPollTargets.mockReturnValue([
            { id: 'uuid-moved', hosts: ['10.0.0.5', '10.0.0.6'], port: 14318 }
        ])
        fetchMock.mockImplementation(input =>
            String(input).includes('10.0.0.5')
                ? Promise.resolve(answersFor('uuid-moved'))
                : Promise.reject(new Error('ECONNREFUSED'))
        )

        startNodeInfoPoller()
        await vi.advanceTimersByTimeAsync(0)
        fetchMock.mockClear()

        // The address it settled on goes away and the other one takes over.
        fetchMock.mockImplementation(input =>
            String(input).includes('10.0.0.6')
                ? Promise.resolve(answersFor('uuid-moved'))
                : Promise.reject(new Error('ECONNREFUSED'))
        )
        mocks.state.mergeNodeInfoResponse.mockClear()
        await vi.advanceTimersByTimeAsync(MODULAR_NODE_INFO_POLL_INTERVAL_MS)

        expect(mocks.state.mergeNodeInfoResponse).toHaveBeenCalledTimes(1)
        expect(outageWarnings()).toHaveLength(0)
        fetchMock.mockClear()
        // And it settles on the new address rather than re-walking from the top.
        await vi.advanceTimersByTimeAsync(MODULAR_NODE_INFO_POLL_INTERVAL_MS)
        expect(fetchMock).toHaveBeenCalledTimes(1)
    })

    it('stops re-walking a node that answers nowhere until the cooldown lapses', async () => {
        fetchMock.mockRejectedValue(new Error('ECONNREFUSED'))
        mocks.state.getNodeInfoPollTargets.mockReturnValue([
            {
                id: 'uuid-offline',
                hosts: ['192.0.2.11', '192.0.2.12', '192.0.2.13', '192.0.2.14'],
                port: 14318
            }
        ])

        startNodeInfoPoller()
        await vi.advanceTimersByTimeAsync(0)
        expect(fetchMock).toHaveBeenCalledTimes(4)
        expect(outageWarnings()).toHaveLength(1)

        fetchMock.mockClear()
        await vi.advanceTimersByTimeAsync(MODULAR_NODE_INFO_POLL_INTERVAL_MS)
        expect(fetchMock).not.toHaveBeenCalled()

        await vi.advanceTimersByTimeAsync(MODULAR_NODE_INFO_POLL_INTERVAL_MS)
        // The first due retry is still inside the walk cooldown, so it checks only
        // the top-ranked address.
        expect(fetchMock).toHaveBeenCalledTimes(1)

        fetchMock.mockClear()
        const firstRetryDelay = MODULAR_NODE_INFO_POLL_INTERVAL_MS * 2
        await vi.advanceTimersByTimeAsync(MODULAR_NODE_INFO_WALK_COOLDOWN_MS - firstRetryDelay)
        expect(fetchMock).not.toHaveBeenCalled()

        await vi.advanceTimersByTimeAsync(MODULAR_NODE_INFO_POLL_INTERVAL_MS)
        // The next due retry is beyond the cooldown and walks the full list, so a
        // recovery on any published address remains discoverable.
        expect(fetchMock).toHaveBeenCalledTimes(4)
        expect(outageWarnings()).toHaveLength(1)
    })

    it('backs off repeated failures geometrically and caps the retry delay', async () => {
        fetchMock.mockRejectedValue(new Error('ECONNREFUSED'))
        mocks.state.getNodeInfoPollTargets.mockReturnValue([
            { id: 'uuid-backoff', hosts: ['192.0.2.31'], port: 14318 }
        ])

        startNodeInfoPoller()
        await vi.advanceTimersByTimeAsync(0)
        expect(fetchMock).toHaveBeenCalledTimes(1)
        fetchMock.mockClear()

        await vi.advanceTimersByTimeAsync(MODULAR_NODE_INFO_POLL_INTERVAL_MS)
        expect(fetchMock).not.toHaveBeenCalled()
        await vi.advanceTimersByTimeAsync(MODULAR_NODE_INFO_POLL_INTERVAL_MS)
        expect(fetchMock).toHaveBeenCalledTimes(1)
        fetchMock.mockClear()

        await vi.advanceTimersByTimeAsync(6_000)
        expect(fetchMock).not.toHaveBeenCalled()
        await vi.advanceTimersByTimeAsync(MODULAR_NODE_INFO_POLL_INTERVAL_MS)
        expect(fetchMock).toHaveBeenCalledTimes(1)
        fetchMock.mockClear()

        await vi.advanceTimersByTimeAsync(14_000)
        expect(fetchMock).not.toHaveBeenCalled()
        await vi.advanceTimersByTimeAsync(MODULAR_NODE_INFO_POLL_INTERVAL_MS)
        expect(fetchMock).toHaveBeenCalledTimes(1)
        fetchMock.mockClear()

        await vi.advanceTimersByTimeAsync(
            MODULAR_NODE_INFO_POLL_BACKOFF_MAX_MS - MODULAR_NODE_INFO_POLL_INTERVAL_MS
        )
        expect(fetchMock).not.toHaveBeenCalled()
        await vi.advanceTimersByTimeAsync(MODULAR_NODE_INFO_POLL_INTERVAL_MS)
        expect(fetchMock).toHaveBeenCalledTimes(1)
        expect(outageWarnings()).toHaveLength(1)
    })

    it('keeps healthy nodes on cadence and resets a failed node on success', async () => {
        let recovering = false
        mocks.state.getNodeInfoPollTargets.mockReturnValue([
            { id: 'uuid-recovering', hosts: ['192.0.2.41'], port: 14318 },
            { id: 'uuid-healthy', hosts: ['10.0.0.41'], port: 14318 }
        ])
        fetchMock.mockImplementation(input => {
            if (String(input).includes('10.0.0.41')) {
                return Promise.resolve(answersFor('uuid-healthy'))
            }
            return recovering
                ? Promise.resolve(answersFor('uuid-recovering'))
                : Promise.reject(new Error('ECONNREFUSED'))
        })

        startNodeInfoPoller()
        await vi.advanceTimersByTimeAsync(0)
        recovering = true
        fetchMock.mockClear()

        await vi.advanceTimersByTimeAsync(MODULAR_NODE_INFO_POLL_INTERVAL_MS)
        expect(fetchCallsFor('192.0.2.41')).toBe(0)
        expect(fetchCallsFor('10.0.0.41')).toBe(1)

        fetchMock.mockClear()
        await vi.advanceTimersByTimeAsync(MODULAR_NODE_INFO_POLL_INTERVAL_MS)
        expect(fetchCallsFor('192.0.2.41')).toBe(1)
        expect(fetchCallsFor('10.0.0.41')).toBe(1)

        fetchMock.mockClear()
        await vi.advanceTimersByTimeAsync(MODULAR_NODE_INFO_POLL_INTERVAL_MS)
        expect(fetchCallsFor('192.0.2.41')).toBe(1)
        expect(fetchCallsFor('10.0.0.41')).toBe(1)
    })

    it('tries a changed endpoint immediately while the old target is backed off', async () => {
        mocks.state.getNodeInfoPollTargets.mockReturnValue([
            { id: 'uuid-retargeted', hosts: ['192.0.2.51'], port: 14318 }
        ])
        fetchMock.mockRejectedValueOnce(new Error('ECONNREFUSED'))

        startNodeInfoPoller()
        await vi.advanceTimersByTimeAsync(0)
        fetchMock.mockClear()

        mocks.state.getNodeInfoPollTargets.mockReturnValue([
            { id: 'uuid-retargeted', hosts: ['10.0.0.51'], port: 14319 }
        ])
        fetchMock.mockResolvedValue(answersFor('uuid-retargeted'))
        await vi.advanceTimersByTimeAsync(MODULAR_NODE_INFO_POLL_INTERVAL_MS)

        expect(fetchMock).toHaveBeenCalledTimes(1)
        expect(String(fetchMock.mock.calls[0][0])).toContain('10.0.0.51:14319')
    })

    it('walks a newly advertised address during a failed-walk cooldown', async () => {
        mocks.state.getNodeInfoPollTargets.mockReturnValue([
            {
                id: 'uuid-new-secondary',
                hosts: ['192.0.2.61', '192.0.2.62'],
                port: 14318
            }
        ])
        fetchMock.mockRejectedValue(new Error('ECONNREFUSED'))

        startNodeInfoPoller()
        await vi.advanceTimersByTimeAsync(0)
        expect(fetchMock).toHaveBeenCalledTimes(2)
        fetchMock.mockClear()

        mocks.state.getNodeInfoPollTargets.mockReturnValue([
            {
                id: 'uuid-new-secondary',
                hosts: ['192.0.2.61', '192.0.2.62', '10.0.0.61'],
                port: 14318
            }
        ])
        fetchMock.mockImplementation(input =>
            String(input).includes('10.0.0.61')
                ? Promise.resolve(answersFor('uuid-new-secondary'))
                : Promise.reject(new Error('ECONNREFUSED'))
        )
        await vi.advanceTimersByTimeAsync(MODULAR_NODE_INFO_POLL_INTERVAL_MS)

        expect(fetchMock).toHaveBeenCalledTimes(3)
        expect(mocks.state.mergeNodeInfoResponse).toHaveBeenCalledWith(
            'uuid-new-secondary',
            expect.objectContaining({ hostUuid: 'uuid-new-secondary' })
        )
    })

    it('keeps an answering address when discovery adds another host', async () => {
        mocks.state.getNodeInfoPollTargets.mockReturnValue([
            {
                id: 'uuid-remembered-growth',
                hosts: ['192.0.2.71', '10.0.0.71'],
                port: 14318
            }
        ])
        fetchMock.mockImplementation(input =>
            String(input).includes('10.0.0.71')
                ? Promise.resolve(answersFor('uuid-remembered-growth'))
                : Promise.reject(new Error('ECONNREFUSED'))
        )

        startNodeInfoPoller()
        await vi.advanceTimersByTimeAsync(0)
        fetchMock.mockClear()

        mocks.state.getNodeInfoPollTargets.mockReturnValue([
            {
                id: 'uuid-remembered-growth',
                hosts: ['192.0.2.71', '192.0.2.72', '10.0.0.71'],
                port: 14318
            }
        ])
        await vi.advanceTimersByTimeAsync(MODULAR_NODE_INFO_POLL_INTERVAL_MS)

        expect(fetchMock).toHaveBeenCalledTimes(1)
        expect(String(fetchMock.mock.calls[0][0])).toContain('10.0.0.71')
    })

    it('forgets retry state when a node disappears from the poll targets', async () => {
        const target = { id: 'uuid-rediscovered', hosts: ['192.0.2.52'], port: 14318 }
        mocks.state.getNodeInfoPollTargets.mockReturnValue([target])
        fetchMock.mockRejectedValue(new Error('ECONNREFUSED'))

        startNodeInfoPoller()
        await vi.advanceTimersByTimeAsync(0)
        await vi.advanceTimersByTimeAsync(MODULAR_NODE_INFO_POLL_INTERVAL_MS * 2)
        fetchMock.mockClear()

        mocks.state.getNodeInfoPollTargets.mockReturnValue([])
        await vi.advanceTimersByTimeAsync(MODULAR_NODE_INFO_POLL_INTERVAL_MS)
        mocks.state.getNodeInfoPollTargets.mockReturnValue([target])
        await vi.advanceTimersByTimeAsync(MODULAR_NODE_INFO_POLL_INTERVAL_MS)

        expect(fetchMock).toHaveBeenCalledTimes(1)
    })

    it('drains a non-OK response body before backing off', async () => {
        const response = new Response('temporarily unavailable', { status: 503 })
        fetchMock.mockResolvedValue(response)
        mocks.state.getNodeInfoPollTargets.mockReturnValue([
            { id: 'uuid-http-error', hosts: ['10.0.0.61'], port: 14318 }
        ])

        startNodeInfoPoller()
        await vi.advanceTimersByTimeAsync(0)

        expect(response.bodyUsed).toBe(true)
        expect(outageWarnings()).toHaveLength(1)
    })

    it('clears a failed node backoff when the poller restarts', async () => {
        fetchMock.mockRejectedValue(new Error('ECONNREFUSED'))
        mocks.state.getNodeInfoPollTargets.mockReturnValue([
            { id: 'uuid-restarted', hosts: ['192.0.2.71'], port: 14318 }
        ])

        startNodeInfoPoller()
        await vi.advanceTimersByTimeAsync(0)
        fetchMock.mockClear()

        stopNodeInfoPoller()
        startNodeInfoPoller()
        await vi.advanceTimersByTimeAsync(0)

        expect(fetchMock).toHaveBeenCalledTimes(1)
    })

    it('polls every other node on time while one node is stuck', async () => {
        mocks.state.getNodeInfoPollTargets.mockReturnValue([
            { id: 'uuid-stuck', hosts: ['192.0.2.1'], port: 14318 },
            { id: 'uuid-healthy', hosts: ['10.0.0.7'], port: 14318 }
        ])
        fetchMock.mockImplementation((input, init) =>
            String(input).includes('10.0.0.7')
                ? Promise.resolve(answersFor('uuid-healthy'))
                : blackhole(input, init)
        )

        startNodeInfoPoller()
        await vi.advanceTimersByTimeAsync(0)
        mocks.state.mergeNodeInfoResponse.mockClear()

        // The stuck node is still waiting out its attempt across these ticks.
        for (let tick = 0; tick < 3; tick += 1) {
            await vi.advanceTimersByTimeAsync(MODULAR_NODE_INFO_POLL_INTERVAL_MS)
        }
        expect(mocks.state.mergeNodeInfoResponse).toHaveBeenCalledTimes(3)
    })

    it('says why a node stopped answering, a timeout apart from a refusal', async () => {
        mocks.state.getNodeInfoPollTargets.mockReturnValue([
            { id: 'uuid-silent', hosts: ['192.0.2.31'], port: 14318 },
            { id: 'uuid-refused', hosts: ['192.0.2.32'], port: 14318 }
        ])
        fetchMock.mockImplementation((input, init) =>
            String(input).includes('192.0.2.31')
                ? blackhole(input, init)
                : Promise.reject(refused('192.0.2.32:14318'))
        )

        startNodeInfoPoller()
        await vi.advanceTimersByTimeAsync(MODULAR_NODE_INFO_POLL_TIMEOUT_MS)

        const reasons = outageReasons()
        // An address that swallows the request is not the same fault as one the
        // local stack answered for, and both used to report the same bare warning.
        expect(reasons['uuid-silent']).toBe(
            `no response within ${MODULAR_NODE_INFO_POLL_TIMEOUT_MS}ms`
        )
        // The syscall `fetch` buries under "fetch failed" is the whole diagnosis.
        expect(reasons['uuid-refused']).toContain('ECONNREFUSED')
    })

    it('names the address that failed when a multi-homed node answers nowhere', async () => {
        mocks.state.getNodeInfoPollTargets.mockReturnValue([
            { id: 'uuid-both-down', hosts: ['192.0.2.41', '192.0.2.42'], port: 14318 }
        ])
        fetchMock.mockImplementation(input =>
            Promise.reject(
                refused(
                    `${String(input).includes('192.0.2.41') ? '192.0.2.41' : '192.0.2.42'}:14318`
                )
            )
        )

        startNodeInfoPoller()
        await vi.advanceTimersByTimeAsync(0)

        // Both addresses were asked, so both are accounted for: a report naming
        // only one cannot be told apart from a node with one address.
        const reason = outageReasons()['uuid-both-down']
        expect(reason).toContain('192.0.2.41: ')
        expect(reason).toContain('192.0.2.42: ')
    })

    it('reports again when the fault changes while the node stays unreachable', async () => {
        mocks.state.getNodeInfoPollTargets.mockReturnValue([
            { id: 'uuid-changing', hosts: ['192.0.2.51'], port: 14318 }
        ])
        // First the address swallows the request, so the poll can only time out.
        fetchMock.mockImplementation((input, init) => blackhole(input, init))

        startNodeInfoPoller()
        await vi.advanceTimersByTimeAsync(MODULAR_NODE_INFO_POLL_TIMEOUT_MS)
        expect(outageReasons()['uuid-changing']).toBe(
            `no response within ${MODULAR_NODE_INFO_POLL_TIMEOUT_MS}ms`
        )

        // Same outage, different fault: the host went from dropping the request to
        // refusing it. Reporting only the first reason would leave the log saying
        // the packets are being swallowed long after they stopped being.
        fetchMock.mockImplementation(() => Promise.reject(refused('192.0.2.51:14318')))
        // The first failure waits four seconds, then retries on the next polling
        // cadence tick.
        await vi.advanceTimersByTimeAsync(MODULAR_NODE_INFO_POLL_INTERVAL_MS * 3)

        expect(outageWarnings()).toHaveLength(2)
        expect(outageReasons()['uuid-changing']).toContain('ECONNREFUSED')

        // A fault that goes back to one already reported is not news again: an
        // endpoint flapping between two states must not produce a line per retry.
        fetchMock.mockImplementation((input, init) => blackhole(input, init))
        // The second failure waits eight seconds; this reaches that retry and lets
        // its request time out.
        await vi.advanceTimersByTimeAsync(MODULAR_NODE_INFO_POLL_INTERVAL_MS * 4)
        expect(outageWarnings()).toHaveLength(2)
    })

    it('reports the new endpoint when a node is retargeted mid-outage', async () => {
        mocks.state.getNodeInfoPollTargets.mockReturnValue([
            { id: 'uuid-moving', hosts: ['192.0.2.81'], port: 14318 }
        ])
        // Dropped rather than refused, so the reason carries no address of its own.
        // A refusal names its address inside the reason, which distinguishes the
        // report on its own; a timeout is the case that needs the endpoint checked.
        fetchMock.mockImplementation((input, init) => blackhole(input, init))

        startNodeInfoPoller()
        await vi.advanceTimersByTimeAsync(MODULAR_NODE_INFO_POLL_TIMEOUT_MS)
        expect(outageUrls()['uuid-moving']).toContain('192.0.2.81:14318')

        // The node is retargeted and still cannot be reached. The retry is right
        // (a changed endpoint owes nothing to the old one's backoff), but reporting
        // nothing would leave that first line — naming an address no longer polled
        // — as the newest thing the log says about this node.
        mocks.state.getNodeInfoPollTargets.mockReturnValue([
            { id: 'uuid-moving', hosts: ['10.0.0.81'], port: 14319 }
        ])
        await vi.advanceTimersByTimeAsync(
            MODULAR_NODE_INFO_POLL_INTERVAL_MS + MODULAR_NODE_INFO_POLL_TIMEOUT_MS
        )

        expect(outageWarnings()).toHaveLength(2)
        expect(outageUrls()['uuid-moving']).toContain('10.0.0.81:14319')
        // Unchanged, which is the point: the second line is owed to the endpoint
        // moving, not to the fault differing.
        expect(outageReasons()['uuid-moving']).toBe(
            `no response within ${MODULAR_NODE_INFO_POLL_TIMEOUT_MS}ms`
        )

        // And the new endpoint is now the one being suppressed against, so the
        // capped retries against it stay as quiet as the old one's did.
        await vi.advanceTimersByTimeAsync(
            MODULAR_NODE_INFO_POLL_INTERVAL_MS * 2 + MODULAR_NODE_INFO_POLL_TIMEOUT_MS
        )
        expect(outageWarnings()).toHaveLength(2)
    })

    it('abandons the poll in flight when stopped, and starts clean afterwards', async () => {
        const pending: { resolve: (response: Response) => void }[] = []
        fetchMock.mockImplementation(
            () =>
                new Promise<Response>(resolve => {
                    pending.push({ resolve })
                })
        )
        mocks.state.getNodeInfoPollTargets.mockReturnValue([
            { id: 'uuid-stopped', hosts: ['192.0.2.21'], port: 14318 }
        ])

        startNodeInfoPoller()
        await vi.advanceTimersByTimeAsync(0)
        expect(pending).toHaveLength(1)

        stopNodeInfoPoller()
        // The answer the abandoned poll was waiting on arrives anyway.
        pending[0].resolve(answersFor('uuid-stopped'))
        await vi.advanceTimersByTimeAsync(MODULAR_NODE_INFO_POLL_TIMEOUT_MS)

        expect(mocks.state.mergeNodeInfoResponse).not.toHaveBeenCalled()
        expect(mocks.logged).toEqual([])

        // The stop left no in-flight guard behind, so the restarted poller's
        // first tick actually polls instead of being skipped.
        startNodeInfoPoller()
        await vi.advanceTimersByTimeAsync(0)
        expect(pending).toHaveLength(2)
    })
})
