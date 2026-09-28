// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type {
    VllmGroupDirectSocket,
    VllmGroupDirectSocketLane,
    VllmGroupFabricLane,
    VllmGroupLimits,
    VllmGroupMemberFabric,
    VllmGroupMemberStatus,
    VllmGroupPlacement,
    VllmGroupPlan,
    VllmGroupRankStatus,
    VllmGroupResourceSettings,
    VllmGroupRingSocket,
    VllmGroupRingSocketMember,
    VllmGroupRunState,
    VllmGroupRunStatus,
    VllmGroupStatus,
    VllmGroupTopology,
    VllmGroupTransport,
    VllmServingGroupRoute
} from '@/shared/types/vllm-group-status'
import {
    VLLM_GROUP_RUN_STATES,
    VLLM_SERVING_GROUP_ROUTE_STATES
} from '@/shared/types/vllm-group-status'
import type { VllmGroupStartFailure } from '@/shared/types/vllm-group'
import {
    VLLM_GROUP_READBACK_PROPERTIES,
    VLLM_GROUP_START_FAILURE_CODES,
    VLLM_GROUP_START_FAILURE_STAGES,
    VLLM_GROUP_START_STDERR_CODES
} from '@/shared/types/vllm-group'
import { VLLM_QWEN38_MODEL, VLLM_QWEN38_RUNTIME } from '@/shared/constants/vllm'
import { ipv4Value } from '@/shared/utils/fabric'

const VLLM_FABRIC_SOCKET_RUNTIME = '0.29.0'
const socketInterfacePattern = /^[A-Za-z0-9_.:-]{1,15}$/
const lowerMacPattern = /^[0-9a-f]{2}(:[0-9a-f]{2}){5}$/

const GIB = 1024 ** 3
const ORDINARY_LIMITS: VllmGroupLimits = {
    runtimeSeconds: 600,
    memoryMaxBytes: 16 * GIB,
    tasksMax: 512
}
const QWEN_LIMITS: VllmGroupLimits = {
    runtimeSeconds: 3600,
    memoryMaxBytes: 112 * GIB,
    tasksMax: 512
}
const gpuUuidPattern =
    /^GPU-[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/

export function object(value: unknown, label: string): Record<string, unknown> {
    if (!value || typeof value !== 'object' || Array.isArray(value))
        throw new Error(`PAIR returned an invalid ${label}.`)
    return value as Record<string, unknown>
}

export function text(value: unknown, label: string, max = 4096): string {
    if (typeof value !== 'string' || !value.trim() || value.length > max)
        throw new Error(`PAIR returned an invalid ${label}.`)
    return value
}

export function digest(value: unknown, label: string): string {
    const result = text(value, label, 64)
    if (!/^[0-9a-f]{64}$/.test(result)) throw new Error(`PAIR returned an invalid ${label}.`)
    return result
}

export function operationId(value: unknown, label: string): string {
    const result = text(value, label, 32)
    if (!/^[0-9a-f]{32}$/.test(result)) throw new Error(`PAIR returned an invalid ${label}.`)
    return result
}

export function identity(value: unknown, label: string): string {
    const result = text(value, label, 128)
    if (Array.from(result).some(char => char.charCodeAt(0) <= 32 || char.charCodeAt(0) === 127))
        throw new Error(`PAIR returned an invalid ${label}.`)
    return result
}

export function flag(value: unknown, label: string): boolean {
    if (typeof value !== 'boolean') throw new Error(`PAIR returned an invalid ${label}.`)
    return value
}

/** Non-negative safe integer. */
export function count(value: unknown, label: string): number {
    if (typeof value !== 'number' || !Number.isSafeInteger(value) || value < 0)
        throw new Error(`PAIR returned an invalid ${label}.`)
    return value
}

/** Positive safe integer. */
function positive(value: unknown, label: string): number {
    const result = count(value, label)
    if (result <= 0) throw new Error(`PAIR returned an invalid ${label}.`)
    return result
}

/** Two or three participants, the only group sizes PAIR reviews. */
export function participants(value: unknown, label: string): unknown[] {
    if (!Array.isArray(value) || value.length < 2 || value.length > 3)
        throw new Error(`PAIR returned an invalid ${label}.`)
    return value
}

/** Strictly parse Engine Manager's compact per-node serving-group projection. */
export function parseVllmServingGroupRoute(value: unknown): VllmServingGroupRoute {
    const row = object(value, 'serving-group route')
    const keys = ['runId', 'generation', 'model', 'coordinator', 'role', 'state', 'members']
    const routing = row.routing
    if (
        Object.keys(row).length !== keys.length + (routing === undefined ? 0 : 1) ||
        keys.some(key => !(key in row)) ||
        (routing !== undefined && routing !== true)
    )
        throw new Error('PAIR returned an invalid serving-group route.')

    const generation = count(row.generation, 'serving-group generation')
    if (generation === 0) throw new Error('PAIR returned an invalid serving-group generation.')
    const coordinator = identity(row.coordinator, 'serving-group coordinator')
    const role = row.role
    if (role !== 'coordinator' && role !== 'participant')
        throw new Error('PAIR returned an invalid serving-group role.')
    const state = row.state
    if (
        typeof state !== 'string' ||
        !VLLM_SERVING_GROUP_ROUTE_STATES.some(candidate => candidate === state)
    )
        throw new Error('PAIR returned an invalid serving-group state.')
    const members = participants(row.members, 'serving-group member list').map(member =>
        identity(member, 'serving-group member identity')
    )
    if (new Set(members).size !== members.length || !members.includes(coordinator))
        throw new Error('PAIR returned inconsistent serving-group participants.')
    if (routing === true && (role !== 'coordinator' || state !== 'starting'))
        throw new Error('PAIR returned route readiness outside a starting coordinator.')

    return {
        runId: operationId(row.runId, 'serving-group run identity'),
        generation,
        model: text(row.model, 'serving-group model', 512),
        coordinator,
        role,
        state: state as VllmServingGroupRoute['state'],
        members,
        ...(routing === true ? { routing: true as const } : {})
    }
}

function parseResourceSettings(value: unknown): VllmGroupResourceSettings {
    const row = object(value, 'serving-group resource settings')
    const gpu = row.gpu_memory_utilization
    const length = row.max_model_len
    const uuid = row.gpu_uuid
    const parallel = row.tensor_parallel_size
    if (gpu !== null && (typeof gpu !== 'number' || !Number.isFinite(gpu) || gpu <= 0 || gpu > 1))
        throw new Error('PAIR returned an invalid serving-group memory fraction.')
    if (
        length !== null &&
        (typeof length !== 'number' ||
            !Number.isInteger(length) ||
            length <= 0 ||
            length > 2147483647)
    )
        throw new Error('PAIR returned an invalid serving-group context length.')
    if (
        uuid !== undefined &&
        uuid !== null &&
        (typeof uuid !== 'string' ||
            !uuid.split(',').every(device => gpuUuidPattern.test(device.trim())))
    )
        throw new Error('PAIR returned an invalid serving-group GPU binding.')
    if (
        parallel !== undefined &&
        parallel !== null &&
        (typeof parallel !== 'number' || !Number.isInteger(parallel) || parallel < 1)
    )
        throw new Error('PAIR returned an invalid serving-group local parallelism.')
    const devices =
        typeof uuid === 'string'
            ? uuid.split(',').map(device => 'GPU-' + device.trim().slice(4).toLowerCase())
            : []
    if (
        new Set(devices).size !== devices.length ||
        (devices.length > 0 && devices.length !== (parallel ?? 1)) ||
        (devices.length === 0 && (parallel ?? 1) > 1)
    )
        throw new Error('PAIR returned an inconsistent serving-group GPU binding.')
    return {
        gpu_memory_utilization: gpu as number | null,
        max_model_len: length as number | null,
        ...(uuid === undefined ? {} : { gpu_uuid: uuid === null ? null : devices.join(',') }),
        ...(parallel === undefined ? {} : { tensor_parallel_size: parallel as number | null })
    }
}

function parsePlacement(value: unknown): VllmGroupPlacement {
    const row = object(value, 'serving-group placement')
    return {
        modelPath: text(row.modelPath, 'serving-group model path'),
        address: text(row.address, 'serving-group address', 128),
        apiPort: count(row.apiPort, 'serving-group API port'),
        masterPort: count(row.masterPort, 'serving-group master port')
    }
}

function parseFabric(value: unknown): VllmGroupMemberFabric {
    const row = object(value, 'serving-group fabric')
    if (!Array.isArray(row.lanes) || row.lanes.length !== 2)
        throw new Error('PAIR returned an invalid two-lane RoCE binding.')
    return {
        lanes: row.lanes.map((value): VllmGroupFabricLane => {
            const lane = object(value, 'serving-group RoCE lane')
            if (text(lane.gidType, 'RDMA GID type', 16) !== 'RoCE v2')
                throw new Error('PAIR returned an unsupported RDMA GID type.')
            return {
                peerNodeId: identity(lane.peerNodeId, 'RoCE peer identity'),
                localAddress: text(lane.localAddress, 'RoCE local address', 64),
                peerAddress: text(lane.peerAddress, 'RoCE peer address', 64),
                interfaceName: identity(lane.interfaceName, 'RoCE interface name'),
                interfaceIndex: count(lane.interfaceIndex, 'RoCE interface index'),
                mac: text(lane.mac, 'RoCE MAC address', 32),
                switchId: identity(lane.switchId, 'RoCE switch identity'),
                portName: identity(lane.portName, 'RoCE port name'),
                rdmaDevice: identity(lane.rdmaDevice, 'RDMA device'),
                gidPort: count(lane.gidPort, 'RoCE GID port'),
                gidIndex: count(lane.gidIndex, 'RoCE GID index'),
                gidType: 'RoCE v2'
            }
        })
    }
}

function parseTransport(value: unknown, allowHistoricalRouting: boolean): VllmGroupTransport {
    const row = object(value, 'serving-group transport')
    if (
        row.mode !== 'host-buffer-roce' ||
        count(row.netGdrLevel, 'transport GDR level') !== 0 ||
        count(row.netGdrC2c, 'transport GDR C2C') !== 0 ||
        count(row.netGdrRead, 'transport GDR read') !== 0 ||
        row.netPlugin !== 'none' ||
        row.envPlugin !== 'none' ||
        row.ginPlugin !== 'none' ||
        flag(row.socketPayloadFallback, 'transport socket fallback')
    )
        throw new Error('PAIR returned an unsupported serving-group transport.')
    const base = {
        mode: 'host-buffer-roce',
        operationId: operationId(row.operationId, 'transport operation'),
        qualificationSha256: digest(row.qualificationSha256, 'transport qualification digest'),
        netGdrLevel: 0,
        netGdrC2c: 0,
        netGdrRead: 0,
        netPlugin: 'none',
        envPlugin: 'none',
        ginPlugin: 'none',
        socketPayloadFallback: false
    } as const
    const historical =
        row.subnetAwareRouting === undefined &&
        row.subnetPrefixLength === undefined &&
        row.mergeNICs === undefined
    if (historical) {
        if (!allowHistoricalRouting)
            throw new Error('PAIR returned a serving-group transport without routing policy.')
        return base
    }
    if (
        !flag(row.mergeNICs, 'transport merged NIC state') ||
        flag(row.subnetAwareRouting, 'transport subnet-aware routing') ||
        count(row.subnetPrefixLength, 'transport subnet prefix') !== 0
    )
        throw new Error('PAIR returned an unsupported serving-group transport.')
    return { ...base, subnetAwareRouting: false, subnetPrefixLength: 0, mergeNICs: true }
}

function parseDirectSocket(value: unknown): VllmGroupDirectSocket {
    const row = object(value, 'serving-group direct socket')
    if (
        row.mode !== 'qualified-direct-socket' ||
        !Array.isArray(row.lanes) ||
        row.lanes.length !== 2
    )
        throw new Error('PAIR returned an invalid direct socket binding.')
    return {
        mode: 'qualified-direct-socket',
        operationId: operationId(row.operationId, 'direct socket operation'),
        qualificationSha256: digest(row.qualificationSha256, 'direct socket qualification digest'),
        lanes: row.lanes.map((value): VllmGroupDirectSocketLane => {
            const lane = object(value, 'direct socket lane')
            return {
                nodeId: identity(lane.nodeId, 'direct socket lane node'),
                interfaceName: identity(lane.interfaceName, 'direct socket interface'),
                interfaceIndex: count(lane.interfaceIndex, 'direct socket interface index'),
                mac: text(lane.mac, 'direct socket MAC address', 32),
                localAddress: text(lane.localAddress, 'direct socket local address', 64),
                peerAddress: text(lane.peerAddress, 'direct socket peer address', 64)
            }
        })
    }
}

function ringAddress(value: unknown, label: string): string {
    const result = text(value, label, 15)
    if (ipv4Value(result) === null) throw new Error(`PAIR returned an invalid ${label}.`)
    return result
}

function parseRingSocket(value: unknown): VllmGroupRingSocket {
    const row = object(value, 'serving-group ring socket')
    if (
        Object.keys(row).length !== 4 ||
        row.mode !== 'qualified-ring-socket' ||
        !Array.isArray(row.members) ||
        row.members.length !== 3
    )
        throw new Error('PAIR returned an invalid ring socket binding.')
    return {
        mode: 'qualified-ring-socket',
        operationId: operationId(row.operationId, 'ring socket operation'),
        qualificationSha256: digest(row.qualificationSha256, 'ring socket qualification digest'),
        members: row.members.map((entry): VllmGroupRingSocketMember => {
            const member = object(entry, 'ring socket member')
            const lanes = member.laneAddresses
            const interfaceName = text(member.interfaceName, 'ring socket interface', 15)
            const mac = text(member.mac, 'ring socket MAC address', 17)
            if (
                Object.keys(member).length !== 6 ||
                !Array.isArray(lanes) ||
                lanes.length !== 2 ||
                !socketInterfacePattern.test(interfaceName) ||
                !lowerMacPattern.test(mac)
            )
                throw new Error('PAIR returned an invalid ring socket member.')
            return {
                nodeId: identity(member.nodeId, 'ring socket member node'),
                interfaceName,
                interfaceIndex: positive(member.interfaceIndex, 'ring socket interface index'),
                mac,
                advertisedAddress: ringAddress(
                    member.advertisedAddress,
                    'ring socket advertised address'
                ),
                laneAddresses: [
                    ringAddress(lanes[0], 'ring socket lane address'),
                    ringAddress(lanes[1], 'ring socket lane address')
                ]
            }
        })
    }
}

function optionalCount(value: unknown, label: string): number | undefined {
    return value === undefined ? undefined : count(value, label)
}
function optionalFlag(value: unknown, label: string): boolean | undefined {
    return value === undefined ? undefined : flag(value, label)
}

function parseTopology(value: unknown): VllmGroupTopology {
    const row = object(value, 'serving-group topology')
    const topology: VllmGroupTopology = {
        tensorParallel: positive(row.tensorParallel, 'tensor parallelism'),
        pipelineParallel: positive(row.pipelineParallel, 'pipeline parallelism'),
        dataParallel: positive(row.dataParallel, 'data parallelism'),
        configSha256: digest(row.configSha256, 'topology digest')
    }
    const expertParallel = optionalCount(row.expertParallel, 'expert parallelism')
    const eplb = optionalFlag(row.eplb, 'EPLB flag')
    const redundantExperts = optionalCount(row.redundantExperts, 'redundant experts')
    const contextLength = optionalCount(row.contextLength, 'context length')
    const maxSequences = optionalCount(row.maxSequences, 'max sequences')
    const kvCacheMemoryBytes = optionalCount(row.kvCacheMemoryBytes, 'KV cache bytes')
    const mtp = optionalFlag(row.mtp, 'MTP flag')
    const dflash = optionalFlag(row.dflash, 'DFlash flag')
    const flashinferAutotune = optionalFlag(row.flashinferAutotune, 'FlashInfer autotune flag')
    if (expertParallel !== undefined) topology.expertParallel = expertParallel
    if (eplb !== undefined) topology.eplb = eplb
    if (redundantExperts !== undefined) topology.redundantExperts = redundantExperts
    if (contextLength !== undefined) topology.contextLength = contextLength
    if (maxSequences !== undefined) topology.maxSequences = maxSequences
    if (kvCacheMemoryBytes !== undefined) topology.kvCacheMemoryBytes = kvCacheMemoryBytes
    if (mtp !== undefined) topology.mtp = mtp
    if (dflash !== undefined) topology.dflash = dflash
    if (flashinferAutotune !== undefined) topology.flashinferAutotune = flashinferAutotune
    return topology
}

function parseLimits(value: unknown): VllmGroupLimits {
    const row = object(value, 'serving-group limits')
    return {
        runtimeSeconds: count(row.runtimeSeconds, 'runtime limit'),
        memoryMaxBytes: count(row.memoryMaxBytes, 'memory limit'),
        tasksMax: count(row.tasksMax, 'task limit')
    }
}

function sameLimits(a: VllmGroupLimits, b: VllmGroupLimits): boolean {
    return (
        a.runtimeSeconds === b.runtimeSeconds &&
        a.memoryMaxBytes === b.memoryMaxBytes &&
        a.tasksMax === b.tasksMax
    )
}

interface VllmGroupPlanOptions {
    /**
     * Reviews and owner-published runs carry limits, placement and resources.
     * The read-only retained status projection omits them, so status reads
     * accept their absence while still validating them strictly when present.
     */
    ownerFields: 'required' | 'optional'
    /** Cleanup-only compatibility for journals retained before routing fields existed. */
    historicalTransport?: boolean
}

/**
 * Strict plan parser. Exact ordinary (TP-only or PP-only, DP1) and fixed
 * Qwen3.8 RoCE profiles are the only reviewed topologies PAIR produces; the
 * coordinator is always the first member and members are distinct by identity
 * and pin.
 */
export function parseVllmGroupPlan(value: unknown, options: VllmGroupPlanOptions): VllmGroupPlan {
    const row = object(value, 'serving-group plan')
    const required = options.ownerFields === 'required'
    const model = text(row.model, 'serving-group model', 512)
    const runtime = text(row.runtime, 'serving-group runtime', 64)
    const coordinator = identity(row.coordinator, 'serving-group coordinator')
    const topology = parseTopology(row.topology)
    const members = participants(row.members, 'serving-group member list').map(
        (value): VllmGroupMemberStatus => {
            const member = object(value, 'serving-group member')
            const result: VllmGroupMemberStatus = {
                nodeId: identity(member.nodeId, 'serving-group member identity'),
                pinSha256: digest(member.pinSha256, 'serving-group member pin'),
                gpuUuid: identity(member.gpuUuid, 'serving-group GPU identity'),
                modelDigest: digest(member.modelDigest, 'serving-group model digest'),
                runtimeDigest: digest(member.runtimeDigest, 'serving-group runtime digest'),
                runtimeCompatibilitySha256: digest(
                    member.runtimeCompatibilitySha256,
                    'serving-group runtime compatibility'
                )
            }
            if (required || member.resources !== undefined)
                result.resources = parseResourceSettings(member.resources)
            if (required || member.placement !== undefined)
                result.placement = parsePlacement(member.placement)
            if (member.fabric !== undefined) result.fabric = parseFabric(member.fabric)
            return result
        }
    )
    // The Go writer serializes limits unconditionally, so a legacy journal that
    // never recorded them arrives as an all-zero block on the status projection.
    // That is "absent" for a status read; a review or owner-published run must
    // carry real limits.
    let limits = required || row.limits !== undefined ? parseLimits(row.limits) : undefined
    if (
        !required &&
        limits &&
        limits.runtimeSeconds === 0 &&
        limits.memoryMaxBytes === 0 &&
        limits.tasksMax === 0
    )
        limits = undefined
    const transport =
        row.transport === undefined
            ? undefined
            : parseTransport(row.transport, options.historicalTransport === true)
    const directSocket =
        row.directSocket === undefined ? undefined : parseDirectSocket(row.directSocket)
    const ringSocket = row.ringSocket === undefined ? undefined : parseRingSocket(row.ringSocket)
    if (
        new Set(members.map(member => member.nodeId)).size !== members.length ||
        new Set(members.map(member => member.pinSha256)).size !== members.length ||
        new Set(members.map(member => member.gpuUuid.toLowerCase())).size !== members.length ||
        members.some(member => member.modelDigest !== members[0].modelDigest) ||
        members.some(
            member => member.runtimeCompatibilitySha256 !== members[0].runtimeCompatibilitySha256
        ) ||
        coordinator !== members[0].nodeId
    )
        throw new Error('PAIR returned inconsistent serving-group participants.')
    const qwen = model === VLLM_QWEN38_MODEL
    const ordinary =
        topology.dataParallel === 1 &&
        ((topology.tensorParallel === members.length && topology.pipelineParallel === 1) ||
            (topology.tensorParallel === 1 && topology.pipelineParallel === members.length)) &&
        topology.expertParallel === undefined &&
        topology.eplb === undefined &&
        topology.redundantExperts === undefined &&
        topology.contextLength === undefined &&
        topology.maxSequences === undefined &&
        topology.kvCacheMemoryBytes === undefined &&
        topology.mtp === undefined &&
        topology.dflash === undefined &&
        topology.flashinferAutotune === undefined &&
        transport === undefined &&
        members.every(member => member.fabric === undefined) &&
        (directSocket === undefined || directSocketMatches(directSocket, members, topology)) &&
        (directSocket === undefined || runtime === VLLM_FABRIC_SOCKET_RUNTIME) &&
        (ringSocket === undefined ||
            (directSocket === undefined &&
                ringSocketMatches(ringSocket, members, topology) &&
                runtime === VLLM_FABRIC_SOCKET_RUNTIME)) &&
        (limits === undefined || sameLimits(limits, ORDINARY_LIMITS))
    // Qwen3.8 serves only on two Sparks, as TP2+EP2.
    const qwenFixed =
        directSocket === undefined &&
        ringSocket === undefined &&
        runtime === VLLM_QWEN38_RUNTIME &&
        members.length === 2 &&
        topology.tensorParallel === 2 &&
        topology.pipelineParallel === 1 &&
        topology.dataParallel === 1 &&
        topology.expertParallel === 2 &&
        topology.eplb === undefined &&
        topology.redundantExperts === undefined &&
        topology.contextLength === 32768 &&
        topology.maxSequences === 2 &&
        topology.kvCacheMemoryBytes === 8 * GIB &&
        topology.mtp === false &&
        topology.dflash === false &&
        topology.flashinferAutotune === false &&
        transport !== undefined &&
        (limits === undefined || sameLimits(limits, QWEN_LIMITS)) &&
        members.every(member => member.fabric?.lanes.length === 2)
    if (qwen ? !qwenFixed : !ordinary)
        throw new Error('PAIR returned an unsupported reviewed topology.')
    return {
        coordinator,
        model,
        runtime,
        topology,
        members,
        ...(limits ? { limits } : {}),
        ...(transport ? { transport } : {}),
        ...(directSocket ? { directSocket } : {}),
        ...(ringSocket ? { ringSocket } : {})
    }
}

/** Ordinary two-node TP2 only, one reciprocal lane per member in plan order. */
function directSocketMatches(
    directSocket: VllmGroupDirectSocket,
    members: VllmGroupMemberStatus[],
    topology: VllmGroupTopology
): boolean {
    const [near, far] = directSocket.lanes
    return (
        members.length === 2 &&
        topology.tensorParallel === 2 &&
        topology.pipelineParallel === 1 &&
        topology.dataParallel === 1 &&
        near.nodeId === members[0].nodeId &&
        far.nodeId === members[1].nodeId &&
        near.localAddress === far.peerAddress &&
        far.localAddress === near.peerAddress
    )
}

/**
 * Ordinary three-node TP3 or PP3 only, one member per plan member in order,
 * each advertising one of its two ascending ring addresses off management.
 * The six addresses pair into /31 cables that each join two distinct members.
 */
function ringSocketMatches(
    ringSocket: VllmGroupRingSocket,
    members: VllmGroupMemberStatus[],
    topology: VllmGroupTopology
): boolean {
    const owners = new Map<number, number>()
    ringSocket.members.forEach((member, index) =>
        member.laneAddresses.forEach(address => owners.set(ipv4Value(address) ?? -1, index))
    )
    const management = new Set(members.map(member => member.placement?.address))
    return (
        members.length === 3 &&
        topology.dataParallel === 1 &&
        ((topology.tensorParallel === 3 && topology.pipelineParallel === 1) ||
            (topology.tensorParallel === 1 && topology.pipelineParallel === 3)) &&
        ringSocket.members.every(
            (member, index) =>
                member.nodeId === members[index].nodeId &&
                (ipv4Value(member.laneAddresses[0]) ?? -1) <
                    (ipv4Value(member.laneAddresses[1]) ?? -1) &&
                member.laneAddresses.includes(member.advertisedAddress) &&
                member.laneAddresses.every(address => !management.has(address))
        ) &&
        owners.size === 6 &&
        Array.from(owners).every(([address, owner]) => {
            const peer = owners.get(address % 2 === 0 ? address + 1 : address - 1)
            return peer !== undefined && peer !== owner
        })
    )
}

function closed<T extends string>(value: unknown, allowed: readonly T[], label: string): T {
    const found = allowed.find(item => item === value)
    if (found === undefined) throw new Error(`PAIR returned an unknown ${label}.`)
    return found
}

function parseStartFailure(value: unknown): VllmGroupStartFailure {
    const row = object(value, 'rank start failure')
    const allowedKeys = ['stage', 'code', 'exit', 'stdoutBytes', 'stderrCode', 'missingProperties']
    if (
        Object.keys(row).some(key => !allowedKeys.includes(key)) ||
        typeof row.exit !== 'number' ||
        !Number.isSafeInteger(row.exit) ||
        row.exit < -1 ||
        row.exit > 255 ||
        typeof row.stdoutBytes !== 'number' ||
        !Number.isSafeInteger(row.stdoutBytes) ||
        row.stdoutBytes < 0 ||
        row.stdoutBytes > 1048576
    )
        throw new Error('PAIR returned invalid bounded rank start-failure metadata.')
    const code = closed(row.code, VLLM_GROUP_START_FAILURE_CODES, 'rank start-failure code')
    const missing = row.missingProperties
    if (
        missing !== undefined &&
        (code !== 'system_manager_readback_incomplete' ||
            !Array.isArray(missing) ||
            missing.length < 1 ||
            missing.length > VLLM_GROUP_READBACK_PROPERTIES.length ||
            missing.some(
                (property, index) =>
                    typeof property !== 'string' ||
                    !(VLLM_GROUP_READBACK_PROPERTIES as readonly string[]).includes(property) ||
                    (index > 0 && String(missing[index - 1]) >= property)
            ))
    )
        throw new Error('PAIR returned invalid system-manager readback metadata.')
    return {
        stage: closed(row.stage, VLLM_GROUP_START_FAILURE_STAGES, 'rank start-failure stage'),
        code,
        exit: row.exit,
        stdoutBytes: row.stdoutBytes,
        stderrCode: closed(row.stderrCode, VLLM_GROUP_START_STDERR_CODES, 'rank stderr code'),
        ...(missing === undefined
            ? {}
            : { missingProperties: missing as VllmGroupStartFailure['missingProperties'] })
    }
}

function parseRank(value: unknown): VllmGroupRankStatus {
    const row = object(value, 'serving-group rank')
    return {
        nodeId: identity(row.nodeId, 'serving-group rank identity'),
        attempted: flag(row.attempted, 'serving-group attempted state'),
        started: flag(row.started, 'serving-group started state'),
        cleanupConfirmed: flag(row.cleanupConfirmed, 'serving-group cleanup state'),
        ...(row.startFailure === undefined
            ? {}
            : { startFailure: parseStartFailure(row.startFailure) }),
        ...(row.cleanupFailure === undefined
            ? {}
            : { cleanupFailure: parseStartFailure(row.cleanupFailure) })
    }
}

/**
 * Strict retained-run parser. Rank evidence must correlate exactly with the
 * plan, and run-level cleanup confirmation is accepted only for a terminal
 * state whose attempted ranks are all confirmed clean.
 */
export function parseVllmGroupRun(
    value: unknown,
    options: VllmGroupPlanOptions = { ownerFields: 'optional' }
): VllmGroupRunStatus {
    const row = object(value, 'serving-group run')
    const state: VllmGroupRunState = closed(row.state, VLLM_GROUP_RUN_STATES, 'serving-group state')
    const ranks = participants(row.ranks, 'serving-group participant list').map(parseRank)
    const cleanupConfirmed = flag(row.cleanupConfirmed, 'serving-group cleanup state')
    const historicalTransport =
        options.ownerFields === 'optional' &&
        cleanupConfirmed &&
        (state === 'stopped' || state === 'failed')
    const plan = parseVllmGroupPlan(row.plan, { ...options, historicalTransport })
    if (
        ranks.length !== plan.members.length ||
        ranks.some(
            (rank, index) =>
                rank.nodeId !== plan.members[index].nodeId ||
                (rank.started && !rank.attempted) ||
                (rank.cleanupConfirmed && !rank.attempted) ||
                (rank.cleanupFailure !== undefined && (!rank.attempted || rank.cleanupConfirmed))
        )
    )
        throw new Error('PAIR returned inconsistent serving-group participants.')
    if (
        (state === 'ready' && ranks.some(rank => !rank.started)) ||
        (cleanupConfirmed &&
            ((state !== 'stopped' && state !== 'failed') ||
                ranks.some(rank => rank.attempted && !rank.cleanupConfirmed)))
    )
        throw new Error('PAIR returned inconsistent serving-group cleanup evidence.')
    return {
        runId: operationId(row.runId, 'serving-group run identity'),
        generation: positive(row.generation, 'serving-group generation'),
        planDigest: digest(row.planDigest, 'serving-group plan digest'),
        plan,
        state,
        ranks,
        cleanupConfirmed,
        ...(typeof row.failure === 'string' && row.failure ? { failure: row.failure } : {})
    }
}

/**
 * Status is the only authority for hold truth. A reserved status must carry
 * its run; an unreserved status may carry only a fully cleaned run.
 */
export function parseVllmGroupStatus(value: unknown): VllmGroupStatus {
    const row = object(value, 'serving-group status')
    if (typeof row.reason !== 'string')
        throw new Error('PAIR returned an invalid serving-group reason.')
    const result: VllmGroupStatus = {
        activationEnabled: flag(row.activationEnabled, 'serving-group activation state'),
        reserved: flag(row.reserved, 'serving-group reservation state'),
        reason: row.reason
    }
    if (row.run !== undefined && row.run !== null) result.run = parseVllmGroupRun(row.run)
    if (result.reserved && !result.run)
        throw new Error('PAIR returned a reserved serving group without its run.')
    if (!result.reserved && result.run && !result.run.cleanupConfirmed)
        throw new Error('PAIR returned an uncleared unreserved serving-group run.')
    return result
}
