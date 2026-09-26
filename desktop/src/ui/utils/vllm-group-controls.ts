// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Pure control-truth for the managed serving-group card. Every predicate is
 * fail-closed: a control is enabled only when PAIR's latest status admits the
 * exact operation, and every disabled control carries the reason it is held.
 */
import type { VllmGroupStatus } from '@/shared/types/vllm-group-status'
import type { VllmGroupCheck, VllmGroupReview } from '@/shared/types/vllm-group'
import type { VllmGroupPending } from '@/ui/stores/vllm-group.store'
import { startHold } from '@/ui/stores/vllm-group.store'

export interface VllmGroupControlState {
    known: boolean
    status: VllmGroupStatus | null
    error: string | null
    review: VllmGroupReview | null
    check: VllmGroupCheck | null
    pending: VllmGroupPending
    uncertainStart: boolean
    owner: string | null
    current: boolean
}

interface VllmGroupControlInputs {
    state: VllmGroupControlState
    selfId: string | null
    /** Owner identity computed from the live connection; null while disconnected. */
    connectionOwner: string | null
    now: number
    selectedNodeIds: string[]
    model: string
    knownNodeIds: ReadonlySet<string>
}

interface VllmGroupControl {
    enabled: boolean
    /** Why the control is held; null when enabled. */
    hold: string | null
}

interface VllmGroupControls {
    fresh: boolean
    held: boolean
    review: VllmGroupControl
    check: VllmGroupControl
    start: VllmGroupControl
    stop: VllmGroupControl
    reconcile: VllmGroupControl
    cleanup: VllmGroupControl
    reviewExpired: boolean
}

function control(hold: string | null): VllmGroupControl {
    return { enabled: hold === null, hold }
}

export function vllmGroupControls(inputs: VllmGroupControlInputs): VllmGroupControls {
    const { state, selfId, connectionOwner, now } = inputs
    const run = state.status?.run ?? null
    const uncleared = !!run && !run.cleanupConfirmed
    const held = state.uncertainStart || state.status?.reserved === true || uncleared
    const sameOwner = !!connectionOwner && !!state.owner && state.owner === connectionOwner
    const fresh = state.known && state.current && sameOwner
    const reviewExpired = !!state.review && state.review.expiresAt <= now

    const ownership = !state.known
        ? state.error || 'Serving-group ownership is not yet known.'
        : !fresh
          ? 'Read the current serving-group status from this controller first.'
          : null
    const busy = state.pending ? `${state.pending} is in progress.` : null

    const reviewHold =
        ownership ??
        busy ??
        (held
            ? 'A retained or pending serving-group operation holds vLLM.'
            : !selfId || inputs.selectedNodeIds[0] !== selfId
              ? 'This controller must be the first selected node.'
              : inputs.selectedNodeIds.length < 2 || inputs.selectedNodeIds.length > 3
                ? 'Select this controller and one or two current nodes.'
                : inputs.selectedNodeIds.some(id => !inputs.knownNodeIds.has(id))
                  ? 'Every selected node must be a current cluster node.'
                  : !inputs.model
                    ? 'Select one exact downloaded model.'
                    : null)

    const checkHold =
        ownership ??
        busy ??
        (!state.review
            ? 'Request a review first.'
            : reviewExpired
              ? 'The review expired. Request a new review.'
              : null)

    // Unknown ownership outranks every other Start reason: nothing about a
    // review matters until this controller has read the current status.
    const startHoldReason = ownership ?? startHold(state, selfId, now)

    const operationHold =
        ownership ??
        busy ??
        (!run
            ? 'No retained serving group is reported.'
            : run.cleanupConfirmed
              ? 'The retained serving group already confirms cleanup.'
              : null)

    const reconcileHold =
        operationHold ??
        (run?.state !== 'cleanup-required' && run?.state !== 'failed'
            ? 'Reconcile applies only to a retained group that requires cleanup.'
            : null)

    const cleanupHold =
        ownership ??
        busy ??
        (!state.status?.reserved || !run ? 'No retained reserved serving group is held.' : null)

    return {
        fresh,
        held,
        reviewExpired,
        review: control(reviewHold),
        check: control(checkHold),
        start: control(startHoldReason),
        stop: control(operationHold),
        reconcile: control(reconcileHold),
        cleanup: control(cleanupHold)
    }
}
