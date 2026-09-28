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
    EngineLogSnapshot,
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
import type { Workload, WorkloadRemoval } from '@/shared/types/workloads'
import type { OnboardingHistorySummary } from '@/shared/types/onboarding-history'
import type {
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
    OnboardingScopes,
    BootstrapCatalog,
    BootstrapControllerKeys,
    BootstrapOperationInvoke,
    BootstrapPlan,
    BootstrapPlanInvoke,
    BootstrapReceipt,
    BootstrapRequestInvoke,
    BootstrapStatus
} from '@/shared/types/onboarding-live'
import type { VllmGroupRunStatus, VllmGroupStatus } from '@/shared/types/vllm-group-status'
import type {
    VllmGroupCheck,
    VllmGroupOperation,
    VllmGroupReconcileRequest,
    VllmGroupReview,
    VllmGroupReviewRequest,
    VllmGroupSelection,
    VllmGroupStartRequest
} from '@/shared/types/vllm-group'
import type { VllmGroupCleanupRequest } from '@/shared/types/vllm-group-cleanup'
import type {
    VllmDistributionRequest,
    VllmExactModelRequest,
    VllmModelReceipt,
    VllmOperationCancelResult,
    VllmRuntimePrepareRequest,
    VllmRuntimePrepareResult
} from '@/shared/types/vllm-model-journey'
import type {
    CableCleanupReview,
    CableCleanupVerifyResult,
    CableRetainedRuns,
    CableReview,
    CableRun,
    CableStartResponse,
    FabricOperation,
    FabricInventorySnapshot,
    FabricRetainedOperations,
    FabricReview,
    FabricSelection
} from '@/shared/types/fabric'
import type {
    VllmModelSelectionRequest,
    VllmModelSelectionResult
} from '@/shared/types/vllm-model-selection'
import type {
    AvailableNode,
    ClusterIdentityPayload,
    ClusterNode,
    Invite
} from '@/shared/types/cluster'
import type {
    DiagnosticMPIReconcileRequest,
    DiagnosticMPIReconcileResult
} from '@/shared/types/diagnostic-mpi-reconcile'
import type {
    DiagnosticMPIApproveRequest,
    DiagnosticMPIManagedInventory,
    DiagnosticMPIOperation,
    DiagnosticMPIOperationBinding,
    DiagnosticMPIRecovery,
    DiagnosticMPIRecoveryReference,
    DiagnosticMPIReview,
    DiagnosticMPIReviewClosure,
    DiagnosticMPIReviewRequest,
    DiagnosticMPISelection
} from '@/shared/types/diagnostic-mpi'
import type {
    NCCLReplacementAdoption,
    NCCLReplacementOperation,
    NCCLReplacementReview,
    NCCLReplacementSelector,
    NCCLReplacementStatus
} from '@/shared/types/diagnostic-runtime-replacement'

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

    // Engines
    'engines:get-initial': { request: void; response: EngineInitialState }
    'engine:logs': { request: { engineType: EngineType }; response: EngineLogSnapshot }
    'engines:get-settings': { request: EngineSettingsTarget; response: EngineSettingsSnapshot }
    'engines:preview-settings': { request: EngineSettingsRequest; response: EngineSettingsPreview }
    'engines:apply-settings': { request: EngineSettingsRequest; response: EngineSettingsReceipt }
    'engine:command': { request: EngineCommandPayload; response: null }
    'engine:search-hub': { request: { engineType: EngineType }; response: EngineHubSearchResponse }
    // Managed vLLM serving group. Review and check are proposals; start, stop,
    // reconcile and cleanup are exact typed operations that PAIR may refuse.
    // Status is the only hold-truth authority for the renderer and bridge fences.
    'engine:vllm-group-review': {
        request: { selection: VllmGroupSelection }
        response: VllmGroupReview
    }
    'engine:vllm-group-check': { request: VllmGroupReviewRequest; response: VllmGroupCheck }
    'engine:vllm-group-start': { request: VllmGroupStartRequest; response: VllmGroupRunStatus }
    'engine:vllm-group-status': { request: void; response: VllmGroupStatus }
    'engine:vllm-group-stop': { request: VllmGroupOperation; response: VllmGroupStatus }
    'engine:vllm-group-reconcile': {
        request: VllmGroupReconcileRequest
        response: VllmGroupStatus
    }
    // Cleanup runs PAIR's own noninteractive fixed-helper reconcile and closure
    // and answers with the refreshed managed group status; unresolved ranks are
    // an error and the group stays held.
    'engine:vllm-group-cleanup': { request: VllmGroupCleanupRequest; response: VllmGroupStatus }
    // Retained model selection for a stopped, PAIR-managed vLLM on this
    // controller. The model must already be in PAIR's downloaded library.
    'engine:vllm-select-model': {
        request: VllmModelSelectionRequest
        response: VllmModelSelectionResult
    }
    // Product-owned immutable model/runtime preparation. Renderer requests are
    // identity-only: no credentials, commands, URLs, or filesystem paths.
    'engine:vllm-pull-exact': {
        request: VllmExactModelRequest
        response: VllmModelReceipt
    }
    'engine:vllm-cancel-pull': {
        request: VllmExactModelRequest
        response: VllmOperationCancelResult
    }
    'engine:vllm-prepare-runtime': {
        request: VllmRuntimePrepareRequest
        response: VllmRuntimePrepareResult
    }
    'engine:vllm-cancel-prepare': {
        request: VllmRuntimePrepareRequest
        response: VllmOperationCancelResult
    }
    'engine:vllm-distribute-model': {
        request: VllmDistributionRequest
        response: VllmModelReceipt
    }
    'engine:vllm-cancel-distribution': {
        request: VllmDistributionRequest
        response: VllmOperationCancelResult
    }

    // Physical-cable evidence and temporary fabric setup. The renderer supplies
    // only identities/ports and server-issued operation ids; Electron injects
    // the fixed approval booleans at the broker boundary.
    'engine:cable-review': { request: FabricSelection; response: CableReview }
    'engine:cable-start': { request: { reviewId: string }; response: CableStartResponse }
    'engine:cable-status': {
        request: { runId?: string; reviewId?: string }
        response: CableRun
    }
    'engine:cable-cancel': { request: { runId: string }; response: CableRun }
    'engine:cable-retained-runs': { request: void; response: CableRetainedRuns }
    'engine:cable-cleanup-review': {
        request: { runId: string; acceptedHostKeys?: FabricSelection['acceptedHostKeys'] }
        response: CableCleanupReview
    }
    'engine:cable-cleanup-verify': {
        request: { runId: string; reviewId: string }
        response: CableCleanupVerifyResult
    }
    'engine:cable-cleanup-cancel': { request: { runId: string }; response: CableRun }
    'engine:fabric-inventory': {
        request: { nodeIds: string[] }
        response: FabricInventorySnapshot
    }
    'engine:fabric-review': {
        request: { selection: FabricSelection; inspectSelectedProfiles?: boolean }
        response: FabricReview
    }
    'engine:fabric-approve': {
        request: { reviewId: string; selectedPortPauseApproved: boolean }
        response: FabricOperation
    }
    'engine:fabric-status': { request: { operationId: string }; response: FabricOperation }
    'engine:fabric-cancel': { request: { operationId: string }; response: FabricOperation }
    'engine:fabric-recover': { request: { operationId: string }; response: FabricOperation }
    'engine:fabric-retained-operations': { request: void; response: FabricRetainedOperations }

    // Read-only retained setup compatibility and recovery state.
    'setup:get-history': { request: void; response: OnboardingHistorySummary }
    'engine:diagnostic-mpi-reconcile': {
        request: DiagnosticMPIReconcileRequest
        response: DiagnosticMPIReconcileResult
    }
    // Managed NCCL correctness smoke. The renderer supplies only exact current
    // node/build or PAIR-issued operation identities; Electron injects the
    // fixed management-network Socket test window at the backend boundary.
    'engine:diagnostic-managed-runtimes': {
        request: void
        response: DiagnosticMPIManagedInventory
    }
    'engine:diagnostic-nccl-replacement-review': {
        request: { buildOperationId: string }
        response: NCCLReplacementReview
    }
    'engine:diagnostic-nccl-replacement-approve': {
        request: NCCLReplacementSelector
        response: NCCLReplacementOperation
    }
    'engine:diagnostic-nccl-replacement-status': {
        request: NCCLReplacementSelector
        response: NCCLReplacementStatus
    }
    'engine:diagnostic-nccl-replacement-close': {
        request: NCCLReplacementSelector
        response: NCCLReplacementStatus
    }
    'engine:diagnostic-nccl-replacement-cancel': {
        request: NCCLReplacementSelector
        response: NCCLReplacementOperation
    }
    'engine:diagnostic-nccl-replacement-retry': {
        request: NCCLReplacementSelector & { expectedRevision: number }
        response: NCCLReplacementOperation
    }
    'engine:diagnostic-nccl-replacement-adopt': {
        request: NCCLReplacementSelector & { expectedRevision: number }
        response: NCCLReplacementAdoption
    }
    'engine:diagnostic-mpi-review': {
        request: DiagnosticMPIReviewRequest
        response: DiagnosticMPIReview
    }
    'engine:diagnostic-mpi-approve': {
        request: DiagnosticMPIApproveRequest
        response: DiagnosticMPIOperation
    }
    'engine:diagnostic-mpi-status': {
        request: DiagnosticMPIOperationBinding
        response: DiagnosticMPIOperation
    }
    'engine:diagnostic-mpi-cancel': {
        request: DiagnosticMPIOperationBinding
        response: DiagnosticMPIOperation
    }
    'engine:diagnostic-mpi-recover': {
        request: DiagnosticMPISelection
        response: DiagnosticMPIRecovery
    }
    // Logical renderer channel over the backend's typed MPI status recovery
    // selector. It can only close one exact, recovered unstarted review.
    'engine:diagnostic-mpi-close-review': {
        request: DiagnosticMPIRecoveryReference
        response: DiagnosticMPIReviewClosure
    }
    'engine:onboarding-candidates': { request: void; response: OnboardingCandidates }
    'engine:onboarding-add-target': {
        request: { address: string; port: number; label?: string }
        response: OnboardingCandidate
    }
    'engine:onboarding-access': {
        request: OnboardingAccessRequest
        response: OnboardingAccessResult
    }
    'engine:onboarding-inspect': {
        request: OnboardingReviewRequest
        response: OnboardingReview
    }
    'engine:onboarding-approve': {
        request: { reviewId: string }
        response: OnboardingOperation
    }
    'engine:onboarding-status': {
        request: OnboardingOperationRequest
        response: OnboardingOperation
    }
    'engine:onboarding-cancel': {
        request: OnboardingOperationRequest
        response: OnboardingOperation
    }
    'engine:onboarding-retry': {
        request: OnboardingOperationRequest
        response: OnboardingOperation
    }
    'engine:onboarding-scopes': { request: void; response: OnboardingScopes }
    'engine:onboarding-discover': {
        request: OnboardingDiscoverRequest
        response: OnboardingCandidates
    }
    'engine:onboarding-import-artifact': {
        request: { file: string }
        response: OnboardingArtifact
    }
    'engine:onboarding-bootstrap-catalog': {
        request: void
        response: BootstrapCatalog
    }
    'engine:onboarding-bootstrap-controller-keys': {
        request: void
        response: BootstrapControllerKeys
    }
    'engine:onboarding-bootstrap-inspect': {
        request: BootstrapRequestInvoke
        response: BootstrapStatus
    }
    'engine:onboarding-bootstrap-review': {
        request: BootstrapRequestInvoke
        response: BootstrapPlan
    }
    'engine:onboarding-bootstrap-apply': {
        request: BootstrapPlanInvoke
        response: BootstrapStatus
    }
    'engine:onboarding-bootstrap-status': {
        request: BootstrapOperationInvoke
        response: BootstrapStatus
    }
    'engine:onboarding-bootstrap-recover': {
        request: BootstrapOperationInvoke
        response: BootstrapStatus
    }
    'engine:onboarding-bootstrap-verify': {
        request: BootstrapPlanInvoke
        response: BootstrapReceipt
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
    'workloads:remove': WorkloadRemoval

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
