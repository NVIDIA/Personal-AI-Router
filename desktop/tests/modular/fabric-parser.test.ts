// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import {
    cableSucceeded,
    fabricOperationCarriesCableMatch,
    parseCableCleanupReview,
    parseCableReview,
    parseCableRun,
    parseFabricOperation,
    parseFabricSelection
} from '@/shared/utils/fabric'

const id = (value: string) => value.repeat(32).slice(0, 32)
const hostKeyFingerprint = `SHA256:${'A'.repeat(43)}`
const invalidHostKeyFingerprints = [
    `sha256:${'A'.repeat(43)}`,
    `SHA256:${'A'.repeat(42)}`,
    `SHA256:${'A'.repeat(44)}`,
    `SHA256:${'A'.repeat(42)}_`,
    `SHA256:${'A'.repeat(42)}B`
]
const port = (nodeId: string, portName = 'p0') => ({
    nodeId,
    switchId: `switch-${nodeId}`,
    portName
})
const target = (nodeId: string) => ({
    nodeId,
    principal: `principal-${nodeId}`,
    ports: [
        {
            switchId: `switch-${nodeId}`,
            portName: 'p0',
            interfaces: [
                {
                    name: `eth-${nodeId}`,
                    index: nodeId === 'node-a' ? 1 : 2,
                    mac: '02:00:00:00:00:01'
                }
            ]
        }
    ],
    rawPrivilege: 'present'
})

function cableRun() {
    return {
        runId: id('a'),
        reviewId: 'cable-review-a',
        ownerNodeId: 'node-a',
        revision: 2,
        state: 'completed',
        targets: [target('node-a'), target('node-b')],
        edges: [{ left: port('node-a'), right: port('node-b'), ageMs: 20, fresh: false }],
        result: 'reciprocal-observations',
        directness: 'unverified',
        remainingMs: 0,
        freshnessRemainingMs: 0,
        cleanupConfirmed: true,
        startedAt: 1,
        finishedAt: 2,
        message: 'Matched observations.',
        topology: { layout: 'direct', status: 'matched' }
    }
}

describe('physical cable and fabric parsers', () => {
    it('admits only exact two-node or p0/p1 ring selections without secrets or callbacks', () => {
        const direct = { nodeIds: ['node-a', 'node-b'], ports: [port('node-a'), port('node-b')] }
        expect(parseFabricSelection(direct)).toEqual(direct)
        expect(
            parseFabricSelection({
                nodeIds: ['node-a', 'node-b', 'node-c'],
                ports: ['node-a', 'node-b', 'node-c'].flatMap(nodeId => [
                    port(nodeId, 'p0'),
                    port(nodeId, 'p1')
                ])
            }).ports
        ).toHaveLength(6)
        for (const bad of [
            { ...direct, password: 'secret' },
            { ...direct, callback: 'http://private' },
            { ...direct, nodeIds: ['node-a', 'node-a'] },
            { ...direct, ports: [port('node-a'), port('node-a', 'p1')] },
            {
                nodeIds: ['node-a', 'node-b', 'node-c'],
                ports: ['node-a', 'node-b', 'node-c'].flatMap(nodeId => [
                    port(nodeId, 'p0'),
                    port(nodeId, 'p2')
                ])
            }
        ])
            expect(() => parseFabricSelection(bad)).toThrow()
    })

    it('accepts only canonical OpenSSH SHA-256 fingerprints in selected host keys', () => {
        const direct = { nodeIds: ['node-a', 'node-b'], ports: [port('node-a'), port('node-b')] }
        expect(
            parseFabricSelection({
                ...direct,
                acceptedHostKeys: [{ candidateId: 'candidate-a', sha256: hostKeyFingerprint }]
            }).acceptedHostKeys
        ).toEqual([{ candidateId: 'candidate-a', sha256: hostKeyFingerprint }])
        for (const sha256 of invalidHostKeyFingerprints)
            expect(() =>
                parseFabricSelection({
                    ...direct,
                    acceptedHostKeys: [{ candidateId: 'candidate-a', sha256 }]
                })
            ).toThrow(/invalid host key fingerprint/)
    })

    it('keeps directness unverified and accepts reciprocal observations only as cable evidence', () => {
        const parsed = parseCableRun(cableRun())
        expect(parsed.directness).toBe('unverified')
        expect(cableSucceeded(parsed)).toBe(true)
        expect(() => parseCableRun({ ...cableRun(), directness: 'direct' })).toThrow()
        expect(() => parseCableRun({ ...cableRun(), rdma: true })).toThrow()
        expect(
            cableSucceeded(
                parseCableRun({
                    ...cableRun(),
                    result: 'incomplete',
                    topology: { layout: 'direct', status: 'missing' }
                })
            )
        ).toBe(false)
    })

    it('keeps a bound cable match visible only while fabric effects remain live or held', () => {
        const operation = {
            schemaVersion: 1 as const,
            operationId: id('c'),
            reviewId: id('c'),
            ownerNodeId: 'node-a',
            cableRunId: id('a'),
            state: 'active' as const,
            targets: [],
            cleanupConfirmed: false,
            effectsApplied: true,
            message: 'Fabric active.',
            createdAt: 1,
            expiresAt: 0
        }

        expect(fabricOperationCarriesCableMatch(operation)).toBe(true)
        expect(
            fabricOperationCarriesCableMatch({
                ...operation,
                state: 'recovery-required',
                effectsApplied: false,
                effectsUnconfirmed: true
            })
        ).toBe(true)
        expect(fabricOperationCarriesCableMatch({ ...operation, cableRunId: undefined })).toBe(
            false
        )
        expect(
            fabricOperationCarriesCableMatch({
                ...operation,
                state: 'cancelled',
                cleanupConfirmed: true
            })
        ).toBe(false)
        expect(
            fabricOperationCarriesCableMatch({
                ...operation,
                state: 'failed',
                effectsApplied: false
            })
        ).toBe(false)
    })

    it('parses permission metadata without accepting private access material', () => {
        const review = {
            reviewId: 'cable-review-a',
            ownerNodeId: 'node-a',
            targets: [target('node-a'), target('node-b')],
            available: true,
            reason: 'Review exact effects.',
            remainingMs: 20_000,
            permission: {
                mode: 'temporary-admin',
                targets: ['node-a', 'node-b'].map(nodeId => ({
                    nodeId,
                    candidateId: `candidate-${nodeId}`,
                    accessLabel: nodeId,
                    hostKeySha256: hostKeyFingerprint,
                    hostKeyTrusted: false,
                    accessAvailable: true,
                    elevationAvailable: true,
                    workerAvailable: true,
                    workerSha256: 'b'.repeat(64),
                    reason: 'Approval required.'
                })),
                effects: ['Finite check only.']
            }
        }
        expect(parseCableReview(review).permission?.targets).toHaveLength(2)
        for (const hostKeySha256 of invalidHostKeyFingerprints)
            expect(() =>
                parseCableReview({
                    ...review,
                    permission: {
                        ...review.permission,
                        targets: review.permission.targets.map(item => ({
                            ...item,
                            hostKeySha256
                        }))
                    }
                })
            ).toThrow(/invalid host key fingerprint/)
        expect(() =>
            parseCableReview({
                ...review,
                permission: {
                    ...review.permission,
                    targets: review.permission.targets.map(item => ({
                        ...item,
                        password: 'secret'
                    }))
                }
            })
        ).toThrow()
    })

    it('uses the same canonical fingerprint contract for held-run cleanup access', () => {
        const review = {
            reviewId: 'cable-cleanup-review-a',
            runId: id('a'),
            available: false,
            remainingMs: 20_000,
            effects: ['Inspect only this run.'],
            targets: [
                {
                    nodeId: 'node-a',
                    candidateId: 'candidate-a',
                    accessLabel: 'node-a',
                    hostKeySha256: hostKeyFingerprint,
                    hostKeyTrusted: false,
                    accessAvailable: false,
                    elevationAvailable: false,
                    inspectorAvailable: false
                }
            ]
        }
        expect(parseCableCleanupReview(review).targets[0].hostKeySha256).toBe(hostKeyFingerprint)
        expect(() =>
            parseCableCleanupReview({
                ...review,
                targets: [{ ...review.targets[0], hostKeySha256: `SHA256:${'A'.repeat(42)}_` }]
            })
        ).toThrow(/invalid host key fingerprint/)
    })

    it('strictly parses authoritative fabric operation state and never invents RDMA success', () => {
        const fabricTargets = ['node-a', 'node-b'].map((nodeId, nodeIndex) => ({
            nodeId,
            principal: `principal-${nodeId}`,
            switchId: `switch-${nodeId}`,
            portName: 'p0',
            interfaces: [0, 1].map(lane => ({
                name: `eth-${nodeId}-${lane}`,
                index: nodeIndex * 2 + lane + 1,
                mac: `02:00:00:00:0${nodeIndex}:0${lane}`,
                physicalPort: {
                    source: 'devlink',
                    switchId: `switch-${nodeId}`,
                    portName: 'p0'
                },
                addresses: [],
                address: `10.0.${lane}.${nodeIndex + 1}/30`,
                driver: 'mlx5_core',
                rdmaDevices: [`mlx5_${nodeIndex * 2 + lane}`],
                mtu: 9000
            }))
        }))
        const candidates = fabricTargets.flatMap((fabricTarget, nodeIndex) =>
            fabricTarget.interfaces.map((iface, lane) => ({
                nodeId: fabricTarget.nodeId,
                peerNodeId: fabricTargets[1 - nodeIndex].nodeId,
                peerPrincipal: fabricTargets[1 - nodeIndex].principal,
                address: iface.address.split('/')[0],
                peerAddress: fabricTargets[1 - nodeIndex].interfaces[lane].address.split('/')[0],
                interfaceName: iface.name,
                interfaceIndex: iface.index,
                mac: iface.mac,
                switchId: fabricTarget.switchId,
                portName: fabricTarget.portName,
                rdmaDevice: iface.rdmaDevices[0],
                rdmaPort: 1,
                gidIndex: 0,
                gidType: 'RoCE v2'
            }))
        )
        const operation = {
            schemaVersion: 1,
            operationId: id('c'),
            reviewId: id('c'),
            ownerNodeId: 'node-a',
            recipeId: 'spark-two-node-temporary-addresses-v1',
            state: 'active',
            targets: fabricTargets,
            cleanupConfirmed: false,
            effectsApplied: true,
            message: 'Fast control route qualified; RDMA remains unverified.',
            createdAt: 1,
            expiresAt: 0,
            qualifiedAt: 2,
            qualificationDigest: 'a'.repeat(64),
            candidateIPs: candidates
        }
        expect(parseFabricOperation(operation).state).toBe('active')
        expect(() => parseFabricOperation({ ...operation, rdmaVerified: true })).toThrow()
        expect(() => parseFabricOperation({ ...operation, state: 'ready' })).toThrow()
        const failed = {
            ...operation,
            state: 'failed',
            cleanupConfirmed: true,
            qualifiedAt: undefined,
            qualificationDigest: undefined,
            candidateIPs: undefined,
            message: 'Exact cleanup confirmed.',
            failure: { nodeId: 'node-b', phase: 'apply', code: 'provider-failed' }
        }
        expect(parseFabricOperation(failed).failure?.phase).toBe('apply')
        expect(
            parseFabricOperation({
                ...failed,
                failure: { nodeId: 'node-b', phase: 'qualify', code: 'route-unavailable' }
            }).failure?.phase
        ).toBe('qualify')
        expect(() =>
            parseFabricOperation({
                ...failed,
                failure: { nodeId: 'node-b', phase: 'execute', code: 'provider-failed' }
            })
        ).toThrow()
    })
})
