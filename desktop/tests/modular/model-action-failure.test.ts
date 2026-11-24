// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * A refused model eject, a refused model load, and a refused remote command each
 * have to release the control they were clicked from.
 *
 * `pending-actions.store` attributes a reported error to a row only when that
 * error carries a `nodeId` *and* a real `engineType`; it skips anything else. A
 * model load or eject has no other superseding push either: an engine that refuses
 * still serves the model afterwards, so the model list never changes, and eject
 * registers no lifecycle entry. An unattributed report therefore leaves the row
 * disabled for the whole safety-net window, under a message that already says
 * nothing happened.
 *
 * These tests drive the real emit path instead of handing the store the payload it
 * is supposed to receive. A real `JsonRpcSubprocess` stands in for the broker, the
 * real supervisor parses the error snapshot it answers with, and the real
 * `emitBridgePush` puts that snapshot on the push bus the preload bridge carries to
 * the renderer. That distinction matters: a hand-written
 * `{ nodeId: 'node-1', engineType: 'lm-studio' }` fed straight to the store passes
 * with production dropping both fields, which is how the eject path stayed stuck
 * while its own unit test stayed green.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { EngineCommandPayload } from '@/shared/types/engine-api'
import type { ServiceError } from '@/shared/types/errors'

vi.mock('electron', () => ({
    // Both halves stay real code paths: `app` for the supervisor, `BrowserWindow`
    // for the broadcaster. With no window open, `emitBridgePush` only ever reaches
    // the in-process push bus, which is what the subscription below listens to.
    app: {
        isPackaged: false,
        getAppPath: () => process.cwd()
    },
    BrowserWindow: { getAllWindows: () => [] }
}))

vi.mock('@/shared/utils/log', () => ({
    createStructuredLogger: () => ({
        info: vi.fn(),
        warn: vi.fn(),
        error: vi.fn(),
        verbose: vi.fn()
    })
}))

vi.mock('@/electron/config/ui-config', () => ({
    isFirstRun: () => false
}))

import { JsonRpcSubprocess } from '@/electron/service-bridge/json-rpc-subprocess'
import { getModularSupervisor } from '@/electron/service-bridge/modular-supervisor'
import { getModularBridgeState } from '@/electron/service-bridge/modular-state'
import { handleServiceBridgeInvoke } from '@/electron/service-bridge/empty-handlers'
import { subscribePush } from '@/electron/service-bridge/push-bus'
import { usePendingActionsStore } from '@/ui/stores/pending-actions.store'

/**
 * A broker that refuses every `engine:action`, the way a refused eject or load
 * answers in the service, and replays each reported error as a one-error
 * `errors:update` snapshot — the shape the broker relay plus `nvpair-errors`
 * broadcast back after an `errors:report`.
 *
 * The 10 ms delay matters: the reply cannot be folded into the write that asked
 * for it, so only an actually observed response can release the row. Exiting on
 * `shutdown` keeps the teardown's `stop()` from waiting out its grace.
 */
const FAKE_BROKER = `
const readline = require('readline').createInterface({ input: process.stdin })
const write = message => process.stdout.write(JSON.stringify(message) + '\\n')
readline.on('line', line => {
    const message = JSON.parse(line)
    if (message.method === 'shutdown') process.exit(0)
    if (message.method === 'engine:action' && message.id !== undefined) {
        setTimeout(() => {
            write({
                jsonrpc: '2.0',
                id: message.id,
                error: { code: -32000, message: 'refused by the engine' }
            })
        }, 10)
    }
    if (message.method === 'errors:report' && message.params) {
        write({ jsonrpc: '2.0', method: 'errors:update', params: [message.params] })
    }
})
`

/** The supervisor state this has to stage: the broker child and its readiness. */
interface SupervisorHarness {
    processes: Map<string, JsonRpcSubprocess>
    brokerReady: boolean
    attachChildHandlers: (child: JsonRpcSubprocess) => void
}

/** The bridge state this has to stage: which node this machine is, and what it has already reported. */
interface BridgeStateHarness {
    selfId: string | null
    errors: ServiceError[]
}

// Neither half of the path can be staged through the public surface. The broker
// child lives in the supervisor's process map and its notification handlers are
// attached privately; `reportError` only takes the `errors:report` relay while the
// broker reads ready; and the node id it defaults to comes from a `selfId` that
// `setSelfId` cannot unset once set. Same reach-in as
// `modular-supervisor-readiness.test.ts`.
const supervisor = getModularSupervisor() as unknown as SupervisorHarness
const bridgeState = getModularBridgeState() as unknown as BridgeStateHarness

const LOCAL_NODE = 'local-node'
const PEER_NODE = 'peer-node'
const MTPLX_MODEL = 'mtplx/mtplx-lite'
const LMSTUDIO_MODEL = 'publisher/demo'

/** Every `errors:update` payload handed on since the last reset, oldest first. */
let pushedSnapshots: ServiceError[][] = []
let unsubscribeCapture: (() => void) | null = null
let broker: JsonRpcSubprocess | null = null

function errorReport(): string {
    const seen = pushedSnapshots.flat()
    return seen.length === 0 ? 'nothing was reported' : `reported ${JSON.stringify(seen)}`
}

/**
 * Wait for a control to stop spinning, and say on failure whether nothing was
 * reported at all or the report simply carried no node and engine to attribute it
 * to. Those are two different defects — a reply nobody observed, and an observed
 * reply nobody could place — and a test going red should not make you guess which
 * one came back.
 *
 * Real timers, deliberately. The round trip needs real async stdio, and at this
 * scale the 60 s safety net cannot expire, so the only way a row can be released
 * inside the wait is the error path under test.
 */
async function expectCleared(label: string, readPending: () => string | undefined): Promise<void> {
    await vi.waitFor(
        () => {
            expect(readPending(), `${label} still spinning; ${errorReport()}`).toBeUndefined()
        },
        { timeout: 8_000, interval: 20 }
    )
}

/**
 * Seed the optimistic entry the way the renderer's `fireCommand` does — `begin`
 * with the payload, then the same payload into `engine:command` — and wait for it
 * to clear. Recording it first is part of the test: a row that was never spinning
 * proves nothing about a fix meant to stop it spinning.
 */
async function fireUntilCleared(
    label: string,
    payload: EngineCommandPayload,
    readPending: () => string | undefined
): Promise<void> {
    usePendingActionsStore.getState().begin(payload)
    expect(readPending(), `${label}: the click recorded no pending entry`).toBeDefined()

    await handleServiceBridgeInvoke('engine:command', payload)
    await expectCleared(label, readPending)
}

/**
 * The subscription shape `window.pairApi` has in the app, fed from the push bus
 * rather than IPC. Everything behind it stays real: parsing the snapshot, deciding
 * which keys it releases, and the safety net that was the only way out before.
 */
function makeFakeWindow() {
    const noopUnsub = (): void => {}
    return {
        pairApi: {
            engines: {
                onStateChanged: () => noopUnsub,
                onProgress: () => noopUnsub,
                onSettingsChanged: () => noopUnsub
            },
            errors: {
                onUpdate: (cb: (errors: ServiceError[]) => void) =>
                    subscribePush(event => {
                        if (event.channel !== 'errors:update') return
                        cb(event.payload)
                    })
            }
        }
    }
}

beforeEach(() => {
    pushedSnapshots = []

    const child = new JsonRpcSubprocess('broker', process.execPath)
    broker = child
    supervisor.processes.set('broker', child)
    supervisor.attachChildHandlers(child)
    supervisor.brokerReady = true
    child.start(['-e', FAKE_BROKER])

    bridgeState.errors = []
    bridgeState.selfId = LOCAL_NODE

    // A second subscriber, used only to describe the failure. The store keeps its
    // own subscription through `window.pairApi`, which `cleanup()` releases.
    unsubscribeCapture = subscribePush(event => {
        if (event.channel === 'errors:update') pushedSnapshots.push(event.payload)
    })

    vi.stubGlobal('window', makeFakeWindow())
    usePendingActionsStore.getState().initialize()
})

afterEach(async () => {
    usePendingActionsStore.getState().cleanup()
    unsubscribeCapture?.()
    unsubscribeCapture = null

    const child = broker
    broker = null
    if (child) {
        // Retire the child before stopping it: the exit handler only acts for the
        // broker currently in the map, so a test that already finished cannot start
        // a broker-crash notification.
        supervisor.processes.delete('broker')
        supervisor.brokerReady = false
        await child.stop(2_000)
    }

    vi.unstubAllGlobals()
})

describe('a refused model action releases the row it was clicked from', () => {
    it('releases an eject whose engine refused to drop the model', async () => {
        // MTPLX ejects by quitting the server, and refuses outright when the named
        // model is not the one it is serving. Either way the model stays loaded, so
        // no model-list push would have released the row.
        await fireUntilCleared(
            'mtplx eject of mtplx/mtplx-lite',
            {
                command: 'unloadModel',
                engineType: 'mtplx',
                nodeId: LOCAL_NODE,
                model: MTPLX_MODEL
            },
            () =>
                usePendingActionsStore.getState().getModelPending(LOCAL_NODE, 'mtplx', MTPLX_MODEL)
        )
    })

    it('releases a load that the engine refused', async () => {
        // A failed `lms load` is reported nowhere else: the model never enters
        // memory, so there is no inventory change to release the row either.
        await fireUntilCleared(
            'lm-studio load of publisher/demo',
            {
                command: 'loadModel',
                engineType: 'lm-studio',
                nodeId: LOCAL_NODE,
                model: LMSTUDIO_MODEL
            },
            () =>
                usePendingActionsStore
                    .getState()
                    .getModelPending(LOCAL_NODE, 'lm-studio', LMSTUDIO_MODEL)
        )
    })

    it('names the node the row is keyed on when this machine has no id of its own', async () => {
        // While this machine has no cluster id, `handleEngineCommand` routes the
        // command on the local path and `reportError` would fall back to an empty
        // nodeId — which the store skips, since an error without a node belongs to
        // no row. The nodeId has to come from the payload either way.
        bridgeState.selfId = null

        await fireUntilCleared(
            'peer-node eject of mtplx/mtplx-lite',
            {
                command: 'unloadModel',
                engineType: 'mtplx',
                nodeId: PEER_NODE,
                model: MTPLX_MODEL
            },
            () => usePendingActionsStore.getState().getModelPending(PEER_NODE, 'mtplx', MTPLX_MODEL)
        )
    })
})

describe('a refused remote command releases the peer control it was aimed at', () => {
    it('clears the peer uninstall and leaves the local one alone', async () => {
        // Remote uninstall is not supported, and the refusal is the only thing that
        // will ever answer this click. A seeded local entry rides along to show the
        // report is attributed to the peer the command was aimed at, not to
        // whichever node `reportError` would default to.
        usePendingActionsStore
            .getState()
            .begin({ command: 'uninstall', engineType: 'mtplx', nodeId: LOCAL_NODE })
        usePendingActionsStore
            .getState()
            .begin({ command: 'uninstall', engineType: 'mtplx', nodeId: PEER_NODE })
        expect(
            usePendingActionsStore.getState().getLifecyclePending(PEER_NODE, 'mtplx'),
            'the peer uninstall was never recorded'
        ).toBe('uninstall')

        await handleServiceBridgeInvoke('engine:command', {
            command: 'uninstall',
            engineType: 'mtplx',
            nodeId: PEER_NODE
        })

        await expectCleared('peer mtplx uninstall', () =>
            usePendingActionsStore.getState().getLifecyclePending(PEER_NODE, 'mtplx')
        )
        expect(
            usePendingActionsStore.getState().getLifecyclePending(LOCAL_NODE, 'mtplx'),
            `a refusal aimed at ${PEER_NODE} released the local row instead; ${errorReport()}`
        ).toBe('uninstall')
    })
})
