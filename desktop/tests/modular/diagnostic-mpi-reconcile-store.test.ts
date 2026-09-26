// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { DiagnosticMPIReconcileResult } from '@/shared/types/diagnostic-mpi-reconcile'
import { useDiagnosticMPIReconcileStore } from '@/ui/stores/diagnostic-mpi-reconcile.store'

const result: DiagnosticMPIReconcileResult = {
    state: 'completed',
    recoveryRequired: false,
    operations: []
}

describe('diagnostic MPI cleanup store', () => {
    const reconcileDiagnosticMpi = vi.fn()

    beforeEach(() => {
        reconcileDiagnosticMpi.mockReset()
        vi.stubGlobal('window', {
            pairApi: { setup: { reconcileDiagnosticMpi } }
        })
        useDiagnosticMPIReconcileStore.getState().reset()
    })

    afterEach(() => vi.unstubAllGlobals())

    it('retains the exact terminal operation summary for the Setup UI', async () => {
        reconcileDiagnosticMpi.mockResolvedValueOnce(result)
        await expect(useDiagnosticMPIReconcileStore.getState().reconcile()).resolves.toEqual(result)
        expect(reconcileDiagnosticMpi).toHaveBeenCalledOnce()
        expect(useDiagnosticMPIReconcileStore.getState()).toMatchObject({
            pending: false,
            result,
            error: null
        })
    })

    it('keeps a failed effect visible without inventing a result', async () => {
        reconcileDiagnosticMpi.mockRejectedValueOnce(new Error('Reconciliation remains held.'))
        await expect(useDiagnosticMPIReconcileStore.getState().reconcile()).resolves.toBeNull()
        expect(useDiagnosticMPIReconcileStore.getState()).toMatchObject({
            pending: false,
            result: null,
            error: 'Reconciliation remains held.'
        })
    })

    it('discards a reply from a connection that was reset while the effect was pending', async () => {
        let resolve!: (value: DiagnosticMPIReconcileResult) => void
        reconcileDiagnosticMpi.mockReturnValueOnce(
            new Promise<DiagnosticMPIReconcileResult>(done => {
                resolve = done
            })
        )
        const pending = useDiagnosticMPIReconcileStore.getState().reconcile()
        useDiagnosticMPIReconcileStore.getState().reset()
        resolve(result)
        await expect(pending).resolves.toBeNull()
        expect(useDiagnosticMPIReconcileStore.getState()).toMatchObject({
            pending: false,
            result: null,
            error: null
        })
    })
})
