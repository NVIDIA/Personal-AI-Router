// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

export const buildId = 'a'.repeat(32)
export const reviewId = 'b'.repeat(32)
export const operationId = 'c'.repeat(32)
export const pin = 'd'.repeat(64)
export const sshHostKeyFingerprint = `SHA256:${'A'.repeat(43)}`

export const rawInventory = {
    records: [
        {
            schemaVersion: 1,
            owner: 'pair-managed-nccl-registry-v1',
            operationId: buildId,
            reviewId: 'e'.repeat(32),
            groupId: 'pair-recipe/nccl-fixture',
            approvedRevision: 4,
            adoptedAt: 1_790_000_000_000,
            adopted: true,
            runtimeValidated: false,
            runAvailable: false,
            targets: ['node-a', 'node-b'].map((nodeId, index) => ({
                nodeId,
                principal: nodeId,
                local: index === 0,
                address: `192.0.2.${index + 1}`,
                clusterPinSha256: pin,
                ...(index
                    ? {
                          candidateId: `candidate-${index}`,
                          sshHostKeySha256: sshHostKeyFingerprint
                      }
                    : {}),
                planDigest: pin,
                attempt: 1,
                artifactObservedAt: '2026-09-22T00:00:00Z',
                registration: {
                    identity: { user: 'fixture', password: 'must-not-cross-the-bridge' },
                    sources: { privateKey: 'must-not-cross-the-bridge' }
                }
            }))
        }
    ],
    recoveryRequired: false
}

export const rawReview = {
    reviewId,
    buildOperationId: buildId,
    operationId,
    groupId: `pair-smoke-${operationId}`,
    ownerNodeId: 'node-a',
    network: 'management',
    transport: 'socket',
    recipeId: 'pair-two-spark-nccl-socket-correctness-v2',
    expiresAt: 1_790_000_120_000,
    targets: [
        {
            nodeId: 'node-a',
            interface: 'enp1s0',
            address: '192.0.2.1',
            sshAddress: '192.0.2.1'
        },
        {
            nodeId: 'node-b',
            interface: 'enp1s0',
            address: '192.0.2.2',
            sshAddress: '192.0.2.2'
        }
    ]
} as const

export const fabricOperationId = 'f'.repeat(32)

export const rawFabricReview = {
    ...rawReview,
    network: 'fabric',
    fabric: {
        operationId: fabricOperationId,
        qualificationDigest: 'e'.repeat(64),
        recipeId: 'spark-two-node-temporary-addresses-v1'
    },
    targets: [
        {
            nodeId: 'node-a',
            interface: 'enp1s0f0np0',
            address: '10.60.0.1',
            sshAddress: '192.0.2.1'
        },
        {
            nodeId: 'node-b',
            interface: 'enp1s0f0np0',
            address: '10.60.0.2',
            sshAddress: '192.0.2.2'
        }
    ]
}

export const rawOperation = {
    operationId,
    groupId: `pair-smoke-${operationId}`,
    ownerNodeId: 'node-a',
    preset: 'nccl-smoke',
    recipeId: 'pair-two-spark-nccl-socket-correctness-v2',
    state: 'running',
    startedAt: 1_790_000_000_000,
    message: 'Running the bounded socket NCCL test on the selected participant interfaces',
    cleanupConfirmed: false,
    memberNodeIds: ['node-a', 'node-b']
} as const
