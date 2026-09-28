// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

export type DiagnosticMPIReconcileState = 'completed' | 'recovery-required'
export type DiagnosticMPIReconcileOperationState = 'cleanup-confirmed' | 'recovery-required'
export type DiagnosticMPIReconcileCode =
    | 'cleanup-unconfirmed'
    | 'binding-unavailable'
    | 'inventory-unconfirmed'
    | 'participant-unavailable'

export type DiagnosticMPIReconcileRequest = Record<string, never>

export interface DiagnosticMPIReconcileOperation {
    nodeId: string
    operationId?: string
    ownerNodeId?: string
    state: DiagnosticMPIReconcileOperationState
    cleanupConfirmed: boolean
    code?: DiagnosticMPIReconcileCode
    message: string
}

export interface DiagnosticMPIReconcileResult {
    state: DiagnosticMPIReconcileState
    recoveryRequired: boolean
    operations: DiagnosticMPIReconcileOperation[]
}
