// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { createElement } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import bootstrapGolden from '../fixtures/bootstrap-golden.json'
import {
    parseBootstrapPlan,
    parseBootstrapReceipt,
    parseBootstrapRequest,
    parseBootstrapStatus
} from '@/shared/utils/onboarding-live'
import { SetupStatus, SetupStepper } from '@/ui/components/ClusterSettings/SetupJourneyAdapters'
import SetupEnrollmentLane from '@/ui/components/ClusterSettings/SetupEnrollmentLane'
import { useOnboardingStore } from '@/ui/stores/onboarding.store'

const candidateId = 'a'.repeat(32)
const accessId = 'b'.repeat(32)
const hostKeySha256 = 'SHA256:fixture-host-key'

describe('setup journey adapters', () => {
    it('renders an accessible four-step state without unavailable Foundations exports', () => {
        const markup = renderToStaticMarkup(
            createElement(SetupStepper, {
                activeStep: 2,
                steps: [
                    'Prepare device',
                    'Verify bootstrap',
                    'Authorize key',
                    'Install or update PAIR'
                ]
            })
        )
        expect(markup).toContain('aria-label="Device setup progress"')
        expect(markup).toContain('aria-current="step"')
        expect(markup).toContain('Authorize key')
    })

    it('announces busy and terminal setup status semantically', () => {
        const markup = renderToStaticMarkup(
            createElement(
                SetupStatus,
                { title: 'Bootstrap verified', tone: 'success', busy: false },
                'Target-produced state is ready.'
            )
        )
        expect(markup).toContain('role="status"')
        expect(markup).toContain('aria-live="polite"')
        expect(markup).toContain('Bootstrap verified')
    })

    it('renders the complete collapsed four-step enrollment journey', () => {
        const markup = renderToStaticMarkup(createElement(SetupEnrollmentLane))
        for (const step of [
            'Prepare device',
            'Verify bootstrap',
            'Authorize key',
            'Install or update PAIR'
        ])
            expect(markup).toContain(step)
        expect(markup).toContain('Set up device')
        expect(markup).toContain('Advanced setup controls')
    })
})

describe('bootstrap store transitions', () => {
    const request = parseBootstrapRequest(bootstrapGolden.request)
    const inspectStatus = parseBootstrapStatus(bootstrapGolden.status)
    const plan = parseBootstrapPlan(bootstrapGolden.plan)
    const applyStatus = parseBootstrapStatus({
        ...bootstrapGolden.status,
        phase: 'apply',
        decision: 'apply'
    })
    const receipt = parseBootstrapReceipt(bootstrapGolden.receipt)
    const reference = { candidateId, accessId, hostKeySha256 }

    beforeEach(() => {
        useOnboardingStore.setState({
            bootstrapRequest: null,
            bootstrapStatus: null,
            bootstrapPlan: null,
            bootstrapReceipt: null,
            pending: null,
            error: null
        })
        vi.stubGlobal('window', {
            pairApi: {
                setup: {
                    inspectBootstrap: vi.fn().mockResolvedValue(inspectStatus),
                    reviewBootstrap: vi.fn().mockResolvedValue(plan),
                    applyBootstrap: vi.fn().mockResolvedValue(applyStatus),
                    getBootstrapStatus: vi.fn().mockResolvedValue(applyStatus),
                    recoverBootstrap: vi.fn().mockResolvedValue(applyStatus),
                    verifyBootstrap: vi.fn().mockResolvedValue(receipt)
                }
            }
        })
    })

    it('advances through inspect, review, apply, recovery, and verify state', async () => {
        await useOnboardingStore.getState().inspectBootstrap({ ...reference, request })
        expect(useOnboardingStore.getState().bootstrapStatus?.phase).toBe('inspect')

        await useOnboardingStore.getState().reviewBootstrap({ ...reference, request })
        expect(useOnboardingStore.getState().bootstrapPlan?.decision).toBe('apply')

        await useOnboardingStore.getState().applyBootstrap({ ...reference, plan })
        expect(useOnboardingStore.getState().bootstrapStatus?.phase).toBe('apply')

        await useOnboardingStore
            .getState()
            .recoverBootstrap({ ...reference, operationId: plan.operationId })
        expect(useOnboardingStore.getState().bootstrapStatus?.operationId).toBe(plan.operationId)

        await useOnboardingStore.getState().verifyBootstrap({ ...reference, plan })
        expect(useOnboardingStore.getState().bootstrapReceipt?.phase).toBe('complete')
        expect(useOnboardingStore.getState().pending).toBeNull()
    })
})
