// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Parsers that turn nvpair-cluster-manager JSON-RPC payloads (relayed verbatim by
 * the broker) into the typed cluster shapes the bridge and renderer share.
 *
 * Kept in its own module so both the invoke handlers (`empty-handlers.ts`) and
 * the notification demux (`modular-supervisor.ts`) can use them without an
 * import cycle.
 */
import type {
    ClusterNode,
    ClusterNodeIdentity,
    Invite,
    InviteState,
    MembershipState
} from '@/shared/types/cluster'
import type { JsonObject, JsonValue } from './json-rpc-subprocess'

function objectValue(value: JsonValue | undefined): JsonObject | null {
    if (!value || typeof value !== 'object' || Array.isArray(value)) return null
    return value
}

function stringValue(value: JsonValue | undefined): string {
    return typeof value === 'string' ? value : ''
}

function numberValue(value: JsonValue | undefined): number {
    return typeof value === 'number' ? value : 0
}

function nullableNumber(value: JsonValue | undefined): number | null {
    return typeof value === 'number' ? value : null
}

function nullableString(value: JsonValue | undefined): string | null {
    return typeof value === 'string' ? value : null
}

function arrayValue(value: JsonValue | undefined): JsonValue[] {
    return Array.isArray(value) ? value : []
}

function inviteState(value: JsonValue | undefined): InviteState {
    switch (stringValue(value)) {
        case 'pending':
            return 'pending'
        case 'paired':
            return 'paired'
        case 'declined':
            return 'declined'
        case 'canceled':
            return 'canceled'
        case 'expired':
            return 'expired'
        case 'rejected':
            return 'rejected'
        default:
            return 'failed'
    }
}

function membershipState(value: JsonValue | undefined): MembershipState {
    switch (stringValue(value)) {
        case 'pending-outbound':
            return 'pending-outbound'
        case 'pending-inbound':
            return 'pending-inbound'
        default:
            return 'member'
    }
}

export function parseInvite(value: JsonValue | undefined): Invite {
    const obj = objectValue(value)
    return {
        inviteId: stringValue(obj?.inviteId),
        fromNodeId: stringValue(obj?.fromNodeId),
        fromNodeUuid: stringValue(obj?.fromNodeUuid),
        fromNodeName: stringValue(obj?.fromNodeName),
        toNodeId: nullableString(obj?.toNodeId),
        clusterId: stringValue(obj?.clusterId),
        clusterFriendlyName: stringValue(obj?.clusterFriendlyName),
        pin: nullableString(obj?.pin),
        state: inviteState(obj?.state),
        reason: stringValue(obj?.reason),
        createdAt: numberValue(obj?.createdAt),
        respondedAt: nullableNumber(obj?.respondedAt)
    }
}

export function emptyInvite(): Invite {
    return {
        inviteId: '',
        fromNodeId: '',
        fromNodeUuid: '',
        fromNodeName: '',
        toNodeId: null,
        clusterId: '',
        clusterFriendlyName: '',
        pin: null,
        state: 'failed',
        reason: '',
        createdAt: Date.now(),
        respondedAt: null
    }
}

function parseClusterNode(value: JsonValue | undefined): ClusterNode {
    const obj = objectValue(value)
    return {
        id: stringValue(obj?.id),
        nodeUuid: stringValue(obj?.nodeUuid),
        name: stringValue(obj?.name),
        ipAddress: stringValue(obj?.ipAddress),
        port: numberValue(obj?.port),
        clusterId: stringValue(obj?.clusterId),
        state: membershipState(obj?.state),
        joinedAt: nullableNumber(obj?.joinedAt),
        lastSeen: nullableNumber(obj?.lastSeen)
    }
}

/** Parse a `{ nodes: ClusterNode[] }` envelope (nodes:get-initial / nodes:changed). */
export function parseClusterNodes(value: JsonValue | undefined): ClusterNode[] {
    return arrayValue(objectValue(value)?.nodes).map(parseClusterNode)
}

export function parseNodeIdentity(value: JsonValue | undefined): ClusterNodeIdentity {
    const obj = objectValue(value)
    return {
        nodeUuid: stringValue(obj?.nodeUuid),
        nodeId: stringValue(obj?.nodeId),
        name: stringValue(obj?.name),
        certFingerprint: stringValue(obj?.certFingerprint),
        clusterId: stringValue(obj?.clusterId)
    }
}
