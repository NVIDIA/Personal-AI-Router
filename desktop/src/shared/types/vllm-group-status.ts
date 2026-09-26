// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { VllmGroupStartFailure } from '@/shared/types/vllm-group'

export const VLLM_GROUP_RUN_STATES = [
    'starting',
    'ready',
    'stopping',
    'stopped',
    'failed',
    'cleanup-required'
] as const
export type VllmGroupRunState = (typeof VLLM_GROUP_RUN_STATES)[number]

/** Rank-local route states also include preparation before the group run is ready. */
export const VLLM_SERVING_GROUP_ROUTE_STATES = [
    'prepared',
    'started',
    ...VLLM_GROUP_RUN_STATES
] as const
export type VllmServingGroupRouteState = (typeof VLLM_SERVING_GROUP_ROUTE_STATES)[number]

/** Exact execution identity projected with each Engine Manager vLLM status. */
export interface VllmServingGroupRoute {
    runId: string
    generation: number
    model: string
    coordinator: string
    role: 'coordinator' | 'participant'
    state: VllmServingGroupRouteState
    members: string[]
    /** Collectively ready coordinator awaiting the local proxy route. */
    routing?: true
}

/** Configured for the owned start; null leaves the vendor default in effect. */
export interface VllmGroupResourceSettings {
    gpu_memory_utilization: number | null
    max_model_len: number | null
    gpu_uuid?: string | null
    tensor_parallel_size?: number | null
}

export interface VllmGroupPlacement {
    modelPath: string
    address: string
    apiPort: number
    masterPort: number
}

export interface VllmGroupFabricLane {
    peerNodeId: string
    localAddress: string
    peerAddress: string
    interfaceName: string
    interfaceIndex: number
    mac: string
    switchId: string
    portName: string
    rdmaDevice: string
    gidPort: number
    gidIndex: number
    gidType: 'RoCE v2'
}

export interface VllmGroupMemberFabric {
    lanes: VllmGroupFabricLane[]
}

/**
 * Member identity as PAIR binds it. Owner-side placement and resource fields
 * are present on reviews and owner-published runs; the read-only retained
 * status projection may omit them, so they are optional here and strict when
 * present.
 */
export interface VllmGroupMemberStatus {
    nodeId: string
    pinSha256: string
    gpuUuid: string
    modelDigest: string
    runtimeDigest: string
    runtimeCompatibilitySha256: string
    resources?: VllmGroupResourceSettings
    placement?: VllmGroupPlacement
    fabric?: VllmGroupMemberFabric
}

export interface VllmGroupRankStatus {
    nodeId: string
    attempted: boolean
    started: boolean
    cleanupConfirmed: boolean
    startFailure?: VllmGroupStartFailure
    cleanupFailure?: VllmGroupStartFailure
}

export interface VllmGroupTopology {
    tensorParallel: number
    pipelineParallel: number
    dataParallel: number
    expertParallel?: number
    eplb?: boolean
    redundantExperts?: number
    contextLength?: number
    maxSequences?: number
    kvCacheMemoryBytes?: number
    mtp?: boolean
    dflash?: boolean
    flashinferAutotune?: boolean
    configSha256: string
}

export interface VllmGroupLimits {
    runtimeSeconds: number
    memoryMaxBytes: number
    tasksMax: number
}

export interface VllmGroupTransport {
    mode: 'host-buffer-roce'
    operationId: string
    qualificationSha256: string
    netGdrLevel: 0
    netGdrC2c: 0
    netGdrRead: 0
    netPlugin: 'none'
    envPlugin: 'none'
    ginPlugin: 'none'
    /** Absent only on a terminal, cleanup-confirmed historical status readback. */
    subnetAwareRouting?: false
    subnetPrefixLength?: 0
    mergeNICs?: true
    socketPayloadFallback: false
}

export interface VllmGroupDirectSocketLane {
    nodeId: string
    interfaceName: string
    interfaceIndex: number
    mac: string
    localAddress: string
    peerAddress: string
}

/** Ordinary two-node TP2 only: NCCL Socket on one reciprocal direct fabric lane. */
export interface VllmGroupDirectSocket {
    mode: 'qualified-direct-socket'
    operationId: string
    qualificationSha256: string
    lanes: VllmGroupDirectSocketLane[]
}

export interface VllmGroupPlan {
    coordinator: string
    model: string
    runtime: string
    topology: VllmGroupTopology
    members: VllmGroupMemberStatus[]
    limits?: VllmGroupLimits
    transport?: VllmGroupTransport
    directSocket?: VllmGroupDirectSocket
}

export interface VllmGroupRunStatus {
    runId: string
    generation: number
    planDigest: string
    plan: VllmGroupPlan
    state: VllmGroupRunState
    ranks: VllmGroupRankStatus[]
    cleanupConfirmed: boolean
    failure?: string
}

export interface VllmGroupStatus {
    /** True only while PAIR reports an admitted native owner. Never inferred by the Desktop. */
    activationEnabled: boolean
    reserved: boolean
    reason: string
    run?: VllmGroupRunStatus
}
