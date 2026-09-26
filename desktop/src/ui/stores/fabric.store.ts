// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { create } from 'zustand'
import type { IFabricApi } from '@/ui/api/fabric-api'
import type {
    CableCleanupReview,
    CableRetainedRuns,
    CableReview,
    CableRun,
    FabricOperation,
    FabricInventorySnapshot,
    FabricReview,
    FabricSelection
} from '@/shared/types/fabric'
import { cableSucceeded, parseFabricSelection } from '@/shared/utils/fabric'
import getErrorString from '@/shared/utils/get-error-string'

type Pending =
    | 'cable-review'
    | 'cable-approve'
    | 'cable-cancel'
    | 'cable-cleanup-review'
    | 'cable-cleanup-verify'
    | 'cable-cleanup-cancel'
    | 'fabric-review'
    | 'fabric-inspect'
    | 'fabric-approve'
    | 'fabric-cancel'
    | 'fabric-recover'
    | null

interface FabricState {
    inventory: FabricInventorySnapshot | null
    retained: CableRetainedRuns | null
    cableReview: CableReview | null
    cableRun: CableRun | null
    cleanupReview: CableCleanupReview | null
    cableKnown: boolean
    fabricReview: FabricReview | null
    fabricOperation: FabricOperation | null
    fabricKnown: boolean
    selection: FabricSelection | null
    pending: Pending
    error: string
    refreshInventory(nodeIds: string[]): Promise<void>
    refreshRetained(): Promise<void>
    reviewCable(selection: FabricSelection): Promise<void>
    approveCable(): Promise<void>
    refreshCable(identity?: { runId?: string; reviewId?: string }): Promise<void>
    cancelCable(): Promise<void>
    reviewCableCleanup(acceptedHostKeys?: FabricSelection['acceptedHostKeys']): Promise<void>
    verifyCableCleanup(): Promise<void>
    cancelCableCleanup(): Promise<void>
    reviewFabric(inspectSelectedProfiles?: boolean): Promise<void>
    approveFabric(selectedPortPauseApproved: boolean): Promise<void>
    refreshFabric(operationId?: string): Promise<void>
    cancelFabric(): Promise<void>
    recoverFabric(): Promise<void>
    discardReview(): void
    reset(): void
}

let api: IFabricApi | null = null
let epoch = 0

export function bindFabricApi(next: IFabricApi): void {
    api = next
}

function requireApi(): IFabricApi {
    if (!api) throw new Error('Cable and fabric API is unavailable.')
    return api
}

const initial = {
    inventory: null,
    retained: null,
    cableReview: null,
    cableRun: null,
    cleanupReview: null,
    cableKnown: false,
    fabricReview: null,
    fabricOperation: null,
    fabricKnown: false,
    selection: null,
    pending: null,
    error: ''
} satisfies Pick<
    FabricState,
    | 'retained'
    | 'inventory'
    | 'cableReview'
    | 'cableRun'
    | 'cleanupReview'
    | 'cableKnown'
    | 'fabricReview'
    | 'fabricOperation'
    | 'fabricKnown'
    | 'selection'
    | 'pending'
    | 'error'
>

export const useFabricStore = create<FabricState>((set, get) => ({
    ...initial,

    refreshInventory: async nodeIds => {
        if (nodeIds.length < 1 || nodeIds.length > 3) {
            set({ inventory: null })
            return
        }
        try {
            set({ inventory: await requireApi().getInventory(nodeIds) })
        } catch (error) {
            set({ inventory: null, error: getErrorString(error) })
        }
    },

    refreshRetained: async () => {
        try {
            const retained = await requireApi().getRetainedCableRuns()
            set({ retained })
            const unfinished = [...retained.runs]
                .filter(run => !run.cleanupConfirmed)
                .sort((left, right) => right.startedAt - left.startedAt)[0]
            const latest = [...retained.runs].sort(
                (left, right) => right.startedAt - left.startedAt
            )[0]
            const selected = unfinished ?? latest
            if (selected) await get().refreshCable({ runId: selected.runId })
        } catch (error) {
            set({ error: getErrorString(error), cableKnown: false })
        }
        await reconcileRetainedFabric(get, set)
    },

    reviewCable: async selectionValue => {
        if (get().pending) return
        if (
            get().retained?.held ||
            (get().fabricOperation && !get().fabricOperation!.cleanupConfirmed)
        ) {
            set({
                error: 'A retained cable or fabric cleanup hold must be resolved before another check.'
            })
            return
        }
        let selection: FabricSelection
        try {
            selection = parseFabricSelection(selectionValue)
        } catch (error) {
            set({ error: getErrorString(error) })
            return
        }
        const revision = ++epoch
        set({ pending: 'cable-review', error: '', cableReview: null, selection })
        try {
            const cableReview = await requireApi().reviewCable(selection)
            if (revision === epoch)
                set({
                    cableReview,
                    cableRun: null,
                    cableKnown: true,
                    fabricReview: null,
                    fabricOperation: null,
                    fabricKnown: false
                })
        } catch (error) {
            if (revision === epoch) set({ error: getErrorString(error), cableKnown: false })
        } finally {
            if (revision === epoch) set({ pending: null })
        }
    },

    approveCable: async () => {
        const review = get().cableReview
        if (!review || !review.available || get().pending) return
        const revision = ++epoch
        // The review id is PAIR-issued recovery identity. The run itself stays
        // unknown until Start or the mandatory readback returns it.
        set({ pending: 'cable-approve', error: '', cableKnown: false, cableRun: null })
        try {
            const response = await requireApi().approveCable(review.reviewId)
            if (revision !== epoch) return
            if ('disposition' in response) {
                set({
                    cableReview: null,
                    cableKnown: true,
                    error: `Cable check was not started (${response.reason}). Review again.`
                })
            } else {
                set({ cableReview: null, cableRun: response, cableKnown: true })
            }
        } catch (error) {
            if (revision === epoch)
                set({
                    error: `${getErrorString(error)} The check will not be resent; reading its server-issued review identity.`
                })
        } finally {
            if (revision === epoch) set({ pending: null })
            await get().refreshCable({ reviewId: review.reviewId })
        }
    },

    refreshCable: async identity => {
        const current = get().cableRun
        const request =
            identity ??
            (current
                ? { runId: current.runId }
                : get().cableReview
                  ? { reviewId: get().cableReview!.reviewId }
                  : null)
        if (!request) return
        try {
            const cableRun = await requireApi().getCableStatus(request)
            set({
                cableRun,
                cableKnown: true,
                error: '',
                ...(cableRun.cleanupRecovery?.holdReleased ? { cleanupReview: null } : {})
            })
            const retained = get().retained
            if (
                cableRun.cleanupRecovery?.holdReleased &&
                retained?.held &&
                retained.runs.some(run => run.runId === cableRun.runId)
            ) {
                try {
                    set({ retained: await requireApi().getRetainedCableRuns() })
                } catch (error) {
                    set({ error: getErrorString(error) })
                }
            }
        } catch (error) {
            set({ cableKnown: false, error: getErrorString(error) })
        }
    },

    cancelCable: async () => {
        const run = get().cableRun
        if (!run || get().pending) return
        const revision = ++epoch
        set({ pending: 'cable-cancel', cableKnown: false, error: '' })
        try {
            const cableRun = await requireApi().cancelCable(run.runId)
            if (revision === epoch) set({ cableRun, cableKnown: true })
        } catch (error) {
            if (revision === epoch) set({ error: getErrorString(error) })
        } finally {
            if (revision === epoch) set({ pending: null })
            await get().refreshCable({ runId: run.runId })
            await get().refreshRetained()
        }
    },

    reviewCableCleanup: async acceptedHostKeys => {
        const run = get().cableRun
        if (!run || run.cleanupConfirmed || run.cleanupRecovery?.holdReleased || get().pending)
            return
        const revision = ++epoch
        set({ pending: 'cable-cleanup-review', cleanupReview: null, error: '' })
        try {
            const cleanupReview = await requireApi().reviewCableCleanup(run.runId, acceptedHostKeys)
            if (revision === epoch) set({ cleanupReview })
        } catch (error) {
            if (revision === epoch) set({ error: getErrorString(error) })
        } finally {
            if (revision === epoch) set({ pending: null })
        }
    },

    verifyCableCleanup: async () => {
        const run = get().cableRun
        const review = get().cleanupReview
        if (!run || !review?.available || review.runId !== run.runId || get().pending) return
        const revision = ++epoch
        set({ pending: 'cable-cleanup-verify', cableKnown: false, error: '' })
        try {
            const result = await requireApi().verifyCableCleanup(run.runId, review.reviewId)
            if (revision === epoch)
                set({ cableRun: result.run, cableKnown: true, cleanupReview: null })
        } catch (error) {
            if (revision === epoch) set({ error: getErrorString(error) })
        } finally {
            if (revision === epoch) set({ pending: null })
            await get().refreshCable({ runId: run.runId })
            await get().refreshRetained()
        }
    },

    cancelCableCleanup: async () => {
        const run = get().cableRun
        if (!run?.cleanupRecovery || get().pending) return
        const revision = ++epoch
        set({ pending: 'cable-cleanup-cancel', cableKnown: false, error: '' })
        try {
            const cableRun = await requireApi().cancelCableCleanup(run.runId)
            if (revision === epoch) set({ cableRun, cableKnown: true })
        } catch (error) {
            if (revision === epoch) set({ error: getErrorString(error) })
        } finally {
            if (revision === epoch) set({ pending: null })
            await get().refreshCable({ runId: run.runId })
        }
    },

    reviewFabric: async inspectSelectedProfiles => {
        const { selection, cableRun, pending } = get()
        if (!selection || !cableSucceeded(cableRun) || pending) {
            set({ error: 'A matched, cleaned-up cable check is required before fabric review.' })
            return
        }
        const revision = ++epoch
        set({
            pending: inspectSelectedProfiles ? 'fabric-inspect' : 'fabric-review',
            fabricReview: null,
            error: ''
        })
        try {
            const fabricReview = await requireApi().reviewFabric(selection, inspectSelectedProfiles)
            if (revision === epoch) set({ fabricReview })
        } catch (error) {
            if (revision === epoch) set({ error: getErrorString(error) })
        } finally {
            if (revision === epoch) set({ pending: null })
        }
    },

    approveFabric: async selectedPortPauseApproved => {
        const review = get().fabricReview
        if (!review || !review.executable || get().pending) return
        const revision = ++epoch
        // As with cable Start, the review id is the backend's operation id. Do
        // not fabricate a local applying state; settle it from RPC/status only.
        set({
            pending: 'fabric-approve',
            fabricKnown: false,
            fabricOperation: null,
            error: ''
        })
        try {
            const fabricOperation = await requireApi().approveFabric(
                review.reviewId,
                selectedPortPauseApproved
            )
            if (revision === epoch) set({ fabricReview: null, fabricOperation, fabricKnown: true })
        } catch (error) {
            if (revision === epoch)
                set({
                    error: `${getErrorString(error)} Approval will not be resent; reading the exact review identity.`
                })
        } finally {
            if (revision === epoch) set({ pending: null })
            await get().refreshFabric(review.reviewId)
        }
    },

    refreshFabric: async operationId => {
        const id = operationId ?? get().fabricOperation?.operationId
        if (!id) return
        const revision = epoch
        try {
            const fabricOperation = await requireApi().getFabricStatus(id)
            if (revision !== epoch || get().pending) return
            set({ fabricOperation, fabricKnown: true, error: '' })
        } catch (error) {
            if (revision !== epoch || get().pending) return
            set({ fabricKnown: false, error: getErrorString(error) })
        }
    },

    cancelFabric: async () => {
        await operateFabric(get, set, 'fabric-cancel', operationId =>
            requireApi().cancelFabric(operationId)
        )
    },

    recoverFabric: async () => {
        await operateFabric(get, set, 'fabric-recover', operationId =>
            requireApi().recoverFabric(operationId)
        )
    },

    discardReview: () => {
        if (get().pending) return
        epoch++
        set({
            cableReview: null,
            cableRun: null,
            cleanupReview: null,
            cableKnown: false,
            fabricReview: null,
            selection: null,
            error: ''
        })
    },

    reset: () => {
        epoch++
        set({ ...initial })
    }
}))

async function operateFabric(
    get: () => FabricState,
    set: (partial: Partial<FabricState>) => void,
    pending: 'fabric-cancel' | 'fabric-recover',
    send: (operationId: string) => Promise<FabricOperation>
): Promise<void> {
    const operation = get().fabricOperation
    if (!operation || get().pending || !get().fabricKnown) return
    const revision = ++epoch
    set({ pending, fabricKnown: false, error: '' })
    try {
        const fabricOperation = await send(operation.operationId)
        if (revision === epoch) set({ fabricOperation, fabricKnown: true })
    } catch (error) {
        if (revision === epoch) set({ error: getErrorString(error) })
    } finally {
        if (revision === epoch) set({ pending: null })
        await get().refreshFabric(operation.operationId)
        await reconcileRetainedFabric(get, set)
    }
}

async function reconcileRetainedFabric(
    get: () => FabricState,
    set: (partial: Partial<FabricState>) => void
): Promise<void> {
    const revision = epoch
    if (get().pending) return
    try {
        const operations = (await requireApi().getRetainedFabricOperations()).operations
        if (revision !== epoch || get().pending) return
        const currentId = get().fabricOperation?.operationId
        const current = currentId
            ? operations.find(operation => operation.operationId === currentId)
            : undefined
        set({
            fabricOperation: current ?? operations[0] ?? null,
            fabricKnown: true
        })
    } catch (error) {
        if (revision !== epoch || get().pending) return
        set({ fabricKnown: false, error: getErrorString(error) })
    }
}

function cableRunning(run: CableRun | null): boolean {
    return Boolean(
        run &&
        (['preparing', 'running', 'cancelling'].includes(run.state) ||
            (run.cleanupRecovery &&
                ['verifying', 'release-pending'].includes(run.cleanupRecovery.state)))
    )
}

function fabricMoving(operation: FabricOperation | null): boolean {
    return !!operation && ['applying', 'rolling-back'].includes(operation.state)
}

let viewers = 0
let watching = false
async function poll(): Promise<void> {
    const state = useFabricStore.getState()
    if (!viewers && !cableRunning(state.cableRun) && !fabricMoving(state.fabricOperation)) {
        watching = false
        return
    }
    if (!state.pending) {
        if (state.cableRun) await state.refreshCable({ runId: state.cableRun.runId })
        if (state.fabricOperation) {
            await state.refreshFabric(state.fabricOperation.operationId)
        }
        await reconcileRetainedFabric(useFabricStore.getState, useFabricStore.setState)
    }
    setTimeout(() => void poll(), 2000)
}

export function watchFabric(): () => void {
    viewers++
    if (!watching) {
        watching = true
        void useFabricStore
            .getState()
            .refreshRetained()
            .finally(() => void poll())
    }
    return () => {
        viewers = Math.max(0, viewers - 1)
    }
}
