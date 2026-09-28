// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { createElement, type ComponentType } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { beforeEach, describe, expect, it } from 'vitest'
import type { OnboardingHistorySummary } from '@/shared/types/onboarding-history'
import SetupServingCard from '@/ui/components/ClusterSettings/SetupServingCard'
import { useDiagnosticMPIReconcileStore } from '@/ui/stores/diagnostic-mpi-reconcile.store'
import type { VllmServingReadiness } from '@/ui/utils/vllm-serving-readiness'

const history: OnboardingHistorySummary = {
    total: 0,
    historyOnly: 0,
    current: 0,
    invalid: 0,
    recoveryRequired: false,
    diagnosticRecoveryRequired: false,
    discoveryBlocked: false,
    mutationSupported: false,
    operations: []
}
const readiness: VllmServingReadiness = {
    nodes: [],
    servingReplicas: 0,
    reusableModels: 0,
    replicatedModels: 0,
    controlsNodeId: null,
    distributionSupported: false
}
const SetupCard = SetupServingCard as ComponentType<{
    initialHistory: OnboardingHistorySummary
    initialReadiness: VllmServingReadiness
}>

describe('Setup diagnostic cleanup recovery action', () => {
    beforeEach(() => useDiagnosticMPIReconcileStore.getState().reset())

    it('appears only for the typed diagnostic recovery signal', () => {
        const clear = renderToStaticMarkup(
            createElement(SetupCard, {
                initialHistory: history,
                initialReadiness: readiness
            })
        )
        const held = renderToStaticMarkup(
            createElement(SetupCard, {
                initialHistory: { ...history, diagnosticRecoveryRequired: true },
                initialReadiness: readiness
            })
        )
        expect(clear).not.toContain('Reconcile diagnostic cleanup')
        expect(held).toContain('Reconcile diagnostic cleanup')
        expect(held).toContain('takes no account credentials')
    })
})
