// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { beforeEach, describe, expect, it } from 'vitest'
import { useConnectionStore } from '@/ui/stores/connection.store'
import { currentVllmGroupOwner } from '@/ui/stores/vllm-group.store'
import { vllmGroupControls, type VllmGroupControlState } from '@/ui/utils/vllm-group-controls'
import {
    groupCheck,
    groupReview,
    groupRun,
    heldStatus,
    inactiveStatus
} from '../fixtures/vllm-group'

function inputs(
    state: Partial<VllmGroupControlState>,
    extra: Partial<Parameters<typeof vllmGroupControls>[0]> = {}
) {
    const owner = currentVllmGroupOwner()
    return vllmGroupControls({
        state: {
            known: true,
            status: inactiveStatus(),
            error: null,
            review: null,
            check: null,
            pending: null,
            uncertainStart: false,
            owner,
            current: true,
            ...state
        },
        selfId: 'node-a',
        connectionOwner: owner,
        now: Date.now(),
        selectedNodeIds: ['node-a', 'node-b'],
        model: groupReview().plan.model,
        knownNodeIds: new Set(['node-a', 'node-b', 'node-c']),
        ...extra
    })
}

describe('serving-group control truth', () => {
    beforeEach(() => {
        useConnectionStore.setState({ connected: true, selfId: 'node-a', clusterId: 'cluster-a' })
    })

    it('holds every control while ownership is unknown and names the reason', () => {
        const controls = inputs({
            known: false,
            status: null,
            error: 'journal unreadable',
            current: false
        })
        for (const control of [
            controls.review,
            controls.check,
            controls.start,
            controls.stop,
            controls.reconcile,
            controls.cleanup
        ])
            expect(control).toEqual({ enabled: false, hold: 'journal unreadable' })
    })

    it('enables only Review on a clear, fresh read and explains what Start still needs', () => {
        const controls = inputs({})
        expect(controls.review.enabled).toBe(true)
        expect(controls.check.hold).toMatch(/Request a review/)
        expect(controls.start.hold).toMatch(/Request and check a review/)
        expect(controls.stop.hold).toMatch(/No retained serving group/)
        expect(controls.cleanup.hold).toMatch(/No retained reserved/)
        expect(inputs({}, { selectedNodeIds: ['node-b', 'node-a'] }).review.hold).toMatch(
            /first selected/
        )
        expect(inputs({}, { selectedNodeIds: ['node-a', 'node-x'] }).review.hold).toMatch(
            /current cluster node/
        )
        expect(inputs({}, { model: '' }).review.hold).toMatch(/exact downloaded model/)
    })

    it('admits Start only with an unexpired admitted review and an admitted check for that review', () => {
        const review = groupReview()
        expect(inputs({ review }).start.hold).toMatch(/Check participants/)
        expect(inputs({ review, check: groupCheck(review, false) }).start.hold).toMatch(
            /not admitted/
        )
        expect(
            inputs({ review, check: groupCheck({ ...review, reviewId: 'b'.repeat(32) }) }).start
                .hold
        ).toMatch(/Check participants/)
        expect(
            inputs({
                review: { ...review, activationEnabled: false, reason: 'held by owner' },
                check: groupCheck(review)
            }).start.hold
        ).toBe('held by owner')
        expect(inputs({ review, check: groupCheck(review) }).start).toEqual({
            enabled: true,
            hold: null
        })
        expect(inputs({ review, check: groupCheck(review), pending: 'check' }).start.hold).toMatch(
            /in progress/
        )
        expect(
            inputs({ review: { ...review, expiresAt: Date.now() - 1 }, check: groupCheck(review) })
                .reviewExpired
        ).toBe(true)
    })

    it('scopes Stop, Reconcile and Cleanup to a retained run, fences any pending operation, and holds Review while held', () => {
        const held = inputs({ status: heldStatus() })
        expect(held.held).toBe(true)
        expect(held.review.hold).toMatch(/holds vLLM/)
        expect(held.stop.enabled).toBe(true)
        expect(held.reconcile.enabled).toBe(true)
        expect(held.cleanup.enabled).toBe(true)
        const busy = inputs({ status: heldStatus(), pending: 'cleanup' })
        expect(busy.stop.hold).toMatch(/in progress/)
        expect(busy.reconcile.hold).toMatch(/in progress/)
        expect(busy.cleanup.hold).toMatch(/in progress/)
        const ready = inputs({
            status: { activationEnabled: true, reserved: true, reason: '', run: groupRun('ready') }
        })
        expect(ready.stop.enabled).toBe(true)
        expect(ready.reconcile.hold).toMatch(/requires cleanup/)
        expect(ready.cleanup.enabled).toBe(true)
        const clean = inputs({ status: { ...inactiveStatus(), run: groupRun('stopped') } })
        expect(clean.stop.hold).toMatch(/already confirms/)
        expect(clean.cleanup.hold).toMatch(/No retained reserved/)
        expect(clean.review.enabled).toBe(true)
        expect(inputs({ uncertainStart: true }).review.hold).toMatch(/holds vLLM/)
    })
})
