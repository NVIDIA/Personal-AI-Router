// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { EngineStatusByNode } from '@/shared/types/engines'
import type { NodeItem } from '@/shared/types/nodes'
import type { VllmServingGroupRoute } from '@/shared/types/vllm-group-status'

type OverviewNodeSection =
    | { kind: 'node'; node: NodeItem }
    | { kind: 'group'; id: string; route: VllmServingGroupRoute; nodes: NodeItem[] }

function routeId(route: VllmServingGroupRoute): string {
    return `${route.coordinator}:${route.runId}:${route.generation}`
}

function sameRoute(left: VllmServingGroupRoute, right: VllmServingGroupRoute): boolean {
    return (
        routeId(left) === routeId(right) &&
        left.model === right.model &&
        left.members.length === right.members.length &&
        left.members.every((member, index) => member === right.members[index])
    )
}

/**
 * Partition online nodes into exact, collectively-ready vLLM groups. A
 * conflicting or incomplete receipt fails closed and leaves its nodes ordinary.
 */
export function buildOverviewNodeSections(
    nodes: readonly NodeItem[],
    statuses: EngineStatusByNode
): OverviewNodeSection[] {
    const byId = new Map(nodes.map(node => [node.id, node]))
    const candidates = nodes.flatMap(node => {
        const status = statuses.get(node.id)?.get('vllm')
        const route = status?.servingGroup
        if (
            status?.processStatus !== 'running' ||
            !route ||
            route.state !== 'ready' ||
            route.role !== 'coordinator' ||
            route.coordinator !== node.id
        )
            return []

        const members = route.members.map(member => byId.get(member))
        if (members.some(member => !member)) return []

        return [{ id: routeId(route), route, nodes: members as NodeItem[] }]
    })

    // Two retained executions may never claim the same card. Reject every
    // overlapping candidate instead of letting iteration order choose a winner.
    const claims = new Map<string, number>()
    candidates.forEach(group =>
        group.route.members.forEach(member => claims.set(member, (claims.get(member) ?? 0) + 1))
    )
    const groups = candidates.filter(
        group =>
            group.route.members.every(member => claims.get(member) === 1) &&
            group.route.members.every(memberId => {
                const memberStatus = statuses.get(memberId)?.get('vllm')
                const memberRoute = memberStatus?.servingGroup
                const routeStateMatches =
                    memberId === group.route.coordinator
                        ? memberRoute?.state === 'ready'
                        : memberRoute?.state === 'started' || memberRoute?.state === 'ready'
                return (
                    memberStatus?.processStatus === 'running' &&
                    !!memberRoute &&
                    routeStateMatches &&
                    memberRoute.role ===
                        (memberId === group.route.coordinator ? 'coordinator' : 'participant') &&
                    sameRoute(group.route, memberRoute)
                )
            })
    )
    const groupByNode = new Map<string, (typeof groups)[number]>()
    groups.forEach(group => group.nodes.forEach(node => groupByNode.set(node.id, group)))
    const emitted = new Set<string>()

    return nodes.flatMap<OverviewNodeSection>(node => {
        const group = groupByNode.get(node.id)
        if (!group) return [{ kind: 'node' as const, node }]
        if (emitted.has(group.id)) return []
        emitted.add(group.id)
        return [{ kind: 'group' as const, ...group }]
    })
}
