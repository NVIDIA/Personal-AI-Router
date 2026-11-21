// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { type IEngineApi, createEngineApi } from '@/ui/api/engine-api'
import type { PreloadServiceTransport as ServiceTransport } from '@/shared/types/service-bridge'
import type {
    AvailableNode,
    ClusterIdentityPayload,
    ClusterModelAliasViewEntry,
    ClusterNode,
    Invite,
    ModelAlias
} from '@/shared/types/cluster'
import type { NodeItem } from '@/shared/types/nodes'
import type { ClusterRouterConfig, ClusterRouterStatus } from '@/shared/types/ws-channels'
import type { ServiceError } from '@/shared/types/errors'
import type { NodeItemMetrics } from '@/shared/types/metrics'
import type { Workload } from '@/shared/types/workloads'
import type { AppInitialSnapshot, ClusterInitialSnapshot } from '@/shared/types/bootstrap'

// ---------------------------------------------------------------------------
// Sub-API interfaces
// ---------------------------------------------------------------------------

export interface IConnectionApi {
    /** Service requests a full state refresh (e.g. after cluster membership changes). */
    onStateRequestRefresh(callback: () => void): () => void
    /** Fires when cluster identity (clusterId / clusterFriendlyName) changes. */
    onClusterIdentity(callback: (payload: ClusterIdentityPayload) => void): () => void
}

export interface IAppApi {
    /** Fetch app bootstrap state: connection state and self id. */
    getInitial(): Promise<AppInitialSnapshot>
}

export interface INodesApi {
    /** Fetch initial cluster member state. */
    getInitial(): Promise<{
        nodes: Record<string, NodeItem>
        fetchedNodes: boolean
    }>
    /** Remove a node from the cluster (revokes membership + pinned trust). */
    removeMember(nodeId: string): Promise<{ nodeId: string; removed: boolean }>
    /** A node was added or updated in the discovery/metrics list. */
    onUpsert(callback: (node: NodeItem) => void): () => void
    /** A node was removed from the discovery/metrics list. */
    onRemove(callback: (nodeId: string) => void): () => void
    /** Full cluster membership snapshot changed. */
    onMembersChanged(callback: (members: ClusterNode[]) => void): () => void
}

export interface IClusterApi {
    /** Fetch cluster bootstrap state: identity, settings, and membership. */
    getInitial(): Promise<ClusterInitialSnapshot>
    /** Start PIN pairing with a remote node; the returned invite carries the PIN to display. */
    inviteNode(ipAddress: string): Promise<Invite>
    /** Poll the state of an outbound pairing session. */
    inviteStatus(inviteId: string): Promise<Invite>
    /** Respond to an inbound invite: accept with the PIN from the inviter, or decline. */
    respondToInvite(inviteId: string, accept: boolean, pin?: string): Promise<Invite>
    /**
     * Abort a still-pending outbound invite this node sent: tears down the
     * pairing session and invalidates the PIN so it can no longer complete.
     */
    cancelInvite(inviteId: string): Promise<Invite>
    /**
     * Dissolve a cluster that was auto-created only to back an outbound invite,
     * when that pairing failed and no peer joined. No-op otherwise.
     */
    abandonIfSolo(): Promise<void>
    /**
     * This node's configured cluster model aliases — caller-side synonyms that
     * the cluster router resolves to engines on any cluster node.
     */
    getModelAliases(): Promise<ModelAlias[]>
    /**
     * Replace a node's alias list wholesale — this computer's own included, which
     * is why one card edits every list. `nodeId` is the cluster id the Overview
     * node cards use, and the broker decides where the document lives: written
     * here when it is this machine, carried to the owning node otherwise and
     * checked again by that node's own settings service. Rejects with that node's
     * own reason when it refuses the document.
     */
    setNodeModelAliases(nodeId: string, aliases: ModelAlias[]): Promise<void>
    /**
     * The cluster alias view: every name this computer's cluster router routes
     * by, peer-announced ones included, each marked with the node that wrote
     * it. Read-only — a peer's alias is edited on that peer.
     */
    getRouterAliases(): Promise<ClusterModelAliasViewEntry[]>
    /**
     * This computer's cluster routing endpoint preference (whether it is served
     * and on which loopback port). Takes effect at the next services restart.
     */
    getRouterConfig(): Promise<ClusterRouterConfig>
    /**
     * Persist part of that preference. Resolves with the whole stored value, so
     * a port the bridge refuses snaps the editor back to what will be used.
     */
    setRouterConfig(patch: Partial<ClusterRouterConfig>): Promise<ClusterRouterConfig>
    /** Whether the endpoint is listening right now, and on which port. */
    getRouterStatus(): Promise<ClusterRouterStatus>
    /** An inbound pairing arrived; prompt the user for the PIN. */
    onInviteReceived(callback: (invite: Invite) => void): () => void
    /** The authoritative set of live inbound invites changed (add or prune). */
    onPendingInvitesChanged(callback: (invites: Invite[]) => void): () => void
}

export interface IDiscoveryApi {
    /** List nodes discovered on the network that are available to invite. */
    getNodes(): Promise<AvailableNode[]>
    /** The list of available discoverable nodes changed. */
    onNodesChanged(callback: (nodes: AvailableNode[]) => void): () => void
}

export interface IWorkloadsApi {
    /** Fetch all active workloads (inference jobs). */
    getInitial(): Promise<Record<string, Workload>>
    /** A workload was created or updated. */
    onUpsert(callback: (workload: Workload) => void): () => void
    /** A workload was completed and removed. */
    onRemove(
        callback: (removal: { workloadId: string; originatedFrom: string | null }) => void
    ): () => void
}

export interface IErrorsApi {
    /** Fetch all active service errors. */
    getInitial(): Promise<ServiceError[]>
    /** Dismiss a specific service error by id. */
    clear(id: string): Promise<void>
    /** The service error list changed. */
    onUpdate(callback: (errors: ServiceError[]) => void): () => void
}

export interface IMetricsApi {
    /** Periodic hardware metrics update (CPU, GPU, memory) for a node. */
    onUpdate(callback: (metrics: NodeItemMetrics) => void): () => void
}

// ---------------------------------------------------------------------------
// Composite API
// ---------------------------------------------------------------------------

/**
 * Service communication API shared by the renderer surfaces.
 * Backed by the Electron preload service transport.
 */
export interface IPairApi {
    app: IAppApi
    connection: IConnectionApi
    nodes: INodesApi
    cluster: IClusterApi
    discovery: IDiscoveryApi
    engines: IEngineApi
    workloads: IWorkloadsApi
    errors: IErrorsApi
    metrics: IMetricsApi
}

// ---------------------------------------------------------------------------
// Factory — thin binding over the typed service transport.
// ---------------------------------------------------------------------------

export function createPairApi(transport: ServiceTransport): IPairApi {
    return {
        app: {
            getInitial: () => transport.invoke('app:get-initial')
        },
        connection: {
            onStateRequestRefresh: cb => transport.subscribePush('state:request-refresh', cb),
            onClusterIdentity: cb => transport.subscribePush('connection:cluster-identity', cb)
        },
        nodes: {
            getInitial: async () => {
                const state = await transport.invoke('nodes:get-initial')
                return {
                    nodes: state.nodes,
                    fetchedNodes: state.fetchedNodes
                }
            },
            removeMember: nodeId => transport.invoke('nodes:remove-member', { nodeId }),
            onUpsert: cb => transport.subscribePush('nodes:upsert', cb),
            onRemove: cb => transport.subscribePush('nodes:remove', cb),
            onMembersChanged: cb => transport.subscribePush('nodes:changed', cb)
        },
        cluster: {
            getInitial: () => transport.invoke('cluster:get-initial'),
            inviteNode: ipAddress => transport.invoke('cluster:invite-node', { ipAddress }),
            inviteStatus: inviteId => transport.invoke('cluster:invite-status', { inviteId }),
            respondToInvite: (inviteId, accept, pin) =>
                transport.invoke('cluster:respond-to-invite', { inviteId, accept, pin }),
            cancelInvite: inviteId => transport.invoke('cluster:cancel-invite', { inviteId }),
            abandonIfSolo: async () => {
                await transport.invoke('cluster:abandon-if-solo')
            },
            getModelAliases: () => transport.invoke('cluster:get-model-aliases'),
            setNodeModelAliases: async (nodeId, aliases) => {
                await transport.invoke('cluster:set-node-model-aliases', { nodeId, aliases })
            },
            getRouterAliases: () => transport.invoke('cluster:get-router-aliases'),
            getRouterConfig: () => transport.invoke('cluster:get-router-config'),
            setRouterConfig: patch => transport.invoke('cluster:set-router-config', patch),
            getRouterStatus: () => transport.invoke('cluster:get-router-status'),
            onInviteReceived: cb => transport.subscribePush('cluster:invite-received', cb),
            onPendingInvitesChanged: cb =>
                transport.subscribePush('cluster:pending-invites-changed', cb)
        },
        discovery: {
            getNodes: () => transport.invoke('discovery:get-nodes'),
            onNodesChanged: cb => transport.subscribePush('discovery:nodes-changed', cb)
        },
        engines: createEngineApi(transport),
        workloads: {
            getInitial: () => transport.invoke('workloads:get-initial'),
            onUpsert: cb => transport.subscribePush('workloads:upsert', cb),
            onRemove: cb => transport.subscribePush('workloads:remove', cb)
        },
        errors: {
            getInitial: () => transport.invoke('errors:get-initial'),
            clear: async id => {
                await transport.invoke('errors:clear', id)
            },
            onUpdate: cb => transport.subscribePush('errors:update', cb)
        },
        metrics: {
            onUpdate: cb => transport.subscribePush('metrics:update', cb)
        }
    }
}
