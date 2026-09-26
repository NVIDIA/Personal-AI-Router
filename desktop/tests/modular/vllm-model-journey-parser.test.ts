// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import {
    exactVllmModel,
    parseVllmDistributionRequest,
    parseVllmModelReceipt,
    parseVllmRuntimePrepareResult,
    vllmDistributionOperationId,
    vllmPrepareOperationId,
    vllmPullOperationId
} from '@/shared/utils/vllm-model-journey'

const MODEL = 'nvidia/Qwen3.8-Flash-Next-NVFP4@fc694b54fb0174e0913e6adf86691ef85a4ead47'
const OPERATION = 'a'.repeat(32)
const DIGEST = 'b'.repeat(64)

describe('vLLM model journey wire boundaries', () => {
    it('requires immutable public model and exact node/operation identities', () => {
        expect(exactVllmModel(MODEL)).toBe(MODEL)
        for (const value of [
            'nvidia/model',
            'nvidia/model@main',
            'https://huggingface.co/nvidia/model',
            `nvidia/model@${'A'.repeat(40)}`
        ])
            expect(() => exactVllmModel(value)).toThrow(/exact public/)

        expect(
            parseVllmDistributionRequest({
                nodeId: 'node-b',
                sourceNodeId: 'node-a',
                model: MODEL,
                operationId: OPERATION
            })
        ).toMatchObject({ nodeId: 'node-b', sourceNodeId: 'node-a' })
        expect(() =>
            parseVllmDistributionRequest({
                nodeId: 'node-b',
                sourceNodeId: 'node-a',
                model: MODEL,
                operationId: OPERATION,
                password: 'forbidden'
            })
        ).toThrow()
    })

    it('returns a renderer-safe receipt without file paths', () => {
        const receipt = parseVllmModelReceipt({
            id: MODEL,
            source: 'huggingface',
            revision: MODEL.slice(-40),
            license: 'nvidia-open-model-license',
            metadataSha256: DIGEST,
            digest: 'c'.repeat(64),
            bytes: 132_734_505_212,
            files: [{ path: '/secret/model.safetensors', sha256: DIGEST }]
        })
        expect(receipt).toEqual({
            id: MODEL,
            source: 'huggingface',
            revision: MODEL.slice(-40),
            license: 'nvidia-open-model-license',
            metadataSha256: DIGEST,
            digest: 'c'.repeat(64),
            bytes: 132_734_505_212
        })
        expect('files' in receipt).toBe(false)
    })

    it('strips provider paths/packages while retaining compatibility evidence', () => {
        const result = parseVllmRuntimePrepareResult(
            {
                operationId: OPERATION,
                state: 'blocked-provider',
                recipeId: 'recipe',
                recipeSha256: DIGEST,
                runtimeVersion: '0.28.1',
                artifactCount: 196,
                artifactBytes: 3_930_666_769,
                provider: {
                    qualified: false,
                    profileId: 'spark-a',
                    expectedClosureSha256: DIGEST,
                    observedClosureSha256: 'c'.repeat(64),
                    allowedClosureSha256: [DIGEST, 'd'.repeat(64)],
                    mismatches: [
                        {
                            name: 'libcuda.so.1',
                            reason: 'provider file identity differs',
                            expectedPath: '/private/expected',
                            observedPath: '/private/observed',
                            expectedSha256: DIGEST,
                            observedSha256: 'c'.repeat(64),
                            expectedPackage: 'secret-package'
                        }
                    ]
                }
            },
            'node-a',
            OPERATION
        )
        expect(result.provider.mismatches[0]).toEqual({
            name: 'libcuda.so.1',
            reason: 'provider file identity differs',
            expectedSha256: DIGEST,
            observedSha256: 'c'.repeat(64)
        })
        expect(JSON.stringify(result)).not.toContain('/private/')
        expect(JSON.stringify(result)).not.toContain('secret-package')
    })

    it('accepts complete empty mismatch arrays from a qualified provider', () => {
        const value = {
            operationId: OPERATION,
            state: 'ready',
            recipeId: 'recipe',
            recipeSha256: DIGEST,
            runtimeVersion: '0.28.1',
            artifactCount: 196,
            artifactBytes: 3_930_666_769,
            provider: {
                qualified: true,
                expectedClosureSha256: DIGEST,
                observedClosureSha256: DIGEST,
                allowedClosureSha256: [DIGEST],
                mismatches: []
            }
        }
        expect(
            parseVllmRuntimePrepareResult(value, 'node-a', OPERATION).provider.mismatches
        ).toEqual([])
        expect(() =>
            parseVllmRuntimePrepareResult(
                {
                    ...value,
                    provider: {
                        ...value.provider,
                        mismatches: null
                    }
                },
                'node-a',
                OPERATION
            )
        ).toThrow(/incomplete provider observation/)
    })

    it('derives a stable receive identity from the exact source, target, and model', async () => {
        const one = await vllmDistributionOperationId('node-a', 'node-b', MODEL)
        const retry = await vllmDistributionOperationId('node-a', 'node-b', MODEL)
        const other = await vllmDistributionOperationId('node-a', 'node-c', MODEL)
        expect(one).toMatch(/^[0-9a-f]{32}$/)
        expect(retry).toBe(one)
        expect(other).not.toBe(one)
        expect(await vllmPullOperationId('node-a', MODEL)).toBe(
            await vllmPullOperationId('node-a', MODEL)
        )
        expect(await vllmPrepareOperationId('node-a')).not.toBe(
            await vllmPrepareOperationId('node-b')
        )
    })
})
