// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import type { EngineStatusByNode, EngineStatusData } from '@/shared/types/engines'
import type { NodeItem } from '@/shared/types/nodes'
import type {
    VllmServingGroupRoute,
    VllmServingGroupRouteState
} from '@/shared/types/vllm-group-status'
import { buildOverviewNodeSections } from '@/ui/utils/overview-vllm-groups'

const node = (id: string): NodeItem => ({
    id,
    name: id,
    status: 'active',
    ipAddress: '192.0.2.1',
    port: 14318,
    allIpAddresses: ['192.0.2.1'],
    topology: { cpu: { model: '', cores: 0, threads: 0 }, gpus: [], ram: 0, storage: [] }
})

const route = (
    runId: string,
    coordinator: string,
    role: VllmServingGroupRoute['role'],
    members: string[],
    state: VllmServingGroupRouteState = 'ready'
): VllmServingGroupRoute => ({
    runId,
    generation: 1,
    model: `model/${runId[0]}`,
    coordinator,
    role,
    state,
    members
})

function statuses(routes: Array<[string, VllmServingGroupRoute]>): EngineStatusByNode {
    const result: EngineStatusByNode = new Map()
    routes.forEach(([nodeId, servingGroup]) => {
        const status: EngineStatusData = {
            nodeId,
            engineType: 'vllm',
            processStatus: 'running',
            enginePort: 8001,
            proxyPort: null,
            servingGroup
        }
        result.set(nodeId, new Map([['vllm', status]]))
    })
    return result
}

describe('Overview ready vLLM groups', () => {
    it('reorders non-adjacent members into multiple disjoint group sections', () => {
        const ids = ['node-a', 'free', 'node-b', 'node-c', 'node-d']
        const first = 'a'.repeat(32)
        const second = 'b'.repeat(32)
        const result = buildOverviewNodeSections(
            ids.map(node),
            statuses([
                ['node-a', route(first, 'node-a', 'coordinator', ['node-a', 'node-b'])],
                ['node-b', route(first, 'node-a', 'participant', ['node-a', 'node-b'], 'started')],
                ['node-c', route(second, 'node-c', 'coordinator', ['node-c', 'node-d'])],
                ['node-d', route(second, 'node-c', 'participant', ['node-c', 'node-d'], 'started')]
            ])
        )

        expect(
            result.map(section =>
                section.kind === 'group'
                    ? section.nodes.map(member => member.id).join('+')
                    : section.node.id
            )
        ).toEqual(['node-a+node-b', 'free', 'node-c+node-d'])
        expect(result.filter(section => section.kind === 'group')).toHaveLength(2)
    })

    it('leaves incomplete, non-ready, mismatched and overlapping executions ungrouped', () => {
        const ids = ['node-a', 'node-b', 'node-c']
        const runId = 'c'.repeat(32)
        const forbiddenParticipantStates: VllmServingGroupRouteState[] = [
            'prepared',
            'starting',
            'stopping',
            'stopped',
            'failed',
            'cleanup-required'
        ]
        const cases: EngineStatusByNode[] = [
            statuses([['node-a', route(runId, 'node-a', 'coordinator', ['node-a', 'node-b'])]]),
            ...forbiddenParticipantStates.map(state =>
                statuses([
                    ['node-a', route(runId, 'node-a', 'coordinator', ['node-a', 'node-b'])],
                    ['node-b', route(runId, 'node-a', 'participant', ['node-a', 'node-b'], state)]
                ])
            ),
            statuses([
                ['node-a', route(runId, 'node-a', 'coordinator', ['node-a', 'node-b'])],
                ['node-b', route('d'.repeat(32), 'node-a', 'participant', ['node-a', 'node-b'])]
            ]),
            statuses([
                ['node-a', route(runId, 'node-a', 'coordinator', ['node-a', 'node-b'])],
                ['node-b', route(runId, 'node-a', 'participant', ['node-a', 'node-b'])],
                ['node-c', route('e'.repeat(32), 'node-c', 'coordinator', ['node-b', 'node-c'])]
            ])
        ]

        cases.forEach(state => {
            const result = buildOverviewNodeSections(ids.map(node), state)
            expect(result.every(section => section.kind === 'node')).toBe(true)
        })
    })
})
