// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

export type OnboardingHistoryClassification = 'history-only' | 'current' | 'invalid'

export interface OnboardingHistoryOperation {
    operationId: string
    state: string
    classification: OnboardingHistoryClassification
    targetCount: number
    finishedAt?: number
    mutationAllowed: false
}

export interface OnboardingHistorySummary {
    total: number
    historyOnly: number
    current: number
    invalid: number
    recoveryRequired: boolean
    diagnosticRecoveryRequired: boolean
    discoveryBlocked: boolean
    mutationSupported: false
    operations: OnboardingHistoryOperation[]
}
