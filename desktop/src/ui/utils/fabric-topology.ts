// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type {
    CableRun,
    FabricInventorySnapshot,
    FabricPortObservation,
    FabricPortRef,
    FabricSelection
} from '@/shared/types/fabric'
import { parseFabricSelection } from '@/shared/utils/fabric'

export type DetectedFabricLayout = 'single' | 'direct' | 'ring'
export type FabricEdgeState = 'pending' | 'matched' | 'missing' | 'misplaced' | 'duplicate'

export interface FabricDiagramEdge {
    left: FabricPortRef
    right: FabricPortRef
    state: FabricEdgeState
}

export interface FabricTopologyCandidate {
    layout: DetectedFabricLayout
    nodeIds: string[]
    ports: FabricPortRef[]
    selection: FabricSelection | null
    edges: FabricDiagramEdge[]
    issues: string[]
}

const observationMaxAgeMs = 30_000
const cableEvidenceMaxAgeMs = 2_000

/** One-shot delay until the oldest selected physical-port observation expires. */
export function fabricInventoryExpiryDelay(
    snapshot: FabricInventorySnapshot | null,
    nodeIds: string[],
    now = Date.now()
): number | null {
    const selected = new Set(nodeIds)
    const deadlines = (snapshot?.nodes ?? [])
        .filter(
            node => selected.has(node.nodeId) && node.status === 'observed' && !!node.observedAt
        )
        .map(node => node.observedAt! + observationMaxAgeMs)
    return deadlines.length === 0 ? null : Math.max(0, Math.min(...deadlines) - now)
}

function combinations(values: string[], count: number): string[][] {
    const result: string[][] = []
    const visit = (start: number, picked: string[]) => {
        if (picked.length === count) {
            result.push(picked)
            return
        }
        for (let index = start; index <= values.length - (count - picked.length); index += 1)
            visit(index + 1, [...picked, values[index]!])
    }
    visit(0, [])
    return result
}

function eligiblePorts(
    snapshot: FabricInventorySnapshot | null,
    nodeId: string,
    now: number
): FabricPortObservation[] {
    const node = snapshot?.nodes.find(entry => entry.nodeId === nodeId)
    if (
        !node ||
        node.status !== 'observed' ||
        !node.observedAt ||
        now - node.observedAt > observationMaxAgeMs
    )
        return []
    return node.ports.filter(port => port.eligible)
}

function directSelection(
    nodeIds: string[],
    snapshot: FabricInventorySnapshot | null,
    now: number
): FabricSelection | null {
    const ports: FabricPortRef[] = []
    for (const nodeId of nodeIds) {
        const eligible = eligiblePorts(snapshot, nodeId, now).sort((left, right) => {
            const preferred = (name: string) => (name === 'p0' ? 0 : name === 'p1' ? 1 : 2)
            return (
                preferred(left.portName) - preferred(right.portName) ||
                left.portName.localeCompare(right.portName) ||
                left.switchId.localeCompare(right.switchId)
            )
        })
        const port = eligible[0]
        if (!port) return null
        ports.push({ nodeId, switchId: port.switchId, portName: port.portName })
    }
    return parseFabricSelection({ nodeIds, ports })
}

function ringSelection(
    nodeIds: string[],
    snapshot: FabricInventorySnapshot | null,
    now: number
): FabricSelection | null {
    const ports: FabricPortRef[] = []
    for (const nodeId of nodeIds) {
        const eligible = eligiblePorts(snapshot, nodeId, now)
        const p0 = eligible.filter(port => port.portName === 'p0')
        const p1 = eligible.filter(port => port.portName === 'p1')
        if (p0.length !== 1 || p1.length !== 1 || p0[0]!.switchId !== p1[0]!.switchId) return null
        ports.push(
            { nodeId, switchId: p0[0]!.switchId, portName: 'p0' },
            { nodeId, switchId: p1[0]!.switchId, portName: 'p1' }
        )
    }
    return parseFabricSelection({ nodeIds, ports })
}

function port(selection: FabricSelection, nodeId: string, portName?: string): FabricPortRef {
    return selection.ports.find(
        entry => entry.nodeId === nodeId && (!portName || entry.portName === portName)
    )!
}

function expectedEdges(selection: FabricSelection): FabricDiagramEdge[] {
    if (selection.nodeIds.length === 2)
        return [
            {
                left: port(selection, selection.nodeIds[0]!),
                right: port(selection, selection.nodeIds[1]!),
                state: 'pending'
            }
        ]
    const [a, b, c] = selection.nodeIds
    return [
        { left: port(selection, a!, 'p0'), right: port(selection, b!, 'p1'), state: 'pending' },
        { left: port(selection, b!, 'p0'), right: port(selection, c!, 'p1'), state: 'pending' },
        { left: port(selection, c!, 'p0'), right: port(selection, a!, 'p1'), state: 'pending' }
    ]
}

function portKey(port: FabricPortRef): string {
    return `${port.nodeId}\n${port.switchId}\n${port.portName}`
}

function edgeKey(left: FabricPortRef, right: FabricPortRef): string {
    return [portKey(left), portKey(right)].sort().join('\n--\n')
}

function nodePair(left: FabricPortRef, right: FabricPortRef): string {
    return [left.nodeId, right.nodeId].sort().join('\n')
}

function sameNodeSet(left: string[], right: string[]): boolean {
    return (
        left.length === right.length && [...left].sort().join('\n') === [...right].sort().join('\n')
    )
}

function applyCableEvidence(
    edges: FabricDiagramEdge[],
    run: CableRun | null,
    nodeIds: string[],
    label: (nodeId: string) => string
): { edges: FabricDiagramEdge[]; issues: string[] } {
    if (
        !run ||
        !sameNodeSet(
            nodeIds,
            run.targets.map(target => target.nodeId)
        )
    )
        return {
            edges,
            issues: ['Physical peer edges are not checked yet. Run the finite cable check.']
        }
    if (
        run.freshnessRemainingMs <= 0 ||
        run.edges.some(edge => !edge.fresh || edge.ageMs > cableEvidenceMaxAgeMs)
    )
        return {
            edges,
            issues: [
                'The last cable evidence has expired. Rerun the finite cable check before treating any edge as current.'
            ]
        }
    if (
        run.state !== 'completed' ||
        !run.cleanupConfirmed ||
        !['incomplete', 'reciprocal-observations', 'ambiguous'].includes(run.result)
    )
        return {
            edges,
            issues: [
                'The last cable check produced no reliable peer-edge evidence. Resolve its reported failure and rerun it.'
            ]
        }

    const observed = run.edges
    const issues: string[] = []
    const result = edges.map(expected => {
        const exact = observed.filter(
            edge => edgeKey(edge.left, edge.right) === edgeKey(expected.left, expected.right)
        )
        const samePair = observed.filter(
            edge => nodePair(edge.left, edge.right) === nodePair(expected.left, expected.right)
        )
        if (exact.length > 1 || samePair.length > 1) {
            issues.push(
                `Duplicate edge observed between ${label(expected.left.nodeId)} and ${label(expected.right.nodeId)}.`
            )
            return { ...expected, state: 'duplicate' as const }
        }
        if (exact.length === 1) return { ...expected, state: 'matched' as const }
        if (samePair.length === 1) {
            const actual = samePair[0]!
            issues.push(
                `Misplaced edge: ${label(actual.left.nodeId)} ${actual.left.portName} is connected to ${label(actual.right.nodeId)} ${actual.right.portName}; expected ${label(expected.left.nodeId)} ${expected.left.portName} to ${label(expected.right.nodeId)} ${expected.right.portName}.`
            )
            return { ...expected, state: 'misplaced' as const }
        }
        issues.push(
            `Missing edge: connect ${label(expected.left.nodeId)} ${expected.left.portName} to ${label(expected.right.nodeId)} ${expected.right.portName}.`
        )
        return { ...expected, state: 'missing' as const }
    })
    for (const edge of observed) {
        if (
            !edges.some(
                expected =>
                    nodePair(expected.left, expected.right) === nodePair(edge.left, edge.right)
            )
        )
            issues.push(
                `Misplaced edge: ${label(edge.left.nodeId)} ${edge.left.portName} is connected to ${label(edge.right.nodeId)} ${edge.right.portName}.`
            )
    }
    return { edges: result, issues: Array.from(new Set(issues)) }
}

function inventoryIssues(
    nodeIds: string[],
    snapshot: FabricInventorySnapshot | null,
    now: number,
    layout: Exclude<DetectedFabricLayout, 'single'>,
    label: (nodeId: string) => string
): string[] {
    const issues: string[] = []
    for (const nodeId of nodeIds) {
        const observation = snapshot?.nodes.find(node => node.nodeId === nodeId)
        if (!observation || observation.status !== 'observed') {
            issues.push(`${label(nodeId)} has no current paired physical-port observation.`)
            continue
        }
        if (!observation.observedAt || now - observation.observedAt > observationMaxAgeMs) {
            issues.push(`${label(nodeId)} physical-port observation is stale; refresh detection.`)
            continue
        }
        const eligible = observation.ports.filter(port => port.eligible)
        if (layout === 'direct' && eligible.length === 0)
            issues.push(`${label(nodeId)} has no eligible active 200-Gbit ConnectX port.`)
        if (layout === 'ring') {
            for (const portName of ['p0', 'p1']) {
                const matches = eligible.filter(port => port.portName === portName)
                if (matches.length === 0)
                    issues.push(`${label(nodeId)} is missing eligible ${portName}.`)
                else if (matches.length > 1)
                    issues.push(
                        `${label(nodeId)} reports duplicate eligible ${portName} identities.`
                    )
            }
            const p0 = eligible.find(port => port.portName === 'p0')
            const p1 = eligible.find(port => port.portName === 'p1')
            if (p0 && p1 && p0.switchId !== p1.switchId)
                issues.push(
                    `${label(nodeId)} p0 and p1 are on different physical switch identities.`
                )
        }
    }
    return issues
}

/** Choose the strongest exact topology the currently observed nodes can support. */
export function buildFabricTopologyCandidate(
    nodeIds: string[],
    snapshot: FabricInventorySnapshot | null,
    cableRun: CableRun | null,
    names: ReadonlyMap<string, string> = new Map(),
    now = Date.now()
): FabricTopologyCandidate {
    const distinctNodes = Array.from(new Set(nodeIds)).slice(0, 3)
    const label = (nodeId: string) => names.get(nodeId) || nodeId
    for (const group of combinations(distinctNodes, 3)) {
        const selection = ringSelection(group, snapshot, now)
        if (!selection) continue
        const evidence = applyCableEvidence(expectedEdges(selection), cableRun, group, label)
        return {
            layout: 'ring',
            nodeIds: group,
            ports: selection.ports,
            selection,
            edges: evidence.edges,
            issues: evidence.issues
        }
    }
    for (const group of combinations(distinctNodes, 2)) {
        const selection = directSelection(group, snapshot, now)
        if (!selection) continue
        const evidence = applyCableEvidence(expectedEdges(selection), cableRun, group, label)
        const ringIssues =
            distinctNodes.length >= 3
                ? inventoryIssues(distinctNodes, snapshot, now, 'ring', label)
                : []
        return {
            layout: 'direct',
            nodeIds: group,
            ports: selection.ports,
            selection,
            edges: evidence.edges,
            issues: [...ringIssues, ...evidence.issues]
        }
    }

    const desired = distinctNodes.length >= 3 ? 'ring' : 'direct'
    const selected = distinctNodes.slice(0, desired === 'ring' ? 3 : 2)
    return {
        layout: distinctNodes.length < 2 ? 'single' : desired,
        nodeIds: selected,
        ports: [],
        selection: null,
        edges: [],
        issues:
            distinctNodes.length < 2
                ? ['One node is visible. Add a paired node for a direct link or two for a ring.']
                : inventoryIssues(selected, snapshot, now, desired, label)
    }
}
