// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { type IEngineApi, createEngineApi } from '@/ui/api/engine-api'
import type { PreloadServiceTransport as ServiceTransport } from '@/shared/types/service-bridge'
import type {
    AvailableNode,
    ClusterIdentityPayload,
    ClusterNode,
    Invite
} from '@/shared/types/cluster'
import type { NodeItem } from '@/shared/types/nodes'
import type { ServiceError } from '@/shared/types/errors'
import type { NodeItemMetrics } from '@/shared/types/metrics'
import type { Workload, WorkloadRemoval } from '@/shared/types/workloads'
import type { AppInitialSnapshot, ClusterInitialSnapshot } from '@/shared/types/bootstrap'
import type { OnboardingHistorySummary } from '@/shared/types/onboarding-history'
import type {
    BootstrapCatalog,
    BootstrapControllerKeys,
    BootstrapOperationInvoke,
    BootstrapPlan,
    BootstrapPlanInvoke,
    BootstrapReceipt,
    BootstrapRequestInvoke,
    BootstrapStatus,
    OnboardingAccessRequest,
    OnboardingAccessResult,
    OnboardingArtifact,
    OnboardingCandidate,
    OnboardingCandidates,
    OnboardingDiscoverRequest,
    OnboardingOperation,
    OnboardingOperationRequest,
    OnboardingReview,
    OnboardingReviewRequest,
    OnboardingScopes
} from '@/shared/types/onboarding-live'
import type { DiagnosticMPIReconcileResult } from '@/shared/types/diagnostic-mpi-reconcile'
import type {
    NCCLReplacementAdoption,
    NCCLReplacementOperation,
    NCCLReplacementReview,
    NCCLReplacementSelector,
    NCCLReplacementStatus
} from '@/shared/types/diagnostic-runtime-replacement'
import type {
    DiagnosticMPIApproveRequest,
    DiagnosticMPIManagedInventory,
    DiagnosticMPIOperation,
    DiagnosticMPIOperationBinding,
    DiagnosticMPIRecovery,
    DiagnosticMPIRecoveryReference,
    DiagnosticMPIReview,
    DiagnosticMPIReviewClosure,
    DiagnosticMPISelection
} from '@/shared/types/diagnostic-mpi'
import { createFabricApi, type IFabricApi } from '@/ui/api/fabric-api'

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
    onRemove(callback: (removal: WorkloadRemoval) => void): () => void
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

export interface ISetupApi {
    /** Read-only retained setup history and its discovery/recovery gates. */
    getHistory(): Promise<OnboardingHistorySummary>
    getCandidates(): Promise<OnboardingCandidates>
    addTarget(address: string, port: number, label?: string): Promise<OnboardingCandidate>
    authorizeAccess(request: OnboardingAccessRequest): Promise<OnboardingAccessResult>
    inspect(request: OnboardingReviewRequest): Promise<OnboardingReview>
    approve(reviewId: string): Promise<OnboardingOperation>
    getOperation(request: OnboardingOperationRequest): Promise<OnboardingOperation>
    cancel(request: OnboardingOperationRequest): Promise<OnboardingOperation>
    retry(request: OnboardingOperationRequest): Promise<OnboardingOperation>
    getScopes(): Promise<OnboardingScopes>
    discover(request: OnboardingDiscoverRequest): Promise<OnboardingCandidates>
    importArtifact(file: string): Promise<OnboardingArtifact>
    getBootstrapCatalog(): Promise<BootstrapCatalog>
    getBootstrapControllerKeys(): Promise<BootstrapControllerKeys>
    inspectBootstrap(request: BootstrapRequestInvoke): Promise<BootstrapStatus>
    reviewBootstrap(request: BootstrapRequestInvoke): Promise<BootstrapPlan>
    applyBootstrap(request: BootstrapPlanInvoke): Promise<BootstrapStatus>
    getBootstrapStatus(request: BootstrapOperationInvoke): Promise<BootstrapStatus>
    recoverBootstrap(request: BootstrapOperationInvoke): Promise<BootstrapStatus>
    verifyBootstrap(request: BootstrapPlanInvoke): Promise<BootstrapReceipt>
    /** Recheck and close only retained, PAIR-owned diagnostic MPI cleanup leases. */
    reconcileDiagnosticMpi(): Promise<DiagnosticMPIReconcileResult>
    getDiagnosticMpiInventory(): Promise<DiagnosticMPIManagedInventory>
    reviewNCCLReplacement(buildOperationId: string): Promise<NCCLReplacementReview>
    approveNCCLReplacement(selector: NCCLReplacementSelector): Promise<NCCLReplacementOperation>
    getNCCLReplacementStatus(selector: NCCLReplacementSelector): Promise<NCCLReplacementStatus>
    closeNCCLReplacementReview(selector: NCCLReplacementSelector): Promise<NCCLReplacementStatus>
    cancelNCCLReplacement(selector: NCCLReplacementSelector): Promise<NCCLReplacementOperation>
    retryNCCLReplacement(
        selector: NCCLReplacementSelector & { expectedRevision: number }
    ): Promise<NCCLReplacementOperation>
    adoptNCCLReplacement(
        selector: NCCLReplacementSelector & { expectedRevision: number }
    ): Promise<NCCLReplacementAdoption>
    reviewDiagnosticMpi(selection: DiagnosticMPISelection): Promise<DiagnosticMPIReview>
    approveDiagnosticMpi(request: DiagnosticMPIApproveRequest): Promise<DiagnosticMPIOperation>
    getDiagnosticMpiStatus(
        operation: DiagnosticMPIOperationBinding
    ): Promise<DiagnosticMPIOperation>
    cancelDiagnosticMpi(operation: DiagnosticMPIOperationBinding): Promise<DiagnosticMPIOperation>
    recoverDiagnosticMpi(selection: DiagnosticMPISelection): Promise<DiagnosticMPIRecovery>
    closeDiagnosticMpiReview(
        reference: DiagnosticMPIRecoveryReference
    ): Promise<DiagnosticMPIReviewClosure>
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
    setup: ISetupApi
    fabric: IFabricApi
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
        },
        setup: {
            getHistory: () => transport.invoke('setup:get-history'),
            getCandidates: () => transport.invoke('engine:onboarding-candidates'),
            addTarget: (address, port, label) =>
                transport.invoke('engine:onboarding-add-target', { address, port, label }),
            authorizeAccess: request => transport.invoke('engine:onboarding-access', request),
            inspect: request => transport.invoke('engine:onboarding-inspect', request),
            approve: reviewId => transport.invoke('engine:onboarding-approve', { reviewId }),
            getOperation: request => transport.invoke('engine:onboarding-status', request),
            cancel: request => transport.invoke('engine:onboarding-cancel', request),
            retry: request => transport.invoke('engine:onboarding-retry', request),
            getScopes: () => transport.invoke('engine:onboarding-scopes'),
            discover: request => transport.invoke('engine:onboarding-discover', request),
            importArtifact: file => transport.invoke('engine:onboarding-import-artifact', { file }),
            getBootstrapCatalog: () => transport.invoke('engine:onboarding-bootstrap-catalog'),
            getBootstrapControllerKeys: () =>
                transport.invoke('engine:onboarding-bootstrap-controller-keys'),
            inspectBootstrap: request =>
                transport.invoke('engine:onboarding-bootstrap-inspect', request),
            reviewBootstrap: request =>
                transport.invoke('engine:onboarding-bootstrap-review', request),
            applyBootstrap: request =>
                transport.invoke('engine:onboarding-bootstrap-apply', request),
            getBootstrapStatus: request =>
                transport.invoke('engine:onboarding-bootstrap-status', request),
            recoverBootstrap: request =>
                transport.invoke('engine:onboarding-bootstrap-recover', request),
            verifyBootstrap: request =>
                transport.invoke('engine:onboarding-bootstrap-verify', request),
            reconcileDiagnosticMpi: () => transport.invoke('engine:diagnostic-mpi-reconcile', {}),
            getDiagnosticMpiInventory: () => transport.invoke('engine:diagnostic-managed-runtimes'),
            reviewNCCLReplacement: buildOperationId =>
                transport.invoke('engine:diagnostic-nccl-replacement-review', { buildOperationId }),
            approveNCCLReplacement: selector =>
                transport.invoke('engine:diagnostic-nccl-replacement-approve', selector),
            getNCCLReplacementStatus: selector =>
                transport.invoke('engine:diagnostic-nccl-replacement-status', selector),
            closeNCCLReplacementReview: selector =>
                transport.invoke('engine:diagnostic-nccl-replacement-close', selector),
            cancelNCCLReplacement: selector =>
                transport.invoke('engine:diagnostic-nccl-replacement-cancel', selector),
            retryNCCLReplacement: request =>
                transport.invoke('engine:diagnostic-nccl-replacement-retry', request),
            adoptNCCLReplacement: request =>
                transport.invoke('engine:diagnostic-nccl-replacement-adopt', request),
            reviewDiagnosticMpi: selection =>
                transport.invoke('engine:diagnostic-mpi-review', selection),
            approveDiagnosticMpi: request =>
                transport.invoke('engine:diagnostic-mpi-approve', request),
            getDiagnosticMpiStatus: operation =>
                transport.invoke('engine:diagnostic-mpi-status', operation),
            cancelDiagnosticMpi: operation =>
                transport.invoke('engine:diagnostic-mpi-cancel', operation),
            recoverDiagnosticMpi: selection =>
                transport.invoke('engine:diagnostic-mpi-recover', selection),
            closeDiagnosticMpiReview: reference =>
                transport.invoke('engine:diagnostic-mpi-close-review', reference)
        },
        fabric: createFabricApi(transport)
    }
}
