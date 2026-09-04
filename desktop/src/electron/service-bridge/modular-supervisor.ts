// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import fs from 'fs'
import path from 'path'
import { app } from 'electron'
import {
    JsonRpcResponseError,
    JsonRpcSubprocess,
    type JsonObject,
    type JsonRpcInboundRequest,
    type JsonRpcNotification,
    type JsonValue
} from './json-rpc-subprocess'
import { createStructuredLogger } from '@/shared/utils/log'
import getErrorString from '@/shared/utils/get-error-string'
import { currentPlatform } from '@/shared/utils/platform'
import {
    getModularBridgeState,
    isProxyEngine,
    isUpstreamUnreachableError,
    parseServiceErrors,
    parseWorkloadsInitial,
    PROXY_ENGINES,
    type ProxyEngine
} from './modular-state'
import { emitBridgePush } from './broadcaster'
import { resolvePullCatchError } from './pull-error-handling'
import { serviceLogLevel } from './service-log-level'
import { engineManagerEngineName } from './empty-handlers'
import { isFirstRun } from '@/electron/config/ui-config'
import { parseClusterNodes, parseInvite, parseNodeIdentity } from './cluster-json'
import { startNodeInfoPoller, stopNodeInfoPoller } from './node-info-poller'
import {
    MODULAR_DEFAULT_LOG_LEVEL,
    MODULAR_INVITE_STATUS_POLL_INTERVAL_MS,
    MODULAR_MODEL_ACTION_TIMEOUT_MS,
    isModularLogLevel,
    type ModularLogLevel
} from '@/shared/constants/modular-runtime'
import { listManualNodeEntries, manualPortsToWire } from './manual-nodes-store'
import {
    MODULAR_RUNTIME_BINARIES,
    modularBinaryFileName
} from '@/shared/constants/modular-binaries'
import type { ModularProcessName } from '@/shared/constants/modular-binaries'
import type { ServiceError, ServiceErrorSeverity } from '@/shared/types/errors'
import type { ClusterNode } from '@/shared/types/cluster'
import type { EngineType } from '@/shared/types/engines'
import { APP_DISPLAY_NAME } from '@/shared/constants/app'

const log = createStructuredLogger('service-bridge')
const ENGINE_PREPARE_SHUTDOWN_METHOD = 'engine:prepare-shutdown'

export class ModularStartupTimeoutError extends Error {
    constructor(timeoutMs: number) {
        super(
            `${APP_DISPLAY_NAME} service did not become ready within ${Math.round(timeoutMs / 1000)} seconds`
        )
        this.name = 'ModularStartupTimeoutError'
    }
}

interface ReadinessWaiter {
    resolve: () => void
    reject: (error: Error) => void
    timeout: ReturnType<typeof setTimeout>
}

/** Ask engine-manager to stop runtime processes without changing saved intent. */
export async function prepareLocalEnginesForShutdown(
    broker: Pick<JsonRpcSubprocess, 'call'>
): Promise<void> {
    await broker.call(ENGINE_PREPARE_SHUTDOWN_METHOD, undefined, 30_000)
}

export function getCliBinDir(): string {
    if (app.isPackaged) {
        return path.join(process.resourcesPath, 'cli-bin')
    }

    return path.join(app.getAppPath(), 'cli-bin')
}

function getModularBinaryPath(baseName: string): string {
    return path.join(getCliBinDir(), modularBinaryFileName(baseName, currentPlatform()))
}

/**
 * Read the build provenance (`sourceFingerprint` + `product`) and per-component
 * versions that `scripts/build-modular-binaries.ts` stamps into
 * `cli-bin/manifest.json`.
 * `components` is keyed by binary base name (e.g. `ollama-proxy`). Returns empty
 * values when the manifest is absent.
 */
export function readCliBinManifest(): {
    commit: string
    product: string
    components: Record<string, string>
} {
    try {
        const manifestPath = path.join(getCliBinDir(), 'manifest.json')
        if (!fs.existsSync(manifestPath)) return { commit: '', product: '', components: {} }
        const parsed: JsonValue = JSON.parse(fs.readFileSync(manifestPath, 'utf8'))
        const obj = objectValue(parsed)
        if (!obj) return { commit: '', product: '', components: {} }
        const components: Record<string, string> = {}
        const componentsObj = objectValue(obj['components'])
        if (componentsObj) {
            for (const [name, value] of Object.entries(componentsObj)) {
                const version = stringValue(value)
                if (version) components[name] = version
            }
        }
        return {
            commit: stringValue(obj['sourceFingerprint']) || stringValue(obj['commit']),
            product: stringValue(obj['product']),
            components
        }
    } catch {
        return { commit: '', product: '', components: {} }
    }
}

/**
 * Read the build provenance (`commit` + `product`) that
 * `scripts/build-modular-binaries.ts` stamps into `cli-bin/manifest.json`. Used
 * for diagnostics only; returns empty strings when the manifest is absent.
 */
function readCliBinManifestInfo(): { commit: string; product: string } {
    const { commit, product } = readCliBinManifest()
    return { commit, product }
}

function objectValue(value: JsonValue | undefined): JsonObject | null {
    if (!value || typeof value !== 'object' || Array.isArray(value)) return null
    return value
}

function stringValue(value: JsonValue | undefined): string {
    return typeof value === 'string' ? value : ''
}

function numberValue(value: JsonValue | undefined): number {
    return typeof value === 'number' ? value : 0
}

function booleanValue(value: JsonValue | undefined): boolean {
    return typeof value === 'boolean' ? value : false
}

/**
 * Extract model names from a `nvpair-engine-manager` `list_models` action result.
 * The action returns the engine's raw response, which differs per engine:
 * Ollama's `/api/tags` yields `{ models: [{ name }] }`, LM Studio's native
 * `/api/v1/models` yields `{ models: [{ key }] }`. A present empty array is
 * authoritative; a missing or malformed inventory throws so callers retain or
 * fall back to their last-good source instead of silently clearing it.
 */
export function parseListModelNames(result: JsonValue | undefined): string[] {
    const obj = objectValue(result)
    if (!obj) throw new Error('list_models returned a non-object response')
    const names: string[] = []
    if (Array.isArray(obj.models)) {
        for (const entry of obj.models) {
            const row = objectValue(entry)
            const name = stringValue(row?.name) || stringValue(row?.key)
            if (name) names.push(name)
        }
        if (obj.models.length > 0 && names.length === 0) {
            throw new Error('list_models returned no usable model names')
        }
        return names
    }
    throw new Error('list_models response is missing its model array')
}

/**
 * Extract the per-engine loaded-in-memory model set from an
 * `engine:models-changed` push. The payload is `{ engine, models }`
 * where `models` is the full `engine:models` result carrying `loadedByEngine`
 * (engine-manager engine name -> loaded model names). Missing/omitted maps yield
 * an empty record, which stamps every local model `idle`.
 */
function parseLoadedByEngine(params: JsonValue | undefined): Record<string, string[]> {
    const models = objectValue(objectValue(params)?.models)
    const loaded = objectValue(models?.loadedByEngine)
    if (!loaded) return {}
    const out: Record<string, string[]> = {}
    for (const [engine, list] of Object.entries(loaded)) {
        if (!Array.isArray(list)) continue
        out[engine] = list.filter((name): name is string => typeof name === 'string')
    }
    return out
}

function normalizeLogLevel(value: string | undefined): ModularLogLevel {
    const lowered = value?.toLowerCase()
    if (lowered && isModularLogLevel(lowered)) return lowered
    return MODULAR_DEFAULT_LOG_LEVEL
}

/**
 * Shape the `pull_model` action params per engine. Ollama's `pull_model` body is
 * sent verbatim to `/api/pull` (reads `name`); LM Studio's CLI action templates
 * `{model}` into `lms get {model} --yes`. Sending the wrong key leaves the
 * placeholder unresolved and the engine-manager rejects the call.
 */
function pullModelParams(engineManagerEngine: string, model: string): JsonObject {
    return engineManagerEngine === 'lmstudio' ? { model } : { name: model }
}

function deleteModelParams(engineManagerEngine: string, model: string): JsonObject {
    return engineManagerEngine === 'lmstudio' ? { model } : { name: model }
}

/**
 * Distinguish "the model was not deleted" from "the model was deleted but the
 * engine did not come back".
 *
 * An action that declares `restart_after` runs the destructive part first and
 * only then bounces the engine, and `nvpair-engine-manager` reports a failed
 * bounce as a failed action:
 * `action "delete_model": engine "lmstudio" failed to restart: …`
 * (`actions.go`, `restartAfterAction`). Reporting that as "Failed to delete"
 * would be wrong twice over — the files really are gone, and a user who retries
 * on that advice just hits `model … not found on disk`.
 */
function isRestartAfterFailure(detail: string | undefined): boolean {
    return /failed to restart/i.test(detail ?? '')
}

/**
 * Inspect a `pull_model` action result for an explicit failure. The backend
 * buffers the engine's whole pull stream into one response string; Ollama's
 * `/api/pull` emits an `{"error":"…"}` NDJSON line on failure. Returns the error
 * text, or `null` when nothing in the stream looks like a failure (LM Studio's
 * CLI text has no such line, so it is treated as success and confirmed by the
 * post-pull `list_models` refresh).
 */
function pullResultError(result: JsonValue | undefined): string | null {
    if (typeof result !== 'string' || result.length === 0) return null
    for (const line of result.split('\n')) {
        const trimmed = line.trim()
        if (!trimmed || trimmed[0] !== '{') continue
        try {
            const parsed: JsonValue = JSON.parse(trimmed)
            const err = stringValue(objectValue(parsed)?.error).trim()
            if (err) return err
        } catch {
            /* non-JSON line (e.g. LM Studio CLI output) — ignore */
        }
    }
    return null
}

/**
 * Upper bound on how long we await a `pull_model` response. The backend buffers
 * the engine's entire pull stream and only returns it when the download
 * finishes, so this must comfortably exceed a large multi-GB download on a slow
 * link without being unbounded (a hung pull must eventually clear its spinner).
 */
const PULL_TIMEOUT_MS = 6 * 60 * 60 * 1000
const DISCOVERY_MODEL_RETRY_MIN_MS = 1_000
const DISCOVERY_MODEL_RETRY_MAX_MS = 30_000

/**
 * Cadence for re-polling clustered peers' authoritative engine status via
 * `engine:remote-get-installed`. The mDNS proxy presence is real-time but only
 * ever says up/down; a peer's installed/not-installed/port + a stopped engine's
 * configured port are only knowable from its `ec` surface, which is on-request
 * only (no cross-cluster push yet). We
 * poll on a light interval (plus on membership change and after each remote op)
 * so a peer's engine state converges without a proactive backend signal.
 */
const REMOTE_STATUS_POLL_MS = 20_000

type ModularRuntimeBinaryDefinition = (typeof MODULAR_RUNTIME_BINARIES)[number]

/** Per-engine local-node → proxy bridge state (see reconcileLocalNodeBridge). */
interface LocalEngineBridge {
    running: boolean
    port: number
    bridgedId: string
    bridgedPort: number
    selfWarned: boolean
}

function emptyLocalEngineBridge(): LocalEngineBridge {
    return { running: false, port: 0, bridgedId: '', bridgedPort: 0, selfWarned: false }
}

/** Translate our `EngineType` into the engine-manager's engine id. */
function engineManagerId(engine: ProxyEngine): string {
    return engine === 'lm-studio' ? 'lmstudio' : engine
}

/** Translate an engine-manager engine id into a proxy engine, or null. */
function proxyEngineFromManagerId(id: string): ProxyEngine | null {
    if (id === 'ollama') return 'ollama'
    if (id === 'lmstudio') return 'lm-studio'
    return null
}

/** The broker relay namespace fronting an engine's reverse proxy. */
function proxyRelayPrefix(engine: ProxyEngine): string {
    return engine === 'ollama' ? 'proxy' : 'lmstudio-proxy'
}

/**
 * Spawns and supervises the modular backend.
 *
 * Ownership is hybrid (see `MODULAR_RUNTIME_BINARIES` and
 * `docs/services-backend.md`):
 *
 * - The `nvpair-ui-broker` is the **only** Electron-spawned binary and is itself the
 *   parent of every broker-owned worker (`ollama-proxy`, `lmstudio-proxy`,
 *   `nvpair-node-scanner`, `nvpair-node-info`, `nvpair-workload-manager`,
 *   `nvpair-cluster-manager`, `nvpair-node-settings`, `nvpair-manual-nodes`,
 *   `nvpair-engine-manager`, `nvpair-errors`, `nvpair-job-scheduler`). Electron passes their resolved paths to
 *   the broker (see `brokerStartupArgs`) and reaches each through a broker relay:
 *   `proxy:` / `lmstudio-proxy:` for the two engine proxies, `engine:` for the
 *   engine-manager, `errors:` for the error pipeline, `node/*` for manual nodes,
 *   `settings/*` and `cluster:` for the rest. Local inference jobs arrive on the
 *   broker's `workloads:subscribe` stream.
 * - The local `/v1/node-info` HTTP poll is the single sanctioned HTTP exception
 *   for telemetry (see `node-info-poller.ts`).
 */
class ModularSupervisor {
    private processes = new Map<ModularProcessName, JsonRpcSubprocess>()
    private readinessWaiters = new Map<number, ReadinessWaiter>()
    private nextReadinessWaiterId = 0
    private readinessReported = false
    // Per-engine local-node → proxy bridge state (the #5 "no active node" fix).
    // Desired state is sourced from each engine's local engine:state-changed; the
    // bridged* fields track what we have actually pushed into the proxy so
    // reconcile is idempotent and re-bridges after a proxy restart.
    private localBridges = new Map<ProxyEngine, LocalEngineBridge>()
    // Engines whose cached [] was written only because the engine stopped.
    // A failed first refresh after restart may release this sentinel to
    // discovery; failures after a successful inventory must retain that
    // last-known-good result (including an authoritative empty list).
    private stoppedModelSentinels = new Set<EngineType>()
    private stoppedModelEngines = new Set<EngineType>()
    private successfulModelInventories = new Set<EngineType>()
    private modelRefreshGenerations = new Map<EngineType, number>()
    private discoveryModelRetryTimers = new Map<ProxyEngine, ReturnType<typeof setTimeout>>()
    private discoveryModelRetryAttempts = new Map<ProxyEngine, number>()
    // Authoritative value is the persisted ui-config `modularLogLevel`; the
    // connector seeds it via setLogLevel() before start() so spawn args use it.
    // This default only applies if start() runs before the connector seeds.
    private logLevel: ModularLogLevel = MODULAR_DEFAULT_LOG_LEVEL
    private brokerReady = false
    private isReady = false
    // True only while stop() is intentionally tearing down the subprocess tree.
    // Used to tell a deliberate broker shutdown (SIGKILL / stdin close) apart
    // from an unexpected broker crash so we don't fire the crash callback on a
    // normal stop/restart.
    private shuttingDown = false
    // Invoked when the broker process exits unexpectedly (not via stop()). The
    // connector registers this to flip status and prompt the user — the
    // supervisor never imports the connector, keeping the dependency one-way.
    private onBrokerCrash?: (info: { code: number | null }) => void
    private onReady?: () => void
    // Set when an invite lazily created the cluster (the cluster-manager refuses
    // to invite while unclustered). Tracks that the cluster exists *only* because
    // of an in-flight pairing, so a failed pairing can dissolve the orphaned solo
    // cluster instead of stranding this node alone in an empty cluster. Cleared
    // the moment a real peer joins (`nodes:changed`) or the cluster is dissolved.
    private autoCreatedSoloForInvite = false
    // One-shot guard for the first-app-open engine bootstrap: on the very first
    // launch we start every already-installed local engine (which also persists
    // the backend's desired-enabled state). In-memory only, so it dedupes across
    // broker restarts within one app process without adding a second auto-start
    // source of truth (the backend still owns lifecycle persistence).
    private firstOpenEngineStartDone = false
    // Pinned cluster peer UUIDs (seeded from `nodes:get-initial` at broker ready,
    // then kept current by the `nodes:changed` membership push, self excluded).
    // These are the only nodes whose `ec` control surface we can reach, so they
    // scope the remote-engine-status polling. Keyed by `ClusterNode.nodeUuid` —
    // the backend keys peers by hostUuid, and it is the same key
    // discovery/`engine:remote-*` use, never the hostname.
    private clusterPeerIds = new Set<string>()
    // Light interval that re-polls clusterPeerIds' authoritative engine status.
    private remoteStatusTimer: ReturnType<typeof setInterval> | null = null
    // Debounced follow-up poll when discovery:nodes-changed lands — the ec peer
    // directory in engine-manager is fed from that relay and may not be ready for
    // the first broker-ready sweep.
    private remoteStatusDebounceTimer: ReturnType<typeof setTimeout> | null = null
    // False until {@link onBrokerReady} finishes its first hydration pass. Proxy
    // ready notifications during startup must not trigger a renderer store refresh
    // while remote peer facts are still unknown.
    private brokerHydrationDone = false
    // Light interval that reconciles main's authoritative pending-invite set by
    // polling each invite's `cluster:invite-status` (pruning terminal or evicted
    // ones). The cluster-manager now pushes receiver-side teardown directly
    // (`cluster:invite-canceled` on cancel, `cluster:invite-expired` on TTL — both
    // handled in handleClusterManagerNotification), so this sweep is the backstop
    // that clears a lingering invite if a push is missed (e.g. across a restart).
    private pendingInviteSweepTimer: ReturnType<typeof setInterval> | null = null

    get ready(): boolean {
        return this.isReady && this.brokerReady
    }

    /** Register a handler invoked when the broker exits unexpectedly. */
    setOnBrokerCrash(cb: (info: { code: number | null }) => void): void {
        this.onBrokerCrash = cb
    }

    /** Register a handler invoked whenever the broker reports app:ready. */
    setOnReady(cb: () => void): void {
        this.onReady = cb
        this.updateReadiness()
    }

    /**
     * Wait for the broker's app:ready notification.
     * Process spawn alone is insufficient: a live broker can stall before its
     * workers are usable, which previously left Overview loading indefinitely.
     * Broker-owned proxies are optional capabilities and report readiness
     * asynchronously through the bridge state.
     */
    waitUntilReady(timeoutMs: number): Promise<void> {
        if (this.ready) return Promise.resolve()
        if (!this.isReady) {
            return Promise.reject(new Error(`${APP_DISPLAY_NAME} service is not running`))
        }

        return new Promise((resolve, reject) => {
            const id = ++this.nextReadinessWaiterId
            const timeout = setTimeout(() => {
                const waiter = this.readinessWaiters.get(id)
                if (!waiter) return
                this.readinessWaiters.delete(id)
                waiter.reject(new ModularStartupTimeoutError(timeoutMs))
            }, timeoutMs)
            this.readinessWaiters.set(id, { resolve, reject, timeout })
            this.updateReadiness()
        })
    }

    /**
     * Record that the most recent invite lazily created the cluster.
     * Unused since invite founding moved to cluster-manager; kept for the
     * legacy abandon-if-solo / nodes:changed guard until that path is removed.
     */
    markAutoCreatedSoloForInvite(): void {
        this.autoCreatedSoloForInvite = true
    }

    /** True while the cluster exists solely to back an in-flight pairing. */
    isAutoCreatedSoloForInvite(): boolean {
        return this.autoCreatedSoloForInvite
    }

    /** Forget the auto-created-solo marker (a peer joined, or we dissolved). */
    clearAutoCreatedSoloForInvite(): void {
        this.autoCreatedSoloForInvite = false
    }

    start(): void {
        if (this.isReady) return

        this.shuttingDown = false
        this.brokerReady = false
        this.brokerHydrationDone = false
        this.readinessReported = false

        // Clear any workloads carried over from a previous run so stale entries
        // from before a restart/crash never linger. `onBrokerReady` then re-seeds
        // the authoritative set from the broker's durable `workloads:get-initial`
        // baseline (and the live stream continues from there), so this is a clean
        // slate rather than a permanent loss. This is the single chokepoint every
        // start path funnels through (manual restart, stop→start, crash→start, CLI).
        getModularBridgeState().clearWorkloads()

        const binDir = getCliBinDir()
        const manifestInfo = readCliBinManifestInfo()
        log.info({
            sublevel: 'lifecycle',
            message: 'Resolving modular service binaries',
            data: {
                mode: app.isPackaged ? 'packaged' : 'dev',
                binDir,
                modularCommit: manifestInfo.commit,
                modularProduct: manifestInfo.product,
                resourcesPath: process.resourcesPath,
                appPath: app.getAppPath()
            }
        })

        this.validateRequiredBinaries()

        for (const definition of this.electronOwnedDefinitions()) {
            const binaryPath = getModularBinaryPath(definition.baseName)
            if (!fs.existsSync(binaryPath)) {
                // Required binaries were validated above; only optional ones can
                // reach here, so skip them rather than throwing.
                log.warn({
                    sublevel: definition.processName,
                    message: 'Optional modular binary not present; skipping',
                    data: { binaryPath }
                })
                continue
            }

            log.info({
                sublevel: definition.processName,
                message: 'Starting modular subprocess',
                data: { binaryPath }
            })

            const child = new JsonRpcSubprocess(definition.processName, binaryPath)
            this.processes.set(definition.processName, child)
            this.attachChildHandlers(child)
            try {
                child.start(this.startupArgs(definition))
            } catch (error) {
                if (this.processes.get(definition.processName) === child) {
                    this.processes.delete(definition.processName)
                }
                throw error
            }
        }

        getModularBridgeState().setLocalDiscoveryModelRefresher(engine =>
            this.refreshDiscoveryEngineModels(engine)
        )

        this.isReady = true
        this.updateReadiness()
        startNodeInfoPoller()
        log.info({ sublevel: 'lifecycle', message: 'Started modular service processes' })
    }

    async stop(): Promise<void> {
        // Mark teardown first so the broker's exit during stop() is treated as a
        // deliberate shutdown, not a crash.
        this.shuttingDown = true
        stopNodeInfoPoller()
        if (this.remoteStatusDebounceTimer) {
            clearTimeout(this.remoteStatusDebounceTimer)
            this.remoteStatusDebounceTimer = null
        }
        if (this.remoteStatusTimer) {
            clearInterval(this.remoteStatusTimer)
            this.remoteStatusTimer = null
        }
        if (this.pendingInviteSweepTimer) {
            clearInterval(this.pendingInviteSweepTimer)
            this.pendingInviteSweepTimer = null
        }
        for (const timer of this.discoveryModelRetryTimers.values()) clearTimeout(timer)
        this.discoveryModelRetryTimers.clear()
        this.discoveryModelRetryAttempts.clear()
        this.clusterPeerIds.clear()
        this.brokerHydrationDone = false

        // Teardown is timed phase by phase because a slow quit is otherwise
        // indistinguishable from a blocked one: the process tree logs through this
        // main process, so if it stalls here the backend's own shutdown lines never
        // reach the log and the gap looks like a backend hang. These lines bound
        // where the time actually went.
        const teardownStartedAt = Date.now()

        // Stop running engines first, while engine-manager is fully alive and not
        // under a shutdown deadline, so its child engine processes (e.g. Ollama)
        // are gone before the teardown loop below — never orphaned by a SIGKILL.
        await this.stopLocalEnginesForShutdown()
        log.info({
            sublevel: 'lifecycle',
            message: `Stopped local engines for shutdown in ${Date.now() - teardownStartedAt}ms`
        })

        const processes = Array.from(this.processes.values()).reverse()
        this.processes.clear()
        this.localBridges.clear()
        this.brokerReady = false
        this.brokerHydrationDone = false
        this.isReady = false
        this.readinessReported = false
        this.rejectReadinessWaiters(
            new Error(`${APP_DISPLAY_NAME} service stopped before becoming ready`)
        )

        for (const child of processes) {
            try {
                // The broker is the parent of the whole worker tree and runs each
                // worker's graceful shutdown on its own exit — notably
                // engine-manager's StopAll() (stops engines it launched). Give it a
                // longer grace so a slow engine shutdown isn't cut short by SIGKILL
                // (which would orphan the engine).
                await child.stop(child.name === 'broker' ? 15_000 : undefined)
            } catch (err) {
                log.warn({
                    sublevel: child.name,
                    message: `Failed to stop ${child.name}: ${getErrorString(err)}`
                })
            }
        }

        log.info({
            sublevel: 'lifecycle',
            message: `Stopped modular service processes in ${Date.now() - teardownStartedAt}ms`
        })
    }

    /** Stop engine processes before teardown without changing saved ON/OFF intent. */
    private async stopLocalEnginesForShutdown(): Promise<void> {
        const broker = this.processes.get('broker')
        if (!broker) return
        try {
            await prepareLocalEnginesForShutdown(broker)
        } catch (err) {
            log.warn({
                sublevel: 'engine-manager',
                message: `Could not stop engines for shutdown: ${getErrorString(err)}`
            })
        }
    }

    hasProcess(name: ModularProcessName): boolean {
        return this.processes.has(name)
    }

    callProcess(
        name: ModularProcessName,
        method: string,
        params?: Parameters<JsonRpcSubprocess['call']>[1],
        timeoutMs?: number
    ): Promise<Awaited<ReturnType<JsonRpcSubprocess['call']>>> {
        const child = this.processes.get(name)
        // Reject rather than throw synchronously: callers on the notification
        // dispatch path (e.g. refreshManagedEngineModels) chain .catch() and
        // expect a rejected promise. A synchronous throw would escape that
        // .catch() and surface as an uncaught main-process exception when a late
        // notification arrives after the process map is cleared during stop().
        if (!child) return Promise.reject(new Error(`${name} is not running`))
        return child.call(method, params, timeoutMs)
    }

    /**
     * Dispatch a long-running request without imposing the normal short timeout.
     * Outcome still arrives through push events. `observeResponse` additionally
     * catches an eventual JSON-RPC rejection without timing out the operation.
     *
     * `onFailure` clears optimistic UI state when the process is unavailable, a
     * write fails, or an observed backend response rejects the request.
     */
    sendProcess(
        name: ModularProcessName,
        method: string,
        params?: JsonValue,
        onFailure?: (error: string, responseRejected: boolean) => void,
        observeResponse = false
    ): void {
        const child = this.processes.get(name)
        if (!child) {
            const error = `${name} is not running`
            log.warn({ sublevel: name, message: `cannot send ${method}: ${error}` })
            onFailure?.(error, false)
            return
        }
        const request = observeResponse
            ? child.sendWithResponse(method, params)
            : child.send(method, params)
        void request.catch(err => {
            const error = getErrorString(err) ?? `failed to send ${method}`
            log.warn({
                sublevel: name,
                message: `failed to send ${method}: ${error}`
            })
            onFailure?.(error, err instanceof JsonRpcResponseError)
        })
    }

    /** Reach an engine's reverse proxy through the broker's relay namespace. */
    callProxy(
        engine: ProxyEngine,
        method: string,
        params?: JsonValue
    ): Promise<JsonValue | undefined> {
        return this.callProcess('broker', `${proxyRelayPrefix(engine)}:${method}`, params)
    }

    /**
     * Record a locally-synthesized error. The broker owns the error pipeline
     * (`nvpair-errors`): forward via its `errors:report` relay (a notification, like
     * a worker emits) and the authoritative state lands back on `errors:update`.
     * Before the broker is ready we keep it in the in-memory store so the UI still
     * shows it.
     */
    reportError(
        message: string,
        severity: ServiceErrorSeverity = 'error',
        idSuffix?: string,
        context?: Partial<
            Pick<
                ServiceError,
                'id' | 'nodeId' | 'engineType' | 'operation' | 'action' | 'modelName'
            >
        >
    ): void {
        const id = context?.id ?? `pair-ui:${idSuffix ?? Date.now()}`
        // nvpair-errors keys (and clears) local-origin errors by the local node id
        // (errKey{localNodeID, id}). A report with an empty nodeId lands under
        // ("", id) and can never be matched by errors:clear, so it sticks
        // forever. Stamp our resolved selfId so clears and node-scoped UIs work.
        const nodeId = context?.nodeId ?? getModularBridgeState().getSelfId() ?? ''
        const broker = this.processes.get('broker')
        if (broker && this.brokerReady) {
            const params: JsonObject = { id, message, timestamp: Date.now(), severity, nodeId }
            if (context?.engineType) params.engineType = context.engineType
            if (context?.operation) params.operation = context.operation
            if (context?.action) params.action = context.action
            if (context?.modelName) params.modelName = context.modelName
            void broker.notify('errors:report', params)
            return
        }
        const error: ServiceError = { id, message, timestamp: Date.now(), severity, nodeId }
        if (context?.engineType) error.engineType = context.engineType
        if (context?.operation) error.operation = context.operation
        if (context?.action) error.action = context.action
        if (context?.modelName) error.modelName = context.modelName
        getModularBridgeState().upsertError(error)
    }

    setLogLevel(level: string): void {
        const nextLevel = normalizeLogLevel(level)
        this.logLevel = nextLevel

        for (const child of this.processes.values()) {
            void child.notify('log/set-level', { level: nextLevel })
        }
    }

    getLogLevel(): string {
        return this.logLevel
    }

    private validateRequiredBinaries(): void {
        for (const definition of MODULAR_RUNTIME_BINARIES) {
            if (definition.optional) continue
            const binaryPath = getModularBinaryPath(definition.baseName)
            if (!fs.existsSync(binaryPath)) {
                throw new Error(
                    `Required modular binary not found: ${binaryPath}. Run npm run build:modular-binaries to populate cli-bin.`
                )
            }
        }
    }

    private electronOwnedDefinitions(): ModularRuntimeBinaryDefinition[] {
        return MODULAR_RUNTIME_BINARIES.filter(definition => definition.launchOwner === 'electron')
    }

    private attachChildHandlers(child: JsonRpcSubprocess): void {
        const isCurrent = (): boolean =>
            this.processes.get(child.name as ModularProcessName) === child

        child.on('notification', notification => {
            if (isCurrent()) this.handleNotification(notification)
        })
        child.on('request', request => {
            if (isCurrent()) this.handleChildRequest(request)
        })
        child.on('log', entry => {
            if (!isCurrent()) return
            getModularBridgeState().appendLog(entry.source, entry.stream, entry.text)
            log[serviceLogLevel(entry.stream, entry.text)]({
                sublevel: entry.source,
                message: entry.text,
                data: { stream: entry.stream }
            })
        })
        child.on('exit', event => {
            if (!isCurrent()) return
            this.processes.delete(child.name as ModularProcessName)
            if (child.name === 'broker') {
                this.brokerReady = false
                this.brokerHydrationDone = false
                this.isReady = false
                this.readinessReported = false
                this.rejectReadinessWaiters(
                    new Error(`${APP_DISPLAY_NAME} service exited before becoming ready`)
                )
            }
            log.warn({
                sublevel: event.source,
                message: `${event.source} exited`,
                data: { code: event.code }
            })
            // An unexpected broker exit (not triggered by our own stop()) takes
            // the whole broker-owned worker tree down with it. Notify the
            // connector so it can flip status and prompt the user to restart.
            if (child.name === 'broker' && !this.shuttingDown) {
                this.onBrokerCrash?.({ code: event.code })
            }
        })
    }

    private logLevelArgs(): string[] {
        return ['--log-level', this.logLevel]
    }

    private binaryPathFor(name: ModularProcessName): string {
        const definition = MODULAR_RUNTIME_BINARIES.find(binary => binary.processName === name)
        if (!definition) throw new Error(`Unknown modular binary: ${name}`)
        return getModularBinaryPath(definition.baseName)
    }

    private startupArgs(definition: ModularRuntimeBinaryDefinition): string[] {
        if (definition.processName === 'broker') {
            return this.brokerStartupArgs()
        }
        return [...definition.args, ...this.logLevelArgs()]
    }

    private brokerStartupArgs(): string[] {
        const args: string[] = []
        const passPath = (flag: string, name: ModularProcessName): void => {
            const binaryPath = this.binaryPathFor(name)
            if (fs.existsSync(binaryPath)) args.push(flag, binaryPath)
        }
        passPath('--scanner-path', 'scanner')
        passPath('--node-info-path', 'node-info')
        passPath('--proxy-path', 'proxy')
        passPath('--lmstudio-proxy-path', 'lmstudio-proxy')
        passPath('--workload-manager-path', 'workload-manager')
        passPath('--cluster-manager-path', 'cluster-manager')
        passPath('--settings-path', 'node-settings')
        passPath('--manual-nodes-path', 'manual-nodes')
        passPath('--engine-manager-path', 'engine-manager')
        // The broker spawns nvpair-errors with --peer-sync itself; pass only the path.
        passPath('--errors-path', 'errors')
        // The broker supervises the scheduler, fans it the discovery + workload
        // streams, and fans its schedule:priority out to the proxies via
        // node/set-priority (all broker-internal).
        passPath('--scheduler-path', 'job-scheduler')
        return [...args, ...this.logLevelArgs()]
    }

    /**
     * Re-add persisted manual nodes through the broker's `node/add` relay. The
     * broker's `nvpair-manual-nodes` loses its in-memory entries on restart (N86),
     * so PAIR UI owns the durable list (`manual-nodes.json`) and replays it once
     * the broker is ready.
     */
    private async replayManualNodes(): Promise<void> {
        const entries = listManualNodeEntries()
        for (const entry of entries) {
            try {
                const params: JsonObject = { address: entry.address, name: entry.name }
                const wirePorts = manualPortsToWire(entry.ports)
                if (wirePorts) params.ports = wirePorts
                await this.callProcess('broker', 'node/add', params)
            } catch (err) {
                log.warn({
                    sublevel: 'manual-nodes',
                    message: `Failed to replay manual node ${entry.address}: ${getErrorString(err)}`
                })
            }
        }
    }

    private async onBrokerReady(): Promise<void> {
        const subscribe = async (method: string, label: string): Promise<void> => {
            try {
                await this.callProcess('broker', method)
            } catch (err) {
                log.warn({
                    sublevel: 'broker',
                    message: `Unable to ${label}: ${getErrorString(err)}`
                })
            }
        }
        await subscribe('discovery:subscribe', 'subscribe to broker discovery')
        await subscribe('proxy:subscribe', 'subscribe to broker ollama-proxy relay')
        await subscribe('lmstudio-proxy:subscribe', 'subscribe to broker lmstudio-proxy relay')
        // Engine events are opt-in and replay no baseline — subscribe then hydrate.
        await subscribe('engine:subscribe', 'subscribe to broker engine relay')
        await subscribe('workloads:subscribe', 'subscribe to broker workloads stream')
        await this.seedWorkloadBaseline()

        await this.syncClusterIdentityToManager()
        await this.resolveSelfId()
        await this.replayManualNodes()
        await this.hydrateEngineManager()
        this.startInstalledEnginesOnFirstOpen()
        await this.seedClusterPeerIds()
        await this.refreshAllRemoteEngineStatus()
        this.startRemoteStatusPolling()
        for (const engine of PROXY_ENGINES) {
            await this.hydrateProxyNodes(engine)
            await this.pollProxyStatus(engine)
        }
        // Discovery relay + proxy overlays can land after the first sweep; poll
        // again so clustered peers converge before the renderer refresh fires.
        await this.refreshAllRemoteEngineStatus()
        this.brokerHydrationDone = true
        this.startPendingInviteSweep()
    }

    /** Renderer store refresh — suppressed during the initial broker hydration pass. */
    private emitStateRefreshIfHydrated(): void {
        if (!this.brokerHydrationDone) return
        emitBridgePush('state:request-refresh', undefined)
    }

    /**
     * Begin reconciling main's authoritative pending-invite set. Idempotent: a
     * re-ready (broker restart) clears any prior timer first. The cluster-manager
     * pushes receiver-side teardown directly (`cluster:invite-canceled` on cancel,
     * `cluster:invite-expired` on TTL — both handled in
     * handleClusterManagerNotification), so this poll is the backstop that drops a
     * resolved / evicted / expired invite when a push is missed (e.g. across a
     * broker restart).
     */
    private startPendingInviteSweep(): void {
        if (this.pendingInviteSweepTimer) clearInterval(this.pendingInviteSweepTimer)
        this.pendingInviteSweepTimer = setInterval(() => {
            void this.sweepPendingInvites()
        }, MODULAR_INVITE_STATUS_POLL_INTERVAL_MS)
    }

    /**
     * One reconciliation pass over the pending-invite set: poll each invite's
     * backend status and prune the ones that resolved (`paired` / `declined` /
     * `expired` / `failed`) or whose session was evicted (unknown inviteId,
     * `-32001`). A transient poll failure is ignored — the next sweep retries.
     */
    private async sweepPendingInvites(): Promise<void> {
        if (!this.brokerReady) return
        const state = getModularBridgeState()
        for (const invite of state.getPendingInvites()) {
            try {
                const status = parseInvite(
                    await this.callProcess('broker', 'cluster:invite-status', {
                        inviteId: invite.inviteId
                    })
                )
                if (status.state !== 'pending') state.prunePendingInvite(invite.inviteId)
            } catch (err) {
                // `-32001` (unknown inviteId) means the cluster-manager evicted the
                // session (terminal or process restart): the invite can never be
                // answered, so drop it. Other errors are transient — leave it for
                // the next sweep.
                if (err instanceof JsonRpcResponseError && err.message.startsWith('-32001')) {
                    state.prunePendingInvite(invite.inviteId)
                }
            }
        }
    }

    /**
     * Record the pinned cluster peers whose `ec` surface remote-engine-status
     * polling may reach. Confirmed `member`s only — a membership snapshot also
     * carries pending invites, which have no reachable control surface yet.
     */
    private setClusterPeerIds(members: ClusterNode[]): void {
        const selfId = getModularBridgeState().getSelfId()
        this.clusterPeerIds = new Set(
            members
                .filter(m => m.state === 'member' && m.nodeUuid && m.nodeUuid !== selfId)
                .map(m => m.nodeUuid)
        )
    }

    /**
     * Seed the peer set from the current membership before the first status
     * sweep. `nodes:changed` fires only on a membership *change*, so without
     * this read a restart with unchanged membership would leave the set empty
     * and never poll any peer's authoritative install state — leaving every
     * peer engine inferred from mDNS presence alone.
     */
    private async seedClusterPeerIds(): Promise<void> {
        try {
            const members = parseClusterNodes(await this.callProcess('broker', 'nodes:get-initial'))
            this.setClusterPeerIds(members)
            log.info({
                sublevel: 'broker',
                message: `cluster peer seed: ${this.clusterPeerIds.size} remote member(s)`,
                data: { peerIds: Array.from(this.clusterPeerIds) }
            })
        } catch (err) {
            log.warn({
                sublevel: 'broker',
                message: `cluster peer seed skipped: ${getErrorString(err)}`
            })
        }
    }

    /**
     * Begin polling clustered peers' authoritative engine status. Idempotent:
     * a re-ready (broker restart) clears any prior timer first. Also runs one
     * immediate sweep so a peer's state converges without waiting a full period.
     */
    private startRemoteStatusPolling(): void {
        if (this.remoteStatusTimer) clearInterval(this.remoteStatusTimer)
        this.remoteStatusTimer = setInterval(() => {
            void this.refreshAllRemoteEngineStatus()
        }, REMOTE_STATUS_POLL_MS)
        void this.refreshAllRemoteEngineStatus()
    }

    /**
     * Poll one clustered peer's authoritative engine status
     * (`engine:remote-get-installed`, mTLS `ec`) and feed the real
     * installed/running/port into the bridge state. A failure is expected and
     * non-fatal for an unclustered / unpinned / unreachable peer.
     *
     * Cached facts are deliberately **kept** on failure. They are the only source
     * that can tell a peer's `not-installed` from `stopped`, and a transient poll
     * failure is not evidence that either changed — discarding them would flip a
     * correct "Install" back into an installed-but-off toggle. Node reachability
     * is already surfaced separately, so a stale snapshot is presented as a
     * disconnected node rather than as a different engine state.
     */
    async refreshRemoteEngineStatus(nodeId: string): Promise<void> {
        if (!this.brokerReady) return
        const selfId = getModularBridgeState().getSelfId()
        if (selfId && nodeId === selfId) return
        try {
            const result = await this.callProcess('broker', 'engine:remote-get-installed', {
                node: nodeId
            })
            getModularBridgeState().applyRemoteEngineFacts(nodeId, result)
            const engines = objectValue(result)?.engines
            log.info({
                sublevel: 'engine-manager',
                message: `remote engine status polled for ${nodeId}`,
                data: { engines: Array.isArray(engines) ? engines : [] }
            })
        } catch (err) {
            log.warn({
                sublevel: 'engine-manager',
                message: `remote engine status for ${nodeId} unavailable: ${getErrorString(err)}`
            })
        }
    }

    /** Poll every pinned cluster peer's authoritative engine status. */
    private async refreshAllRemoteEngineStatus(): Promise<void> {
        if (!this.brokerReady) return
        const selfId = getModularBridgeState().getSelfId()
        const peerIds = new Set(this.clusterPeerIds)
        for (const node of getModularBridgeState().getAvailableNodes()) {
            if (node.id && node.id !== selfId) peerIds.add(node.id)
        }
        if (peerIds.size === 0) return
        log.info({
            sublevel: 'engine-manager',
            message: `remote engine status sweep: ${peerIds.size} peer(s)`,
            data: { peerIds: Array.from(peerIds) }
        })
        await Promise.allSettled(Array.from(peerIds).map(id => this.refreshRemoteEngineStatus(id)))
    }

    /**
     * Debounce a remote status sweep after discovery:nodes-changed. Engine-manager
     * folds that relay into its ec peer directory; polling immediately on broker
     * ready can race the first snapshot and leave peers stuck without facts.
     */
    private scheduleRemoteEngineStatusRefresh(): void {
        if (this.remoteStatusDebounceTimer) clearTimeout(this.remoteStatusDebounceTimer)
        this.remoteStatusDebounceTimer = setTimeout(() => {
            this.remoteStatusDebounceTimer = null
            void this.refreshAllRemoteEngineStatus()
        }, 500)
    }

    private async pollProxyStatus(engine: ProxyEngine): Promise<void> {
        try {
            const result = await this.callProcess(
                'broker',
                `${proxyRelayPrefix(engine)}:get-status`
            )
            const obj = objectValue(result)
            if (obj && booleanValue(obj.ready)) {
                getModularBridgeState().handleNotification({
                    source: engine === 'ollama' ? 'proxy' : 'lmstudio-proxy',
                    method: 'ready',
                    params: { port: numberValue(obj.port) }
                })
                this.emitStateRefreshIfHydrated()
            }
        } catch (err) {
            log.verbose({
                sublevel: 'broker',
                message: `Unable to read ${engine} proxy status: ${getErrorString(err)}`
            })
        }
    }

    private async hydrateProxyNodes(engine: ProxyEngine): Promise<void> {
        try {
            const result = await this.callProxy(engine, 'nodes/list')
            const obj = objectValue(result)
            if (!obj || !Array.isArray(obj.nodes)) return
            for (const node of obj.nodes) {
                getModularBridgeState().handleNotification({
                    source: engine === 'ollama' ? 'proxy' : 'lmstudio-proxy',
                    method: 'node/discovered',
                    params: node
                })
            }
        } catch (err) {
            log.verbose({
                sublevel: proxyRelayPrefix(engine),
                message: `Unable to hydrate ${engine} proxy nodes: ${getErrorString(err)}`
            })
        }
    }

    private async hydrateEngineManager(): Promise<void> {
        if (!this.processes.has('broker')) return
        try {
            const result = await this.callProcess('broker', 'engine:get-installed')
            const obj = objectValue(result)
            if (!obj || !Array.isArray(obj.engines)) return
            for (const engine of obj.engines) {
                getModularBridgeState().applyEngineManagerStatus(engine)
                this.refreshManagedEngineModels(engine)
            }
        } catch (err) {
            log.verbose({
                sublevel: 'engine-manager',
                message: `Unable to hydrate engine-manager state: ${getErrorString(err)}`
            })
        }
    }

    /**
     * On the very first app open, start every already-installed local engine.
     * The backend persists desired-enabled as a side effect of start, so this is
     * the "installed before PAIR" ease-of-use bootstrap — PAIR keeps no auto-start
     * list of its own. Runs at most once per app process (the in-memory guard) and
     * never completes first-run itself (the renderer's Welcome flow owns that).
     * Starting an already-running engine is a no-op in the backend, so re-issuing
     * start is safe.
     */
    private startInstalledEnginesOnFirstOpen(): void {
        if (this.firstOpenEngineStartDone || !isFirstRun()) return

        const state = getModularBridgeState()
        const selfId = state.getSelfId()
        // Self not resolved yet — leave the guard unset so a later broker-ready
        // (e.g. after a restart) can still run the first-open bootstrap.
        if (!selfId) return
        this.firstOpenEngineStartDone = true

        for (const status of state.getEngineInitialState().statuses) {
            if (status.nodeId !== selfId) continue
            if (status.processStatus !== 'stopped' && status.processStatus !== 'running') continue

            const engine = engineManagerEngineName(status.engineType)
            // A stopped engine will emit `engine:state-changed` on start, which
            // clears this optimistic op; a running engine's start is a backend
            // no-op (no state event), so we skip the op to avoid a stuck spinner —
            // it is already shown running and we only re-issue start to persist
            // the backend's desired-enabled flag.
            if (status.processStatus === 'stopped') {
                state.beginLocalEngineOp(status.engineType, 'starting')
            }
            this.sendProcess(
                'broker',
                'engine:start',
                { engine },
                error => {
                    state.clearPendingEngineOp(status.engineType)
                    this.reportError(
                        `Failed to start ${engine} on first open: ${error}`,
                        'error',
                        `engine-cmd:first-open-start:${engine}`
                    )
                },
                true
            )
        }
    }

    private childByName(name: string): JsonRpcSubprocess | undefined {
        for (const child of this.processes.values()) {
            if (child.name === name) return child
        }
        return undefined
    }

    private handleChildRequest(request: JsonRpcInboundRequest): void {
        const child = this.childByName(request.source)
        if (!child) return

        void child.respondError(request.id, -32601, `method not found: ${request.method}`)
    }

    private handleNotification(notification: JsonRpcNotification): void {
        log.verbose({
            sublevel: notification.source,
            message: notification.method,
            data:
                notification.params && typeof notification.params === 'object'
                    ? notification.params
                    : {}
        })

        // Service-error pipeline: the broker forwards every worker's error into
        // nvpair-errors and relays the authoritative snapshot back as errors:update.
        if (notification.method === 'errors:update') {
            this.handleErrorsNotification(notification)
            return
        }

        // Engine control plane: the broker relays nvpair-engine-manager's opt-in
        // engine:* events verbatim (engine:ready / state-changed / models-changed /
        // install-progress / pull-progress / remote-progress).
        if (notification.method.startsWith('engine:')) {
            this.handleEngineManagerNotification(notification)
            return
        }

        // Workload stream: the broker relays per-request proxy lifecycle events
        // (and peer workloads from the workload-manager it supervises) as
        // workloads:upsert / workloads:remove once we workloads:subscribe.
        if (notification.method === 'workloads:upsert') {
            getModularBridgeState().upsertWorkloadFromInfo(notification.params)
            return
        }
        if (notification.method === 'workloads:remove') {
            getModularBridgeState().removeWorkloadFromParams(notification.params)
            return
        }

        // Cluster-manager pushes, relayed verbatim by the broker. Low-volume
        // pairing / membership events — always fan out to the renderer.
        if (
            notification.method === 'cluster:invite-received' ||
            notification.method === 'cluster:invite-canceled' ||
            notification.method === 'cluster:invite-expired' ||
            notification.method === 'cluster:invite-failed' ||
            notification.method === 'cluster:identity-changed' ||
            notification.method === 'nodes:changed'
        ) {
            this.handleClusterManagerNotification(notification)
            return
        }

        // nvpair-node-settings is broker-supervised; the broker relays its
        // connection/cluster-identity push verbatim (source 'broker'). It carries
        // only the id, so onClusterIdentityChanged re-reads the persisted friendly
        // name (via the broker settings relay) and pushes the identity to the UI.
        if (notification.method === 'connection/cluster-identity') {
            const id = stringValue(objectValue(notification.params)?.id)
            void this.onClusterIdentityChanged(id)
            return
        }

        const event = this.normalizeBrokerProxy(notification)
        // Apply to bridge state first so the side-effects below (reconcile,
        // refresh) observe the fresh proxy port / node set this event carries —
        // a `<proxy>:ready` updates the bound proxy port, which the self-forward
        // guard in reconcileLocalNodeBridge depends on.
        getModularBridgeState().handleNotification(event)

        if (event.source === 'broker' && event.method === 'discovery:nodes-changed') {
            this.scheduleRemoteEngineStatusRefresh()
        }

        const proxyEngine: ProxyEngine | null =
            event.source === 'proxy'
                ? 'ollama'
                : event.source === 'lmstudio-proxy'
                  ? 'lm-studio'
                  : null
        if (proxyEngine && event.method === 'ready') {
            // A (re)bound proxy starts with an empty manual-node set, so forget
            // what we think we bridged and re-push the local node if applicable.
            const bridge = this.getLocalBridge(proxyEngine)
            bridge.bridgedId = ''
            bridge.bridgedPort = 0
            void this.reconcileLocalNodeBridge(proxyEngine)
            this.emitStateRefreshIfHydrated()
        }
        if (event.source === 'broker' && event.method === 'app:ready') {
            this.brokerReady = true
            for (const engine of PROXY_ENGINES) void this.reconcileLocalNodeBridge(engine)
            void this.onBrokerReady().then(() => {
                emitBridgePush('state:request-refresh', undefined)
            })
        }
        this.updateReadiness()
    }

    private updateReadiness(): void {
        if (!this.ready) {
            this.readinessReported = false
            return
        }

        for (const waiter of this.readinessWaiters.values()) {
            clearTimeout(waiter.timeout)
            waiter.resolve()
        }
        this.readinessWaiters.clear()

        if (this.readinessReported) return
        this.readinessReported = true
        this.onReady?.()
    }

    private rejectReadinessWaiters(error: Error): void {
        for (const waiter of this.readinessWaiters.values()) {
            clearTimeout(waiter.timeout)
            waiter.reject(error)
        }
        this.readinessWaiters.clear()
    }

    /** Rewrite broker `proxy:`/`lmstudio-proxy:` relay frames into proxy-source events. */
    private normalizeBrokerProxy(notification: JsonRpcNotification): JsonRpcNotification {
        if (notification.source !== 'broker') return notification
        if (notification.method.startsWith('lmstudio-proxy:')) {
            return {
                source: 'lmstudio-proxy',
                method: notification.method.slice('lmstudio-proxy:'.length),
                params: notification.params
            }
        }
        if (notification.method.startsWith('proxy:')) {
            return {
                source: 'proxy',
                method: notification.method.slice('proxy:'.length),
                params: notification.params
            }
        }
        return notification
    }

    private handleErrorsNotification(notification: JsonRpcNotification): void {
        if (notification.method !== 'errors:update') return
        const allErrors = parseServiceErrors(notification.params)
        const state = getModularBridgeState()
        // Per-node proxy "upstream unreachable" warnings duplicate the node
        // list's offline group, so they must never reach the error UI. Drop them
        // from the snapshot unconditionally (no dedup, no logging).
        const errors = allErrors.filter(error => !isUpstreamUnreachableError(error))
        // The UI never shows these, so the user can never dismiss one. For any we
        // ourselves reported (nodeId === selfId), clear it from the backend
        // registry — same path as handleErrorsClear — so nvpair-errors stops holding
        // it and re-syncing it cluster-wide on its 30s heartbeat. Peer-origin
        // copies are owned by their origin node (a local clear is a no-op) and
        // are left to that node's identical clear or to EvictNode.
        const selfId = state.getSelfId()
        if (selfId && this.processes.has('broker')) {
            for (const error of allErrors) {
                if (!isUpstreamUnreachableError(error)) continue
                if (error.nodeId !== selfId) continue
                void this.callProcess('broker', 'errors:clear', { id: error.id }).catch(() => {})
            }
        }
        // A lifecycle failure (start/install/uninstall) carries engineType +
        // operation and, for command-mode start / uninstall failures, emits NO
        // engine:state-changed. Clear the optimistic transitional status when a
        // *newly* reported error matches, so the spinner never sticks. Guarding on
        // "new" (vs the prior snapshot) stops a lingering past error from
        // false-clearing a fresh op — the broker re-sends the whole snapshot on
        // every change.
        const previousIds = new Set(state.getErrors().map(error => error.id))
        state.replaceErrors(errors)
        for (const error of errors) {
            if (previousIds.has(error.id)) continue
            if (error.engineType && error.operation) {
                state.failLocalEngineOp(error.engineType, error.operation)
            }
        }
    }

    /**
     * Cluster-manager → broker → here. Six notifications:
     * - `cluster:invite-received`: an inbound pairing arrived; prompt for the PIN.
     * - `cluster:invite-canceled`: the inviter aborted a still-pending inbound
     *   invite; drop it so the receiver's PIN prompt dismisses at once.
     * - `cluster:invite-expired`: an unanswered invite hit its TTL. Emitted on
     *   the receiver too, so drop any matching inbound invite.
     * - `cluster:invite-failed`: a terminal pairing failure (a wrong PIN evicts
     *   the session); drop any matching inbound invite.
     * - `nodes:changed`: full membership snapshot.
     * - `cluster:identity-changed`: the local clusterId originated here (create or
     *   adopt-on-join); persist it to node-settings (the cluster_id owner) and
     *   tell the renderer.
     */
    private handleClusterManagerNotification(notification: JsonRpcNotification): void {
        if (notification.method === 'cluster:invite-received') {
            const invite = parseInvite(notification.params)
            // Record it in the authoritative set (re-emits the full snapshot for
            // the Settings card / CLI cache), then keep the per-arrival signal the
            // modal and tray use to surface this specific invite.
            getModularBridgeState().addPendingInvite(invite)
            emitBridgePush('cluster:invite-received', invite)
            return
        }
        if (notification.method === 'cluster:invite-canceled') {
            // Receiver-side: the inviter aborted this still-pending inbound invite
            // (its cancel-invite RPC). Prune it from the authoritative set now
            // (re-emits `cluster:pending-invites-changed`) so the PIN prompt in the
            // modal, tray, and CLI cache dismisses immediately instead of waiting for
            // the status sweep. `cluster:invite-declined` is the only inviter-side
            // terminal signal PAIR does not consume here — it carries an outbound
            // invite this node never tracked, so the outbound pairing UI reconciles
            // it via its status poll instead (recorded under ignoredMethods in
            // docs/service-contract-exceptions.json).
            const invite = parseInvite(notification.params)
            getModularBridgeState().prunePendingInvite(invite.inviteId)
            return
        }
        if (notification.method === 'cluster:invite-expired') {
            // An unanswered invite hit its TTL (default 5m). The receiver runs
            // its own inbound TTL reaper and emits this, so prune any
            // matching inbound invite to dismiss the PIN prompt / Settings card /
            // CLI cache at once (mirror of cluster:invite-canceled). On the inviter
            // the outbound invite lives in the renderer pairing hook and is
            // reconciled via the cluster:invite-status poll, so this is a no-op
            // there (the inviteId is not in the receiver-side pending set).
            const invite = parseInvite(notification.params)
            getModularBridgeState().prunePendingInvite(invite.inviteId)
            return
        }
        if (notification.method === 'cluster:invite-failed') {
            // Terminal pairing failure — a wrong PIN evicts the session.
            // The inviter emits this; on the sender the outbound invite lives in
            // the renderer pairing hook, reconciled via the cluster:invite-status
            // poll, so this is normally a no-op here. Prune any matching inbound
            // invite so a receiver-side PIN prompt / Settings card / CLI cache
            // dismisses at once (mirror of cluster:invite-canceled).
            const invite = parseInvite(notification.params)
            getModularBridgeState().prunePendingInvite(invite.inviteId)
            return
        }
        if (notification.method === 'nodes:changed') {
            const members = parseClusterNodes(notification.params)
            const selfId = getModularBridgeState().getSelfId()
            // A real peer joined → the cluster is no longer a throwaway solo
            // cluster, so the auto-dissolve guard must not fire on a later invite
            // failure.
            if (this.autoCreatedSoloForInvite) {
                const hasPeer = members.some(m => m.state === 'member' && m.nodeUuid !== selfId)
                if (hasPeer) this.autoCreatedSoloForInvite = false
            }
            // Track pinned peers so remote-engine-status polling knows which
            // nodes' `ec` surfaces are reachable, and refresh them now so a
            // freshly-joined peer's engine state converges immediately. Keyed by
            // nodeUuid — the same key `engine:remote-*` and discovery use.
            this.setClusterPeerIds(members)
            void this.refreshAllRemoteEngineStatus()
            // A pairing that completed makes the inviter a `member`, so its
            // inbound invite (if any) is obsolete — drop it from the set.
            getModularBridgeState().reconcilePendingInvitesWithMembers(members)
            emitBridgePush('nodes:changed', members)
            return
        }
        if (notification.method === 'cluster:identity-changed') {
            const obj = objectValue(notification.params)
            const clusterId = stringValue(obj?.clusterId)
            const clusterFriendlyName = stringValue(obj?.clusterFriendlyName)
            void this.persistClusterIdentity(clusterId, clusterFriendlyName)
        }
    }

    /**
     * Persist a cluster identity that originated in the cluster-manager into
     * node-settings (the cluster_id / cluster_friendly_name owner) and push it to
     * the UI. The friendly name is written **before** the id on purpose: writing
     * the id makes node-settings emit `connection/cluster-identity` (id only),
     * which we react to by re-reading the persisted name — so the name must
     * already be on disk to avoid a clobber race.
     */
    private async persistClusterIdentity(
        clusterId: string,
        clusterFriendlyName: string
    ): Promise<void> {
        try {
            await this.callProcess('broker', 'settings/set-cluster-friendly-name', {
                value: clusterFriendlyName
            })
            await this.callProcess('broker', 'settings/set-cluster-id', { value: clusterId })
        } catch (err) {
            log.verbose({
                sublevel: 'node-settings',
                message: `persist cluster identity skipped: ${getErrorString(err)}`
            })
        }
        // An empty clusterId means we left the cluster: drop peers' workloads so
        // they don't linger or get re-served on the next refresh.
        if (!clusterId) getModularBridgeState().dropRemoteWorkloads()
        emitBridgePush('connection:cluster-identity', {
            clusterId: clusterId || null,
            clusterFriendlyName
        })
    }

    /**
     * Seed the workload catalog from the broker's durable baseline
     * (`workloads:get-initial`, added this sync) right after subscribing, so the
     * main-process map holds every current + historic job before the renderer or
     * CLI asks — and so a broker restart (which funnels through `start()` →
     * `clearWorkloads()`) repopulates the full set instead of only re-streamed
     * live jobs. Best-effort: the relay stream is the backstop if it fails.
     */
    private async seedWorkloadBaseline(): Promise<void> {
        try {
            const result = await this.callProcess('broker', 'workloads:get-initial')
            getModularBridgeState().seedWorkloads(parseWorkloadsInitial(result ?? null))
        } catch (err) {
            log.verbose({
                sublevel: 'broker',
                message: `workload baseline seed skipped: ${getErrorString(err)}`
            })
        }
    }

    /**
     * Fix `selfId` to the cluster-manager's stable node UUID as soon as the
     * broker is ready. The UUID is minted at cluster-manager startup, so it is
     * available immediately (independent of ever joining a cluster) and is the
     * same key discovery/proxy/workloads/errors use. Resolving it eagerly here —
     * rather than lazily on the renderer's first `app:get-initial` — lets the
     * local-node proxy bridge and remote-status polling exclude self correctly
     * from the first tick. `setSelfId` is a no-op when unchanged.
     */
    private async resolveSelfId(): Promise<void> {
        try {
            const identity = parseNodeIdentity(
                await this.callProcess('broker', 'cluster:get-node-id')
            )
            if (identity.nodeUuid) getModularBridgeState().setSelfId(identity.nodeUuid)
        } catch (err) {
            log.verbose({
                sublevel: 'broker',
                message: `self-id resolution skipped: ${getErrorString(err)}`
            })
        }
    }

    /**
     * Push the persisted cluster identity (owned by node-settings) into the
     * cluster-manager via the broker relay, so outbound invites carry the right
     * clusterId. `cluster:set-identity` never re-emits `cluster:identity-changed`,
     * so this cannot loop with the persistence path above.
     */
    private async syncClusterIdentityToManager(): Promise<void> {
        try {
            const idResult = objectValue(
                await this.callProcess('broker', 'settings/get-cluster-id')
            )
            await this.callProcess('broker', 'cluster:set-identity', {
                clusterId: stringValue(idResult?.value),
                clusterFriendlyName: await this.readClusterFriendlyName()
            })
        } catch (err) {
            log.verbose({
                sublevel: 'broker',
                message: `cluster:set-identity sync skipped: ${getErrorString(err)}`
            })
        }
    }

    /** Read the persisted cluster friendly name (node-settings owns it). */
    private async readClusterFriendlyName(): Promise<string> {
        try {
            const result = objectValue(
                await this.callProcess('broker', 'settings/get-cluster-friendly-name')
            )
            return stringValue(result?.value)
        } catch {
            return ''
        }
    }

    /**
     * node-settings emitted `connection/cluster-identity`, which carries only the
     * id. Re-read the persisted friendly name (node-settings owns it) so the
     * cluster-manager stays in sync, and push the identity to the UI. The
     * backend's id-only push is immaterial to the renderer (the friendly name is
     * not displayed); see docs/services-parity.md §10.
     */
    private async onClusterIdentityChanged(clusterId: string): Promise<void> {
        const clusterFriendlyName = await this.readClusterFriendlyName()
        try {
            await this.callProcess('broker', 'cluster:set-identity', {
                clusterId,
                clusterFriendlyName
            })
        } catch (err) {
            log.verbose({
                sublevel: 'broker',
                message: `cluster:set-identity sync skipped: ${getErrorString(err)}`
            })
        }
        // An empty clusterId means we left the cluster: drop peers' workloads so
        // they don't linger or get re-served on the next refresh.
        if (!clusterId) getModularBridgeState().dropRemoteWorkloads()
        emitBridgePush('connection:cluster-identity', {
            clusterId: clusterId || null,
            clusterFriendlyName
        })
    }

    private handleEngineManagerNotification(notification: JsonRpcNotification): void {
        if (notification.method === 'engine:ready') {
            void this.hydrateEngineManager()
            return
        }
        if (notification.method === 'engine:state-changed') {
            getModularBridgeState().applyEngineManagerStatus(notification.params)
            this.refreshManagedEngineModels(notification.params)
            this.updateLocalNodeBridgeFromEngineState(notification.params)
            return
        }
        if (notification.method === 'engine:models-changed') {
            // An engine's in-memory (loaded) model set changed — explicit
            // load/unload, LM Studio JIT auto-load, or idle/TTL eviction. The push
            // carries the full engine:models snapshot; forward its per-engine
            // loaded set so the local node's model rows reflect residency at once,
            // ahead of the next discovery model-refresh sweep.
            getModularBridgeState().applyLocalLoadedModels(parseLoadedByEngine(notification.params))
            return
        }
        if (notification.method === 'engine:install-progress') {
            getModularBridgeState().applyEngineManagerProgress(notification.params)
            return
        }
        if (notification.method === 'engine:pull-progress') {
            // Live download progress for a local model pull (driven via
            // engine:action{pull_model}) — the local counterpart of
            // engine:remote-progress. Advances the optimistic pull entry's
            // percent; the awaited pull RPC still owns clearing it.
            getModularBridgeState().applyLocalEngineProgress(notification.params)
            return
        }
        if (notification.method === 'engine:remote-progress') {
            getModularBridgeState().applyRemoteEngineProgress(notification.params)
        }
    }

    /**
     * Download a model through `nvpair-engine-manager`'s `pull_model` action. The
     * backend routes `engine:action{pull_model}` through its streaming pull path
     * so it emits live `engine:pull-progress` frames while the
     * download runs and reports pull failures via `errors:report` with
     * engine-attributed copy. The action response remains the completion signal,
     * so we:
     *   1. show an optimistic `pulling` entry immediately (so the modal closing
     *      isn't followed by dead silence) that the streamed progress advances,
     *   2. **await** the action response — unlike lifecycle ops, the response is
     *      the only completion signal, so we use a long timeout instead of
     *      fire-and-forget,
     *   3. parse it for an explicit error and otherwise refresh the authoritative
     *      model list, then clear the optimistic entry.
     * Fire-and-forget from the caller's perspective: it is `void`-ed so the UI
     * command returns immediately.
     */
    async pullModel(engine: string, engineType: EngineType, model: string): Promise<void> {
        if (!this.brokerReady) {
            this.reportError(
                `Cannot download ${model}: the modular backend is not running.`,
                'warning',
                `engine-pull:${engine}`
            )
            return
        }
        const state = getModularBridgeState()
        if (state.isModelPullActive(engineType, model)) return

        state.beginModelPull(engineType, model)
        try {
            const result = await this.callProcess(
                'broker',
                'engine:action',
                { engine, action: 'pull_model', params: pullModelParams(engine, model) },
                PULL_TIMEOUT_MS
            )
            const failure = pullResultError(result)
            if (failure) {
                this.reportError(failure, 'error', `engine-pull:${engine}:${model}`, {
                    engineType,
                    operation: 'pull',
                    modelName: model,
                    action: 'retry'
                })
            } else {
                await this.refreshEngineModels(engine, engineType)
            }
        } catch (err) {
            const message = getErrorString(err) ?? ''
            const pullError = resolvePullCatchError(message)
            if (pullError) {
                this.reportError(pullError, 'error', `engine-pull:${engine}:${model}`, {
                    engineType,
                    operation: 'pull',
                    modelName: model,
                    action: 'retry'
                })
            }
        } finally {
            state.finishModelPull(engineType, model)
        }
    }

    /**
     * Delete a downloaded model locally. Awaits the backend action, refreshes
     * `list_models`, and surfaces RPC failures so the optimistic spinner clears
     * on real completion instead of the safety-net timeout.
     *
     * Whether deleting also restarts the engine is the manifest's decision
     * (`restart_after`, which LM Studio declares because it serves its model list
     * from an index built at startup). The action does not reply until that
     * restart finishes, so the refresh below already reads the restarted engine.
     *
     * Every reported failure carries `engineType` + `modelName` so
     * `pending-actions.store` clears the row's "Deleting…" entry on the error
     * push. Without that context the error lands unattributed and the spinner
     * spins on to the safety-net timeout even though the operation is over.
     */
    async deleteModel(engine: string, engineType: EngineType, model: string): Promise<void> {
        const errorContext = { engineType, operation: 'delete', modelName: model } as const
        if (!this.brokerReady) {
            this.reportError(
                `Cannot delete ${model}: the modular backend is not running.`,
                'warning',
                `engine-delete:${engine}`,
                errorContext
            )
            return
        }
        try {
            await this.callProcess(
                'broker',
                'engine:action',
                { engine, action: 'delete_model', params: deleteModelParams(engine, model) },
                MODULAR_MODEL_ACTION_TIMEOUT_MS
            )
        } catch (err) {
            const detail = getErrorString(err)
            const restartFailed = isRestartAfterFailure(detail)
            this.reportError(
                restartFailed
                    ? `Deleted ${model}, but ${engine} failed to restart: ${detail}`
                    : `Failed to delete ${model}: ${detail}`,
                'error',
                `engine-delete:${engine}:${model}`,
                errorContext
            )
            // A restart failure means the files are already gone, so the cached
            // list is stale exactly as it would be on success. Refresh anyway
            // (it never throws) so the row converges instead of showing a model
            // that no longer exists.
            if (restartFailed) await this.refreshEngineModels(engine, engineType)
            return
        }
        await this.refreshEngineModels(engine, engineType)
    }

    /**
     * Download a model on a remote peer via `engine:remote-pull-model`
     * (`nvpair-engine-manager` ec surface). Unlike a local pull, the backend emits
     * **no** terminal `engine:remote-progress` for a remote pull — the peer's
     * terminal stream frame becomes this RPC's reply
     * (`nvpair-engine-manager/controlstream.go`), never a notification. So the
     * awaited RPC settling is the *only* completion signal; we clear the
     * optimistic entry here on resolve/reject. Fire-and-forget for the caller
     * (`void`-ed) so the UI command returns immediately.
     */
    async pullModelRemote(
        nodeId: string,
        engine: string,
        engineType: EngineType,
        model: string
    ): Promise<void> {
        const state = getModularBridgeState()
        if (state.isRemoteModelPullActive(nodeId, engineType, model)) return
        state.beginRemoteModelPull(nodeId, engineType, model)
        try {
            await this.callProcess(
                'broker',
                'engine:remote-pull-model',
                { node: nodeId, engine, model },
                PULL_TIMEOUT_MS
            )
            // The peer's /v1/models re-enriches into discovery asynchronously;
            // refresh its authoritative status now so a just-installed engine
            // does not read stale.
            await this.refreshRemoteEngineStatus(nodeId)
        } catch (err) {
            const message = getErrorString(err) ?? ''
            const pullError = resolvePullCatchError(message)
            if (pullError) {
                this.reportError(
                    pullError,
                    'error',
                    `engine-remote-pull:${nodeId}:${engine}:${model}`,
                    {
                        engineType,
                        nodeId,
                        operation: 'pull',
                        modelName: model,
                        action: 'retry'
                    }
                )
            }
        } finally {
            state.finishRemoteModelPull(nodeId, engineType, model)
        }
    }

    /**
     * Install (and optionally start) an engine on a remote peer via
     * `engine:remote-install`. Streams progress frames as `engine:remote-progress`
     * but its terminal is only the awaited RPC reply, so we clear the optimistic
     * `installing` op here and refresh the peer's authoritative status.
     */
    async installEngineRemote(
        nodeId: string,
        engine: string,
        engineType: EngineType,
        start: boolean
    ): Promise<void> {
        const state = getModularBridgeState()
        state.beginRemoteEngineOp(nodeId, engineType, 'installing')
        try {
            await this.callProcess(
                'broker',
                'engine:remote-install',
                { node: nodeId, engine, start },
                PULL_TIMEOUT_MS
            )
            await this.refreshRemoteEngineStatus(nodeId)
        } catch (err) {
            // Use engine-manager's canonical error identity so a rejection here
            // upserts the backend's own report (and offers retry) rather than
            // duplicating it under a UI-synthesized id.
            this.reportError(
                `Failed to install ${engine} on ${nodeId}: ${getErrorString(err)}`,
                'error',
                `engine-remote-install:${nodeId}:${engine}`,
                {
                    id: `engine-manager:install-failed:${engine}`,
                    nodeId,
                    engineType: engine,
                    operation: 'install',
                    action: 'retry'
                }
            )
        } finally {
            state.clearPendingRemoteEngineOp(nodeId, engineType)
        }
    }

    /**
     * Start or stop an engine on a remote peer via `engine:remote-start` /
     * `engine:remote-stop`. These return the resulting `EngineStatus`, so we
     * apply it authoritatively the instant the RPC resolves instead of waiting
     * on the next facts poll or an mDNS presence flip.
     */
    async toggleEngineRemote(
        nodeId: string,
        engine: string,
        engineType: EngineType,
        running: boolean
    ): Promise<void> {
        const state = getModularBridgeState()
        state.beginRemoteEngineOp(nodeId, engineType, running ? 'stopping' : 'starting')
        try {
            const result = await this.callProcess(
                'broker',
                running ? 'engine:remote-stop' : 'engine:remote-start',
                { node: nodeId, engine },
                PULL_TIMEOUT_MS
            )
            state.clearPendingRemoteEngineOp(nodeId, engineType)
            state.applyRemoteEngineStatusResult(nodeId, engineType, result)
        } catch (err) {
            state.clearPendingRemoteEngineOp(nodeId, engineType)
            this.reportError(
                `Failed to ${running ? 'stop' : 'start'} ${engine} on ${nodeId}: ${getErrorString(err)}`,
                'error',
                `engine-remote-toggle:${nodeId}:${engine}`
            )
        }
    }

    /**
     * Load, unload (eject), or delete a model on a remote peer via the ec surface.
     *
     * A peer runs the same manifests we do, so a remote delete on LM Studio also
     * bounces the peer's engine and can fail the same restart-after way — the
     * message distinguishes the two cases exactly as the local path does. The
     * error is stamped with the *peer's* nodeId (not our selfId) plus
     * `engineType`/`modelName` so `pending-actions.store` clears the right row's
     * spinner instead of leaving it to the safety-net timeout.
     */
    async modelActionRemote(
        nodeId: string,
        engine: string,
        engineType: EngineType,
        model: string,
        command: 'loadModel' | 'unloadModel' | 'deleteModel'
    ): Promise<void> {
        const method =
            command === 'loadModel'
                ? 'engine:remote-load-model'
                : command === 'unloadModel'
                  ? 'engine:remote-unload-model'
                  : 'engine:remote-delete-model'
        try {
            await this.callProcess(
                'broker',
                method,
                { node: nodeId, engine, model },
                PULL_TIMEOUT_MS
            )
            await this.refreshRemoteEngineStatus(nodeId)
        } catch (err) {
            const verb =
                command === 'loadModel' ? 'load' : command === 'unloadModel' ? 'unload' : 'delete'
            const detail = getErrorString(err)
            const restartFailed = command === 'deleteModel' && isRestartAfterFailure(detail)
            this.reportError(
                restartFailed
                    ? `Deleted ${model} on ${nodeId}, but ${engine} failed to restart: ${detail}`
                    : `Failed to ${verb} ${model} on ${nodeId}: ${detail}`,
                'error',
                `engine-remote-model:${nodeId}:${engineType}:${model}:${verb}`,
                { nodeId, engineType, operation: verb, modelName: model }
            )
            if (restartFailed) await this.refreshRemoteEngineStatus(nodeId)
        }
    }

    /**
     * Re-pull an engine's authoritative `list_models` after a model op and push
     * it. Awaitable (so {@link pullModel} can land the new model in the list
     * before clearing its spinner) and never throws — a failed refresh just logs.
     */
    private async refreshEngineModels(engine: string, engineType: EngineType): Promise<void> {
        if (this.stoppedModelEngines.has(engineType)) return
        const generation = this.beginModelRefresh(engineType)
        try {
            const result = await this.callProcess('broker', 'engine:action', {
                engine,
                action: 'list_models'
            })
            const models = parseListModelNames(result)
            this.commitModelInventory(engineType, models, generation)
        } catch (err) {
            this.handleModelRefreshFailure(engineType, generation)
            log.verbose({
                sublevel: 'engine-manager',
                message: `list_models after pull for ${engine} failed: ${getErrorString(err)}`
            })
        }
    }

    /**
     * Pull the local node's model list for an engine via engine-manager's
     * `list_models` action, then cache + push it. Runs for **every** engine
     * including Ollama: the discovery plane (proxy mDNS) advertises no model list,
     * so `/api/tags` is the only working source for the local node's models.
     * `setLocalEngineModels` scopes the result to the local node, so remote nodes
     * keep their discovery-derived lists. The action is an HTTP GET against the
     * engine's loopback API and only works while it is running, so we clear the
     * list when the engine is stopped/uninstalled.
     */
    refreshManagedEngineModels(params: JsonValue | undefined): void {
        const obj = objectValue(params)
        if (!obj) return
        const engine = stringValue(obj.engine)
        const engineType = getModularBridgeState().modelPullTarget(engine)
        if (!engineType) return

        if (!booleanValue(obj.running)) {
            this.beginModelRefresh(engineType)
            this.stoppedModelEngines.add(engineType)
            this.stoppedModelSentinels.add(engineType)
            this.successfulModelInventories.delete(engineType)
            if (isProxyEngine(engineType)) {
                this.cancelDiscoveryModelRefreshRetry(engineType)
            }
            getModularBridgeState().setLocalEngineModels(engineType, [])
            return
        }

        // A late running notification can arrive during teardown, after stop()
        // has cleared the process map. The stopped branch above must still
        // invalidate older requests, but a running branch must not make a new
        // broker call in that window.
        if (!this.hasProcess('broker')) return
        this.stoppedModelEngines.delete(engineType)
        const generation = this.beginModelRefresh(engineType)
        this.callProcess('broker', 'engine:action', {
            engine,
            action: 'list_models'
        })
            .then(result => {
                const models = parseListModelNames(result)
                this.commitModelInventory(engineType, models, generation)
            })
            .catch(err => {
                this.handleModelRefreshFailure(engineType, generation)
                log.verbose({
                    sublevel: 'engine-manager',
                    message: `list_models for ${engine} failed: ${getErrorString(err)}`
                })
            })
    }

    /**
     * Re-pull the local node's model list for a proxy engine when discovery
     * reports it changed (a pull/delete on an already-running engine emits no
     * `engine:state-changed`, so {@link refreshManagedEngineModels} alone would
     * miss it). On success the cache holds the complete, untruncated list; if
     * engine-manager has never supplied a valid inventory (for example an
     * external engine it has not adopted), a failure falls back to discovery.
     * Once a valid inventory exists, transient failures retain it.
     */
    refreshDiscoveryEngineModels(engine: ProxyEngine): void {
        this.cancelDiscoveryModelRefreshRetry(engine, false)
        if (this.stoppedModelEngines.has(engine) || this.shuttingDown) return
        if (!this.hasProcess('broker')) {
            this.scheduleDiscoveryModelRefreshRetry(engine)
            return
        }
        const generation = this.beginModelRefresh(engine)
        this.callProcess('broker', 'engine:action', {
            engine: engineManagerId(engine),
            action: 'list_models'
        })
            .then(result => {
                const models = parseListModelNames(result)
                this.commitModelInventory(engine, models, generation)
            })
            .catch(() => {
                if (this.modelRefreshGenerations.get(engine) === generation) {
                    this.scheduleDiscoveryModelRefreshRetry(engine)
                }
                this.handleModelRefreshFailure(engine, generation)
            })
    }

    private beginModelRefresh(engine: EngineType): number {
        const generation = (this.modelRefreshGenerations.get(engine) ?? 0) + 1
        this.modelRefreshGenerations.set(engine, generation)
        return generation
    }

    private commitModelInventory(engine: EngineType, models: string[], generation: number): void {
        if (this.modelRefreshGenerations.get(engine) !== generation) return
        this.stoppedModelSentinels.delete(engine)
        this.successfulModelInventories.add(engine)
        if (isProxyEngine(engine)) {
            this.cancelDiscoveryModelRefreshRetry(engine)
        }
        getModularBridgeState().setLocalEngineModels(engine, models)
    }

    private scheduleDiscoveryModelRefreshRetry(engine: ProxyEngine): void {
        if (
            this.discoveryModelRetryTimers.has(engine) ||
            this.stoppedModelEngines.has(engine) ||
            this.shuttingDown
        ) {
            return
        }
        const attempt = (this.discoveryModelRetryAttempts.get(engine) ?? 0) + 1
        this.discoveryModelRetryAttempts.set(engine, attempt)
        const delay = Math.min(
            DISCOVERY_MODEL_RETRY_MIN_MS * 2 ** (attempt - 1),
            DISCOVERY_MODEL_RETRY_MAX_MS
        )
        const timer = setTimeout(() => {
            this.discoveryModelRetryTimers.delete(engine)
            this.refreshDiscoveryEngineModels(engine)
        }, delay)
        this.discoveryModelRetryTimers.set(engine, timer)
    }

    private cancelDiscoveryModelRefreshRetry(engine: ProxyEngine, resetAttempt = true): void {
        const timer = this.discoveryModelRetryTimers.get(engine)
        if (timer) clearTimeout(timer)
        this.discoveryModelRetryTimers.delete(engine)
        if (resetAttempt) this.discoveryModelRetryAttempts.delete(engine)
    }

    private handleModelRefreshFailure(engine: EngineType, generation: number): void {
        if (this.modelRefreshGenerations.get(engine) !== generation) return
        if (!isProxyEngine(engine)) return
        const wasStoppedSentinel = this.stoppedModelSentinels.delete(engine)
        if (wasStoppedSentinel || !this.successfulModelInventories.has(engine)) {
            // A stop caches [] so stale models are not shown while the engine is
            // down. If the first inventory after restart fails, release that
            // sentinel and use discovery. Once a real inventory succeeds,
            // including [], later transient failures retain it.
            getModularBridgeState().fallbackLocalEngineModelsToDiscovery(engine)
        }
    }

    /** Per-engine local bridge state, created lazily. */
    private getLocalBridge(engine: ProxyEngine): LocalEngineBridge {
        let bridge = this.localBridges.get(engine)
        if (!bridge) {
            bridge = emptyLocalEngineBridge()
            this.localBridges.set(engine, bridge)
        }
        return bridge
    }

    /**
     * Update the desired local-node → proxy bridge from a local engine
     * engine:state-changed and reconcile. The engine:state-changed carries the
     * **real** local engine port, which is the one the proxy must route to (mDNS
     * self-discovery can advertise the wrong port even when it works). Applies to
     * every proxy-fronted engine (Ollama → ollama-proxy, LM Studio →
     * lmstudio-proxy); loopback-only engines are ignored.
     */
    private updateLocalNodeBridgeFromEngineState(params: JsonValue | undefined): void {
        const obj = objectValue(params)
        if (!obj) return
        const engine = proxyEngineFromManagerId(stringValue(obj.engine))
        if (!engine) return
        const bridge = this.getLocalBridge(engine)
        bridge.running = booleanValue(obj.running)
        bridge.port = numberValue(obj.port)
        void this.reconcileLocalNodeBridge(engine)
    }

    /**
     * Bridge the local node directly into an engine's reverse proxy so inference
     * works even when mDNS self-discovery on this machine is unreliable — the
     * failure that leaves the proxy's node list empty and makes it return
     * `{"error":"no active node selected or available"}` despite a healthy local
     * engine. Keyed by the local node id so it **de-dups against an
     * mDNS-discovered local entry**: the proxy's `Nodes()` prefers a discovered
     * node over a manual one with the same id, so when mDNS works this manual
     * entry is shadowed (no duplicate), and when it doesn't this is the only
     * entry. Idempotent and safe to call repeatedly; re-bridges after a proxy
     * restart (which clears the proxy's manual set) via the `<proxy>:ready` hook.
     */
    private async reconcileLocalNodeBridge(engine: ProxyEngine): Promise<void> {
        if (!this.processes.has('broker')) return
        const selfId = getModularBridgeState().getSelfId()
        // selfId not resolved yet — reconcile re-runs on the next trigger
        // (engine:state-changed, proxy ready, broker ready).
        if (!selfId) return

        const bridge = this.getLocalBridge(engine)

        // Never register the local node at the proxy's own listen port. The
        // proxy would forward requests to itself (127.0.0.1:proxyPort →
        // 127.0.0.1:proxyPort), recursing until the client cancels — a flood of
        // `proxy/request` 502 "context canceled" events. This collision happens
        // when the proxy occupies the engine's native port (the Ollama proxy
        // binds 11434 so dumb clients reach the cluster) while the engine's real
        // port also resolves to that value. Treat a self-target as "do not
        // bridge" and fall through to remove any stale self-pointing entry.
        const proxyPort = getModularBridgeState().getProxyPort(engine)
        // Only a known, matching proxy port is a self-target; an unknown
        // (null) proxy port never blocks bridging.
        const selfTarget = proxyPort !== null && bridge.port === proxyPort
        if (selfTarget && bridge.port > 0) {
            if (!bridge.selfWarned) {
                bridge.selfWarned = true
                log.warn({
                    sublevel: proxyRelayPrefix(engine),
                    message:
                        `Skipping local-node ${engine} proxy bridge: engine port ` +
                        `${bridge.port} matches the proxy's own listen port ` +
                        '(would self-forward). Resolve the engine/proxy port collision.'
                })
            }
        } else {
            bridge.selfWarned = false
        }

        const shouldBridge = bridge.running && bridge.port > 0 && !selfTarget
        if (shouldBridge) {
            if (bridge.bridgedId === selfId && bridge.bridgedPort === bridge.port) {
                return
            }
            try {
                await this.callProxy(engine, 'node/add-manual', {
                    id: selfId,
                    host: '127.0.0.1',
                    port: bridge.port,
                    addresses: ['127.0.0.1']
                })
                bridge.bridgedId = selfId
                bridge.bridgedPort = bridge.port
            } catch (err) {
                log.warn({
                    sublevel: proxyRelayPrefix(engine),
                    message: `Failed to bridge local node into ${engine} proxy: ${getErrorString(err)}`
                })
            }
            return
        }

        if (bridge.bridgedId) {
            const previousId = bridge.bridgedId
            bridge.bridgedId = ''
            bridge.bridgedPort = 0
            try {
                await this.callProxy(engine, 'node/remove-manual', { id: previousId })
            } catch (err) {
                log.verbose({
                    sublevel: proxyRelayPrefix(engine),
                    message: `Local node was not bridged into ${engine} proxy: ${getErrorString(err)}`
                })
            }
        }
    }
}

let supervisor: ModularSupervisor | null = null

export function getModularSupervisor(): ModularSupervisor {
    if (!supervisor) {
        supervisor = new ModularSupervisor()
    }
    return supervisor
}
