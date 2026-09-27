// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { create } from 'zustand'
import type {
    DiagnosticMPIApproveRequest,
    DiagnosticMPIManagedInventory,
    DiagnosticMPIOperation,
    DiagnosticMPIOperationBinding,
    DiagnosticMPIRecovery,
    DiagnosticMPIRecoveryReference,
    DiagnosticMPIReview,
    DiagnosticMPIReviewRequest,
    DiagnosticMPISelection
} from '@/shared/types/diagnostic-mpi'
import getErrorString from '@/shared/utils/get-error-string'

type Pending = 'inventory' | 'review' | 'approve' | 'status' | 'cancel' | 'recover' | 'close'

interface DiagnosticMPIState {
    inventory: DiagnosticMPIManagedInventory | null
    review: DiagnosticMPIReview | null
    operation: DiagnosticMPIOperation | null
    recovery: DiagnosticMPIRecovery | null
    approvalAttemptedReviewId: string | null
    approvalRecoverySelection: DiagnosticMPISelection | null
    operationKnown: boolean
    pending: Pending | null
    error: string
    refreshInventory(): Promise<void>
    requestReview(request: DiagnosticMPIReviewRequest): Promise<void>
    approveReview(): Promise<void>
    refreshOperation(): Promise<void>
    cancelOperation(): Promise<void>
    recover(selection: DiagnosticMPISelection): Promise<void>
    closeReview(): Promise<void>
    reset(): void
}

const initial = {
    inventory: null,
    review: null,
    operation: null,
    recovery: null,
    approvalAttemptedReviewId: null,
    approvalRecoverySelection: null,
    operationKnown: false,
    pending: null,
    error: ''
} satisfies Pick<
    DiagnosticMPIState,
    | 'inventory'
    | 'review'
    | 'operation'
    | 'recovery'
    | 'approvalAttemptedReviewId'
    | 'approvalRecoverySelection'
    | 'operationKnown'
    | 'pending'
    | 'error'
>

let epoch = 0

function operationBinding(operation: DiagnosticMPIOperation): DiagnosticMPIOperationBinding {
    return {
        operationId: operation.operationId,
        groupId: operation.groupId,
        ownerNodeId: operation.ownerNodeId,
        memberNodeIds: operation.memberNodeIds,
        recipeId: operation.recipeId
    }
}

function reviewApproval(review: DiagnosticMPIReview): DiagnosticMPIApproveRequest {
    return {
        reviewId: review.reviewId,
        operationId: review.operationId,
        groupId: review.groupId,
        ownerNodeId: review.ownerNodeId,
        memberNodeIds: review.targets.map(target => target.nodeId).sort(),
        recipeId: review.recipeId
    }
}

function reviewReference(review: DiagnosticMPIReview): DiagnosticMPIRecoveryReference {
    return {
        ...reviewApproval(review),
        buildOperationId: review.buildOperationId,
        recipeId: review.recipeId
    }
}

function api() {
    if (!window.pairApi) throw new Error('PAIR service is unavailable.')
    return window.pairApi.setup
}

export const useDiagnosticMPIStore = create<DiagnosticMPIState>((set, get) => ({
    ...initial,

    refreshInventory: async () => {
        if (get().pending) return
        const revision = ++epoch
        set({ pending: 'inventory', error: '' })
        try {
            const inventory = await api().getDiagnosticMpiInventory()
            if (revision === epoch) set({ inventory })
        } catch (error) {
            if (revision === epoch) set({ error: getErrorString(error) })
        } finally {
            if (revision === epoch) set({ pending: null })
        }
    },

    requestReview: async request => {
        if (get().pending || get().review || get().approvalRecoverySelection) return
        const revision = ++epoch
        set({
            pending: 'review',
            operation: null,
            operationKnown: false,
            recovery: null,
            error: ''
        })
        try {
            const review = await api().reviewDiagnosticMpi(request)
            if (revision === epoch)
                set(current => ({
                    review,
                    approvalAttemptedReviewId:
                        current.approvalAttemptedReviewId === review.reviewId
                            ? review.reviewId
                            : null
                }))
        } catch (error) {
            if (revision === epoch) set({ error: getErrorString(error) })
        } finally {
            if (revision === epoch) set({ pending: null })
        }
    },

    approveReview: async () => {
        const review = get().review
        if (!review || get().pending || get().approvalAttemptedReviewId === review.reviewId) return
        const revision = ++epoch
        set({
            pending: 'approve',
            approvalAttemptedReviewId: review.reviewId,
            approvalRecoverySelection: {
                buildOperationId: review.buildOperationId,
                nodeIds: review.targets.map(target => target.nodeId).sort()
            },
            operationKnown: false,
            error: ''
        })
        try {
            const operation = await api().approveDiagnosticMpi(reviewApproval(review))
            if (revision === epoch)
                set({
                    review: null,
                    recovery: null,
                    approvalAttemptedReviewId: null,
                    approvalRecoverySelection: null,
                    operation,
                    operationKnown: true
                })
        } catch (error) {
            if (revision === epoch)
                set({
                    review: null,
                    error: `${getErrorString(error)} Start will not be resent; recover this exact build and node selection.`
                })
        } finally {
            if (revision === epoch) set({ pending: null })
        }
    },

    refreshOperation: async () => {
        const operation = get().operation
        if (!operation || get().pending) return
        const revision = ++epoch
        set({ pending: 'status', operationKnown: false, error: '' })
        try {
            const current = await api().getDiagnosticMpiStatus(operationBinding(operation))
            if (revision === epoch) set({ operation: current, operationKnown: true })
        } catch (error) {
            if (revision === epoch) set({ error: getErrorString(error) })
        } finally {
            if (revision === epoch) set({ pending: null })
        }
    },

    cancelOperation: async () => {
        const operation = get().operation
        if (!operation || get().pending || !get().operationKnown) return
        const revision = ++epoch
        set({ pending: 'cancel', operationKnown: false, error: '' })
        try {
            const current = await api().cancelDiagnosticMpi(operationBinding(operation))
            if (revision === epoch) set({ operation: current, operationKnown: true })
        } catch (error) {
            if (revision === epoch) set({ error: getErrorString(error) })
        } finally {
            if (revision === epoch) set({ pending: null })
        }
    },

    recover: async selection => {
        if (get().pending || get().review) return
        const exactSelection = get().approvalRecoverySelection ?? selection
        const revision = ++epoch
        set({ pending: 'recover', operation: null, operationKnown: false, error: '' })
        try {
            const recovery = await api().recoverDiagnosticMpi(exactSelection)
            if (revision === epoch)
                set({
                    recovery,
                    operation: recovery.operation,
                    approvalAttemptedReviewId: recovery.recoveryRequired
                        ? get().approvalAttemptedReviewId
                        : null,
                    approvalRecoverySelection: recovery.recoveryRequired
                        ? get().approvalRecoverySelection
                        : null,
                    operationKnown: recovery.operation !== null
                })
        } catch (error) {
            if (revision === epoch) set({ error: getErrorString(error) })
        } finally {
            if (revision === epoch) set({ pending: null })
        }
    },

    closeReview: async () => {
        const review = get().review
        const recovered = get().recovery?.reference
        const reference = review ? reviewReference(review) : recovered
        if (!reference || get().pending) return
        const revision = ++epoch
        set({ pending: 'close', operationKnown: false, error: '' })
        try {
            const result = await api().closeDiagnosticMpiReview(reference)
            if (revision === epoch)
                set({
                    review: null,
                    recovery: null,
                    approvalAttemptedReviewId: null,
                    approvalRecoverySelection: null,
                    operation: result.operation,
                    operationKnown: result.operation !== null
                })
        } catch (error) {
            if (revision === epoch) set({ error: getErrorString(error) })
        } finally {
            if (revision === epoch) set({ pending: null })
        }
    },

    reset: () => {
        epoch++
        set({ ...initial })
    }
}))

function moving(operation: DiagnosticMPIOperation | null): boolean {
    return !!operation && ['preparing', 'running', 'cancelling'].includes(operation.state)
}

let viewers = 0
let watching = false

async function poll(): Promise<void> {
    const state = useDiagnosticMPIStore.getState()
    if (!viewers && !moving(state.operation)) {
        watching = false
        return
    }
    if (!state.pending && moving(state.operation)) await state.refreshOperation()
    setTimeout(() => void poll(), 2000)
}

export function watchDiagnosticMPI(): () => void {
    viewers++
    if (!watching) {
        watching = true
        void useDiagnosticMPIStore
            .getState()
            .refreshInventory()
            .finally(() => void poll())
    }
    return () => {
        viewers = Math.max(0, viewers - 1)
    }
}
