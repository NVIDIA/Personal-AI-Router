// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

export type DiagnosticMPIRecipe =
    | 'pair-two-spark-nccl-socket-smoke-v1'
    | 'pair-two-spark-nccl-socket-correctness-v2'
    | 'pair-three-spark-nccl-socket-correctness-v3'

export interface DiagnosticMPIManagedTarget {
    nodeId: string
    local: boolean
}

/** Safe renderer projection of one adopted NCCL build registry entry. */
export interface DiagnosticMPIManagedRuntime {
    buildOperationId: string
    adoptedAt: number
    adopted: boolean
    runtimeValidated: boolean
    runAvailable: boolean
    targets: DiagnosticMPIManagedTarget[]
}

export interface DiagnosticMPIManagedInventory {
    records: DiagnosticMPIManagedRuntime[]
    recoveryRequired: boolean
}

export interface DiagnosticMPISelection {
    buildOperationId: string
    nodeIds: string[]
}

/** Only NCCL Socket moves to the fabric; MPI launch and SSH stay on management. */
export type DiagnosticMPINetwork = 'management' | 'fabric'

export interface DiagnosticMPIReviewRequest extends DiagnosticMPISelection {
    network: DiagnosticMPINetwork
}

export type DiagnosticMPIFabricRecipe =
    | 'spark-two-node-temporary-addresses-v1'
    | 'spark-three-node-ring-routed-v2'

export interface DiagnosticMPIReviewFabric {
    operationId: string
    qualificationDigest: string
    recipeId: DiagnosticMPIFabricRecipe
}

/** A fabric review's interface and address are the node's NCCL Socket end. */
export interface DiagnosticMPIReviewTarget {
    nodeId: string
    interface: string
    address: string
}

export interface DiagnosticMPIReview {
    reviewId: string
    buildOperationId: string
    operationId: string
    groupId: string
    ownerNodeId: string
    network: DiagnosticMPINetwork
    transport: 'socket'
    recipeId: DiagnosticMPIRecipe
    /** Present exactly when `network` is `fabric`. */
    fabric?: DiagnosticMPIReviewFabric
    expiresAt: number
    targets: DiagnosticMPIReviewTarget[]
}

export interface DiagnosticMPISample {
    bytes: number
    algorithmGBps: number
    busGBps: number
    latencyUs: number
    wrong: number
}

export type DiagnosticMPIOperationState =
    | 'preparing'
    | 'running'
    | 'cancelling'
    | 'passed'
    | 'failed'
    | 'cancelled'

export interface DiagnosticMPIOperation {
    operationId: string
    groupId: string
    ownerNodeId: string
    preset: 'nccl-smoke'
    recipeId: DiagnosticMPIRecipe
    state: DiagnosticMPIOperationState
    startedAt: number
    finishedAt?: number
    message: string
    cleanupConfirmed: boolean
    memberNodeIds: string[]
    samples: DiagnosticMPISample[]
}

export interface DiagnosticMPIOperationBinding {
    operationId: string
    groupId: string
    ownerNodeId: string
    memberNodeIds: string[]
    recipeId: DiagnosticMPIRecipe
}

export interface DiagnosticMPIApproveRequest extends DiagnosticMPIOperationBinding {
    reviewId: string
}

export interface DiagnosticMPIRecoveryReference extends DiagnosticMPIApproveRequest {
    buildOperationId: string
    recipeId: DiagnosticMPIRecipe
}

export interface DiagnosticMPIRecovery {
    reference: DiagnosticMPIRecoveryReference | null
    operation: DiagnosticMPIOperation | null
    recoveryRequired: boolean
}

export interface DiagnosticMPIReviewClosure {
    operation: DiagnosticMPIOperation | null
    reviewClosed: boolean
}
