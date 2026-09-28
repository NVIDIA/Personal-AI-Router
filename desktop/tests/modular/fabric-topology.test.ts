// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import type {
    CableRun,
    FabricCandidateIP,
    FabricInventorySnapshot,
    FabricOperation,
    FabricPortRef
} from '@/shared/types/fabric'
import { parseFabricInventoryRequest, parseFabricInventorySnapshot } from '@/shared/utils/fabric'
import {
    activeFabricTopology,
    buildFabricTopologyCandidate,
    fabricInventoryExpiryDelay
} from '@/ui/utils/fabric-topology'

const NOW = 1_800_000_000_000
const nodeIds = ['node-a', 'node-b', 'node-c']

function snapshot(missing?: { nodeId: string; portName: string }): FabricInventorySnapshot {
    return {
        nodes: nodeIds.map((nodeId, index) => ({
            nodeId,
            status: 'observed',
            observedAt: NOW,
            ports: ['p0', 'p1']
                .filter(portName => missing?.nodeId !== nodeId || missing.portName !== portName)
                .map(portName => ({
                    switchId: `switch-${index}`,
                    portName,
                    eligible: true
                }))
        }))
    }
}

function ref(nodeId: string, portName: string): FabricPortRef {
    return { nodeId, switchId: `switch-${nodeIds.indexOf(nodeId)}`, portName }
}

function cable(edges: Array<[FabricPortRef, FabricPortRef]>): CableRun {
    return {
        runId: '1'.repeat(32),
        reviewId: '2'.repeat(32),
        ownerNodeId: 'node-a',
        revision: 1,
        state: 'completed',
        targets: nodeIds.map(nodeId => ({
            nodeId,
            principal: `principal-${nodeId}`,
            ports: [],
            rawPrivilege: 'present'
        })),
        edges: edges.map(([left, right]) => ({ left, right, ageMs: 10, fresh: true })),
        result: 'reciprocal-observations',
        directness: 'unverified',
        remainingMs: 0,
        freshnessRemainingMs: 1000,
        cleanupConfirmed: true,
        startedAt: NOW - 1000,
        finishedAt: NOW,
        message: '',
        topology: { layout: 'ring', status: 'matched' }
    }
}

describe('fabric topology detection', () => {
    it('schedules one refresh at the oldest selected observation deadline', () => {
        const current = snapshot()
        current.nodes[1]!.observedAt = NOW - 20_000
        expect(fabricInventoryExpiryDelay(current, nodeIds, NOW)).toBe(10_000)
        expect(fabricInventoryExpiryDelay(current, nodeIds, NOW + 10_001)).toBe(0)
        expect(fabricInventoryExpiryDelay(current, ['unknown'], NOW)).toBeNull()
    })

    it('withdraws exact port autofill when the observation deadline passes', () => {
        const expired = snapshot()
        for (const node of expired.nodes) node.observedAt = NOW - 30_001
        const candidate = buildFabricTopologyCandidate(nodeIds, expired, null, new Map(), NOW)
        expect(candidate.selection).toBeNull()
        expect(candidate.issues).toContain(
            'node-a physical-port observation is stale; refresh detection.'
        )
    })

    it('prefills the exact p0/p1 three-node ring from fresh paired observations', () => {
        const candidate = buildFabricTopologyCandidate(nodeIds, snapshot(), null, new Map(), NOW)
        expect(candidate.layout).toBe('ring')
        expect(candidate.selection?.nodeIds).toEqual(nodeIds)
        expect(candidate.selection?.ports).toHaveLength(6)
        expect(candidate.edges.map(edge => `${edge.left.portName}-${edge.right.portName}`)).toEqual(
            ['p0-p1', 'p0-p1', 'p0-p1']
        )
        expect(candidate.issues).toContain(
            'Physical peer edges are not checked yet. Run the finite cable check.'
        )
    })

    it('falls back to the strongest exact direct pair and names the missing ring port', () => {
        const candidate = buildFabricTopologyCandidate(
            nodeIds,
            snapshot({ nodeId: 'node-c', portName: 'p1' }),
            null,
            new Map([['node-c', 'Spark C']]),
            NOW
        )
        expect(candidate.layout).toBe('direct')
        expect(candidate.selection?.nodeIds).toEqual(['node-a', 'node-b'])
        expect(candidate.issues).toContain('Spark C is missing eligible p1.')
    })

    it('names a misplaced edge before fabric review', () => {
        const candidate = buildFabricTopologyCandidate(
            nodeIds,
            snapshot(),
            cable([
                [ref('node-a', 'p1'), ref('node-b', 'p0')],
                [ref('node-b', 'p0'), ref('node-c', 'p1')],
                [ref('node-c', 'p0'), ref('node-a', 'p1')]
            ]),
            new Map(),
            NOW
        )
        expect(candidate.edges.some(edge => edge.state === 'misplaced')).toBe(true)
        expect(candidate.issues.some(issue => issue.startsWith('Misplaced edge:'))).toBe(true)
    })

    it('never renders expired historical cable evidence as a current match', () => {
        const expired = cable([
            [ref('node-a', 'p0'), ref('node-b', 'p1')],
            [ref('node-b', 'p0'), ref('node-c', 'p1')],
            [ref('node-c', 'p0'), ref('node-a', 'p1')]
        ])
        expired.freshnessRemainingMs = 0
        expired.edges = expired.edges.map(edge => ({ ...edge, fresh: false, ageMs: 2500 }))
        const candidate = buildFabricTopologyCandidate(nodeIds, snapshot(), expired, new Map(), NOW)
        expect(candidate.edges.every(edge => edge.state === 'pending')).toBe(true)
        expect(candidate.issues).toContain(
            'The last cable evidence has expired. Rerun the finite cable check before treating any edge as current.'
        )
    })
})

describe('active fabric topology', () => {
    const lane = (
        nodeId: string,
        portName: string,
        address: string,
        peerNodeId: string,
        peerAddress: string,
        gateway?: string
    ): FabricCandidateIP => ({
        nodeId,
        peerNodeId,
        peerPrincipal: `principal-${peerNodeId}`,
        address,
        peerAddress,
        interfaceName: `if-${portName}`,
        interfaceIndex: 1,
        mac: '00:00:00:00:00:00',
        switchId: `switch-${nodeIds.indexOf(nodeId)}`,
        portName,
        rdmaDevice: '',
        rdmaPort: 0,
        gidIndex: 0,
        gidType: '',
        ...(gateway ? { gateway } : {})
    })
    const ring = (state: FabricOperation['state']): FabricOperation => ({
        schemaVersion: 1,
        operationId: '3'.repeat(32),
        reviewId: '4'.repeat(32),
        ownerNodeId: 'node-a',
        state,
        targets: nodeIds.map(nodeId => ({
            nodeId,
            principal: `principal-${nodeId}`,
            interfaces: []
        })),
        cleanupConfirmed: false,
        effectsApplied: true,
        message: '',
        createdAt: NOW,
        expiresAt: NOW + 60_000,
        qualifiedAt: NOW,
        candidateIPs: [
            lane('node-a', 'p0', '10.0.0.0', 'node-b', '10.0.0.1'),
            lane('node-b', 'p1', '10.0.0.1', 'node-a', '10.0.0.0'),
            lane('node-a', 'p1', '10.0.0.2', 'node-c', '10.0.0.3'),
            lane('node-c', 'p0', '10.0.0.3', 'node-a', '10.0.0.2'),
            lane('node-b', 'p0', '10.0.0.4', 'node-c', '10.0.0.5'),
            lane('node-c', 'p1', '10.0.0.5', 'node-b', '10.0.0.4'),
            lane('node-a', 'p0', '10.0.0.0', 'node-b', '10.0.0.4', '10.0.0.1'),
            lane('node-b', 'p0', '10.0.0.4', 'node-c', '10.0.0.3', '10.0.0.5'),
            lane('node-c', 'p0', '10.0.0.3', 'node-a', '10.0.0.0', '10.0.0.2')
        ]
    })

    it('draws one live edge per qualified cable and ignores routed paths', () => {
        const topology = activeFabricTopology(ring('active'))
        expect(topology?.layout).toBe('ring')
        expect(topology?.edges.map(edge => edge.state)).toEqual(['active', 'active', 'active'])
        expect(
            topology?.edges.map(
                edge =>
                    `${edge.left.nodeId}.${edge.left.portName}-${edge.right.nodeId}.${edge.right.portName}`
            )
        ).toEqual(['node-a.p0-node-b.p1', 'node-a.p1-node-c.p0', 'node-b.p0-node-c.p1'])
    })

    it('draws nothing for a fabric that is not active', () => {
        expect(activeFabricTopology(ring('applying'))).toBeNull()
        expect(activeFabricTopology(ring('rolling-back'))).toBeNull()
    })
})

describe('fabric inventory parser', () => {
    it('accepts only a bounded distinct request and exact observation schema', () => {
        expect(parseFabricInventoryRequest({ nodeIds })).toEqual({ nodeIds })
        expect(parseFabricInventorySnapshot(snapshot())).toEqual(snapshot())
        expect(() => parseFabricInventoryRequest({ nodeIds: ['node-a', 'node-a'] })).toThrow()
        expect(() =>
            parseFabricInventorySnapshot({
                ...snapshot(),
                nodes: [{ ...snapshot().nodes[0], arbitrary: 'command' }]
            })
        ).toThrow()
    })
})
