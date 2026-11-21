// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Strongly-typed contract for every logical service message exchanged between
 * the UI and the Electron preload service bridge.
 *
 * - `WsInvokeChannelMap`: request/response shape for each logical invoke channel.
 *   The UI invokes `window.pairApi`, preload forwards over IPC, and Electron
 *   main replies with either modular subprocess data or an explicit empty
 *   payload.
 *
 * - `WsPushChannelMap`: payload shape for every service-to-renderer push event.
 *
 * Both maps are the single source of truth; the preload transport and service
 * bridge handlers are generic over these keys so TypeScript catches a
 * mismatched channel or payload at compile time.
 *
 * Discipline:
 *   - One-off payloads are inlined, not named. Named shapes are only lifted
 *     when (a) a second file imports them or (b) inlining would duplicate the
 *     shape twice inside this file.
 *   - `export` is reserved for the handful of symbols consumed by the
 *     service bridge generics — nothing else.
 */
import type {
    EngineCommandPayload,
    EngineHubSearchResponse,
    EngineInitialState,
    EngineStatePatch
} from '@/shared/types/engine-api'
import type { EngineProgress, EngineType } from '@/shared/types/engines'
import type {
    EngineSettingsTarget,
    EngineSettingsRequest,
    EngineSettingsSnapshot,
    EngineSettingsPreview,
    EngineSettingsReceipt
} from '@/shared/types/engine-settings'
import type { AppInitialSnapshot, ClusterInitialSnapshot } from '@/shared/types/bootstrap'
import type { ServiceError } from '@/shared/types/errors'
import type { NodeItem } from '@/shared/types/nodes'
import type { NodeItemMetrics } from '@/shared/types/metrics'
import type { Workload } from '@/shared/types/workloads'
import type {
    AvailableNode,
    ClusterIdentityPayload,
    ClusterModelAliasViewEntry,
    ClusterNode,
    Invite,
    ModelAlias
} from '@/shared/types/cluster'

// -----------------------------------------------------------------------------
// Invoke channels — (request, response) pairs
// -----------------------------------------------------------------------------

/**
 * Snapshot returned by `nodes:get-initial`. Plain mutable types on the wire —
 * the server holds `DeepReadonly<T>` internally but JSON transport deep-clones
 * into plain objects on the client, so the wire contract is mutable.
 */
interface NodesInitialResponse {
    nodes: Record<string, NodeItem>
    fetchedNodes: boolean
}

/**
 * This computer's cluster routing endpoint preference, persisted in
 * `ui-config.json` and turned into broker argv when the services start.
 */
export interface ClusterRouterConfig {
    enabled: boolean
    port: number
}

/**
 * What the proxy reports about that endpoint right now: `running` together with
 * the port it is actually bound to, or `false`/`0` when there is no listener.
 * The bridge reads this from the proxy rather than answering it from
 * configuration, so a port somebody else already holds is never reported as an
 * endpoint.
 */
export interface ClusterRouterStatus {
    running: boolean
    port: number
}

export interface WsInvokeChannelMap {
    // App bootstrap
    'app:get-initial': { request: void; response: AppInitialSnapshot }

    // Nodes
    'nodes:get-initial': { request: void; response: NodesInitialResponse }
    // Remove a node from the cluster (cluster-manager `nodes:remove`) and drop any
    // local manual discovery entry for it in the same call. Distinct from the
    // `nodes:remove` push (state update broadcast).
    'nodes:remove-member': {
        request: { nodeId: string }
        response: { nodeId: string; removed: boolean }
    }

    // Discovery
    'discovery:get-nodes': { request: void; response: AvailableNode[] }

    // Cluster — PIN-pairing handshake (nvpair-cluster-manager)
    'cluster:get-initial': { request: void; response: ClusterInitialSnapshot }
    'cluster:invite-node': { request: { ipAddress: string }; response: Invite }
    'cluster:invite-status': { request: { inviteId: string }; response: Invite }
    'cluster:respond-to-invite': {
        request: { inviteId: string; accept: boolean; pin?: string }
        response: Invite
    }
    // Abort a still-pending outbound invite (the inviter's counterpart to a
    // joiner decline): tears down the pairing session, invalidates the PIN, and
    // best-effort notifies the joiner. Returns the updated `Invite` (`canceled`).
    'cluster:cancel-invite': { request: { inviteId: string }; response: Invite }
    // Dissolve a cluster that was auto-created solely to back an invite, when
    // that pairing failed and no peer joined (no-op otherwise).
    'cluster:abandon-if-solo': { request: void; response: null }

    // Cluster model aliases — this node's configured routing document, stored
    // by nvpair-node-settings and reached through the broker's `settings/`
    // relay. One entry is two names, a caller-side name and the model it stands
    // for, so this contract and the settings wire already spell the same thing;
    // the service bridge decodes rather than forwarding raw JSON.
    'cluster:get-model-aliases': { request: void; response: ModelAlias[] }

    // A node's alias document, written from here — this computer's own included,
    // which is why the editor for every list sits on that node's card and not in
    // two places. `nodeId` is the cluster id the node cards are keyed by, and the
    // broker — which alone knows which id is this machine — either applies it
    // locally or carries it to that node over cluster mTLS, where its own
    // settings service validates it again.
    // Replacing a whole document is why an empty list is a request and an absent
    // one is refused. Resolves once the owning node has written it; rejects with
    // that node's own reason when it refuses.
    'cluster:set-node-model-aliases': {
        request: { nodeId: string; aliases: ModelAlias[] }
        response: null
    }

    // The cluster alias view, relayed to the proxy child's process-scoped
    // `router/get-aliases`: the merged table this node routes by, including
    // peer-announced entries marked with the node that wrote them. Read-only
    // — a peer's alias is edited on the peer, which is the whole reason the
    // view carries origins.
    'cluster:get-router-aliases': { request: void; response: ClusterModelAliasViewEntry[] }

    // Cluster routing endpoint. The config pair reads this computer's preference;
    // set applies the patch to the running services whenever a broker is up and
    // stores what they accepted. A service refusal — a port its proxy could not
    // bind — rejects with the services' own sentence and stores nothing; only a
    // port this side rules unbindable is answered from the stored preference
    // without a service call. With no broker there is nothing to apply, so the
    // patch stores for the next start, which reads the same preference, and
    // resolves as a plain success. The status read relays the proxy's
    // process-scoped `router/status`.
    'cluster:get-router-config': { request: void; response: ClusterRouterConfig }
    'cluster:set-router-config': {
        request: Partial<ClusterRouterConfig>
        response: ClusterRouterConfig
    }
    'cluster:get-router-status': { request: void; response: ClusterRouterStatus }

    // Engines
    'engines:get-initial': { request: void; response: EngineInitialState }
    'engines:get-settings': { request: EngineSettingsTarget; response: EngineSettingsSnapshot }
    'engines:preview-settings': { request: EngineSettingsRequest; response: EngineSettingsPreview }
    'engines:apply-settings': { request: EngineSettingsRequest; response: EngineSettingsReceipt }
    'engine:command': { request: EngineCommandPayload; response: null }
    'engine:search-hub': {
        request: { engineType: EngineType; nodeId: string; query?: string }
        response: EngineHubSearchResponse
    }

    // Errors
    'errors:get-initial': { request: void; response: ServiceError[] }
    'errors:clear': { request: string; response: null }

    // Workloads
    'workloads:get-initial': { request: void; response: Record<string, Workload> }
}

export type WsInvokeChannel = keyof WsInvokeChannelMap
export type WsInvokeRequest<C extends WsInvokeChannel> = WsInvokeChannelMap[C]['request']
export type WsInvokeResponse<C extends WsInvokeChannel> = WsInvokeChannelMap[C]['response']

// -----------------------------------------------------------------------------
// Push channels — server → client broadcast events (single payload per event)
// -----------------------------------------------------------------------------

export interface WsPushChannelMap {
    // Nodes / cluster state
    'nodes:upsert': NodeItem
    'nodes:remove': string
    /** Full cluster membership snapshot, pushed on every change (cluster-manager `nodes:changed`). */
    'nodes:changed': ClusterNode[]

    // Engines
    'engines:state-changed': EngineStatePatch
    'engines:settings-changed': EngineSettingsSnapshot
    'engines:settings-disconnected': { nodeId: string }
    'engines:progress-changed': EngineProgress
    'engines:progress-cleared': { key: string }

    // Metrics
    'metrics:update': NodeItemMetrics

    // Workloads. Removal carries the origin node (`originatedFrom`) because
    // workload ids are a per-node proxy counter (the catalog is keyed by the
    // (originatedFrom, id) pair).
    'workloads:upsert': Workload
    'workloads:remove': { workloadId: string; originatedFrom: string | null }

    // Errors
    'errors:update': ServiceError[]

    // Cluster flow — an inbound pairing arrived; prompt the user for the PIN.
    'cluster:invite-received': Invite
    // Full authoritative snapshot of every live inbound invite awaiting a PIN,
    // re-emitted by Electron main on every add/prune (arrival, paired, decline,
    // status-poll terminal state, or client TTL expiry).
    'cluster:pending-invites-changed': Invite[]

    // Discovery
    'discovery:nodes-changed': AvailableNode[]

    // Connection lifecycle
    'connection:cluster-identity': ClusterIdentityPayload
    'state:request-refresh': void
}

export type WsPushChannel = keyof WsPushChannelMap
export type WsPushPayload<C extends WsPushChannel> = WsPushChannelMap[C]
