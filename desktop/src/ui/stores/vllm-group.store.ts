// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Managed vLLM serving-group state.
 *
 * Two channels are kept apart on purpose. `known`/`status`/`error` are hold
 * truth: every vLLM effect fence reads them, an unknown status holds, and
 * only a fresh successful PAIR read releases. `review`/`check`/`pending`/
 * `uncertainStart`/`actionError` are the managed lifecycle a user drives.
 * Review and check are proposals, never execution; Start, Stop, Reconcile and
 * Cleanup are exact typed operations that PAIR may refuse, and each of them
 * marks ownership unknown until the next read confirms it.
 *
 * This store is the single writer of hold truth. The transport it drives is
 * pure: it parses replies and never touches state, so a stale in-flight read
 * cannot override a newer invalidation or effect result.
 */
import { create } from 'zustand'
import type { VllmGroupRunStatus, VllmGroupStatus } from '@/shared/types/vllm-group-status'
import type {
    VllmGroupCheck,
    VllmGroupElevation,
    VllmGroupOperation,
    VllmGroupReconcileRequest,
    VllmGroupReview,
    VllmGroupSelection
} from '@/shared/types/vllm-group'
import type { VllmGroupCleanupRequest } from '@/shared/types/vllm-group-cleanup'
import { VLLM_QWEN38_MODEL } from '@/shared/constants/vllm'
import { parseVllmGroupSelection } from '@/shared/utils/vllm-group'
import { vllmGroupCleanupBinding, vllmGroupHoldsTarget } from '@/shared/utils/vllm-group-cleanup'
import getErrorString from '@/shared/utils/get-error-string'
import { useConnectionStore } from '@/ui/stores/connection.store'

/** The typed, state-free transport the store drives. Bound by the renderer API; tests bind a fake. */
export interface VllmGroupApi {
    getServingGroupStatus(): Promise<VllmGroupStatus>
    reviewServingGroup(selection: VllmGroupSelection): Promise<VllmGroupReview>
    checkServingGroup(reviewId: string): Promise<VllmGroupCheck>
    startServingGroup(
        reviewId: string,
        elevation?: VllmGroupElevation[]
    ): Promise<VllmGroupRunStatus>
    stopServingGroup(operation: VllmGroupOperation): Promise<VllmGroupStatus>
    reconcileServingGroup(operation: VllmGroupReconcileRequest): Promise<VllmGroupStatus>
    requestServingGroupCleanup(request: VllmGroupCleanupRequest): Promise<VllmGroupStatus>
}

let api: VllmGroupApi | null = null
let watchingConnection = false
/**
 * Binds the transport and, once, the controller-identity watch. The watch is
 * installed here rather than at import so the store never touches the
 * connection store during module initialization (the renderer API, this
 * store and the connection store form an import cycle).
 */
export function bindVllmGroupApi(next: VllmGroupApi | null): void {
    api = next
    if (watchingConnection) return
    watchingConnection = true
    useConnectionStore.subscribe((connection, previous) => {
        if (
            connection.connected !== previous.connected ||
            connection.selfId !== previous.selfId ||
            connection.clusterId !== previous.clusterId
        )
            useVllmGroupStore.getState().invalidate()
    })
}
function requireApi(): VllmGroupApi {
    if (!api) throw new Error('Serving-group controls are unavailable until PAIR connects.')
    return api
}

function clearGroupElevation(elevation: VllmGroupElevation[] | undefined): void {
    for (const entry of elevation ?? []) {
        if (entry.elevationPassword !== undefined) entry.elevationPassword = ''
    }
}

export type VllmGroupPending =
    | 'review'
    | 'check'
    | 'start'
    | 'stop'
    | 'reconcile'
    | 'cleanup'
    | null

interface VllmGroupState {
    known: boolean
    status: VllmGroupStatus | null
    error: string | null
    /** Last run PAIR reported under the current owner, kept for display while ownership is unknown. Never authority. */
    lastRun: VllmGroupRunStatus | null
    review: VllmGroupReview | null
    check: VllmGroupCheck | null
    pending: VllmGroupPending
    /** A Start was consumed and no fresh status read has yet reported what PAIR retained. */
    uncertainStart: boolean
    /** Controller identity (self and cluster) the current status was read under. */
    owner: string | null
    /** The status came from this controller's most recent successful read. */
    current: boolean
    actionError: string
    reset(): void
    refresh(): Promise<void>
    requestReview(selection: VllmGroupSelection): Promise<void>
    checkReview(): Promise<void>
    start(expectedReviewId?: string, elevation?: VllmGroupElevation[]): Promise<void>
    stop(expectedOperation?: VllmGroupOperation): Promise<void>
    reconcile(
        expectedOperation?: VllmGroupOperation,
        elevation?: VllmGroupElevation[]
    ): Promise<void>
    requestCleanup(expectedBinding?: VllmGroupCleanupRequest): Promise<void>
    discardReview(): void
    invalidate(reason?: string): void
}

let epoch = 0
/** The read currently in flight. Callers wait for it and then take their own. */
let inflight: Promise<void> | null = null
let pendingDigest = ''
let previousRunId = ''

function active(run: VllmGroupRunStatus | null | undefined): boolean {
    return !!run && !run.cleanupConfirmed
}

export function currentVllmGroupOwner(): string | null {
    const connection = useConnectionStore.getState()
    return connection.connected && connection.selfId && connection.clusterId
        ? JSON.stringify([connection.selfId, connection.clusterId])
        : null
}

function reviewedTopologyMatches(review: VllmGroupReview, selection: VllmGroupSelection): boolean {
    const nodes = selection.nodeIds.length
    const topology = review.plan.topology
    if (selection.model === VLLM_QWEN38_MODEL)
        return (
            selection.parallelism === undefined &&
            nodes === 2 &&
            topology.tensorParallel === 2 &&
            topology.pipelineParallel === 1 &&
            topology.dataParallel === 1 &&
            topology.expertParallel === 2
        )
    // A two-node default is tensor parallel only over a bound direct fabric lane.
    const pipeline =
        selection.parallelism === 'pipeline' ||
        (selection.parallelism === undefined &&
            (nodes === 3 || review.plan.directSocket === undefined))
    return (
        topology.tensorParallel === (pipeline ? 1 : nodes) &&
        topology.pipelineParallel === (pipeline ? nodes : 1) &&
        topology.dataParallel === 1
    )
}

const unknownState = {
    known: false as const,
    status: null,
    current: false as const
}

export const useVllmGroupStore = create<VllmGroupState>((set, get) => ({
    known: false,
    status: null,
    error: null,
    lastRun: null,
    review: null,
    check: null,
    pending: null,
    uncertainStart: false,
    owner: null,
    current: false,
    actionError: '',

    reset: () => {
        epoch++
        set({
            ...unknownState,
            error: 'Service disconnected.',
            lastRun: null,
            review: null,
            check: null,
            pending: null,
            owner: null,
            actionError: ''
        })
    },
    invalidate: reason => {
        epoch++
        set({
            ...unknownState,
            error:
                reason ??
                'Controller connection changed; serving-group ownership must be read again.',
            lastRun: null,
            review: null,
            check: null,
            pending: null,
            owner: null
        })
    },
    discardReview: () => {
        if (!get().pending) set({ review: null, check: null, actionError: '' })
    },

    refresh: async () => {
        // A follow-up read after an effect must start after the effect. Wait for
        // any in-flight read (its result may be discarded by the epoch guard)
        // and then take a fresh one instead of silently skipping.
        while (inflight) await inflight.catch(() => undefined)
        const owner = currentVllmGroupOwner()
        if (!owner) {
            get().invalidate()
            return
        }
        const revision = epoch
        inflight = (async () => {
            try {
                const status = await requireApi().getServingGroupStatus()
                if (revision !== epoch || owner !== currentVllmGroupOwner()) return
                const selfId = useConnectionStore.getState().selfId
                if (status.run && status.run.plan.coordinator !== selfId)
                    throw new Error('PAIR returned a group belonging to another controller.')
                const prior = get()
                const sameOwner = prior.owner === owner
                if (
                    sameOwner &&
                    active(prior.lastRun) &&
                    (!status.run ||
                        status.run.runId !== prior.lastRun?.runId ||
                        status.run.generation !== prior.lastRun?.generation)
                )
                    throw new Error(
                        'PAIR has not returned the retained group. Its ownership and cleanup remain unconfirmed.'
                    )
                // A consumed Start is settled by whatever this fresh read reports:
                // the new run it created, a retained state that never changed (Start
                // was not retained), or some other operation. Start is never resent.
                let actionError = prior.actionError
                if (prior.uncertainStart) {
                    const bound =
                        !!status.run &&
                        status.run.planDigest === pendingDigest &&
                        status.run.runId !== previousRunId
                    const settled = bound
                        ? ''
                        : !status.run || status.run.runId === previousRunId
                          ? 'PAIR did not retain the Start; no serving group was created.'
                          : 'PAIR retained a different operation than the sent Start.'
                    if (settled) actionError = actionError ? `${actionError} ${settled}` : settled
                }
                set({
                    known: true,
                    status,
                    error: null,
                    lastRun: status.run ?? (sameOwner ? prior.lastRun : null),
                    owner,
                    current: true,
                    uncertainStart: false,
                    actionError
                })
            } catch (error) {
                if (revision === epoch) set({ ...unknownState, error: getErrorString(error) })
            }
        })()
        try {
            await inflight
        } finally {
            inflight = null
        }
    },

    requestReview: async selection => {
        const state = get()
        const selfId = useConnectionStore.getState().selfId
        if (
            !state.known ||
            !state.current ||
            !state.owner ||
            state.owner !== currentVllmGroupOwner()
        ) {
            set({
                actionError: 'Read the current serving-group status before requesting a review.'
            })
            return
        }
        if (
            state.pending ||
            state.uncertainStart ||
            state.status?.reserved ||
            active(state.status?.run)
        ) {
            set({ actionError: 'A retained or pending serving-group operation must finish first.' })
            return
        }
        let typed: VllmGroupSelection
        try {
            typed = parseVllmGroupSelection(selection)
        } catch (error) {
            set({ actionError: getErrorString(error) })
            return
        }
        if (!selfId || typed.nodeIds[0] !== selfId) {
            set({ actionError: 'This controller must be the first selected node.' })
            return
        }
        const revision = ++epoch
        set({ pending: 'review', review: null, check: null, actionError: '' })
        try {
            const review = await requireApi().reviewServingGroup(typed)
            if (
                review.plan.model !== typed.model ||
                review.plan.coordinator !== selfId ||
                review.plan.members.map(member => member.nodeId).join('\n') !==
                    typed.nodeIds.join('\n') ||
                !reviewedTopologyMatches(review, typed)
            )
                throw new Error('PAIR returned a review for a different selection.')
            if (revision === epoch) set({ review })
        } catch (error) {
            if (revision === epoch) set({ actionError: getErrorString(error) })
        } finally {
            if (revision === epoch) set({ pending: null })
        }
    },

    checkReview: async () => {
        const state = get()
        const review = state.review
        if (!review || !state.current || state.owner !== currentVllmGroupOwner() || state.pending)
            return
        if (review.expiresAt <= Date.now()) {
            set({ actionError: 'The review expired. Request a new review.' })
            return
        }
        const revision = ++epoch
        set({ pending: 'check', check: null, actionError: '' })
        try {
            const check = await requireApi().checkServingGroup(review.reviewId)
            if (
                check.reviewId !== review.reviewId ||
                check.participants.map(participant => participant.nodeId).join('\n') !==
                    review.plan.members.map(member => member.nodeId).join('\n')
            )
                throw new Error('PAIR returned a check for a different review.')
            if (revision === epoch) set({ check })
        } catch (error) {
            if (revision === epoch) set({ check: null, actionError: getErrorString(error) })
        } finally {
            if (revision === epoch) set({ pending: null })
        }
    },

    start: async (expectedReviewId, elevation) => {
        const state = get()
        const review = state.review
        const selfId = useConnectionStore.getState().selfId
        const hold = startHold(state, selfId, Date.now(), expectedReviewId)
        if (hold) {
            set({ actionError: hold })
            return
        }
        if (!review) return
        const revision = ++epoch
        pendingDigest = review.planDigest
        previousRunId = state.status?.run?.runId ?? ''
        // Consume the review and mark ownership unknown before sending. The
        // outcome is settled only by the fresh read in `finally`; nothing can
        // issue a second Start for this review.
        set({
            ...unknownState,
            pending: 'start',
            review: null,
            check: null,
            uncertainStart: true,
            error: 'Start was sent; serving-group ownership is unknown until PAIR reports the retained operation.',
            actionError: ''
        })
        try {
            const run = await requireApi().startServingGroup(review.reviewId, elevation)
            if (
                run.planDigest !== review.planDigest ||
                run.runId === previousRunId ||
                JSON.stringify(run.plan) !== JSON.stringify(review.plan)
            )
                throw new Error(
                    'Start returned a different or previous operation; reconciling status.'
                )
            if (revision === epoch) set({ lastRun: run, pending: null })
        } catch (error) {
            if (revision === epoch)
                set({
                    actionError: `${getErrorString(error)} Start will not be resent; checking the retained owner.`
                })
        } finally {
            clearGroupElevation(elevation)
            if (revision === epoch && get().pending === 'start') set({ pending: null })
            await get().refresh()
        }
    },

    stop: async expectedOperation => {
        await operate(get, set, 'stop', expectedOperation, operation =>
            requireApi().stopServingGroup(operation)
        )
    },

    reconcile: async (expectedOperation, elevation) => {
        try {
            await operate(get, set, 'reconcile', expectedOperation, operation =>
                requireApi().reconcileServingGroup({
                    ...operation,
                    ...(elevation ? { elevation } : {})
                })
            )
        } finally {
            clearGroupElevation(elevation)
        }
    },

    requestCleanup: async expectedBinding => {
        const state = get()
        if (
            !state.known ||
            !state.status ||
            !state.owner ||
            state.owner !== currentVllmGroupOwner()
        ) {
            set({ actionError: 'Read the current serving-group status before requesting cleanup.' })
            return
        }
        if (state.pending) {
            set({ actionError: 'Another serving-group operation is in progress.' })
            return
        }
        const binding = vllmGroupCleanupBinding(state.status)
        if (!binding) {
            set({ actionError: 'No retained reserved serving group is held.' })
            return
        }
        if (
            expectedBinding &&
            (expectedBinding.runId !== binding.runId ||
                expectedBinding.generation !== binding.generation ||
                expectedBinding.planDigest !== binding.planDigest)
        ) {
            set({
                actionError:
                    'The retained serving group changed; review the refreshed status first.'
            })
            return
        }
        const revision = ++epoch
        // Cleanup is an effect: ownership is unknown until PAIR reports the outcome.
        set({
            ...unknownState,
            pending: 'cleanup',
            error: 'Cleanup was sent; serving-group ownership is unknown until PAIR reports the retained operation.',
            actionError: ''
        })
        try {
            const status = await requireApi().requestServingGroupCleanup(binding)
            if (
                !status.run ||
                status.run.runId !== binding.runId ||
                status.run.generation !== binding.generation
            )
                throw new Error('Cleanup returned a different operation; cleanup is unconfirmed.')
            // The returned status is authority: a still-reserved reply keeps the hold.
            if (revision === epoch)
                set({
                    known: true,
                    status,
                    error: null,
                    lastRun: status.run,
                    current: true,
                    pending: null
                })
        } catch (error) {
            if (revision === epoch) set({ actionError: getErrorString(error) })
        } finally {
            if (revision === epoch && get().pending === 'cleanup') set({ pending: null })
            await get().refresh()
        }
    }
}))

/** Every reason Start must not be sent, in the order a user can fix them. Null means admitted by PAIR. */
export function startHold(
    state: Pick<
        VllmGroupState,
        'review' | 'check' | 'known' | 'current' | 'owner' | 'pending' | 'uncertainStart' | 'status'
    >,
    selfId: string | null,
    now: number,
    expectedReviewId?: string
): string | null {
    const review = state.review
    if (!review) return 'Request and check a review first.'
    if (expectedReviewId !== undefined && expectedReviewId !== review.reviewId)
        return 'The displayed review is no longer current.'
    if (!state.known || !state.current || !state.owner || state.owner !== currentVllmGroupOwner())
        return 'Serving-group ownership must be read by this controller first.'
    if (!selfId || review.plan.coordinator !== selfId)
        return 'This controller is not the reviewed coordinator.'
    if (state.pending) return 'Another serving-group operation is in progress.'
    if (state.uncertainStart)
        return 'A previous Start is not yet settled; Start will not be resent.'
    if (state.status?.reserved || active(state.status?.run))
        return 'A retained serving group holds vLLM; stop or reconcile it first.'
    if (review.expiresAt <= now) return 'The review expired. Request a new review.'
    if (!review.activationEnabled)
        return review.reason || 'PAIR has not admitted activation for this review.'
    if (!state.check || state.check.reviewId !== review.reviewId)
        return 'Check participants before starting.'
    if (!state.check.activationEnabled) return 'PAIR reports a participant that is not admitted.'
    return null
}

async function operate(
    get: () => VllmGroupState,
    set: (partial: Partial<VllmGroupState>) => void,
    verb: 'stop' | 'reconcile',
    expectedOperation: VllmGroupOperation | undefined,
    send: (operation: VllmGroupOperation) => Promise<VllmGroupStatus>
): Promise<void> {
    const state = get()
    const run = state.status?.run ?? null
    const label = verb === 'stop' ? 'Stop' : 'Reconcile'
    if (!state.known || !run || !state.owner || state.owner !== currentVllmGroupOwner()) {
        set({ actionError: 'Read the current serving-group status before this operation.' })
        return
    }
    if (
        expectedOperation &&
        (expectedOperation.runId !== run.runId || expectedOperation.generation !== run.generation)
    ) {
        set({
            actionError: 'The retained serving group changed; review the refreshed status first.'
        })
        return
    }
    if (run.cleanupConfirmed) {
        set({ actionError: 'The retained serving group already confirms cleanup.' })
        return
    }
    if (state.pending) {
        set({ actionError: 'Another serving-group operation is in progress.' })
        return
    }
    const operation = { runId: run.runId, generation: run.generation }
    const revision = ++epoch
    // The effect is sent; ownership is unknown until PAIR reports the outcome.
    set({
        ...unknownState,
        pending: verb,
        error: `${label} was sent; serving-group ownership is unknown until PAIR reports the retained operation.`,
        actionError: ''
    })
    try {
        const status = await send(operation)
        if (
            !status.run ||
            status.run.runId !== operation.runId ||
            status.run.generation !== operation.generation
        )
            throw new Error(`${label} returned a different operation; cleanup is unconfirmed.`)
        if (revision === epoch)
            set({
                known: true,
                status,
                error: null,
                lastRun: status.run,
                current: true,
                pending: null
            })
    } catch (error) {
        if (revision === epoch) set({ actionError: getErrorString(error) })
    } finally {
        if (revision === epoch && get().pending === verb) set({ pending: null })
        await get().refresh()
    }
}

/**
 * Strictest hold, used for this controller and for any caller that names no
 * target: unknown ownership, a reserved or uncleared run, or an unsettled Start.
 * A zustand selector-safe pure function so every surface shares one rule.
 */
export function vllmGroupSelfHold(
    state: Pick<VllmGroupState, 'known' | 'status' | 'uncertainStart'>
): boolean {
    return (
        state.uncertainStart ||
        !state.known ||
        state.status?.reserved !== false ||
        Boolean(state.status?.run && !state.status.run.cleanupConfirmed)
    )
}

/**
 * Target-aware hold for one node, selector-safe. Self keeps the strict rule; a
 * remote target is held only when the retained run names it or when unknown
 * ownership sits on a host that could own a journal. An unsettled Start holds
 * every target.
 */
export function vllmGroupHoldsNode(
    state: Pick<VllmGroupState, 'known' | 'status' | 'uncertainStart'>,
    nodeId: string,
    selfId: string | null,
    localVllmSupported: boolean
): boolean {
    return (
        state.uncertainStart ||
        vllmGroupHoldsTarget(
            { known: state.known, status: state.status, localVllmSupported },
            nodeId,
            selfId
        )
    )
}

/**
 * Target-aware hold for ordinary vLLM mutations. Without a node the strictest
 * rule applies.
 */
export function vllmMutationsBlocked(
    nodeId?: string,
    selfId: string | null = null,
    localVllmSupported = true
): boolean {
    const state = useVllmGroupStore.getState()
    if (!nodeId) return vllmGroupSelfHold(state)
    return vllmGroupHoldsNode(state, nodeId, selfId, localVllmSupported)
}

export function vllmMutationBlockReason(): string {
    const state = useVllmGroupStore.getState()
    if (state.uncertainStart)
        return 'A serving-group Start is not yet settled by a fresh status read.'
    return state.error || state.status?.reason || 'Serving-group ownership is not yet known.'
}

// One cheap status loop while a viewer is mounted or an owned operation is
// unfinished. Hiding the card must not erase an unsettled Start or a hold.
let viewers = 0
let watching = false
async function poll(): Promise<void> {
    const state = useVllmGroupStore.getState()
    if (!viewers && !state.uncertainStart && !active(state.lastRun)) {
        watching = false
        return
    }
    await state.refresh()
    setTimeout(() => void poll(), 2000)
}
export function watchVllmGroup(): () => void {
    viewers++
    if (!watching) {
        watching = true
        void poll()
    }
    return () => {
        viewers = Math.max(0, viewers - 1)
    }
}
