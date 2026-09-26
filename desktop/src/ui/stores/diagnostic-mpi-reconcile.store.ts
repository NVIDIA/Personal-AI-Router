// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { create } from 'zustand'
import type { DiagnosticMPIReconcileResult } from '@/shared/types/diagnostic-mpi-reconcile'
import getErrorString from '@/shared/utils/get-error-string'

interface DiagnosticMPIReconcileState {
    pending: boolean
    result: DiagnosticMPIReconcileResult | null
    error: string | null
    reconcile(): Promise<DiagnosticMPIReconcileResult | null>
    reset(): void
}

let epoch = 0

export const useDiagnosticMPIReconcileStore = create<DiagnosticMPIReconcileState>((set, get) => ({
    pending: false,
    result: null,
    error: null,

    reconcile: async () => {
        if (get().pending) return null
        const revision = ++epoch
        set({ pending: true, result: null, error: null })
        try {
            const result = await window.pairApi.setup.reconcileDiagnosticMpi()
            if (revision !== epoch) return null
            set({ result, pending: false })
            return result
        } catch (error) {
            if (revision === epoch) set({ error: getErrorString(error), pending: false })
            return null
        }
    },

    reset: () => {
        epoch++
        set({ pending: false, result: null, error: null })
    }
}))
