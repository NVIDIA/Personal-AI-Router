// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { create } from 'zustand'
import type { ServiceError } from '@/shared/types/errors'

const MAX_ERRORS = 100

interface ErrorsStore {
    errors: ServiceError[]
    clearError: (id: string) => void
    addLocalError: (message: string) => void
    initialize: () => Promise<void>
    refresh: () => void
    cleanup: () => void
}

let unsubs: Array<() => void> = []

export const useErrorsStore = create<ErrorsStore>(set => ({
    errors: [],

    clearError: (id: string) => {
        set(state => ({ errors: state.errors.filter(e => e.id !== id) }))
        window.pairApi?.errors.clear(id)
    },

    addLocalError: (message: string) => {
        const error: ServiceError = {
            id: `local-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`,
            message,
            timestamp: Date.now()
        }
        set(state => ({
            errors: [error, ...state.errors].slice(0, MAX_ERRORS)
        }))
    },

    refresh: () => {
        window.pairApi?.errors
            .getInitial()
            .then(errors => set({ errors }))
            .catch(() => {})
    },

    initialize: async () => {
        try {
            const errors = await window.pairApi.errors.getInitial()
            set({ errors })
        } catch (error) {
            console.error('Failed to initialize errors store:', error)
        }

        if (window.pairApi) {
            unsubs.push(
                window.pairApi.errors.onUpdate((errors: ServiceError[]) => {
                    set({ errors })
                })
            )
        }
    },

    cleanup: () => {
        unsubs.forEach(u => u())
        unsubs = []
    }
}))
