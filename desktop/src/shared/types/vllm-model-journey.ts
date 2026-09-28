// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

export interface VllmExactModelRequest {
    nodeId: string
    model: string
    operationId: string
}

export interface VllmDistributionRequest extends VllmExactModelRequest {
    sourceNodeId: string
}

export interface VllmRuntimePrepareRequest {
    nodeId: string
    operationId: string
}

/** Renderer-safe retained receipt. Per-file paths never cross this boundary. */
export interface VllmModelReceipt {
    id: string
    source: 'huggingface'
    revision: string
    license: string
    metadataSha256: string
    digest: string
    bytes: number
}

export interface VllmOperationCancelResult {
    accepted: boolean
    operationId: string
}

/** Renderer-safe provider mismatch. Host filesystem paths/packages are omitted. */
export interface VllmProviderMismatch {
    name: string
    reason: string
    expectedSha256?: string
    observedSha256?: string
    expectedBytes?: number
    observedBytes?: number
}

export interface VllmProviderObservation {
    qualified: boolean
    profileId?: string
    expectedClosureSha256: string
    observedClosureSha256?: string
    allowedClosureSha256: string[]
    mismatches: VllmProviderMismatch[]
}

export interface VllmRuntimePrepareResult {
    nodeId: string
    operationId: string
    state: string
    recipeId: string
    recipeSha256: string
    runtimeVersion: string
    artifactCount: number
    artifactBytes: number
    resumed: boolean
    activeRuntime?: string
    previousRuntime?: string
    message?: string
    provider: VllmProviderObservation
}
