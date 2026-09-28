// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { WsInvokeRequest } from '@/shared/types/ws-channels'
import {
    parseDiagnosticMPIReconcileRequest,
    parseDiagnosticMPIReconcileResult
} from '@/shared/utils/diagnostic-mpi-reconcile'
import { getModularSupervisor } from './modular-supervisor'

const DIAGNOSTIC_MPI_RECONCILE_TIMEOUT_MS = 185_000

export const diagnosticMPIReconcileHandlers = {
    'engine:diagnostic-mpi-reconcile': async (
        payload?: WsInvokeRequest<'engine:diagnostic-mpi-reconcile'>
    ) => {
        const supervisor = getModularSupervisor()
        if (!supervisor.ready) {
            throw new Error('PAIR service is unavailable for diagnostic cleanup recovery.')
        }
        const request = parseDiagnosticMPIReconcileRequest(payload ?? {})
        return parseDiagnosticMPIReconcileResult(
            await supervisor.callProcess(
                'broker',
                'engine:diagnostic-mpi-reconcile',
                request,
                DIAGNOSTIC_MPI_RECONCILE_TIMEOUT_MS
            )
        )
    }
}
