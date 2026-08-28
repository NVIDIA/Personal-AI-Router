// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { getModularBridgeState } from './modular-state'
import type { JsonValue } from './json-rpc-subprocess'
import getErrorString from '@/shared/utils/get-error-string'
import { createStructuredLogger } from '@/shared/utils/log'
import {
    MODULAR_NODE_INFO_PATH,
    MODULAR_NODE_INFO_POLL_BACKOFF_MAX_MS,
    MODULAR_NODE_INFO_POLL_INTERVAL_MS,
    MODULAR_NODE_INFO_POLL_TIMEOUT_MS,
    MODULAR_NODE_INFO_WALK_COOLDOWN_MS
} from '@/shared/constants/modular-runtime'

const log = createStructuredLogger('service-bridge')
const NODE_INFO_ERROR_BODY_DRAIN_LIMIT_BYTES = 64 * 1024

let pollTimer: ReturnType<typeof setInterval> | null = null

/**
 * Aborted by {@link stopNodeInfoPoller}, which is how a poll already mid-fetch is
 * disowned: its result is ignored rather than merged, and it reports nothing. A
 * fresh controller per run means a restart's polls are never cancelled by the
 * previous run's stop.
 */
let pollRun: AbortController | null = null

/**
 * Nodes whose poll from an earlier tick has not finished.
 *
 * Gating per node rather than per cycle is what stops one node from delaying every
 * other node's telemetry: a node still walking its addresses skips this tick,
 * while every other node is polled on time.
 */
const inFlight = new Set<string>()

/**
 * Where each node was last reached — the reason the steady state costs one
 * connect per node per tick rather than a walk.
 *
 * A non-null `host` answered for this node and is asked alone until it stops:
 * re-confirming a working address every two seconds would spend connections to
 * learn nothing. `null` means the last full walk found nothing, and `walkedAt` is
 * when it ran, so a node that answers nowhere is not re-walked on every tick.
 */
type PollChoice =
    | {
          host: string
          walkedAt: 0
      }
    | {
          host: null
          walkedAt: number
          targetKey: string
      }

const pollChoices = new Map<string, PollChoice>()

/** One attempt against one address: what it reported, or why it did not. */
interface Probe {
    parsed: JsonValue | null
    /** Empty when {@link parsed} is set, or when the poller was stopped. */
    reason: string
}

/** One address this tick asked and the reason it gave nothing back. */
interface FailedAttempt {
    host: string
    reason: string
}

/**
 * Retry timing for nodes whose last poll failed. The target key makes an address
 * or port change immediately eligible instead of carrying an old endpoint's
 * outage delay onto a newly advertised one.
 */
interface PollBackoff {
    consecutiveFailures: number
    retryAt: number
    targetKey: string
}

const pollBackoffs = new Map<string, PollBackoff>()

/**
 * What has already been said about a node's current outage: the endpoint the
 * report describes, and every distinct reason given for it.
 *
 * Scoped to the endpoint because suppressing the repeats turns this warning from
 * a record of one attempt into a description of a state, and a description has to
 * be reissued when what it describes moves. A retry at a newly advertised address
 * failing the same way would otherwise say nothing, leaving the last line naming
 * an address the node no longer publishes and the poller no longer asks.
 */
interface ReportedOutage {
    targetKey: string
    reasons: Set<string>
}

/**
 * Nodes whose last poll failed. This is kept separately from retry timing so an
 * endpoint change can retry immediately without turning one outage into repeated
 * warnings.
 *
 * Reporting a reason the outage has not shown before is one line when it starts,
 * one each time the fault changes underneath it, one when the endpoint it names
 * moves, and one when it ends, however many capped retry attempts fall between
 * those transitions.
 */
const failingNodes = new Map<string, ReportedOutage>()

export function startNodeInfoPoller(): void {
    if (pollTimer) return

    pollRun = new AbortController()
    pollNodeInfoOnce()
    pollTimer = setInterval(pollNodeInfoOnce, MODULAR_NODE_INFO_POLL_INTERVAL_MS)
}

export function stopNodeInfoPoller(): void {
    if (pollTimer) {
        clearInterval(pollTimer)
        pollTimer = null
    }
    // Runs even with no timer left: a poll can still be mid-fetch, and its outcome
    // must not land after the poller was told to stop.
    pollRun?.abort()
    pollRun = null
    inFlight.clear()
    pollChoices.clear()
    pollBackoffs.clear()
    failingNodes.clear()
}

function pollNodeInfoOnce(): void {
    const run = pollRun
    if (!run) return

    const targets = getModularBridgeState().getNodeInfoPollTargets()
    // A node that is no longer polled has no outage to still be in, and nothing
    // left to remember about where it answered.
    const polled = new Set(targets.map(target => target.id))
    for (const nodeId of failingNodes.keys()) {
        if (!polled.has(nodeId)) failingNodes.delete(nodeId)
    }
    for (const nodeId of pollChoices.keys()) {
        if (!polled.has(nodeId)) pollChoices.delete(nodeId)
    }
    for (const nodeId of pollBackoffs.keys()) {
        if (!polled.has(nodeId)) pollBackoffs.delete(nodeId)
    }

    for (const target of targets) {
        if (inFlight.has(target.id)) continue
        const targetKey = pollTargetKey(target.hosts, target.port)
        if (!pollDue(target.id, targetKey, Date.now())) continue
        inFlight.add(target.id)
        void pollTarget(target.id, target.hosts, target.port, targetKey, run.signal)
            .catch((err: unknown) => {
                if (run.signal.aborted) return
                log.warn({
                    sublevel: 'node-info-poller',
                    message: `Error polling node info: ${getErrorString(err)}`
                })
            })
            .finally(() => {
                inFlight.delete(target.id)
            })
    }
}

async function pollTarget(
    nodeId: string,
    hosts: string[],
    port: number,
    targetKey: string,
    run: AbortSignal
): Promise<void> {
    // Every address asked this tick, with the reason it gave nothing back. The
    // outage report reads it, so a node that has gone quiet says whether nothing
    // came back or the local stack refused the connection outright — the
    // difference between a slow peer and a host that has stopped letting the poll
    // out, and the one fact the report used to omit.
    const failed: FailedAttempt[] = []

    // The remembered address is asked by itself, before anything else is
    // considered. Racing it against its siblings would let one of them displace an
    // address with nothing wrong with it, and every swap costs a reconnect.
    const remembered = rememberedHost(nodeId, hosts)
    if (remembered) {
        const probe = await probeHost(nodeId, remembered, port, run)
        if (run.aborted) return
        if (probe.parsed) {
            accept(nodeId, remembered, port, probe.parsed)
            return
        }
        failed.push({ host: remembered, reason: probe.reason })
        // It stopped answering. That is the one legitimate reason to look at the
        // rest of the node's list again.
        pollChoices.delete(nodeId)
        log.verbose({
            sublevel: 'node-info',
            message: 'Node info address stopped answering; trying the rest of its list',
            data: { nodeId, url: nodeInfoUrl(remembered, port), reason: probe.reason }
        })
    }

    const rest = remembered ? hosts.filter(host => host !== remembered) : hosts
    // Cooling down: check only the address the node ranks first, where a recovery
    // will appear, instead of paying the whole walk again. The cooldown restarts
    // only after a walk that really tried everything, so these cheap checks can
    // never postpone the next one indefinitely.
    const cooling = withinWalkCooldown(pollChoices.get(nodeId), targetKey)
    const order = cooling ? rest.slice(0, 1) : rest

    // Asked together, because an address that drops connections rather than
    // refusing them costs a full timeout, and paying that per address let one such
    // address use up the walk before a working one was ever tried. The answers are
    // then read in the node's published order, so the address used is the same one
    // a sequential walk would have chosen — not whichever replied first.
    const probes = order.map(host => probeHost(nodeId, host, port, run))
    for (const [index, probe] of probes.entries()) {
        const result = await probe
        if (run.aborted) return
        if (result.parsed) {
            accept(nodeId, order[index], port, result.parsed)
            return
        }
        failed.push({ host: order[index], reason: result.reason })
    }
    if (run.aborted) return
    if (!cooling) pollChoices.set(nodeId, { host: null, walkedAt: Date.now(), targetKey })
    noteNotAnswering(nodeId, failed, port, targetKey)
}

/**
 * The address remembered for a node, when the node still publishes it. A re-rank
 * that dropped the address retires it: the node no longer claims to answer there.
 */
function rememberedHost(nodeId: string, hosts: string[]): string | null {
    const choice = pollChoices.get(nodeId)
    if (!choice?.host) return null
    if (hosts.includes(choice.host)) return choice.host
    pollChoices.delete(nodeId)
    return null
}

function withinWalkCooldown(choice: PollChoice | undefined, targetKey: string): boolean {
    if (!choice || choice.host !== null) return false
    if (choice.targetKey !== targetKey) return false
    return Date.now() - choice.walkedAt < MODULAR_NODE_INFO_WALK_COOLDOWN_MS
}

/**
 * One attempt against one address, returning what it reported if it answered for
 * this node. Probing is kept separate from {@link accept} so several addresses can
 * be asked at once while only the best-ranked answer is the one used.
 */
async function probeHost(
    nodeId: string,
    host: string,
    port: number,
    run: AbortSignal
): Promise<Probe> {
    const probe = await fetchNodeInfo(host, port, run)
    // An abandoned run has no verdict to report: its answer belongs to a poller
    // that has already stopped, so recording an outcome or merging telemetry would
    // repopulate state the stop cleared.
    if (run.aborted) return { parsed: null, reason: '' }
    if (!probe.parsed) return probe

    // A reused/shared address can answer for another machine. That response is not
    // success for this node, but neither should it stop failover to the remaining
    // addresses the intended node published.
    const reportedUuid = nodeInfoHostUuid(probe.parsed)
    if (reportedUuid && reportedUuid !== nodeId) {
        log.verbose({
            sublevel: 'node-info',
            message: 'Node info candidate answered for a different host; trying next',
            data: { nodeId, reportedUuid, url: nodeInfoUrl(host, port) }
        })
        return { parsed: null, reason: `answered for ${reportedUuid}` }
    }
    return probe
}

/** Remember the address that answered, and merge the telemetry it reported. */
function accept(
    nodeId: string,
    host: string,
    port: number,
    parsed: JsonValue
): void {
    pollChoices.set(nodeId, { host, walkedAt: 0 })
    noteAnswering(nodeId, nodeInfoUrl(host, port))
    getModularBridgeState().mergeNodeInfoResponse(nodeId, parsed)
}

function nodeInfoHostUuid(value: JsonValue): string {
    if (!value || typeof value !== 'object' || Array.isArray(value)) return ''
    const hostUuid = value.hostUuid
    return typeof hostUuid === 'string' ? hostUuid : ''
}

/** Report a node's endpoint coming back, once. */
function noteAnswering(nodeId: string, url: string): void {
    pollBackoffs.delete(nodeId)
    if (!failingNodes.delete(nodeId)) return
    log.info({
        sublevel: 'node-info',
        message: 'Node info is answering again',
        data: { nodeId, url }
    })
}

/**
 * Report a node's endpoint going quiet, with what every address it published
 * said. Once per distinct reason rather than once per attempt: a node that
 * cannot be reached is retried at a capped backoff for as long as it stays
 * discovered, so a line per attempt buries the log — but a line per outage with
 * no reason on it cannot be acted on either, and a single line dates the
 * diagnosis without saying so, because the fault that ends an outage is not
 * always the one that began it.
 */
function noteNotAnswering(
    nodeId: string,
    failed: FailedAttempt[],
    port: number,
    targetKey: string
): void {
    const previous = pollBackoffs.get(nodeId)
    const consecutiveFailures =
        previous?.targetKey === targetKey ? previous.consecutiveFailures + 1 : 1
    pollBackoffs.set(nodeId, {
        consecutiveFailures,
        retryAt: Date.now() + pollBackoffDelay(consecutiveFailures),
        targetKey
    })

    const previousReport = failingNodes.get(nodeId)
    // Only what was reported about the endpoint being asked now. A reason held
    // over from the address the node published before would suppress the first
    // report of the address it publishes instead, and a refusal names its address
    // in the reason while a timeout does not — so without this, whether a moved
    // endpoint gets reported at all depends on which fault it moved under.
    const reported = previousReport?.targetKey === targetKey ? previousReport.reasons : undefined
    const reasons = new Set(failed.map(attempt => attempt.reason))
    // Compared as reasons, not as the line below: a full walk asks every address
    // while a cooldown tick asks one, so the line differs between ticks of an
    // outage that has not changed at all. Accumulated rather than replaced, so a
    // fault alternating between two known states reports neither again.
    if (reported && Array.from(reasons).every(reason => reported.has(reason))) return
    if (reported) {
        for (const reason of reasons) reported.add(reason)
    } else {
        failingNodes.set(nodeId, { targetKey, reasons })
    }

    log.warn({
        sublevel: 'node-info',
        message: reported
            ? 'Node info is still not answering, and now for a reason this outage has not reported'
            : 'Node info is not answering; this node keeps the metrics last read from it',
        data: {
            nodeId,
            url: failed.map(attempt => nodeInfoUrl(attempt.host, port)).join(', '),
            // Bare when one address was asked, host-prefixed when several, so a
            // multi-homed node says which of its addresses failed and how.
            reason:
                failed.length === 1
                    ? failed[0].reason
                    : failed.map(attempt => `${attempt.host}: ${attempt.reason}`).join('; ')
        }
    })
}

function pollDue(nodeId: string, targetKey: string, now: number): boolean {
    const backoff = pollBackoffs.get(nodeId)
    if (!backoff) return true
    if (backoff.targetKey !== targetKey) {
        pollBackoffs.delete(nodeId)
        return true
    }
    return now >= backoff.retryAt
}

function pollBackoffDelay(consecutiveFailures: number): number {
    let delay = MODULAR_NODE_INFO_POLL_INTERVAL_MS
    for (let failure = 0; failure < consecutiveFailures; failure += 1) {
        if (delay >= MODULAR_NODE_INFO_POLL_BACKOFF_MAX_MS) {
            return MODULAR_NODE_INFO_POLL_BACKOFF_MAX_MS
        }
        delay = Math.min(delay * 2, MODULAR_NODE_INFO_POLL_BACKOFF_MAX_MS)
    }
    return delay
}

function pollTargetKey(hosts: string[], port: number): string {
    return `${port}|${hosts.join('|')}`
}

async function fetchNodeInfo(host: string, port: number, run: AbortSignal): Promise<Probe> {
    const url = nodeInfoUrl(host, port)
    // This attempt's deadline, composed with the run's signal so a stop cancels the
    // request in flight rather than racing it to the end.
    const attempt = new AbortController()
    const attemptTimer = setTimeout(() => attempt.abort(), MODULAR_NODE_INFO_POLL_TIMEOUT_MS)
    const signal = AbortSignal.any([run, attempt.signal])

    try {
        const response = await fetch(url, { signal })
        if (!response.ok) {
            await drainResponseBody(response)
            return { parsed: null, reason: `HTTP ${response.status}` }
        }

        return { parsed: await response.json(), reason: '' }
    } catch (err) {
        // The poller stopping is not a verdict on this endpoint: the caller
        // discards the attempt, so no reason is ever read from it.
        if (run.aborted) return { parsed: null, reason: '' }
        return {
            parsed: null,
            // A deadline that expired and a connection the local stack answered
            // for are the same outage to a reader and different faults to a fixer,
            // and `fetch` gives both the same message. Naming the timeout here is
            // what separates them.
            reason: attempt.signal.aborted
                ? `no response within ${MODULAR_NODE_INFO_POLL_TIMEOUT_MS}ms`
                : getErrorString(err)
        }
    } finally {
        clearTimeout(attemptTimer)
    }
}

/**
 * Consume ordinary short error bodies so Undici can reuse their connection.
 * Oversized or streaming bodies are cancelled after the bound; forfeiting that
 * connection is preferable to spending unbounded bandwidth on a failing peer.
 */
async function drainResponseBody(response: Response): Promise<void> {
    const body = response.body
    if (!body) return

    const reader = body.getReader()
    let drained = 0
    try {
        while (drained <= NODE_INFO_ERROR_BODY_DRAIN_LIMIT_BYTES) {
            const chunk = await reader.read()
            if (chunk.done) return
            drained += chunk.value.byteLength
        }
        await reader.cancel()
    } catch {
        // This is already a failed poll; an unreadable body only means its
        // connection cannot be reused.
    } finally {
        reader.releaseLock()
    }
}

function nodeInfoUrl(host: string, port: number): string {
    const normalizedHost = host.includes(':') && !host.startsWith('[') ? `[${host}]` : host
    return `http://${normalizedHost}:${port}${MODULAR_NODE_INFO_PATH}`
}
