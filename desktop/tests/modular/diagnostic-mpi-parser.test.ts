// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import {
    bindDiagnosticMPIOperation,
    parseDiagnosticMPIManagedInventory,
    parseDiagnosticMPIOperation,
    parseDiagnosticMPIRecovery,
    parseDiagnosticMPIReview,
    parseDiagnosticMPIReviewRequest,
    parseDiagnosticMPISelection
} from '@/shared/utils/diagnostic-mpi'
import {
    buildId,
    fabricOperationId,
    operationId,
    rawFabricReview,
    rawInventory,
    rawOperation,
    rawReview,
    reviewId,
    sshHostKeyFingerprint
} from './diagnostic-mpi-fixtures'

describe('managed NCCL Desktop contract', () => {
    it('projects only safe registry fields and never forwards registration or key material', () => {
        const inventory = parseDiagnosticMPIManagedInventory(rawInventory)
        expect(inventory).toEqual({
            records: [
                {
                    buildOperationId: buildId,
                    adoptedAt: 1_790_000_000_000,
                    adopted: true,
                    runtimeValidated: false,
                    runAvailable: false,
                    targets: [
                        { nodeId: 'node-a', local: true },
                        { nodeId: 'node-b', local: false }
                    ]
                }
            ],
            recoveryRequired: false
        })
        expect(JSON.stringify(inventory)).not.toMatch(/password|privateKey|sshHostKey|clusterPin/i)
        expect(sshHostKeyFingerprint).toMatch(/^SHA256:/)
        const remote = rawInventory.records[0].targets[1]
        expect(() =>
            parseDiagnosticMPIManagedInventory({
                ...rawInventory,
                records: [
                    {
                        ...rawInventory.records[0],
                        targets: [
                            rawInventory.records[0].targets[0],
                            { ...remote, sshHostKeySha256: 'd'.repeat(64) }
                        ]
                    }
                ]
            })
        ).toThrow(/SSH host key/i)
    })

    it('canonicalizes exactly two or three nodes and rejects generic relay fields', () => {
        expect(
            parseDiagnosticMPISelection({
                buildOperationId: buildId,
                nodeIds: ['node-b', 'node-a']
            })
        ).toEqual({ buildOperationId: buildId, nodeIds: ['node-a', 'node-b'] })
        expect(() =>
            parseDiagnosticMPISelection({
                buildOperationId: buildId,
                nodeIds: ['node-a'],
                command: 'mpirun'
            })
        ).toThrow(/fields/i)
        expect(() =>
            parseDiagnosticMPISelection({
                buildOperationId: buildId,
                nodeIds: ['node-a', 'node-a']
            })
        ).toThrow(/duplicate/i)
    })

    it('requires an explicit management or fabric network for a review', () => {
        expect(
            parseDiagnosticMPIReviewRequest({
                buildOperationId: buildId,
                nodeIds: ['node-b', 'node-a'],
                network: 'fabric'
            })
        ).toEqual({ buildOperationId: buildId, nodeIds: ['node-a', 'node-b'], network: 'fabric' })
        expect(() =>
            parseDiagnosticMPIReviewRequest({
                buildOperationId: buildId,
                nodeIds: ['node-a', 'node-b']
            })
        ).toThrow(/incomplete/i)
        expect(() =>
            parseDiagnosticMPIReviewRequest({
                buildOperationId: buildId,
                nodeIds: ['node-a', 'node-b'],
                network: 'rdma'
            })
        ).toThrow(/network/i)
        expect(() =>
            parseDiagnosticMPIReviewRequest({
                buildOperationId: buildId,
                nodeIds: ['node-a', 'node-b'],
                network: 'fabric',
                interface: 'enp1s0f0np0'
            })
        ).toThrow(/fields/i)
    })

    it('binds a fabric review to exactly one fabric that matches its node count', () => {
        const review = parseDiagnosticMPIReview(rawFabricReview)
        expect(review.network).toBe('fabric')
        expect(review.fabric).toEqual({
            operationId: fabricOperationId,
            qualificationDigest: 'e'.repeat(64),
            recipeId: 'spark-two-node-temporary-addresses-v1'
        })
        expect(review.targets[0]).toEqual({
            nodeId: 'node-a',
            interface: 'enp1s0f0np0',
            address: '10.60.0.1'
        })
        expect(parseDiagnosticMPIReview(rawReview).fabric).toBeUndefined()
        for (const changed of [
            { ...rawFabricReview, fabric: undefined },
            { ...rawReview, fabric: rawFabricReview.fabric }
        ])
            expect(() => parseDiagnosticMPIReview(changed)).toThrow(/fabric binding/i)
        expect(() =>
            parseDiagnosticMPIReview({
                ...rawFabricReview,
                fabric: { ...rawFabricReview.fabric, recipeId: 'spark-three-node-ring-routed-v2' }
            })
        ).toThrow(/node count/i)
        expect(() =>
            parseDiagnosticMPIReview({
                ...rawFabricReview,
                fabric: { ...rawFabricReview.fabric, qualificationDigest: 'not-a-digest' }
            })
        ).toThrow(/qualification/i)
        expect(() =>
            parseDiagnosticMPIReview({
                ...rawFabricReview,
                fabric: { ...rawFabricReview.fabric, gateway: '10.60.0.2' }
            })
        ).toThrow(/fields/i)
    })

    it('accepts only the fixed Socket review and exact operation binding', () => {
        const review = parseDiagnosticMPIReview(rawReview)
        const operation = parseDiagnosticMPIOperation(rawOperation)
        expect(review.network).toBe('management')
        expect(review.transport).toBe('socket')
        expect(operation.samples).toEqual([])
        expect(operation.message).toBe('PAIR is running the bounded Socket NCCL correctness smoke.')
        expect(
            bindDiagnosticMPIOperation(operation, {
                operationId,
                groupId: `pair-smoke-${operationId}`,
                ownerNodeId: 'node-a',
                memberNodeIds: ['node-a', 'node-b'],
                recipeId: 'pair-two-spark-nccl-socket-correctness-v2'
            })
        ).toBe(operation)
        expect(() => parseDiagnosticMPIReview({ ...rawReview, transport: 'rdma' })).toThrow(
            /Socket transport/i
        )
        expect(() => parseDiagnosticMPIReview({ ...rawReview, network: 'rdma' })).toThrow(
            /network/i
        )
        expect(() =>
            parseDiagnosticMPIOperation({ ...rawOperation, stderr: '/private/path' })
        ).toThrow(/fields/i)
        for (const message of [
            'failed(/home/operator/private.log)',
            'open(../private/key)',
            'Authorization: Bearer abc123',
            'access_token: abc123',
            'client_secret: abc123',
            'credentials: abc123',
            'Everything is peachy according to the backend.'
        ]) {
            const projected = parseDiagnosticMPIOperation({ ...rawOperation, message })
            expect(projected.message).toBe(
                'PAIR is running the bounded Socket NCCL correctness smoke.'
            )
            expect(projected.message).not.toContain(message)
        }
    })

    it('binds recovered references to the same build, owner, recipe and roster', () => {
        const result = parseDiagnosticMPIRecovery({
            reference: {
                reviewId,
                buildOperationId: buildId,
                operationId,
                groupId: `pair-smoke-${operationId}`,
                ownerNodeId: 'node-a',
                memberNodeIds: ['node-a', 'node-b'],
                recipeId: 'pair-two-spark-nccl-socket-correctness-v2'
            },
            operation: rawOperation,
            recoveryRequired: false
        })
        expect(result.operation?.operationId).toBe(operationId)
        expect(() =>
            parseDiagnosticMPIRecovery({
                reference: {
                    ...result.reference!,
                    recipeId: 'pair-two-spark-nccl-socket-smoke-v1'
                },
                operation: rawOperation,
                recoveryRequired: false
            })
        ).toThrow(/reference/i)
        expect(() =>
            parseDiagnosticMPIRecovery({
                reference: null,
                operation: rawOperation,
                recoveryRequired: false
            })
        ).toThrow(/lacks/i)
    })
})
