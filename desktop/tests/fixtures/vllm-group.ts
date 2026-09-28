// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { VllmGroupCheck, VllmGroupReview } from '@/shared/types/vllm-group'
import type {
    VllmGroupRunState,
    VllmGroupRunStatus,
    VllmGroupStatus
} from '@/shared/types/vllm-group-status'
import { VLLM_QWEN38_MODEL, VLLM_QWEN38_RUNTIME } from '@/shared/constants/vllm'

export const HELD_RUN_ID = 'facfa61ad839f1108ea0972f3f389b3c'
export const GIB = 1024 ** 3

/** Wire round trip: what the bridge sees after JSON transport. */
export const wire = (value: unknown): unknown => JSON.parse(JSON.stringify(value))

/** Owner-published review with every owner field present. */
export function groupReview(
    nodes: 2 | 3 = 2,
    parallelism?: 'tensor' | 'pipeline'
): VllmGroupReview {
    const pipeline = parallelism === 'pipeline' || (parallelism === undefined && nodes === 3)
    return {
        reviewId: 'a'.repeat(32),
        planDigest: 'b'.repeat(64),
        expiresAt: Date.now() + 60_000,
        activationEnabled: true,
        reason: '',
        plan: {
            coordinator: 'node-a',
            model: 'Qwen/fixture@' + 'c'.repeat(40),
            runtime: '0.28.0',
            limits: { runtimeSeconds: 600, memoryMaxBytes: 16 * GIB, tasksMax: 512 },
            topology: {
                tensorParallel: pipeline ? 1 : nodes,
                pipelineParallel: pipeline ? nodes : 1,
                dataParallel: 1,
                configSha256: 'd'.repeat(64)
            },
            members: ['node-a', 'node-b', 'node-c'].slice(0, nodes).map((nodeId, index) => ({
                nodeId,
                pinSha256: String(index + 1).repeat(64),
                gpuUuid: `GPU-0000000${index + 1}-0000-0000-0000-000000000001`,
                modelDigest: 'e'.repeat(64),
                runtimeDigest: String(index + 5).repeat(64),
                runtimeCompatibilitySha256: 'f'.repeat(64),
                resources: { gpu_memory_utilization: 0.05, max_model_len: 2048 },
                placement: {
                    modelPath: '/home/fixture/model',
                    address: `192.168.50.${index + 1}`,
                    apiPort: 28000,
                    masterPort: 29501
                }
            }))
        }
    }
}

/** Owner-published two-node default review that bound a direct fabric lane, so it is TP2. */
export function directSocketReview(): VllmGroupReview {
    const review = groupReview(2)
    review.plan.directSocket = {
        mode: 'qualified-direct-socket',
        operationId: '8'.repeat(32),
        qualificationSha256: '9'.repeat(64),
        lanes: [
            {
                nodeId: 'node-a',
                interfaceName: 'enp1s0f0np0',
                interfaceIndex: 3,
                mac: '02:00:00:0a:00:00',
                localAddress: '172.31.240.1',
                peerAddress: '172.31.240.2'
            },
            {
                nodeId: 'node-b',
                interfaceName: 'enp1s0f1np1',
                interfaceIndex: 4,
                mac: '02:00:00:0b:00:01',
                localAddress: '172.31.240.2',
                peerAddress: '172.31.240.1'
            }
        ]
    }
    return review
}

/** Owner-published fixed Qwen3.8 review: two Sparks, TP2+EP2, over the direct fabric. */
export function qwenGroupReview(): VllmGroupReview {
    const review = groupReview(2)
    const lane = (
        peerNodeId: string,
        localAddress: string,
        peerAddress: string,
        ordinal: number
    ) => ({
        peerNodeId,
        localAddress,
        peerAddress,
        interfaceName: `enp${ordinal}`,
        interfaceIndex: ordinal + 1,
        mac: `02:00:00:00:00:0${ordinal + 1}`,
        switchId: 'switch-1',
        portName: `p${ordinal}`,
        rdmaDevice: `mlx5_${ordinal}`,
        gidPort: 1,
        gidIndex: 3,
        gidType: 'RoCE v2' as const
    })
    review.plan.model = VLLM_QWEN38_MODEL
    review.plan.runtime = VLLM_QWEN38_RUNTIME
    review.plan.limits = { runtimeSeconds: 3600, memoryMaxBytes: 112 * GIB, tasksMax: 512 }
    review.plan.topology = {
        tensorParallel: 2,
        pipelineParallel: 1,
        dataParallel: 1,
        expertParallel: 2,
        contextLength: 32768,
        maxSequences: 2,
        kvCacheMemoryBytes: 8 * GIB,
        mtp: false,
        dflash: false,
        flashinferAutotune: false,
        configSha256: 'd'.repeat(64)
    }
    review.plan.transport = {
        mode: 'host-buffer-roce',
        operationId: '9'.repeat(32),
        qualificationSha256: 'a'.repeat(64),
        netGdrLevel: 0,
        netGdrC2c: 0,
        netGdrRead: 0,
        netPlugin: 'none',
        envPlugin: 'none',
        ginPlugin: 'none',
        subnetAwareRouting: false,
        subnetPrefixLength: 0,
        mergeNICs: true,
        socketPayloadFallback: false
    }
    review.plan.members.forEach((member, index) => {
        member.resources = { gpu_memory_utilization: 0.05, max_model_len: 32768 }
        const peer = review.plan.members[1 - index].nodeId
        member.fabric = {
            lanes: [
                lane(peer, `10.253.${index}.0`, `10.253.${index}.1`, 0),
                lane(peer, `10.253.${index}.2`, `10.253.${index}.3`, 1)
            ]
        }
    })
    return review
}

/** Owner-published run derived from a review. */
export function groupRun(
    state: VllmGroupRunState = 'starting',
    review: VllmGroupReview = groupReview()
): VllmGroupRunStatus {
    const started = state !== 'starting'
    const clean = state === 'stopped' || state === 'failed'
    return {
        runId: '1'.repeat(32),
        generation: 1,
        planDigest: review.planDigest,
        plan: review.plan,
        state,
        ranks: review.plan.members.map(member => ({
            nodeId: member.nodeId,
            attempted: true,
            started,
            cleanupConfirmed: clean
        })),
        cleanupConfirmed: clean
    }
}

/** The retained generation-13 journal as the read-only status projection reports it: no owner fields. */
export function heldRun(): VllmGroupRunStatus {
    const digest = 'a'.repeat(64)
    return {
        runId: HELD_RUN_ID,
        generation: 13,
        planDigest: digest,
        plan: {
            coordinator: 'node-a',
            model: 'example/model',
            runtime: '0.28.0',
            // The Go writer always serializes limits; a legacy journal reports zeros.
            limits: { runtimeSeconds: 0, memoryMaxBytes: 0, tasksMax: 0 },
            topology: {
                tensorParallel: 3,
                pipelineParallel: 1,
                dataParallel: 1,
                configSha256: digest
            },
            members: ['node-a', 'node-b', 'node-c'].map((nodeId, index) => ({
                nodeId,
                pinSha256: String(index + 1).repeat(64),
                gpuUuid: `GPU-${nodeId}`,
                modelDigest: digest,
                runtimeDigest: digest,
                runtimeCompatibilitySha256: digest
            }))
        },
        state: 'cleanup-required',
        ranks: ['node-a', 'node-b', 'node-c'].map(nodeId => ({
            nodeId,
            attempted: true,
            started: false,
            cleanupConfirmed: false
        })),
        cleanupConfirmed: false,
        failure: 'all owned ranks require cleanup'
    }
}

export function heldStatus(): VllmGroupStatus {
    return {
        activationEnabled: false,
        reserved: true,
        reason: 'retained serving group holds vLLM until every attempted rank confirms cleanup; fresh native admission required',
        run: heldRun()
    }
}

export function inactiveStatus(): VllmGroupStatus {
    return { activationEnabled: false, reserved: false, reason: '' }
}

export function groupCheck(review: VllmGroupReview, activationEnabled = true): VllmGroupCheck {
    return {
        reviewId: review.reviewId,
        activationEnabled,
        participants: review.plan.members.map(member => ({
            nodeId: member.nodeId,
            state: activationEnabled ? 'available' : 'unavailable',
            activationEnabled,
            reason: activationEnabled ? '' : 'native owner is not admitted'
        }))
    }
}
