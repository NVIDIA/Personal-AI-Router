// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

export type OnboardingAuth = 'password' | 'existing-key'
export type OnboardingStartupLifetime = 'session' | 'persistent'

export interface OnboardingCandidate {
    candidateId: string
    label: string
    address: string
    port: number
    accessId?: string
    accessLabel?: string
    accessAvailable: boolean
    hostKeySha256?: string
    hostKeyTrusted: boolean
    bootstrapSource: string
    bootstrapState: OnboardingEnrollment
    bootstrapPlatform?: BootstrapPlatform
    bootstrapArchitecture?: BootstrapArchitecture
    reason?: string
}

export interface OnboardingArtifact {
    sourceFingerprint?: string
    artifactId: string
    version: string
    platform: string
    arch: string
    sha256: string
    provenance: string
}

export interface OnboardingCandidates {
    candidates: OnboardingCandidate[]
    artifacts: OnboardingArtifact[]
}

export interface OnboardingAccessResult {
    candidates: OnboardingCandidate[]
}

export interface OnboardingAccessRequest {
    purpose?: 'cable'
    candidateIds: string[]
    username: string
    auth: OnboardingAuth
    startupLifetime: OnboardingStartupLifetime
    keyPath?: string
    password?: string
    passphrase?: string
    elevationPassword?: string
    privateKeyBase64?: string
    credentialProvider?: 'controller-ssh-config'
    credentialPurpose?: 'enrolled-peer-upgrade'
    publicKeySha256?: string
    credentialExpiresAt?: number
}

export interface OnboardingAcceptedHostKey {
    candidateId: string
    sha256: string
}

export interface OnboardingReviewRequest {
    candidateIds: string[]
    artifactId?: string
    acceptedHostKeys: OnboardingAcceptedHostKey[]
}

export interface OnboardingInstallationSummary {
    nodeId: string
    clusterId: string
    version: string
    sourceFingerprint: string
    unit: string
    bundle: string
    legacyRollbackSha256?: string
}

export interface OnboardingReviewTarget {
    action?: 'upgrade'
    existingInstallation?: OnboardingInstallationSummary
    startupLifetime: OnboardingStartupLifetime
    needsLinger: boolean
    candidateId: string
    label: string
    address: string
    port: number
    accessId: string
    accessLabel: string
    hostname?: string
    platform?: string
    arch?: string
    hostKeySha256?: string
    artifact?: OnboardingArtifact
    status: 'ready' | 'blocked'
    reason?: string
}

export interface OnboardingReview {
    reviewId: string
    expiresAt: number
    controllerNodeId: string
    targetClusterId: string
    targets: OnboardingReviewTarget[]
    canApprove: boolean
}

export interface OnboardingTargetState {
    candidateId: string
    stage: string
    message?: string
    nodeId?: string
    canRetry: boolean
    canCancel: boolean
    cleanupConfirmed: boolean
}

export interface OnboardingOperation {
    operationId: string
    reviewId: string
    targetClusterId: string
    revision: number
    state: string
    targets: OnboardingTargetState[]
    startedAt: number
    finishedAt?: number
}

export interface OnboardingOperationRequest {
    operationId: string
    candidateId?: string
}

export type OnboardingEnrollment = 'ssh-ready' | 'bootstrap-required'
export type BootstrapPlatform = 'windows' | 'darwin' | 'linux'
export type BootstrapArchitecture = 'amd64' | 'arm64'
export type BootstrapRole = 'auto' | 'desktop' | 'headless'
export type BootstrapRuntimeOwner = Exclude<BootstrapRole, 'auto'>
export type BootstrapLane = 'quick-connect' | 'zero-touch'
export type BootstrapDecision = 'apply' | 'repair-owned' | 'no-op' | 'refuse-foreign'
export type BootstrapPhase = 'inspect' | 'review' | 'apply' | 'verify' | 'complete' | 'blocked'
export type BootstrapOwnership = 'absent' | 'owned' | 'foreign' | 'not-applicable' | 'unavailable'
export type BootstrapResourceKind =
    | 'ssh-service'
    | 'firewall'
    | 'authorized-key'
    | 'helper'
    | 'pair-artifact'
    | 'active-role'
export type BootstrapPublicKeyAlgorithm = 'ssh-ed25519' | 'ssh-rsa' | 'ecdsa-sha2-nistp256'
export type BootstrapHelperAction = 'inspect' | 'apply' | 'verify' | 'rank-reconcile'

export interface OnboardingDiscoveryScope {
    scopeId: string
    interface: string
    localAddress: string
    cidr: string
    eligible: boolean
    reason?: string
}

export interface OnboardingScopes {
    scopes: OnboardingDiscoveryScope[]
}

export interface OnboardingDiscoverRequest {
    scopeId: string
    cancel?: boolean
}

export interface BootstrapTarget {
    platform: BootstrapPlatform
    architecture: BootstrapArchitecture
}

export interface BootstrapAccountIdentity {
    name: string
    homePath: string
    authorizedKeysPath: string
}

export interface BootstrapPublicKeyIdentity {
    algorithm: BootstrapPublicKeyAlgorithm
    material: string
    fingerprintSha256: string
}

export interface BootstrapSshEndpoint {
    address: string
    port: number
}

export interface BootstrapArtifactIdentity {
    id: string
    version: string
    sha256: string
    path: string
}

export interface BootstrapBinding {
    target: BootstrapTarget
    lane: BootstrapLane
    role: BootstrapRole
    runtimeOwner: BootstrapRuntimeOwner
    account: BootstrapAccountIdentity
    controllerKey: BootstrapPublicKeyIdentity
    endpoint: BootstrapSshEndpoint
    product: BootstrapArtifactIdentity
    helper: BootstrapArtifactIdentity
}

export interface BootstrapRequest {
    schemaVersion: 1
    operationId: string
    binding: BootstrapBinding
}

export interface BootstrapEndpointObservation {
    ownership: BootstrapOwnership
    endpoint: BootstrapSshEndpoint | null
}

export interface BootstrapAuthorizedKeyIdentity {
    accountName: string
    path: string
    fingerprintSha256: string
}

export interface BootstrapAuthorizedKeyObservation {
    ownership: BootstrapOwnership
    identity: BootstrapAuthorizedKeyIdentity | null
}

export interface BootstrapArtifactObservation {
    ownership: BootstrapOwnership
    identity: BootstrapArtifactIdentity | null
}

export interface BootstrapRuntimeOwnerObservation {
    ownership: BootstrapOwnership
    owner: BootstrapRuntimeOwner
}

export interface BootstrapObservations {
    sshService: BootstrapEndpointObservation
    firewall: BootstrapEndpointObservation
    authorizedKey: BootstrapAuthorizedKeyObservation
    helper: BootstrapArtifactObservation
    product: BootstrapArtifactObservation
    runtimeOwners: BootstrapRuntimeOwnerObservation[]
}

export interface BootstrapPlan {
    schemaVersion: 1
    operationId: string
    phase: 'review'
    decision: BootstrapDecision
    binding: BootstrapBinding
    observed: BootstrapObservations
    actions: BootstrapResourceKind[]
}

export interface BootstrapStatus {
    schemaVersion: 1
    operationId: string
    phase: BootstrapPhase
    decision?: BootstrapDecision
    binding: BootstrapBinding
    observed: BootstrapObservations
}

export interface BootstrapReceipt {
    schemaVersion: 1
    operationId: string
    phase: 'complete'
    decision: Exclude<BootstrapDecision, 'refuse-foreign'>
    binding: BootstrapBinding
    verified: BootstrapObservations
}

export interface BootstrapCatalogArtifact {
    identity: BootstrapArtifactIdentity
    fileName: string
    size: number
    provenance: 'official-release' | 'engineering'
    signature: BootstrapCatalogSignature
}

interface BootstrapCatalogSignature {
    status: 'unsigned' | 'signed'
    kind: 'none' | 'authenticode' | 'apple-code-sign' | 'detached-release'
    identity: string
    notarized: boolean
    signatureFile: string
    checksumFile: string
    contentSHA256: string
    contentSize: number
}

interface BootstrapCatalogCombination {
    fileName: string
    size: number
    sha256: string
    provenance: 'official-release' | 'engineering'
    signature: BootstrapCatalogSignature
}

export interface BootstrapCatalogTarget {
    target: BootstrapTarget
    roles: BootstrapRuntimeOwner[]
    bootstrap: BootstrapCatalogArtifact
    helper: BootstrapCatalogArtifact
    product: BootstrapCatalogArtifact
    combination: BootstrapCatalogCombination
}

export interface BootstrapCatalog {
    schemaVersion: 1
    integrity: {
        checksumAlgorithm: 'sha256'
        checksumFile: string
        signature: BootstrapCatalogSignature
    }
    targets: BootstrapCatalogTarget[]
}

export interface BootstrapControllerKeys {
    schemaVersion: 1
    keys: BootstrapPublicKeyIdentity[]
}

export interface BootstrapHelperResponse {
    schemaVersion: 1
    operationId?: string
    action?: BootstrapHelperAction
    accepted: boolean
    status?: BootstrapStatus
    receipt?: BootstrapReceipt
    reason?: string
}

export interface BootstrapTargetReference {
    candidateId: string
    accessId: string
    hostKeySha256: string
}

export interface BootstrapRequestInvoke extends BootstrapTargetReference {
    request: BootstrapRequest
}

export interface BootstrapPlanInvoke extends BootstrapTargetReference {
    plan: BootstrapPlan
}

export interface BootstrapOperationInvoke extends BootstrapTargetReference {
    operationId: string
}
