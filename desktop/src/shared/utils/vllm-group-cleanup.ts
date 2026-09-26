// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type {
    VllmGroupCleanupRequest,
    VllmGroupStatusView
} from '@/shared/types/vllm-group-cleanup'
import type { VllmGroupStatus } from '@/shared/types/vllm-group-status'

const runIdPattern = /^[0-9a-f]{32}$/
const digestPattern = /^[0-9a-f]{64}$/

/** Strictly validates a cleanup request shape. Arbitrary strings are rejected. */
export function parseVllmGroupCleanupRequest(value: unknown): VllmGroupCleanupRequest {
    if (!value || typeof value !== 'object' || Array.isArray(value))
        throw new Error('Serving-group cleanup request must be an object.')
    const { runId, generation, planDigest } = value as Record<string, unknown>
    if (typeof runId !== 'string' || !runIdPattern.test(runId))
        throw new Error('Serving-group cleanup request has an invalid run identity.')
    if (typeof generation !== 'number' || !Number.isSafeInteger(generation) || generation <= 0)
        throw new Error('Serving-group cleanup request has an invalid generation.')
    if (typeof planDigest !== 'string' || !digestPattern.test(planDigest))
        throw new Error('Serving-group cleanup request has an invalid plan digest.')
    return { runId, generation, planDigest }
}

/**
 * Derives the exact binding from a parsed status. Only a reserved retained run
 * can be bound; anything else yields null so no request is ever constructed.
 */
export function vllmGroupCleanupBinding(status: VllmGroupStatus): VllmGroupCleanupRequest | null {
    if (!status.reserved || !status.run) return null
    return parseVllmGroupCleanupRequest({
        runId: status.run.runId,
        generation: status.run.generation,
        planDigest: status.run.planDigest
    })
}

/**
 * Binds a displayed cleanup request to a freshly read status. The fresh
 * binding, never the displayed one, is what may be sent. Throws when nothing
 * is held or when the retained operation changed since it was displayed, so a
 * stale or cached identity never reaches PAIR.
 */
export function bindVllmGroupCleanup(
    fresh: VllmGroupStatus,
    requested: VllmGroupCleanupRequest
): VllmGroupCleanupRequest {
    const binding = vllmGroupCleanupBinding(fresh)
    if (!binding)
        throw new Error(
            'No retained reserved serving group is held, so there is nothing to clean up.'
        )
    if (
        binding.runId !== requested.runId ||
        binding.generation !== requested.generation ||
        binding.planDigest !== requested.planDigest
    )
        throw new Error(
            'The retained serving group changed after it was displayed. Review the refreshed status before requesting cleanup again.'
        )
    return binding
}

/**
 * PAIR answers a cleanup with the status of the exact operation it acted on.
 * Any other reply leaves cleanup unconfirmed and must not be published as
 * authority for that operation.
 */
export function bindVllmGroupCleanupStatus(
    status: VllmGroupStatus,
    binding: VllmGroupCleanupRequest
): VllmGroupStatus {
    if (
        !status.run ||
        status.run.runId !== binding.runId ||
        status.run.generation !== binding.generation
    )
        throw new Error('Cleanup returned a different operation; cleanup is unconfirmed.')
    return status
}

/**
 * Target-aware hold. Self keeps the strict rule: unknown, error, or reserved
 * state holds every vLLM mutation. A remote target is held only when the
 * retained run names it, or when the status is unknown on a host that could
 * genuinely own a journal. An unsupported local platform cannot own one, so
 * its unknown status does not hold remote Linux targets.
 */
export function vllmGroupHoldsTarget(
    view: VllmGroupStatusView,
    nodeId: string,
    selfId: string | null
): boolean {
    const self = selfId === null || nodeId === selfId
    if (!view.known || !view.status) return self || view.localVllmSupported
    const { status } = view
    const uncleared = Boolean(status.run && !status.run.cleanupConfirmed)
    if (self) return status.reserved || uncleared
    if (!status.reserved && !uncleared) return false
    return status.run?.plan.members.some(member => member.nodeId === nodeId) ?? true
}
