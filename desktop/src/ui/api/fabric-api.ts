// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { PreloadServiceTransport as ServiceTransport } from '@/shared/types/service-bridge'
import type {
    CableCleanupReview,
    CableCleanupVerifyResult,
    CableRetainedRuns,
    CableReview,
    CableRun,
    CableStartResponse,
    FabricOperation,
    FabricInventorySnapshot,
    FabricRetainedOperations,
    FabricReview,
    FabricSelection
} from '@/shared/types/fabric'
import {
    parseCableCleanupReviewRequest,
    parseCableStatusRequest,
    parseFabricInventoryRequest,
    parseFabricSelection,
    parseOperationIdRequest,
    parseReviewIdRequest
} from '@/shared/utils/fabric'
import { bindFabricApi } from '@/ui/stores/fabric.store'

export interface IFabricApi {
    getInventory(nodeIds: string[]): Promise<FabricInventorySnapshot>
    reviewCable(selection: FabricSelection): Promise<CableReview>
    approveCable(reviewId: string): Promise<CableStartResponse>
    getCableStatus(identity: { runId?: string; reviewId?: string }): Promise<CableRun>
    cancelCable(runId: string): Promise<CableRun>
    getRetainedCableRuns(): Promise<CableRetainedRuns>
    reviewCableCleanup(
        runId: string,
        acceptedHostKeys?: FabricSelection['acceptedHostKeys']
    ): Promise<CableCleanupReview>
    verifyCableCleanup(runId: string, reviewId: string): Promise<CableCleanupVerifyResult>
    cancelCableCleanup(runId: string): Promise<CableRun>
    reviewFabric(
        selection: FabricSelection,
        inspectSelectedProfiles?: boolean
    ): Promise<FabricReview>
    approveFabric(reviewId: string, selectedPortPauseApproved: boolean): Promise<FabricOperation>
    getFabricStatus(operationId: string): Promise<FabricOperation>
    cancelFabric(operationId: string): Promise<FabricOperation>
    recoverFabric(operationId: string): Promise<FabricOperation>
    getRetainedFabricOperations(): Promise<FabricRetainedOperations>
}

export function createFabricApi(transport: ServiceTransport): IFabricApi {
    const api: IFabricApi = {
        getInventory: nodeIds =>
            transport.invoke('engine:fabric-inventory', parseFabricInventoryRequest({ nodeIds })),
        reviewCable: selection =>
            transport.invoke('engine:cable-review', parseFabricSelection(selection)),
        approveCable: reviewId =>
            transport.invoke('engine:cable-start', parseReviewIdRequest({ reviewId })),
        getCableStatus: identity =>
            transport.invoke('engine:cable-status', parseCableStatusRequest(identity)),
        cancelCable: runId =>
            transport.invoke('engine:cable-cancel', {
                runId: parseCableStatusRequest({ runId }).runId!
            }),
        getRetainedCableRuns: () => transport.invoke('engine:cable-retained-runs'),
        reviewCableCleanup: (runId, acceptedHostKeys) =>
            transport.invoke(
                'engine:cable-cleanup-review',
                parseCableCleanupReviewRequest({ runId, acceptedHostKeys })
            ),
        verifyCableCleanup: (runId, reviewId) =>
            transport.invoke('engine:cable-cleanup-verify', {
                runId: parseCableCleanupReviewRequest({ runId }).runId,
                reviewId: parseReviewIdRequest({ reviewId }).reviewId
            }),
        cancelCableCleanup: runId =>
            transport.invoke('engine:cable-cleanup-cancel', {
                runId: parseCableCleanupReviewRequest({ runId }).runId
            }),
        reviewFabric: (selection, inspectSelectedProfiles) =>
            transport.invoke('engine:fabric-review', {
                selection: parseFabricSelection(selection),
                ...(inspectSelectedProfiles === undefined ? {} : { inspectSelectedProfiles })
            }),
        approveFabric: (reviewId, selectedPortPauseApproved) =>
            transport.invoke('engine:fabric-approve', {
                reviewId: parseOperationIdRequest({ operationId: reviewId }).operationId,
                selectedPortPauseApproved
            }),
        getFabricStatus: operationId =>
            transport.invoke('engine:fabric-status', parseOperationIdRequest({ operationId })),
        cancelFabric: operationId =>
            transport.invoke('engine:fabric-cancel', parseOperationIdRequest({ operationId })),
        recoverFabric: operationId =>
            transport.invoke('engine:fabric-recover', parseOperationIdRequest({ operationId })),
        getRetainedFabricOperations: () => transport.invoke('engine:fabric-retained-operations')
    }
    bindFabricApi(api)
    return api
}
