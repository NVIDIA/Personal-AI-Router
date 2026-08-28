// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { EngineInitialState } from '@/shared/types/engine-api'
import type {
    EngineModels,
    EngineProcessStatus,
    EngineProgress,
    EngineStatusData,
    EngineType,
    ModelItem
} from '@/shared/types/engines'
import type { LogEntry, LogPage } from '@/shared/types/log'
import type { NodeItemMetrics } from '@/shared/types/metrics'
import type { NodeItem } from '@/shared/types/nodes'
import type { ServiceError, ServiceErrorAction, ServiceErrorSeverity } from '@/shared/types/errors'
import type { Workload, WorkloadState } from '@/shared/types/workloads'
import type { ClusterNode, Invite } from '@/shared/types/cluster'
import type { WsInvokeResponse } from '@/shared/types/ws-channels'
import {
    MODULAR_NODE_INFO_SELF_HOST,
    MODULAR_SUPERSEDE_MIN_AGE_MS
} from '@/shared/constants/modular-runtime'
import { WorkloadStates } from '@/shared/constants/workloads'
import { isEngineType, emptyEngineStatus } from '@/shared/utils/engines'
import { engineProgressKey } from '@/shared/utils/engine-progress'
import { workloadKey } from '@/shared/utils/workloads'
import { currentPlatform, platformDisplayName } from '@/shared/utils/platform'
import { emitBridgePush } from './broadcaster'
import { mergePullProgressPercent } from './pull-error-handling'
import type { JsonObject, JsonRpcNotification, JsonValue } from './json-rpc-subprocess'
import { serviceLogLevel } from './service-log-level'
// Live node sources are the two reverse proxies, relayed through the broker,
// and the broker's consolidated discovery snapshot. Electron does not consume
// worker discovery protocols directly.
type ProxyNodeSource = 'ollama-proxy' | 'lmstudio-proxy'
type BrokerNodeSource = ProxyNodeSource | 'broker'

/**
 * Engines surfaced by the broker's proxy plane. Other engine-manager engines
 * are not currently routed across nodes.
 */
export type ProxyEngine = Extract<EngineType, 'ollama' | 'lm-studio'>
export const PROXY_ENGINES: readonly ProxyEngine[] = ['ollama', 'lm-studio']

/** Map a proxy node source onto the engine it describes. */
const PROXY_SOURCE_ENGINE: Record<ProxyNodeSource, ProxyEngine> = {
    'ollama-proxy': 'ollama',
    'lmstudio-proxy': 'lm-studio'
}

/** Per-engine presence on a node — each proxy reports its own engine. */
interface EnginePresence {
    up: boolean
    /**
     * The node's promoted inference **proxy** port for this engine, as
     * advertised in discovery. Under secure inference the broker registers the
     * `ol`/`lm` service at the proxy's port — never the engine's own port, which
     * is loopback-private and reachable by peers only through that proxy's
     * cluster-mTLS ingress. The engine's real server port is not in discovery;
     * it comes from `engine:remote-get-installed` facts (a peer) or
     * engine-manager status (self).
     */
    port: number
    version: string | null
}

/**
 * Authoritative install/run facts for a remote peer's engine, read from that
 * peer's cluster-scoped `ec` control surface via `engine:remote-get-installed`
 * (pin-based mTLS). Unlike {@link EnginePresence} — which is inferred from mDNS
 * proxy advertisement and can only ever say "up/down" — these are the peer
 * engine-manager's own truth: whether it is installed, whether it is running,
 * and the configured server port (present even while stopped). Only available
 * for clustered, reachable peers; when absent we fall back to presence.
 */
interface RemoteEngineFacts {
    installed: boolean
    running: boolean
    healthy: boolean
    port: number
}

interface ModularNode {
    // Canonical cross-domain node key = the backend's stable per-host UUID
    // (`shared/nodeid`). The broker carries it on `AvailableNode.hostUuid`, the
    // proxies key `Node.ID` by it, and `/v1/node-info` reports it, so every
    // domain now agrees on this id: workload `originatedFrom`/`scheduledOn`,
    // error `nodeId`, and proxy routing all resolve against it. Never key,
    // dedupe, or attribute by hostname — that is display only (`name`).
    id: string
    sources: BrokerNodeSource[]
    // Display hostname (broker `AvailableNode.name` / proxy `Node.Host`). Shown
    // to the user; never a correlation key.
    name: string
    host: string
    // Whether the node is a locally-trusted (pinned) peer and whether it already
    // belongs to a cluster, from broker discovery (`AvailableNode.trusted` /
    // `.clustered`). Absent on the proxy `node/*` feed (defaults false), so a
    // proxy-sourced update keeps the last broker-reported values on merge.
    // `clustered` gates invite eligibility (a node already in a cluster cannot be
    // invited into another).
    trusted: boolean
    clustered: boolean
    nodeInfoPort: number
    nodeInfoUp: boolean
    // The node's own ranked candidate list from broker discovery
    // (`AvailableNode.ipAddresses`, or its single `ipAddress`). Authoritative and
    // ordered: a broker snapshot replaces this wholesale, so an address the node
    // has re-ranked away stops being polled and stops being projected. It is a
    // separate field from {@link proxyAddresses} for exactly that reason — while
    // the two feeds shared one list, a union was the only merge that did not
    // discard the proxy's addresses, and a union can never drop anything.
    brokerAddresses: string[]
    // Addresses known only to the proxy `node/*` feed (`Node.Addresses`), which
    // discovers independently of the broker. Projected behind the broker's order
    // by {@link nodeAddresses}, and the only address list a proxy-only node has.
    proxyAddresses: string[]
    // The node's canonical LAN address. Both discovery sources now stamp it with
    // the backend's shared `netpick.Primary` result: the broker's
    // `AvailableNode.ipAddress` and the proxy `node/*` `ip` field. Used directly
    // for the display IP and the node-info poll target — PAIR UI no longer ranks
    // addresses itself. The broker value is preferred on merge; a proxy-only node
    // carries the proxy's value. Empty only until a node's first event arrives.
    reachableAddress: string
    gpus: ModularGpu[]
    cpu: ModularCpu | null
    memory: ModularMemory | null
    // Node-level list of inference-ready hardware ids from /v1/node-info, mapped
    // straight to SystemTopology.inferenceHardwareIds. undefined = the backend
    // does not report readiness yet (UI shows all GPUs). See the routing
    // limitations in docs/services-parity.md.
    inferenceHardwareIds?: string[]
    // The node's unified model list, enriched by the broker daemon from the
    // peer's engine-manager `/v1/models` (models-http) and carried on
    // `AvailableNode.models`. It replaces the retired per-engine proxy
    // `models=` TXT as the remote-node model source; the local node still prefers
    // engine-manager `list_models`. Kept as the flat node-level union for the
    // fallback path (a peer that sends no per-engine attribution).
    models: string[]
    // Per-engine attribution of the enriched model list, keyed by engine-manager
    // engine name ("ollama", "lmstudio"), carried on `AvailableNode.modelsByEngine`.
    // When present it is authoritative — {@link toEngineModels} attributes each
    // model to the engine that actually serves it, so a dual-engine node no
    // longer blanks its cards. A present engine key with [] is known empty; an
    // empty object means no successful attribution (or a peer that predates
    // per-engine attribution) and falls back to {@link models}. Mirrors
    // noderec.DirectoryNode.EngineModels.
    modelsByEngine: Record<string, string[]>
    // Per-engine set of models currently loaded in memory, keyed by
    // engine-manager engine name ("ollama", "lmstudio"), carried on
    // `AvailableNode.loadedByEngine`. Normally a subset of
    // {@link modelsByEngine}. An engine key with an empty list means
    // "running, nothing loaded"; a missing key means loaded state wasn't
    // reported. Drives {@link ModelItem.status} `'loaded'`. Empty object when no
    // engine reports loaded state. Mirrors noderec.DirectoryNode.LoadedByEngine.
    loadedByEngine: Record<string, string[]>
    engines: Record<ProxyEngine, EnginePresence>
    lastSeen: number
}

function emptyPresence(): EnginePresence {
    return { up: false, port: 0, version: null }
}

function emptyEngines(): Record<ProxyEngine, EnginePresence> {
    return { ollama: emptyPresence(), 'lm-studio': emptyPresence() }
}

/** Immutably set one engine's presence, preserving the other. */
function setEngine(
    engines: Record<ProxyEngine, EnginePresence>,
    engine: ProxyEngine,
    presence: EnginePresence
): Record<ProxyEngine, EnginePresence> {
    return {
        ollama: engine === 'ollama' ? presence : engines.ollama,
        'lm-studio': engine === 'lm-studio' ? presence : engines['lm-studio']
    }
}

/**
 * A node's primary reachable service port: a live engine's promoted proxy port
 * when one is up (the endpoint peers route inference to), else the node-info
 * HTTP port. Display-only — never dialed directly for inference.
 */
function primaryServicePort(node: ModularNode): number {
    for (const engine of PROXY_ENGINES) {
        const presence = node.engines[engine]
        if (presence.up && presence.port > 0) return presence.port
    }
    return node.nodeInfoPort
}

/** Whether any proxy engine reports the node as up. */
function anyEngineUp(node: ModularNode): boolean {
    return PROXY_ENGINES.some(engine => node.engines[engine].up)
}

interface ModularGpu {
    name: string
    vramBytes: number
    vramUsedBytes: number
    utilizationPercent: number
}

interface ModularCpu {
    name: string
    cores: number
    utilizationPercent: number
}

interface ModularMemory {
    totalBytes: number
    usedBytes: number
}

const appStartTime = new Date().toISOString()

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

function stringArrayValue(value: JsonValue | undefined): string[] {
    if (!Array.isArray(value)) return []
    return value.filter((entry): entry is string => typeof entry === 'string')
}

/**
 * Parse the broker `AvailableNode.modelsByEngine` map (engine name -> model
 * names). A missing/malformed field yields `{}`, which {@link modelsForEngine}
 * treats as "no attribution" and falls back to the flat union. Keyed by
 * engine-manager engine name ("ollama", "lmstudio").
 */
function parseModelsByEngine(value: JsonValue | undefined): Record<string, string[]> {
    const obj = objectValue(value)
    if (!obj) return {}
    const out: Record<string, string[]> = {}
    for (const [engine, list] of Object.entries(obj)) {
        out[engine] = stringArrayValue(list)
    }
    return out
}

/**
 * Read an optional string array, preserving the absent vs empty distinction the
 * `inference_hardware_ids` contract relies on: a missing field returns
 * `undefined` (backend reports no readiness — show all GPUs), an empty array
 * returns `[]` (no inference-ready hardware). See the routing limitations in
 * docs/services-parity.md.
 */
function optionalStringArrayValue(value: JsonValue | undefined): string[] | undefined {
    if (!Array.isArray(value)) return undefined
    return value.filter((entry): entry is string => typeof entry === 'string')
}

function nullableStringValue(value: JsonValue | undefined): string | null {
    return typeof value === 'string' ? value : null
}

function nullableNumberValue(value: JsonValue | undefined): number | null {
    return typeof value === 'number' ? value : null
}

function serviceErrorSeverity(value: JsonValue | undefined): ServiceErrorSeverity | undefined {
    return value === 'info' || value === 'warning' || value === 'error' ? value : undefined
}

function serviceErrorAction(value: JsonValue | undefined): ServiceErrorAction | undefined {
    return value === 'retry' || value === 'none' || value === 'dismiss' ? value : undefined
}

function parseServiceError(value: JsonValue | undefined): ServiceError | null {
    const obj = objectValue(value)
    if (!obj) return null
    const id = stringValue(obj.id)
    const message = stringValue(obj.message)
    if (!id || !message) return null

    const error: ServiceError = {
        id,
        message,
        timestamp: numberValue(obj.timestamp) || Date.now()
    }
    const severity = serviceErrorSeverity(obj.severity)
    if (severity) error.severity = severity
    const action = serviceErrorAction(obj.action)
    if (action) error.action = action
    const nodeId = stringValue(obj.nodeId)
    if (nodeId) error.nodeId = nodeId
    // The engine-manager stamps its own id (`lmstudio`); map it onto our closed
    // `EngineType` union so the renderer's retry/display logic recognizes it.
    // Unknown ids fall through unchanged (no retry affordance will match).
    const engineType = stringValue(obj.engineType)
    if (engineType) error.engineType = engineManagerEngineType(engineType) ?? engineType
    const operation = stringValue(obj.operation)
    if (operation) error.operation = operation
    const modelName = stringValue(obj.modelName)
    if (modelName) error.modelName = modelName
    return error
}

/** Parse a `nvpair-errors` `errors:update` / `errors:get-initial` payload. */
export function parseServiceErrors(value: JsonValue | undefined): ServiceError[] {
    if (!Array.isArray(value)) return []
    const errors: ServiceError[] = []
    for (const entry of value) {
        const error = parseServiceError(entry)
        if (error) errors.push(error)
    }
    return errors
}

/**
 * Per-node proxy "upstream unreachable" warnings duplicate the node list's
 * offline group, so they are never surfaced in the error UI. Backend id shape:
 * `<engine>-proxy:upstream-unreachable:<nodeId>`
 * (services/ollama-proxy/proxy.go `upstreamUnreachableID`).
 */
export function isUpstreamUnreachableError(error: ServiceError): boolean {
    return error.id.includes(':upstream-unreachable:')
}

const WORKLOAD_STATE_SET: ReadonlySet<string> = new Set(WorkloadStates)

function isWorkloadState(value: string): value is WorkloadState {
    return WORKLOAD_STATE_SET.has(value)
}

/** Parse a workload-manager `params.workloadInfo` into a typed Workload. */
function parseWorkload(value: JsonValue | undefined): Workload | null {
    const obj = objectValue(value)
    if (!obj) return null
    const id = stringValue(obj.id)
    if (!id) return null
    // The workload-manager stamps the engine-manager id (`lmstudio`); map it onto
    // our closed `EngineType` union (`lm-studio`) before narrowing so LM Studio
    // jobs are not silently dropped.
    const engine = engineManagerEngineType(stringValue(obj.engine))
    if (!engine) return null
    const stateValue = stringValue(obj.state)
    if (!isWorkloadState(stateValue)) return null

    const workload: Workload = {
        id,
        model: stringValue(obj.model),
        engine,
        state: stateValue,
        originatedFrom: nullableStringValue(obj.originatedFrom),
        createdAt: numberValue(obj.createdAt),
        startedAt: nullableNumberValue(obj.startedAt),
        completedAt: nullableNumberValue(obj.completedAt),
        error: nullableStringValue(obj.error),
        requesterId: nullableStringValue(obj.requesterId)
    }
    const scheduledOn = nullableStringValue(obj.scheduledOn)
    if (scheduledOn) workload.scheduledOn = scheduledOn
    return workload
}

/**
 * Parse a broker `workloads:get-initial` baseline (`{ workloads: workloadInfo[] }`,
 * ordered by createdAt). Each element is a bare `workloadInfo` — the same shape
 * carried inside a `workloads:upsert` `params.workloadInfo` — so it parses with
 * {@link parseWorkload} directly. Malformed entries are skipped.
 */
export function parseWorkloadsInitial(value: JsonValue | undefined): Workload[] {
    const obj = objectValue(value)
    const entries = Array.isArray(obj?.workloads) ? obj.workloads : []
    const workloads: Workload[] = []
    for (const entry of entries) {
        const workload = parseWorkload(entry)
        if (workload) workloads.push(workload)
    }
    return workloads
}

/** True for an engine fronted by a broker-supervised reverse proxy. */
export function isProxyEngine(engine: EngineType): engine is ProxyEngine {
    return engine === 'ollama' || engine === 'lm-studio'
}

const PENDING_OP_IDLE_TIMEOUT_MS = 90_000
// Vendor installers can be quiet for minutes; allow a bounded 30-minute window.
const INSTALL_PENDING_OP_IDLE_TIMEOUT_MS = 30 * 60_000

function pendingEngineOpIdleTimeoutMs(status: EngineProcessStatus): number {
    return status === 'installing' ? INSTALL_PENDING_OP_IDLE_TIMEOUT_MS : PENDING_OP_IDLE_TIMEOUT_MS
}

/**
 * Map a `nvpair-engine-manager` engine identifier onto our closed `EngineType`
 * union. The engine-manager uses `lmstudio`; we use `lm-studio`.
 */
function engineManagerEngineType(name: string): EngineType | null {
    const normalized = name === 'lmstudio' ? 'lm-studio' : name
    return isEngineType(normalized) ? normalized : null
}

/**
 * Inverse of {@link engineManagerEngineType} for the proxy engines: our closed
 * union uses `lm-studio`; engine-manager (and the `modelsByEngine` map keys) use
 * `lmstudio`. Ollama is spelled the same on both sides.
 */
function proxyEngineToManagerName(engine: ProxyEngine): string {
    return engine === 'lm-studio' ? 'lmstudio' : engine
}

/**
 * The set of models a node reports loaded in memory for an engine, read from
 * `ModularNode.loadedByEngine` (enriched from the backend's per-engine
 * `loadedByEngine`). Keyed by engine-manager name, so `lm-studio` maps
 * to `lmstudio`; any other type is spelled the same on both sides.
 */
function loadedNamesForEngine(node: ModularNode | undefined, engineType: EngineType): Set<string> {
    const managerName = engineType === 'lm-studio' ? 'lmstudio' : engineType
    return new Set(node?.loadedByEngine[managerName] ?? [])
}

function sameStringList(left: string[], right: string[]): boolean {
    if (left.length !== right.length) return false
    for (let index = 0; index < left.length; index += 1) {
        if (left[index] !== right[index]) return false
    }
    return true
}

/**
 * Order-insensitive comparison for de-duped string lists. mDNS browses report a
 * node's addresses (and TXT) in whatever order the responder returned them, so
 * an order-only change must NOT count as a node change.
 */
function sameStringSet(left: string[], right: string[]): boolean {
    if (left.length !== right.length) return false
    const set = new Set(left)
    for (const value of right) {
        if (!set.has(value)) return false
    }
    return true
}

/** Union two address lists, de-duped, keeping the first list's order. */
function mergeAddresses(existing: string[], next: string[]): string[] {
    const seen = new Set<string>()
    const out: string[] = []
    for (const addr of [...existing, ...next]) {
        if (!addr || seen.has(addr)) continue
        seen.add(addr)
        out.push(addr)
    }
    return out
}

/**
 * Every address a node is currently known to answer at: the broker's ranked
 * order first, then whatever only the proxy feed reported. The single effective
 * list — poll order, `NodeItem.allIpAddresses`, and `AvailableNode.ipAddresses`
 * all derive from it, so none of them can outlive a broker re-rank.
 */
function nodeAddresses(node: ModularNode): string[] {
    return mergeAddresses(node.brokerAddresses, node.proxyAddresses)
}

/**
 * The node's primary IP for display and node-info polling. Both discovery
 * sources now stamp `ModularNode.reachableAddress` with the backend's canonical
 * `netpick.Primary` LAN address (the broker's `AvailableNode.ipAddress` and the
 * proxy `node/*` `ip` field), so there is no local heuristic to fall
 * back to; `host` only covers a node that somehow carries no canonical address.
 */
function primaryNodeAddress(node: ModularNode): string {
    return node.reachableAddress || node.host
}

function mergeSources(sources: BrokerNodeSource[], source: BrokerNodeSource): BrokerNodeSource[] {
    return sources.includes(source) ? sources : [...sources, source]
}

function removeSource(sources: BrokerNodeSource[], source: BrokerNodeSource): BrokerNodeSource[] {
    return sources.filter(entry => entry !== source)
}

function nvidiaGpuRank(gpu: ModularGpu): number {
    return gpu.name.toLowerCase().includes('nvidia') ? 0 : 1
}

function gpuArrayValue(value: JsonValue | undefined): ModularGpu[] {
    if (!Array.isArray(value)) return []
    const gpus: ModularGpu[] = []
    for (const entry of value) {
        const obj = objectValue(entry)
        if (!obj) continue
        gpus.push({
            name: stringValue(obj.name),
            vramBytes: numberValue(obj.vram_bytes),
            vramUsedBytes: numberValue(obj.vram_used_bytes),
            utilizationPercent: numberValue(obj.utilization_percent)
        })
    }
    return gpus.sort((left, right) => nvidiaGpuRank(left) - nvidiaGpuRank(right))
}

function cpuValue(value: JsonValue | undefined): ModularCpu | null {
    const obj = objectValue(value)
    if (!obj) return null
    return {
        name: stringValue(obj.name),
        cores: numberValue(obj.cores),
        utilizationPercent: numberValue(obj.utilization_percent)
    }
}

function memoryValue(value: JsonValue | undefined): ModularMemory | null {
    const obj = objectValue(value)
    if (!obj) return null
    return {
        totalBytes: numberValue(obj.total_bytes),
        usedBytes: numberValue(obj.used_bytes)
    }
}

function normalizeLastSeen(value: number): number {
    if (value <= 0) return Date.now()
    return value < 10_000_000_000 ? value * 1000 : value
}

function sameGpu(left: ModularGpu, right: ModularGpu): boolean {
    return (
        left.name === right.name &&
        left.vramBytes === right.vramBytes &&
        left.vramUsedBytes === right.vramUsedBytes &&
        left.utilizationPercent === right.utilizationPercent
    )
}

function sameGpuList(left: ModularGpu[], right: ModularGpu[]): boolean {
    if (left.length !== right.length) return false
    for (let index = 0; index < left.length; index += 1) {
        if (!sameGpu(left[index], right[index])) return false
    }
    return true
}

function sameCpu(left: ModularCpu | null, right: ModularCpu | null): boolean {
    if (!left || !right) return left === right
    return (
        left.name === right.name &&
        left.cores === right.cores &&
        left.utilizationPercent === right.utilizationPercent
    )
}

function sameMemory(left: ModularMemory | null, right: ModularMemory | null): boolean {
    if (!left || !right) return left === right
    return left.totalBytes === right.totalBytes && left.usedBytes === right.usedBytes
}

function sameInferenceHardwareIds(left?: string[], right?: string[]): boolean {
    // Preserve absent (undefined) vs empty ([]) — they mean different things.
    if (!left || !right) return left === right
    return sameStringList(left, right)
}

function sameTelemetry(
    node: ModularNode,
    gpus: ModularGpu[],
    cpu: ModularCpu | null,
    memory: ModularMemory | null,
    inferenceHardwareIds: string[] | undefined
): boolean {
    return (
        sameGpuList(node.gpus, gpus) &&
        sameCpu(node.cpu, cpu) &&
        sameMemory(node.memory, memory) &&
        sameInferenceHardwareIds(node.inferenceHardwareIds, inferenceHardwareIds)
    )
}

function modelItem(name: string, loaded = false): ModelItem {
    return {
        name,
        size: 0,
        downloaded: true,
        status: loaded ? 'loaded' : 'idle',
        parameterSize: '',
        quantization: '',
        family: '',
        digest: '',
        sizeVram: null,
        expiresAt: null,
        expiry: '10m',
        capabilities: []
    }
}

function toNodeItem(node: ModularNode, selfId: string | null): NodeItem {
    const ipAddress = primaryNodeAddress(node)
    const addresses = nodeAddresses(node)
    // `NodeItem.port` is the node's primary reachable service port: a live
    // engine's promoted proxy port when one is up, otherwise the node-info HTTP
    // port. This keeps the value meaningful regardless of which discovery source
    // produced the node (a proxy reports the promoted proxy port, the broker
    // reports the node-info port).
    const port = primaryServicePort(node)
    return {
        id: node.id,
        // Display label: the hostname, else the reachable address; never the raw
        // UUID key (falls back to it only if a node somehow has neither).
        name: node.name || ipAddress || node.id,
        // The backend evicts mDNS nodes on its own (~60s of unbroken silence with
        // no inference traffic from the node either), but manual nodes persist and
        // report reachability via flags. Reflect that so an unreachable/sleeping
        // node drops into the UI's offline group instead of lingering as an active
        // card.
        status: anyEngineUp(node) || node.nodeInfoUp ? 'active' : 'offline',
        ipAddress,
        port,
        allIpAddresses: addresses.length > 0 ? addresses : [ipAddress],
        topology: {
            cpu: {
                model: node.cpu?.name ?? '',
                cores: node.cpu?.cores ?? 0,
                threads: node.cpu?.cores ?? 0
            },
            gpus: node.gpus.map((gpu, index) => ({
                id: `${node.id}:gpu:${index}`,
                name: gpu.name,
                vramTotal: gpu.vramBytes
            })),
            ram: node.memory?.totalBytes ?? 0,
            storage: [],
            inferenceHardwareIds: node.inferenceHardwareIds
        },
        // The local node's OS is known from the running process; the backend's
        // discovery/node-info plane reports no OS for remote nodes, so they fall
        // back to a placeholder. Remote OS is not reported by the current
        // discovery/node-info contract.
        os: node.id === selfId ? platformDisplayName(currentPlatform()) : 'Windows'
    }
}

function toMetrics(node: ModularNode): NodeItemMetrics {
    const memoryUsage =
        node.memory && node.memory.totalBytes > 0
            ? (node.memory.usedBytes / node.memory.totalBytes) * 100
            : 0
    const current = {
        timestamp: Date.now(),
        cpuUtilization: node.cpu?.utilizationPercent ?? 0,
        memoryUsage,
        gpuUtilization: node.gpus.map((gpu, index) => ({
            id: `${node.id}:gpu:${index}`,
            value: gpu.utilizationPercent
        })),
        gpuVramUsage: node.gpus.map((gpu, index) => ({
            id: `${node.id}:gpu:${index}`,
            value: gpu.vramBytes > 0 ? (gpu.vramUsedBytes / gpu.vramBytes) * 100 : 0
        }))
    }
    return { id: node.id, current, historical: [current] }
}

function toEngineStatus(
    node: ModularNode,
    engine: ProxyEngine,
    proxyPort: number | null
): EngineStatusData {
    const presence = node.engines[engine]
    const status: EngineStatusData = {
        engineType: engine,
        nodeId: node.id,
        processStatus: presence.up ? 'running' : 'stopped',
        // Discovery advertises the promoted proxy port, never the engine's own
        // (now loopback-private) port, so the engine port is unknown from a
        // proxy `node/*` event; it is filled only from engine-manager facts.
        enginePort: null,
        // Prefer an authoritative proxy port from the caller (the local broker's
        // bound port). Otherwise the port the peer advertised in discovery IS
        // its promoted proxy port for this engine — never a guess.
        proxyPort:
            proxyPort && proxyPort > 0
                ? proxyPort
                : presence.up && presence.port > 0
                  ? presence.port
                  : null
    }
    if (presence.version) {
        status.installedVersion = presence.version
    }
    return status
}

/**
 * Parse a proxy `node/*` payload for a given engine. The `ollama-proxy` and
 * `lmstudio-proxy` both emit the same `Node` shape (id/host/port/addresses/txt);
 * the engine is determined by which relay namespace it arrived on. The presence
 * is stamped onto that engine only; the other engine stays empty.
 */
function parseProxyNode(params: JsonValue | undefined, engine: ProxyEngine): ModularNode | null {
    const obj = objectValue(params)
    if (!obj) return null
    // The proxy keys `Node.ID` by the stable per-host UUID (and rejects empty),
    // so this is already the canonical node key — no TXT parsing needed.
    const id = stringValue(obj.id)
    if (!id) return null
    const engines = emptyEngines()
    engines[engine] = {
        up: true,
        // Under secure inference this is the peer's promoted inference proxy
        // port, not the engine's own (loopback-private) port.
        port: numberValue(obj.port),
        // The proxy `Node` carries no version field; engine version comes from
        // nvpair-engine-manager, not discovery.
        version: null
    }
    return {
        id,
        sources: [engine === 'ollama' ? 'ollama-proxy' : 'lmstudio-proxy'],
        // `Node.Host` is the hostname; empty for the self-bridge manual node,
        // in which case the broker discovery entry supplies the display name on
        // merge (see mergeNode). Never fall back to the UUID id here.
        name: stringValue(obj.name) || stringValue(obj.host),
        host: stringValue(obj.host),
        // The proxy `node/*` feed carries no trust/cluster flags; keep whatever
        // the broker last reported (see mergeNode).
        trusted: false,
        clustered: false,
        // The proxy `node/*` payload no longer carries a model list (models-http
        // moved it off the mDNS TXT to the broker's `AvailableNode.models`); a
        // proxy-sourced node contributes engine presence only, never models.
        models: [],
        modelsByEngine: {},
        // A proxy `node/*` event carries no loaded-model state either; the
        // broker-enriched value is preserved across the proxy merge (see mergeNode).
        loadedByEngine: {},
        nodeInfoPort: 0,
        nodeInfoUp: false,
        // A proxy `node/*` event carries no broker ranking; its `Addresses` are
        // this feed's own contribution and are kept behind the broker's list.
        brokerAddresses: [],
        proxyAddresses: stringArrayValue(obj.addresses),
        // The proxy stamps the node's canonical LAN address (netpick.Primary over
        // its TXT + addresses) on the `ip` field of every node/* payload,
        // so a proxy-only node carries a real reachable address instead
        // of forcing PAIR UI to re-rank the raw address list itself.
        reachableAddress: stringValue(obj.ip),
        gpus: [],
        cpu: null,
        memory: null,
        engines,
        lastSeen: Date.now()
    }
}

function parseBrokerNode(params: JsonValue | undefined): ModularNode | null {
    const obj = objectValue(params)
    if (!obj) return null
    // `AvailableNode.hostUuid` is the canonical node key. The broker's discovery
    // store rejects an empty UUID, so every node it reports carries one; a
    // payload without it is malformed and dropped rather than keyed by hostname.
    const hostUuid = stringValue(obj.hostUuid)
    if (!hostUuid) return null
    // `AvailableNode.id`/`.name` are the mDNS instance hostname (display only).
    const hostname = stringValue(obj.name) || stringValue(obj.id)
    const ipAddress = stringValue(obj.ipAddress) || stringValue(obj.ip_address)
    const port = numberValue(obj.port)
    // `AvailableNode.ipAddresses` is the node's own ranked candidate list, omitted
    // when it published a single address (where it would only repeat ipAddress).
    const ipAddresses = stringArrayValue(obj.ipAddresses)
    const brokerAddresses = ipAddresses.length > 0 ? ipAddresses : ipAddress ? [ipAddress] : []
    return {
        id: hostUuid,
        sources: ['broker'],
        name: hostname,
        host: ipAddress,
        // Broker discovery is authoritative for trust/cluster membership state.
        trusted: booleanValue(obj.trusted),
        clustered: booleanValue(obj.clustered),
        nodeInfoPort: port,
        nodeInfoUp: Boolean(ipAddress) && port > 0,
        brokerAddresses,
        proxyAddresses: [],
        // Broker `AvailableNode.ipAddress` is the head of the node's own ranked
        // candidate list — one canonical address shared across broker /
        // cluster-manager / errors / workload-manager, so the display IP and the
        // node-info poll target agree. The backend owns the ranking and the
        // failover across the rest of the list; PAIR never re-ranks.
        reachableAddress: ipAddress,
        gpus: [],
        cpu: null,
        memory: null,
        // Broker `AvailableNode.models`: the node's model list, enriched by its
        // daemon from the peer's engine-manager `/v1/models` (models-http). This
        // is the remote-node model source now that the proxy `models=` TXT is
        // retired. `omitempty` on the wire — absent when the node runs no engines.
        models: stringArrayValue(obj.models),
        // Broker `AvailableNode.modelsByEngine`: the same list attributed per
        // engine. `omitempty` on the wire — an empty object (no engines, or a
        // peer that predates per-engine attribution) drives the
        // {@link modelsForEngine} fallback to the flat union.
        modelsByEngine: parseModelsByEngine(obj.modelsByEngine),
        // Broker `AvailableNode.loadedByEngine`: the per-engine set of models
        // loaded in memory, enriched from the peer's engine-manager
        // `/v1/models`. `omitempty` on the wire — absent for a peer that reports
        // no loaded state, which stamps every model `idle`.
        loadedByEngine: parseModelsByEngine(obj.loadedByEngine),
        engines: emptyEngines(),
        lastSeen: normalizeLastSeen(numberValue(obj.lastSeen) || numberValue(obj.last_seen))
    }
}

function brokerNodesValue(value: JsonValue | undefined): ModularNode[] {
    const obj = objectValue(value)
    const entries = Array.isArray(value) ? value : Array.isArray(obj?.nodes) ? obj.nodes : []
    const nodes = new Map<string, ModularNode>()
    for (const entry of entries) {
        const node = parseBrokerNode(entry)
        if (node) nodes.set(node.id, node)
    }
    return Array.from(nodes.values())
}

function samePresence(left: EnginePresence, right: EnginePresence): boolean {
    return left.up === right.up && left.port === right.port && left.version === right.version
}

function sameEngines(
    left: Record<ProxyEngine, EnginePresence>,
    right: Record<ProxyEngine, EnginePresence>
): boolean {
    for (const engine of PROXY_ENGINES) {
        if (!samePresence(left[engine], right[engine])) return false
    }
    return true
}

function sameNode(left: ModularNode, right: ModularNode): boolean {
    return (
        left.id === right.id &&
        sameStringList(left.sources, right.sources) &&
        left.name === right.name &&
        left.host === right.host &&
        left.trusted === right.trusted &&
        left.clustered === right.clustered &&
        left.reachableAddress === right.reachableAddress &&
        left.nodeInfoPort === right.nodeInfoPort &&
        left.nodeInfoUp === right.nodeInfoUp &&
        // Order matters for the broker's list and only for it: that order is the
        // node's own ranking of where to reach it, and it drives the poll order.
        sameStringList(left.brokerAddresses, right.brokerAddresses) &&
        sameStringSet(left.proxyAddresses, right.proxyAddresses) &&
        sameTelemetry(left, right.gpus, right.cpu, right.memory, right.inferenceHardwareIds) &&
        sameStringList(left.models, right.models) &&
        sameModelsByEngine(left.modelsByEngine, right.modelsByEngine) &&
        // A residency-only change (a model loaded/evicted with the installed set
        // unchanged) must still count as a node change so the loaded dot updates.
        sameModelsByEngine(left.loadedByEngine, right.loadedByEngine) &&
        sameEngines(left.engines, right.engines) &&
        left.lastSeen === right.lastSeen
    )
}

/**
 * Compare two per-engine model maps. The attribution can change while the flat
 * union stays identical (e.g. a model that both engines now serve moved between
 * the per-engine buckets), so this must be checked independently of
 * {@link sameStringList} on `models` or a card would miss the re-attribution.
 */
function sameModelsByEngine(
    left: Record<string, string[]>,
    right: Record<string, string[]>
): boolean {
    const leftKeys = Object.keys(left)
    if (leftKeys.length !== Object.keys(right).length) return false
    for (const key of leftKeys) {
        if (!(key in right) || !sameStringList(left[key], right[key])) return false
    }
    return true
}

class ModularBridgeState {
    private nodes = new Map<string, ModularNode>()
    private brokerNodeIds = new Set<string>()
    private errors: ServiceError[] = []
    private workloads = new Map<string, Workload>()
    private logs: LogEntry[] = []
    // Per-engine bound proxy port reported by the broker. 0 = not reported yet;
    // we never fabricate a default — an unknown port surfaces as null, not a
    // guess. `ollama` is the `ollama-proxy`, `lm-studio` is the `lmstudio-proxy`.
    private proxyPorts: Record<ProxyEngine, number> = { ollama: 0, 'lm-studio': 0 }
    private selfId: string | null = null
    /**
     * Authoritative local-engine facts from `nvpair-engine-manager`, keyed by
     * engine type. Stored independent of `selfId` because engine-manager
     * reports before the self node is discovered, then resolved against
     * `selfId` at emit time.
     */
    private engineManagerFacts = new Map<
        EngineType,
        { installed: boolean; running: boolean; port: number }
    >()
    /**
     * Local model lists pulled from `nvpair-engine-manager`'s `list_models` action by
     * the supervisor. Cached so `engines:get-initial` can include them and survive
     * a UI refresh, just like the engine-manager lifecycle facts above. Stored for
     * every engine including the discovery engine (Ollama): for non-discovery
     * engines (LM Studio, …) it is the only model source; for Ollama it is the
     * complete authoritative list that {@link discoveryModelsForNode} prefers over
     * the (possibly TXT-truncated) mDNS-advertised list.
     */
    private localManagerModels = new Map<EngineType, ModelItem[]>()
    /** Supervisor-registered hook to re-pull the local discovery engine's
     *  authoritative `list_models` when discovery reports its model set changed. */
    private onLocalDiscoveryModelsChanged: ((engine: ProxyEngine) => void) | null = null
    /**
     * Optimistic transitional process status for a local engine while a
     * user-initiated lifecycle op is in flight. `nvpair-engine-manager` never emits
     * `starting`/`stopping`/`installing`/`uninstalling` — it only streams
     * `engine:install-progress` (install only) and one terminal
     * `engine:state-changed`. The UI renders its spinner/progress line solely for
     * these transitional statuses, so without synthesizing them here the UI sits
     * idle for the entire op. Held until the backend resolves the op (state
     * change, terminal progress, or matching error), with a safety timer.
     */
    private pendingEngineOps = new Map<EngineType, EngineProcessStatus>()
    private pendingOpTimers = new Map<EngineType, ReturnType<typeof setTimeout>>()
    /**
     * Optimistic transitional status for a remote peer while a user-initiated
     * `engine:remote-*` op is in flight on this node (the client). Keyed by
     * `${nodeId}:${engineType}`; cleared when discovery reports the resolved
     * run state or on idle timeout.
     */
    private pendingRemoteEngineOps = new Map<string, EngineProcessStatus>()
    private pendingRemoteOpTimers = new Map<string, ReturnType<typeof setTimeout>>()
    /**
     * In-flight model pulls keyed by {@link engineProgressKey}. The pull is
     * dispatched fire-and-forget, so we synthesize a `pulling` progress entry on
     * dispatch (so {@link IncomingSyncPullRow} renders immediately) and clear it
     * when the supervisor's awaited completion resolves. While the pull runs the
     * backend streams live `engine:pull-progress` frames that advance
     * this entry's percent in place. Held here too so `engines:get-initial` can
     * replay it after a UI refresh mid-download.
     */
    private activePulls = new Map<string, EngineProgress>()
    /**
     * Authoritative remote-engine facts keyed by {@link remoteOpKey}
     * (`${nodeId}:${engineType}`). Populated by the supervisor from each
     * clustered peer's `engine:remote-get-installed` (mTLS `ec` surface) — the
     * only source of a remote engine's real installed/running/port. When present
     * these override the best-effort mDNS presence in
     * {@link resolveRemoteEngineStatus}; when absent (an unclustered peer, or one
     * not polled yet) only an active advertisement is inferred from. Kept across a
     * failed poll — see `refreshRemoteEngineStatus`. Dropped when the node is
     * evicted.
     */
    private remoteEngineFacts = new Map<string, RemoteEngineFacts>()
    /**
     * The model of an in-flight remote pull keyed by {@link remoteOpKey}. The
     * backend's `engine:remote-progress` frames carry **no** `model` field
     * (`nvpair-engine-manager/remote.go` `remoteProgress`), so we stamp the model
     * we dispatched with here and backfill it onto every incoming frame — that
     * keeps the optimistic entry, the streamed percent updates, and the terminal
     * clear all on one {@link engineProgressKey}. Cleared by
     * {@link finishRemoteModelPull}.
     */
    private remotePullModels = new Map<string, string>()
    /**
     * The model of an in-flight local pull keyed by `engineType`. The backend's
     * `engine:pull-progress` frames (the local counterpart of
     * `engine:remote-progress`) carry **no** `model` field
     * (`nvpair-engine-manager/executor.go` `emitPullProgress`), so we stamp the
     * model we dispatched here and backfill it onto every incoming frame — one
     * active local pull per engine, matching the backend's engine-scoped
     * progress hub. Set by {@link beginModelPull}, cleared by
     * {@link finishModelPull}.
     */
    private localPullModels = new Map<EngineType, string>()
    /**
     * The authoritative set of live inbound invites awaiting the local user's PIN
     * entry, keyed by `inviteId`. The cluster-manager pushes `cluster:invite-received`
     * exactly once per invite (always `pending`) and has no list-pending RPC, so
     * main accumulates them here and prunes them as they resolve — via membership
     * (`nodes:changed`), the local accept/decline result, the receiver-side
     * `cluster:invite-canceled` / `cluster:invite-expired` pushes, or the per-invite
     * `cluster:invite-status` sweep (the backstop for a missed push). The backend
     * owns inbound-invite expiry, so there is no client-side TTL.
     * Every mutation re-emits the full `cluster:pending-invites-changed` snapshot.
     */
    private pendingInvites = new Map<string, Invite>()

    getNodesInitial(): WsInvokeResponse<'nodes:get-initial'> {
        const nodes: Record<string, NodeItem> = {}
        for (const node of Array.from(this.nodes.values())) {
            nodes[node.id] = toNodeItem(node, this.selfId)
        }
        return {
            nodes,
            fetchedNodes: true
        }
    }

    getSelfId(): string | null {
        return this.selfId
    }

    /**
     * The display hostname for a node key (UUID), or '' when unknown. Used as a
     * last-resort manual-removal key when address matching finds no persisted
     * entry (see {@link resolveManualNodeKey}).
     */
    getNodeHostname(nodeId: string): string {
        return this.nodes.get(nodeId)?.name ?? ''
    }

    /**
     * Every address a node key (UUID) is known to be reachable at — its
     * canonical `reachableAddress`, its discovered addresses, and its `host`.
     * Used to map a UUID back to the manual-nodes store entry (keyed by the
     * user-entered address) when removing a manual node. Empty for an unknown id.
     */
    getNodeAddresses(nodeId: string): string[] {
        const node = this.nodes.get(nodeId)
        if (!node) return []
        const out = new Set<string>()
        if (node.reachableAddress) out.add(node.reachableAddress)
        for (const address of nodeAddresses(node)) out.add(address)
        if (node.host) out.add(node.host)
        return Array.from(out)
    }

    /** The authoritative live inbound invites awaiting a PIN, oldest first. */
    getPendingInvites(): Invite[] {
        return Array.from(this.pendingInvites.values()).sort((a, b) => a.createdAt - b.createdAt)
    }

    /**
     * Record a freshly-arrived inbound invite (from `cluster:invite-received`).
     * Only `pending` invites are actionable; a non-pending arrival prunes any
     * stored copy instead. Re-emits the snapshot when the set changes.
     */
    addPendingInvite(invite: Invite): void {
        if (invite.state !== 'pending' || !invite.inviteId) {
            this.prunePendingInvite(invite.inviteId)
            return
        }
        this.pendingInvites.set(invite.inviteId, invite)
        this.emitPendingInvites()
    }

    /** Drop a single invite by id (resolved, evicted, or expired). */
    prunePendingInvite(inviteId: string): void {
        if (this.pendingInvites.delete(inviteId)) this.emitPendingInvites()
    }

    /**
     * Drop any pending invite whose inviter is now a confirmed `member` — the
     * pairing completed (`nodes:changed`), so the PIN prompt is obsolete.
     */
    reconcilePendingInvitesWithMembers(members: ClusterNode[]): void {
        if (this.pendingInvites.size === 0) return
        const memberUuids = new Set(members.filter(m => m.state === 'member').map(m => m.nodeUuid))
        let changed = false
        for (const invite of Array.from(this.pendingInvites.values())) {
            if (memberUuids.has(invite.fromNodeUuid)) {
                this.pendingInvites.delete(invite.inviteId)
                changed = true
            }
        }
        if (changed) this.emitPendingInvites()
    }

    private emitPendingInvites(): void {
        emitBridgePush('cluster:pending-invites-changed', this.getPendingInvites())
    }

    /**
     * Fix the local node id to the cluster-manager's stable node UUID
     * (`cluster:get-node-id` → `nodeUuid`). This is the sole self-identity
     * source: discovery and proxy ids are the same UUID key, so there is no
     * hostname guessing to guard against. A no-op when unchanged; a change
     * triggers a renderer refresh so per-node views re-resolve self.
     */
    setSelfId(nodeUuid: string): void {
        const trimmed = nodeUuid.trim()
        if (!trimmed || this.selfId === trimmed) return
        this.selfId = trimmed
        emitBridgePush('state:request-refresh', undefined)
    }

    /** Bound proxy port for an engine reported by the broker, or null if unknown. */
    getProxyPort(engine: ProxyEngine): number | null {
        const port = this.proxyPorts[engine]
        return port > 0 ? port : null
    }

    getEngineInitialState(): EngineInitialState {
        const statuses: EngineStatusData[] = []
        const models: EngineModels[] = []

        // Local engines (self): every engine `nvpair-engine-manager` reports,
        // proxy-fronted or not. engine-manager is authoritative for the local
        // node's install/run state; proxy-engine models prefer its cached
        // `list_models` and fall back to discovery. Emitted here (not in the
        // per-node loop) so they survive a UI refresh even before the self node
        // is discovered.
        const selfId = this.selfId
        for (const engineType of Array.from(this.engineManagerFacts.keys())) {
            const selfNode = selfId ? this.nodes.get(selfId) : undefined
            const status = this.resolveLocalEngineStatus(engineType, selfNode)
            if (status) statuses.push(status)
            if (!selfId) continue
            if (isProxyEngine(engineType)) {
                models.push(this.discoveryModelsForNode(selfNode, engineType, selfId))
            } else {
                const cached = this.localManagerModels.get(engineType)
                if (cached) {
                    const loaded = loadedNamesForEngine(selfNode, engineType)
                    models.push({
                        engineType,
                        nodeId: selfId,
                        models: cached.map(item => ({
                            ...item,
                            status: loaded.has(item.name) ? 'loaded' : 'idle'
                        }))
                    })
                }
            }
        }

        // Remote nodes: per proxy-engine status + models from discovery. Status
        // prefers the authoritative `engine:remote-get-installed` facts (mTLS)
        // over the inferred presence, and is omitted entirely when neither is
        // known — see {@link resolveRemoteEngineStatus}.
        for (const node of Array.from(this.nodes.values())) {
            if (node.id === selfId) continue
            for (const engine of PROXY_ENGINES) {
                const status = this.resolveRemoteEngineStatus(node.id, engine)
                if (status) statuses.push(status)
                models.push(this.toEngineModels(node, engine))
            }
        }

        return {
            statuses,
            models,
            activeProgress: Array.from(this.activePulls.values()),
            updateAvailable: []
        }
    }

    getAvailableNodes(): WsInvokeResponse<'discovery:get-nodes'> {
        return Array.from(this.nodes.values()).map(node => {
            const addresses = nodeAddresses(node)
            return {
                id: node.id,
                // Display label: hostname, else reachable address; never the raw UUID.
                name: node.name || primaryNodeAddress(node) || node.id,
                ipAddress: primaryNodeAddress(node),
                // Only for a genuinely multi-homed node: a one-entry list would just
                // repeat ipAddress.
                ...(addresses.length > 1 ? { ipAddresses: addresses } : {}),
                port: node.nodeInfoPort > 0 ? node.nodeInfoPort : primaryServicePort(node),
                lastSeen: node.lastSeen,
                // Invite eligibility: a node already in a cluster cannot be invited
                // into another; `trusted` marks an already-pinned peer.
                trusted: node.trusted,
                clustered: node.clustered
            }
        })
    }

    getNodeInfoPollTargets(): { id: string; hosts: string[]; port: number }[] {
        const targets: { id: string; hosts: string[]; port: number }[] = []
        for (const node of Array.from(this.nodes.values())) {
            if (!node.nodeInfoUp || node.nodeInfoPort <= 0) continue
            // This machine appears in its own discovery snapshot, but its own
            // node-info is on this machine: loopback reaches it without depending
            // on the host's inbound path. Loopback alone, with no failover — the
            // addresses it advertises are longer routes to the listener loopback
            // just reached, so asking them after loopback fails only spends
            // connections.
            if (node.id === this.selfId) {
                targets.push({
                    id: node.id,
                    hosts: [MODULAR_NODE_INFO_SELF_HOST],
                    port: node.nodeInfoPort
                })
                continue
            }
            // Lead with the canonical display address, then retain the node's
            // published alternatives. The poller walks them in order and verifies
            // hostUuid, so a direct-connect address that resolves to another host
            // cannot hide telemetry available at a later address.
            const hosts = mergeAddresses([primaryNodeAddress(node)], nodeAddresses(node))
            if (hosts.length === 0) continue
            targets.push({ id: node.id, hosts, port: node.nodeInfoPort })
        }
        return targets
    }

    mergeNodeInfoResponse(nodeId: string, response: JsonValue): void {
        const node = this.nodes.get(nodeId)
        if (!node) return

        const obj = objectValue(response)
        if (!obj) return

        // `/v1/node-info` now reports the node's own hostUuid. If it disagrees
        // with the node we polled (the address resolved to a different host),
        // skip the merge rather than attribute another host's telemetry here.
        const reportedUuid = stringValue(obj.hostUuid)
        if (reportedUuid && reportedUuid !== nodeId) return

        const gpus = gpuArrayValue(obj.GPUs)
        const cpu = cpuValue(obj.cpu)
        const memory = memoryValue(obj.memory)
        const inferenceHardwareIds = optionalStringArrayValue(obj.inference_hardware_ids)
        if (node.nodeInfoUp && sameTelemetry(node, gpus, cpu, memory, inferenceHardwareIds)) {
            emitBridgePush('metrics:update', toMetrics(node))
            return
        }

        this.upsertNode({
            ...node,
            gpus,
            cpu,
            memory,
            inferenceHardwareIds,
            nodeInfoUp: true
        })
    }

    getErrors(): ServiceError[] {
        return [...this.errors]
    }

    /**
     * Authoritative replace from `nvpair-errors` `errors:update`.
     *
     * Errors are pushed to whichever windows are open and are replayed from
     * `errors:get-initial` when Overview is opened later. They never raise or
     * focus a window: an error the user did not ask about must not interrupt
     * what they are doing.
     */
    replaceErrors(errors: ServiceError[]): void {
        this.errors = [...errors]
        emitBridgePush('errors:update', [...this.errors])
    }

    /** Local fallback upsert used only when `nvpair-errors` is not running. */
    upsertError(error: ServiceError): void {
        this.errors = [error, ...this.errors.filter(existing => existing.id !== error.id)].slice(
            0,
            200
        )
        emitBridgePush('errors:update', [...this.errors])
    }

    clearError(id: string): void {
        this.errors = this.errors.filter(error => error.id !== id)
        emitBridgePush('errors:update', [...this.errors])
    }

    getWorkloads(): WsInvokeResponse<'workloads:get-initial'> {
        const workloads: Record<string, Workload> = {}
        for (const [key, workload] of this.workloads) {
            workloads[key] = workload
        }
        return workloads
    }

    /**
     * Seed the catalog from a broker `workloads:get-initial` baseline and return
     * the full current map. Adds each entry (keyed by `(originatedFrom, id)`)
     * only when the key is absent, rather than clearing or overwriting: a live
     * `workloads:upsert` that already landed at that key (the realtime stream is
     * at least as fresh as this durable snapshot) is preserved, and the baseline
     * only fills in jobs the stream has not delivered yet. No push is emitted —
     * the caller (renderer / CLI) receives the seeded snapshot as the invoke
     * response and observes subsequent changes on the push stream.
     */
    seedWorkloads(workloads: Workload[]): WsInvokeResponse<'workloads:get-initial'> {
        for (const workload of workloads) {
            const key = workloadKey(workload.originatedFrom, workload.id)
            if (!this.workloads.has(key)) this.workloads.set(key, workload)
        }
        return this.getWorkloads()
    }

    /** Relay a broker `workloads:upsert` (`{ workloadInfo }`) into the catalog + UI. */
    upsertWorkloadFromInfo(params: JsonValue | undefined): void {
        const obj = objectValue(params)
        const workload = parseWorkload(obj?.workloadInfo)
        if (!workload) return
        this.workloads.set(workloadKey(workload.originatedFrom, workload.id), workload)
        emitBridgePush('workloads:upsert', workload)
    }

    /** Relay a broker `workloads:remove` (`{ workloadId, originatedFrom? }`) into the catalog + UI. */
    removeWorkloadFromParams(params: JsonValue | undefined): void {
        const obj = objectValue(params)
        const workloadId = stringValue(obj?.workloadId)
        if (!workloadId) return
        const originatedFrom = nullableStringValue(obj?.originatedFrom)
        this.workloads.delete(workloadKey(originatedFrom, workloadId))
        emitBridgePush('workloads:remove', { workloadId, originatedFrom })
    }

    /** Push a `workloads:remove` for an entry and drop it from the catalog. */
    private evictWorkload(key: string, workload: Workload): void {
        this.workloads.delete(key)
        emitBridgePush('workloads:remove', {
            workloadId: workload.id,
            originatedFrom: workload.originatedFrom
        })
    }

    /**
     * Drop every workload that touched a remote peer (originated on or scheduled
     * on a node other than this one). Called when the local node leaves a cluster
     * (clusterId cleared): the peers are gone, so their jobs must not linger in the
     * catalog or be re-served by `workloads:get-initial` on the next refresh.
     * Purely-local jobs (origin + schedule self/unset) are kept.
     */
    dropRemoteWorkloads(): void {
        const selfId = this.selfId
        const isRemoteNode = (nodeId: string | null | undefined): boolean =>
            nodeId !== null && nodeId !== undefined && nodeId !== selfId
        for (const [key, workload] of Array.from(this.workloads)) {
            if (!isRemoteNode(workload.originatedFrom) && !isRemoteNode(workload.scheduledOn)) {
                continue
            }
            this.evictWorkload(key, workload)
        }
    }

    /**
     * Empty the entire workload catalog. Called on backend restart to drop
     * anything that ended (or whose tracking proxy died) during the restart,
     * which would otherwise linger forever in this map. Emits a `workloads:remove`
     * per entry so both the renderer and CLI `workloads watch` clear immediately;
     * the supervisor then re-seeds from the broker's durable `workloads:get-initial`
     * baseline and the live `workloads:upsert` stream repopulates from there.
     */
    clearWorkloads(): void {
        for (const [key, workload] of Array.from(this.workloads)) {
            this.evictWorkload(key, workload)
        }
    }

    /**
     * Record a `nvpair-engine-manager` `engine:state-changed` (or hydrated
     * `engine:get-installed` entry). Facts are kept regardless of whether the
     * self node is known yet; `setSelfId` triggers a refresh once it resolves.
     */
    applyEngineManagerStatus(params: JsonValue | undefined): void {
        const obj = objectValue(params)
        if (!obj) return
        const engineType = engineManagerEngineType(stringValue(obj.engine))
        if (!engineType) return

        this.engineManagerFacts.set(engineType, {
            installed: booleanValue(obj.installed),
            running: booleanValue(obj.running),
            port: numberValue(obj.port)
        })
        // A fresh authoritative state is the resolution of whatever op was in
        // flight (start/stop done, install `done`+installed, uninstall removed).
        // Drop the optimistic status without re-emitting here — the emit below
        // already carries the resolved, fact-derived status.
        this.discardPendingOp(engineType)
        this.emitLocalEngineStatus(engineType)
    }

    /**
     * Begin an optimistic transitional status for a local-engine lifecycle op so
     * the UI shows a spinner / progress line immediately. The backend never emits
     * these intermediate states; it resolves the op later via
     * `engine:state-changed`, a terminal `engine:install-progress` stage, or a
     * matching `errors:report`, each of which clears the optimistic status.
     */
    beginLocalEngineOp(engineType: EngineType, status: EngineProcessStatus): void {
        if (!this.selfId) return
        this.pendingEngineOps.set(engineType, status)
        this.armPendingOpTimer(engineType, status)
        this.emitLocalEngineStatus(engineType)
    }

    /**
     * Clear a pending op when its lifecycle operation fails with an
     * `errors:report` that carries no `engine:state-changed` (LM Studio
     * start-command failure, uninstall failure, install failure). Called by the
     * supervisor's error demux. `operation` is the engine-manager error's
     * `operation` field (`start` | `install` | `uninstall`).
     */
    failLocalEngineOp(engineManagerEngine: string, operation: string): void {
        if (operation !== 'start' && operation !== 'install' && operation !== 'uninstall') return
        const engineType = engineManagerEngineType(engineManagerEngine)
        if (!engineType) return
        this.clearLocalEngineOp(engineType)
    }

    /**
     * Clear an optimistic op when a fire-and-forget lifecycle send never reaches
     * the backend (process down / write rejected), so no resolving
     * `engine:state-changed` or `errors:report` will ever arrive. Unlike
     * {@link failLocalEngineOp} this is keyed directly by `EngineType` and covers
     * every op (including `stopping`), since the caller already knows the engine.
     */
    clearPendingEngineOp(engineType: EngineType): void {
        this.clearLocalEngineOp(engineType)
    }

    private armPendingOpTimer(engineType: EngineType, status: EngineProcessStatus): void {
        const existing = this.pendingOpTimers.get(engineType)
        if (existing) clearTimeout(existing)
        this.pendingOpTimers.set(
            engineType,
            setTimeout(
                () => this.clearLocalEngineOp(engineType),
                pendingEngineOpIdleTimeoutMs(status)
            )
        )
    }

    /** Drop a pending op + its timer without emitting (caller emits). */
    private discardPendingOp(engineType: EngineType): void {
        const timer = this.pendingOpTimers.get(engineType)
        if (timer) {
            clearTimeout(timer)
            this.pendingOpTimers.delete(engineType)
        }
        this.pendingEngineOps.delete(engineType)
    }

    /** Drop a pending op and re-emit the now fact-derived status. */
    private clearLocalEngineOp(engineType: EngineType): void {
        if (!this.pendingEngineOps.has(engineType)) {
            this.discardPendingOp(engineType)
            return
        }
        this.discardPendingOp(engineType)
        this.emitLocalEngineStatus(engineType)
    }

    private remoteOpKey(nodeId: string, engineType: EngineType): string {
        return `${nodeId}:${engineType}`
    }

    /**
     * Whether a remote peer's proxy-engine is currently reported as running.
     * Mirrors {@link resolveRemoteEngineStatus}: authoritative facts are used
     * alone when present, and the coarser per-engine mDNS proxy presence
     * (`up`/`down`) is the fallback only for a peer with no facts, so the toggle
     * decision matches the displayed status.
     */
    isRemoteEngineRunning(nodeId: string, engineType: EngineType): boolean {
        if (!isProxyEngine(engineType)) return false
        const facts = this.remoteEngineFacts.get(this.remoteOpKey(nodeId, engineType))
        if (facts) return facts.running
        const node = this.nodes.get(nodeId)
        if (!node || node.clustered || node.trusted) return false
        return node.engines[engineType].up
    }

    beginRemoteEngineOp(nodeId: string, engineType: EngineType, status: EngineProcessStatus): void {
        const key = this.remoteOpKey(nodeId, engineType)
        this.pendingRemoteEngineOps.set(key, status)
        this.armPendingRemoteOpTimer(nodeId, engineType, status)
        this.emitRemoteEngineStatus(nodeId, engineType)
    }

    clearPendingRemoteEngineOp(nodeId: string, engineType: EngineType): void {
        this.clearRemoteEngineOp(nodeId, engineType)
    }

    beginRemoteModelPull(nodeId: string, engineType: EngineType, model: string): void {
        const node = this.nodes.get(nodeId)
        const progress: EngineProgress = {
            engineType,
            nodeId,
            nodeName: node?.name ?? nodeId,
            operation: 'pull',
            model,
            status: 'pulling'
        }
        // Remember the model so the model-less `engine:remote-progress` frames can
        // be re-associated with this exact entry (see remotePullModels).
        this.remotePullModels.set(this.remoteOpKey(nodeId, engineType), model)
        this.activePulls.set(engineProgressKey(progress), progress)
        emitBridgePush('engines:progress-changed', progress)
    }

    /** Whether an optimistic remote pull entry for this model is still in flight. */
    isRemoteModelPullActive(nodeId: string, engineType: EngineType, model: string): boolean {
        return this.activePulls.has(
            engineProgressKey({ nodeId, engineType, operation: 'pull', model })
        )
    }

    /**
     * Clear the optimistic remote pull entry once the awaited
     * `engine:remote-pull-model` RPC settles. The backend emits **no** terminal
     * `engine:remote-progress` for a pull (the peer's terminal stream frame
     * becomes the JSON-RPC reply, never a notification —
     * `nvpair-engine-manager/controlstream.go`), so this awaited-completion clear
     * is the only signal that removes the spinner.
     */
    finishRemoteModelPull(nodeId: string, engineType: EngineType, model: string): void {
        this.remotePullModels.delete(this.remoteOpKey(nodeId, engineType))
        const key = engineProgressKey({ nodeId, engineType, operation: 'pull', model })
        if (!this.activePulls.delete(key)) return
        emitBridgePush('engines:progress-cleared', { key })
    }

    applyRemoteEngineProgress(params: JsonValue | undefined): void {
        const obj = objectValue(params)
        if (!obj) return
        const nodeId = stringValue(obj.node)
        const engineType = engineManagerEngineType(stringValue(obj.engine))
        if (!nodeId || !engineType) return

        const op = stringValue(obj.op)
        const operation = op === 'pull' || op === 'pull_model' ? 'pull' : 'install'
        const stage = stringValue(obj.stage) || 'working'
        // `engine:remote-progress` carries no `model`, so for a pull we backfill
        // the model captured at dispatch — otherwise the frame would emit under a
        // different key than the optimistic entry and never update/clear it.
        const model =
            stringValue(obj.model) ||
            (operation === 'pull'
                ? (this.remotePullModels.get(this.remoteOpKey(nodeId, engineType)) ?? '')
                : '')

        if (stage === 'done' || stage === 'already-installed' || stage === 'failed') {
            const progressKey =
                operation === 'pull' && model
                    ? engineProgressKey({ nodeId, engineType, operation: 'pull', model })
                    : `${nodeId}:${engineType}:${operation}`
            emitBridgePush('engines:progress-cleared', { key: progressKey })
            if (operation === 'pull') {
                this.remotePullModels.delete(this.remoteOpKey(nodeId, engineType))
                if (model) {
                    this.activePulls.delete(
                        engineProgressKey({ nodeId, engineType, operation: 'pull', model })
                    )
                }
            }
            if (stage === 'already-installed' || stage === 'failed') {
                this.clearRemoteEngineOp(nodeId, engineType)
            }
            if (stage === 'done' && operation === 'install') {
                this.clearRemoteEngineOp(nodeId, engineType)
            }
            return
        }

        if (
            operation === 'install' &&
            this.pendingRemoteEngineOps.get(this.remoteOpKey(nodeId, engineType)) !== 'installing'
        ) {
            this.pendingRemoteEngineOps.set(this.remoteOpKey(nodeId, engineType), 'installing')
            this.emitRemoteEngineStatus(nodeId, engineType)
        }
        if (operation === 'install') {
            this.armPendingRemoteOpTimer(nodeId, engineType, 'installing')
        }

        const node = this.nodes.get(nodeId)
        const pullKey =
            operation === 'pull' && model
                ? engineProgressKey({ nodeId, engineType, operation: 'pull', model })
                : ''
        const existingPull = pullKey ? this.activePulls.get(pullKey) : undefined
        const framePercent = numberValue(obj.percent)
        const percent =
            operation === 'pull'
                ? mergePullProgressPercent(framePercent, existingPull?.percent)
                : framePercent
        const progress: EngineProgress = {
            engineType,
            nodeId,
            nodeName: node?.name ?? nodeId,
            operation,
            status: stage,
            percent,
            model: model || undefined
        }
        if (operation === 'pull' && model) {
            this.activePulls.set(engineProgressKey(progress), progress)
        }
        emitBridgePush('engines:progress-changed', progress)
    }

    private armPendingRemoteOpTimer(
        nodeId: string,
        engineType: EngineType,
        status: EngineProcessStatus
    ): void {
        const key = this.remoteOpKey(nodeId, engineType)
        const existing = this.pendingRemoteOpTimers.get(key)
        if (existing) clearTimeout(existing)
        this.pendingRemoteOpTimers.set(
            key,
            setTimeout(
                () => this.clearRemoteEngineOp(nodeId, engineType),
                pendingEngineOpIdleTimeoutMs(status)
            )
        )
    }

    private discardPendingRemoteOp(nodeId: string, engineType: EngineType): void {
        const key = this.remoteOpKey(nodeId, engineType)
        const timer = this.pendingRemoteOpTimers.get(key)
        if (timer) {
            clearTimeout(timer)
            this.pendingRemoteOpTimers.delete(key)
        }
        this.pendingRemoteEngineOps.delete(key)
    }

    private clearRemoteEngineOp(nodeId: string, engineType: EngineType): void {
        if (!this.pendingRemoteEngineOps.has(this.remoteOpKey(nodeId, engineType))) {
            this.discardPendingRemoteOp(nodeId, engineType)
            return
        }
        this.discardPendingRemoteOp(nodeId, engineType)
        this.emitRemoteEngineStatus(nodeId, engineType)
    }

    private maybeClearRemotePendingFromDiscovery(
        nodeId: string,
        engine: ProxyEngine,
        node: ModularNode
    ): void {
        const pending = this.pendingRemoteEngineOps.get(this.remoteOpKey(nodeId, engine))
        if (!pending) return
        const up = node.engines[engine].up
        if (pending === 'starting' && up) {
            this.discardPendingRemoteOp(nodeId, engine)
        } else if (pending === 'stopping' && !up) {
            this.discardPendingRemoteOp(nodeId, engine)
        } else if (pending === 'installing' && up) {
            this.discardPendingRemoteOp(nodeId, engine)
        }
    }

    private emitRemoteEngineStatus(nodeId: string, engineType: EngineType): void {
        if (!isProxyEngine(engineType)) return
        if (!this.nodes.has(nodeId)) return
        // Re-emit models alongside status: authoritative facts arriving here can
        // flip which engine is the single active one, which changes model
        // attribution ({@link modelsForEngine}). Without this the list would stay
        // stale until the next node-change event. When install/run state is
        // unknown, push `initializing` so a prior inferred `stopped` is retracted
        // from the renderer store (the compact row hides that placeholder).
        emitBridgePush('engines:state-changed', {
            nodeId,
            engineType,
            status: this.remoteEngineStatusForPush(nodeId, engineType),
            models: this.discoveryModelsForNode(this.nodes.get(nodeId), engineType, nodeId)
        })
    }

    /**
     * Status to publish for a remote peer engine. Returns an `initializing`
     * placeholder when nothing is known so stale inferred state is cleared
     * without asserting installed-but-off.
     */
    private remoteEngineStatusForPush(nodeId: string, engine: ProxyEngine): EngineStatusData {
        const status = this.resolveRemoteEngineStatus(nodeId, engine)
        return status ?? emptyEngineStatus(nodeId, engine)
    }

    /**
     * Build a remote peer's engine status, in precedence order:
     *   1. an in-flight transitional op (`starting`/`installing`/…) — the
     *      optimistic spinner while a user action is pending;
     *   2. authoritative `engine:remote-get-installed` facts (mTLS `ec`) — the
     *      only source of a peer's real installed/not-installed/port;
     *   3. best-effort per-engine mDNS proxy presence — all an
     *      unclustered/unreachable peer can offer.
     * Facts decide install/run state and the engine port. Proxy presence is
     * per-engine (each relay adds/removes its peer as that engine becomes
     * reachable), but it is coarser than facts: it only says `up`/`down`, so it
     * cannot tell `stopped` from `not-installed` and never carries the engine's
     * own port. Facts are the peer engine-manager's own truth, so they win
     * outright for status/engine-port; an advertisement with no facts is still
     * positive evidence of a running engine. A just-started engine lands
     * immediately because {@link applyRemoteEngineStatusResult} writes the toggle
     * RPC's `EngineStatus` into facts, and the `pendingRemoteEngineOps` overlay
     * covers the gap before that.
     *
     * Returns **null** when nothing is known: no facts and no advertisement. An
     * absent advertisement is not evidence — it cannot distinguish a stopped
     * engine from one that was never installed — so no status is asserted and
     * the renderer renders the peer engine as unavailable rather than as an
     * installed-but-off toggle.
     *
     * The peer's promoted **proxy** port IS carried in discovery (the `ol`/`lm`
     * advertisement points at the proxy), so it is surfaced as `proxyPort` from
     * that per-engine presence regardless of facts.
     * The peer's engine port stays private (loopback) and comes only from facts;
     * the LOCAL proxy port is never attributed to a peer.
     */
    private resolveRemoteEngineStatus(
        nodeId: string,
        engine: ProxyEngine
    ): EngineStatusData | null {
        const node = this.nodes.get(nodeId)
        const presence = node?.engines[engine]
        const facts = this.remoteEngineFacts.get(this.remoteOpKey(nodeId, engine))
        const pending = this.pendingRemoteEngineOps.get(this.remoteOpKey(nodeId, engine))

        let base: EngineStatusData | null = null
        if (facts) {
            base = {
                engineType: engine,
                nodeId,
                processStatus: facts.running
                    ? 'running'
                    : facts.installed
                      ? 'stopped'
                      : 'not-installed',
                // Only the peer engine-manager's own port is a real engine port;
                // the discovery presence port is the proxy, so never fall back to
                // it for the engine port.
                enginePort: facts.installed && facts.port > 0 ? facts.port : null,
                // The peer's promoted proxy port, when its engine is advertised.
                proxyPort: presence?.up && presence.port > 0 ? presence.port : null
            }
        } else if (node && presence?.up && !node.clustered && !node.trusted) {
            // A live advertisement means the engine is reachable and serving, so
            // `running` is sound even without facts — for an unclustered peer that
            // cannot be polled over ec. Clustered / pinned members must wait for
            // `engine:remote-get-installed` facts so a routable proxy overlay does
            // not masquerade as install/run truth (and cannot distinguish stopped
            // from not-installed).
            base = toEngineStatus(node, engine, null)
        }
        if (base && presence?.version) base.installedVersion = presence.version

        if (!pending) return base
        // A user-initiated remote op shows its optimistic status even while the
        // underlying state is still unknown.
        return base
            ? { ...base, processStatus: pending }
            : {
                  engineType: engine,
                  nodeId,
                  processStatus: pending,
                  enginePort: null,
                  proxyPort: null
              }
    }

    /**
     * Apply a peer's full `engine:remote-get-installed` response
     * (`{ engines: EngineStatus[] }` from its `ec` `/v1/engines`). Stores facts
     * for each proxy engine and re-emits status so installed/running/port reflect
     * the peer's own truth rather than inferred presence.
     */
    applyRemoteEngineFacts(nodeId: string, result: JsonValue | undefined): void {
        const obj = objectValue(result)
        const list = obj?.engines
        if (!Array.isArray(list)) return
        const seen = new Set<ProxyEngine>()
        for (const entry of list) {
            const engineObj = objectValue(entry)
            if (!engineObj) continue
            const engineType = engineManagerEngineType(stringValue(engineObj.engine))
            if (!engineType || !isProxyEngine(engineType)) continue
            this.remoteEngineFacts.set(this.remoteOpKey(nodeId, engineType), {
                installed: booleanValue(engineObj.installed),
                running: booleanValue(engineObj.running),
                healthy: booleanValue(engineObj.healthy),
                port: numberValue(engineObj.port)
            })
            seen.add(engineType)
        }
        for (const engine of PROXY_ENGINES) {
            if (!seen.has(engine)) {
                this.remoteEngineFacts.delete(this.remoteOpKey(nodeId, engine))
            }
            this.emitRemoteEngineStatus(nodeId, engine)
        }
    }

    /**
     * Apply the single `EngineStatus` returned by `engine:remote-start` /
     * `engine:remote-stop`, so a toggled remote engine's resolved run state lands
     * immediately instead of waiting on the next facts poll or mDNS flip.
     */
    applyRemoteEngineStatusResult(
        nodeId: string,
        engineType: EngineType,
        result: JsonValue | undefined
    ): void {
        if (!isProxyEngine(engineType)) return
        const obj = objectValue(result)
        if (!obj) return
        this.remoteEngineFacts.set(this.remoteOpKey(nodeId, engineType), {
            installed: booleanValue(obj.installed),
            running: booleanValue(obj.running),
            healthy: booleanValue(obj.healthy),
            port: numberValue(obj.port)
        })
        this.emitRemoteEngineStatus(nodeId, engineType)
    }

    /** Forget a node's authoritative remote-engine facts (on eviction/leave). */
    private dropRemoteEngineFacts(nodeId: string): void {
        for (const engine of PROXY_ENGINES) {
            this.remoteEngineFacts.delete(this.remoteOpKey(nodeId, engine))
            this.remotePullModels.delete(this.remoteOpKey(nodeId, engine))
        }
    }

    /**
     * Resolve which local engine (if any) needs its models pulled from
     * engine-manager's `list_models` action for a given engine-manager engine
     * id. Returns null only for unknown engines — **the discovery engine (Ollama)
     * is included.** Its mDNS advertisement carries no model list (the proxy
     * `node/*` TXT is empty), so engine-manager's `/api/tags` is the only working
     * source for the local node's Ollama models. `setLocalEngineModels` scopes the
     * result to the local node, so remote nodes keep their discovery-derived lists
     * — no double-sourcing.
     */
    modelPullTarget(engineManagerEngine: string): EngineType | null {
        return engineManagerEngineType(engineManagerEngine)
    }

    /**
     * Registered by the supervisor. Invoked with the affected proxy engine when
     * discovery reports the local node's model set for that engine changed, so the
     * supervisor can re-pull the complete, authoritative `list_models` (TXT can
     * truncate; a pull/delete on an already-running engine emits no
     * `engine:state-changed`).
     */
    setLocalDiscoveryModelRefresher(fn: (engine: ProxyEngine) => void): void {
        this.onLocalDiscoveryModelsChanged = fn
    }

    /**
     * Cache and push the local node's model list for a non-discovery engine.
     * Called by the supervisor after a `list_models` pull (or with an empty list
     * when the engine is stopped, since its HTTP `list_models` is unreachable).
     */
    setLocalEngineModels(engineType: EngineType, modelNames: string[]): void {
        const nodeId = this.selfId
        // Stamp `'loaded'` from the self node's loaded set so a `list_models`
        // refresh (pull/lifecycle) preserves residency instead of resetting every
        // row to idle. The loaded set is seeded by discovery self-enrichment and
        // kept fresh by {@link applyLocalLoadedModels}.
        const loaded = loadedNamesForEngine(nodeId ? this.nodes.get(nodeId) : undefined, engineType)
        const items = modelNames.map(name => modelItem(name, loaded.has(name)))
        this.localManagerModels.set(engineType, items)
        if (!nodeId) return
        emitBridgePush('engines:state-changed', {
            nodeId,
            engineType,
            models: { engineType, nodeId, models: items }
        })
    }

    /**
     * Update the local node's per-engine loaded-in-memory model set from an
     * `engine:models-changed` push and re-publish the affected local
     * engines, so an explicit load/unload, LM Studio JIT auto-load, or TTL/idle
     * eviction reflects immediately instead of waiting for the next discovery
     * model-refresh sweep. The push carries the full `engine:models` snapshot, so
     * the whole loaded map is replaced: an engine absent from `loadedByEngine` is
     * not running / not queryable and therefore has nothing loaded. Discovery
     * self-enrichment (`AvailableNode.loadedByEngine` over the loopback
     * `/v1/models`) remains the seed and backstop until the self node is
     * discovered.
     */
    applyLocalLoadedModels(loadedByEngine: Record<string, string[]>): void {
        const nodeId = this.selfId
        if (!nodeId) return
        const node = this.nodes.get(nodeId)
        // Before the self node is discovered there is nothing to stamp against;
        // discovery seeds the loaded set when the node arrives.
        if (!node) return
        if (sameModelsByEngine(node.loadedByEngine, loadedByEngine)) return
        const next: ModularNode = { ...node, loadedByEngine }
        this.nodes.set(nodeId, next)
        // Loaded state feeds only the engine model rows (node identity, metrics,
        // and the invite-only discovery snapshot are unaffected), so re-emit just
        // the per-engine model patches.
        for (const engine of PROXY_ENGINES) {
            const status =
                this.resolveLocalEngineStatus(engine, next) ??
                toEngineStatus(next, engine, this.getProxyPort(engine))
            emitBridgePush('engines:state-changed', {
                nodeId,
                engineType: engine,
                status,
                models: this.discoveryModelsForNode(next, engine, nodeId)
            })
        }
    }

    /**
     * Drop the local node's cached engine-manager model override for a proxy
     * engine and re-publish the discovery-derived list. Used when engine-manager
     * cannot supply an initial `list_models` inventory (for example an external
     * engine it has not adopted), or when a stopped-state empty sentinel must be
     * released after restart. Clearing the override lets discovery be the source
     * of truth rather than blanking the UI with an empty list.
     */
    fallbackLocalEngineModelsToDiscovery(engine: ProxyEngine): void {
        this.localManagerModels.delete(engine)
        const nodeId = this.selfId
        if (!nodeId) return
        const models = this.discoveryModelsForNode(this.nodes.get(nodeId), engine, nodeId)
        emitBridgePush('engines:state-changed', {
            nodeId,
            engineType: engine,
            models
        })
    }

    /**
     * Whether an engine is "active" on a node for the purpose of model
     * attribution, resolved from the authoritative engine-manager running fact
     * (`engineManagerFacts` for the local node, `remoteEngineFacts` from
     * `engine:remote-get-installed`/mTLS for a clustered peer) with best-effort
     * mDNS proxy `up` presence used only as a fallback.
     *
     * Remote peers prefer facts over proxy presence for attribution. Presence is
     * per-engine but coarse — it only says the peer is reachable for that engine,
     * not which engine actually serves a given model. For a pre-attribution peer
     * that sends no `modelsByEngine`, whenever both engines are momentarily `up`,
     * leaning on presence makes two engines look "active" at once, which defeats
     * the single-active-engine attribution in {@link modelsForEngine} and blanks
     * the model list. So once a peer's authoritative facts exist they are the sole
     * truth; presence is used only when facts are absent (an unclustered peer, or
     * one that has not been polled yet).
     *
     * The local node keeps the union: its models come from the `localManagerModels`
     * cache in {@link discoveryModelsForNode}, and the presence fallback still
     * preserves attribution for an externally-running engine that engine-manager
     * has not adopted (fact `running: false`) but the daemon enriched with models.
     */
    private engineActiveForModels(node: ModularNode, engine: ProxyEngine): boolean {
        const presenceUp = node.engines[engine].up
        if (node.id === this.selfId) {
            return (this.engineManagerFacts.get(engine)?.running ?? false) || presenceUp
        }
        const facts = this.remoteEngineFacts.get(this.remoteOpKey(node.id, engine))
        return facts ? facts.running : presenceUp
    }

    /**
     * Attribute a node's model list to one proxy engine. The backend's per-engine
     * breakdown (`AvailableNode.modelsByEngine`) is authoritative when present,
     * so a dual-engine node attributes each model to the engine that serves it.
     * A pre-attribution peer sends no map, so the flat `AvailableNode.models`
     * union is attributed only when exactly one proxy engine is active (never
     * cross-attributed when both run). Mirrors
     * noderec.DirectoryNode.EngineModels.
     */
    private modelsForEngine(node: ModularNode, engine: ProxyEngine): string[] {
        // Per-engine attribution is authoritative when the node carries any:
        // return exactly this engine's models so a dual-engine node attributes
        // each model to the engine that serves it, and a missing key means this
        // engine serves none (NOT the cross-engine union). Mirrors
        // noderec.DirectoryNode.EngineModels.
        if (Object.keys(node.modelsByEngine).length > 0) {
            return node.modelsByEngine[proxyEngineToManagerName(engine)] ?? []
        }
        // Fallback for a pre-attribution peer: the flat union is unattributed, so
        // attribute it only when exactly one proxy engine is active on the node,
        // and never cross-attribute when both run.
        const active = PROXY_ENGINES.filter(candidate =>
            this.engineActiveForModels(node, candidate)
        )
        return active.length === 1 && active[0] === engine ? node.models : []
    }

    private toEngineModels(node: ModularNode, engine: ProxyEngine): EngineModels {
        const loaded = loadedNamesForEngine(node, engine)
        return {
            engineType: engine,
            nodeId: node.id,
            models: this.modelsForEngine(node, engine).map(name =>
                modelItem(name, loaded.has(name))
            )
        }
    }

    /**
     * Proxy-engine models for a node. The broker daemon enriches each node's
     * model list from its engine-manager `/v1/models` (models-http) onto
     * `AvailableNode.models`, which {@link parseBrokerNode} stamps node-level and
     * {@link toEngineModels} attributes to a uniquely-active proxy engine — so
     * discovery is a live, refresh-safe source for every node.
     *
     * For the **local** node we prefer engine-manager `list_models` whenever a
     * successful result is cached, including an explicit empty list. Cache
     * absence means unknown and falls back to discovery (for example, an
     * externally-running Ollama that engine-manager has not adopted). This
     * presence check prevents a last-model deletion from resurrecting stale
     * discovery data. The cache is re-pulled whenever discovery reports the
     * local model set changed (see {@link upsertNode}).
     */
    private discoveryModelsForNode(
        node: ModularNode | undefined,
        engine: ProxyEngine,
        nodeId: string
    ): EngineModels {
        if (nodeId === this.selfId) {
            const cached = this.localManagerModels.get(engine)
            if (cached !== undefined) {
                // The cached list is the authoritative `list_models` set (names
                // only); stamp `'loaded'` from the self node's discovery/push
                // loaded set so the local card reflects in-memory residency too.
                const loaded = loadedNamesForEngine(node, engine)
                return {
                    engineType: engine,
                    nodeId,
                    models: cached.map(item => ({
                        ...item,
                        status: loaded.has(item.name) ? 'loaded' : 'idle'
                    }))
                }
            }
        }
        if (!node) return { engineType: engine, nodeId, models: [] }
        return this.toEngineModels(node, engine)
    }

    /**
     * Resolve the local node's status for an engine. `nvpair-engine-manager` is the
     * sole authority on the local engine's install/running state — we do NOT let
     * a proxy's discovery `up` flag override it, so the UI's run state always
     * matches what the toggle (`engine:status`) decides on. An externally-running
     * engine the manager hasn't adopted therefore shows as `stopped` until the
     * user starts it (which adopts it); discovery only contributes models. The
     * backend has no managed/external distinction, so we never synthesize
     * `installType` — it reports install/uninstall/stop failures via
     * errors:report.
     */
    private resolveLocalEngineStatus(
        engineType: EngineType,
        node: ModularNode | undefined
    ): EngineStatusData | undefined {
        const nodeId = this.selfId
        if (!nodeId) return undefined
        const facts = this.engineManagerFacts.get(engineType)

        // An in-flight lifecycle op outranks the fact-derived status so the UI
        // shows the spinner/progress immediately; the op is dropped the moment
        // the backend reports a resolution.
        const pending = this.pendingEngineOps.get(engineType)
        if (pending) {
            return {
                engineType,
                nodeId,
                processStatus: pending,
                enginePort: facts && facts.running && facts.port > 0 ? facts.port : null,
                proxyPort: isProxyEngine(engineType) ? this.getProxyPort(engineType) : null
            }
        }

        if (facts) {
            return {
                engineType,
                nodeId,
                processStatus: facts.running
                    ? 'running'
                    : facts.installed
                      ? 'stopped'
                      : 'not-installed',
                // The engine-manager reports the manifest's configured port for a
                // pinned-port engine (Ollama 11434, managed LM Studio 1235 behind
                // its 1234 proxy facade) whether it is running or merely installed,
                // so surface it in both states —
                // matching the `EngineStatusData.enginePort` contract. Auto-assign
                // engines report 0 until started, which stays null.
                enginePort: facts.installed && facts.port > 0 ? facts.port : null,
                // Each proxy-fronted engine has its own broker proxy
                // (`ollama-proxy` / `lmstudio-proxy`); report that engine's bound
                // proxy port. Loopback-only engines get null.
                proxyPort: isProxyEngine(engineType) ? this.getProxyPort(engineType) : null
            }
        }

        return node && isProxyEngine(engineType)
            ? toEngineStatus(node, engineType, this.getProxyPort(engineType))
            : undefined
    }

    /** Push a fresh `engines:state-changed` for one local engine. */
    private emitLocalEngineStatus(engineType: EngineType): void {
        const nodeId = this.selfId
        if (!nodeId) return
        const status = this.resolveLocalEngineStatus(engineType, this.nodes.get(nodeId))
        if (!status) return
        emitBridgePush('engines:state-changed', { nodeId, engineType, status })
    }

    /** Project a `nvpair-engine-manager` `engine:install-progress` for the local node. */
    applyEngineManagerProgress(params: JsonValue | undefined): void {
        const nodeId = this.selfId
        if (!nodeId) return
        const obj = objectValue(params)
        if (!obj) return
        const engineType = engineManagerEngineType(stringValue(obj.engine))
        if (!engineType) return

        const stage = stringValue(obj.stage) || 'installing'

        // nvpair-engine-manager emits these terminal stages (see install.go):
        //   done / already-installed -> success, failed -> error (percent -1).
        // On any terminal stage we clear the ephemeral progress entry.
        if (stage === 'done' || stage === 'already-installed' || stage === 'failed') {
            emitBridgePush('engines:progress-cleared', {
                key: `${nodeId}:${engineType}:install`
            })
            // `done` is immediately followed by engine:state-changed
            // (installed:true), which clears the optimistic `installing` status
            // and emits the resolved one. `already-installed` and `failed` emit
            // NO state-changed, so we must clear the optimistic status here or
            // the spinner sticks.
            if (stage === 'already-installed' || stage === 'failed') {
                this.clearLocalEngineOp(engineType)
            }
            return
        }

        // Reflect any in-flight install as a transitional status — even one
        // started outside our UI (backend auto-resume) — so the progress line
        // renders. The UI only shows progress when processStatus is transitional.
        if (this.pendingEngineOps.get(engineType) !== 'installing') {
            this.pendingEngineOps.set(engineType, 'installing')
            this.emitLocalEngineStatus(engineType)
        }
        this.armPendingOpTimer(engineType, 'installing')

        const node = this.nodes.get(nodeId)
        const progress: EngineProgress = {
            engineType,
            nodeId,
            nodeName: node?.name ?? nodeId,
            operation: 'install',
            status: stage,
            percent: numberValue(obj.percent)
        }
        emitBridgePush('engines:progress-changed', progress)
    }

    /**
     * Emit an optimistic `pulling` progress entry for a model the user just asked
     * to download. This indeterminate entry makes the UI's spinner row appear the
     * instant the modal closes instead of looking like nothing happened; the
     * backend then streams live percent via `engine:pull-progress`
     * ({@link applyLocalEngineProgress}), which updates this same entry
     * in place. Cleared by {@link finishModelPull} when the supervisor's awaited
     * pull resolves.
     */
    beginModelPull(engineType: EngineType, model: string): void {
        const nodeId = this.selfId
        if (!nodeId) return
        const node = this.nodes.get(nodeId)
        const progress: EngineProgress = {
            engineType,
            nodeId,
            nodeName: node?.name ?? nodeId,
            operation: 'pull',
            model,
            status: 'pulling'
        }
        // Remember the model so the model-less `engine:pull-progress` frames can
        // be re-associated with this exact entry (see localPullModels).
        this.localPullModels.set(engineType, model)
        this.activePulls.set(engineProgressKey(progress), progress)
        emitBridgePush('engines:progress-changed', progress)
    }

    /**
     * Project a `nvpair-engine-manager` `engine:pull-progress` for the local node
     * — the local counterpart of `engine:remote-progress`: live
     * download progress for a model pull driven via `engine:action`
     * `pull_model`. The frame carries `{ engine, op, stage, percent, message }`
     * and **no** `model`, so we backfill the model captured at dispatch and
     * refresh the optimistic entry in place. Clearing stays with the awaited pull
     * ({@link finishModelPull}), so this only advances percent/stage: it ignores
     * the terminal error sentinel (`percent: -1`) and the non-download frames'
     * `percent: 0` so the rendered bar never jumps backwards.
     */
    applyLocalEngineProgress(params: JsonValue | undefined): void {
        const nodeId = this.selfId
        if (!nodeId) return
        const obj = objectValue(params)
        if (!obj) return
        const engineType = engineManagerEngineType(stringValue(obj.engine))
        if (!engineType) return

        const model = this.localPullModels.get(engineType)
        if (!model) return
        const key = engineProgressKey({ nodeId, engineType, operation: 'pull', model })
        const existing = this.activePulls.get(key)
        if (!existing) return

        const percent = numberValue(obj.percent)
        const progress: EngineProgress = {
            ...existing,
            status: stringValue(obj.stage) || existing.status,
            percent: mergePullProgressPercent(percent, existing.percent)
        }
        this.activePulls.set(key, progress)
        emitBridgePush('engines:progress-changed', progress)
    }

    /** Whether an optimistic pull entry for this model is still in flight. */
    isModelPullActive(engineType: EngineType, model: string): boolean {
        const nodeId = this.selfId
        if (!nodeId) return false
        return this.activePulls.has(
            engineProgressKey({ nodeId, engineType, operation: 'pull', model })
        )
    }

    /** Clear the optimistic pull entry once the pull completes, fails, or times out. */
    finishModelPull(engineType: EngineType, model: string): void {
        const nodeId = this.selfId
        if (!nodeId) return
        this.localPullModels.delete(engineType)
        const key = engineProgressKey({ nodeId, engineType, operation: 'pull', model })
        if (!this.activePulls.delete(key)) return
        emitBridgePush('engines:progress-cleared', { key })
    }

    getLogs(): LogEntry[] {
        return [...this.logs]
    }

    getLogPage(): LogPage {
        return {
            entries: [...this.logs],
            total: this.logs.length,
            levels: ['info', 'warn', 'error', 'verbose'],
            sources: Array.from(new Set(this.logs.map(entry => entry.source ?? 'service-bridge'))),
            sublevels: Array.from(new Set(this.logs.map(entry => entry.sublevel)))
        }
    }

    getAppStartTime(): string {
        return appStartTime
    }

    appendLog(source: string, stream: 'stdout' | 'stderr', text: string): void {
        const entry: LogEntry = {
            level: serviceLogLevel(stream, text),
            time: new Date().toISOString(),
            source,
            sublevel: stream,
            message: text
        }
        this.logs.push(entry)
        if (this.logs.length > 2000) {
            this.logs = this.logs.slice(this.logs.length - 1000)
        }
    }

    handleNotification(notification: JsonRpcNotification): void {
        if (notification.source === 'proxy') {
            this.handleProxyNotification(notification, 'ollama')
            return
        }
        if (notification.source === 'lmstudio-proxy') {
            this.handleProxyNotification(notification, 'lm-studio')
            return
        }
        if (notification.source === 'broker') {
            this.handleBrokerNotification(notification)
        }
    }

    private handleProxyNotification(notification: JsonRpcNotification, engine: ProxyEngine): void {
        if (notification.method === 'ready') {
            const params = objectValue(notification.params)
            // Trust the broker-reported port only. If `ready` carries no port we
            // keep the last known value (0 = unknown) rather than guessing.
            const nextPort = numberValue(params?.port)
            if (nextPort <= 0) return
            const changed = nextPort !== this.proxyPorts[engine]
            this.proxyPorts[engine] = nextPort
            // A runtime proxy:set-port (or a broker steer onto a free port)
            // re-emits `ready` with the new port. Push a fresh status for that
            // engine so the Edit Node proxy port reflects the actually-bound value
            // immediately. Only on a real change so the startup baseline `ready`
            // (and redundant re-readies) stay quiet.
            if (changed) this.emitLocalEngineStatus(engine)
            return
        }

        if (notification.method === 'error') {
            const params = objectValue(notification.params)
            this.upsertError({
                id: `${engine}-proxy:${Date.now()}`,
                message: stringValue(params?.message) || 'Modular proxy failed',
                timestamp: Date.now(),
                severity: 'error',
                nodeId: this.selfId ?? ''
            })
            return
        }

        if (notification.method === 'node/removed') {
            const node = parseProxyNode(notification.params, engine)
            if (!node) return
            this.clearNodeEngine(node.id, engine)
            return
        }

        if (notification.method === 'node/discovered' || notification.method === 'node/updated') {
            const node = parseProxyNode(notification.params, engine)
            if (!node) return
            this.upsertNode(node, engine === 'ollama' ? 'ollama-proxy' : 'lmstudio-proxy')
        }
    }

    /**
     * A proxy reported a node gone for one engine. Drop just that engine's
     * presence + source; keep the node alive if another engine or the node-info
     * snapshot still references it, otherwise evict it entirely.
     */
    private clearNodeEngine(nodeId: string, engine: ProxyEngine): void {
        const existing = this.nodes.get(nodeId)
        if (!existing) return
        const source: BrokerNodeSource = engine === 'ollama' ? 'ollama-proxy' : 'lmstudio-proxy'
        const sources = removeSource(existing.sources, source)
        if (sources.length === 0 && !existing.nodeInfoUp) {
            this.removeNodeEntry(nodeId)
            return
        }
        const next: ModularNode = {
            ...existing,
            sources,
            // With no proxy left reporting the node, that feed's addresses are
            // claimed by nothing; the broker's ranked list stands alone.
            proxyAddresses: sources.some(entry => entry !== 'broker')
                ? existing.proxyAddresses
                : [],
            engines: setEngine(existing.engines, engine, emptyPresence()),
            lastSeen: Date.now()
        }
        this.nodes.set(next.id, next)
        this.emitNodeChanged(next)
    }

    private handleBrokerNotification(notification: JsonRpcNotification): void {
        if (notification.method !== 'discovery:nodes-changed') return

        const nodes = brokerNodesValue(notification.params)
        const nextBrokerNodeIds = new Set(nodes.map(node => node.id))
        for (const nodeId of Array.from(this.brokerNodeIds)) {
            if (!nextBrokerNodeIds.has(nodeId)) {
                this.removeBrokerNode(nodeId)
            }
        }
        this.brokerNodeIds = nextBrokerNodeIds

        for (const node of nodes) {
            this.upsertNode(node, 'broker')
        }
    }

    private removeBrokerNode(nodeId: string): void {
        const existing = this.nodes.get(nodeId)
        if (!existing) return

        const remainingSources = removeSource(existing.sources, 'broker')
        if (remainingSources.length === 0) {
            this.removeNodeEntry(nodeId)
            return
        }

        // A proxy source still reports the node; drop only the node-info snapshot
        // (the broker is the sole node-info / telemetry source now).
        const next: ModularNode = {
            ...existing,
            sources: remainingSources,
            nodeInfoPort: 0,
            nodeInfoUp: false,
            // Clear the broker's now-stale canonical address and ranking; a
            // still-present proxy source repopulates `reachableAddress` from its
            // `ip` field on the next node/* event, so we never keep a dead broker
            // IP — nor project addresses no live source claims any more.
            reachableAddress: '',
            brokerAddresses: [],
            gpus: [],
            cpu: null,
            memory: null,
            lastSeen: Date.now()
        }
        this.nodes.set(next.id, next)
        this.emitNodeChanged(next)
    }

    private upsertNode(node: ModularNode, source?: BrokerNodeSource): void {
        // Self is identified authoritatively by the cluster-manager's node UUID
        // (`cluster:get-node-id`), which equals the discovery/proxy hostUuid key,
        // so there is no hostname guessing here — `setSelfId` is the sole source.
        const existing = this.nodes.get(node.id)
        const merged = this.mergeNode(existing, node, source)
        // Collapse a wiped-and-rejoined host onto its new UUID before storing,
        // so the pre-wipe record never reaches the map (see resolveAddressCollision).
        if (this.resolveAddressCollision(merged)) {
            this.removeNodeEntry(merged.id)
            return
        }
        if (existing && sameNode(existing, merged)) return
        this.nodes.set(merged.id, merged)
        this.emitNodeChanged(merged)

        // A model pulled/deleted while an engine is already running produces no
        // `engine:state-changed`, so the engine-manager model cache would go stale.
        // Refresh on a flat-union change and on this engine's attribution change:
        // deleting a model shared with another engine changes only the latter.
        if (merged.id === this.selfId && existing) {
            const unionChanged = !sameStringList(existing.models, merged.models)
            for (const engine of PROXY_ENGINES) {
                const managerEngine = proxyEngineToManagerName(engine)
                const hadEngine = managerEngine in existing.modelsByEngine
                const hasEngine = managerEngine in merged.modelsByEngine
                const attributionChanged =
                    hadEngine !== hasEngine ||
                    !sameStringList(
                        existing.modelsByEngine[managerEngine] ?? [],
                        merged.modelsByEngine[managerEngine] ?? []
                    )
                // Proxy presence is not a lifecycle fact: the broker can keep
                // serving engine-manager while this optional routing proxy is
                // unavailable. Use the local manager fact first, with proxy
                // presence only as the existing external-engine fallback.
                if (
                    this.engineActiveForModels(merged, engine) &&
                    (unionChanged || attributionChanged)
                ) {
                    this.onLocalDiscoveryModelsChanged?.(engine)
                }
            }
        }
    }

    /**
     * Collapse two UUIDs that are really one machine.
     *
     * A node whose appdata is wiped mints a fresh hostUuid, so peers advertise
     * the same machine under a new key while the pre-wipe key survives — the
     * fleet sees a duplicate "ghost" row. Nothing on the wire marks the two as
     * the same host, so this infers it from the three signals the node-scanner
     * uses to nominate a superseded record (`directory.supersedeCandidates`):
     * same address, same hostname, and a `lastSeen` gap of at least
     * MODULAR_SUPERSEDE_MIN_AGE_MS.
     *
     * It stops there, and the scanner does not. Those signals come from an
     * unauthenticated mDNS advertisement, and `lastSeen` is not a liveness
     * clock — the backend re-stamps a node only when its record CHANGES, so a
     * healthy peer's `lastSeen` freezes exactly like a ghost's. The scanner
     * therefore treats them as a suspicion and deletes only once node-info
     * names a different host. This has no such proof available, so it must stay
     * a display-side collapse: it hides a row until the next snapshot, and must
     * never be read as the directory having evicted anything.
     *
     * Two caveats on how faithfully this can mirror the backend, both stemming
     * from a proxy-sourced node carrying less than a discovery record does:
     * `parseProxyNode` stamps `lastSeen` locally rather than echoing the
     * backend's, and `primaryNodeAddress` falls back to `host` (a hostname for a
     * proxy node), which can collapse the address and name conditions into one.
     * Both only ever make this rule fire LESS often than the scanner's, so the
     * node list stays a subset of the directory rather than contradicting it.
     *
     * Returns true when `node` is the STALE side and must not be stored — a
     * backend that has not yet evicted the ghost keeps re-publishing it in every
     * `discovery:nodes-changed` snapshot, so refusing it on arrival is what
     * makes the eviction stick. Otherwise the losing entries are evicted here.
     */
    private resolveAddressCollision(node: ModularNode): boolean {
        const address = primaryNodeAddress(node)
        if (!address || !node.name) return false
        const rivals: ModularNode[] = []
        for (const other of this.nodes.values()) {
            if (other.id === node.id || other.id === this.selfId) continue
            if (primaryNodeAddress(other) !== address || other.name !== node.name) continue
            rivals.push(other)
        }
        // Self is pinned by the cluster-manager's node UUID, so it is always the
        // live identity for this machine's address: skipped above so it can
        // never be evicted, and skipped as a claimant below so it never evicts.
        // The backend applies the same asymmetry (directory.supersedeCandidates).
        if (node.id === this.selfId) return false
        // Decide before mutating. Returning mid-loop would strand entries
        // already marked stale, leaving a partially applied collapse.
        if (rivals.some(other => other.lastSeen - node.lastSeen >= MODULAR_SUPERSEDE_MIN_AGE_MS)) {
            return true
        }
        for (const other of rivals) {
            if (node.lastSeen - other.lastSeen >= MODULAR_SUPERSEDE_MIN_AGE_MS) {
                this.removeNodeEntry(other.id)
            }
        }
        return false
    }

    /** Delete a node and clean every index that referenced it. */
    private removeNodeEntry(nodeId: string): void {
        if (!this.nodes.delete(nodeId)) return
        this.brokerNodeIds.delete(nodeId)
        this.dropRemoteEngineFacts(nodeId)
        emitBridgePush('nodes:remove', nodeId)
        emitBridgePush('discovery:nodes-changed', this.getAvailableNodes())
    }

    private emitNodeChanged(merged: ModularNode): void {
        emitBridgePush('nodes:upsert', toNodeItem(merged, this.selfId))
        emitBridgePush('metrics:update', toMetrics(merged))
        emitBridgePush('discovery:nodes-changed', this.getAvailableNodes())
        // For the local node, engine-manager is the source of truth for
        // install/running state; discovery only fills in models. A remote node
        // has no local engine-manager, so its status comes from authoritative
        // peer facts or its advertisement, and is omitted when neither is known.
        // Push per proxy-engine (Ollama + LM Studio) so both light up per node.
        const isSelf = merged.id === this.selfId
        for (const engine of PROXY_ENGINES) {
            if (!isSelf) {
                this.maybeClearRemotePendingFromDiscovery(merged.id, engine, merged)
            }
            const status = isSelf
                ? (this.resolveLocalEngineStatus(engine, merged) ??
                  toEngineStatus(merged, engine, this.getProxyPort(engine)))
                : this.remoteEngineStatusForPush(merged.id, engine)
            emitBridgePush('engines:state-changed', {
                nodeId: merged.id,
                engineType: engine,
                ...(status ? { status } : {}),
                models: this.discoveryModelsForNode(merged, engine, merged.id)
            })
        }
    }

    private mergeNode(
        existing: ModularNode | undefined,
        next: ModularNode,
        source: BrokerNodeSource | undefined
    ): ModularNode {
        if (!existing || !source) return next

        if (source === 'broker') {
            // node-info discovery snapshot (now also carries manual nodes). Keep
            // the live per-engine proxy data we already merged; refresh identity,
            // addresses, and node-info from the snapshot.
            return {
                ...next,
                sources: mergeSources(existing.sources, 'broker'),
                // Broker discovery is authoritative for the display hostname and
                // trust/cluster flags; keep the prior hostname only if a snapshot
                // ever lacks one.
                name: next.name || existing.name,
                // The snapshot is the broker's whole list, so it replaces the
                // previous one (carried by the spread): an address the node has
                // re-ranked away must stop being polled and projected, which a
                // union could never express. The proxy feed's own contribution
                // survives untouched and is projected behind this list.
                proxyAddresses: existing.proxyAddresses,
                // Prefer the broker's canonical address (netpick.Primary), keeping
                // a prior value only if a snapshot ever lacks ipAddress. The proxy
                // supplies one too now, but the broker's wins on merge.
                reachableAddress: next.reachableAddress || existing.reachableAddress,
                gpus: existing.gpus,
                cpu: existing.cpu,
                memory: existing.memory,
                inferenceHardwareIds: existing.inferenceHardwareIds,
                engines: existing.engines
            }
        }

        // A proxy source (ollama-proxy / lmstudio-proxy): refresh only that
        // engine's presence; keep the other engine, telemetry, and node-info.
        const engine = PROXY_SOURCE_ENGINE[source]
        return {
            ...next,
            sources: mergeSources(existing.sources, source),
            // The proxy feed carries no hostname/trust/cluster facts; keep the
            // broker-reported values rather than overwriting them with the proxy
            // node's empty name and default-false flags.
            name: existing.name || next.name,
            trusted: existing.trusted,
            clustered: existing.clustered,
            // Symmetrically: this event is the proxy feed's whole list (carried by
            // the spread), and the broker's ranking is none of its business.
            brokerAddresses: existing.brokerAddresses,
            // The proxy now carries the node's canonical `ip`, but the
            // broker's value is preferred when present; fall back to the proxy's
            // for a proxy-only node (existing empty after a broker drop).
            reachableAddress: existing.reachableAddress || next.reachableAddress,
            nodeInfoPort: existing.nodeInfoPort,
            nodeInfoUp: existing.nodeInfoUp,
            gpus: existing.gpus,
            cpu: existing.cpu,
            memory: existing.memory,
            inferenceHardwareIds: existing.inferenceHardwareIds,
            // A proxy event carries no model list (models-http); keep the
            // broker-enriched `AvailableNode.models` (+ per-engine attribution and
            // loaded set) we already merged.
            models: existing.models,
            modelsByEngine: existing.modelsByEngine,
            loadedByEngine: existing.loadedByEngine,
            engines: setEngine(existing.engines, engine, next.engines[engine]),
            lastSeen: Math.max(existing.lastSeen, next.lastSeen)
        }
    }
}

let state: ModularBridgeState | null = null

export function getModularBridgeState(): ModularBridgeState {
    if (!state) {
        state = new ModularBridgeState()
    }
    return state
}
