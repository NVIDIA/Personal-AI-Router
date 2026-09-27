// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type {
    CableCleanupRecovery,
    CableCleanupReview,
    CableCleanupReviewTarget,
    CableCleanupVerifyResult,
    CableEdge,
    CableInterface,
    CableParticipantDiagnostic,
    CablePort,
    CableRetainedRun,
    CableRetainedRuns,
    CableReview,
    CableRun,
    CableRunDiagnostics,
    CableStartNotStarted,
    CableStartResponse,
    CableTarget,
    FabricAcceptedHostKey,
    FabricCandidateIP,
    FabricGeneratedDefault,
    FabricInventorySnapshot,
    FabricInterface,
    FabricOperation,
    FabricPermissionReview,
    FabricPermissionTarget,
    FabricPortRef,
    FabricRetainedOperations,
    FabricReview,
    FabricRoute,
    FabricSelection,
    FabricTarget
} from '@/shared/types/fabric'

type Row = Record<string, unknown>

const routedRingRecipe = 'spark-three-node-ring-routed-v2'
const reviewRecipes = ['spark-two-node-temporary-addresses-v1', routedRingRecipe] as const
const operationRecipes = [...reviewRecipes, 'spark-three-node-ring-temporary-addresses-v1'] as const

function row(value: unknown, label: string, keys: readonly string[]): Row {
    if (!value || typeof value !== 'object' || Array.isArray(value)) invalid(label)
    const result = value as Row
    if (Object.keys(result).some(key => !keys.includes(key))) invalid(label)
    return result
}

function invalid(label: string): never {
    throw new Error(`PAIR returned an invalid ${label}.`)
}

function string(value: unknown, label: string, max = 4096, empty = false): string {
    if (typeof value !== 'string' || (!empty && !value) || value.length > max) invalid(label)
    if (Array.from(value).some(char => char.charCodeAt(0) < 32 || char.charCodeAt(0) === 127))
        invalid(label)
    return value
}

function id(value: unknown, label: string): string {
    return string(value, label, 256)
}

function operationId(value: unknown, label: string): string {
    const result = string(value, label, 32)
    if (!/^[0-9a-f]{32}$/.test(result)) invalid(label)
    return result
}

function digest(value: unknown, label: string): string {
    const result = string(value, label, 64)
    if (!/^[0-9a-f]{64}$/i.test(result)) invalid(label)
    return result.toLowerCase()
}

function sshFingerprint(value: unknown, label: string): string {
    const result = string(value, label, 50)
    // OpenSSH SHA-256 fingerprints are one 32-byte digest in unpadded,
    // canonical base64. The final character carries four data bits.
    if (!/^SHA256:[A-Za-z0-9+/]{42}[AEIMQUYcgkosw048]$/.test(result)) invalid(label)
    return result
}

function integer(value: unknown, label: string, min = 0, max = Number.MAX_SAFE_INTEGER): number {
    if (!Number.isSafeInteger(value) || (value as number) < min || (value as number) > max)
        invalid(label)
    return value as number
}

function flag(value: unknown, label: string): boolean {
    if (typeof value !== 'boolean') invalid(label)
    return value
}

function list(value: unknown, label: string, min = 0, max = 128): unknown[] {
    if (!Array.isArray(value) || value.length < min || value.length > max) invalid(label)
    return value
}

function strings(value: unknown, label: string, max = 128): string[] {
    return list(value, label, 0, max).map(item => string(item, label, 4096, true))
}

function optionalString(value: unknown, label: string, max = 4096): string | undefined {
    return value === undefined ? undefined : string(value, label, max, true)
}

function oneOf<T extends string>(value: unknown, values: readonly T[], label: string): T {
    if (!values.includes(value as T)) invalid(label)
    return value as T
}

function distinct(values: string[], label: string): void {
    if (new Set(values).size !== values.length) invalid(label)
}

export function ipv4Value(value: string): number | null {
    const parts = value.split('.')
    if (
        parts.length !== 4 ||
        parts.some(part => !/^(0|[1-9][0-9]{0,2})$/.test(part) || Number(part) > 255)
    )
        return null
    return parts.reduce((total, part) => total * 256 + Number(part), 0)
}

function ipv4(value: unknown, label: string): string {
    const result = string(value, label, 15)
    if (ipv4Value(result) === null) invalid(label)
    return result
}

export function exactFabricKeys(value: unknown, keys: readonly string[], label: string): Row {
    return row(value, label, keys)
}

function parsePortRef(value: unknown): FabricPortRef {
    const valueRow = row(value, 'physical port reference', ['nodeId', 'switchId', 'portName'])
    return {
        nodeId: id(valueRow.nodeId, 'physical port node'),
        switchId: id(valueRow.switchId, 'physical switch identity'),
        portName: id(valueRow.portName, 'physical port name')
    }
}

function parseAcceptedHostKey(value: unknown): FabricAcceptedHostKey {
    const valueRow = row(value, 'accepted host key', ['candidateId', 'sha256'])
    return {
        candidateId: id(valueRow.candidateId, 'access candidate'),
        sha256: sshFingerprint(valueRow.sha256, 'host key fingerprint')
    }
}

/** UI selection is the complete product input: no paths, credentials, commands or callbacks. */
export function parseFabricSelection(value: unknown): FabricSelection {
    const valueRow = row(value, 'fabric selection', ['nodeIds', 'ports', 'acceptedHostKeys'])
    const nodeIds = list(valueRow.nodeIds, 'fabric nodes', 2, 3).map(item =>
        id(item, 'fabric node')
    )
    distinct(nodeIds, 'fabric nodes')
    const ports = list(valueRow.ports, 'fabric ports', nodeIds.length, nodeIds.length * 2).map(
        parsePortRef
    )
    const keys = ports.map(port => `${port.nodeId}\n${port.switchId}\n${port.portName}`)
    distinct(keys, 'fabric ports')
    if (ports.some(port => !nodeIds.includes(port.nodeId))) invalid('fabric port owner')
    for (const nodeId of nodeIds) {
        const selected = ports.filter(port => port.nodeId === nodeId)
        if (nodeIds.length === 2 && selected.length !== 1) invalid('two-node cable selection')
        if (
            nodeIds.length === 3 &&
            (selected.length !== 2 ||
                !selected.some(port => port.portName === 'p0') ||
                !selected.some(port => port.portName === 'p1'))
        )
            invalid('three-node ring selection')
    }
    const acceptedHostKeys =
        valueRow.acceptedHostKeys === undefined
            ? undefined
            : list(valueRow.acceptedHostKeys, 'accepted host keys', 0, 3).map(parseAcceptedHostKey)
    if (acceptedHostKeys)
        distinct(
            acceptedHostKeys.map(key => key.candidateId),
            'accepted host keys'
        )
    return { nodeIds, ports, ...(acceptedHostKeys ? { acceptedHostKeys } : {}) }
}

export function parseFabricInventoryRequest(value: unknown): { nodeIds: string[] } {
    const valueRow = row(value, 'fabric inventory request', ['nodeIds'])
    const nodeIds = list(valueRow.nodeIds, 'fabric inventory nodes', 1, 3).map(item =>
        id(item, 'fabric inventory node')
    )
    distinct(nodeIds, 'fabric inventory nodes')
    return { nodeIds }
}

export function parseFabricInventorySnapshot(value: unknown): FabricInventorySnapshot {
    const valueRow = row(value, 'fabric inventory', ['nodes'])
    const nodes = list(valueRow.nodes, 'fabric inventory nodes', 1, 3).map(item => {
        const node = row(item, 'fabric node observation', [
            'nodeId',
            'status',
            'observedAt',
            'ports',
            'reason'
        ])
        const status = oneOf(
            node.status,
            ['observed', 'unavailable'] as const,
            'fabric node observation status'
        )
        const ports = list(node.ports, 'fabric observed ports', 0, 16).map(item => {
            const port = row(item, 'fabric port observation', [
                'switchId',
                'portName',
                'eligible',
                'reason'
            ])
            return {
                switchId: id(port.switchId, 'fabric observed switch'),
                portName: id(port.portName, 'fabric observed port'),
                eligible: flag(port.eligible, 'fabric observed port eligibility'),
                ...(port.reason === undefined
                    ? {}
                    : { reason: string(port.reason, 'fabric observed port reason', 512) })
            }
        })
        distinct(
            ports.map(port => `${port.switchId}\n${port.portName}`),
            'fabric observed ports'
        )
        if (
            (status === 'observed' &&
                (!Number.isSafeInteger(node.observedAt) || (node.observedAt as number) <= 0)) ||
            (status === 'unavailable' && (ports.length !== 0 || node.observedAt !== undefined))
        )
            invalid('fabric node observation')
        return {
            nodeId: id(node.nodeId, 'fabric observed node'),
            status,
            ...(node.observedAt === undefined
                ? {}
                : { observedAt: integer(node.observedAt, 'fabric observation time', 1) }),
            ports,
            ...(node.reason === undefined
                ? {}
                : { reason: string(node.reason, 'fabric observation reason', 512) })
        }
    })
    distinct(
        nodes.map(node => node.nodeId),
        'fabric inventory nodes'
    )
    return { nodes }
}

export function parseReviewIdRequest(value: unknown): { reviewId: string } {
    const valueRow = row(value, 'review request', ['reviewId'])
    return { reviewId: id(valueRow.reviewId, 'review identity') }
}

export function parseOperationIdRequest(value: unknown): { operationId: string } {
    const valueRow = row(value, 'operation request', ['operationId'])
    return { operationId: operationId(valueRow.operationId, 'fabric operation') }
}

export function parseCableCleanupReviewRequest(value: unknown): {
    runId: string
    acceptedHostKeys?: FabricAcceptedHostKey[]
} {
    const valueRow = row(value, 'cable cleanup review request', ['runId', 'acceptedHostKeys'])
    const runId = operationId(valueRow.runId, 'cable run identity')
    const acceptedHostKeys =
        valueRow.acceptedHostKeys === undefined
            ? undefined
            : list(valueRow.acceptedHostKeys, 'accepted host keys', 0, 3).map(parseAcceptedHostKey)
    if (acceptedHostKeys)
        distinct(
            acceptedHostKeys.map(key => key.candidateId),
            'accepted host keys'
        )
    return { runId, ...(acceptedHostKeys ? { acceptedHostKeys } : {}) }
}

export function parseCableStatusRequest(value: unknown): { runId?: string; reviewId?: string } {
    const valueRow = row(value, 'cable status request', ['runId', 'reviewId'])
    const rawRunId = optionalString(valueRow.runId, 'cable run identity', 128)
    const runId = rawRunId === undefined ? undefined : operationId(rawRunId, 'cable run identity')
    const reviewId = optionalString(valueRow.reviewId, 'cable review identity', 128)
    if ((runId === undefined) === (reviewId === undefined)) invalid('cable status identity')
    return { ...(runId ? { runId } : {}), ...(reviewId ? { reviewId } : {}) }
}

function parsePermissionTarget(value: unknown): FabricPermissionTarget {
    const valueRow = row(value, 'permission target', [
        'nodeId',
        'candidateId',
        'accessLabel',
        'hostKeySha256',
        'hostKeyTrusted',
        'accessAvailable',
        'elevationAvailable',
        'workerAvailable',
        'workerSha256',
        'reason'
    ])
    const hostKeySha256 =
        valueRow.hostKeySha256 === undefined
            ? undefined
            : sshFingerprint(valueRow.hostKeySha256, 'host key fingerprint')
    const workerSha256 = optionalString(valueRow.workerSha256, 'worker digest', 64)
    return {
        nodeId: id(valueRow.nodeId, 'permission node'),
        candidateId: optionalString(valueRow.candidateId, 'permission candidate', 256) ?? '',
        accessLabel: optionalString(valueRow.accessLabel, 'access label', 256) ?? '',
        ...(hostKeySha256 ? { hostKeySha256 } : {}),
        hostKeyTrusted: flag(valueRow.hostKeyTrusted, 'host key trust'),
        accessAvailable: flag(valueRow.accessAvailable, 'access availability'),
        elevationAvailable: flag(valueRow.elevationAvailable, 'elevation availability'),
        workerAvailable: flag(valueRow.workerAvailable, 'worker availability'),
        ...(workerSha256 ? { workerSha256: digest(workerSha256, 'worker digest') } : {}),
        ...(valueRow.reason === undefined
            ? {}
            : { reason: string(valueRow.reason, 'permission reason', 2048, true) })
    }
}

function parsePermission(value: unknown): FabricPermissionReview {
    const valueRow = row(value, 'permission review', ['mode', 'targets', 'effects'])
    return {
        mode: id(valueRow.mode, 'permission mode'),
        targets: list(valueRow.targets, 'permission targets', 2, 3).map(parsePermissionTarget),
        effects: strings(valueRow.effects, 'permission effects', 16)
    }
}

function parseCableInterface(value: unknown): CableInterface {
    const valueRow = row(value, 'cable interface', ['name', 'index', 'mac'])
    return {
        name: id(valueRow.name, 'interface name'),
        index: integer(valueRow.index, 'interface index', 1),
        mac: string(valueRow.mac, 'interface MAC', 32)
    }
}

function parseCablePort(value: unknown): CablePort {
    const valueRow = row(value, 'cable port', ['switchId', 'portName', 'interfaces'])
    return {
        switchId: id(valueRow.switchId, 'physical switch identity'),
        portName: id(valueRow.portName, 'physical port name'),
        interfaces: list(valueRow.interfaces, 'physical port interfaces', 1, 4).map(
            parseCableInterface
        )
    }
}

function parseCableTarget(value: unknown): CableTarget {
    const valueRow = row(value, 'cable target', [
        'nodeId',
        'principal',
        'ports',
        'rawPrivilege',
        'reason'
    ])
    return {
        nodeId: id(valueRow.nodeId, 'cable node'),
        principal: optionalString(valueRow.principal, 'paired principal', 256) ?? '',
        ports: list(valueRow.ports, 'cable ports', 0, 2).map(parseCablePort),
        rawPrivilege: oneOf(
            valueRow.rawPrivilege,
            ['present', 'approval-needed', 'unsupported', 'unknown'] as const,
            'raw-network privilege'
        ),
        ...(valueRow.reason === undefined
            ? {}
            : { reason: string(valueRow.reason, 'cable target reason', 2048, true) })
    }
}

export function parseCableReview(value: unknown): CableReview {
    const valueRow = row(value, 'cable review', [
        'reviewId',
        'ownerNodeId',
        'targets',
        'available',
        'reason',
        'remainingMs',
        'consumedRunId',
        'permission'
    ])
    const targets = list(valueRow.targets, 'cable targets', 2, 3).map(parseCableTarget)
    distinct(
        targets.map(target => target.nodeId),
        'cable target identities'
    )
    return {
        reviewId: id(valueRow.reviewId, 'cable review identity'),
        ownerNodeId: id(valueRow.ownerNodeId, 'cable owner'),
        targets,
        available: flag(valueRow.available, 'cable availability'),
        ...(valueRow.reason === undefined
            ? {}
            : { reason: string(valueRow.reason, 'cable review reason', 2048, true) }),
        remainingMs: integer(valueRow.remainingMs, 'cable review lifetime'),
        ...(valueRow.consumedRunId === undefined
            ? {}
            : {
                  consumedRunId: operationId(valueRow.consumedRunId, 'consumed cable run')
              }),
        ...(valueRow.permission === undefined
            ? {}
            : { permission: parsePermission(valueRow.permission) })
    }
}

function parseCableEdge(value: unknown): CableEdge {
    const valueRow = row(value, 'cable edge', ['left', 'right', 'ageMs', 'fresh'])
    return {
        left: parsePortRef(valueRow.left),
        right: parsePortRef(valueRow.right),
        ageMs: integer(valueRow.ageMs, 'cable observation age', 0, 60_000),
        fresh: flag(valueRow.fresh, 'cable freshness')
    }
}

function parseCableParticipant(value: unknown): CableParticipantDiagnostic {
    const valueRow = row(value, 'cable participant diagnostic', [
        'nodeId',
        'phase',
        'code',
        'workerState',
        'sent',
        'received',
        'cleanupConfirmed',
        'finalValidationCode',
        'preparationResourceOutcome',
        'factsDifference'
    ])
    let factsDifference
    if (valueRow.factsDifference !== undefined) {
        const facts = row(valueRow.factsDifference, 'cable facts difference', [
            'targetIndex',
            'field',
            'readStatus'
        ])
        factsDifference = {
            targetIndex: integer(facts.targetIndex, 'cable difference target', 0, 2),
            field: id(facts.field, 'cable difference field'),
            ...(facts.readStatus === undefined
                ? {}
                : { readStatus: id(facts.readStatus, 'cable read status') })
        }
    }
    return {
        nodeId: id(valueRow.nodeId, 'diagnostic node'),
        phase: id(valueRow.phase, 'diagnostic phase'),
        ...(valueRow.code === undefined ? {} : { code: id(valueRow.code, 'diagnostic code') }),
        ...(valueRow.workerState === undefined
            ? {}
            : {
                  workerState: oneOf(
                      valueRow.workerState,
                      ['completed', 'cancelled', 'failed'] as const,
                      'worker state'
                  )
              }),
        ...(valueRow.sent === undefined
            ? {}
            : { sent: integer(valueRow.sent, 'sent frame count', 0, 40) }),
        ...(valueRow.received === undefined
            ? {}
            : { received: integer(valueRow.received, 'received frame count', 0, 256) }),
        cleanupConfirmed: flag(valueRow.cleanupConfirmed, 'participant cleanup'),
        ...(valueRow.finalValidationCode === undefined
            ? {}
            : { finalValidationCode: id(valueRow.finalValidationCode, 'final validation code') }),
        ...(valueRow.preparationResourceOutcome === undefined
            ? {}
            : {
                  preparationResourceOutcome: id(
                      valueRow.preparationResourceOutcome,
                      'preparation resource outcome'
                  )
              }),
        ...(factsDifference ? { factsDifference } : {})
    }
}

function parseCableDiagnostics(value: unknown): CableRunDiagnostics {
    const valueRow = row(value, 'cable diagnostics', ['failure', 'participants'])
    let failure
    if (valueRow.failure !== undefined) {
        const failureRow = row(valueRow.failure, 'cable failure', ['phase', 'code'])
        failure = {
            phase: id(failureRow.phase, 'cable failure phase'),
            code: id(failureRow.code, 'cable failure code')
        }
    }
    return {
        ...(failure ? { failure } : {}),
        participants: list(valueRow.participants, 'cable diagnostics participants', 2, 3).map(
            parseCableParticipant
        )
    }
}

function parseCleanupRecovery(value: unknown): CableCleanupRecovery {
    const valueRow = row(value, 'cable cleanup recovery', [
        'attemptId',
        'reviewId',
        'revision',
        'state',
        'holdReleased',
        'code',
        'message',
        'startedAt',
        'finishedAt',
        'targets'
    ])
    return {
        attemptId: id(valueRow.attemptId, 'cleanup attempt'),
        reviewId: id(valueRow.reviewId, 'cleanup review'),
        revision: integer(valueRow.revision, 'cleanup revision', 1),
        state: oneOf(
            valueRow.state,
            ['verifying', 'release-pending', 'released', 'failed', 'cancelled'] as const,
            'cleanup state'
        ),
        holdReleased: flag(valueRow.holdReleased, 'cleanup hold release'),
        code: id(valueRow.code, 'cleanup code'),
        message: string(valueRow.message, 'cleanup message', 2048, true),
        startedAt: integer(valueRow.startedAt, 'cleanup start time', 1),
        ...(valueRow.finishedAt === undefined
            ? {}
            : { finishedAt: integer(valueRow.finishedAt, 'cleanup finish time', 1) }),
        targets: list(valueRow.targets, 'cleanup targets', 0, 3).map(value => {
            const target = row(value, 'cleanup target', ['nodeId', 'state', 'reason'])
            return {
                nodeId: id(target.nodeId, 'cleanup target node'),
                state: id(target.state, 'cleanup target state'),
                ...(target.reason === undefined
                    ? {}
                    : { reason: string(target.reason, 'cleanup target reason', 2048, true) })
            }
        })
    }
}

export function parseCableRun(value: unknown): CableRun {
    const valueRow = row(value, 'cable run', [
        'runId',
        'reviewId',
        'ownerNodeId',
        'revision',
        'state',
        'targets',
        'edges',
        'result',
        'directness',
        'remainingMs',
        'freshnessRemainingMs',
        'cleanupConfirmed',
        'startedAt',
        'finishedAt',
        'message',
        'diagnostics',
        'cleanupRecovery',
        'topology'
    ])
    const targets = list(valueRow.targets, 'cable run targets', 2, 3).map(parseCableTarget)
    let topology
    if (valueRow.topology !== undefined) {
        const topologyRow = row(valueRow.topology, 'cable topology', ['layout', 'status'])
        topology = {
            layout: oneOf(topologyRow.layout, ['direct', 'ring'] as const, 'cable layout'),
            status: oneOf(
                topologyRow.status,
                ['unavailable', 'unexpected', 'missing', 'matched'] as const,
                'cable topology status'
            )
        }
    }
    const result: CableRun = {
        runId: operationId(valueRow.runId, 'cable run'),
        reviewId: id(valueRow.reviewId, 'cable review'),
        ownerNodeId: id(valueRow.ownerNodeId, 'cable owner'),
        revision: integer(valueRow.revision, 'cable revision', 1),
        state: oneOf(
            valueRow.state,
            ['preparing', 'running', 'cancelling', 'completed', 'cancelled', 'failed'] as const,
            'cable state'
        ),
        targets,
        edges: list(valueRow.edges, 'cable edges', 0, 6).map(parseCableEdge),
        result: oneOf(
            valueRow.result,
            ['unavailable', 'incomplete', 'reciprocal-observations', 'ambiguous'] as const,
            'cable result'
        ),
        directness: oneOf(valueRow.directness, ['unverified'] as const, 'cable directness'),
        remainingMs: integer(valueRow.remainingMs, 'cable time remaining', 0, 20_000),
        freshnessRemainingMs: integer(
            valueRow.freshnessRemainingMs,
            'cable freshness remaining',
            0,
            2_000
        ),
        cleanupConfirmed: flag(valueRow.cleanupConfirmed, 'cable cleanup'),
        startedAt: integer(valueRow.startedAt, 'cable start time', 1),
        ...(valueRow.finishedAt === undefined
            ? {}
            : { finishedAt: integer(valueRow.finishedAt, 'cable finish time', 1) }),
        message: string(valueRow.message, 'cable message', 2048, true),
        ...(valueRow.diagnostics === undefined
            ? {}
            : { diagnostics: parseCableDiagnostics(valueRow.diagnostics) }),
        ...(valueRow.cleanupRecovery === undefined
            ? {}
            : { cleanupRecovery: parseCleanupRecovery(valueRow.cleanupRecovery) }),
        ...(topology ? { topology } : {})
    }
    if (result.diagnostics && result.diagnostics.participants.length !== targets.length)
        invalid('cable diagnostics')
    if (result.state !== 'running' && result.edges.some(edge => edge.fresh))
        invalid('historical cable edge')
    return result
}

export function parseCableStartResponse(value: unknown): CableStartResponse {
    if (value && typeof value === 'object' && !Array.isArray(value) && 'disposition' in value) {
        const valueRow = row(value, 'cable start refusal', [
            'disposition',
            'reviewId',
            'ownerNodeId',
            'reason'
        ])
        const result: CableStartNotStarted = {
            disposition: oneOf(valueRow.disposition, ['not-started'] as const, 'disposition'),
            reviewId: id(valueRow.reviewId, 'cable review'),
            ownerNodeId: id(valueRow.ownerNodeId, 'cable owner'),
            reason: oneOf(
                valueRow.reason,
                [
                    'review-expired',
                    'review-unavailable',
                    'trust-changed',
                    'access-changed'
                ] as const,
                'cable refusal reason'
            )
        }
        return result
    }
    return parseCableRun(value)
}

function parseRetainedRun(value: unknown): CableRetainedRun {
    const valueRow = row(value, 'retained cable run', [
        'runId',
        'reviewId',
        'ownerNodeId',
        'revision',
        'state',
        'cleanupConfirmed',
        'startedAt',
        'nodeIds',
        'ports'
    ])
    return {
        runId: operationId(valueRow.runId, 'retained cable run'),
        reviewId: id(valueRow.reviewId, 'retained cable review'),
        ownerNodeId: id(valueRow.ownerNodeId, 'retained cable owner'),
        revision: integer(valueRow.revision, 'retained cable revision', 1),
        state: oneOf(
            valueRow.state,
            ['preparing', 'running', 'cancelling', 'completed', 'cancelled', 'failed'] as const,
            'retained cable state'
        ),
        cleanupConfirmed: flag(valueRow.cleanupConfirmed, 'retained cable cleanup'),
        startedAt: integer(valueRow.startedAt, 'retained cable start time', 1),
        nodeIds: strings(valueRow.nodeIds, 'retained cable nodes', 3),
        ports: list(valueRow.ports, 'retained cable ports', 2, 6).map(parsePortRef)
    }
}

export function parseCableRetainedRuns(value: unknown): CableRetainedRuns {
    const valueRow = row(value, 'retained cable runs', [
        'ownerNodeId',
        'held',
        'limited',
        'reason',
        'runs'
    ])
    return {
        ownerNodeId: id(valueRow.ownerNodeId, 'retained cable owner'),
        held: flag(valueRow.held, 'retained cable hold'),
        limited: flag(valueRow.limited, 'retained cable limit'),
        ...(valueRow.reason === undefined
            ? {}
            : { reason: string(valueRow.reason, 'retained cable reason', 2048, true) }),
        runs: list(valueRow.runs, 'retained cable runs', 0, 128).map(parseRetainedRun)
    }
}

function parseCleanupReviewTarget(value: unknown): CableCleanupReviewTarget {
    const valueRow = row(value, 'cable cleanup review target', [
        'nodeId',
        'candidateId',
        'accessLabel',
        'hostKeySha256',
        'hostKeyTrusted',
        'accessAvailable',
        'elevationAvailable',
        'inspectorAvailable',
        'reason'
    ])
    const hostKeySha256 =
        valueRow.hostKeySha256 === undefined
            ? undefined
            : sshFingerprint(valueRow.hostKeySha256, 'host key fingerprint')
    return {
        nodeId: id(valueRow.nodeId, 'cleanup review node'),
        candidateId: optionalString(valueRow.candidateId, 'cleanup candidate', 256) ?? '',
        accessLabel: optionalString(valueRow.accessLabel, 'cleanup access label', 256) ?? '',
        ...(hostKeySha256 ? { hostKeySha256 } : {}),
        hostKeyTrusted: flag(valueRow.hostKeyTrusted, 'cleanup host key trust'),
        accessAvailable: flag(valueRow.accessAvailable, 'cleanup access availability'),
        elevationAvailable: flag(valueRow.elevationAvailable, 'cleanup elevation availability'),
        inspectorAvailable: flag(valueRow.inspectorAvailable, 'cleanup inspector availability'),
        ...(valueRow.reason === undefined
            ? {}
            : { reason: string(valueRow.reason, 'cleanup review reason', 2048, true) })
    }
}

export function parseCableCleanupReview(value: unknown): CableCleanupReview {
    const valueRow = row(value, 'cable cleanup review', [
        'reviewId',
        'runId',
        'available',
        'remainingMs',
        'reason',
        'effects',
        'targets'
    ])
    return {
        reviewId: id(valueRow.reviewId, 'cleanup review identity'),
        runId: operationId(valueRow.runId, 'cleanup run identity'),
        available: flag(valueRow.available, 'cleanup review availability'),
        remainingMs: integer(valueRow.remainingMs, 'cleanup review lifetime', 0, 30_000),
        ...(valueRow.reason === undefined
            ? {}
            : { reason: string(valueRow.reason, 'cleanup review reason', 2048, true) }),
        effects: strings(valueRow.effects, 'cleanup effects', 16),
        targets: list(valueRow.targets, 'cleanup review targets', 0, 3).map(
            parseCleanupReviewTarget
        )
    }
}

export function parseCableCleanupVerifyResult(value: unknown): CableCleanupVerifyResult {
    const valueRow = row(value, 'cable cleanup result', ['disposition', 'reviewId', 'run'])
    const result = {
        disposition: oneOf(
            valueRow.disposition,
            ['accepted', 'not-started'] as const,
            'cleanup disposition'
        ),
        reviewId: id(valueRow.reviewId, 'cleanup review identity'),
        run: parseCableRun(valueRow.run)
    }
    if (result.run.cleanupRecovery && result.run.cleanupRecovery.reviewId !== result.reviewId)
        invalid('cleanup result binding')
    return result
}

function parseGeneratedDefault(value: unknown): FabricGeneratedDefault {
    const valueRow = row(value, 'generated network profile', [
        'schemaVersion',
        'interfaceName',
        'index',
        'owner',
        'busId',
        'devicePath',
        'permanentMAC',
        'originalAutoconnect',
        'uuid',
        'name',
        'settingsPath',
        'profileDigest',
        'activePath'
    ])
    if (valueRow.schemaVersion !== 1) invalid('generated network profile schema')
    return {
        schemaVersion: 1,
        interfaceName: id(valueRow.interfaceName, 'generated profile interface'),
        index: integer(valueRow.index, 'generated profile interface index', 1),
        owner: id(valueRow.owner, 'generated profile owner'),
        busId: string(valueRow.busId, 'generated profile bus identity', 32),
        devicePath: string(valueRow.devicePath, 'generated profile device path', 512),
        permanentMAC: string(valueRow.permanentMAC, 'generated profile MAC', 32),
        originalAutoconnect: flag(valueRow.originalAutoconnect, 'original autoconnect'),
        uuid: id(valueRow.uuid, 'generated profile UUID'),
        name: id(valueRow.name, 'generated profile name'),
        settingsPath: string(valueRow.settingsPath, 'generated profile settings path', 512),
        profileDigest: digest(valueRow.profileDigest, 'generated profile digest'),
        activePath: string(valueRow.activePath, 'generated active profile path', 512, true)
    }
}

function parseFabricRoute(value: unknown): FabricRoute {
    const valueRow = row(value, 'fabric host route', ['destination', 'gateway'])
    const destination = string(valueRow.destination, 'fabric route destination', 18)
    if (!destination.endsWith('/32') || ipv4Value(destination.slice(0, -3)) === null)
        invalid('fabric route destination')
    return { destination, gateway: ipv4(valueRow.gateway, 'fabric route gateway') }
}

function parseFabricInterface(value: unknown): FabricInterface {
    const valueRow = row(value, 'fabric interface', [
        'name',
        'index',
        'mac',
        'physicalPort',
        'addresses',
        'address',
        'driver',
        'rdmaDevices',
        'mtu',
        'generatedDefault',
        'routes'
    ])
    const port = row(valueRow.physicalPort, 'fabric physical port', [
        'source',
        'switchId',
        'portName'
    ])
    return {
        name: id(valueRow.name, 'fabric interface name'),
        index: integer(valueRow.index, 'fabric interface index', 1),
        mac: string(valueRow.mac, 'fabric interface MAC', 32),
        physicalPort: {
            source: id(port.source, 'physical port source'),
            switchId: id(port.switchId, 'physical switch identity'),
            portName: id(port.portName, 'physical port name')
        },
        addresses: strings(valueRow.addresses, 'fabric addresses', 64),
        address: string(valueRow.address, 'fabric proposed address', 64),
        driver: id(valueRow.driver, 'fabric driver'),
        rdmaDevices: strings(valueRow.rdmaDevices, 'RDMA devices', 8),
        mtu: integer(valueRow.mtu, 'fabric MTU', 1, 1_000_000),
        ...(valueRow.generatedDefault === undefined
            ? {}
            : { generatedDefault: parseGeneratedDefault(valueRow.generatedDefault) }),
        ...(valueRow.routes === undefined
            ? {}
            : { routes: list(valueRow.routes, 'fabric host routes', 1, 1).map(parseFabricRoute) })
    }
}

function parseFabricTarget(value: unknown): FabricTarget {
    const valueRow = row(value, 'fabric target', [
        'nodeId',
        'principal',
        'switchId',
        'portName',
        'ports',
        'advertisedAddress',
        'interfaces'
    ])
    const ports =
        valueRow.ports === undefined
            ? undefined
            : list(valueRow.ports, 'fabric target ports', 1, 2).map(port => {
                  const portRow = row(port, 'fabric target port', ['switchId', 'portName'])
                  return {
                      switchId: id(portRow.switchId, 'fabric switch identity'),
                      portName: id(portRow.portName, 'fabric port name')
                  }
              })
    return {
        nodeId: id(valueRow.nodeId, 'fabric node'),
        principal: id(valueRow.principal, 'fabric principal'),
        ...(valueRow.switchId === undefined
            ? {}
            : { switchId: id(valueRow.switchId, 'fabric switch identity') }),
        ...(valueRow.portName === undefined
            ? {}
            : { portName: id(valueRow.portName, 'fabric port name') }),
        ...(ports ? { ports } : {}),
        ...(valueRow.advertisedAddress === undefined
            ? {}
            : { advertisedAddress: ipv4(valueRow.advertisedAddress, 'fabric advertised address') }),
        interfaces: list(valueRow.interfaces, 'fabric interfaces', 2, 2).map(parseFabricInterface)
    }
}

// Only the routed ring carries advertised addresses and host routes. Each
// member advertises its p0 address and routes to one peer's advertised address
// through the other end of its own p0 /31.
function validateRecipeTargets(recipeId: string | undefined, targets: FabricTarget[]): void {
    for (const target of targets) {
        const routed = target.interfaces.filter(iface => iface.routes)
        if (recipeId !== routedRingRecipe) {
            if (target.advertisedAddress !== undefined || routed.length)
                invalid('fabric recipe routes')
            continue
        }
        if (routed.length !== 1) invalid('routed ring target')
        const p0 = routed[0]
        const route = p0.routes?.[0]
        const [own, bits] = p0.address.split('/')
        const ownValue = ipv4Value(own)
        const gatewayValue = route ? ipv4Value(route.gateway) : null
        if (
            !route ||
            p0.physicalPort.portName !== 'p0' ||
            bits !== '31' ||
            ownValue === null ||
            gatewayValue === null ||
            gatewayValue === ownValue ||
            Math.floor(gatewayValue / 2) !== Math.floor(ownValue / 2) ||
            target.advertisedAddress !== own ||
            !targets.some(
                peer =>
                    peer.nodeId !== target.nodeId &&
                    peer.advertisedAddress !== undefined &&
                    `${peer.advertisedAddress}/32` === route.destination
            )
        )
            invalid('routed ring target')
    }
}

// A routed proof names one reviewed host route of its own member.
function validateRoutedCandidates(targets: FabricTarget[], candidates: FabricCandidateIP[]): void {
    const routes = targets.flatMap(target =>
        target.interfaces.flatMap(iface =>
            (iface.routes ?? []).map(route => ({ nodeId: target.nodeId, iface, route }))
        )
    )
    const routed = candidates.filter(candidate => candidate.gateway !== undefined)
    const bound = routed.map(candidate =>
        routes.findIndex(
            ({ nodeId, iface, route }) =>
                candidate.nodeId === nodeId &&
                candidate.interfaceName === iface.name &&
                candidate.interfaceIndex === iface.index &&
                `${candidate.address}/31` === iface.address &&
                `${candidate.peerAddress}/32` === route.destination &&
                candidate.gateway === route.gateway
        )
    )
    if (bound.includes(-1) || new Set(bound).size !== bound.length)
        invalid('routed fabric candidates')
}

export function parseFabricReview(value: unknown): FabricReview {
    const valueRow = row(value, 'fabric review', [
        'schemaVersion',
        'reviewId',
        'ownerNodeId',
        'recipeId',
        'cableRunId',
        'persistence',
        'state',
        'executable',
        'remainingMs',
        'targets',
        'blockers',
        'effectsApplied',
        'permission',
        'inspectionRequired',
        'inspectionAvailable'
    ])
    if (valueRow.schemaVersion !== 1 || valueRow.effectsApplied !== false) invalid('fabric review')
    const targets = list(valueRow.targets, 'fabric review targets', 0, 3).map(parseFabricTarget)
    const review: FabricReview = {
        schemaVersion: 1,
        reviewId: operationId(valueRow.reviewId, 'fabric review'),
        ownerNodeId: optionalString(valueRow.ownerNodeId, 'fabric owner', 128) ?? '',
        recipeId: oneOf(valueRow.recipeId, reviewRecipes, 'fabric recipe'),
        ...(valueRow.cableRunId === undefined
            ? {}
            : { cableRunId: operationId(valueRow.cableRunId, 'bound cable run') }),
        persistence: oneOf(valueRow.persistence, ['until-reboot'] as const, 'fabric persistence'),
        state: oneOf(valueRow.state, ['blocked', 'ready'] as const, 'fabric review state'),
        executable: flag(valueRow.executable, 'fabric executable state'),
        remainingMs: integer(valueRow.remainingMs, 'fabric review lifetime', 0, 30_000),
        targets,
        blockers: strings(valueRow.blockers, 'fabric blockers', 32),
        effectsApplied: false,
        ...(valueRow.permission === undefined
            ? {}
            : { permission: parsePermission(valueRow.permission) }),
        ...(valueRow.inspectionRequired === undefined
            ? {}
            : { inspectionRequired: flag(valueRow.inspectionRequired, 'inspection requirement') }),
        ...(valueRow.inspectionAvailable === undefined
            ? {}
            : {
                  inspectionAvailable: flag(valueRow.inspectionAvailable, 'inspection availability')
              })
    }
    validateRecipeTargets(review.recipeId, review.targets)
    return review
}

function parseFabricCandidate(value: unknown): FabricCandidateIP {
    const valueRow = row(value, 'fabric candidate', [
        'nodeId',
        'peerNodeId',
        'peerPrincipal',
        'address',
        'peerAddress',
        'interfaceName',
        'interfaceIndex',
        'mac',
        'switchId',
        'portName',
        'rdmaDevice',
        'rdmaPort',
        'gidIndex',
        'gidType',
        'gateway'
    ])
    const endpoint = {
        nodeId: id(valueRow.nodeId, 'fabric candidate node'),
        peerNodeId: id(valueRow.peerNodeId, 'fabric candidate peer'),
        peerPrincipal: id(valueRow.peerPrincipal, 'fabric peer principal'),
        address: string(valueRow.address, 'fabric candidate address', 64),
        peerAddress: string(valueRow.peerAddress, 'fabric peer address', 64),
        interfaceName: id(valueRow.interfaceName, 'fabric interface name'),
        interfaceIndex: integer(valueRow.interfaceIndex, 'fabric interface index', 1),
        mac: string(valueRow.mac, 'fabric candidate MAC', 32),
        switchId: id(valueRow.switchId, 'fabric switch identity'),
        portName: id(valueRow.portName, 'fabric port name')
    }
    if (valueRow.gateway === undefined)
        return {
            ...endpoint,
            rdmaDevice: id(valueRow.rdmaDevice, 'RDMA device'),
            rdmaPort: integer(valueRow.rdmaPort, 'RDMA port', 1, 255),
            gidIndex: integer(valueRow.gidIndex, 'RDMA GID index', 0, 255),
            gidType: oneOf(valueRow.gidType, ['RoCE v2'] as const, 'RDMA GID type')
        }
    if (
        valueRow.rdmaDevice !== '' ||
        valueRow.rdmaPort !== 0 ||
        valueRow.gidIndex !== 0 ||
        valueRow.gidType !== ''
    )
        invalid('routed fabric candidate')
    return {
        ...endpoint,
        rdmaDevice: '',
        rdmaPort: 0,
        gidIndex: 0,
        gidType: '',
        gateway: ipv4(valueRow.gateway, 'fabric candidate gateway')
    }
}

export function parseFabricOperation(value: unknown): FabricOperation {
    const valueRow = row(value, 'fabric operation', [
        'failure',
        'permission',
        'schemaVersion',
        'operationId',
        'reviewId',
        'ownerNodeId',
        'recipeId',
        'cableRunId',
        'state',
        'targets',
        'cleanupConfirmed',
        'effectsApplied',
        'effectsUnconfirmed',
        'message',
        'createdAt',
        'expiresAt',
        'qualifiedAt',
        'qualificationDigest',
        'candidateIPs'
    ])
    if (valueRow.schemaVersion !== 1) invalid('fabric operation schema')
    let failure
    if (valueRow.failure !== undefined) {
        const failureRow = row(valueRow.failure, 'fabric failure', ['nodeId', 'phase', 'code'])
        failure = {
            nodeId: id(failureRow.nodeId, 'fabric failure node'),
            phase: oneOf(
                failureRow.phase,
                ['inspect', 'apply', 'qualify'] as const,
                'fabric failure phase'
            ),
            code: id(failureRow.code, 'fabric failure code')
        }
    }
    const candidateIPs =
        valueRow.candidateIPs === undefined
            ? undefined
            : list(valueRow.candidateIPs, 'fabric candidates', 0, 9).map(parseFabricCandidate)
    const qualificationDigest = optionalString(
        valueRow.qualificationDigest,
        'fabric qualification digest',
        64
    )
    const operation: FabricOperation = {
        schemaVersion: 1,
        operationId: operationId(valueRow.operationId, 'fabric operation'),
        reviewId: operationId(valueRow.reviewId, 'fabric review'),
        ownerNodeId: id(valueRow.ownerNodeId, 'fabric owner'),
        ...(valueRow.recipeId === undefined
            ? {}
            : { recipeId: oneOf(valueRow.recipeId, operationRecipes, 'fabric recipe') }),
        ...(valueRow.cableRunId === undefined
            ? {}
            : { cableRunId: operationId(valueRow.cableRunId, 'bound cable run') }),
        state: oneOf(
            valueRow.state,
            [
                'applying',
                'active',
                'rolling-back',
                'cancelled',
                'failed',
                'recovery-required',
                'not-started'
            ] as const,
            'fabric operation state'
        ),
        targets: list(valueRow.targets, 'fabric operation targets', 0, 3).map(parseFabricTarget),
        cleanupConfirmed: flag(valueRow.cleanupConfirmed, 'fabric cleanup'),
        effectsApplied: flag(valueRow.effectsApplied, 'fabric effects'),
        ...(valueRow.effectsUnconfirmed === undefined
            ? {}
            : { effectsUnconfirmed: flag(valueRow.effectsUnconfirmed, 'fabric uncertainty') }),
        message: string(valueRow.message, 'fabric operation message', 2048, true),
        createdAt: integer(valueRow.createdAt, 'fabric creation time'),
        expiresAt: integer(valueRow.expiresAt, 'fabric expiry time'),
        ...(valueRow.qualifiedAt === undefined
            ? {}
            : { qualifiedAt: integer(valueRow.qualifiedAt, 'fabric qualification time', 1) }),
        ...(qualificationDigest
            ? { qualificationDigest: digest(qualificationDigest, 'fabric qualification digest') }
            : {}),
        ...(candidateIPs ? { candidateIPs } : {}),
        ...(valueRow.permission === undefined
            ? {}
            : { permission: parsePermission(valueRow.permission) }),
        ...(failure ? { failure } : {})
    }
    const participantCount = operation.recipeId?.includes('three-node') ? 3 : 2
    if (operation.recipeId && operation.targets.length !== participantCount)
        invalid('fabric participant set')
    validateRecipeTargets(operation.recipeId, operation.targets)
    if (operation.candidateIPs) validateRoutedCandidates(operation.targets, operation.candidateIPs)
    const routedProofs = operation.recipeId === routedRingRecipe ? participantCount : 0
    if (
        operation.state === 'active' &&
        operation.recipeId !== undefined &&
        (!operation.effectsApplied ||
            operation.effectsUnconfirmed === true ||
            operation.cleanupConfirmed ||
            !operation.qualifiedAt ||
            !operation.qualificationDigest ||
            operation.candidateIPs?.length !== participantCount * 2 + routedProofs)
    )
        invalid('active fabric qualification')
    if (
        operation.state === 'not-started' &&
        (!operation.cleanupConfirmed || operation.effectsApplied || operation.effectsUnconfirmed)
    )
        invalid('fabric refusal')
    if (
        !['active', 'applying'].includes(operation.state) &&
        (operation.qualifiedAt || operation.qualificationDigest || operation.candidateIPs?.length)
    )
        invalid('fabric qualification lifetime')
    return operation
}

export function parseFabricRetainedOperations(value: unknown): FabricRetainedOperations {
    const valueRow = row(value, 'retained fabric operations', ['operations'])
    const operations = list(valueRow.operations, 'retained fabric operations').map(
        parseFabricOperation
    )
    if (operations.some(operation => operation.cleanupConfirmed))
        invalid('retained fabric operations')
    return { operations }
}

export function acceptedHostKeys(review: CableReview | FabricReview): FabricAcceptedHostKey[] {
    return (review.permission?.targets ?? [])
        .filter(target => !target.hostKeyTrusted && !!target.candidateId && !!target.hostKeySha256)
        .map(target => ({ candidateId: target.candidateId, sha256: target.hostKeySha256! }))
}

export function acceptedCleanupHostKeys(review: CableCleanupReview): FabricAcceptedHostKey[] {
    return review.targets
        .filter(target => !target.hostKeyTrusted && !!target.candidateId && !!target.hostKeySha256)
        .map(target => ({ candidateId: target.candidateId, sha256: target.hostKeySha256! }))
}

export function cableSucceeded(run: CableRun | null): boolean {
    return Boolean(
        run &&
        run.state === 'completed' &&
        run.cleanupConfirmed &&
        run.result === 'reciprocal-observations' &&
        run.topology?.status === 'matched'
    )
}

export function fabricOperationCarriesCableMatch(operation: FabricOperation | null): boolean {
    return Boolean(
        operation?.cableRunId &&
        !operation.cleanupConfirmed &&
        (operation.state === 'active' || operation.effectsApplied || operation.effectsUnconfirmed)
    )
}
