// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

export interface FabricPortRef {
    nodeId: string
    switchId: string
    portName: string
}

export interface FabricAcceptedHostKey {
    candidateId: string
    sha256: string
}

export interface FabricSelection {
    nodeIds: string[]
    ports: FabricPortRef[]
    acceptedHostKeys?: FabricAcceptedHostKey[]
}

/** Fresh, paired Engine Manager projection of node-info physical-port facts. */
export interface FabricPortObservation {
    switchId: string
    portName: string
    eligible: boolean
    reason?: string
}

export interface FabricNodeObservation {
    nodeId: string
    status: 'observed' | 'unavailable'
    observedAt?: number
    ports: FabricPortObservation[]
    reason?: string
}

export interface FabricInventorySnapshot {
    nodes: FabricNodeObservation[]
}

export interface FabricPermissionTarget {
    nodeId: string
    candidateId: string
    accessLabel: string
    hostKeySha256?: string
    hostKeyTrusted: boolean
    accessAvailable: boolean
    elevationAvailable: boolean
    workerAvailable: boolean
    workerSha256?: string
    reason?: string
}

export interface FabricPermissionReview {
    mode: string
    targets: FabricPermissionTarget[]
    effects: string[]
}

export interface CableInterface {
    name: string
    index: number
    mac: string
}

export interface CablePort {
    switchId: string
    portName: string
    interfaces: CableInterface[]
}

export type CableRawPrivilege = 'present' | 'approval-needed' | 'unsupported' | 'unknown'

export interface CableTarget {
    nodeId: string
    principal: string
    ports: CablePort[]
    rawPrivilege: CableRawPrivilege
    reason?: string
}

export interface CableReview {
    reviewId: string
    ownerNodeId: string
    targets: CableTarget[]
    available: boolean
    reason?: string
    remainingMs: number
    consumedRunId?: string
    permission?: FabricPermissionReview
}

export interface CableStartNotStarted {
    disposition: 'not-started'
    reviewId: string
    ownerNodeId: string
    reason: 'review-expired' | 'review-unavailable' | 'trust-changed' | 'access-changed'
}

export interface CableEdge {
    left: FabricPortRef
    right: FabricPortRef
    ageMs: number
    fresh: boolean
}

export interface CableTopology {
    layout: 'direct' | 'ring'
    status: 'unavailable' | 'unexpected' | 'missing' | 'matched'
}

export interface CableFailure {
    phase: string
    code: string
}

export interface CableFactsDifference {
    targetIndex: number
    field: string
    readStatus?: string
}

export interface CableParticipantDiagnostic {
    nodeId: string
    phase: string
    code?: string
    workerState?: 'completed' | 'cancelled' | 'failed'
    sent?: number
    received?: number
    cleanupConfirmed: boolean
    finalValidationCode?: string
    preparationResourceOutcome?: string
    factsDifference?: CableFactsDifference
}

export interface CableRunDiagnostics {
    failure?: CableFailure
    participants: CableParticipantDiagnostic[]
}

export interface CableCleanupReviewTarget {
    nodeId: string
    candidateId: string
    accessLabel: string
    hostKeySha256?: string
    hostKeyTrusted: boolean
    accessAvailable: boolean
    elevationAvailable: boolean
    inspectorAvailable: boolean
    reason?: string
}

export interface CableCleanupReview {
    reviewId: string
    runId: string
    available: boolean
    remainingMs: number
    reason?: string
    effects: string[]
    targets: CableCleanupReviewTarget[]
}

export interface CableCleanupRecovery {
    attemptId: string
    reviewId: string
    revision: number
    state: 'verifying' | 'release-pending' | 'released' | 'failed' | 'cancelled'
    holdReleased: boolean
    code: string
    message: string
    startedAt: number
    finishedAt?: number
    targets: Array<{ nodeId: string; state: string; reason?: string }>
}

export type CableRunState =
    | 'preparing'
    | 'running'
    | 'cancelling'
    | 'completed'
    | 'cancelled'
    | 'failed'

export interface CableRun {
    runId: string
    reviewId: string
    ownerNodeId: string
    revision: number
    state: CableRunState
    targets: CableTarget[]
    edges: CableEdge[]
    result: 'unavailable' | 'incomplete' | 'reciprocal-observations' | 'ambiguous'
    directness: 'unverified'
    remainingMs: number
    freshnessRemainingMs: number
    cleanupConfirmed: boolean
    startedAt: number
    finishedAt?: number
    message: string
    diagnostics?: CableRunDiagnostics
    cleanupRecovery?: CableCleanupRecovery
    topology?: CableTopology
}

export interface CableRetainedRun {
    runId: string
    reviewId: string
    ownerNodeId: string
    revision: number
    state: CableRunState
    cleanupConfirmed: boolean
    startedAt: number
    nodeIds: string[]
    ports: FabricPortRef[]
}

export interface CableRetainedRuns {
    ownerNodeId: string
    held: boolean
    limited: boolean
    reason?: string
    runs: CableRetainedRun[]
}

export interface FabricPhysicalPort {
    source: string
    switchId: string
    portName: string
}

export interface FabricGeneratedDefault {
    schemaVersion: 1
    interfaceName: string
    index: number
    owner: string
    busId: string
    devicePath: string
    permanentMAC: string
    originalAutoconnect: boolean
    uuid: string
    name: string
    settingsPath: string
    profileDigest: string
    activePath: string
}

export interface FabricInterface {
    name: string
    index: number
    mac: string
    physicalPort: FabricPhysicalPort
    addresses: string[]
    address: string
    driver: string
    rdmaDevices: string[]
    mtu: number
    generatedDefault?: FabricGeneratedDefault
}

export interface FabricTarget {
    nodeId: string
    principal: string
    switchId?: string
    portName?: string
    ports?: Array<{ switchId: string; portName: string }>
    interfaces: FabricInterface[]
}

export interface FabricReview {
    schemaVersion: 1
    reviewId: string
    ownerNodeId: string
    recipeId:
        | 'spark-two-node-temporary-addresses-v1'
        | 'spark-three-node-ring-temporary-addresses-v1'
    cableRunId?: string
    persistence: 'until-reboot'
    state: 'blocked' | 'ready'
    executable: boolean
    remainingMs: number
    targets: FabricTarget[]
    blockers: string[]
    effectsApplied: false
    permission?: FabricPermissionReview
    inspectionRequired?: boolean
    inspectionAvailable?: boolean
}

export interface FabricCandidateIP {
    nodeId: string
    peerNodeId: string
    peerPrincipal: string
    address: string
    peerAddress: string
    interfaceName: string
    interfaceIndex: number
    mac: string
    switchId: string
    portName: string
    rdmaDevice: string
    rdmaPort: number
    gidIndex: number
    gidType: string
}

export type FabricOperationState =
    | 'applying'
    | 'active'
    | 'rolling-back'
    | 'cancelled'
    | 'failed'
    | 'recovery-required'
    | 'not-started'

export interface FabricOperation {
    schemaVersion: 1
    operationId: string
    reviewId: string
    ownerNodeId: string
    recipeId?: FabricReview['recipeId']
    cableRunId?: string
    state: FabricOperationState
    targets: FabricTarget[]
    cleanupConfirmed: boolean
    effectsApplied: boolean
    effectsUnconfirmed?: boolean
    message: string
    createdAt: number
    expiresAt: number
    qualifiedAt?: number
    qualificationDigest?: string
    candidateIPs?: FabricCandidateIP[]
    permission?: FabricPermissionReview
    failure?: { nodeId: string; phase: 'inspect' | 'apply' | 'qualify'; code: string }
}

export interface FabricRetainedOperations {
    operations: FabricOperation[]
}

export type CableStartResponse = CableRun | CableStartNotStarted

export interface CableCleanupVerifyResult {
    disposition: 'accepted' | 'not-started'
    reviewId: string
    run: CableRun
}
