// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { beforeEach, describe, expect, it, vi } from 'vitest'

const MODEL = 'nvidia/Qwen3.8-Flash-Next-NVFP4@fc694b54fb0174e0913e6adf86691ef85a4ead47'
const OPERATION = 'a'.repeat(32)
const DIGEST = 'b'.repeat(64)
const receipt = {
    id: MODEL,
    source: 'huggingface',
    revision: MODEL.slice(-40),
    license: 'nvidia-open-model-license',
    metadataSha256: DIGEST,
    digest: 'c'.repeat(64),
    bytes: 132_734_505_212,
    files: [{ path: '/must/not/reach/renderer' }]
}

const mocks = vi.hoisted(() => ({
    supervisor: {
        ready: true,
        callProcess: vi.fn(),
        refreshEngineModels: vi.fn(),
        refreshRemoteEngineStatus: vi.fn()
    },
    state: {
        getSelfId: vi.fn(() => 'node-a'),
        isEngineCommandAllowed: vi.fn(() => true),
        getEngineInitialState: vi.fn(),
        beginModelPull: vi.fn(),
        finishModelPull: vi.fn(),
        beginRemoteModelPull: vi.fn(),
        finishRemoteModelPull: vi.fn(),
        beginVllmJourney: vi.fn(),
        finishVllmJourney: vi.fn()
    }
}))

vi.mock('@/electron/service-bridge/modular-supervisor', () => ({
    getModularSupervisor: () => mocks.supervisor
}))
vi.mock('@/electron/service-bridge/modular-state', () => ({
    getModularBridgeState: () => mocks.state
}))

import { vllmModelJourneyHandlers } from '@/electron/service-bridge/vllm-model-journey-handlers'

describe('vLLM model journey bridge', () => {
    beforeEach(() => {
        vi.clearAllMocks()
        mocks.supervisor.ready = true
        mocks.state.getSelfId.mockReturnValue('node-a')
        mocks.state.isEngineCommandAllowed.mockReturnValue(true)
        mocks.state.getEngineInitialState.mockReturnValue({
            statuses: ['node-a', 'node-b', 'node-c'].map(nodeId => ({
                nodeId,
                engineType: 'vllm',
                processStatus: 'stopped',
                managed: true
            })),
            models: [
                {
                    nodeId: 'node-a',
                    engineType: 'vllm',
                    models: [{ name: MODEL, downloaded: true }]
                },
                { nodeId: 'node-b', engineType: 'vllm', models: [] },
                { nodeId: 'node-c', engineType: 'vllm', models: [] }
            ]
        })
    })

    it('pulls exact locally with no renderer credential/path and returns a bounded receipt', async () => {
        mocks.supervisor.callProcess.mockResolvedValueOnce(receipt)
        const result = await vllmModelJourneyHandlers['engine:vllm-pull-exact']({
            nodeId: 'node-a',
            model: MODEL,
            operationId: OPERATION
        })
        expect(mocks.supervisor.callProcess).toHaveBeenCalledExactlyOnceWith(
            'broker',
            'engine:action',
            {
                engine: 'vllm',
                action: 'pull_model',
                params: { model: MODEL, operationId: OPERATION }
            },
            5 * 60 * 60_000 + 59 * 60_000
        )
        expect(result.license).toBe('nvidia-open-model-license')
        expect(JSON.stringify(result)).not.toContain('/must/not/reach')
        expect(mocks.state.beginModelPull).toHaveBeenCalledWith('vllm', MODEL)
        expect(mocks.state.finishModelPull).toHaveBeenCalledWith('vllm', MODEL)
    })

    it('binds remote pull and cancel to the same exact operation', async () => {
        mocks.supervisor.callProcess
            .mockResolvedValueOnce({ opId: OPERATION, result: receipt })
            .mockResolvedValueOnce({ accepted: true })
        await vllmModelJourneyHandlers['engine:vllm-pull-exact']({
            nodeId: 'node-b',
            model: MODEL,
            operationId: OPERATION
        })
        expect(mocks.supervisor.callProcess).toHaveBeenNthCalledWith(
            1,
            'broker',
            'engine:remote-pull-model',
            {
                node: 'node-b',
                engine: 'vllm',
                model: MODEL,
                operationId: OPERATION,
                params: { model: MODEL, operationId: OPERATION }
            },
            5 * 60 * 60_000 + 59 * 60_000
        )
        const canceled = await vllmModelJourneyHandlers['engine:vllm-cancel-pull']({
            nodeId: 'node-b',
            model: MODEL,
            operationId: OPERATION
        })
        expect(canceled).toEqual({ accepted: true, operationId: OPERATION })
    })

    it('prepares remotely and strips provider paths from the renderer result', async () => {
        mocks.supervisor.callProcess.mockResolvedValueOnce({
            operationId: OPERATION,
            state: 'blocked-provider',
            recipeId: 'qwen-recipe',
            recipeSha256: DIGEST,
            runtimeVersion: '0.28.1',
            artifactCount: 196,
            artifactBytes: 3_930_666_769,
            provider: {
                qualified: false,
                profileId: 'spark-bc',
                expectedClosureSha256: DIGEST,
                observedClosureSha256: DIGEST,
                allowedClosureSha256: [DIGEST],
                mismatches: [{ name: 'x', reason: 'none', observedPath: '/private' }]
            }
        })
        const result = await vllmModelJourneyHandlers['engine:vllm-prepare-runtime']({
            nodeId: 'node-b',
            operationId: OPERATION
        })
        expect(mocks.supervisor.callProcess).toHaveBeenCalledWith(
            'broker',
            'engine:remote-vllm-qwen38-prepare',
            { node: 'node-b', engine: 'vllm', operationId: OPERATION },
            6 * 60 * 60_000 + 60_000
        )
        expect(result.provider.observedClosureSha256).toBe(DIGEST)
        expect(JSON.stringify(result)).not.toContain('/private')
    })

    it('distributes from the exact reported source directly to a remote target', async () => {
        mocks.supervisor.callProcess.mockResolvedValueOnce({
            opId: OPERATION,
            operationId: OPERATION,
            result: receipt
        })
        const result = await vllmModelJourneyHandlers['engine:vllm-distribute-model']({
            nodeId: 'node-b',
            sourceNodeId: 'node-a',
            model: MODEL,
            operationId: OPERATION
        })
        expect(mocks.supervisor.callProcess).toHaveBeenCalledWith(
            'broker',
            'engine:remote-distribute-model',
            {
                node: 'node-b',
                engine: 'vllm',
                operationId: OPERATION,
                sourceNode: 'node-a',
                model: MODEL
            },
            5 * 60 * 60_000 + 59 * 60_000
        )
        expect(result.digest).toBe('c'.repeat(64))
        expect(mocks.state.beginVllmJourney).toHaveBeenCalledWith(
            'node-b',
            'distribute',
            OPERATION,
            MODEL
        )
        expect(mocks.state.finishVllmJourney).toHaveBeenCalledWith('node-b', 'distribute')
    })

    it('refuses paths, credentials, and held targets before dispatch', async () => {
        await expect(
            vllmModelJourneyHandlers['engine:vllm-pull-exact']({
                nodeId: 'node-a',
                model: MODEL,
                operationId: OPERATION,
                token: 'forbidden'
            } as never)
        ).rejects.toThrow()
        mocks.state.isEngineCommandAllowed.mockReturnValueOnce(false)
        await expect(
            vllmModelJourneyHandlers['engine:vllm-pull-exact']({
                nodeId: 'node-a',
                model: MODEL,
                operationId: OPERATION
            })
        ).rejects.toThrow(/held/)
        expect(mocks.supervisor.callProcess).not.toHaveBeenCalled()
    })

    it('does not forward backend host paths into renderer errors', async () => {
        const cases = [
            {
                raw: '-32000: open /opt/nvidia/pair/private-stage: permission denied',
                safe: 'open [host path] permission denied'
            },
            {
                raw: '-32000: open "C:\\Users\\John Doe\\PAIR\\private stage": denied',
                safe: 'open [host path]: denied'
            },
            {
                raw: '-32000: open "C:/Users/John Doe/PAIR/private stage": denied',
                safe: 'open [host path]: denied'
            },
            {
                raw: '-32000: open C:/Users/John/PAIR/private-stage: denied',
                safe: 'open [host path] denied'
            },
            {
                raw: "-32000: open '\\\\server\\Pair Share\\private stage': denied",
                safe: 'open [host path]: denied'
            },
            {
                raw: "-32000: open '/home/John Doe/.cache/private stage': denied",
                safe: 'open [host path]: denied'
            },
            {
                raw: '-32000: password="hunter 2" token=abc Bearer abc.def',
                safe: 'password=[redacted] token=[redacted] Bearer [redacted]'
            },
            {
                raw: '-32000: {"password":"hunter 2","privateKey":"abc","path":"C:\\Users\\John Doe\\secret"}',
                safe: '{"password"=[redacted],"privateKey"=[redacted],"path":[host path]}'
            }
        ]
        for (const test of cases) {
            mocks.supervisor.callProcess.mockRejectedValueOnce(new Error(test.raw))
            await expect(
                vllmModelJourneyHandlers['engine:vllm-pull-exact']({
                    nodeId: 'node-a',
                    model: MODEL,
                    operationId: OPERATION
                })
            ).rejects.toThrow(test.safe)
        }
    })
})
