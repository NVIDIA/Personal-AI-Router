// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { create } from 'zustand'
import type {
    BootstrapCatalog,
    BootstrapControllerKeys,
    BootstrapOperationInvoke,
    BootstrapPlan,
    BootstrapPlanInvoke,
    BootstrapReceipt,
    BootstrapRequest,
    BootstrapRequestInvoke,
    BootstrapStatus,
    OnboardingAccessRequest,
    OnboardingArtifact,
    OnboardingCandidate,
    OnboardingDiscoverRequest,
    OnboardingOperation,
    OnboardingReview,
    OnboardingReviewRequest,
    OnboardingScopes
} from '@/shared/types/onboarding-live'
import getErrorString from '@/shared/utils/get-error-string'

type PendingAction =
    | 'candidates'
    | 'add-target'
    | 'access'
    | 'inspect'
    | 'approve'
    | 'status'
    | 'cancel'
    | 'retry'
    | 'scopes'
    | 'discover'
    | 'import'
    | 'bootstrap-metadata'
    | 'bootstrap-inspect'
    | 'bootstrap-review'
    | 'bootstrap-apply'
    | 'bootstrap-status'
    | 'bootstrap-recover'
    | 'bootstrap-verify'

interface OnboardingState {
    candidates: OnboardingCandidate[]
    artifacts: OnboardingArtifact[]
    review: OnboardingReview | null
    operation: OnboardingOperation | null
    scopes: OnboardingScopes | null
    bootstrapCatalog: BootstrapCatalog | null
    bootstrapControllerKeys: BootstrapControllerKeys | null
    bootstrapRequest: BootstrapRequest | null
    bootstrapStatus: BootstrapStatus | null
    bootstrapPlan: BootstrapPlan | null
    bootstrapReceipt: BootstrapReceipt | null
    pending: PendingAction | null
    error: string | null
    refreshCandidates(): Promise<void>
    addTarget(address: string, port: number, label?: string): Promise<void>
    authorizeAccess(request: OnboardingAccessRequest): Promise<void>
    inspect(request: OnboardingReviewRequest): Promise<void>
    approve(): Promise<void>
    refreshOperation(): Promise<void>
    cancel(candidateId?: string): Promise<void>
    retry(candidateId?: string): Promise<void>
    clearReview(): void
    loadScopes(): Promise<void>
    discover(request: OnboardingDiscoverRequest): Promise<void>
    importArtifact(file: string): Promise<void>
    loadBootstrapMetadata(): Promise<void>
    inspectBootstrap(request: BootstrapRequestInvoke): Promise<void>
    reviewBootstrap(request: BootstrapRequestInvoke): Promise<void>
    applyBootstrap(request: BootstrapPlanInvoke): Promise<void>
    refreshBootstrap(request: BootstrapOperationInvoke): Promise<void>
    recoverBootstrap(request: BootstrapOperationInvoke): Promise<void>
    verifyBootstrap(request: BootstrapPlanInvoke): Promise<void>
    clearBootstrap(): void
}

function mergeCandidates(
    current: OnboardingCandidate[],
    replacements: OnboardingCandidate[]
): OnboardingCandidate[] {
    const byId = new Map(current.map(candidate => [candidate.candidateId, candidate]))
    for (const candidate of replacements) byId.set(candidate.candidateId, candidate)
    return Array.from(byId.values()).sort((left, right) => left.label.localeCompare(right.label))
}

async function run(
    action: PendingAction,
    set: (value: Partial<OnboardingState>) => void,
    effect: () => Promise<Partial<OnboardingState>>
): Promise<void> {
    set({ pending: action, error: null })
    try {
        set({ ...(await effect()), pending: null })
    } catch (error) {
        set({ pending: null, error: getErrorString(error) })
    }
}

export const useOnboardingStore = create<OnboardingState>((set, get) => ({
    candidates: [],
    artifacts: [],
    review: null,
    operation: null,
    scopes: null,
    bootstrapCatalog: null,
    bootstrapControllerKeys: null,
    bootstrapRequest: null,
    bootstrapStatus: null,
    bootstrapPlan: null,
    bootstrapReceipt: null,
    pending: null,
    error: null,

    refreshCandidates: () =>
        run('candidates', set, async () => {
            const result = await window.pairApi.setup.getCandidates()
            return { candidates: result.candidates, artifacts: result.artifacts }
        }),

    addTarget: (address, port, label) =>
        run('add-target', set, async () => {
            const candidate = await window.pairApi.setup.addTarget(address, port, label)
            return { candidates: mergeCandidates(get().candidates, [candidate]), review: null }
        }),

    authorizeAccess: request =>
        run('access', set, async () => {
            const result = await window.pairApi.setup.authorizeAccess(request)
            return {
                candidates: mergeCandidates(get().candidates, result.candidates),
                review: null
            }
        }),

    inspect: request =>
        run('inspect', set, async () => ({
            review: await window.pairApi.setup.inspect(request),
            operation: null
        })),

    approve: () =>
        run('approve', set, async () => {
            const review = get().review
            if (!review || !review.canApprove) throw new Error('A ready setup review is required.')
            return { operation: await window.pairApi.setup.approve(review.reviewId) }
        }),

    refreshOperation: () =>
        run('status', set, async () => {
            const operation = get().operation
            if (!operation) throw new Error('A setup operation is required.')
            return {
                operation: await window.pairApi.setup.getOperation({
                    operationId: operation.operationId
                })
            }
        }),

    cancel: candidateId =>
        run('cancel', set, async () => {
            const operation = get().operation
            if (!operation) throw new Error('A setup operation is required.')
            return {
                operation: await window.pairApi.setup.cancel({
                    operationId: operation.operationId,
                    candidateId
                })
            }
        }),

    retry: candidateId =>
        run('retry', set, async () => {
            const operation = get().operation
            if (!operation) throw new Error('A setup operation is required.')
            return {
                operation: await window.pairApi.setup.retry({
                    operationId: operation.operationId,
                    candidateId
                })
            }
        }),

    clearReview: () => set({ review: null, error: null }),

    loadScopes: () =>
        run('scopes', set, async () => ({ scopes: await window.pairApi.setup.getScopes() })),

    discover: request =>
        run('discover', set, async () => {
            const result = await window.pairApi.setup.discover(request)
            return { candidates: mergeCandidates(get().candidates, result.candidates) }
        }),

    importArtifact: file =>
        run('import', set, async () => {
            const artifact = await window.pairApi.setup.importArtifact(file)
            const artifacts = get().artifacts.filter(
                item => item.artifactId !== artifact.artifactId
            )
            return { artifacts: [...artifacts, artifact] }
        }),

    loadBootstrapMetadata: () =>
        run('bootstrap-metadata', set, async () => {
            const [bootstrapCatalog, bootstrapControllerKeys] = await Promise.all([
                window.pairApi.setup.getBootstrapCatalog(),
                window.pairApi.setup.getBootstrapControllerKeys()
            ])
            return { bootstrapCatalog, bootstrapControllerKeys }
        }),

    inspectBootstrap: request =>
        run('bootstrap-inspect', set, async () => ({
            bootstrapRequest: request.request,
            bootstrapStatus: await window.pairApi.setup.inspectBootstrap(request),
            bootstrapPlan: null,
            bootstrapReceipt: null
        })),

    reviewBootstrap: request =>
        run('bootstrap-review', set, async () => ({
            bootstrapRequest: request.request,
            bootstrapPlan: await window.pairApi.setup.reviewBootstrap(request),
            bootstrapReceipt: null
        })),

    applyBootstrap: request =>
        run('bootstrap-apply', set, async () => ({
            bootstrapStatus: await window.pairApi.setup.applyBootstrap(request),
            bootstrapReceipt: null
        })),

    refreshBootstrap: request =>
        run('bootstrap-status', set, async () => ({
            bootstrapStatus: await window.pairApi.setup.getBootstrapStatus(request)
        })),

    recoverBootstrap: request =>
        run('bootstrap-recover', set, async () => ({
            bootstrapStatus: await window.pairApi.setup.recoverBootstrap(request)
        })),

    verifyBootstrap: request =>
        run('bootstrap-verify', set, async () => ({
            bootstrapReceipt: await window.pairApi.setup.verifyBootstrap(request)
        })),

    clearBootstrap: () =>
        set({
            bootstrapRequest: null,
            bootstrapStatus: null,
            bootstrapPlan: null,
            bootstrapReceipt: null,
            error: null
        })
}))
