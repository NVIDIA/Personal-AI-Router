// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import {
    parseVllmGroupPlan,
    parseVllmGroupRun,
    parseVllmGroupStatus,
    parseVllmServingGroupRoute
} from '@/shared/utils/vllm-group-status'
import {
    parseVllmGroupCheck,
    parseVllmGroupOperation,
    parseVllmGroupReconcileRequest,
    parseVllmGroupReview,
    parseVllmGroupSelection,
    parseVllmGroupStartRequest
} from '@/shared/utils/vllm-group'
import {
    VLLM_GROUP_START_FAILURE_CODES,
    VLLM_GROUP_START_FAILURE_STAGES,
    VLLM_GROUP_START_STDERR_CODES
} from '@/shared/types/vllm-group'
import {
    groupCheck,
    groupReview,
    groupRun,
    heldRun,
    heldStatus,
    inactiveStatus,
    qwenGroupReview,
    wire
} from '../fixtures/vllm-group'

type Mutable = Record<string, any> // eslint-disable-line @typescript-eslint/no-explicit-any
const mutate = (value: unknown, update: (row: Mutable) => void): unknown => {
    const copy = wire(value) as Mutable
    update(copy)
    return copy
}
/** Adds a distinct third Spark under the given topology. */
const addThirdSpark = (plan: Mutable, topology: Mutable): void => {
    Object.assign(plan.topology, topology)
    plan.members.push({
        ...plan.members[1],
        nodeId: 'node-c',
        pinSha256: '3'.repeat(64),
        gpuUuid: 'GPU-00000003-0000-0000-0000-000000000001'
    })
}
const threeSparkQwenLayouts = [
    { tensorParallel: 1, pipelineParallel: 3, dataParallel: 1, expertParallel: 1 },
    {
        tensorParallel: 1,
        pipelineParallel: 1,
        dataParallel: 3,
        expertParallel: 3,
        eplb: true,
        redundantExperts: 1
    },
    { tensorParallel: 2, pipelineParallel: 1, dataParallel: 1, expertParallel: 2 }
]

describe('serving-group review parser', () => {
    it('keeps the requested tensor or pipeline mode and refuses a topology that ignores it', () => {
        expect(
            parseVllmGroupReview(wire(groupReview(3, 'tensor'))).plan.topology.tensorParallel
        ).toBe(3)
        expect(
            parseVllmGroupReview(wire(groupReview(3, 'pipeline'))).plan.topology.pipelineParallel
        ).toBe(3)
        expect(parseVllmGroupReview(wire(groupReview(2))).plan.topology.tensorParallel).toBe(2)
        expect(() =>
            parseVllmGroupReview(
                mutate(groupReview(2), row => (row.plan.topology.tensorParallel = 3))
            )
        ).toThrow(/unsupported reviewed topology/)
    })

    it('requires owner fields on a review', () => {
        for (const remove of [
            (row: Mutable) => delete row.plan.limits,
            (row: Mutable) => delete row.plan.members[0].placement,
            (row: Mutable) => delete row.plan.members[1].resources
        ])
            expect(() => parseVllmGroupReview(mutate(groupReview(), remove))).toThrow()
    })

    it('accepts only the exact two-Spark Qwen profile', () => {
        const plan = parseVllmGroupReview(wire(qwenGroupReview())).plan
        expect(plan.topology).toMatchObject({
            tensorParallel: 2,
            pipelineParallel: 1,
            dataParallel: 1,
            expertParallel: 2
        })
        expect(plan.topology.eplb).toBeUndefined()
        expect(plan.transport).toMatchObject({
            subnetAwareRouting: false,
            subnetPrefixLength: 0,
            mergeNICs: true
        })
        for (const spoil of [
            (row: Mutable) => (row.plan.topology.mtp = true),
            (row: Mutable) => delete row.plan.transport,
            (row: Mutable) => (row.plan.transport.socketPayloadFallback = true),
            (row: Mutable) => delete row.plan.transport.subnetAwareRouting,
            (row: Mutable) => (row.plan.transport.subnetAwareRouting = true),
            (row: Mutable) => (row.plan.transport.subnetPrefixLength = 31),
            (row: Mutable) => (row.plan.transport.mergeNICs = false),
            (row: Mutable) => (row.plan.topology.maxSequences = 3),
            (row: Mutable) => (row.plan.limits.memoryMaxBytes = 16 * 1024 ** 3),
            (row: Mutable) => delete row.plan.members[0].fabric
        ])
            expect(() => parseVllmGroupReview(mutate(qwenGroupReview(), spoil))).toThrow()
    })

    it('refuses every three-Spark Qwen layout', () => {
        for (const layout of threeSparkQwenLayouts)
            expect(() =>
                parseVllmGroupReview(
                    mutate(qwenGroupReview(), row => addThirdSpark(row.plan, layout))
                )
            ).toThrow(/unsupported reviewed topology/)
    })

    it('accepts a qualified direct socket only on an ordinary two-node TP2 plan', () => {
        const lane = {
            nodeId: 'node-a',
            interfaceName: 'enp1s0f0np0',
            interfaceIndex: 3,
            mac: '02:00:00:0a:00:00',
            localAddress: '172.31.240.1',
            peerAddress: '172.31.240.2'
        }
        const directSocket = {
            mode: 'qualified-direct-socket',
            operationId: 'a'.repeat(32),
            qualificationSha256: 'b'.repeat(64),
            lanes: [
                lane,
                {
                    ...lane,
                    nodeId: 'node-b',
                    interfaceName: 'enp1s0f1np1',
                    localAddress: '172.31.240.2',
                    peerAddress: '172.31.240.1'
                }
            ]
        }
        const attach = (socket: unknown) => (row: Mutable) => (row.plan.directSocket = socket)
        const attachCurrent = (socket: unknown) => (row: Mutable) => {
            row.plan.runtime = '0.29.0'
            attach(socket)(row)
        }
        const plan = parseVllmGroupReview(mutate(groupReview(2), attachCurrent(directSocket))).plan
        expect(plan.topology.tensorParallel).toBe(2)
        expect(plan.directSocket?.lanes.map(entry => entry.interfaceName)).toEqual([
            'enp1s0f0np0',
            'enp1s0f1np1'
        ])
        const refused: Array<[unknown, (row: Mutable) => void]> = [
            [groupReview(2, 'pipeline'), attachCurrent(directSocket)],
            [groupReview(3, 'tensor'), attachCurrent(directSocket)],
            [qwenGroupReview(), attach(directSocket)],
            [groupReview(2), attach(directSocket)],
            [groupReview(2), attachCurrent({ ...directSocket, mode: 'host-buffer-roce' })],
            [
                groupReview(2),
                attachCurrent({ ...directSocket, lanes: [...directSocket.lanes].reverse() })
            ],
            [
                groupReview(2),
                attachCurrent({
                    ...directSocket,
                    lanes: [lane, { ...directSocket.lanes[1], peerAddress: '172.31.240.9' }]
                })
            ],
            [groupReview(2), attachCurrent({ ...directSocket, lanes: [lane] })]
        ]
        for (const [review, spoil] of refused)
            expect(() => parseVllmGroupReview(mutate(review, spoil))).toThrow()
    })

    it('refuses participants that are not distinct or not coordinator-first', () => {
        for (const spoil of [
            (row: Mutable) => (row.plan.members[1].pinSha256 = row.plan.members[0].pinSha256),
            (row: Mutable) => (row.plan.members[1].nodeId = 'node-a'),
            (row: Mutable) => (row.plan.coordinator = 'node-b'),
            (row: Mutable) => (row.plan.members[1].modelDigest = '0'.repeat(64)),
            (row: Mutable) => row.plan.members.push(row.plan.members[0])
        ])
            expect(() => parseVllmGroupReview(mutate(groupReview(2), spoil))).toThrow(
                /participants|member list/
            )
    })
})

describe('serving-group route parser', () => {
    const route = {
        runId: 'c'.repeat(32),
        generation: 4,
        model: 'Qwen/Qwen2.5-0.5B-Instruct',
        coordinator: 'node-a',
        role: 'coordinator',
        state: 'starting',
        members: ['node-a', 'node-b']
    }

    it('reads route readiness only on a starting coordinator', () => {
        expect(parseVllmServingGroupRoute(route)).not.toHaveProperty('routing')
        expect(parseVllmServingGroupRoute({ ...route, routing: true }).routing).toBe(true)
        for (const spoiled of [
            { ...route, routing: false },
            { ...route, routing: true, role: 'participant' },
            { ...route, routing: true, state: 'ready' },
            { ...route, routing: true, extra: 1 }
        ])
            expect(() => parseVllmServingGroupRoute(spoiled)).toThrow()
    })
})

describe('serving-group run and status parser', () => {
    it('reads a cleanup-confirmed pre-routing Qwen journal without granting it route authority', () => {
        const historical = {
            ...inactiveStatus(),
            run: groupRun('failed', qwenGroupReview())
        }
        historical.run.generation = 55
        const wireStatus = mutate(historical, row => {
            delete row.run.plan.transport.subnetAwareRouting
            delete row.run.plan.transport.subnetPrefixLength
            delete row.run.plan.transport.mergeNICs
        })
        const parsed = parseVllmGroupStatus(wireStatus)
        expect(parsed.run?.cleanupConfirmed).toBe(true)
        expect(parsed.run?.plan.transport).not.toHaveProperty('subnetAwareRouting')
        for (const layout of threeSparkQwenLayouts)
            expect(() =>
                parseVllmGroupStatus(
                    mutate(historical, row => {
                        addThirdSpark(row.run.plan, layout)
                        row.run.ranks.push({ ...row.run.ranks[1], nodeId: 'node-c' })
                    })
                )
            ).toThrow(/unsupported reviewed topology/)
        expect(() =>
            parseVllmGroupRun((wireStatus as Mutable).run, { ownerFields: 'required' })
        ).toThrow(/routing policy/)
        expect(() =>
            parseVllmGroupReview(
                mutate(qwenGroupReview(), row => {
                    delete row.plan.transport.subnetAwareRouting
                    delete row.plan.transport.subnetPrefixLength
                    delete row.plan.transport.mergeNICs
                })
            )
        ).toThrow(/routing policy/)
        expect(() =>
            parseVllmGroupStatus(
                mutate(wireStatus, row => {
                    row.reserved = true
                    row.run.state = 'starting'
                    row.run.cleanupConfirmed = false
                    row.run.ranks.forEach((rank: Mutable) => (rank.cleanupConfirmed = false))
                })
            )
        ).toThrow(/routing policy/)
    })

    it('reads the retained generation-13 projection without owner fields and refuses it as an owner run', () => {
        const run = parseVllmGroupRun(wire(heldRun()))
        expect(run.generation).toBe(13)
        expect(run.plan.limits).toBeUndefined()
        expect(run.plan.members[0].placement).toBeUndefined()
        expect(() => parseVllmGroupRun(wire(heldRun()), { ownerFields: 'required' })).toThrow()
        expect(
            parseVllmGroupRun(wire(groupRun()), { ownerFields: 'required' }).plan.limits
        ).toEqual(groupReview().plan.limits)
    })

    it('refuses rank drift, impossible rank evidence and unknown states', () => {
        expect(() =>
            parseVllmGroupRun(mutate(heldRun(), row => (row.ranks[2].nodeId = 'node-x')))
        ).toThrow(/participants/)
        expect(() => parseVllmGroupRun(mutate(heldRun(), row => row.ranks.pop()))).toThrow()
        expect(() =>
            parseVllmGroupRun(
                mutate(
                    heldRun(),
                    row => (row.ranks[0] = { ...row.ranks[0], attempted: false, started: true })
                )
            )
        ).toThrow(/participants/)
        expect(() => parseVllmGroupRun(mutate(heldRun(), row => (row.state = 'running')))).toThrow(
            /unknown/
        )
        expect(() =>
            parseVllmGroupRun(mutate(groupRun('ready'), row => (row.ranks[0].started = false)))
        ).toThrow(/cleanup evidence/)
        expect(() =>
            parseVllmGroupRun(mutate(groupRun('ready'), row => (row.cleanupConfirmed = true)))
        ).toThrow(/cleanup evidence/)
        expect(() =>
            parseVllmGroupRun(
                mutate(groupRun('stopped'), row => (row.ranks[1].cleanupConfirmed = false))
            )
        ).toThrow(/cleanup evidence/)
    })

    it('accepts a completed run whose never-attempted rank needs no cleanup evidence', () => {
        const run = parseVllmGroupRun(
            mutate(
                groupRun('stopped'),
                row =>
                    (row.ranks[1] = {
                        nodeId: 'node-b',
                        attempted: false,
                        started: false,
                        cleanupConfirmed: false
                    })
            )
        )
        expect(run.cleanupConfirmed).toBe(true)
    })

    it('round-trips bounded rank start diagnostics and refuses unbounded or unknown ones', () => {
        const failure = {
            stage: VLLM_GROUP_START_FAILURE_STAGES[1],
            code: VLLM_GROUP_START_FAILURE_CODES[0],
            exit: 1,
            stdoutBytes: 0,
            stderrCode: VLLM_GROUP_START_STDERR_CODES[0]
        }
        const withFailure = mutate(groupRun('failed'), row => (row.ranks[0].startFailure = failure))
        expect(parseVllmGroupRun(withFailure).ranks[0].startFailure).toEqual(failure)
        const readback = mutate(groupRun('failed'), row => {
            row.ranks[0].startFailure = {
                ...failure,
                code: 'system_manager_readback_incomplete',
                missingProperties: ['ActiveState', 'Id']
            }
        })
        expect(parseVllmGroupRun(readback).ranks[0].startFailure?.missingProperties).toEqual([
            'ActiveState',
            'Id'
        ])
        for (const spoil of [
            (row: Mutable) => (row.ranks[0].startFailure = { ...failure, code: 'made_up' }),
            (row: Mutable) => (row.ranks[0].startFailure = { ...failure, stdout: 'raw text' }),
            (row: Mutable) =>
                (row.ranks[0].startFailure = { ...failure, stdoutBytes: 2 * 1048576 }),
            (row: Mutable) => (row.ranks[0].startFailure = { ...failure, exit: 300 }),
            (row: Mutable) =>
                (row.ranks[0].startFailure = { ...failure, missingProperties: ['Id'] }),
            (row: Mutable) =>
                (row.ranks[0].startFailure = {
                    ...failure,
                    code: 'system_manager_readback_incomplete',
                    missingProperties: ['Id', 'ActiveState']
                })
        ])
            expect(() => parseVllmGroupRun(mutate(groupRun('failed'), spoil))).toThrow()
    })

    it('round-trips bounded cleanup diagnostics only for unresolved attempted ranks', () => {
        const failure = {
            stage: 'helper',
            code: 'process_failed',
            exit: 1,
            stdoutBytes: 0,
            stderrCode: 'sudo_authentication_required'
        }
        const held = mutate(heldRun(), row => {
            row.ranks[0].cleanupConfirmed = true
            row.ranks[1].cleanupFailure = failure
            row.ranks[2].cleanupFailure = failure
        })
        const parsed = parseVllmGroupRun(held)
        expect(parsed.ranks[0].cleanupFailure).toBeUndefined()
        expect(parsed.ranks[1].cleanupFailure).toEqual(failure)
        expect(parsed.ranks[2].cleanupFailure?.stderrCode).toBe('sudo_authentication_required')
        for (const spoil of [
            (row: Mutable) => (row.ranks[0].cleanupFailure = failure),
            (row: Mutable) => {
                row.ranks[1].cleanupConfirmed = true
                row.ranks[1].cleanupFailure = failure
            },
            (row: Mutable) =>
                (row.ranks[1].cleanupFailure = { ...failure, stderr: 'raw private text' })
        ])
            expect(() => parseVllmGroupRun(mutate(held, spoil))).toThrow()
    })

    it('treats status as hold truth: reserved needs its run and an unreserved run must be clean', () => {
        expect(parseVllmGroupStatus(wire(heldStatus())).reserved).toBe(true)
        expect(parseVllmGroupStatus(wire(inactiveStatus()))).toEqual(inactiveStatus())
        expect(() => parseVllmGroupStatus(mutate(heldStatus(), row => delete row.run))).toThrow(
            /without its run/
        )
        expect(() =>
            parseVllmGroupStatus(mutate(heldStatus(), row => (row.reserved = false)))
        ).toThrow(/uncleared unreserved/)
        expect(() => parseVllmGroupStatus(mutate(heldStatus(), row => delete row.reason))).toThrow(
            /reason/
        )
        const released = { ...inactiveStatus(), run: groupRun('stopped') }
        expect(parseVllmGroupStatus(wire(released)).run?.cleanupConfirmed).toBe(true)
        const enabled = mutate(heldStatus(), row => (row.activationEnabled = true))
        expect(parseVllmGroupStatus(enabled).activationEnabled).toBe(true)
    })

    it('treats an all-zero limits block as absent on a status read but never on an owner review', () => {
        const zero = { runtimeSeconds: 0, memoryMaxBytes: 0, tasksMax: 0 }
        const projected = parseVllmGroupRun(mutate(heldRun(), row => (row.plan.limits = zero)))
        expect(projected.plan).not.toHaveProperty('limits')
        expect(() =>
            parseVllmGroupRun(
                mutate(heldRun(), row => (row.plan.limits = zero)),
                {
                    ownerFields: 'required'
                }
            )
        ).toThrow()
        expect(() =>
            parseVllmGroupReview(mutate(groupReview(), row => (row.plan.limits = zero)))
        ).toThrow(/unsupported reviewed topology/)
        expect(() =>
            parseVllmGroupRun(
                mutate(heldRun(), row => (row.plan.limits = { ...zero, tasksMax: 512 }))
            )
        ).toThrow(/unsupported reviewed topology/)
    })

    it('does not synthesize owner fields for a status projection', () => {
        const plan = parseVllmGroupPlan(wire(heldRun().plan), { ownerFields: 'optional' })
        expect(plan).not.toHaveProperty('limits')
        expect(plan.members[0]).not.toHaveProperty('resources')
    })
})

describe('serving-group check parser and request gates', () => {
    it('reads participant capability results and ignores owner-only fields', () => {
        const check = wire(groupCheck(groupReview())) as Mutable
        check.participants[0].protocol = 'nvpair-vllm-group/1'
        check.participants[0].effectsApplied = false
        expect(parseVllmGroupCheck(check).participants[0]).toEqual({
            nodeId: 'node-a',
            state: 'available',
            activationEnabled: true,
            reason: ''
        })
        expect(() =>
            parseVllmGroupCheck(mutate(groupCheck(groupReview()), row => row.participants.pop()))
        ).toThrow()
        expect(() =>
            parseVllmGroupCheck(
                mutate(
                    groupCheck(groupReview()),
                    row => (row.participants[0].activationEnabled = 'yes')
                )
            )
        ).toThrow()
    })

    it('accepts only typed product requests and bounded one-use administrator access', () => {
        const selection = { nodeIds: ['node-a', 'node-b'], model: groupReview().plan.model }
        expect(parseVllmGroupSelection(selection)).toEqual(selection)
        expect(parseVllmGroupSelection({ ...selection, parallelism: 'pipeline' }).parallelism).toBe(
            'pipeline'
        )
        for (const bad of [
            { ...selection, runtime: 'caller-override' },
            { ...selection, nodeIds: ['node-a', 'node-a'] },
            { ...selection, nodeIds: ['node-a'] },
            { ...selection, nodeIds: ['node-a', 'node-b', 'node-c', 'node-d'] },
            { ...selection, parallelism: 'expert' },
            { ...selection, model: '' },
            { ...selection, elevation: [] },
            null
        ])
            expect(() => parseVllmGroupSelection(bad)).toThrow()

        expect(parseVllmGroupStartRequest({ reviewId: 'a'.repeat(32) })).toEqual({
            reviewId: 'a'.repeat(32)
        })
        expect(
            parseVllmGroupStartRequest({
                reviewId: 'a'.repeat(32),
                elevation: [
                    { nodeId: 'node-a', elevationPassword: 'fixture-secret' },
                    { nodeId: 'node-b', nonInteractive: true }
                ]
            })
        ).toEqual({
            reviewId: 'a'.repeat(32),
            elevation: [
                { nodeId: 'node-a', elevationPassword: 'fixture-secret' },
                { nodeId: 'node-b', nonInteractive: true }
            ]
        })
        for (const bad of [
            { reviewId: 'A'.repeat(32) },
            { reviewId: 'a'.repeat(31) },
            { reviewId: 'a'.repeat(32), elevation: [] },
            {
                reviewId: 'a'.repeat(32),
                elevation: [
                    {
                        nodeId: 'node-a',
                        elevationPassword: 'secret',
                        nonInteractive: true
                    }
                ]
            },
            {}
        ])
            expect(() => parseVllmGroupStartRequest(bad)).toThrow()

        expect(parseVllmGroupOperation({ runId: '1'.repeat(32), generation: 1 })).toEqual({
            runId: '1'.repeat(32),
            generation: 1
        })
        for (const bad of [
            { runId: '1'.repeat(32), generation: 0 },
            { runId: '1'.repeat(32), generation: 1.5 },
            { runId: '1'.repeat(32), generation: 1, planDigest: 'b'.repeat(64) },
            { runId: '1'.repeat(32), generation: 1, elevation: [] },
            { runId: 'x', generation: 1 }
        ])
            expect(() => parseVllmGroupOperation(bad)).toThrow()

        expect(
            parseVllmGroupReconcileRequest({
                runId: '1'.repeat(32),
                generation: 1,
                elevation: [{ nodeId: 'node-a', elevationPassword: 'fixture-secret' }]
            })
        ).toEqual({
            runId: '1'.repeat(32),
            generation: 1,
            elevation: [{ nodeId: 'node-a', elevationPassword: 'fixture-secret' }]
        })
    })
})
