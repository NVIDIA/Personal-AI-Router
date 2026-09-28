// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Managed NCCL bridge. The renderer can select only an adopted build, two or
 * three node identities and the NCCL Socket network, or address one exact
 * PAIR-issued review/operation. Fixed Socket and dedicated-window authority is
 * injected here; credentials, commands, paths, keys and raw process output have
 * no request or reply field.
 */
import type { WsInvokeRequest } from '@/shared/types/ws-channels'
import type {
    DiagnosticMPIOperation,
    DiagnosticMPIOperationBinding
} from '@/shared/types/diagnostic-mpi'
import {
    bindDiagnosticMPIOperation,
    parseDiagnosticMPIApproveRequest,
    parseDiagnosticMPIManagedInventory,
    parseDiagnosticMPIOperation,
    parseDiagnosticMPIOperationBinding,
    parseDiagnosticMPIRecovery,
    parseDiagnosticMPIRecoveryReference,
    parseDiagnosticMPIReview,
    parseDiagnosticMPIReviewClosure,
    parseDiagnosticMPIReviewRequest,
    parseDiagnosticMPISelection
} from '@/shared/utils/diagnostic-mpi'
import { getModularSupervisor } from './modular-supervisor'
import { JsonRpcResponseError, type JsonObject } from './json-rpc-subprocess'

type BackendMethod =
    | 'engine:diagnostic-managed-runtimes'
    | 'engine:diagnostic-mpi-review'
    | 'engine:diagnostic-mpi-approve'
    | 'engine:diagnostic-mpi-status'
    | 'engine:diagnostic-mpi-cancel'
    | 'engine:diagnostic-mpi-recover'

const timeouts: Record<BackendMethod, number> = {
    'engine:diagnostic-managed-runtimes': 30_000,
    // Engine Manager budgets up to 165 s for a three-node fabric review.
    'engine:diagnostic-mpi-review': 180_000,
    'engine:diagnostic-mpi-approve': 110_000,
    'engine:diagnostic-mpi-status': 110_000,
    'engine:diagnostic-mpi-cancel': 110_000,
    'engine:diagnostic-mpi-recover': 110_000
}

const publicFailures: Record<BackendMethod, string> = {
    'engine:diagnostic-managed-runtimes':
        'PAIR could not read the managed NCCL registry. No backend diagnostic text was exposed.',
    'engine:diagnostic-mpi-review':
        'PAIR could not create the managed NCCL review. No backend diagnostic text was exposed.',
    'engine:diagnostic-mpi-approve':
        'PAIR did not confirm the managed NCCL Start. No backend diagnostic text was exposed.',
    'engine:diagnostic-mpi-status':
        'PAIR could not read managed NCCL status. No backend diagnostic text was exposed.',
    'engine:diagnostic-mpi-cancel':
        'PAIR could not confirm managed NCCL cancellation. No backend diagnostic text was exposed.',
    'engine:diagnostic-mpi-recover':
        'PAIR could not recover the retained managed NCCL selector. No backend diagnostic text was exposed.'
}

// Engine Manager tags its fabric review refusals with these codes. Only this
// renderer-owned wording crosses the bridge, never the backend's own text.
const fabricRefusals = new Map<string, string>([
    [
        '-32010',
        'No active fabric on this controller joins exactly the selected nodes. Apply a two-node direct fabric or a three-node routed ring here first, or review on the management network.'
    ],
    [
        '-32011',
        'The fabric involving the selected nodes is stale, ambiguous, unrouted, or could not be re-proven. Roll it back or recover it before reviewing over the fabric, or review on the management network.'
    ]
])

async function call(method: BackendMethod, params: JsonObject) {
    const supervisor = getModularSupervisor()
    if (!supervisor.ready)
        throw new Error('PAIR service is unavailable for the managed NCCL smoke.')
    try {
        return await supervisor.callProcess('broker', method, params, timeouts[method])
    } catch (error) {
        const refusal =
            method === 'engine:diagnostic-mpi-review' && error instanceof JsonRpcResponseError
                ? fabricRefusals.get(error.message.split(':', 1)[0])
                : undefined
        throw new Error(refusal ?? publicFailures[method])
    }
}

function backendOperationParams(binding: DiagnosticMPIOperationBinding): JsonObject {
    return {
        ownerNodeId: binding.ownerNodeId,
        operationId: binding.operationId,
        groupId: binding.groupId
    }
}

async function readOperation(
    binding: DiagnosticMPIOperationBinding
): Promise<DiagnosticMPIOperation> {
    return bindDiagnosticMPIOperation(
        parseDiagnosticMPIOperation(
            await call('engine:diagnostic-mpi-status', backendOperationParams(binding))
        ),
        binding
    )
}

export const diagnosticMPIHandlers = {
    'engine:diagnostic-managed-runtimes': async (
        payload?: WsInvokeRequest<'engine:diagnostic-managed-runtimes'>
    ) => {
        if (payload !== undefined)
            throw new Error('Managed NCCL inventory takes no renderer parameters.')
        return parseDiagnosticMPIManagedInventory(
            await call('engine:diagnostic-managed-runtimes', {})
        )
    },

    'engine:diagnostic-mpi-review': async (
        payload?: WsInvokeRequest<'engine:diagnostic-mpi-review'>
    ) => {
        const request = parseDiagnosticMPIReviewRequest(payload)
        const review = parseDiagnosticMPIReview(
            await call('engine:diagnostic-mpi-review', {
                buildOperationId: request.buildOperationId,
                memberNodeIds: request.nodeIds,
                network: request.network,
                dedicatedTestWindow: true
            })
        )
        if (
            review.buildOperationId !== request.buildOperationId ||
            review.network !== request.network ||
            review.targets
                .map(target => target.nodeId)
                .sort()
                .join('\n') !== request.nodeIds.join('\n')
        )
            throw new Error('PAIR returned a managed NCCL review for a different selection.')
        return review
    },

    'engine:diagnostic-mpi-approve': async (
        payload?: WsInvokeRequest<'engine:diagnostic-mpi-approve'>
    ) => {
        const request = parseDiagnosticMPIApproveRequest(payload)
        const binding: DiagnosticMPIOperationBinding = request
        try {
            return bindDiagnosticMPIOperation(
                parseDiagnosticMPIOperation(
                    await call('engine:diagnostic-mpi-approve', {
                        ownerNodeId: request.ownerNodeId,
                        reviewId: request.reviewId
                    })
                ),
                binding
            )
        } catch (approvalError) {
            // Approval is one-use. A lost reply is settled by the exact
            // pre-issued operation selector and is never resent.
            try {
                return await readOperation(binding)
            } catch {
                throw approvalError
            }
        }
    },

    'engine:diagnostic-mpi-status': async (
        payload?: WsInvokeRequest<'engine:diagnostic-mpi-status'>
    ) => readOperation(parseDiagnosticMPIOperationBinding(payload)),

    'engine:diagnostic-mpi-cancel': async (
        payload?: WsInvokeRequest<'engine:diagnostic-mpi-cancel'>
    ) => {
        const binding = parseDiagnosticMPIOperationBinding(payload)
        return bindDiagnosticMPIOperation(
            parseDiagnosticMPIOperation(
                await call('engine:diagnostic-mpi-cancel', backendOperationParams(binding))
            ),
            binding
        )
    },

    'engine:diagnostic-mpi-recover': async (
        payload?: WsInvokeRequest<'engine:diagnostic-mpi-recover'>
    ) => {
        const selection = parseDiagnosticMPISelection(payload)
        const recovery = parseDiagnosticMPIRecovery(
            await call('engine:diagnostic-mpi-recover', {
                buildOperationId: selection.buildOperationId,
                memberNodeIds: selection.nodeIds
            })
        )
        if (
            recovery.reference &&
            (recovery.reference.buildOperationId !== selection.buildOperationId ||
                recovery.reference.memberNodeIds.join('\n') !== selection.nodeIds.join('\n'))
        )
            throw new Error('PAIR recovered a managed NCCL selector for a different selection.')
        return recovery
    },

    'engine:diagnostic-mpi-close-review': async (
        payload?: WsInvokeRequest<'engine:diagnostic-mpi-close-review'>
    ) => {
        const reference = parseDiagnosticMPIRecoveryReference(payload)
        const closure = parseDiagnosticMPIReviewClosure(
            await call('engine:diagnostic-mpi-status', {
                ownerNodeId: reference.ownerNodeId,
                reviewId: reference.reviewId,
                closeUnstartedReview: true
            })
        )
        if (closure.operation) bindDiagnosticMPIOperation(closure.operation, reference)
        return closure
    }
}
