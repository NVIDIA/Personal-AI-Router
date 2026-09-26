// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import {
    parseVllmGroupStatus,
    parseVllmServingGroupRoute
} from '../../src/shared/utils/vllm-group-status'
import { heldStatus, inactiveStatus, wire } from '../fixtures/vllm-group'

describe('serving-group status parser', () => {
    it('accepts exact held ownership without granting activation', () => {
        const value = parseVllmGroupStatus(wire(heldStatus()))
        expect(value.activationEnabled).toBe(false)
        expect(value.run?.generation).toBe(13)
        expect(value.run?.plan.members).toHaveLength(3)
        expect(value.run?.failure).toBe('all owned ranks require cleanup')
    })

    it('rejects participant drift without treating activation metadata as authority', () => {
        const drifted = heldStatus()
        drifted.run!.ranks[2].nodeId = 'node-x'
        expect(() => parseVllmGroupStatus(wire(drifted))).toThrow(/participants/)

        const enabled = heldStatus()
        enabled.activationEnabled = true
        expect(parseVllmGroupStatus(wire(enabled)).activationEnabled).toBe(true)
    })

    it('accepts an inactive missing journal status', () => {
        expect(parseVllmGroupStatus(wire(inactiveStatus()))).toEqual(inactiveStatus())
    })

    it('accepts a cleanup-confirmed completed run after reservation release', () => {
        const completed = heldStatus()
        completed.reserved = false
        completed.run!.state = 'stopped'
        completed.run!.cleanupConfirmed = true
        completed.run!.ranks.forEach(rank => {
            rank.cleanupConfirmed = true
        })
        expect(parseVllmGroupStatus(wire(completed)).run?.cleanupConfirmed).toBe(true)
    })

    it('strictly parses the compact engine route projection', () => {
        const route = {
            runId: 'a'.repeat(32),
            generation: 23,
            model: 'example/model@revision',
            coordinator: 'node-a',
            role: 'coordinator',
            state: 'ready',
            members: ['node-a', 'node-b']
        }
        expect(parseVllmServingGroupRoute(route)).toEqual(route)
        expect(() => parseVllmServingGroupRoute({ ...route, role: 'leader' })).toThrow(/role/)
        expect(() =>
            parseVllmServingGroupRoute({ ...route, members: ['node-a', 'node-a'] })
        ).toThrow(/participants/)
        expect(() => parseVllmServingGroupRoute({ ...route, command: 'start' })).toThrow(/route/)
    })
})
