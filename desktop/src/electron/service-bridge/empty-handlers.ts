// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { WsInvokeChannel, WsInvokeRequest, WsInvokeResponse } from '@/shared/types/ws-channels'
import type { ClusterInitialSnapshot } from '@/shared/types/bootstrap'
import type { ClusterNode, ClusterNodeIdentity, Invite } from '@/shared/types/cluster'
import type { EngineType } from '@/shared/types/engines'
import type { ServiceError } from '@/shared/types/errors'
import {
    MODULAR_CLUSTER_MANAGER_PORT,
    MODULAR_ENGINE_LIFECYCLE_CALL_TIMEOUT_MS
} from '@/shared/constants/modular-runtime'
import getErrorString from '@/shared/utils/get-error-string'
import { getEngineHubModels } from '@/electron/model-hub'
import { getModularSupervisor } from './modular-supervisor'
import {
    getModularBridgeState,
    isProxyEngine,
    isUpstreamUnreachableError,
    parseServiceErrors,
    parseWorkloadsInitial
} from './modular-state'
import type { ProxyEngine } from './modular-state'
import type { JsonObject, JsonValue } from './json-rpc-subprocess'
import { emptyInvite, parseClusterNodes, parseInvite, parseNodeIdentity } from './cluster-json'
import { removeManualNodeEntry, resolveManualNodeKey } from './manual-nodes-store'

type BridgeHandler<C extends WsInvokeChannel> = (
    payload?: WsInvokeRequest<C>
) => WsInvokeResponse<C> | Promise<WsInvokeResponse<C>>

type BridgeHandlerMap = {
    [C in WsInvokeChannel]: BridgeHandler<C>
}

// The broker allows pairing exchanges up to 30 seconds; keep the UI bridge outside that deadline.
const CLUSTER_PAIRING_CALL_TIMEOUT_MS = 35_000

function wait(ms: number): Promise<void> {
    return new Promise(resolve => setTimeout(resolve, ms))
}

function objectValue(value: JsonValue | undefined): JsonObject | null {
    if (!value || typeof value !== 'object' || Array.isArray(value)) return null
    return value
}

function stringValue(value: JsonValue | undefined): string {
    return typeof value === 'string' ? value : ''
}

function booleanValue(value: JsonValue | undefined): boolean {
    return typeof value === 'boolean' ? value : false
}

function numberValue(value: JsonValue | undefined): number {
    return typeof value === 'number' ? value : 0
}

/** Relay a `cluster:*` / `nodes:*` request to nvpair-cluster-manager via the broker. */
function callCluster(
    method: string,
    params?: JsonObject,
    timeoutMs?: number
): Promise<JsonValue | undefined> {
    return getModularSupervisor().callProcess('broker', method, params, timeoutMs)
}

async function getClusterIdentity(): Promise<ClusterNodeIdentity> {
    try {
        return parseNodeIdentity(await callCluster('cluster:get-node-id'))
    } catch {
        return { nodeUuid: '', nodeId: '', name: '', certFingerprint: '', clusterId: '' }
    }
}

async function getClusterMembers(): Promise<ClusterNode[]> {
    try {
        return parseClusterNodes(await callCluster('nodes:get-initial'))
    } catch {
        return []
    }
}

/** Map our `EngineType` onto the `nvpair-engine-manager` engine identifier. */
export function engineManagerEngineName(engineType: EngineType): string {
    return engineType === 'lm-studio' ? 'lmstudio' : engineType
}

/** The proxy engine for a wire `EngineType`, or null for loopback-only engines. */
function proxyEngineFor(engineType: EngineType): ProxyEngine | null {
    return isProxyEngine(engineType) ? engineType : null
}

async function handleGetSelfId(): Promise<string | null> {
    const state = getModularBridgeState()
    const identity = await getClusterIdentity()
    // Self is the stable node UUID — the same key discovery/proxy/workloads/
    // errors use — never the hostname. `nodeUuid` is minted at cluster-manager
    // startup, so it is available immediately.
    if (identity.nodeUuid) {
        state.setSelfId(identity.nodeUuid)
        return identity.nodeUuid
    }

    const current = state.getSelfId()
    if (current) return current

    for (let attempt = 0; attempt < 10; attempt += 1) {
        await wait(500)
        const next = state.getSelfId()
        if (next) return next
    }

    return null
}

async function nodeSettingString(method: string): Promise<string> {
    try {
        // `nvpair-node-settings` is broker-supervised; the broker relays the
        // `settings/` namespace verbatim to it.
        const result = await getModularSupervisor().callProcess('broker', method)
        return stringValue(objectValue(result)?.value)
    } catch {
        return ''
    }
}

async function toggleLocalEngine(engine: string, engineType: EngineType): Promise<void> {
    const supervisor = getModularSupervisor()
    try {
        // `engine:status` is a fast detect (no lifecycle lock), so awaiting the
        // read is fine. Lifecycle ops stay fire-and-forget to the renderer. A stop
        // observes its eventual response because an ownership rejection has no
        // resolving `engine:state-changed`; start keeps its existing push path.
        const status = await supervisor.callProcess('broker', 'engine:status', { engine })
        const running = booleanValue(objectValue(status)?.running)
        // Synthesize the transitional status the engine-manager never emits, so
        // the UI shows a spinner immediately instead of sitting idle until the
        // terminal engine:state-changed lands.
        getModularBridgeState().beginLocalEngineOp(engineType, running ? 'stopping' : 'starting')
        supervisor.sendProcess(
            'broker',
            running ? 'engine:stop' : 'engine:start',
            { engine },
            error => {
                // A send failure or rejected stop has no resolving
                // engine:state-changed — clear the optimistic spinner and report.
                getModularBridgeState().clearPendingEngineOp(engineType)
                supervisor.reportError(
                    `Failed to ${running ? 'stop' : 'start'} ${engine}: ${error}`,
                    'error',
                    `engine-cmd:toggle:${engine}`
                )
            },
            running
        )
    } catch (err) {
        supervisor.reportError(
            `Failed to toggle ${engine}: ${getErrorString(err)}`,
            'error',
            `engine-cmd:toggle:${engine}`
        )
    }
}

/**
 * Set a local engine's HTTP server port. `nvpair-engine-manager`'s `engine:set-port`
 * **persists** the chosen port as a per-user manifest override (the single source
 * of truth, restored on restart) and applies it live: a running engine is bounced
 * onto the new port, a stopped one just records it, and setting the port back to
 * the bundled default drops the override. Awaited because the RPC returns only
 * after the rebind completes; its terminal `engine:state-changed` clears the
 * optimistic op below.
 *
 * A **running adopted** engine (one PAIR attached to instead of launching) is
 * refused by the backend — PAIR can't move a process it didn't start — so we clear
 * the optimistic op and surface a clear "stop it in its own app first" error.
 * Remaining start-options limitation (env/args) is documented in
 * docs/services-parity.md#engine-lifecycle.
 */
async function applyEnginePort(
    engine: string,
    engineType: EngineType,
    port: number
): Promise<void> {
    const supervisor = getModularSupervisor()
    const state = getModularBridgeState()
    // Optimistic spinner during the rebind round-trip; the engine-manager's
    // terminal engine:state-changed (running or stopped) clears it.
    state.beginLocalEngineOp(engineType, 'starting')
    try {
        await supervisor.callProcess(
            'broker',
            'engine:set-port',
            { engine, port },
            MODULAR_ENGINE_LIFECYCLE_CALL_TIMEOUT_MS
        )
    } catch (err) {
        // Refused on a running adopted engine (and any other set-port failure):
        // no engine:state-changed follows, so clear the optimistic op now instead
        // of leaving a spinner up until the idle-timeout.
        state.failLocalEngineOp(engine, 'start')
        supervisor.reportError(
            `Failed to set ${engine} port: ${getErrorString(err)}`,
            'error',
            `engine-cmd:setPort:${engine}`
        )
    }
}

/**
 * Set the local Ollama proxy's listen port. Routed through the broker's
 * `proxy:set-port` relay, which persists the port (`proxy-port.json`, restored on
 * restart), live-rebinds the proxy, and **steers it clear of any running engine
 * port** (engines win — a bumped proxy surfaces a sticky `warning` on the errors
 * pipeline). The proxy re-emits `proxy:ready` with the actually-bound port, which
 * the bridge already folds into `status.proxyPort`, so success needs no further
 * action here beyond surfacing a failure.
 */
async function applyProxyPort(proxyEngine: ProxyEngine, port: number): Promise<void> {
    const supervisor = getModularSupervisor()
    if (!Number.isInteger(port) || port < 1 || port > 65535) {
        supervisor.reportError(
            `Invalid proxy port ${port}: must be between 1 and 65535.`,
            'warning',
            `engine-cmd:setPort:proxy:${proxyEngine}`
        )
        return
    }
    try {
        await supervisor.callProxy(proxyEngine, 'set-port', { port })
    } catch (err) {
        supervisor.reportError(
            `Failed to set the ${proxyEngine} proxy port: ${getErrorString(err)}`,
            'error',
            `engine-cmd:setPort:proxy:${proxyEngine}`
        )
    }
}

/**
 * Apply the engine server port and/or proxy port for the local node in one safe
 * transaction. The UI sends only the ports that changed; this picks an ordering
 * so the engine and proxy never fight over a port mid-flight — including a full
 * swap (server↔proxy).
 *
 * The backend has no atomic multi-port API, so we serialize the steps here:
 *  - **single port** → the focused `applyEnginePort` / `applyProxyPort` helper;
 *  - **both, engine stopped** → persist the engine port (no start, respecting
 *    the user's stopped state), then rebind the proxy;
 *  - **both, running, no collision** → bounce the engine onto its new port
 *    first (the failure-prone step — a running *adopted* engine is refused —
 *    so the proxy is left untouched if it throws), then rebind the proxy;
 *  - **both, running, collision/swap** → stop the engine to free its port, move
 *    the proxy (the engine's old port is now free for the proxy's target),
 *    persist the engine's new port, then start it back up.
 *
 * Every success path ends in an `engine:set-port` / `doStart` / `proxy:ready`
 * that re-emits authoritative state, which clears the optimistic op and snaps
 * the UI inputs to backend truth. On failure we clear the optimistic op and
 * report, so the UI reverts to the real ports.
 */
async function applyEnginePorts(
    engine: string,
    engineType: EngineType,
    enginePort: number | undefined,
    proxyPort: number | undefined
): Promise<void> {
    const supervisor = getModularSupervisor()
    const state = getModularBridgeState()

    const wantEngine = typeof enginePort === 'number'
    const wantProxy = typeof proxyPort === 'number'
    if (!wantEngine && !wantProxy) return

    const proxyEngine = proxyEngineFor(engineType)
    if (wantProxy && !proxyEngine) {
        supervisor.reportError(
            `The ${engineType} engine has no cluster proxy port to set.`,
            'warning',
            `engine-cmd:setPorts:proxy:${engineType}`
        )
        return
    }

    const validPort = (port: number): boolean =>
        Number.isInteger(port) && port >= 1 && port <= 65535

    if (wantEngine && !validPort(enginePort)) {
        supervisor.reportError(
            `Invalid server port ${enginePort}: must be between 1 and 65535.`,
            'warning',
            `engine-cmd:setPorts:${engine}`
        )
        return
    }
    if (wantProxy && !validPort(proxyPort)) {
        supervisor.reportError(
            `Invalid proxy port ${proxyPort}: must be between 1 and 65535.`,
            'warning',
            `engine-cmd:setPorts:proxy:${engineType}`
        )
        return
    }
    if (wantEngine && wantProxy && enginePort === proxyPort) {
        supervisor.reportError(
            'The server and proxy ports must be different.',
            'warning',
            `engine-cmd:setPorts:${engine}`
        )
        return
    }

    // Single-port changes delegate to the focused helpers (they own their own
    // optimistic op + error reporting).
    if (wantEngine && !wantProxy) {
        await applyEnginePort(engine, engineType, enginePort)
        return
    }
    if (wantProxy && !wantEngine && proxyEngine) {
        await applyProxyPort(proxyEngine, proxyPort)
        return
    }

    // Both changed past this point (the single-port cases returned above);
    // narrow the optional inputs to numbers for the rest of the transaction.
    if (typeof enginePort !== 'number' || typeof proxyPort !== 'number') return
    // wantProxy is true here, so proxyEngine was verified non-null above.
    if (!proxyEngine) return

    // Both changed. Read the live engine state to detect a transient collision
    // (the engine wants the proxy's current port, or the proxy wants the
    // engine's current port) that requires serializing through a stop.
    const currentProxy = state.getProxyPort(proxyEngine)
    let engineRunning = false
    let currentEnginePort = 0
    try {
        const status = objectValue(
            await supervisor.callProcess('broker', 'engine:status', { engine })
        )
        engineRunning = booleanValue(status?.running)
        currentEnginePort = numberValue(status?.port)
    } catch {
        // Status read failed — treat as stopped and take the safe persist path.
        engineRunning = false
    }

    state.beginLocalEngineOp(engineType, 'starting')
    try {
        if (!engineRunning) {
            // Nothing is bound: persist the engine port (no start), then rebind
            // the proxy. set-port on a stopped engine emits engine:state-changed
            // (running:false, new port), which clears the optimistic op.
            await supervisor.callProcess(
                'broker',
                'engine:set-port',
                {
                    engine,
                    port: enginePort
                },
                MODULAR_ENGINE_LIFECYCLE_CALL_TIMEOUT_MS
            )
            await supervisor.callProxy(proxyEngine, 'set-port', { port: proxyPort })
            return
        }

        const collision = enginePort === currentProxy || proxyPort === currentEnginePort
        if (!collision) {
            // Engine first (a running adopted engine is refused here, leaving the
            // proxy untouched), then rebind the proxy.
            await supervisor.callProcess(
                'broker',
                'engine:set-port',
                {
                    engine,
                    port: enginePort
                },
                MODULAR_ENGINE_LIFECYCLE_CALL_TIMEOUT_MS
            )
            await supervisor.callProxy(proxyEngine, 'set-port', { port: proxyPort })
            return
        }

        // Collision / swap: stop first so neither bind races the other.
        await supervisor.callProcess(
            'broker',
            'engine:stop',
            { engine },
            MODULAR_ENGINE_LIFECYCLE_CALL_TIMEOUT_MS
        )
        // The stop's engine:state-changed cleared the optimistic op — re-assert
        // it so the UI keeps a spinner through the rest of the transaction.
        state.beginLocalEngineOp(engineType, 'starting')
        await supervisor.callProxy(proxyEngine, 'set-port', { port: proxyPort })
        await supervisor.callProcess(
            'broker',
            'engine:set-port',
            {
                engine,
                port: enginePort
            },
            MODULAR_ENGINE_LIFECYCLE_CALL_TIMEOUT_MS
        )
        await supervisor.callProcess(
            'broker',
            'engine:start',
            { engine },
            MODULAR_ENGINE_LIFECYCLE_CALL_TIMEOUT_MS
        )
    } catch (err) {
        // No engine:state-changed follows a refused/failed step, so clear the
        // optimistic op now and report — the UI reverts to the real ports.
        state.failLocalEngineOp(engine, 'start')
        supervisor.reportError(
            `Failed to apply ${engine} ports: ${getErrorString(err)}`,
            'error',
            `engine-cmd:setPorts:${engine}`
        )
    }
}

/**
 * Dispatch a UI engine command to the local `nvpair-engine-manager`. Lifecycle and
 * model operations are **fire-and-forget**: the engine-manager runs each in its
 * own goroutine and reports progress (`engine:install-progress`), completion
 * (`engine:state-changed`), and failures (`errors:report`) through push events.
 * We must not await these RPCs — a download or model pull runs for minutes and
 * would trip the request timeout, fabricating a failure while the operation is
 * actually succeeding.
 */
function routeEngineManagerCommand(payload: WsInvokeRequest<'engine:command'>): void {
    const supervisor = getModularSupervisor()
    const state = getModularBridgeState()
    const engine = engineManagerEngineName(payload.engineType)
    // Use the engine-manager's stable error key. If the manager already reported
    // the operation failure, nvpair-errors upserts this fallback instead of showing
    // a duplicate; admission/transport failures still get a visible error.
    const failPendingOp = (action: string, operation: 'install' | 'uninstall') => {
        return (error: string): void => {
            state.clearPendingEngineOp(payload.engineType)
            supervisor.reportError(`Failed to ${action} ${engine}: ${error}`, 'error', undefined, {
                id: `engine-manager:${operation}-failed:${engine}`,
                engineType: engine,
                operation,
                action: 'retry'
            })
        }
    }
    const failAction =
        (
            action: string,
            context?: Pick<ServiceError, 'nodeId' | 'engineType' | 'operation' | 'modelName'>
        ) =>
        (error: string): void =>
            supervisor.reportError(
                `Failed to ${action} on ${engine}: ${error}`,
                'error',
                `engine-cmd:${action}:${engine}`,
                context
            )
    switch (payload.command) {
        case 'install':
        case 'installAll':
            state.beginLocalEngineOp(payload.engineType, 'installing')
            // Start the engine as soon as the install succeeds. The backend
            // performs install-then-start atomically and persists desired-enabled.
            supervisor.sendProcess(
                'broker',
                'engine:install',
                { engine, start: true },
                failPendingOp('install', 'install'),
                true
            )
            break
        case 'uninstall':
            state.beginLocalEngineOp(payload.engineType, 'uninstalling')
            supervisor.sendProcess(
                'broker',
                'engine:uninstall',
                { engine },
                failPendingOp('uninstall', 'uninstall'),
                true
            )
            break
        case 'update':
            // The engine-manager serializes per-engine ops via its lifecycle
            // lock, so the queued install waits for the uninstall to finish. The
            // uninstall's engine:state-changed briefly clears this, then the
            // install's progress re-establishes `installing`.
            state.beginLocalEngineOp(payload.engineType, 'installing')
            supervisor.sendProcess(
                'broker',
                'engine:uninstall',
                { engine },
                failPendingOp('update', 'uninstall'),
                true
            )
            supervisor.sendProcess(
                'broker',
                'engine:install',
                { engine, start: true },
                failPendingOp('update', 'install'),
                true
            )
            break
        case 'toggle':
            void toggleLocalEngine(engine, payload.engineType)
            break
        case 'setServedModel':
            // vLLM serves one model per process, so the model is a start-time
            // setting rather than a model operation. engine:set-model persists it
            // as a manifest override — the same mechanism engine:set-port uses —
            // and restarts a running engine onto it. Local only: the backend
            // exposes no remote served-model control.
            supervisor.sendProcess(
                'broker',
                'engine:set-model',
                { engine, model: payload.model ?? '' },
                failAction('set the served model', {
                    nodeId: payload.nodeId,
                    engineType: payload.engineType,
                    modelName: payload.model
                }),
                true
            )
            break
        case 'setPorts':
            // Both ports persist on this node: the engine HTTP server port via
            // engine:set-port (manifest override), the proxy port via the broker's
            // proxy:set-port (which steers it clear of running engine ports). The
            // bridge diffs + orders them safely (incl. swaps). Local only — the
            // backend exposes no remote port control.
            void applyEnginePorts(engine, payload.engineType, payload.enginePort, payload.proxyPort)
            break
        case 'pullModel':
            // Pulls need UI feedback the backend doesn't emit (progress + failure),
            // so the supervisor owns the optimistic-progress + awaited-completion
            // orchestration. Void it: the command stays fire-and-forget for the UI.
            if (payload.model) {
                void supervisor.pullModel(engine, payload.engineType, payload.model)
            }
            break
        case 'deleteModel':
            if (payload.model) {
                void supervisor.deleteModel(engine, payload.engineType, payload.model)
            }
            break
        case 'loadModel':
            // "Load" warms a model into the engine's memory/VRAM. Ollama has no
            // first-class load action, so we POST its `run_model` HTTP action
            // (`/api/generate`) with no prompt: Ollama loads the model into VRAM
            // and returns immediately (`done_reason: "load"`) without generating.
            // LM Studio declares a real `load_model` CLI action (`lms load`).
            // Both require the engine running (HTTP/CLI action), matching the
            // disabled rule in ModelRow.tsx.
            // See docs/services-parity.md#models.
            if (payload.model) {
                if (payload.engineType === 'ollama') {
                    supervisor.sendProcess(
                        'broker',
                        'engine:action',
                        {
                            engine,
                            action: 'run_model',
                            params: { model: payload.model, stream: false }
                        },
                        failAction('load model', {
                            nodeId: payload.nodeId,
                            engineType: payload.engineType,
                            operation: 'load',
                            modelName: payload.model
                        }),
                        true
                    )
                } else {
                    supervisor.sendProcess(
                        'broker',
                        'engine:action',
                        {
                            engine,
                            action: 'load_model',
                            params: { model: payload.model }
                        },
                        failAction('load model')
                    )
                }
            }
            break
        case 'unloadModel':
            if (payload.model) {
                const unloadParams: JsonObject =
                    payload.engineType === 'ollama'
                        ? { model: payload.model, keep_alive: 0 }
                        : { model: payload.model }
                supervisor.sendProcess(
                    'broker',
                    'engine:action',
                    {
                        engine,
                        action: 'unload_model',
                        params: unloadParams
                    },
                    failAction('unload model')
                )
            }
            break
        default:
            supervisor.reportError(
                `${payload.command} is not supported by nvpair-engine-manager yet.`,
                'warning',
                `engine-cmd:${payload.command}`
            )
    }
}

async function toggleRemoteEngine(
    nodeId: string,
    engine: string,
    engineType: EngineType
): Promise<void> {
    const supervisor = getModularSupervisor()
    const running = getModularBridgeState().isRemoteEngineRunning(nodeId, engineType)
    await supervisor.toggleEngineRemote(nodeId, engine, engineType, running)
}

/**
 * Dispatch a UI engine command to a remote peer via `nvpair-engine-manager`'s
 * `engine:remote-*` client methods (cluster-scoped mTLS `ec` surface). Install,
 * start/stop, pull, and model load/unload/delete are supported remotely;
 * uninstall, update, and port changes remain local-only.
 * (optimistic status, awaited completion, authoritative refresh) lives on the
 * supervisor because the backend settles these ops only via the RPC reply, never
 * a terminal notification.
 */
function routeRemoteEngineCommand(payload: WsInvokeRequest<'engine:command'>): void {
    const supervisor = getModularSupervisor()
    const engine = engineManagerEngineName(payload.engineType)
    const nodeId = payload.nodeId

    const refuseRemote = (detail: string): void => {
        supervisor.reportError(detail, 'warning', `engine-cmd:remote:${payload.command}`)
    }

    switch (payload.command) {
        case 'install':
        case 'installAll':
            void supervisor.installEngineRemote(nodeId, engine, payload.engineType, true)
            break
        case 'toggle':
            void toggleRemoteEngine(nodeId, engine, payload.engineType)
            break
        case 'pullModel':
            if (payload.model) {
                void supervisor.pullModelRemote(nodeId, engine, payload.engineType, payload.model)
            }
            break
        case 'uninstall':
        case 'update':
            refuseRemote(
                `${payload.command} is only available on the local node — remote uninstall/update is not supported yet.`
            )
            break
        case 'setPorts':
            refuseRemote(
                'Port changes only apply to this machine — remote engine port control is not supported.'
            )
            break
        case 'deleteModel':
        case 'loadModel':
        case 'unloadModel':
            if (payload.model) {
                void supervisor.modelActionRemote(
                    nodeId,
                    engine,
                    payload.engineType,
                    payload.model,
                    payload.command
                )
            }
            break
        default:
            supervisor.reportError(
                `${payload.command} is not supported on remote nodes.`,
                'warning',
                `engine-cmd:remote:${payload.command}`
            )
    }
}

async function handleEngineCommand(payload?: WsInvokeRequest<'engine:command'>): Promise<null> {
    if (!payload) return null
    const supervisor = getModularSupervisor()

    // "Load" loads a model into the engine's memory/VRAM — never proxy routing.
    // Proxy node selection is owned by the backend nvpair-job-scheduler (it drives
    // node/set-priority via the broker); PAIR UI never pins node/select, and a user
    // clicking "Load" must not pin a route. Ollama loads via its `run_model` HTTP
    // action, LM Studio via its `load_model` CLI action — both handled locally in
    // routeEngineManagerCommand.

    // Remote peers: `nvpair-engine-manager` exposes `engine:remote-*` client methods
    // (cluster mTLS `ec` surface) for install/start/stop/pull and model ops.
    // Local-only ops are refused in {@link routeRemoteEngineCommand}.
    const selfId = getModularBridgeState().getSelfId()
    if (selfId && payload.nodeId && payload.nodeId !== selfId) {
        if (supervisor.hasProcess('broker')) {
            routeRemoteEngineCommand(payload)
            return null
        }
        supervisor.reportError(
            `${payload.command} is not available until the modular backend is running.`,
            'warning',
            `engine-cmd:${payload.command}`,
            {
                nodeId: payload.nodeId,
                engineType: payload.engineType,
                modelName: payload.model
            }
        )
        return null
    }

    // Lifecycle + model management is the local engine-manager's job, reached
    // through the broker's `engine:*` relay (the broker supervises it).
    if (supervisor.hasProcess('broker')) {
        routeEngineManagerCommand(payload)
        return null
    }

    supervisor.reportError(
        `${payload.command} is not available until the modular backend is running.`,
        'warning',
        `engine-cmd:${payload.command}`,
        {
            nodeId: payload.nodeId,
            engineType: payload.engineType,
            modelName: payload.model
        }
    )
    return null
}

async function handleClusterGetInitial(): Promise<ClusterInitialSnapshot> {
    const identity = await getClusterIdentity()
    const members = await getClusterMembers()
    // node-settings owns cluster_friendly_name; the cluster-manager identity does
    // not carry it. Hydrate it here so a cold-start snapshot includes the name.
    const clusterFriendlyName = await nodeSettingString('settings/get-cluster-friendly-name')
    return {
        info: {
            clusterId: identity.clusterId || null,
            isClustered: identity.clusterId !== '',
            clusterFriendlyName
        },
        identity,
        members,
        // The cluster-manager has no list-pending-invites RPC; main accumulates
        // inbound invites from the `cluster:invite-received` push and prunes them
        // as they resolve, so hydrate the snapshot from that authoritative set.
        pendingInvites: getModularBridgeState().getPendingInvites()
    }
}

async function handleErrorsGetInitial(): Promise<ServiceError[]> {
    const supervisor = getModularSupervisor()
    if (supervisor.hasProcess('broker')) {
        try {
            const result = await supervisor.callProcess('broker', 'errors:get-initial')
            // Drop per-node upstream-unreachable warnings here too so the initial
            // snapshot never shows them. The state fallback below already stored
            // only the filtered list, so it needs no extra filtering.
            return parseServiceErrors(result ?? null).filter(
                error => !isUpstreamUnreachableError(error)
            )
        } catch {
            return getModularBridgeState().getErrors()
        }
    }
    return getModularBridgeState().getErrors()
}

/**
 * Fetch the authoritative workload baseline from the broker's durable store
 * (`workloads:get-initial`, added this sync) and seed the bridge catalog with
 * it, mirroring {@link handleErrorsGetInitial}. Falls back to the in-memory map
 * on failure or before the broker is up. Shared by the renderer store and the
 * CLI `workloads list`.
 */
async function handleWorkloadsGetInitial(): Promise<WsInvokeResponse<'workloads:get-initial'>> {
    const supervisor = getModularSupervisor()
    const state = getModularBridgeState()
    if (supervisor.hasProcess('broker')) {
        try {
            const result = await supervisor.callProcess('broker', 'workloads:get-initial')
            return state.seedWorkloads(parseWorkloadsInitial(result ?? null))
        } catch {
            return state.getWorkloads()
        }
    }
    return state.getWorkloads()
}

function handleErrorsClear(id: string): null {
    const supervisor = getModularSupervisor()
    if (supervisor.hasProcess('broker')) {
        void supervisor.callProcess('broker', 'errors:clear', { id }).catch(() => {})
        return null
    }
    getModularBridgeState().clearError(id)
    return null
}

async function handleClusterInviteNode(
    payload?: WsInvokeRequest<'cluster:invite-node'>
): Promise<Invite> {
    if (!payload) return emptyInvite()

    // cluster-manager auto-founds a solo cluster on the first invite while
    // unclustered (under inviteMu, with invite-created provenance). Do not
    // pre-call cluster:create here: parallel Invites used to race concurrent
    // creates, and an explicit create clears invite-created so the backend
    // would not dissolve a failed throwaway solo.
    //
    // The renderer never knows the backend pairing port. The cluster-manager's
    // EAP-NOOB pairing server listens on MODULAR_CLUSTER_MANAGER_PORT; inject it
    // here at the backend boundary. See the current client-side responsibilities
    // in docs/services-parity.md.
    return parseInvite(
        await callCluster(
            'cluster:invite-node',
            {
                address: payload.ipAddress,
                port: MODULAR_CLUSTER_MANAGER_PORT
            },
            CLUSTER_PAIRING_CALL_TIMEOUT_MS
        )
    )
}

/**
 * Called by the UI when an outbound pairing reaches a terminal non-paired state
 * (declined / expired / failed / abandoned). Historically dissolved a desktop-
 * marked throwaway solo created via pre-invite cluster:create. Founding now
 * lives entirely in cluster-manager with invite-created provenance, which
 * dissolves itself on terminal outbound invites; this handler is a no-op unless
 * a legacy in-memory mark is still set.
 */
async function handleClusterAbandonIfSolo(): Promise<null> {
    const supervisor = getModularSupervisor()
    if (!supervisor.isAutoCreatedSoloForInvite()) return null

    const members = await getClusterMembers()
    const selfId = getModularBridgeState().getSelfId()
    // Self is keyed by nodeUuid; `id` is the hostname (display only).
    const hasPeer = members.some(m => m.state === 'member' && m.nodeUuid !== selfId)
    if (hasPeer) {
        supervisor.clearAutoCreatedSoloForInvite()
        return null
    }

    // Undo the throwaway solo cluster via the same canonical self-departure as a
    // user leave: `cluster:leave` resets the cluster-manager to unclustered and
    // emits `cluster:identity-changed` (empty) + `nodes:changed` (empty), which
    // the supervisor consumes to persist "unclustered" and collapse the UI.
    supervisor.clearAutoCreatedSoloForInvite()
    await callCluster('cluster:leave', {})
    return null
}

async function handleClusterInviteStatus(
    payload?: WsInvokeRequest<'cluster:invite-status'>
): Promise<Invite> {
    if (!payload) return emptyInvite()
    return parseInvite(await callCluster('cluster:invite-status', { inviteId: payload.inviteId }))
}

async function handleClusterRespondToInvite(
    payload?: WsInvokeRequest<'cluster:respond-to-invite'>
): Promise<Invite> {
    if (!payload) return emptyInvite()
    const params: JsonObject = { inviteId: payload.inviteId, accept: payload.accept }
    if (payload.pin !== undefined) params.pin = payload.pin
    const result = parseInvite(
        await callCluster('cluster:respond-to-invite', params, CLUSTER_PAIRING_CALL_TIMEOUT_MS)
    )
    // Any non-pending outcome is terminal for this invite (a wrong PIN evicts the
    // backend session, so `failed` is non-retryable too). Drop it from the
    // authoritative set now rather than waiting for the sweep.
    if (result.state !== 'pending') {
        getModularBridgeState().prunePendingInvite(payload.inviteId)
    }
    return result
}

/**
 * Abort a still-pending outbound invite this node originated. Relays to the
 * cluster-manager, which evicts its EAP-NOOB Server session (invalidating the
 * PIN so a later Completion is rejected) and best-effort signals the joiner to
 * drop its pending-inbound invite. Returns the updated `Invite` (`canceled`).
 * Prunes any matching entry from the authoritative set for good measure — the
 * outbound invite lives in the renderer pairing hook, so this is normally a
 * no-op, but it keeps a stray copy from lingering.
 */
async function handleClusterCancelInvite(
    payload?: WsInvokeRequest<'cluster:cancel-invite'>
): Promise<Invite> {
    if (!payload) return emptyInvite()
    const result = parseInvite(
        await callCluster(
            'cluster:cancel-invite',
            { inviteId: payload.inviteId },
            CLUSTER_PAIRING_CALL_TIMEOUT_MS
        )
    )
    getModularBridgeState().prunePendingInvite(payload.inviteId)
    return result
}

async function handleNodeRemoveMember(
    payload?: WsInvokeRequest<'nodes:remove-member'>
): Promise<WsInvokeResponse<'nodes:remove-member'>> {
    if (!payload) return { nodeId: '', removed: false }
    const supervisor = getModularSupervisor()
    const state = getModularBridgeState()
    const selfId = state.getSelfId()
    const isSelfLeave = selfId !== null && payload.nodeId === selfId

    // `payload.nodeId` is the node's UUID, but the manual-nodes store and the
    // broker's `node/remove` relay key a manual entry by the name it was added
    // with — never the UUID. Map the UUID back through the node's reachable
    // addresses to the persisted entry so it is actually pruned (and does not
    // reappear on the next replay). If it is not a manual node (no address match)
    // fall back to the display hostname, then the raw id; both are harmless
    // no-ops for a purely-discovered peer. The broker supervises
    // nvpair-manual-nodes and relays `node/*` verbatim.
    const manualKey =
        resolveManualNodeKey(state.getNodeAddresses(payload.nodeId)) ||
        state.getNodeHostname(payload.nodeId) ||
        payload.nodeId
    removeManualNodeEntry(manualKey)
    supervisor.callProcess('broker', 'node/remove', { id: manualKey }).catch(() => {})

    if (isSelfLeave) {
        // Self-departure is `cluster:leave`, not `nodes:remove` (the cluster-manager
        // now rejects a self-targeted `nodes:remove`). `cluster:leave` announces a
        // signed self-tombstone to peers, drops every pin/member, resets to
        // unclustered, then emits `cluster:identity-changed` (empty clusterId) and
        // `nodes:changed` (empty) — both already consumed by the supervisor, which
        // persists "unclustered" to node-settings and collapses the UI. No manual
        // identity clear / roster push needed.
        supervisor.clearAutoCreatedSoloForInvite()
        const leaveResult = objectValue(await callCluster('cluster:leave', {}))
        return { nodeId: payload.nodeId, removed: booleanValue(leaveResult?.left) }
    }

    // Peer removal: revoke the peer's membership + pinned trust.
    const result = objectValue(await callCluster('nodes:remove', { nodeId: payload.nodeId }))
    return {
        nodeId: stringValue(result?.nodeId) || payload.nodeId,
        removed: booleanValue(result?.removed)
    }
}

const EMPTY_SERVICE_BRIDGE_HANDLERS: BridgeHandlerMap = {
    'app:get-initial': async () => ({
        connected: getModularSupervisor().ready,
        selfId: await handleGetSelfId()
    }),

    'nodes:get-initial': () => getModularBridgeState().getNodesInitial(),
    'nodes:remove-member': payload => handleNodeRemoveMember(payload),

    'discovery:get-nodes': () => getModularBridgeState().getAvailableNodes(),

    'cluster:get-initial': () => handleClusterGetInitial(),
    'cluster:invite-node': payload => handleClusterInviteNode(payload),
    'cluster:invite-status': payload => handleClusterInviteStatus(payload),
    'cluster:respond-to-invite': payload => handleClusterRespondToInvite(payload),
    'cluster:cancel-invite': payload => handleClusterCancelInvite(payload),
    'cluster:abandon-if-solo': () => handleClusterAbandonIfSolo(),

    'engines:get-initial': () => getModularBridgeState().getEngineInitialState(),
    'engine:command': payload => handleEngineCommand(payload),
    'engine:search-hub': payload =>
        payload ? getEngineHubModels(payload.engineType) : { models: [] },

    'errors:get-initial': () => handleErrorsGetInitial(),
    'errors:clear': payload => (payload ? handleErrorsClear(payload) : null),

    'workloads:get-initial': () => handleWorkloadsGetInitial()
}

export function handleServiceBridgeInvoke<C extends WsInvokeChannel>(
    channel: C,
    payload: WsInvokeRequest<C>
): Promise<WsInvokeResponse<C>> {
    return Promise.resolve(EMPTY_SERVICE_BRIDGE_HANDLERS[channel](payload))
}
